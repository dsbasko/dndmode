//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/dsbasko/dndmode/internal/macos/eventtap"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
)

// captureFailure maps an eventtap capture error onto the line to print and the
// exit code to return.
//
// Every INPUT outcome is exitConfigErr because they all mean the same thing to
// a caller: the config was not touched and the old unlock code still works.
// They are enumerated rather than collapsed into a default so the operator
// learns WHICH safeguard fired — Escape, a ceiling, a ring that lagged, two
// entries that disagreed — without any of the messages carrying a step, a
// keycode or a count. eventtap's sentinels are bare static strings for the same
// reason and pin the property with their own test.
//
// ErrTapInstallFailed is the one outcome that is NOT about the input, so it is
// the one that does not exit 1. The machine refused the tap — Accessibility
// revoked between the pre-check and the install, SecureEventInput acquired in
// that window, the kernel out of mach ports — and a caller that sees exit 1
// would go looking for a typo in a config file that is in fact fine. main.go
// maps the identical sentinel to exitPlatformErr on the session path (Step 17);
// the same failure must not carry two different codes depending on which
// command hit it.
//
// context.Canceled arrives from the branch's private signal handler (a kill
// from another terminal, or SIGHUP when the window closes); it is not a
// failure of the capture so much as an abandonment of it, and it reads better
// as its own line than as a wrapped tap error.
func captureFailure(err error) (string, int) {
	switch {
	case errors.Is(err, eventtap.ErrCaptureCancelled):
		return "cancelled — the config was not changed.", exitConfigErr
	case errors.Is(err, eventtap.ErrCaptureMismatch):
		return "the two entries did not match — the config was not changed. Re-run to try again.", exitConfigErr
	case errors.Is(err, eventtap.ErrCaptureTooLong):
		return "that unlock code has too many steps — the config was not changed.", exitConfigErr
	case errors.Is(err, eventtap.ErrCaptureLostEvents):
		return "some keystrokes were missed, so the sequence would be incomplete — the config was not changed.", exitConfigErr
	case errors.Is(err, eventtap.ErrCaptureTimedOut):
		return "timed out waiting for the unlock code — the config was not changed.", exitConfigErr
	case errors.Is(err, context.Canceled):
		return "aborted — the config was not changed.", exitConfigErr
	case errors.Is(err, eventtap.ErrTapInstallFailed):
		return fmt.Sprintf(
			"the input tap could not be installed, so nothing was captured: %v. "+
				"Re-grant Accessibility (System Settings → Privacy & Security → Accessibility) "+
				"and close any sudo prompt or password field, then re-run.", err), exitPlatformErr
	default:
		// Not an input error either: whatever reaches here describes the event
		// tap run loop or its teardown, never what was typed.
		return fmt.Sprintf("capturing the unlock code failed: %v", err), exitPlatformErr
	}
}

// runSetPassword is the --set-password branch: capture a new unlock sequence
// from real keystrokes, twice, and store it in the config as a salted digest.
// It returns the process exit code and starts no dndmode session at all.
//
// run() dispatches here BEFORE its Step 3, so the command never constructs a
// RestoreState, never touches Focus, audio, the IOPMAssertion or the
// single-instance lock, and never opens a window. The only thing it changes on
// the machine is one key in config.yml.
//
// outW is the UNGATED writer and carries exactly three lines over the whole
// command: the two capture prompts and the success line. errW is the gated one
// and carries EVERYTHING else — flag conflicts, an unusable config, a refused
// code, every capture failure — which means that by default this command is as
// silent as the rest of dndmode and speaks only through its exit code.
//
// The asymmetry is deliberate on both sides. The gate exists so a visible
// terminal under overlay_style none/glass cannot leak hints about the secret;
// --set-password has no overlay at all, the operator is physically at the
// keyboard, and prompts that name neither a value nor a length leak nothing.
// The errors go back INTO the gate because their text is where lengths and
// counts would surface.
func runSetPassword(ctx context.Context, fl setPasswordFlags, outW, errW io.Writer, debugOn *bool, log *slog.Logger) int {
	if name := fl.conflictingFlag(); name != "" {
		_, _ = fmt.Fprintf(errW,
			"dndmode: --set-password cannot be combined with %s — it rewrites the config and exits without starting a session.\n",
			name)
		return exitConfigErr
	}

	// Platform check ahead of everything else, exactly as run() does: a
	// cross-arch or pre-Sonoma host must surface exit 2 rather than a confusing
	// failure further down, and the event tap this command installs is the same
	// Apple-Silicon-only machinery.
	ver := permissions.CurrentOSVersion()
	if err := permissions.CheckPlatform(permissions.CurrentArch(), ver); err != nil {
		switch {
		case errors.Is(err, permissions.ErrNonArm64):
			_, _ = fmt.Fprintf(errW,
				"dndmode: requires macOS on Apple Silicon (arm64), got %s/%s.\n",
				runtime.GOOS, runtime.GOARCH)
		case errors.Is(err, permissions.ErrMacOSBelow14):
			_, _ = fmt.Fprintf(errW,
				"dndmode: requires macOS 14 (Sonoma) or newer, got %d.%d.\n",
				ver.Major, ver.Minor)
		default:
			_, _ = fmt.Fprintf(errW, "dndmode: platform check failed: %v. Re-run on macOS 14+ Apple Silicon.\n", err)
		}
		return exitPlatformErr
	}

	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: resolve home dir: %v.\n", err)
		return exitPlatformErr
	}

	// The prompts go to stdout, the keystrokes come from stdin, and BOTH have
	// to be a terminal. runSetPasswordAt checks the input side (that is the
	// descriptor it is handed, and the one MakeRaw works on); the output side
	// can only be checked here, where the concrete *os.File still exists —
	// below this line the sink is an io.Writer with no descriptor.
	//
	// Checking only stdin would let `dndmode --set-password > log.txt` through:
	// stdin is still a tty, so MakeRaw succeeds and the HID tap goes up, while
	// both prompts land in the file. The operator sees a frozen terminal with a
	// dead keyboard and no explanation until a capture ceiling fires.
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		_, _ = fmt.Fprintln(errW,
			"dndmode: --set-password is interactive — its prompts must reach a terminal, so stdout cannot be redirected to a file or a pipe.")
		return exitConfigErr
	}

	return runSetPasswordAt(ctx, filepath.Join(home, configRelPath), int(os.Stdin.Fd()), outW, errW, debugOn, log)
}

// runSetPasswordAt is runSetPassword with the two pieces of process-global
// state — the config path and the terminal file descriptor — passed in.
//
// The split is what makes the branch testable at all. With a temp path and the
// descriptor of an ordinary file, the whole pre-capture half runs under `go
// test`: the symlink guard, the first-run creation, the surgery dry run and
// the non-tty refusal, none of which may reach the real ~/.config/dndmode or
// try to install an event tap.
func runSetPasswordAt(ctx context.Context, cfgPath string, ttyFD int, outW, errW io.Writer, debugOn *bool, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}

	loader, cfg, created, err := prepareConfigForCapture(cfgPath)
	// `debug: true` in the config raises the gate here, exactly as run() Step 5
	// does for a session — "either source enables output" is a property of the
	// whole binary, not of the session path. This branch runs BEFORE Step 5, so
	// without this line a user who put `debug: true` in their config instead of
	// typing --debug would get a --set-password that fails in total silence,
	// which is the one command where silence is least affordable: the keyboard
	// is already dead by the time most of these diagnostics fire. errW and the
	// logger both hold this same *bool, so raising it here opens both.
	//
	// AHEAD of the error branch, not after it: prepareConfigForCapture returns
	// a loaded cfg on every failure that happens after Load for exactly this
	// reason, and a gate raised only on the success path would swallow the
	// diagnostic for the config that needs it most — the one that cannot be
	// rewritten. On the failures that happen BEFORE Load cfg is the zero value,
	// so this reads false and nothing is raised.
	if cfg.Debug {
		*debugOn = true
	}
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v\n", err)
		return exitConfigErr
	}
	// Through the GATE, not through outW: "exactly three ungated lines" is a
	// property of this command, and a first run must not quietly make it four.
	if created {
		_, _ = fmt.Fprintf(errW, "dndmode: created default config at %s\n", cfgPath)
	}

	// A live dndmode owns the keyboard; this command must not take it away.
	// installForCapture head-inserts its own kCGHIDEventTap and its callback
	// returns NULL for everything, so a capture started beside a running
	// session would sit IN FRONT of that session's tap and swallow the unlock
	// code the owner types — for as long as the capture ceilings allow, i.e.
	// up to two full passes. The shield would still be up and the machine
	// would look dead to the one person it is supposed to let back in.
	//
	// Read-only, and deliberately not the lock: this branch never writes
	// runtime.json and never takes ownership, it only refuses to run beside an
	// owner. It reads the same file as run() Step 5c and agrees with it about
	// what "already running" means; where it deliberately does NOT agree is on
	// an unreadable file — see probeLivePeer for why an inconclusive answer has
	// to abort here and may warn-and-continue there.
	//
	// After the config work so `debug: true` can explain the refusal, and
	// before the tty check, the grant wait and anything that installs a tap.
	runtimeMgr := runtimepkg.NewManager(filepath.Join(filepath.Dir(cfgPath), filepath.Base(runtimeRelPath)), log)
	if detail, code := probeLivePeer(runtimeMgr, log); code != exitOK {
		_, _ = fmt.Fprintf(errW, "dndmode: %s. %s\n", detail, peerBeforeCapture)
		return code
	}

	// The tty check sits ahead of WaitForGrants rather than next to MakeRaw
	// where the flow diagram draws the terminal work. Capture without a
	// terminal is meaningless — there is nobody to read the prompts and nothing
	// for MakeRaw to work on — and a piped run that reached WaitForGrants first
	// would block forever on an Accessibility grant it is never going to get,
	// with no output to say so.
	if !term.IsTerminal(ttyFD) {
		_, _ = fmt.Fprintln(errW,
			"dndmode: --set-password is interactive — run it from a terminal, not from a pipe or a script.")
		return exitConfigErr
	}

	// Accessibility: without it CGEventTapCreate returns NULL and the capture
	// cannot start. Same seams and same 500ms cadence as run() Step 9.
	statusOut := io.Discard
	if *debugOn {
		// The real *os.File, not the gated writer: NewStatusWriter detects a TTY
		// to decide between \r-repaint and plain lines, and a wrapper defeats it.
		statusOut = os.Stdout
	}
	// Signals during the grant wait. The branch runs before run()'s
	// signal.NotifyContext and its own raw-mode handler is not installed until
	// MakeRaw succeeds, so this window would otherwise be watched by nobody:
	// ctx is context.Background() from run(), the context.Canceled branch below
	// could never be taken, and a Ctrl-C on a machine that has never granted
	// Accessibility would kill the process through the default disposition
	// rather than through exit 3 the way a session does.
	//
	// Stopped before MakeRaw, so the raw-mode handler installed further down is
	// the only one holding the channel while the terminal is unusable.
	grantCtx, stopGrantSignals := signal.NotifyContext(ctx,
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	err = permissions.WaitForGrants(grantCtx,
		permissions.NewCgoChecker(),
		permissions.NewDeepLinker(),
		permissions.NewStatusWriter(statusOut),
		func() { permissions.PromptAccessibility() },
		log, 500*time.Millisecond,
	)
	stopGrantSignals()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_, _ = fmt.Fprintln(errW, "dndmode: aborted while waiting for permissions.")
			return exitPermissionDenied
		}
		_, _ = fmt.Fprintf(errW, "dndmode: wait for grants failed: %v. Check Console.app for TCC daemon errors and re-run.\n", err)
		return exitPlatformErr
	}

	// Secure Event Input is not a nicety here, it is the difference between a
	// clear refusal and an apparent hang: with a password field in focus (a
	// sudo prompt, 1Password) the tap sees nothing at all, so the capture would
	// sit there collecting an empty sequence until a ceiling fires.
	if permissions.IsSecureEventInputActive() {
		_, _ = fmt.Fprintln(errW,
			"dndmode: Secure Event Input is active (typically Terminal sudo prompt, password fields, or 1Password). Close those, then re-run.")
		return exitSecureInputConflict
	}

	// --- The reservation, taken BEFORE anything can install a tap ---
	//
	// The probe on arrival proved nothing about the stretch since: the tty
	// check is instant, but WaitForGrants is unbounded by construction — it
	// sits there until the operator finishes clicking through System Settings.
	// A session started inside that window publishes runtime.json and installs
	// its own kCGHIDEventTap, and the capture below would then head-insert ITS
	// tap in front of that one and swallow every keystroke meant to unlock the
	// shield, for as long as the capture ceilings allow. That is precisely the
	// outcome probeLivePeer fails closed to avoid, so it may not be left
	// reachable through a stale answer.
	//
	// Re-probing alone would only narrow the window; nothing stops a session
	// from arriving one instruction later. What closes it is holding the
	// publish lock from HERE — ahead of the tap — through the save: a session
	// that starts now blocks on the same lock at its Step 13.3 and refuses
	// before it raises anything, and a second --set-password refuses before it
	// captures, rather than both capturing and racing their saves.
	//
	// After WaitForGrants and not before it, deliberately: the lock is what
	// makes every session refuse to start, and a --set-password parked forever
	// on a permission prompt that has never been granted must not be able to
	// hold it. From here on the hold is bounded by the capture ceilings.
	//
	// Fails CLOSED, unlike the session path, and for the reason that governs
	// every other refusal on this branch: not capturing is always available
	// here, and the failures this guards against — a swallowed unlock code, a
	// config naming a secret the live shield rejects — are the ones the whole
	// command exists to avoid.
	releaseLock, lockErr := acquirePublishLock(filepath.Dir(cfgPath))
	// Released when the branch returns, i.e. just after the save. Explicitly
	// NOT released before the deferred terminal restore: nothing between them
	// touches config.yml, and the ordering that matters is only that no session
	// may publish between this probe and the rename.
	defer releaseLock()
	if lockErr != nil {
		_, _ = fmt.Fprintf(errW,
			"dndmode: cannot claim the publish lock, so another dndmode cannot be ruled out: %v. %s\n",
			lockErr, lockBeforeCapture)
		return exitRuntimeJSON
	}

	if detail, code := probeLivePeer(runtimeMgr, log); code != exitOK {
		_, _ = fmt.Fprintf(errW, "dndmode: %s. %s\n", detail, peerBeforeCapture)
		return code
	}

	// The state to restore comes from unix, NOT from term.MakeRaw's return
	// value. *term.State wraps an unexported struct with no accessor, so a
	// *unix.Termios cannot be recovered from it and IoctlSetTermios would not
	// compile — and the obvious repair, term.Restore, goes through TIOCSETA on
	// Darwin, i.e. WITHOUT TCSAFLUSH, which loses the input flush silently.
	old, err := unix.IoctlGetTermios(ttyFD, unix.TIOCGETA)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot read terminal state: %v.\n", err)
		return exitConfigErr
	}

	// Raw for the whole branch. The tap hides keystrokes only while it is
	// installed; the windows on either side of it — between the first prompt
	// and the tap going live, and between the tap coming down and this command
	// exiting — are closed by raw mode and nothing else. MakeRaw is kept for
	// its well-tested flag work even though its return value is unused: hand
	// rolling the flags would make a second source of truth for raw mode.
	if _, err := term.MakeRaw(ttyFD); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot switch the terminal to raw mode: %v.\n", err)
		return exitConfigErr
	}

	// ONE ioctl restores the terminal, and it is the same one everywhere —
	// the deferred call below and the signal handler both go through here.
	//
	// TIOCSETAF is tcsetattr with TCSAFLUSH on Darwin: restore the saved
	// attributes AND discard unread input, atomically. The tempting
	// alternative — drain stdin, then term.Restore — hangs on the ordinary
	// SUCCESS path: MakeRaw sets VMIN=1/VTIME=0 so a read blocks until a byte
	// arrives, and the tap swallowed every keystroke at kCGHIDEventTap, so the
	// queue is empty and the first read never returns. Restore then never runs,
	// the terminal stays raw, and with ISIG cleared Ctrl-C arrives as the byte
	// 0x03 that nothing is listening for. Even a non-blocking drain plus
	// Restore is two actions with a gap between them.
	restore := func() { _ = unix.IoctlSetTermios(ttyFD, unix.TIOCSETAF, old) }

	// The branch runs BEFORE run()'s signal.NotifyContext, so nothing else is
	// watching for signals yet: without this handler a SIGTERM would kill the
	// process with the terminal still in raw mode, leaving the user to type
	// `stty sane` blind.
	//
	// Its real audience is `kill` from another terminal and the SIGHUP that
	// arrives when the window closes. Ctrl-C is NOT in it in practice: while
	// the tap is up Ctrl-C is suppressed at kCGHIDEventTap and never reaches
	// the process, and under raw mode without the tap ISIG is cleared so it
	// arrives as a byte instead of a signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	capCtx, capCancel := context.WithCancel(ctx)
	// commitMu makes "has this run been abandoned?" and "publish the new
	// secret" one indivisible decision. Without it the handler could land in
	// the window between CaptureConfirmed returning and SaveUnlockHash's
	// rename: it would restore the terminal — which is what an abort looks
	// like from the outside — and the rename would go through anyway. The user
	// would walk away believing the old code survived, and learn otherwise
	// from behind a shield that no longer answers to it. Held by the commit
	// section below for exactly one atomic file write.
	var commitMu sync.Mutex
	go func() {
		select {
		case <-sigCh:
			// Cancel under the mutex, so only two orderings exist: cancel
			// before the commit reads capCtx.Err() (the capture aborts and the
			// config is left alone) or after the rename (the new code is live
			// and this run must not claim otherwise).
			commitMu.Lock()
			capCancel()
			commitMu.Unlock()
			// Then the terminal. Cancelling capCtx is what makes
			// CaptureConfirmed return, and its own defer takes the tap down;
			// restoring after the cancel rather than before it costs the
			// uncontended lock and nothing else, and during a commit the delay
			// is the one the deferred restore() would have imposed anyway —
			// that defer already runs AFTER the config write on purpose.
			restore()
		case <-capCtx.Done():
		}
	}()
	// One defer for the raw mode AND the handler it needs, registered only now
	// that MakeRaw has actually succeeded. It runs AFTER the config is written
	// (below) on purpose: CaptureConfirmed has already released the tap by the
	// time it returns, so everything typed during validation, salt generation
	// and the file write lands in the tty queue — and TIOCSETAF is what throws
	// it away instead of handing it to the shell.
	//
	// Inside it the ORDER is load-bearing, and it is the reverse of the
	// obvious one. signal.Stop puts SIGTERM/SIGHUP back on their default
	// disposition — process death, no handler — so calling it while the tty is
	// still raw reopens, for the width of the two calls after it, exactly the
	// hole the handler was installed to close: a signal landing there kills the
	// process with ONLCR and ISIG cleared and leaves the user typing `stty
	// sane` blind. restore() therefore goes FIRST, signal.Stop last. The two
	// orderings that this leaves are both benign: a signal before restore() is
	// still caught by the goroutine, which restores the tty itself (TIOCSETAF
	// twice with the same termios is idempotent, and the second flush only
	// discards more of what the shell must not receive), and a signal after
	// restore() but before signal.Stop is either swallowed by an already-armed
	// handler or kills a process whose terminal is already sane. capCancel sits
	// between them so the goroutine cannot be left parked on a select whose
	// signal arm has just been unregistered.
	defer func() {
		restore()
		capCancel()
		signal.Stop(sigCh)
	}()

	// Every diagnostic from here to the deferred restore() is terminated with
	// CRLF for the same reason the prompt constants are: the tty is raw, ONLCR
	// is cleared, and a bare "\n" leaves the cursor in the column it was in, so
	// the shell prompt comes back indented under the tail of the message.
	steps, err := eventtap.CaptureConfirmed(capCtx, setPasswordPrompt(outW), log)
	if err != nil {
		msg, code := captureFailure(err)
		_, _ = fmt.Fprintf(errW, "dndmode: %s\r\n", msg)
		return code
	}
	// The captured plaintext lives exactly as long as the two calls below need
	// it. Nothing between here and the wipe prints it, and nothing keeps it.
	defer func() { clear(steps) }()

	if verr := validateCapturedCode(steps); verr != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v.\r\n", verr)
		return exitConfigErr
	}

	// No weak-code warning here, under ANY --debug. Routing it through the gate
	// would not be enough: --debug opens that writer, and the warning is
	// CONDITIONAL — it fires below config.WeakUnlockSteps and stays quiet at or
	// above it — so its mere appearance proves the new secret is shorter than
	// the threshold no matter how carefully the sentence avoids numbers. The
	// leak is in the condition, not the wording. The startup path keeps the
	// warning: there it describes a secret already sitting in the user's own
	// file. Its UX value is carried here by the unconditional recommendation in
	// the first prompt.

	// The last probe, immediately before the rename, still under the lock taken
	// ahead of the capture.
	//
	// Under that lock the two orderings that matter are already safe: a session
	// that published BEFORE the lock was taken was seen by the probe up there
	// and this branch never captured, and a session that starts after it blocks
	// at its own Step 13.3 until the save lands, then finds a live peer or a
	// stale fingerprint and refuses. So this call is not what closes the race —
	// it is the backstop for a publisher the lock cannot reach at all.
	//
	// flock coordinates the processes that take it, and one class of peer never
	// will: a session started from a dndmode binary that predates this lock and
	// is still running across the upgrade. It publishes runtime.json without
	// contending for anything, and it owns a tap this capture would sit in front
	// of. Only runtime.json shows it, and only a read this late shows it in
	// time — the probe before the capture ran minutes ago by then.
	//
	// Cheap enough to be worth it regardless: one stat-and-read of a file this
	// branch is about to make an irreversible decision against.
	if detail, code := probeLivePeer(runtimeMgr, log); code != exitOK {
		_, _ = fmt.Fprintf(errW, "dndmode: %s. %s\r\n", detail, peerAfterCapture)
		return code
	}

	// Read the abandonment flag and publish under one lock — see commitMu.
	// capCtx.Err() is checked here rather than right after CaptureConfirmed
	// because everything between the two is still abortable; the rename is the
	// first step that is not.
	commitMu.Lock()
	capErr := capCtx.Err()
	var serr error
	if capErr == nil {
		serr = loader.SaveUnlockHash(steps)
	}
	commitMu.Unlock()

	if capErr != nil {
		// Same sentence a signal during the capture itself produces: from the
		// user's side both are "I killed it and my config was left alone".
		msg, code := captureFailure(capErr)
		_, _ = fmt.Fprintf(errW, "dndmode: %s\r\n", msg)
		return code
	}
	if serr != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v\r\n", serr)
		return exitConfigErr
	}

	// Neither the length nor the value — only the fact and the file.
	_, _ = fmt.Fprintf(outW, resultUpdated, cfgPath)
	return exitOK
}
