//go:build darwin

package globalhotkey

import (
	"errors"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

func TestCarbonModifiers_EachFlag(t *testing.T) {
	tests := []struct {
		name string
		in   hotkey.ModFlag
		want uint32
	}{
		{"ctrl", hotkey.ModCtrl, carbonControlKey},
		{"option", hotkey.ModOption, carbonOptionKey},
		{"cmd", hotkey.ModCmd, carbonCmdKey},
		{"shift", hotkey.ModShift, carbonShiftKey},
		{
			"default activate_hotkey (ctrl+option+cmd)",
			hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
			carbonControlKey | carbonOptionKey | carbonCmdKey,
		},
		{
			"all four",
			hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd | hotkey.ModShift,
			carbonControlKey | carbonOptionKey | carbonCmdKey | carbonShiftKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := carbonModifiers(tt.in)
			if err != nil {
				t.Fatalf("carbonModifiers(%#x) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("carbonModifiers(%#x) = %#x, want %#x", tt.in, got, tt.want)
			}
		})
	}
}

// TestCarbonModifiers_RejectsModifierless pins the trap guard: a global
// hotkey with no modifier would fire on every press of a bare key in every
// application, raising the shield at random. config.ValidateUnlockCode
// enforces the same requirement on one-step unlock codes.
func TestCarbonModifiers_RejectsModifierless(t *testing.T) {
	tests := []struct {
		name string
		in   hotkey.ModFlag
	}{
		{"no bits at all", 0},
		{"fn only — stripped by the mask", hotkey.ModFn},
		{"capslock only (0x10000)", hotkey.ModFlag(0x10000)},
		{"numpad only (0x200000)", hotkey.ModFlag(0x200000)},
		{"help only (0x400000)", hotkey.ModFlag(0x400000)},
		{"every system bit at once", hotkey.ModFn | 0x10000 | 0x200000 | 0x400000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := carbonModifiers(tt.in)
			if !errors.Is(err, ErrNoModifier) {
				t.Errorf("carbonModifiers(%#x) error = %v, want ErrNoModifier", tt.in, err)
			}
		})
	}
}

// TestCarbonModifiers_StripsSystemBits is the fail-safe pin. A system-set
// bit riding along with a real modifier must be dropped, not refused and not
// smuggled into the Carbon mask: Carbon has no encoding for Fn, CapsLock,
// NumPad or Help, so leaking one could only produce a mask that means
// something else. The user-intentional part must convert exactly as if the
// system bit had never been there.
func TestCarbonModifiers_StripsSystemBits(t *testing.T) {
	const systemNoise = hotkey.ModFn | 0x10000 | 0x200000 | 0x400000 | 0x100

	clean, err := carbonModifiers(hotkey.ModCtrl | hotkey.ModCmd)
	if err != nil {
		t.Fatalf("clean input errored: %v", err)
	}
	noisy, err := carbonModifiers(hotkey.ModCtrl | hotkey.ModCmd | systemNoise)
	if err != nil {
		t.Fatalf("noisy input errored: %v", err)
	}
	if clean != noisy {
		t.Errorf("system bits changed the result: clean=%#x noisy=%#x", clean, noisy)
	}
}

// TestCarbonModifiers_MaskMatchesMatcherPackage pins the conversion to the
// SAME mask the unlock comparison uses. If matcher.UserIntentionalMask ever
// gains or loses a bit, a combination accepted by the config parser and one
// accepted here would drift apart, and the drift would show up as an
// activation hotkey that silently never fires.
func TestCarbonModifiers_MaskMatchesMatcherPackage(t *testing.T) {
	for _, flag := range []hotkey.ModFlag{
		hotkey.ModCtrl, hotkey.ModOption, hotkey.ModCmd, hotkey.ModShift,
	} {
		if matcher.UserIntentionalMask&flag == 0 {
			t.Fatalf("flag %#x is convertible here but absent from matcher.UserIntentionalMask", flag)
		}
		if _, err := carbonModifiers(flag); err != nil {
			t.Errorf("flag %#x is in UserIntentionalMask but rejected here: %v", flag, err)
		}
	}

	// And nothing outside the mask may survive on its own.
	for _, flag := range []hotkey.ModFlag{hotkey.ModFn, 0x10000, 0x200000, 0x400000} {
		if matcher.UserIntentionalMask&flag != 0 {
			t.Fatalf("flag %#x unexpectedly present in matcher.UserIntentionalMask", flag)
		}
		if _, err := carbonModifiers(flag); !errors.Is(err, ErrNoModifier) {
			t.Errorf("flag %#x is outside the mask but accepted here", flag)
		}
	}
}
