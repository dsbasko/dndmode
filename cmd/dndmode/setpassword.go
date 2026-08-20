//go:build darwin

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/eventtap"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
	"github.com/dsbasko/dndmode/internal/macos/powerassert"
	"github.com/dsbasko/dndmode/internal/matcher"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
)

// setPasswordFlags carries the flags --set-password refuses to be combined
// with. It exists so the mutual-exclusion rule is one pure function over four
// strings rather than a chain of pointer dereferences inside run().
//
// --debug is deliberately absent: it does not change what the command DOES,
// only whether its diagnostics reach the terminal, and diagnosing a
// --set-password that exits 1 without a word is exactly what it is for.
type setPasswordFlags struct {
	style string
	timer string
	mute  string
	focus string
}

// conflictingFlag returns the name of the first flag that cannot be combined
// with --set-password, or "" when the combination is legal.
//
// Every one of them configures a dndmode SESSION — the overlay look, the
// auto-disable deadline, audio muting, Focus. --set-password starts no
// session: it rewrites one key in the config file and exits. Silently ignoring
// them would be worse than refusing, because `dndmode --set-password --timer
// 30m` reads like "capture, then run for 30 minutes" and would instead capture
// and quit.
func (f setPasswordFlags) conflictingFlag() string {
	switch {
	case f.style != "":
		return "--style"
	case f.timer != "":
		return "--timer"
	case f.mute != "":
		return "--mute"
	case f.focus != "":
		return "--focus"
	default:
		return ""
	}
}

// Prompt and result lines, terminated with CRLF rather than LF.
//
// All three are written while the tty is in raw mode: term.MakeRaw clears
// ONLCR, so a bare "\n" moves the cursor down WITHOUT returning it to column
// zero and the second prompt starts under the tail of the first. The success
// line is printed before the deferred restore too (the restore has to outlive
// the config write — see runSetPasswordAt), so it needs the same treatment.
const (
	promptFirstPass = "Type your unlock sequence, then press Return. " +
		"Esc cancels. Nothing is echoed. %d or more steps recommended.\r\n"
	promptSecondPass = "Type it again to confirm.\r\n"
	resultUpdated    = "dndmode: unlock code updated in %s\r\n"
)

// setPasswordPrompt builds the callback eventtap.CaptureConfirmed invokes
// between its two passes. It writes to the UNGATED writer: these two lines and
// the success line are the only output of this command that is not routed
// through the debug gate, and a prompt nobody can see would leave the operator
// staring at a dead terminal whose keyboard has stopped responding.
//
// The recommendation is unconditional — the same sentence for a three-step
// code and for a thirty-step one — which is what makes it safe to print. The
// conditional weak-code warning main() shows at startup is deliberately NOT
// reproduced here: see the note in runSetPasswordAt.
//
// The threshold is interpolated from config.WeakUnlockSteps rather than spelled
// as a digit so the advice cannot drift away from the constant the rest of the
// project judges codes by.
func setPasswordPrompt(w io.Writer) func(pass int) {
	return func(pass int) {
		if pass == 1 {
			_, _ = fmt.Fprintf(w, promptFirstPass, config.WeakUnlockSteps)
			return
		}
		_, _ = fmt.Fprint(w, promptSecondPass)
	}
}

// validateCapturedCode is config.ValidateUnlockCode's verdict rephrased
// without the step count.
//
// The stock message embeds len(steps) twice ("unlock code of %d steps is too
// short … so a %d-step code is exhausted in minutes"). On the STARTUP path that
// is harmless — it rides the debug gate and describes a secret the user can
// read in their own file. Printed right after a capture it would put the length
// of the secret that was just typed into the terminal scrollback, into tmux
// capture buffers and onto any screen share that happens to be running, which
// is the leak this whole feature exists to close. The user already knows how
// many keys they pressed; the terminal must not.
//
// The thresholds themselves stay in the text: MinUnlockSteps and
// WeakUnlockSteps are public, documented in the README and in the config
// template, and a refusal that does not say what would be accepted just makes
// the user guess with the keyboard held hostage.
//
// The verdict is delegated rather than re-derived: config.ValidateUnlockCode
// stays the single source of truth for WHICH codes are acceptable, and this
// function only decides how to say no.
func validateCapturedCode(steps []hotkey.Spec) error {
	if err := config.ValidateUnlockCode(steps); err == nil {
		return nil
	}
	switch {
	case len(steps) == 0:
		return errors.New("nothing was captured: an unlock code needs at least one step")
	case len(steps) == 1:
		return fmt.Errorf(
			"a single-step unlock code must carry at least one modifier (e.g. Ctrl+Option+Cmd+X); "+
				"a bare key would unlock on the first thing a bystander types — "+
				"use %d or more steps instead",
			config.MinUnlockSteps)
	default:
		return fmt.Errorf(
			"that unlock code is too short: every keypress is a fresh match attempt, "+
				"so use at least %d steps (%d or more recommended)",
			config.MinUnlockSteps, config.WeakUnlockSteps)
	}
}

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

// prepareConfigForCapture runs everything that can reject a config BEFORE
// anyone is asked to type: the symlink inspection, the load (which creates a
// default on first run) and a dry run of the line surgery.
//
// # Lstat runs ahead of Load, and both of its outcomes are named
//
// Load() reads through os.ReadFile, so a config.yml that is a DANGLING symlink
// looks exactly like a missing file to it: it takes the fs.ErrNotExist branch,
// writes a default config and renames it over the link, destroying the link
// entry without a word. SaveUnlockHash carries its own dangling-link guard,
// but by then Load has already run and there is no link left to refuse. So the
// check has to happen here, first.
//
// The other branch matters just as much. fs.ErrNotExist from Lstat is the
// ordinary first run, and it MUST fall through to Load so the default config
// gets created. A reflexive `if _, err := os.Lstat(p); err != nil { return … }`
// would give every new user a --set-password that exits 1 and prints nothing
// (all of this branch's errors ride the debug gate), and no test running on a
// machine that already has a config would ever notice.
//
// # The dry run is only the structural half of the verification
//
// config.VerifyStructure, not the full pre-rename check: at this point there
// are no captured steps by construction and the salt/hash are placeholders, so
// "does this digest recognize what was typed" is unsatisfiable and folding it
// in would make the dry run fail on every single invocation.
//
// What it does catch is a config whose YAML shape the surgery cannot handle —
// a quoted key, a uniformly indented root mapping, flow style. Such a file has
// to be rejected while rejecting it is still cheap. The alternative is finding
// out after the user has typed a secret twice under a tap that owns the
// keyboard, which is the worst possible moment.
func prepareConfigForCapture(cfgPath string) (*config.Loader, config.Config, bool, error) {
	var zero config.Config

	fi, err := os.Lstat(cfgPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Ordinary first run — fall through to Load, which creates the default.
	case err != nil:
		return nil, zero, false, fmt.Errorf("cannot inspect config %s: %w", cfgPath, err)
	case fi.Mode()&os.ModeSymlink != 0:
		if _, serr := os.Stat(cfgPath); serr != nil {
			return nil, zero, false, fmt.Errorf(
				"config %s is a symlink whose target cannot be read: %w — "+
					"repoint or remove the link, then re-run", cfgPath, serr)
		}
	}

	loader := config.NewLoader(cfgPath)
	// cfg travels back to the caller for ONE field: Debug. This branch runs
	// ahead of run()'s Step 5, so `debug: true` in the file has not raised the
	// output gate yet and every diagnostic below would be discarded on a
	// machine whose owner asked for output in the config rather than on the
	// command line. Nothing else here reads cfg — the surgery works on raw
	// bytes on purpose.
	cfg, created, err := loader.Load()
	if err != nil {
		return nil, zero, false, err
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, cfg, created, fmt.Errorf("read config %s: %w", cfgPath, err)
	}
	// Placeholders of the real widths, so the dry run exercises the same
	// decode gate RewriteSecretAsHash applies to the values it will really
	// write. They are never persisted: the rewritten bytes are inspected and
	// dropped.
	saltB64 := base64.StdEncoding.EncodeToString(make([]byte, matcher.SaltLen))
	hashB64 := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))
	newRaw, err := config.RewriteSecretAsHash(raw, saltB64, hashB64)
	if err != nil {
		return nil, cfg, created, fmt.Errorf("cannot rewrite %s: %w", cfgPath, err)
	}
	if verr := config.VerifyStructure(raw, newRaw, saltB64, hashB64); verr != nil {
		return nil, cfg, created, fmt.Errorf("cannot rewrite %s: %w", cfgPath, verr)
	}
	return loader, cfg, created, nil
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
	// owner. Same triple, same warn-not-fatal read failure and same exit 5 as
	// run() Step 5c — a peer check that disagreed with the session path about
	// what "already running" means would be worse than none.
	//
	// After the config work so `debug: true` can explain the refusal, and
	// before the tty check, the grant wait and anything that installs a tap.
	runtimeMgr := runtimepkg.NewManager(filepath.Join(filepath.Dir(cfgPath), filepath.Base(runtimeRelPath)), log)
	if alive, peerPID, lerr := runtimepkg.IsLiveInstance(runtimeMgr, powerassert.NewKernLiveChecker(), log); lerr != nil {
		log.Warn("pre-check inconclusive", slog.Any("err", lerr))
	} else if alive {
		_, _ = fmt.Fprintf(errW,
			"dndmode: another instance is already active (PID=%d) — capturing a new code now would take the keyboard away from it. End that session first, then re-run.\n",
			peerPID)
		return exitConcurrentInstance
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
	go func() {
		select {
		case <-sigCh:
			// Terminal first, then unwind: cancelling capCtx makes
			// CaptureConfirmed return, and its own defer takes the tap down.
			restore()
			capCancel()
		case <-capCtx.Done():
		}
	}()
	// One defer for the raw mode AND the handler it needs, registered only now
	// that MakeRaw has actually succeeded. It runs AFTER the config is written
	// (below) on purpose: CaptureConfirmed has already released the tap by the
	// time it returns, so everything typed during validation, salt generation
	// and the file write lands in the tty queue — and TIOCSETAF is what throws
	// it away instead of handing it to the shell.
	defer func() {
		signal.Stop(sigCh)
		capCancel()
		restore()
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

	if serr := loader.SaveUnlockHash(steps); serr != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v\r\n", serr)
		return exitConfigErr
	}

	// Neither the length nor the value — only the fact and the file.
	_, _ = fmt.Fprintf(outW, resultUpdated, cfgPath)
	return exitOK
}
