//go:build darwin

package hotkey_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
)

type testDeps struct {
	// hotkey package is pure functional — no deps to inject.
	// testDeps kept for convention compliance.
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	return &testDeps{}
}

func TestParse_Hotkey_Success(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, got hotkey.Spec, err error)
	}{
		{
			name:       "default hotkey Ctrl+Option+Cmd+X",
			input:      "Ctrl+Option+Cmd+X",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := hotkey.Spec{
					Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
					KeyCode:   0x07, // kVK_ANSI_X
				}
				if got != want {
					t.Errorf("Parse() = %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "lowercase tokens",
			input:      "ctrl+option+cmd+x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.KeyCode != 0x07 {
					t.Errorf("KeyCode = %#x, want 0x07", got.KeyCode)
				}
				if got.Modifiers != (hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd) {
					t.Errorf("Modifiers = %#x, want %#x", got.Modifiers, hotkey.ModCtrl|hotkey.ModOption|hotkey.ModCmd)
				}
			},
		},
		{
			name:       "uppercase tokens",
			input:      "CTRL+OPTION+CMD+X",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.KeyCode != 0x07 {
					t.Errorf("KeyCode = %#x, want 0x07", got.KeyCode)
				}
			},
		},
		{
			name:       "whitespace around tokens",
			input:      "  Ctrl + Option + Cmd + X  ",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.KeyCode != 0x07 {
					t.Errorf("KeyCode = %#x, want 0x07", got.KeyCode)
				}
			},
		},
		{
			name:       "single modifier + space",
			input:      "shift+space",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.KeyCode != 0x31 {
					t.Errorf("KeyCode = %#x, want 0x31 (kVK_Space)", got.KeyCode)
				}
				if got.Modifiers != hotkey.ModShift {
					t.Errorf("Modifiers = %#x, want ModShift only", got.Modifiers)
				}
			},
		},
		{
			// `fn` parses but contributes no bit, so it cannot satisfy the
			// legacy "at least one modifier" rule on its own. Accepting it
			// here would mean `hotkey: fn+f1` unlocks the shield on a bare
			// F1 press — a single keypress, which is exactly what that rule
			// exists to forbid.
			name:       "fn alone does not satisfy the legacy modifier requirement",
			input:      "fn+f1",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrInvalidHotkey) {
					t.Fatalf("err = %v, want ErrInvalidHotkey", err)
				}
			},
		},
		{
			name:       "fn combined with a real modifier is accepted and dropped",
			input:      "ctrl+fn+f1",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Modifiers != hotkey.ModCtrl {
					t.Errorf("Modifiers = %#x, want ModCtrl only (fn must contribute no bit)", got.Modifiers)
				}
				if got.KeyCode != 0x7A {
					t.Errorf("KeyCode = %#x, want 0x7A (kVK_F1)", got.KeyCode)
				}
			},
		},
		{
			name:       "modifier order does not matter",
			input:      "x+cmd+ctrl",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Modifiers != (hotkey.ModCtrl | hotkey.ModCmd) {
					t.Errorf("Modifiers = %#x, want ModCtrl|ModCmd", got.Modifiers)
				}
				if got.KeyCode != 0x07 {
					t.Errorf("KeyCode = %#x, want 0x07", got.KeyCode)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			got, err := hotkey.Parse(tt.input)
			tt.validateResp(t, got, err)
		})
	}
}

func TestParse_Hotkey_Errors(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, got hotkey.Spec, err error)
	}{
		{
			name:       "empty string",
			input:      "",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrEmpty) {
					t.Errorf("got %v, want errors.Is(err, ErrEmpty)", err)
				}
			},
		},
		{
			name:       "whitespace only treated as empty",
			input:      "   ",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrEmpty) {
					t.Errorf("got %v, want errors.Is(err, ErrEmpty)", err)
				}
			},
		},
		{
			name:       "modifier-only Ctrl+Cmd",
			input:      "ctrl+cmd",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrModifierOnly) {
					t.Errorf("got %v, want errors.Is(err, ErrModifierOnly)", err)
				}
			},
		},
		{
			name:       "single key without modifier",
			input:      "x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrInvalidHotkey) {
					t.Errorf("got %v, want errors.Is(err, ErrInvalidHotkey)", err)
				}
			},
		},
		{
			name:       "duplicate modifier",
			input:      "ctrl+ctrl+x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrDuplicateMod) {
					t.Errorf("got %v, want errors.Is(err, ErrDuplicateMod)", err)
				}
			},
		},
		{
			name:       "two non-modifier keys",
			input:      "ctrl+x+y",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrInvalidHotkey) {
					t.Errorf("got %v, want errors.Is(err, ErrInvalidHotkey)", err)
				}
			},
		},
		{
			name:       "unknown key token",
			input:      "ctrl+nonexistent",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrUnknownToken) {
					t.Errorf("got %v, want errors.Is(err, ErrUnknownToken)", err)
				}
			},
		},
		{
			name:       "alias 'alt' not supported (only 'option')",
			input:      "alt+x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrUnknownToken) {
					t.Errorf("got %v, want errors.Is(err, ErrUnknownToken) for alias 'alt'", err)
				}
			},
		},
		{
			name:       "alias 'command' not supported (only 'cmd')",
			input:      "command+x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrUnknownToken) {
					t.Errorf("got %v, want errors.Is(err, ErrUnknownToken) for alias 'command'", err)
				}
			},
		},
		{
			name:       "empty token between two pluses",
			input:      "ctrl++x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrInvalidHotkey) {
					t.Errorf("got %v, want errors.Is(err, ErrInvalidHotkey)", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			got, err := hotkey.Parse(tt.input)
			tt.validateResp(t, got, err)
		})
	}
}

func TestParse_Hotkey_KeyCodeResolution(t *testing.T) {
	// physical key position (kVK_*) lookup table.
	tests := []struct {
		name     string
		input    string
		wantCode uint16
	}{
		{name: "x → kVK_ANSI_X", input: "ctrl+x", wantCode: 0x07},
		{name: "space → kVK_Space", input: "shift+space", wantCode: 0x31},
		{name: "f1 → kVK_F1", input: "ctrl+f1", wantCode: 0x7A},
		{name: "f12 → kVK_F12", input: "ctrl+f12", wantCode: 0x6F},
		{name: "escape → kVK_Escape", input: "ctrl+escape", wantCode: 0x35},
		{name: "esc alias → kVK_Escape", input: "ctrl+esc", wantCode: 0x35},
		{name: "enter → kVK_Return", input: "ctrl+enter", wantCode: 0x24},
		{name: "return → kVK_Return", input: "ctrl+return", wantCode: 0x24},
		{name: "0 → kVK_ANSI_0", input: "ctrl+0", wantCode: 0x1D},
		{name: "left arrow → kVK_LeftArrow", input: "ctrl+left", wantCode: 0x7B},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hotkey.Parse(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.KeyCode != tt.wantCode {
				t.Errorf("KeyCode = %#x, want %#x", got.KeyCode, tt.wantCode)
			}
		})
	}
}

func TestParseStep_Hotkey_Success(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, got hotkey.Spec, err error)
	}{
		{
			name:       "bare key without modifiers",
			input:      "s",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := hotkey.Spec{Modifiers: 0, KeyCode: 0x01} // kVK_ANSI_S
				if got != want {
					t.Errorf("ParseStep() = %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "step with one modifier",
			input:      "ctrl+s",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := hotkey.Spec{Modifiers: hotkey.ModCtrl, KeyCode: 0x01}
				if got != want {
					t.Errorf("ParseStep() = %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "step with several modifiers",
			input:      "Ctrl+Option+Cmd+X",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := hotkey.Spec{
					Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
					KeyCode:   0x07, // kVK_ANSI_X
				}
				if got != want {
					t.Errorf("ParseStep() = %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "case-insensitive bare key",
			input:      "S",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.KeyCode != 0x01 || got.Modifiers != 0 {
					t.Errorf("ParseStep() = %+v, want {0 0x01}", got)
				}
			},
		},
		{
			name:       "space key as a bare step",
			input:      "space",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.KeyCode != 0x31 || got.Modifiers != 0 {
					t.Errorf("ParseStep() = %+v, want {0 0x31}", got)
				}
			},
		},
		{
			name:       "surrounding whitespace trimmed",
			input:      "  ctrl+z  ",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := hotkey.Spec{Modifiers: hotkey.ModCtrl, KeyCode: 0x06} // kVK_ANSI_Z
				if got != want {
					t.Errorf("ParseStep() = %+v, want %+v", got, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			got, err := hotkey.ParseStep(tt.input)
			tt.validateResp(t, got, err)
		})
	}
}

func TestParseStep_Hotkey_Errors(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, got hotkey.Spec, err error)
	}{
		{
			name:       "empty string",
			input:      "",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrEmpty) {
					t.Errorf("got %v, want errors.Is(err, ErrEmpty)", err)
				}
			},
		},
		{
			name:       "whitespace only",
			input:      " \t ",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrEmpty) {
					t.Errorf("got %v, want errors.Is(err, ErrEmpty)", err)
				}
			},
		},
		{
			name:       "unknown token",
			input:      "nonexistent",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrUnknownToken) {
					t.Errorf("got %v, want errors.Is(err, ErrUnknownToken)", err)
				}
			},
		},
		{
			name:       "two non-modifier keys",
			input:      "x+y",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrInvalidHotkey) {
					t.Errorf("got %v, want errors.Is(err, ErrInvalidHotkey)", err)
				}
			},
		},
		{
			name:       "modifier-only step",
			input:      "ctrl+cmd",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrModifierOnly) {
					t.Errorf("got %v, want errors.Is(err, ErrModifierOnly)", err)
				}
			},
		},
		{
			name:       "duplicate modifier",
			input:      "ctrl+ctrl+x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrDuplicateMod) {
					t.Errorf("got %v, want errors.Is(err, ErrDuplicateMod)", err)
				}
			},
		},
		{
			name:       "empty token between two pluses",
			input:      "ctrl++x",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrInvalidHotkey) {
					t.Errorf("got %v, want errors.Is(err, ErrInvalidHotkey)", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			got, err := hotkey.ParseStep(tt.input)
			tt.validateResp(t, got, err)
		})
	}
}

// Parse is now ParseStep + "at least one modifier". That refactor moves the
// bare-modifier input "ctrl" from ErrInvalidHotkey (it used to trip the
// len(tokens) < 2 guard) to ErrModifierOnly, which is the more accurate
// category. The change is deliberate — this test pins it.
func TestParse_Hotkey_BareModifierIsModifierOnly(t *testing.T) {
	td := newTestDeps(t)
	_ = td

	_, err := hotkey.Parse("ctrl")
	if !errors.Is(err, hotkey.ErrModifierOnly) {
		t.Errorf("Parse(%q) error = %v, want errors.Is(err, ErrModifierOnly)", "ctrl", err)
	}
	if errors.Is(err, hotkey.ErrInvalidHotkey) {
		t.Errorf("Parse(%q) error = %v, must no longer be ErrInvalidHotkey", "ctrl", err)
	}
}

func TestParseSequence_Hotkey_Success(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, got []hotkey.Spec, err error)
	}{
		{
			name:       "passphrase style code",
			input:      "s w o r d",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := []hotkey.Spec{
					{KeyCode: 0x01}, // s
					{KeyCode: 0x0D}, // w
					{KeyCode: 0x1F}, // o
					{KeyCode: 0x0F}, // r
					{KeyCode: 0x02}, // d
				}
				assertSequence(t, got, want)
			},
		},
		{
			name:       "legacy hotkey is a code of length 1",
			input:      "Ctrl+Option+Cmd+X",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := []hotkey.Spec{{
					Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
					KeyCode:   0x07,
				}}
				assertSequence(t, got, want)
			},
		},
		{
			name:       "mixed bare and modified steps",
			input:      "ctrl+s w cmd+z",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := []hotkey.Spec{
					{Modifiers: hotkey.ModCtrl, KeyCode: 0x01},
					{KeyCode: 0x0D},
					{Modifiers: hotkey.ModCmd, KeyCode: 0x06},
				}
				assertSequence(t, got, want)
			},
		},
		{
			name:       "repeated leading and trailing whitespace collapses",
			input:      "  \t s   w \t\t o  ",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := []hotkey.Spec{{KeyCode: 0x01}, {KeyCode: 0x0D}, {KeyCode: 0x1F}}
				assertSequence(t, got, want)
			},
		},
		{
			name:       "space key named by token, not by separator",
			input:      "s space w",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := []hotkey.Spec{{KeyCode: 0x01}, {KeyCode: 0x31}, {KeyCode: 0x0D}}
				assertSequence(t, got, want)
			},
		},
		{
			name:       "self-overlapping code is accepted verbatim",
			input:      "a b a b",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := []hotkey.Spec{
					{KeyCode: 0x00}, {KeyCode: 0x0B}, {KeyCode: 0x00}, {KeyCode: 0x0B},
				}
				assertSequence(t, got, want)
			},
		},
		{
			name:       "MaxSteps steps accepted",
			input:      repeatSteps("a", hotkey.MaxSteps),
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(got) != hotkey.MaxSteps {
					t.Errorf("len = %d, want %d", len(got), hotkey.MaxSteps)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			got, err := hotkey.ParseSequence(tt.input)
			tt.validateResp(t, got, err)
		})
	}
}

func TestParseSequence_Hotkey_Errors(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, got []hotkey.Spec, err error)
	}{
		{
			name:       "empty string",
			input:      "",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrEmpty) {
					t.Errorf("got %v, want errors.Is(err, ErrEmpty)", err)
				}
				if got != nil {
					t.Errorf("steps = %v, want nil on error", got)
				}
			},
		},
		{
			name:       "whitespace only",
			input:      "  \t\n ",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ []hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrEmpty) {
					t.Errorf("got %v, want errors.Is(err, ErrEmpty)", err)
				}
			},
		},
		{
			name:       "unknown token in the middle reports its position",
			input:      "s w nonexistent d",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ []hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrUnknownToken) {
					t.Fatalf("got %v, want errors.Is(err, ErrUnknownToken)", err)
				}
				if !strings.Contains(err.Error(), "step 3") {
					t.Errorf("error %q must name the failing step position (step 3)", err)
				}
			},
		},
		{
			name:       "modifier-only step in the middle",
			input:      "s ctrl d",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, _ []hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrModifierOnly) {
					t.Fatalf("got %v, want errors.Is(err, ErrModifierOnly)", err)
				}
				if !strings.Contains(err.Error(), "step 2") {
					t.Errorf("error %q must name the failing step position (step 2)", err)
				}
			},
		},
		{
			name:       "MaxSteps+1 steps rejected",
			input:      repeatSteps("a", hotkey.MaxSteps+1),
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got []hotkey.Spec, err error) {
				if !errors.Is(err, hotkey.ErrTooManySteps) {
					t.Errorf("got %v, want errors.Is(err, ErrTooManySteps)", err)
				}
				if got != nil {
					t.Errorf("steps = %v, want nil on error", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			got, err := hotkey.ParseSequence(tt.input)
			tt.validateResp(t, got, err)
		})
	}
}

func assertSequence(t *testing.T, got, want []hotkey.Spec) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

func repeatSteps(step string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = step
	}
	return strings.Join(parts, " ")
}
