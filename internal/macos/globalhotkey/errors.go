//go:build darwin

package globalhotkey

import "errors"

// Sentinel errors. Use errors.Is to identify category.
//
// Unlike internal/config/hotkey, error values here MAY be wrapped with the
// offending combination by callers: the activation combination is not a
// secret (see the package doc). Only the unlock code must stay out of the
// terminal scrollback.
var (
	// ErrNoModifier is returned by Register when the requested combination
	// carries no user-intentional modifier once matcher.UserIntentionalMask
	// has been applied — e.g. a bare `d`, or `fn+d`, whose Fn bit is dropped
	// by hotkey.ParseStep because macOS raises it for the whole function-key
	// group regardless of intent.
	//
	// A modifier-less global hotkey is not a usable trigger but a trap: every
	// press of that single key, in any application, would raise the shield.
	// The same requirement is enforced on one-step unlock codes by
	// config.ValidateUnlockCode.
	ErrNoModifier = errors.New("globalhotkey: activation combination needs at least one modifier (Ctrl, Option, Cmd or Shift)")

	// ErrComboTaken is returned when Carbon reports eventHotKeyExistsErr:
	// the combination is already registered system-wide, by macOS itself or
	// by another application (Raycast, Alfred, Karabiner…).
	//
	// This sentinel is the reason Carbon was chosen over a tap for the
	// waiting path: a conflict surfaces as a distinct, actionable error at
	// startup, while the user is still watching the terminal, instead of as
	// a hotkey that mysteriously never fires.
	ErrComboTaken = errors.New("globalhotkey: combination is already registered by another application")

	// ErrRegisterFailed wraps any other non-zero OSStatus from
	// InstallEventHandler or RegisterEventHotKey. The numeric status is
	// interpolated by the caller for diagnostics.
	ErrRegisterFailed = errors.New("globalhotkey: Carbon refused the registration")

	// ErrAlreadyRegistered is returned by a second concurrent Register call.
	// This package holds exactly one registration per process: the Carbon
	// handler routes to a single package-level channel, so a second live
	// registration would have no way to say which combination fired.
	//
	// It is a programming error rather than a runtime condition — watch mode
	// registers once in the shell and releases before exit.
	ErrAlreadyRegistered = errors.New("globalhotkey: a registration is already active in this process")
)
