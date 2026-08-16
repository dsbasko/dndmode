//go:build darwin

package matcher_test

import (
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// testDeps groups all dependencies for a single matcher test case
// (Go test convention).
type testDeps struct {
	steps []hotkey.Spec
	seq   *matcher.Sequence
}

func newTestDeps(t *testing.T, steps ...hotkey.Spec) *testDeps {
	t.Helper()
	return &testDeps{
		steps: steps,
		seq:   matcher.NewSequence(steps),
	}
}

// System-defined modifier bits that MUST be ignored before comparison
// (silent CGEventTap match failure when CapsLock is on).
//
// Source: <Apple Headers> CGEventTypes.h kCGEventFlagMask*.
//
//	kCGEventFlagMaskAlphaShift  = 0x00010000 (CapsLock toggle bit)
//	kCGEventFlagMaskNumericPad  = 0x00200000 (set on every numeric-pad key event)
//	kCGEventFlagMaskHelp        = 0x00400000 (legacy ADB Help key bit; some HW sets)
//	NX_NONCOALSESCEDMASK        = 0x00000100 (mouse non-coalesced; sometimes leaks)
const (
	flagCapsLock     hotkey.ModFlag = 0x10000  // kCGEventFlagMaskAlphaShift
	flagNumericPad   hotkey.ModFlag = 0x200000 // kCGEventFlagMaskNumericPad
	flagHelp         hotkey.ModFlag = 0x400000 // kCGEventFlagMaskHelp
	flagNonCoalesced hotkey.ModFlag = 0x100    // NX_NONCOALSESCEDMASK
)

// Virtual keyCodes (kVK_ANSI_*) used across the sequence fixtures below.
// Matching is by physical position, so these are the codes a US-ANSI
// keyboard produces for the named characters.
const (
	kvkA     uint16 = 0x00
	kvkS     uint16 = 0x01
	kvkD     uint16 = 0x02
	kvkW     uint16 = 0x0D
	kvkX     uint16 = 0x07
	kvkY     uint16 = 0x10
	kvkZ     uint16 = 0x06
	kvkF1    uint16 = 0x7A
	kvkSpace uint16 = 0x31
)

// TestSequence_MatchTail_SingleStep_CFG05 exhaustively covers the
// single-step (legacy `hotkey`) case: an unlock code of length 1 is an
// ordinary Sequence, so every assertion the old Matcher.Match test made
// carries over verbatim.
// Spec under test for most cases: "ctrl+option+cmd+x" (default unlock code).
func TestSequence_MatchTail_SingleStep_CFG05(t *testing.T) {
	defaultSpec := hotkey.Spec{
		Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
		KeyCode:   kvkX,
	}

	tests := []struct {
		name  string
		spec  hotkey.Spec
		event matcher.KeyEvent
		want  bool
	}{
		{
			name: "exact hotkey matches → true",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
				KeyCode:   kvkX,
			},
			want: true,
		},
		{
			name: "wrong KeyCode (Y instead of X) → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
				KeyCode:   kvkY,
			},
			want: false,
		},
		{
			name: "missing modifier (no Cmd) → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption,
				KeyCode:   kvkX,
			},
			want: false,
		},
		{
			name: "extra modifier (Shift added) → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModShift,
				KeyCode:   kvkX,
			},
			want: false,
		},
		{
			name: "CapsLock bit set in event but not in spec → still true",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagCapsLock,
				KeyCode:   kvkX,
			},
			want: true,
		},
		{
			name: "NumPad bit ignored",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagNumericPad,
				KeyCode:   kvkX,
			},
			want: true,
		},
		{
			name: "NX_NONCOALSESCEDMASK bit ignored",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagNonCoalesced,
				KeyCode:   kvkX,
			},
			want: true,
		},
		{
			name: "Help bit ignored",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagHelp,
				KeyCode:   kvkX,
			},
			want: true,
		},
		{
			name: "all system bits set + correct user mods → still true",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd |
					flagCapsLock | flagNumericPad | flagHelp | flagNonCoalesced,
				KeyCode: kvkX,
			},
			want: true,
		},
		{
			name: "Fn-bit in event but not in spec → still true (Fn is a system bit)",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModFn,
				KeyCode:   kvkX,
			},
			want: true,
		},
		{
			// The lockout guard, stated as a test: macOS raises SecondaryFn
			// for every key of the function-key group whether or not the user
			// holds Fn, so a step written as a bare `f1` MUST match the event
			// that arrives carrying the bit. If this flips to false, an
			// unlock code containing an F-key, an arrow or Forward Delete can
			// never be entered and the machine stays shielded forever.
			name: "bare F-key spec matched by Fn-decorated event → true",
			spec: hotkey.Spec{Modifiers: 0, KeyCode: kvkF1},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModFn,
				KeyCode:   kvkF1,
			},
			want: true,
		},
		{
			name: "bare F-key spec rejected if event has Fn + a real mod → false",
			spec: hotkey.Spec{Modifiers: 0, KeyCode: kvkF1},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModFn | hotkey.ModCtrl,
				KeyCode:   kvkF1,
			},
			want: false,
		},
		{
			name: "bare F-key spec with Fn + CapsLock on event → still true (both stripped)",
			spec: hotkey.Spec{Modifiers: 0, KeyCode: kvkF1},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModFn | flagCapsLock,
				KeyCode:   kvkF1,
			},
			want: true,
		},
		{
			name: "no modifiers in event + correct KeyCode → false (spec.Modifiers != 0)",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: 0,
				KeyCode:   kvkX,
			},
			want: false,
		},
		{
			name: "completely unrelated event → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModShift,
				KeyCode:   kvkSpace,
			},
			want: false,
		},
		{
			name: "bare-key step matched by unmodified event → true",
			spec: hotkey.Spec{Modifiers: 0, KeyCode: kvkS},
			event: matcher.KeyEvent{
				Modifiers: 0,
				KeyCode:   kvkS,
			},
			want: true,
		},
		{
			name: "bare-key step broken by a stray user-intentional modifier → false",
			spec: hotkey.Spec{Modifiers: 0, KeyCode: kvkS},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModShift,
				KeyCode:   kvkS,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t, tt.spec)
			got := td.seq.MatchTail([]matcher.KeyEvent{tt.event})
			if got != tt.want {
				t.Errorf("MatchTail(%+v) = %v, want %v (steps = %+v)",
					tt.event, got, tt.want, td.steps)
			}
		})
	}
}

// bare builds a single unmodified step for the given keyCode — the shape
// of every step in a passphrase-style code such as "s w o r d".
func bare(keyCode uint16) hotkey.Spec {
	return hotkey.Spec{Modifiers: 0, KeyCode: keyCode}
}

// ev builds a KeyEvent with the given raw CGEventFlags and keyCode.
func ev(mods hotkey.ModFlag, keyCode uint16) matcher.KeyEvent {
	return matcher.KeyEvent{Modifiers: mods, KeyCode: keyCode}
}

// TestSequence_MatchTail_MultiStep covers the multi-step unlock code —
// the mechanism this package exists for after the switch from a single
// hotkey to a code.
func TestSequence_MatchTail_MultiStep(t *testing.T) {
	// Code under test: "s w a d" (bare keys) with a modified step in the
	// mixed fixtures below.
	code := []hotkey.Spec{bare(kvkS), bare(kvkW), bare(kvkA), bare(kvkD)}

	tests := []struct {
		name  string
		steps []hotkey.Spec
		tail  []matcher.KeyEvent
		want  bool
	}{
		{
			name:  "exact multi-step match → true",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkW), ev(0, kvkA), ev(0, kvkD),
			},
			want: true,
		},
		{
			name:  "extra user-intentional modifier in the middle → false",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(hotkey.ModShift, kvkW), ev(0, kvkA), ev(0, kvkD),
			},
			want: false,
		},
		{
			name:  "CapsLock bit on every step → still true (system bit stripped)",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(flagCapsLock, kvkS), ev(flagCapsLock, kvkW),
				ev(flagCapsLock, kvkA), ev(flagCapsLock, kvkD),
			},
			want: true,
		},
		{
			name:  "NumPad bit on one step → still true (system bit stripped)",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(flagNumericPad, kvkW), ev(0, kvkA), ev(0, kvkD),
			},
			want: true,
		},
		{
			name:  "keyCode mismatch in the middle → false",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkY), ev(0, kvkA), ev(0, kvkD),
			},
			want: false,
		},
		{
			name:  "keyCode mismatch on the last step → false",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkW), ev(0, kvkA), ev(0, kvkZ),
			},
			want: false,
		},
		{
			name:  "right keys in the wrong order → false",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkW), ev(0, kvkS), ev(0, kvkA), ev(0, kvkD),
			},
			want: false,
		},
		{
			name:  "tail shorter than the code → false",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkW), ev(0, kvkA),
			},
			want: false,
		},
		{
			name:  "tail longer than the code → false",
			steps: code,
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkW), ev(0, kvkA), ev(0, kvkD), ev(0, kvkZ),
			},
			want: false,
		},
		{
			name:  "empty tail against a non-empty code → false",
			steps: code,
			tail:  []matcher.KeyEvent{},
			want:  false,
		},
		{
			name:  "nil tail against a non-empty code → false",
			steps: code,
			tail:  nil,
			want:  false,
		},
		{
			name: "mixed code (ctrl+s w cmd+z) exact → true",
			steps: []hotkey.Spec{
				{Modifiers: hotkey.ModCtrl, KeyCode: kvkS},
				bare(kvkW),
				{Modifiers: hotkey.ModCmd, KeyCode: kvkZ},
			},
			tail: []matcher.KeyEvent{
				ev(hotkey.ModCtrl, kvkS), ev(0, kvkW), ev(hotkey.ModCmd, kvkZ),
			},
			want: true,
		},
		{
			name: "mixed code with a missing modifier on step 1 → false",
			steps: []hotkey.Spec{
				{Modifiers: hotkey.ModCtrl, KeyCode: kvkS},
				bare(kvkW),
				{Modifiers: hotkey.ModCmd, KeyCode: kvkZ},
			},
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkW), ev(hotkey.ModCmd, kvkZ),
			},
			want: false,
		},
		{
			name:  "self-overlapping code (a s a s) exact → true",
			steps: []hotkey.Spec{bare(kvkA), bare(kvkS), bare(kvkA), bare(kvkS)},
			tail: []matcher.KeyEvent{
				ev(0, kvkA), ev(0, kvkS), ev(0, kvkA), ev(0, kvkS),
			},
			want: true,
		},
		{
			name:  "self-overlapping code shifted by one → false",
			steps: []hotkey.Spec{bare(kvkA), bare(kvkS), bare(kvkA), bare(kvkS)},
			tail: []matcher.KeyEvent{
				ev(0, kvkS), ev(0, kvkA), ev(0, kvkS), ev(0, kvkA),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t, tt.steps...)
			if got := td.seq.MatchTail(tt.tail); got != tt.want {
				t.Errorf("MatchTail(%+v) = %v, want %v (steps = %+v)",
					tt.tail, got, tt.want, td.steps)
			}
		})
	}
}

// TestSequence_MatchTail_AllSystemBits_MaxLengthCode is the belt-and-braces
// case for the longest accepted code: every step carries every system bit
// and the match must still succeed. A regression here would lock the owner
// out with CapsLock on.
func TestSequence_MatchTail_AllSystemBits_MaxLengthCode(t *testing.T) {
	const systemBits = flagCapsLock | flagNumericPad | flagHelp | flagNonCoalesced

	steps := make([]hotkey.Spec, hotkey.MaxSteps)
	tail := make([]matcher.KeyEvent, hotkey.MaxSteps)
	for i := range steps {
		// #nosec G115 -- i is bounded by hotkey.MaxSteps (32).
		code := uint16(i)
		steps[i] = bare(code)
		tail[i] = ev(systemBits, code)
	}

	seq := matcher.NewSequence(steps)
	if got := seq.Len(); got != hotkey.MaxSteps {
		t.Fatalf("Len() = %d, want %d", got, hotkey.MaxSteps)
	}
	if !seq.MatchTail(tail) {
		t.Error("MatchTail() = false on a MaxSteps code with only system bits set, want true")
	}
}

// TestSequence_Len reports the configured step count — the poller sizes its
// tail window from it, so an off-by-one here would silently break matching.
func TestSequence_Len(t *testing.T) {
	tests := []struct {
		name  string
		steps []hotkey.Spec
		want  int
	}{
		{name: "nil steps", steps: nil, want: 0},
		{name: "empty steps", steps: []hotkey.Spec{}, want: 0},
		{name: "single step", steps: []hotkey.Spec{bare(kvkS)}, want: 1},
		{
			name:  "four steps",
			steps: []hotkey.Spec{bare(kvkS), bare(kvkW), bare(kvkA), bare(kvkD)},
			want:  4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matcher.NewSequence(tt.steps).Len(); got != tt.want {
				t.Errorf("Len() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSequence_EmptyCode_NeverMatches pins the defensive guard in
// MatchTail: without it the per-step loop would vacuously return true for
// an empty tail, and a misconfigured build would unlock on the very first
// poller tick with no input at all.
func TestSequence_EmptyCode_NeverMatches(t *testing.T) {
	for _, steps := range [][]hotkey.Spec{nil, {}} {
		seq := matcher.NewSequence(steps)
		if seq.MatchTail(nil) {
			t.Errorf("MatchTail(nil) = true on a zero-length code, want false")
		}
		if seq.MatchTail([]matcher.KeyEvent{}) {
			t.Errorf("MatchTail(empty) = true on a zero-length code, want false")
		}
		if seq.MatchTail([]matcher.KeyEvent{ev(0, kvkS)}) {
			t.Errorf("MatchTail(one event) = true on a zero-length code, want false")
		}
	}
}

// TestSequence_NewSequence_CopiesSteps verifies the "immutable after
// construction" guarantee: mutating the caller's slice after NewSequence
// must not change what the Sequence matches. The Sequence is shared with
// the poller goroutine without locking, so aliasing the caller's storage
// would be a data race waiting to happen.
func TestSequence_NewSequence_CopiesSteps(t *testing.T) {
	steps := []hotkey.Spec{bare(kvkS), bare(kvkW)}
	seq := matcher.NewSequence(steps)

	steps[0] = hotkey.Spec{Modifiers: hotkey.ModCmd, KeyCode: kvkZ}

	if !seq.MatchTail([]matcher.KeyEvent{ev(0, kvkS), ev(0, kvkW)}) {
		t.Error("MatchTail() = false after the caller mutated its slice, want true (steps must be copied)")
	}
	if seq.Len() != 2 {
		t.Errorf("Len() = %d after caller mutation, want 2", seq.Len())
	}
}

// TestSequence_MatchTail_DoesNotMutateTail guards the poller's buffer
// reuse: the tail slice is allocated once and refilled on every candidate
// window, so MatchTail must treat it as read-only.
func TestSequence_MatchTail_DoesNotMutateTail(t *testing.T) {
	seq := matcher.NewSequence([]hotkey.Spec{bare(kvkS), bare(kvkW)})

	tail := []matcher.KeyEvent{ev(flagCapsLock, kvkS), ev(flagCapsLock, kvkW)}
	want := []matcher.KeyEvent{ev(flagCapsLock, kvkS), ev(flagCapsLock, kvkW)}

	if !seq.MatchTail(tail) {
		t.Fatal("MatchTail() = false, want true")
	}
	for i := range want {
		if tail[i] != want[i] {
			t.Errorf("tail[%d] = %+v after MatchTail, want %+v (tail must not be mutated)",
				i, tail[i], want[i])
		}
	}
}

// TestMatcher_UserIntentionalMask_Constant — sanity check on the public
// mask constant. Adding/removing a bit here would silently break the
// CGEventTap callback (which applies the same mask before writing a record
// into the keystroke ring). Catches accidental refactor.
func TestMatcher_UserIntentionalMask_Constant(t *testing.T) {
	want := hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModShift
	if matcher.UserIntentionalMask != want {
		t.Errorf("UserIntentionalMask = %#x, want %#x", matcher.UserIntentionalMask, want)
	}
	// Numeric value check — catches if hotkey.Mod* constants change.
	const wantNumeric uint64 = 0x040000 | 0x080000 | 0x100000 | 0x020000
	if uint64(matcher.UserIntentionalMask) != wantNumeric {
		t.Errorf("UserIntentionalMask numeric = %#x, want %#x (union of canonical kCGEventFlagMask* bits)",
			uint64(matcher.UserIntentionalMask), wantNumeric)
	}
	// SecondaryFn must stay OUT: macOS sets it for the whole function-key
	// group on its own, so honouring it would make a bare `up` / `f1` step
	// unmatchable — a permanent lockout with no feedback. See the constant's
	// doc comment for the full argument before re-adding it.
	if matcher.UserIntentionalMask&hotkey.ModFn != 0 {
		t.Error("UserIntentionalMask contains ModFn (SecondaryFn): the bit is system-set " +
			"for F-keys, arrows and Forward Delete, so treating it as user intent locks " +
			"those keys out of every unlock code")
	}
}
