//go:build linux

package evdev

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// Bounds of one capture pass; identical to the macOS values.
const (
	captureIdleTimeout  = 10 * time.Second
	captureTotalTimeout = 60 * time.Second
)

// Terminator key codes (kVK_*).
const (
	keyReturn = 0x24
	keyEscape = 0x35
)

// Capture errors. None of them carries a key code or a length: they are
// printed, and the sequence is a secret.
var (
	ErrCaptureCancelled      = errors.New("evdev: unlock sequence capture cancelled")
	ErrCaptureTooLong        = errors.New("evdev: unlock sequence has too many steps")
	ErrCaptureTimedOut       = errors.New("evdev: unlock sequence capture timed out")
	ErrCaptureMismatch       = errors.New("evdev: the two entries did not match")
	ErrCaptureUnsupportedKey = errors.New("evdev: a key that cannot be part of an unlock code was pressed")
)

// CaptureConfirmed grabs the input devices and records an unlock sequence
// twice; it returns the steps only when both entries agree. prompt(1) and
// prompt(2) are called before each pass.
//
// While it runs, every keystroke is swallowed — including Ctrl-C — so the
// capture is bounded by Esc, an idle timeout, a per-pass ceiling and the
// step limit.
func CaptureConfirmed(ctx context.Context, prompt func(pass int), log *slog.Logger) ([]hotkey.Spec, error) {
	g, err := openGrabber(log)
	if err != nil {
		return nil, err
	}
	defer g.close()
	return confirmPasses(ctx, g.presses, prompt, captureIdleTimeout, captureTotalTimeout)
}

// confirmPasses runs two collectSteps passes and compares them.
func confirmPasses(ctx context.Context, presses <-chan matcher.KeyEvent, prompt func(int), idle, total time.Duration) ([]hotkey.Spec, error) {
	if prompt == nil {
		prompt = func(int) {}
	}
	prompt(1)
	first, err := collectSteps(ctx, presses, idle, total)
	if err != nil {
		return nil, err
	}
	prompt(2)
	second, err := collectSteps(ctx, presses, idle, total)
	if err != nil {
		clear(first)
		return nil, err
	}
	defer clear(second)
	if !slices.Equal(first, second) {
		clear(first)
		return nil, ErrCaptureMismatch
	}
	return first, nil
}

// collectSteps records presses until an unmodified Return. An unmodified
// Escape cancels. With a modifier held, both are ordinary steps.
func collectSteps(ctx context.Context, presses <-chan matcher.KeyEvent, idle, total time.Duration) ([]hotkey.Spec, error) {
	totalT := time.NewTimer(total)
	defer totalT.Stop()
	idleT := time.NewTimer(idle)
	defer idleT.Stop()

	var steps []hotkey.Spec
	fail := func(err error) ([]hotkey.Spec, error) {
		clear(steps)
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-totalT.C:
			return fail(ErrCaptureTimedOut)
		case <-idleT.C:
			return fail(ErrCaptureTimedOut)
		case ke := <-presses:
			idleT.Reset(idle)
			mods := ke.Modifiers & matcher.UserIntentionalMask
			switch {
			case mods == 0 && ke.KeyCode == keyReturn:
				return steps, nil
			case mods == 0 && ke.KeyCode == keyEscape:
				return fail(ErrCaptureCancelled)
			case ke.KeyCode == UnknownKeyCode:
				return fail(ErrCaptureUnsupportedKey)
			}
			if len(steps) == hotkey.MaxSteps {
				return fail(ErrCaptureTooLong)
			}
			steps = append(steps, hotkey.Spec{Modifiers: mods, KeyCode: ke.KeyCode})
		}
	}
}
