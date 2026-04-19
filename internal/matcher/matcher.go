//go:build darwin

// Package matcher compares synthetic KeyEvent values against a configured
// hotkey.Spec. It is intentionally pure-Go (no cgo, no syscalls, no IO,
// no allocations in hot path) so that all matching logic can be exhaustively
// unit-tested in Phase 1 without standing up CGEventTap.
//
// In Phase 4, the CGEventTap callback constructs a KeyEvent from
// CGEventGetFlags / CGEventGetIntegerValueField(kCGKeyboardEventKeycode)
// and calls Matcher.Match — see the design notes.
//
// macOS sets system-defined modifier bits in CGEventFlags
// independently of user intent (CapsLock toggle = 0x10000, NumPad bit on
// every numeric-pad key = 0x200000, NX_NONCOALSESCEDMASK = 0x100, etc.).
// These bits MUST be masked out before comparison — otherwise a user with
// CapsLock toggled would be unable to trigger the exit hotkey, leaving the
// MacBook permanently locked. The UserIntentionalMask constant below names
// exactly the 5 canonical user-intentional modifier bits that survive the
// mask.
package matcher

import "github.com/dsbasko/dndmode/internal/config/hotkey"

// UserIntentionalMask is the set of modifier bits that count as deliberate
// user input. System bits outside this mask (CapsLock 0x10000,
// NumPad 0x200000, Help 0x400000, NX_NONCOALSESCEDMASK 0x100, …) are
// stripped from the event's modifier flags before comparison against the
// configured Spec — see. Phase 4 CGEventTap callback applies
// the same mask before constructing KeyEvent.
const UserIntentionalMask hotkey.ModFlag = hotkey.ModCtrl |
	hotkey.ModOption |
	hotkey.ModCmd |
	hotkey.ModShift |
	hotkey.ModFn

// KeyEvent is the synthetic input to Matcher.Match. In Phase 4, the
// CGEventTap callback constructs a KeyEvent from
// CGEventGetIntegerValueField(kCGKeyboardEventKeycode) (KeyCode) and
// CGEventGetFlags (Modifiers).
type KeyEvent struct {
	// Modifiers is the raw CGEventFlags value as returned by
	// CGEventGetFlags. It MAY contain system-defined bits — Match()
	// strips them via UserIntentionalMask before comparison.
	Modifiers hotkey.ModFlag
	// KeyCode is the macOS virtual keyCode (kVK_*), matched by physical
	// position so RU/AZERTY layouts produce the same value as US-ANSI
	//.
	KeyCode uint16
}

// Matcher checks KeyEvent values against a configured Spec.
//
// Matcher is immutable after construction and Match is a pure function —
// safe to call from any goroutine concurrently without locking.
type Matcher struct {
	spec hotkey.Spec
}

// New returns a Matcher bound to the given Spec.
func New(spec hotkey.Spec) *Matcher {
	return &Matcher{spec: spec}
}

// Spec returns the configured Spec (for diagnostics / debug logging).
func (m *Matcher) Spec() hotkey.Spec { return m.spec }

// Match returns true iff the user-intentional modifier bits of ev exactly
// equal m.spec.Modifiers AND ev.KeyCode equals m.spec.KeyCode.
//
// The comparison is "exact equality after masking", not "subset" — extra
// user-intentional modifiers (e.g. Shift when spec is Ctrl+Cmd+X) cause
// rejection. System bits outside UserIntentionalMask (CapsLock, NumPad,
// Help, NX_NONCOALSESCEDMASK) are stripped before comparison and therefore
// do NOT affect the result.
//
// Pure function — no syscalls, no IO, no allocations. O(1).
func (m *Matcher) Match(ev KeyEvent) bool {
	intentional := ev.Modifiers & UserIntentionalMask
	return intentional == m.spec.Modifiers && ev.KeyCode == m.spec.KeyCode
}
