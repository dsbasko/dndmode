//go:build darwin

package hotkey

import "testing"

func TestKeyCodeTable_Internal_HasMinimumEntries(t *testing.T) {
	// ensure the keyCode table covers all letters, digits, F-keys
	// and critical special keys. Catches regressions if entries are deleted.
	required := map[string]uint16{
		// Letters
		"a": 0x00, "x": 0x07, "z": 0x06, "m": 0x2E,
		// Digits
		"0": 0x1D, "9": 0x19,
		// F-keys
		"f1": 0x7A, "f12": 0x6F,
		// Special
		"space": 0x31, "return": 0x24, "enter": 0x24, "tab": 0x30,
		"escape": 0x35, "esc": 0x35, "delete": 0x33,
		// Arrows
		"left": 0x7B, "right": 0x7C, "up": 0x7E, "down": 0x7D,
	}

	for name, want := range required {
		got, ok := keyCodeTable[name]
		if !ok {
			t.Errorf("keyCodeTable[%q] missing", name)
			continue
		}
		if got != want {
			t.Errorf("keyCodeTable[%q] = %#x, want %#x", name, got, want)
		}
	}

	// Sanity: at least 70 entries total (26 letters + 10 digits + 12 F-keys
	// + 7 control + 4 arrows + 11 punctuation = 70; алиасы enter/esc
	// добавляют ещё одну запись).
	if len(keyCodeTable) < 70 {
		t.Errorf("keyCodeTable has %d entries, want >= 70", len(keyCodeTable))
	}
}

func TestModifierTable_Internal_NoAliases(t *testing.T) {
	// the design notes: only the canonical modifiers, no aliases.
	// `fn` is deliberately NOT here — it parses through ignoredModifiers and
	// contributes no bit, because macOS raises SecondaryFn for the whole
	// function-key group on its own (see the ignoredModifiers comment). A
	// modifierTable entry for it would produce a Spec that
	// matcher.UserIntentionalMask strips, i.e. one that can never match.
	wantKeys := map[string]bool{
		"ctrl":   true,
		"option": true,
		"cmd":    true,
		"shift":  true,
	}
	if _, found := modifierTable["fn"]; found {
		t.Error("modifierTable contains \"fn\": it must stay in ignoredModifiers, " +
			"otherwise a step could demand a bit the matcher strips")
	}
	if !ignoredModifiers["fn"] {
		t.Error("ignoredModifiers is missing \"fn\": configs written against the " +
			"older grammar must keep parsing")
	}
	if len(modifierTable) != len(wantKeys) {
		t.Errorf("modifierTable has %d entries, want exactly %d", len(modifierTable), len(wantKeys))
	}
	for k := range modifierTable {
		if !wantKeys[k] {
			t.Errorf("modifierTable contains unexpected key %q (aliases not allowed in v1)", k)
		}
	}

	// Explicit deny-list: aliases that MUST NOT be added to v1.
	forbidden := []string{"alt", "control", "command", "opt", "super", "win", "meta"}
	for _, k := range forbidden {
		if _, found := modifierTable[k]; found {
			t.Errorf("modifierTable contains forbidden alias %q", k)
		}
	}
}
