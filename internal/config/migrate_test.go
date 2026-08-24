//go:build darwin

package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
)

// ancientConfig is what a first-release install looks like: the required key
// and nothing else. Every later key has to be appendable onto this.
const ancientConfig = "hotkey: Ctrl+Option+Cmd+X\n"

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestMigrateFile_AncientConfigGainsEveryKey(t *testing.T) {
	path := writeTempConfig(t, ancientConfig)

	added, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if len(added) == 0 {
		t.Fatal("a first-release config was reported as already current")
	}
	if !slices.Contains(added, "activate_hotkey") {
		t.Errorf("added = %v, expected it to include activate_hotkey", added)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if missing := config.MissingSections(raw); len(missing) != 0 {
		t.Errorf("still missing after migration: %v", missing)
	}
}

// TestMigrateFile_PreservesTheSecretAndUserEdits is the property that makes
// automatic migration acceptable: the user's own lines survive verbatim.
func TestMigrateFile_PreservesTheSecretAndUserEdits(t *testing.T) {
	const original = `# my own note, do not lose this
unlock_code: s w o r d f i s h
overlay_style: matrix
mute: false
`
	path := writeTempConfig(t, original)

	if _, err := config.MigrateFile(path); err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)

	if !strings.HasPrefix(got, original) {
		t.Error("migration did not leave the original content untouched at the head of the file")
	}
	for _, line := range []string{
		"# my own note, do not lose this",
		"unlock_code: s w o r d f i s h",
		"overlay_style: matrix",
		"mute: false",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("migration lost the line %q", line)
		}
	}

	// The keys the user had set must NOT have been re-appended: a second
	// `mute:` line further down would be a duplicate key, and the user's own
	// value could end up shadowed by the template's.
	if n := strings.Count(got, "\nmute: "); n != 1 {
		t.Errorf("found %d live `mute:` lines, want exactly 1", n)
	}
	if n := strings.Count(got, "\noverlay_style: "); n != 1 {
		t.Errorf("found %d live `overlay_style:` lines, want exactly 1", n)
	}
}

// TestMigrateFile_SemanticsUnchanged verifies the safety claim end to end: a
// migrated config must load to exactly the same settings as the original.
func TestMigrateFile_SemanticsUnchanged(t *testing.T) {
	const original = `unlock_code: s w o r d f i s h
overlay_style: glass
glass_blur: 32
mute: false
focus: true
`
	path := writeTempConfig(t, original)

	before, _, _, err := config.NewLoader(path).LoadWithSource()
	if err != nil {
		t.Fatalf("load before: %v", err)
	}
	if _, err := config.MigrateFile(path); err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	after, _, _, err := config.NewLoader(path).LoadWithSource()
	if err != nil {
		t.Fatalf("load after: %v", err)
	}

	if before.OverlayStyle != after.OverlayStyle ||
		config.NormalizeMute(before.Mute) != config.NormalizeMute(after.Mute) ||
		before.Focus != after.Focus ||
		before.UnlockCode != after.UnlockCode ||
		config.NormalizeGlassBlur(before.GlassBlur) != config.NormalizeGlassBlur(after.GlassBlur) {
		t.Errorf("migration changed settings:\nbefore=%+v\nafter =%+v", before, after)
	}
}

// TestMigrateFile_Idempotent pins that startup migration is a one-time event
// per key rather than a rewrite on every launch.
func TestMigrateFile_Idempotent(t *testing.T) {
	path := writeTempConfig(t, ancientConfig)

	if _, err := config.MigrateFile(path); err != nil {
		t.Fatalf("first MigrateFile: %v", err)
	}
	firstPass, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}

	added, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("second MigrateFile: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("second migration added %v, want nothing", added)
	}
	secondPass, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after second: %v", err)
	}
	if string(firstPass) != string(secondPass) {
		t.Error("second migration rewrote the file")
	}
}

// TestMigrateFile_PartialConfigGainsOnlyWhatIsMissing covers the realistic
// upgrade: a config from a middle release that has some keys but not the new
// one.
func TestMigrateFile_PartialConfigGainsOnlyWhatIsMissing(t *testing.T) {
	const original = `unlock_code: s w o r d f i s h
# overlay_style: black
# mute: true
`
	path := writeTempConfig(t, original)

	added, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if slices.Contains(added, "overlay_style") || slices.Contains(added, "mute") {
		t.Errorf("added = %v, but overlay_style and mute were already documented", added)
	}
	if !slices.Contains(added, "activate_hotkey") {
		t.Errorf("added = %v, expected activate_hotkey", added)
	}
}

// TestMigrateFile_RefusesUnparseableConfig pins that migration does not touch
// a file that was already broken — the user needs Load's diagnostic pointing
// at the bad line, not a longer file with the same error in it.
func TestMigrateFile_RefusesUnparseableConfig(t *testing.T) {
	const broken = "unlock_code: [unclosed\n"
	path := writeTempConfig(t, broken)

	if _, err := config.MigrateFile(path); err == nil {
		t.Error("MigrateFile accepted a config that does not parse")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != broken {
		t.Error("MigrateFile modified a config it could not parse")
	}
}

// TestMigrateFile_MigratedConfigStillResolvesItsSecret is the check that
// matters most for an unattended upgrade: whatever else changed, the machine
// must still be unlockable.
func TestMigrateFile_MigratedConfigStillResolvesItsSecret(t *testing.T) {
	path := writeTempConfig(t, ancientConfig) // legacy `hotkey:` form

	if _, err := config.MigrateFile(path); err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}

	cfg, _, _, err := config.NewLoader(path).LoadWithSource()
	if err != nil {
		t.Fatalf("load after migration: %v", err)
	}
	verifier, source, _, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		t.Fatalf("resolve after migration: %v", err)
	}
	if source != config.UnlockSourceHotkey {
		t.Errorf("source = %q, want %q", source, config.UnlockSourceHotkey)
	}
	if verifier.MaxLen() == 0 {
		t.Error("migrated config resolved to an empty verifier: the shield could never be lowered")
	}
}
