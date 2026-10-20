//go:build darwin || linux

package runtime

import (
	"context"
	"errors"
	"syscall"
)

// The interfaces below are the platform seams RecoverFromCrash and
// IsLiveInstance need. They are declared HERE rather than imported from a
// platform package so this package builds on every OS dndmode supports:
// the macOS implementations (powerassert, focus, audiomute) and the Linux
// ones satisfy them structurally, with no import in either direction.
//
// Each interface names only the methods this package actually calls, so a
// platform type with a wider method set (powerassert.AssertionReleaser,
// focus.ShortcutsRunner, audiomute.VolumeRunner) is assignable as-is.

// LiveChecker reports whether a PID belongs to a running process.
type LiveChecker interface {
	IsAlive(pid int) bool
}

// AssertionReleaser releases a stored awake-lock by its id. On Linux the
// awake-lock dies with its owner, so the id is always 0 and this is never
// called with anything else.
type AssertionReleaser interface {
	Release(id uint32) error
}

// FocusRunner runs a named Do-Not-Disturb toggle ("dndmode-off" during
// recovery).
type FocusRunner interface {
	Run(ctx context.Context, name string) error
}

// VolumeSetter mutes or unmutes system audio.
type VolumeSetter interface {
	SetMuted(ctx context.Context, muted bool) error
}

// kernLiveChecker probes a PID with kill(pid, 0).
type kernLiveChecker struct{}

// NewKernLiveChecker returns a LiveChecker backed by kill(pid, 0). Same
// semantics as powerassert.NewKernLiveChecker on macOS: ESRCH is "dead",
// anything else (including EPERM — the process exists but belongs to
// someone else) is "alive", because a false "dead" would let two sessions
// run side by side.
func NewKernLiveChecker() LiveChecker { return kernLiveChecker{} }

// IsAlive implements LiveChecker.
func (kernLiveChecker) IsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// noopReleaser is an AssertionReleaser for platforms whose awake-lock is
// owned by a child process and cannot outlive dndmode.
type noopReleaser struct{}

// NewNoopReleaser returns an AssertionReleaser that releases nothing.
func NewNoopReleaser() AssertionReleaser { return noopReleaser{} }

// Release implements AssertionReleaser.
func (noopReleaser) Release(uint32) error { return nil }
