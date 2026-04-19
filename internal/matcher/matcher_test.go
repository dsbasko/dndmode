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
	spec    hotkey.Spec
	matcher *matcher.Matcher
}

func newTestDeps(t *testing.T, spec hotkey.Spec) *testDeps {
	t.Helper()
	return &testDeps{
		spec:    spec,
		matcher: matcher.New(spec),
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

// TestMatcher_Match_CFG05 exhaustively covers +.
// Spec under test for most cases: "ctrl+option+cmd+x" (default exit hotkey).
func TestMatcher_Match_CFG05(t *testing.T) {
	defaultSpec := hotkey.Spec{
		Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
		KeyCode:   0x07, // kVK_ANSI_X
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
				KeyCode:   0x07,
			},
			want: true,
		},
		{
			name: "wrong KeyCode (Y instead of X) → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
				KeyCode:   0x10, // kVK_ANSI_Y
			},
			want: false,
		},
		{
			name: "missing modifier (no Cmd) → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption,
				KeyCode:   0x07,
			},
			want: false,
		},
		{
			name: "extra modifier (Shift added) → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModShift,
				KeyCode:   0x07,
			},
			want: false,
		},
		{
			name: "CapsLock bit set in event but not in spec → still true",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagCapsLock,
				KeyCode:   0x07,
			},
			want: true,
		},
		{
			name: "NumPad bit ignored",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagNumericPad,
				KeyCode:   0x07,
			},
			want: true,
		},
		{
			name: "NX_NONCOALSESCEDMASK bit ignored",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagNonCoalesced,
				KeyCode:   0x07,
			},
			want: true,
		},
		{
			name: "Help bit ignored",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | flagHelp,
				KeyCode:   0x07,
			},
			want: true,
		},
		{
			name: "all system bits set + correct user mods → still true",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd |
					flagCapsLock | flagNumericPad | flagHelp | flagNonCoalesced,
				KeyCode: 0x07,
			},
			want: true,
		},
		{
			name: "Fn-bit in event but not in spec → false (Fn IS user-intentional)",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModFn,
				KeyCode:   0x07,
			},
			want: false,
		},
		{
			name: "Fn-required spec matched by Fn-event → true",
			spec: hotkey.Spec{Modifiers: hotkey.ModFn, KeyCode: 0x7A /* F1 */},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModFn,
				KeyCode:   0x7A,
			},
			want: true,
		},
		{
			name: "Fn-spec rejected if event has Fn + extra mod → false",
			spec: hotkey.Spec{Modifiers: hotkey.ModFn, KeyCode: 0x7A},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModFn | hotkey.ModCtrl,
				KeyCode:   0x7A,
			},
			want: false,
		},
		{
			name: "Fn-spec with CapsLock-bit on event → still true (system bit ignored)",
			spec: hotkey.Spec{Modifiers: hotkey.ModFn, KeyCode: 0x7A},
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModFn | flagCapsLock,
				KeyCode:   0x7A,
			},
			want: true,
		},
		{
			name: "no modifiers in event + correct KeyCode → false (defensive: matcher itself does not enforce, but spec.Modifiers!= 0)",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: 0,
				KeyCode:   0x07,
			},
			want: false,
		},
		{
			name: "completely unrelated event → false",
			spec: defaultSpec,
			event: matcher.KeyEvent{
				Modifiers: hotkey.ModShift,
				KeyCode:   0x31, // space
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t, tt.spec)
			got := td.matcher.Match(tt.event)
			if got != tt.want {
				t.Errorf("Match(%+v) = %v, want %v (spec = %+v)",
					tt.event, got, tt.want, tt.spec)
			}
		})
	}
}

// TestMatcher_New_StoresSpec verifies New() preserves the spec verbatim
// and Spec() exposes it for diagnostics.
func TestMatcher_New_StoresSpec(t *testing.T) {
	spec := hotkey.Spec{
		Modifiers: hotkey.ModCtrl | hotkey.ModShift,
		KeyCode:   0x31, // space
	}
	m := matcher.New(spec)
	if got := m.Spec(); got != spec {
		t.Errorf("Spec() = %+v, want %+v", got, spec)
	}
}

// TestMatcher_UserIntentionalMask_Constant — sanity check on the public
// mask constant. Adding/removing a bit here would silently break Phase 4
// CGEventTap callback (which applies the same mask before constructing
// KeyEvent). Catches accidental refactor.
func TestMatcher_UserIntentionalMask_Constant(t *testing.T) {
	want := hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModShift | hotkey.ModFn
	if matcher.UserIntentionalMask != want {
		t.Errorf("UserIntentionalMask = %#x, want %#x", matcher.UserIntentionalMask, want)
	}
	// Numeric value check — catches if hotkey.Mod* constants change.
	const wantNumeric uint64 = 0x040000 | 0x080000 | 0x100000 | 0x020000 | 0x800000
	if uint64(matcher.UserIntentionalMask) != wantNumeric {
		t.Errorf("UserIntentionalMask numeric = %#x, want %#x (union of canonical kCGEventFlagMask* bits)",
			uint64(matcher.UserIntentionalMask), wantNumeric)
	}
}
