//go:build darwin

package config_test

import (
	"errors"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

func mustSpec(t *testing.T, s string) hotkey.Spec {
	t.Helper()
	spec, err := hotkey.Parse(s)
	if err != nil {
		t.Fatalf("hotkey.Parse(%q): %v", s, err)
	}
	return spec
}

func mustSequence(t *testing.T, s string) matcher.Verifier {
	t.Helper()
	steps, err := hotkey.ParseSequence(s)
	if err != nil {
		t.Fatalf("hotkey.ParseSequence(%q): %v", s, err)
	}
	return matcher.NewSequence(steps)
}

// TestResolveActivateHotkey_AbsentUsesDefault pins the half of the
// absent/empty asymmetry that keeps existing installs working: a config
// written before this key existed must come back with the shipped default
// rather than with "disabled".
func TestResolveActivateHotkey_AbsentUsesDefault(t *testing.T) {
	cfg := &config.Config{} // ActivateHotkey == nil, i.e. key absent

	got, err := config.ResolveActivateHotkey(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveActivateHotkey: %v", err)
	}
	if want := mustSpec(t, config.DefaultActivateHotkey); got != want {
		t.Errorf("got %+v, want %+v (the DefaultActivateHotkey spec)", got, want)
	}
}

// TestResolveActivateHotkey_EmptyDisables pins the other half: an explicitly
// empty value is a deliberate "watch mode off", not a mistake and not a
// fallback to the default.
func TestResolveActivateHotkey_EmptyDisables(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t"} {
		t.Run("value="+raw, func(t *testing.T) {
			cfg := &config.Config{ActivateHotkey: new(raw)}
			if _, err := config.ResolveActivateHotkey(cfg, nil); !errors.Is(err, config.ErrActivateHotkeyDisabled) {
				t.Errorf("error = %v, want ErrActivateHotkeyDisabled", err)
			}
		})
	}
}

func TestResolveActivateHotkey_ParsesExplicitValue(t *testing.T) {
	cfg := &config.Config{ActivateHotkey: new("Ctrl+Shift+L")}

	got, err := config.ResolveActivateHotkey(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveActivateHotkey: %v", err)
	}
	if want := mustSpec(t, "Ctrl+Shift+L"); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// modifierlessCombos are combinations that must never become an activation
// hotkey: each would fire on a single bare keypress in any application,
// raising the shield at random. `fn+…` entries are here because
// hotkey.ParseStep drops the Fn token — macOS raises that bit for the whole
// function-key group on its own — which makes `fn+d` the same request as a
// bare `d`.
var modifierlessCombos = []string{"d", "fn+d", "up", "fn+up", "space"}

func TestResolveActivateHotkey_RejectsModifierless(t *testing.T) {
	for _, raw := range modifierlessCombos {
		t.Run(raw, func(t *testing.T) {
			cfg := &config.Config{ActivateHotkey: new(raw)}
			if _, err := config.ResolveActivateHotkey(cfg, nil); err == nil {
				t.Errorf("ResolveActivateHotkey(%q) accepted a modifier-less combination", raw)
			}
		})
	}
}

// TestParse_RejectsModifierless_WatchModeDependsOnIt pins WHERE the guard
// above actually lives. ResolveActivateHotkey does not re-check modifiers;
// it relies on hotkey.Parse refusing them, which is why weakening Parse
// would silently open watch mode to bare-key triggers. This test fails at
// the source if that ever happens, instead of leaving the consequence to be
// discovered on a locked machine.
func TestParse_RejectsModifierless_WatchModeDependsOnIt(t *testing.T) {
	for _, raw := range modifierlessCombos {
		t.Run(raw, func(t *testing.T) {
			if _, err := hotkey.Parse(raw); err == nil {
				t.Errorf("hotkey.Parse(%q) accepted a modifier-less combination; "+
					"config.ResolveActivateHotkey depends on this refusal", raw)
			}
		})
	}
}

func TestResolveActivateHotkey_RejectsGarbage(t *testing.T) {
	for _, raw := range []string{"ctrl+", "ctrl+cmd", "not+a+key", "ctrl+a b"} {
		t.Run(raw, func(t *testing.T) {
			cfg := &config.Config{ActivateHotkey: new(raw)}
			if _, err := config.ResolveActivateHotkey(cfg, nil); err == nil {
				t.Errorf("ResolveActivateHotkey(%q) accepted an invalid combination", raw)
			}
		})
	}
}

// TestResolveActivateHotkey_CollisionWithPlaintextUnlock pins the hole the
// check exists to close: if the published activation combination also
// satisfies the unlock code, anyone who read the README can lower the
// shield it raised.
func TestResolveActivateHotkey_CollisionWithPlaintextUnlock(t *testing.T) {
	const combo = "Ctrl+Option+Cmd+X"
	cfg := &config.Config{ActivateHotkey: new(combo)}

	_, err := config.ResolveActivateHotkey(cfg, mustSequence(t, combo))
	if !errors.Is(err, config.ErrActivateHotkeyCollision) {
		t.Errorf("error = %v, want ErrActivateHotkeyCollision", err)
	}
}

// TestResolveActivateHotkey_CollisionWithHashedUnlock is the reason the
// check goes through Verifier.Match instead of comparing steps: it catches
// the collision for the hashed form too, without the digest ever being
// reversed. --set-password cannot currently produce a one-step digest
// (MinUnlockSteps forbids it), so this builds one directly — the guard must
// not depend on a sibling code path's invariant holding.
func TestResolveActivateHotkey_CollisionWithHashedUnlock(t *testing.T) {
	const combo = "Ctrl+Option+Cmd+X"
	steps, err := hotkey.ParseSequence(combo)
	if err != nil {
		t.Fatalf("ParseSequence: %v", err)
	}

	salt := make([]byte, matcher.SaltLen) // all-zero salt is fine for a test
	digest, err := matcher.NewDigest(salt, matcher.HashSteps(salt, steps))
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}

	cfg := &config.Config{ActivateHotkey: new(combo)}
	if _, err := config.ResolveActivateHotkey(cfg, digest); !errors.Is(err, config.ErrActivateHotkeyCollision) {
		t.Errorf("error = %v, want ErrActivateHotkeyCollision", err)
	}
}

// TestResolveActivateHotkey_NoCollisionCases guards the other direction:
// the check must not refuse combinations that are merely similar, or fire
// when there is nothing to compare against.
func TestResolveActivateHotkey_NoCollisionCases(t *testing.T) {
	tests := []struct {
		name   string
		combo  string
		unlock matcher.Verifier
	}{
		{"nil verifier", "Ctrl+Option+Cmd+D", nil},
		{"different key", "Ctrl+Option+Cmd+D", mustSequence(t, "Ctrl+Option+Cmd+X")},
		{"different modifiers", "Ctrl+Shift+X", mustSequence(t, "Ctrl+Option+Cmd+X")},
		{"multi-step unlock", "Ctrl+Option+Cmd+D", mustSequence(t, "s w o r d f i s h")},
		{"multi-step starting with the combo", "Ctrl+Option+Cmd+D", mustSequence(t, "Ctrl+Option+Cmd+D a b c")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ActivateHotkey: new(tt.combo)}
			if _, err := config.ResolveActivateHotkey(cfg, tt.unlock); err != nil {
				t.Errorf("ResolveActivateHotkey: unexpected error %v", err)
			}
		})
	}
}

// TestDefaults_DoNotCollide is the pin that matters for a user who never
// opens the config file: the two shipped defaults must not be the same
// combination. If someone ever edits one of the constants to match the
// other, this fails instead of shipping a shield that unlocks itself.
func TestDefaults_DoNotCollide(t *testing.T) {
	cfg := &config.Config{} // both defaults in play

	if _, err := config.ResolveActivateHotkey(cfg, mustSequence(t, config.DefaultUnlockCode)); err != nil {
		t.Fatalf("shipped defaults collide or are otherwise invalid: %v", err)
	}
}
