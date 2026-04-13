//go:build darwin

package hotkey_test

import (
	"errors"
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
			name:       "fn modifier supported",
			input:      "fn+f1",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, got hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Modifiers != hotkey.ModFn {
					t.Errorf("Modifiers = %#x, want ModFn only", got.Modifiers)
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
		{name: "f1 → kVK_F1", input: "fn+f1", wantCode: 0x7A},
		{name: "f12 → kVK_F12", input: "fn+f12", wantCode: 0x6F},
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
