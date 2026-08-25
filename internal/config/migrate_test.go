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

	mig, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if !mig.Changed() {
		t.Fatal("a first-release config was reported as already current")
	}
	if !slices.Contains(mig.Documented, "activate_hotkey") {
		t.Errorf("Documented = %v, expected it to include activate_hotkey", mig.Documented)
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

	mig, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("second MigrateFile: %v", err)
	}
	if mig.Changed() {
		t.Errorf("second migration changed %+v, want nothing", mig)
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

	mig, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if slices.Contains(mig.Documented, "overlay_style") || slices.Contains(mig.Documented, "mute") {
		t.Errorf("Documented = %v, but overlay_style and mute were already documented", mig.Documented)
	}
	if !slices.Contains(mig.Documented, "activate_hotkey") {
		t.Errorf("Documented = %v, expected activate_hotkey", mig.Documented)
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

// legacyTerminalSection is the terminal_language documentation block as an
// earlier release wrote it, with the user's own value set to the old spelling.
// It is the realistic upgrade case: the block is present (so nothing is
// appended) but every word in it names a language that no longer exists.
const legacyTerminalSection = `unlock_code: s w o r d f i s h

# --- terminal_language -------------------------------------------------------
# Source language rendered by overlay_style 'terminal': go (default), python,
# typescript, rust or yc. Each has its own compiled-in corpus + syntax
# highlighting. Only used by 'terminal'; ignored otherwise.
#   yc : YoptaScript (yopta.space) — JavaScript as spoken by the gopniks of the
#        Russian courtyard.
# Per-run override: the --style terminal:<lang> flag (e.g. --style terminal:yc).
terminal_language: yc
`

// TestMigrateFile_RespellsLegacyTerminalLanguage is the upgrade this rename
// exists for: a config that selected YoptaScript under its old name keeps
// selecting it, and stops describing a spelling the program no longer prints.
func TestMigrateFile_RespellsLegacyTerminalLanguage(t *testing.T) {
	path := writeTempConfig(t, legacyTerminalSection)

	mig, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if !slices.Contains(mig.Renamed, "terminal_language") {
		t.Fatalf("Renamed = %v, want it to include terminal_language", mig.Renamed)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "\nterminal_language: ys\n") {
		t.Errorf("the live setting was not respelled:\n%s", got)
	}
	// The whole point of migrating documentation is that it stops being wrong.
	// A single surviving `yc` in this block is a user reading about a value the
	// validator would now reject as a typo.
	for _, stale := range []string{"rust or yc", "#   yc :", "terminal:yc"} {
		if strings.Contains(got, stale) {
			t.Errorf("documentation still carries the old spelling %q:\n%s", stale, got)
		}
	}
	if !strings.Contains(got, "#   ys : YoptaScript") {
		t.Errorf("documentation lost its YoptaScript entry:\n%s", got)
	}
}

// TestMigrateFile_RespelledConfigStillSelectsYoptaScript closes the loop
// through the production entry points: whatever the file now says, the language
// resolved from it must be the one the C side knows.
func TestMigrateFile_RespelledConfigStillSelectsYoptaScript(t *testing.T) {
	path := writeTempConfig(t, legacyTerminalSection)

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

	if after.TerminalLanguage != config.TerminalLangYopta {
		t.Errorf("TerminalLanguage = %q, want %q", after.TerminalLanguage, config.TerminalLangYopta)
	}
	// The property that makes the rewrite safe to do behind the user's back:
	// the setting is the same before and after, only its spelling moved.
	if got, want := config.NormalizeTerminalLanguage(after.TerminalLanguage),
		config.NormalizeTerminalLanguage(before.TerminalLanguage); got != want {
		t.Errorf("migration changed the resolved language: %q -> %q", want, got)
	}
}

// TestMigrateFile_RenameIsIdempotent pins that a respelled config is not
// rewritten again on the next launch — startup migration has to converge, or
// every run would churn a file the user may keep in version control.
func TestMigrateFile_RenameIsIdempotent(t *testing.T) {
	path := writeTempConfig(t, legacyTerminalSection)

	if _, err := config.MigrateFile(path); err != nil {
		t.Fatalf("first MigrateFile: %v", err)
	}
	firstPass, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}

	mig, err := config.MigrateFile(path)
	if err != nil {
		t.Fatalf("second MigrateFile: %v", err)
	}
	if len(mig.Renamed) != 0 {
		t.Errorf("second migration respelled %v, want nothing", mig.Renamed)
	}
	secondPass, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after second: %v", err)
	}
	if string(firstPass) != string(secondPass) {
		t.Error("second migration rewrote the file")
	}
}

// TestRenameLegacyValues_Shapes is the specification of the line surgery: the
// YAML shapes it must respell, and the ones it must copy through untouched.
// Declining is always the safe answer — the old spelling still loads — so the
// negative cases matter as much as the positive ones.
func TestRenameLegacyValues_Shapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          string
		want        string
		wantRenamed bool
	}{
		{
			name:        "plain value",
			in:          "terminal_language: yc\n",
			want:        "terminal_language: ys\n",
			wantRenamed: true,
		},
		{
			name:        "double quoted",
			in:          "terminal_language: \"yc\"\n",
			want:        "terminal_language: \"ys\"\n",
			wantRenamed: true,
		},
		{
			name:        "single quoted",
			in:          "terminal_language: 'yc'\n",
			want:        "terminal_language: 'ys'\n",
			wantRenamed: true,
		},
		{
			name:        "space before the colon",
			in:          "terminal_language : yc\n",
			want:        "terminal_language : ys\n",
			wantRenamed: true,
		},
		{
			// The comment and the padding in front of it are the user's
			// formatting; a rename is no reason to reflow their file.
			name:        "trailing comment and padding survive",
			in:          "terminal_language: yc   # the good one\n",
			want:        "terminal_language: ys   # the good one\n",
			wantRenamed: true,
		},
		{
			name:        "CRLF line endings are preserved",
			in:          "terminal_language: yc\r\nmute: false\r\n",
			want:        "terminal_language: ys\r\nmute: false\r\n",
			wantRenamed: true,
		},
		{
			name:        "no final newline stays that way",
			in:          "terminal_language: yc",
			want:        "terminal_language: ys",
			wantRenamed: true,
		},
		{
			name: "another language is not touched",
			in:   "terminal_language: rust\n",
			want: "terminal_language: rust\n",
		},
		{
			name: "the current spelling is already current",
			in:   "terminal_language: ys\n",
			want: "terminal_language: ys\n",
		},
		{
			// Indented means it belongs to some nested mapping this surgery
			// does not understand. Leaving it alone is correct: the loader
			// still accepts the old spelling either way.
			name: "an indented key is left alone",
			in:   "  terminal_language: yc\n",
			want: "  terminal_language: yc\n",
		},
		{
			name: "a different key with the same value is left alone",
			in:   "overlay_style: yc\n",
			want: "overlay_style: yc\n",
		},
		{
			// Scoped to the key's own documentation block, so prose the user
			// wrote elsewhere in the file keeps whatever it says.
			name: "a comment outside the section is left alone",
			in:   "# I picked yc on purpose\nterminal_language: rust\n",
			want: "# I picked yc on purpose\nterminal_language: rust\n",
		},
		{
			name: "a longer word merely containing the spelling is left alone",
			in: "# --- terminal_language ---\n" +
				"# the encyclopedia entry says otherwise\n",
			want: "# --- terminal_language ---\n" +
				"# the encyclopedia entry says otherwise\n",
		},
		{
			// A user who commented their setting out still has documentation
			// naming the value, and it has to age with the rest of the block.
			name: "a commented-out setting inside the section is respelled",
			in: "# --- terminal_language ---\n" +
				"# terminal_language: yc\n",
			want: "# --- terminal_language ---\n" +
				"# terminal_language: ys\n",
			wantRenamed: true,
		},
		{
			name: "a file that never mentions the key is returned unchanged",
			in:   "unlock_code: s w o r d f i s h\n",
			want: "unlock_code: s w o r d f i s h\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, renamed := config.RenameLegacyValues([]byte(tt.in))
			if string(got) != tt.want {
				t.Errorf("RenameLegacyValues(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if gotRenamed := slices.Contains(renamed, "terminal_language"); gotRenamed != tt.wantRenamed {
				t.Errorf("renamed = %v, want terminal_language present = %v", renamed, tt.wantRenamed)
			}
		})
	}
}

// TestRenameLegacyValues_PreservesEveryOtherByte is the blunt version of the
// same claim: the file that comes back differs from the one that went in by the
// respelled tokens and nothing else.
func TestRenameLegacyValues_PreservesEveryOtherByte(t *testing.T) {
	t.Parallel()
	got, renamed := config.RenameLegacyValues([]byte(legacyTerminalSection))
	if len(renamed) == 0 {
		t.Fatal("nothing was renamed in a config that carries the old spelling")
	}
	want := strings.ReplaceAll(legacyTerminalSection, "yc", "ys")
	if string(got) != want {
		t.Errorf("RenameLegacyValues rewrote more than the spelling:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
