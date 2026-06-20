//go:build darwin

package caffeinate

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// binPath is the absolute path to the caffeinate binary. It is a package var
// (not a const) solely so internal tests can point it at a stub script to
// exercise the Start/Release lifecycle hermetically; production never reassigns
// it. /usr/bin/caffeinate is part of the base macOS install (see man caffeinate
// LOCATION), so an absolute path avoids $PATH ambiguity.
var binPath = "/usr/bin/caffeinate"

// releaseGrace bounds how long Release (and the exec.CommandContext watcher via
// cmd.WaitDelay) waits for a SIGTERM'd caffeinate to exit before escalating to
// SIGKILL. caffeinate drops its assertion and exits within milliseconds of
// SIGTERM, so this ceiling is never normally reached. It is a var (not a const)
// only so the SIGKILL-escalation unit test can lower it; production never
// reassigns it.
var releaseGrace = 2 * time.Second

// Process wraps a running caffeinate child. It implements the
// state.Releaser interface (Name/Release) so RestoreState can tear it down in
// LIFO order alongside the other dndmode resources.
type Process struct {
	cmd  *exec.Cmd
	log  *slog.Logger
	done chan struct{} // closed when the wait goroutine reaps the child

	stopOnce sync.Once
	mu       sync.Mutex
	waitErr  error
}

// buildArgs assembles the caffeinate flag set. Kept pure (no IO) so it is
// fully unit-testable.
//
//   - -i  prevent system idle sleep (always)
//   - -s  prevent system sleep — honored only on AC power (always; harmless on
//     battery, matches the user's existing `-dis` habit)
//   - -d  prevent display sleep — included UNLESS the operator opted into
//     allow_display_sleep (mirrors the powerassert assertion-type polarity so
//     the existing config toggle keeps its meaning in none mode)
//   - -w <pid>  release the assertion when dndmode's PID exits (crash safety)
//
// -m (disk) and -u (a 5s user-active pulse, useless for a persistent hold) are
// intentionally omitted.
func buildArgs(pid int, allowDisplaySleep bool) []string {
	args := make([]string, 0, 5)
	if !allowDisplaySleep {
		args = append(args, "-d")
	}
	args = append(args, "-i", "-s", "-w", strconv.Itoa(pid))
	return args
}

// Start launches caffeinate as a child bound to ctx and returns a Process
// handle. pid is dndmode's own PID (os.Getpid), threaded into caffeinate's -w.
// ctx binding is a secondary safety net: if ctx is cancelled (SIGINT/SIGTERM/
// SIGHUP) the os/exec watcher kills the child even before Release runs.
func Start(ctx context.Context, pid int, allowDisplaySleep bool, log *slog.Logger) (*Process, error) {
	args := buildArgs(pid, allowDisplaySleep)
	cmd := exec.CommandContext(ctx, binPath, args...)
	// On ctx-cancel (SIGINT/SIGTERM/SIGHUP) tear the child down GRACEFULLY. The
	// default exec.CommandContext cancel sends SIGKILL at once, which would
	// pre-empt the SIGTERM teardown Release documents; override to SIGTERM and
	// let WaitDelay escalate to SIGKILL only if caffeinate wedges. cmd.Process is
	// nil now but the closure reads it lazily — the watcher only invokes Cancel
	// after a successful Start, by which point Process is set.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = releaseGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s %v: %w", binPath, args, err)
	}

	p := &Process{cmd: cmd, log: log, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(p.done)
	}()

	log.Debug("caffeinate started",
		slog.Int("child_pid", cmd.Process.Pid),
		slog.Any("args", args))
	return p, nil
}

// Name identifies this releaser in the RestoreState cleanup log.
func (p *Process) Name() string { return "caffeinate" }

// Done is closed once the caffeinate child has exited and been reaped — whether
// via Release, ctx-cancel, the -w watch, or an external kill. Callers (main.go's
// none path) select on it to notice an unexpected early death.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err returns the error from the child's Wait, or nil if it has not exited yet.
// A non-nil value after a deliberate stop (e.g. "signal: terminated") is
// expected, not a failure.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

// Release stops the caffeinate child and blocks until it is reaped. It is
// idempotent (safe to call after the child already exited) and never reports a
// deliberate-termination Wait error as a failure — its contract is "the
// assertion is gone", which a dead child satisfies regardless of how it died.
//
// On the common dndmode shutdown path the child is usually ALREADY gone by the
// time Release runs: a ctx-cancel makes the exec.CommandContext watcher fire
// cmd.Cancel (our SIGTERM) first, so Release just observes the closed done
// channel via the fast path. The explicit SIGTERM below covers a direct Release
// with no ctx-cancel (e.g. an unexpected-death cleanup).
func (p *Process) Release() error {
	select {
	case <-p.done:
		return nil // already exited (ctx-cancel, -w watch, or prior Release)
	default:
	}

	p.stopOnce.Do(func() {
		// SIGTERM: caffeinate drops its assertion and exits promptly. Ignore
		// the error — the only realistic cause is a race where the child just
		// exited, which the <-p.done wait below resolves either way.
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			p.log.Debug("caffeinate SIGTERM failed (likely already exited)", slog.Any("err", err))
		}
	})

	select {
	case <-p.done:
	case <-time.After(releaseGrace):
		p.log.Warn("caffeinate did not exit on SIGTERM; sending SIGKILL")
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// Compile-time check: *Process satisfies state.Releaser (Name/Release) without
// importing the state package, which would create an import cycle. Mirrors
// powerassert/assertion.go and focus/releaser.go.
var _ interface {
	Release() error
	Name() string
} = (*Process)(nil)
