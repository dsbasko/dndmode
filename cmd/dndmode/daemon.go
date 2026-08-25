//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dsbasko/dndmode/internal/state"
	watchpkg "github.com/dsbasko/dndmode/internal/state/watch"
)

// `dndmode --watch` runs in the background. The terminal command is a
// LAUNCHER: it re-executes this same binary with the same arguments in its own
// session, waits until that process reports it is ready to receive presses,
// and returns. The background process is an ordinary one — visible in `ps` as
// `dndmode --watch`, described by `dndmode --status`, stopped by `dndmode
// --kill` or a plain SIGTERM — and there is still no launchd job: nothing
// restarts it, nothing starts it at login, and it dies with the machine.
//
// # How the two halves talk
//
// The child inherits the launcher's stdout and stderr, so everything it
// prints during startup — the --debug banners, a config error, the
// permission-wait status line — lands on the terminal exactly as it did when
// watch mode ran in the foreground. It also inherits ONE extra descriptor: the
// write end of a pipe, on readyFD. When the child has registered the
// activation combination and written its record it points its own stdout and
// stderr at the log file (so nothing it prints later can reach a terminal that
// may no longer exist — or, worse, a closed pipe whose SIGPIPE would kill it),
// writes readyToken to that descriptor and closes it. The launcher reads the
// pipe to EOF: the token means success; EOF without it means the child exited
// before it was ready, and the launcher reaps it and returns its exit code, so
// `dndmode --watch` fails with the same code and the same (already printed)
// diagnostic the foreground mode would have produced.
//
// Ctrl-C at the launcher while the child is still starting — typically while
// it waits for an Accessibility grant — is forwarded to the child as SIGTERM,
// which it handles like any session signal. Without that a launcher killed by
// Ctrl-C would leave a child waiting in the background that comes up as a
// daemon minutes later, when the user has long stopped expecting it.
//
// # Why re-exec and not fork
//
// A Go process with a cgo runtime and a main goroutine pinned to thread 0
// cannot fork() and carry on: only the calling thread survives in the child,
// and neither the Go scheduler nor AppKit tolerates that. Re-executing the
// binary starts a clean process that runs the ordinary startup path — every
// check the foreground mode made, it still makes, in the process that will
// actually hold the hotkey.

// watchDaemonEnv marks the re-executed child. The launcher sets it; nothing
// else does. Its presence plus a pipe on readyFD is what tells run() that it
// is the background half rather than the launcher.
const watchDaemonEnv = "DNDMODE_WATCH_DAEMON"

// readyFD is the descriptor the child inherits the ready pipe on: the first
// one after stdin/stdout/stderr, which is where os/exec's ExtraFiles start.
const readyFD = 3

// readyToken is what the child writes once it is ready. The launcher only
// checks for its presence, so the exact bytes are not a contract with anything
// but this file.
const readyToken = "ready\n"

// daemonExitGrace bounds how long the launcher waits for a child that is on
// its way out — one that reported nothing and closed the pipe, or one that was
// just sent SIGTERM — before it gives up and SIGKILLs it. The child exits
// promptly on every failure path; the bound exists so a wedged child cannot
// hang the terminal forever.
const daemonExitGrace = 15 * time.Second

const (
	// watchRecordRelPath is the user-relative path of the watch record, a
	// sibling of config.yml, runtime.json and runtime.lock.
	watchRecordRelPath = ".config/dndmode/watch.json"
	// watchLogRelPath is where the background process sends its stdout and
	// stderr. Truncated on every start; holds the same lines the foreground
	// mode used to print to the terminal, gated by --debug the same way.
	watchLogRelPath = ".config/dndmode/watch.log"
)

// spawnWatchDaemon is the launcher half: it re-executes this binary with the
// same arguments as a detached process, waits for its ready token, and returns
// the exit code `dndmode --watch` should report. On success the child is left
// running and this process returns exitOK without waiting on it.
//
// errW is UNGATED on purpose: the lines below are watch-mode lifecycle
// output, which bypasses the debug gate for the reason the watch banner does
// (see run()) — they name no secret, and a launcher that fails in total
// silence is indistinguishable from one that started a daemon.
func spawnWatchDaemon(errW io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot locate my own executable to start the watch process: %v\n", err)
		return exitPlatformErr
	}

	// Signals are forwarded, not handled: the child owns the startup, the
	// launcher only relays the user's wish to abort it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	env := append(os.Environ(), watchDaemonEnv+"=1")
	res := launchDetached(ctx, exe, os.Args[1:], env, os.Stdout, os.Stderr, daemonExitGrace)
	switch {
	case res.err != nil:
		_, _ = fmt.Fprintf(errW, "dndmode: cannot start the watch process: %v\n", res.err)
		return exitPlatformErr
	case res.ready:
		return exitOK
	case res.interrupted:
		_, _ = fmt.Fprintln(errW, "dndmode: interrupted — the watch process was stopped before it was ready.")
		if res.exitCode >= 0 {
			return res.exitCode
		}
		return exitPlatformErr
	case res.exitCode < 0:
		_, _ = fmt.Fprintf(errW, "dndmode: the watch process died before it was ready: %v\n", res.waitErr)
		return exitPlatformErr
	case res.exitCode == exitOK:
		// Exit 0 without the token is not a success: nothing is watching.
		_, _ = fmt.Fprintln(errW, "dndmode: the watch process exited without becoming ready; nothing is watching.")
		return exitPlatformErr
	default:
		// The child printed its own diagnostic on the inherited stderr (gated
		// by --debug exactly as the foreground mode was). Mirror its code.
		return res.exitCode
	}
}

// launchResult is what launchDetached learned about the child.
type launchResult struct {
	// err is set when the child could not be started at all.
	err error
	// ready: the child wrote readyToken and is running detached.
	ready bool
	// interrupted: ctx was cancelled before the child was ready; the child
	// was sent SIGTERM and reaped.
	interrupted bool
	// exitCode is the reaped child's exit code, -1 when it died by a signal.
	// Meaningful only when !ready && err == nil.
	exitCode int
	// waitErr is the error Wait returned for a reaped child (the ExitError for
	// a non-zero code or a signal death, or the grace timeout).
	waitErr error
	// pid of the child, for tests and diagnostics. Zero when err != nil.
	pid int
}

// launchDetached starts exe in its own session with the ready pipe on readyFD
// and waits until the child reports ready, exits, or ctx is cancelled. It is
// the mechanical half of spawnWatchDaemon, with no dndmode messages in it, so
// it can be exercised with /bin/sh in a unit test.
//
// stdout/stderr are handed to the child AS DESCRIPTORS (no copying goroutine),
// which is what lets the launcher exit while the child lives on.
func launchDetached(ctx context.Context, exe string, args, env []string, stdout, stderr *os.File, grace time.Duration) launchResult {
	r, w, err := os.Pipe()
	if err != nil {
		return launchResult{err: fmt.Errorf("ready pipe: %w", err)}
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = env
	// A daemon must not pin the directory it was started from (a volume the
	// user wants to eject, a checkout they want to delete). Every path dndmode
	// uses is absolute under $HOME.
	cmd.Dir = "/"
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{w} // becomes readyFD in the child
	// Own session: no controlling terminal, so closing the terminal window
	// later does not SIGHUP the daemon, and Ctrl-C in that terminal does not
	// reach it (the launcher forwards a deliberate abort as SIGTERM instead).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return launchResult{err: err}
	}
	// The child holds its own copy now. Ours must go, or the pipe never
	// reaches EOF while this process lives.
	_ = w.Close()

	// The reader stops at the TOKEN, not at EOF. A subprocess the child ran
	// before it was ready (crash recovery's osascript, the Shortcuts check)
	// can have inherited the pipe's write end, and waiting for every holder
	// to close it would block the terminal on a process that has nothing to
	// do with the protocol. EOF without the token is the failure signal.
	readyCh := make(chan bool, 1)
	go func() {
		defer close(readyCh)
		defer func() { _ = r.Close() }()
		var got []byte
		chunk := make([]byte, 64)
		for {
			n, err := r.Read(chunk)
			got = append(got, chunk[:n]...)
			if bytes.Contains(got, []byte(readyToken)) {
				readyCh <- true
				return
			}
			if err != nil {
				readyCh <- false
				return
			}
		}
	}()

	ready := false
	interrupted := false
	select {
	case ready = <-readyCh:
	case <-ctx.Done():
		interrupted = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	if ready && !interrupted {
		// Deliberately no Wait: the child is reparented to launchd when this
		// process exits, and launchd reaps it. Waiting here would keep the
		// terminal blocked for the daemon's whole life, which is the very
		// thing this function exists to avoid.
		return launchResult{ready: true, pid: cmd.Process.Pid}
	}

	code, werr := waitBounded(cmd, grace)
	if interrupted {
		// Drain the reader so its goroutine is not left blocked on a pipe the
		// dead child can no longer close.
		<-readyCh
	}
	return launchResult{interrupted: interrupted, exitCode: code, waitErr: werr, pid: cmd.Process.Pid}
}

// waitBounded reaps cmd, SIGKILLing it if it is still alive after grace.
// Returns the exit code (-1 for a signal death) and Wait's error.
func waitBounded(cmd *exec.Cmd, grace time.Duration) (int, error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return cmd.ProcessState.ExitCode(), err
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		err := <-done
		return cmd.ProcessState.ExitCode(), fmt.Errorf("still running after %s, killed: %w", grace, err)
	}
}

// daemonReadyPipe tells the background half from the launcher. It returns
// (nil, false, nil) in an ordinary process; (pipe, true, nil) in a child the
// launcher started; and an error when watchDaemonEnv is set but readyFD is
// not the pipe the launcher would have put there — which means somebody set
// the variable by hand, and running as a daemon with nobody to report to would
// be worse than refusing.
func daemonReadyPipe() (*os.File, bool, error) {
	if os.Getenv(watchDaemonEnv) == "" {
		return nil, false, nil
	}
	if err := checkReadyFD(readyFD); err != nil {
		return nil, true, fmt.Errorf("%s is set but %v; unset it — `dndmode --watch` sets it for the process it starts", watchDaemonEnv, err)
	}
	// The launcher's ExtraFiles cleared close-on-exec so the descriptor
	// survived the exec into this process; restore it so no subprocess this
	// process runs before it is ready (osascript, shortcuts) inherits the
	// pipe and holds it open past our close.
	unix.CloseOnExec(readyFD)
	return os.NewFile(readyFD, "dndmode-ready"), true, nil
}

// checkReadyFD verifies fd is an open pipe. Split out so the check can be
// unit-tested on descriptors other than readyFD.
func checkReadyFD(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("fd %d is not open (%w)", fd, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFIFO {
		return fmt.Errorf("fd %d is not a pipe", fd)
	}
	return nil
}

// redirectFDs points every fd in fds at the open file f (dup2). Used by the
// background half to send its stdout and stderr to the log file; os.Stdout
// and os.Stderr keep working because they wrap the descriptor NUMBERS, which
// do not change.
func redirectFDs(f *os.File, fds ...int) error {
	for _, fd := range fds {
		if err := unix.Dup2(int(f.Fd()), fd); err != nil {
			return fmt.Errorf("dup2 onto fd %d: %w", fd, err)
		}
	}
	return nil
}

// errAlreadyWatching is the under-lock refusal from becomeWatchDaemon: a live
// watch process published its record between the Step 5d pre-check and now.
type errAlreadyWatching struct{ pid int }

func (e errAlreadyWatching) Error() string {
	return fmt.Sprintf("already watching (PID %d)", e.pid)
}

// daemonDeps carries what becomeWatchDaemon needs from run().
type daemonDeps struct {
	// dir is ~/.config/dndmode: home of the record, the log and the publish
	// lock.
	dir string
	// record is the Manager for watch.json, constructed at Step 5c.
	record *watchpkg.Manager
	// comboLabel is the activation combination in the user's own spelling —
	// public, printed in the banner and stored in the record.
	comboLabel string
	// ready is the pipe to the launcher.
	ready *os.File
	// rs is the shell-level cleanup stack; the record's Manager is pushed on
	// it so the file goes on exit.
	rs *state.RestoreState
	// term is the inherited terminal, for the one banner the user sees.
	term io.Writer
	// errW is the ungated stderr for refusals that happen before the redirect.
	errW io.Writer
	log  *slog.Logger
	// prober defaults to the kernel one; tests inject a fake.
	prober watchpkg.Prober
}

// becomeWatchDaemon turns the already-registered watch process into the
// background one: it publishes the record, opens the log, prints the banner
// to the terminal, redirects its own stdout/stderr to the log and tells the
// launcher it is ready. On any failure it returns the exit code for run() to
// return; the launcher mirrors it.
//
// Called AFTER globalhotkey.Register, so a combination that is taken is
// reported before any record exists, and after rs holds the registration, so
// the cleanup that follows a failure here releases it.
func becomeWatchDaemon(d daemonDeps) int {
	prober := d.prober
	if prober == nil {
		prober = watchpkg.NewKernProber()
	}

	logPath := filepath.Join(d.dir, filepath.Base(watchLogRelPath))
	rec := watchpkg.Record{
		PID:            os.Getpid(),
		StartedAt:      time.Now().UTC(),
		ActivateHotkey: d.comboLabel,
		LogPath:        logPath,
	}
	if start, err := prober.StartTime(rec.PID); err != nil {
		// Not fatal: the record then identifies the process by PID alone, and
		// Inspect's fallback handles it. Logged so the weaker identity is
		// visible to anyone reading the log.
		d.log.Warn("cannot read own start time; the watch record will identify this process by PID only", slog.Any("err", err))
	} else {
		// UTC like StartedAt, so the two timestamps in the file read alike;
		// Inspect compares instants, so the zone is cosmetic.
		rec.ProcStartedAt = start.UTC()
	}

	// Under the publish lock for the same reason runtime.json is: two
	// `--watch` started together both pass the Step 5d pre-check (neither has
	// written yet), and without the re-check here the second would overwrite
	// the first's record and leave a live watch process that `--status` cannot
	// see and `--kill` cannot reach.
	err := withPublishLock(d.dir, func() error {
		st, ierr := watchpkg.Inspect(d.record, prober)
		switch {
		case ierr != nil:
			d.log.Warn("watch re-check inconclusive", slog.Any("err", ierr))
		case st.Running:
			return errAlreadyWatching{pid: st.Record.PID}
		}
		return d.record.Write(rec)
	})
	_, already := errors.AsType[errAlreadyWatching](err)
	switch {
	case err == nil:
	case already:
		_, _ = fmt.Fprintf(d.errW, "dndmode: %v. Run `dndmode --kill` to stop it first.\n", err)
		return exitConcurrentInstance
	case errors.Is(err, errPublishLockBusy):
		_, _ = fmt.Fprintln(d.errW,
			"dndmode: another dndmode is publishing its state, or a --set-password is setting a new unlock code. Wait for it to finish, then re-run.")
		return exitConcurrentInstance
	case errors.Is(err, errPublishLockUnusable):
		_, _ = fmt.Fprintf(d.errW,
			"dndmode: cannot claim the publish lock, so a concurrent dndmode --watch cannot be ruled out: %v.\nCheck that %s is writable, then re-run.\n",
			err, d.dir)
		return exitRuntimeJSON
	default:
		_, _ = fmt.Fprintf(d.errW, "dndmode: write %s failed: %v. Check that %s is writable, then re-run.\n", d.record.Path(), err, d.dir)
		return exitPlatformErr
	}
	d.rs.Push(d.record) // removed on exit, before the hotkey registration is released

	// 0600 like every other file in the directory. Truncated: the log
	// describes THIS process, and a `--status` pointing at it should not
	// show a previous daemon's lines first.
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		_, _ = fmt.Fprintf(d.errW, "dndmode: cannot open %s: %v. Check that %s is writable, then re-run.\n", logPath, err, d.dir)
		return exitPlatformErr
	}
	defer func() { _ = logFile.Close() }()

	// The last thing the terminal sees from this process, in the same shape
	// --status answers with. Ungated, like the foreground banner it replaces:
	// it names the activation combination, which is public, and a launcher
	// that returns silently would leave the user unsure whether anything is
	// watching.
	_, _ = fmt.Fprintln(d.term, "dndmode: watching in the background")
	kvLine(d.term, "pid", strconv.Itoa(rec.PID))
	kvLine(d.term, "hotkey", d.comboLabel+"  — press it to lock")
	kvLine(d.term, "log", abbreviateHome(logPath))
	kvLine(d.term, "manage", "dndmode --status · dndmode --kill")

	if err := redirectFDs(logFile, int(os.Stdout.Fd()), int(os.Stderr.Fd())); err != nil {
		_, _ = fmt.Fprintf(d.errW, "dndmode: cannot redirect output to %s: %v\n", logPath, err)
		return exitPlatformErr
	}
	// From here on os.Stdout / os.Stderr are the log file.
	_, _ = fmt.Fprintln(os.Stdout, watchingBanner(d.comboLabel))

	if _, err := io.WriteString(d.ready, readyToken); err != nil {
		// The launcher is gone (killed, or its terminal closed). There is
		// nobody to report to, but nothing about this process's ability to
		// watch has changed; carry on, and say so in the log.
		d.log.Warn("launcher did not receive the ready signal", slog.Any("err", err))
	}
	_ = d.ready.Close()
	return exitOK
}

// watchingBanner is the line the watch process prints when it (re)enters the
// waiting state — once at start and after every session. It replaces the
// foreground-era "Ctrl-C to quit", which no longer applies to a process with
// no terminal.
func watchingBanner(comboLabel string) string {
	return fmt.Sprintf("dndmode: watching. press %s to lock, `dndmode --kill` to stop.", comboLabel)
}
