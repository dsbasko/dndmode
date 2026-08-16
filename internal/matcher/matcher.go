//go:build darwin

// Package matcher compares a tail of synthetic KeyEvent values against a
// configured unlock code ([]hotkey.Spec). It is intentionally pure-Go
// (no cgo, no syscalls, no IO, no allocations in hot path) so that all
// matching logic can be exhaustively unit-tested without standing up
// CGEventTap.
//
// This code is the production matcher, not a test-only twin of a C-side
// comparison: the CGEventTap callback in tap_darwin.m only ACCUMULATES
// keystrokes into a static ring buffer and compares nothing. The poller
// goroutine takes a snapshot of that ring and calls Sequence.MatchTail
// here. Keeping the comparison on the Go side is what allows the C
// callback to make zero Go calls, which is the strongest form of the
// nosplit invariant the callback has to satisfy.
//
// macOS sets system-defined modifier bits in CGEventFlags
// independently of user intent (CapsLock toggle = 0x10000, NumPad bit on
// every numeric-pad key = 0x200000, SecondaryFn = 0x800000 on every key of
// the function-key group, NX_NONCOALSESCEDMASK = 0x100, etc.).
// These bits MUST be masked out before comparison — otherwise a user with
// CapsLock toggled would be unable to enter the unlock code, leaving the
// MacBook permanently locked. The UserIntentionalMask constant below names
// exactly the 4 canonical user-intentional modifier bits that survive the
// mask.
package matcher

import "github.com/dsbasko/dndmode/internal/config/hotkey"

// UserIntentionalMask is the set of modifier bits that count as deliberate
// user input. System bits outside this mask (CapsLock 0x10000,
// NumPad 0x200000, SecondaryFn 0x800000, Help 0x400000,
// NX_NONCOALSESCEDMASK 0x100, …) are
// stripped from the event's modifier flags before comparison against the
// configured Spec. The CGEventTap callback applies the same mask before
// writing a record into the keystroke ring, so the stripping happens twice
// and is idempotent — the Go side does not trust the C side to have done it.
//
// SecondaryFn (Fn) is deliberately NOT in this mask, and its absence is a
// lockout guard of exactly the same kind as the CapsLock one. macOS raises
// that bit for the whole "function key group" — F1-F12, the arrow keys,
// Forward Delete, Home/End/PageUp/PageDown — whether or not the physical Fn
// key is held (NSEventModifierFlagFunction, same bit as
// kCGEventFlagMaskSecondaryFn). Treating it as user intent would mean a
// step declared as a bare `up` could never match the ↑ the user actually
// presses: the event carries 0x800000, the Spec does not, and the exact
// comparison in MatchTail rejects it — silently, forever, with the shield
// up. Stripping the bit is the fail-safe direction: it matches whether or
// not macOS decided to decorate the event, so no keyboard, layout or
// firmware quirk can turn an accepted config into a locked-out machine.
// The price is that Fn cannot be a distinguishing modifier at all
// (hotkey.ParseStep therefore accepts `fn+` and ignores it).
const UserIntentionalMask hotkey.ModFlag = hotkey.ModCtrl |
	hotkey.ModOption |
	hotkey.ModCmd |
	hotkey.ModShift

// KeyEvent is one recorded keystroke — the Go mirror of the C-side
// dnd_keyrec_t written by the CGEventTap callback from
// CGEventGetIntegerValueField(kCGKeyboardEventKeycode) (KeyCode) and
// CGEventGetFlags (Modifiers).
type KeyEvent struct {
	// Modifiers is the raw CGEventFlags value as returned by
	// CGEventGetFlags. It MAY contain system-defined bits — MatchTail
	// strips them via UserIntentionalMask before comparison.
	Modifiers hotkey.ModFlag
	// KeyCode is the macOS virtual keyCode (kVK_*), matched by physical
	// position so RU/AZERTY layouts produce the same value as US-ANSI
	//.
	KeyCode uint16
}

// Sequence checks a tail of KeyEvent values against a configured unlock
// code — a slice of hotkey.Spec steps. A legacy single-combination hotkey
// is simply a Sequence of length 1, so there is exactly one matching path
// in the codebase and no second source of truth.
//
// Sequence is immutable after construction (NewSequence copies the steps)
// and MatchTail is a pure function — safe to call from any goroutine
// concurrently without locking.
type Sequence struct {
	steps []hotkey.Spec
}

// NewSequence returns a Sequence bound to a copy of the given steps.
// The copy is what makes the "immutable after construction" guarantee
// hold even if the caller reuses or mutates its slice afterwards; it
// happens once at Install time, never in the hot path.
func NewSequence(steps []hotkey.Spec) *Sequence {
	cp := make([]hotkey.Spec, len(steps))
	copy(cp, steps)
	return &Sequence{steps: cp}
}

// Len returns the number of steps in the configured unlock code. The
// poller uses it to size the tail window it assembles from the ring
// snapshot.
func (s *Sequence) Len() int { return len(s.steps) }

// MatchTail returns true iff tail is exactly the configured unlock code:
// len(tail) equals Len() and, for every step i, the user-intentional
// modifier bits of tail[i] exactly equal steps[i].Modifiers AND
// tail[i].KeyCode equals steps[i].KeyCode.
//
// The per-step comparison is "exact equality after masking", not "subset" —
// extra user-intentional modifiers (e.g. Shift held while the step is a
// bare "s") cause rejection. System bits outside UserIntentionalMask
// (CapsLock, NumPad, Help, NX_NONCOALSESCEDMASK) are stripped before
// comparison and therefore do NOT affect the result.
//
// A zero-length Sequence never matches. Config validation rejects an empty
// unlock code long before Install, but the guard is kept here because the
// loop below would otherwise return true for an empty tail — i.e. a
// misconfigured build would unlock on the very first tick with no input
// at all.
//
// Pure function — no syscalls, no IO, no allocations. O(Len()).
func (s *Sequence) MatchTail(tail []KeyEvent) bool {
	if len(s.steps) == 0 || len(tail) != len(s.steps) {
		return false
	}
	for i, step := range s.steps {
		intentional := tail[i].Modifiers & UserIntentionalMask
		if intentional != step.Modifiers || tail[i].KeyCode != step.KeyCode {
			return false
		}
	}
	return true
}
