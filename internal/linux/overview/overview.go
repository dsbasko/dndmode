//go:build linux

// Package overview closes the GNOME Shell overview (Activities) before the
// shield goes up.
//
// While the overview is open GNOME Shell shows every window as a
// thumbnail, a fullscreen one included, so a shield created over it sits
// in the overview as a small window with the desktop open around it, and
// the input grab leaves no way to close the overview by hand. The shell's
// own D-Bus property OverviewActive is writable by any client of the
// session bus; writing false runs the same hide as Esc.
package overview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	dest       = "org.gnome.Shell"
	objectPath = "/org/gnome/Shell"
	propsIface = "org.freedesktop.DBus.Properties"
	prop       = "OverviewActive"

	// pollInterval and hideWait bound the wait for the hide animation
	// (about 250 ms in GNOME 46): the property stays true until it ends.
	pollInterval = 25 * time.Millisecond
	hideWait     = 2 * time.Second
)

// ErrStillOpen: the overview did not close within hideWait.
var ErrStillOpen = errors.New("overview: GNOME overview is still open")

// runFunc runs a command and returns its stdout; injectable for tests.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Runner reads and writes OverviewActive through gdbus.
type Runner struct {
	run  runFunc
	poll time.Duration
	wait time.Duration
}

// NewRunner returns the gdbus-backed Runner.
func NewRunner() *Runner {
	return &Runner{run: execRun, poll: pollInterval, wait: hideWait}
}

// active reports whether the overview is open.
func (r *Runner) active(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "gdbus", "call", "--session",
		"--dest", dest, "--object-path", objectPath,
		"--method", propsIface+".Get", dest, prop)
	if err != nil {
		return false, err
	}
	switch s := strings.TrimSpace(string(out)); s {
	case "(<true>,)":
		return true, nil
	case "(<false>,)":
		return false, nil
	default:
		return false, fmt.Errorf("overview: unexpected %s value %q", prop, s)
	}
}

// close asks the shell to hide the overview.
func (r *Runner) close(ctx context.Context) error {
	_, err := r.run(ctx, "gdbus", "call", "--session",
		"--dest", dest, "--object-path", objectPath,
		"--method", propsIface+".Set", dest, prop, "<false>")
	return err
}

// Hide closes the overview if it is open and waits until the hide
// animation is over, so the shield windows created next are laid out
// fullscreen and a glass shot does not catch the overview. A closed
// overview costs one read. An error means the shell could not be asked
// (not GNOME, no session bus) or did not close in time; the caller goes on
// anyway, the shield is still better than none.
func Hide(ctx context.Context, r *Runner) error {
	open, err := r.active(ctx)
	if err != nil || !open {
		return err
	}
	if err := r.close(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(r.wait)
	for {
		open, err := r.active(ctx)
		if err != nil || !open {
			return err
		}
		if time.Now().After(deadline) {
			return ErrStillOpen
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.poll):
		}
	}
}
