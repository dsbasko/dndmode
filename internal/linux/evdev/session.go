//go:build linux

// Package evdev is the Linux input lock: it takes an exclusive grab
// (EVIOCGRAB) of every /dev/input/event* device, so no key, click, scroll or
// touchpad gesture reaches the compositor, the console or any other reader —
// on Wayland and X11 alike — and matches the tail of what is typed against
// the unlock code.
//
// It is the counterpart of the macOS eventtap package, with the same shape:
// Install returns a Releaser for the RestoreState stack, a match sends one
// value on the supervisor's exit-trigger channel, and no keystroke ever
// leaks through, not even the keys of the unlock code itself.
//
// Key codes are translated to macOS kVK_* values at the boundary (keymap.go),
// so the shared hotkey / matcher / config packages and stored digests work
// unchanged.
//
// Access: the user needs read access to /dev/input/event*, which on Ubuntu
// means membership in the 'input' group.
package evdev

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/dsbasko/dndmode/internal/matcher"
)

// historyCap is the number of recent presses kept for tail matching: twice
// hotkey.MaxSteps, the same headroom the macOS ring has.
const historyCap = 64

// ErrEmptyUnlockCode: a verifier that can never match would raise a shield
// with no way to lower it.
var ErrEmptyUnlockCode = errors.New("evdev: unlock code has no steps")

// Releaser owns a session's input grab. It satisfies state.Releaser.
type Releaser struct {
	g    *grabber
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// Install grabs every input device and starts matching. Each time the tail
// of the typed presses satisfies v, one value is sent on sink (non-blocking:
// a full sink means a stop is already on its way).
func Install(v matcher.Verifier, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
	if v == nil || v.MaxLen() == 0 {
		return nil, ErrEmptyUnlockCode
	}
	g, err := openGrabber(log)
	if err != nil {
		return nil, err
	}
	r := &Releaser{g: g, stop: make(chan struct{}), done: make(chan struct{})}
	go r.loop(v, sink)
	return r, nil
}

// loop appends every press to the history and checks its tails.
func (r *Releaser) loop(v matcher.Verifier, sink chan<- struct{}) {
	defer close(r.done)
	hist := make([]matcher.KeyEvent, 0, historyCap)
	// The history holds the unlock code once it has been typed; wipe it on
	// the way out rather than leave it for the GC.
	defer func() { clear(hist[:cap(hist)]) }()
	for {
		select {
		case <-r.stop:
			return
		case ke := <-r.g.presses:
			hist = pushHistory(hist, ke)
			if matchAny(hist, v) {
				select {
				case sink <- struct{}{}:
				default:
				}
			}
		}
	}
}

// pushHistory appends ke, dropping the oldest entry when full.
func pushHistory(hist []matcher.KeyEvent, ke matcher.KeyEvent) []matcher.KeyEvent {
	if len(hist) == cap(hist) {
		copy(hist, hist[1:])
		hist = hist[:len(hist)-1]
	}
	return append(hist, ke)
}

// matchAny reports whether some tail of hist, of any length the verifier
// admits, is the unlock code.
func matchAny(hist []matcher.KeyEvent, v matcher.Verifier) bool {
	for l := max(v.MinLen(), 1); l <= v.MaxLen() && l <= len(hist); l++ {
		if v.Match(hist[len(hist)-l:]) {
			return true
		}
	}
	return false
}

// Name implements state.Releaser.
func (r *Releaser) Name() string { return "evdev" }

// Release drops every grab. Idempotent.
func (r *Releaser) Release() error {
	r.once.Do(func() {
		close(r.stop)
		<-r.done
		r.g.close()
	})
	return nil
}
