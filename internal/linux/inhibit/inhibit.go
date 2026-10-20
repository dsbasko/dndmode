//go:build linux

// Package inhibit is the Linux awake-lock: it keeps the session from going
// idle (screen blank and lock) and the machine from suspending for as long
// as dndmode runs. It is the counterpart of both macOS awake paths — the
// IOPMAssertion of a full session and the caffeinate child of none mode —
// because on Linux one mechanism covers both.
//
// The lock is held by a child process chain:
//
//	systemd-inhibit --what=idle:sleep ... \
//	  sh -c 'gnome-session-inhibit --inhibit-only ... & tail --pid=<dndmode pid> ...; kill $!'
//
// systemd-inhibit takes the logind inhibitor, gnome-session-inhibit the
// GNOME one (GNOME blanks and locks the screen on its own idle timer and
// does not look at logind for that). Either is used alone when the other is
// missing. `tail --pid` exits within a second of dndmode dying, so a
// kill -9 never leaves the machine stuck awake — the job `caffeinate -w`
// does on macOS — and there is nothing for crash recovery to release.
//
// gnome-session-inhibit runs with --inhibit-only under a small sh rather
// than as the parent of tail: given a command, it moves that command into a
// process group of its own, which Release could then not reach.
package inhibit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Reason is shown by `systemd-inhibit --list` and GNOME's inhibitor list.
const Reason = "dndmode active"

// releaseGrace is how long Release waits after SIGTERM before SIGKILL.
var releaseGrace = 2 * time.Second

// lookPath is exec.LookPath, injectable for tests.
var lookPath = exec.LookPath

// ErrNoInhibitor: neither systemd-inhibit nor gnome-session-inhibit exists.
var ErrNoInhibitor = errors.New("inhibit: neither systemd-inhibit nor gnome-session-inhibit is installed")

// Process is a running inhibitor chain. It satisfies state.Releaser.
type Process struct {
	cmd  *exec.Cmd
	log  *slog.Logger
	done chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	waitErr  error
}

// buildArgv assembles the inhibitor chain for pid. allowDisplaySleep drops
// "idle" from both inhibitors: the screen may then blank (and GNOME may lock
// it) while the system itself stays up.
func buildArgv(pid int, allowDisplaySleep bool) ([]string, error) {
	tailPath, err := lookPath("tail")
	if err != nil {
		return nil, fmt.Errorf("inhibit: tail not found: %w", err)
	}
	argv := []string{tailPath, "--pid=" + strconv.Itoa(pid), "-f", "/dev/null"}

	gsiWhat, sysWhat := "idle:suspend", "idle:sleep"
	if allowDisplaySleep {
		gsiWhat, sysWhat = "suspend", "sleep"
	}
	found := false
	if p, err := lookPath("gnome-session-inhibit"); err == nil && inGNOME() {
		shPath, err := lookPath("sh")
		if err != nil {
			return nil, fmt.Errorf("inhibit: sh not found: %w", err)
		}
		argv = []string{shPath, "-c", gsiScript, strconv.Itoa(pid),
			p, "--inhibit", gsiWhat, "--app-id", "dndmode", "--reason", Reason, "--inhibit-only"}
		found = true
	}
	if p, err := lookPath("systemd-inhibit"); err == nil {
		argv = append([]string{p, "--what=" + sysWhat, "--who=dndmode", "--why=" + Reason, "--mode=block"}, argv...)
		found = true
	}
	if !found {
		return nil, ErrNoInhibitor
	}
	return argv, nil
}

// gsiScript starts the GNOME inhibitor ("$@") in the background and waits
// on tail for dndmode's PID ($0); when tail returns, the inhibitor goes too.
const gsiScript = `"$@" & g=$!
tail --pid="$0" -f /dev/null
kill "$g" 2>/dev/null`

// inGNOME reports whether the session is GNOME (Ubuntu's default).
func inGNOME() bool {
	return strings.Contains(strings.ToUpper(os.Getenv("XDG_CURRENT_DESKTOP")), "GNOME")
}

// Start launches the inhibitor chain tied to pid (dndmode's own PID). The
// chain runs in its own process group so Release can stop every member, not
// just the head.
func Start(ctx context.Context, pid int, allowDisplaySleep bool, log *slog.Logger) (*Process, error) {
	if log == nil {
		log = slog.Default()
	}
	argv, err := buildArgv(pid, allowDisplaySleep)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = releaseGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}

	p := &Process{cmd: cmd, log: log, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	log.Debug("inhibitor started", slog.Int("child_pid", cmd.Process.Pid), slog.Any("argv", argv))
	return p, nil
}

// Name implements state.Releaser.
func (p *Process) Name() string { return "inhibit" }

// Done is closed when the chain has exited, for whatever reason.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is the chain's wait error; meaningful after Done.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

// Release stops the whole chain: SIGTERM to the process group, SIGKILL
// after releaseGrace. Idempotent.
func (p *Process) Release() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	pgid := p.cmd.Process.Pid
	p.stopOnce.Do(func() {
		if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
			p.log.Debug("inhibitor SIGTERM failed (likely already exited)", slog.Any("err", err))
		}
	})
	select {
	case <-p.done:
	case <-time.After(releaseGrace):
		p.log.Warn("inhibitor did not exit on SIGTERM; sending SIGKILL")
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	}
	return nil
}
