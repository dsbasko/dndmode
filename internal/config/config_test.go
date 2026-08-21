//go:build darwin

package config_test

import (
	"bytes"
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// digestPair returns the base64 unlock_salt / unlock_hash pair that
// --set-password would write for the given steps. Built through
// matcher.HashSteps rather than pasted as a literal so the fixture cannot
// drift away from the scheme it is supposed to exercise; the salt is a fixed
// pattern because these tests assert on resolution, not on randomness.
func digestPair(t *testing.T, steps []hotkey.Spec) (saltB64, hashB64 string) {
	t.Helper()
	salt := bytes.Repeat([]byte{0xA5}, matcher.SaltLen)
	return base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(matcher.HashSteps(salt, steps))
}

// keyEvents mirrors what the poller hands a Verifier for a typed sequence:
// the same steps, arriving as matcher.KeyEvent values.
func keyEvents(steps []hotkey.Spec) []matcher.KeyEvent {
	evs := make([]matcher.KeyEvent, len(steps))
	for i, st := range steps {
		evs[i] = matcher.KeyEvent{Modifiers: st.Modifiers, KeyCode: st.KeyCode}
	}
	return evs
}

type testDeps struct {
	tmpDir string
	path   string
	loader *config.Loader
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	tmp := t.TempDir()
	// subdir → exercises MkdirAll(0o700) inside writeDefault
	path := filepath.Join(tmp, "subdir", "config.yml")
	return &testDeps{
		tmpDir: tmp,
		path:   path,
		loader: config.NewLoader(path),
	}
}

// Loader.Load() reads an existing valid YAML file and returns
// (cfg, false /*created*/, nil).
func TestLoader_Load_ReadsExistingFile(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error)
	}{
		{
			name: "valid YAML with hotkey field",
			setupMocks: func(td *testDeps) {
				if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(td.path, []byte("hotkey: Ctrl+Shift+Q\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			validateResp: func(t *testing.T, _ *testDeps, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false (file pre-existed)")
				}
				if cfg.Hotkey != "Ctrl+Shift+Q" {
					t.Errorf("cfg.Hotkey = %q, want %q", cfg.Hotkey, "Ctrl+Shift+Q")
				}
			},
		},
		{
			name: "valid YAML with default-shaped hotkey",
			setupMocks: func(td *testDeps) {
				if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
					t.Fatal(err)
				}
				body := "hotkey: " + config.DefaultHotkey + "\n"
				if err := os.WriteFile(td.path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			validateResp: func(t *testing.T, _ *testDeps, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false")
				}
				if cfg.Hotkey != config.DefaultHotkey {
					t.Errorf("cfg.Hotkey = %q, want %q", cfg.Hotkey, config.DefaultHotkey)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			cfg, created, err := td.loader.Load()
			tt.validateResp(t, td, cfg, created, err)
		})
	}
}

// (allow_display_sleep) — Loader.Load() parses the inverted-polarity
// allow_display_sleep toggle. Absence of the key yields the Go zero value
// false, meaning the display STAYS AWAKE (default). Setting it true restores
// the legacy display-may-idle-off behavior. yaml.Strict() must ACCEPT the key
// now that it is a declared struct field (it is no longer "unknown").
func TestLoader_Load_ParsesAllowDisplaySleep(t *testing.T) {
	tests := []struct {
		name      string
		yamlBody  string
		wantAllow bool
	}{
		{
			name:      "allow_display_sleep: true → AllowDisplaySleep == true",
			yamlBody:  "hotkey: Ctrl+X\nallow_display_sleep: true\n",
			wantAllow: true,
		},
		{
			name:      "key absent → AllowDisplaySleep == false (display stays awake)",
			yamlBody:  "hotkey: Ctrl+X\n",
			wantAllow: false,
		},
		{
			name:      "allow_display_sleep: false → AllowDisplaySleep == false",
			yamlBody:  "hotkey: Ctrl+X\nallow_display_sleep: false\n",
			wantAllow: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.yamlBody), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, created, err := td.loader.Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if created {
				t.Errorf("created = true, want false (file pre-existed)")
			}
			if cfg.AllowDisplaySleep != tt.wantAllow {
				t.Errorf("cfg.AllowDisplaySleep = %v, want %v", cfg.AllowDisplaySleep, tt.wantAllow)
			}
		})
	}
}

// Loader.Load() with a missing file creates the parent directory
// (0o700), writes the default config (0o600) and returns (cfg, true, nil).
func TestLoader_Load_WritesDefaultWhenMissing(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func(td *testDeps)
		validateResp func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error)
	}{
		{
			name:       "fresh path → creates parent dir (0o700) + file (0o600) with default unlock code",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !created {
					t.Errorf("created = false, want true")
				}
				if cfg.UnlockCode != config.DefaultUnlockCode {
					t.Errorf("cfg.UnlockCode = %q, want %q", cfg.UnlockCode, config.DefaultUnlockCode)
				}
				if cfg.Hotkey != "" {
					t.Errorf("cfg.Hotkey = %q on a fresh config, want empty (the deprecated key is written commented-out)", cfg.Hotkey)
				}

				// File exists at expected path with mode 0o600 (P1.7).
				info, err := os.Stat(td.path)
				if err != nil {
					t.Fatalf("stat config file: %v", err)
				}
				if mode := info.Mode().Perm(); mode != 0o600 {
					t.Errorf("config file mode = %#o, want 0o600", mode)
				}

				// Parent dir exists with mode 0o700 (P1.7).
				dirInfo, err := os.Stat(filepath.Dir(td.path))
				if err != nil {
					t.Fatalf("stat parent dir: %v", err)
				}
				if mode := dirInfo.Mode().Perm(); mode != 0o700 {
					t.Errorf("parent dir mode = %#o, want 0o700 (P1.7)", mode)
				}

				// File contents valid YAML and contains the default hotkey.
				body, err := os.ReadFile(td.path)
				if err != nil {
					t.Fatalf("read written file: %v", err)
				}
				if !strings.Contains(string(body), "unlock_code:") {
					t.Errorf("written file missing 'unlock_code:' key: %s", body)
				}
				if !strings.Contains(string(body), config.DefaultUnlockCode) {
					t.Errorf("written file missing default unlock code value: %s", body)
				}
				// The deprecated key must stay COMMENTED OUT: an active
				// `hotkey:` next to `unlock_code:` would make the generated
				// file an ambiguous secret and fail ResolveUnlockCode.
				if regexp.MustCompile(`(?m)^hotkey:`).MatchString(string(body)) {
					t.Errorf("written file has an ACTIVE 'hotkey:' key alongside unlock_code: %s", body)
				}
			},
		},
		{
			name: "second Load on freshly-written file returns created=false",
			setupMocks: func(td *testDeps) {
				// Prime: first call writes the default.
				if _, _, err := td.loader.Load(); err != nil {
					t.Fatalf("priming Load failed: %v", err)
				}
			},
			validateResp: func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true on second Load, want false")
				}
				if cfg.UnlockCode != config.DefaultUnlockCode {
					t.Errorf("cfg.UnlockCode = %q, want %q", cfg.UnlockCode, config.DefaultUnlockCode)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)
			cfg, created, err := td.loader.Load()
			tt.validateResp(t, td, cfg, created, err)
		})
	}
}

// On YAML syntax error, Load() returns a goccy-formatted pretty
// error containing a `[line:col]` location prefix.
func TestLoader_Load_PrettyErrorOnSyntaxError(t *testing.T) {
	tests := []struct {
		name          string
		yamlBody      string
		expectLineCol bool
		setupMocks    func(td *testDeps, body string)
		validateResp  func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error, expectLineCol bool)
	}{
		{
			name:          "invalid indent triggers goccy pretty error",
			yamlBody:      "hotkey: ctrl+x\n  bad: indent\n",
			expectLineCol: true,
			setupMocks: func(td *testDeps, body string) {
				if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(td.path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			validateResp: func(t *testing.T, _ *testDeps, _ config.Config, _ bool, err error, expectLineCol bool) {
				if err == nil {
					t.Fatal("expected parse error, got nil")
				}
				if !strings.Contains(err.Error(), "parse config") {
					t.Errorf("error not wrapped with 'parse config' context: %v", err)
				}
				if expectLineCol {
					re := regexp.MustCompile(`\[\d+:\d+\]`)
					if !re.MatchString(err.Error()) {
						t.Errorf("error message missing line:col [L:C] format: %q", err.Error())
					}
				}
			},
		},
		{
			name:          "garbage non-YAML",
			yamlBody:      "{{{ this is not yaml ::: \n\n\t",
			expectLineCol: false, // garbage may produce error without [L:C]
			setupMocks: func(td *testDeps, body string) {
				if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(td.path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			validateResp: func(t *testing.T, _ *testDeps, _ config.Config, _ bool, err error, _ bool) {
				if err == nil {
					t.Fatal("expected parse error, got nil")
				}
				if !strings.Contains(err.Error(), "parse config") {
					t.Errorf("error not wrapped with 'parse config' context: %v", err)
				}
			},
		},
		{
			name:          "unknown YAML key rejected by yaml.Strict",
			yamlBody:      "hotkey: Ctrl+X\nuntrusted_field: payload\n",
			expectLineCol: true,
			setupMocks: func(td *testDeps, body string) {
				if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(td.path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			validateResp: func(t *testing.T, _ *testDeps, _ config.Config, _ bool, err error, expectLineCol bool) {
				if err == nil {
					t.Fatal("expected strict-mode error for unknown key, got nil")
				}
				if !strings.Contains(err.Error(), "parse config") {
					t.Errorf("error not wrapped with 'parse config' context: %v", err)
				}
				if expectLineCol {
					re := regexp.MustCompile(`\[\d+:\d+\]`)
					if !re.MatchString(err.Error()) {
						t.Errorf("error message missing line:col [L:C] format: %q", err.Error())
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td, tt.yamlBody)
			cfg, created, err := td.loader.Load()
			tt.validateResp(t, td, cfg, created, err, tt.expectLineCol)
		})
	}
}

// TestValidateTerminalLanguage pins the --style terminal:<lang> gate: the four
// supported languages and "" (default) are accepted; anything else (including
// case variants and aliases) is rejected.
func TestValidateTerminalLanguage(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"", config.TerminalLangGo, config.TerminalLangPython,
		config.TerminalLangTypeScript, config.TerminalLangRust,
	} {
		if err := config.ValidateTerminalLanguage(s); err != nil {
			t.Errorf("ValidateTerminalLanguage(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"ruby", "golang", "py", "ts", "Go", "PYTHON", "c++"} {
		if err := config.ValidateTerminalLanguage(s); err == nil {
			t.Errorf("ValidateTerminalLanguage(%q) = nil, want error", s)
		}
	}
}

// TestNormalizeTerminalLanguage pins the ""=>go default and pass-through for the
// explicit languages (mirrors NormalizeOverlayStyle's empty=>black rule).
func TestNormalizeTerminalLanguage(t *testing.T) {
	t.Parallel()
	if got := config.NormalizeTerminalLanguage(""); got != config.TerminalLangGo {
		t.Errorf("NormalizeTerminalLanguage(%q) = %q, want %q", "", got, config.TerminalLangGo)
	}
	for _, s := range []string{
		config.TerminalLangPython, config.TerminalLangRust, config.TerminalLangTypeScript,
	} {
		if got := config.NormalizeTerminalLanguage(s); got != s {
			t.Errorf("NormalizeTerminalLanguage(%q) = %q, want unchanged", s, got)
		}
	}
}

// QUICK-gh8 — overlay_style is an optional string field. yaml.Strict() rejects
// unknown KEYS but does NOT validate VALUES, so a recognised key with any value
// parses cleanly; value validation is the caller's job (main.go via
// config.ValidateOverlayStyle). These cases pin: every valid style (`black`,
// `matrix`, `terminal`, `glass`, `none`) round-trips, an ABSENT key leaves
// OverlayStyle == "" which NormalizeOverlayStyle maps to "black", and an invalid
// VALUE still Load()s but ValidateOverlayStyle rejects it while accepting "",
// "black", "matrix", "terminal", "glass", "none".
func TestLoader_Load_OverlayStyle(t *testing.T) {
	tests := []struct {
		name         string
		yamlBody     string
		validateResp func(t *testing.T, cfg config.Config, created bool, err error)
	}{
		{
			name:     "overlay_style: matrix present",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: matrix\n",
			validateResp: func(t *testing.T, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false (file pre-existed)")
				}
				if cfg.OverlayStyle != config.OverlayStyleMatrix {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, config.OverlayStyleMatrix)
				}
			},
		},
		{
			name:     "overlay_style: terminal present",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: terminal\n",
			validateResp: func(t *testing.T, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false (file pre-existed)")
				}
				if cfg.OverlayStyle != config.OverlayStyleTerminal {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, config.OverlayStyleTerminal)
				}
			},
		},
		{
			name:     "overlay_style: dvd present",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: dvd\n",
			validateResp: func(t *testing.T, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false (file pre-existed)")
				}
				if cfg.OverlayStyle != config.OverlayStyleDVD {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, config.OverlayStyleDVD)
				}
			},
		},
		{
			name:     "overlay_style: glass present",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: glass\n",
			validateResp: func(t *testing.T, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false (file pre-existed)")
				}
				if cfg.OverlayStyle != config.OverlayStyleGlass {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, config.OverlayStyleGlass)
				}
			},
		},
		{
			name:     "overlay_style: none present",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: none\n",
			validateResp: func(t *testing.T, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if created {
					t.Errorf("created = true, want false (file pre-existed)")
				}
				if cfg.OverlayStyle != config.OverlayStyleNone {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, config.OverlayStyleNone)
				}
			},
		},
		{
			name:     "overlay_style: black present",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: black\n",
			validateResp: func(t *testing.T, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.OverlayStyle != config.OverlayStyleBlack {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, config.OverlayStyleBlack)
				}
			},
		},
		{
			name:     "overlay_style absent → empty, normalizes to black",
			yamlBody: "hotkey: Ctrl+Shift+Q\n",
			validateResp: func(t *testing.T, cfg config.Config, _ bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.OverlayStyle != "" {
					t.Errorf("cfg.OverlayStyle = %q, want %q (absent key)", cfg.OverlayStyle, "")
				}
				if got := config.NormalizeOverlayStyle(cfg.OverlayStyle); got != config.OverlayStyleBlack {
					t.Errorf("NormalizeOverlayStyle(%q) = %q, want %q", cfg.OverlayStyle, got, config.OverlayStyleBlack)
				}
			},
		},
		{
			name:     "overlay_style: neon (invalid value) loads but fails validation",
			yamlBody: "hotkey: Ctrl+Shift+Q\noverlay_style: neon\n",
			validateResp: func(t *testing.T, cfg config.Config, _ bool, err error) {
				// yaml.Strict() rejects unknown KEYS, not unknown VALUES → Load succeeds.
				if err != nil {
					t.Fatalf("unexpected Load error for known key / bad value: %v", err)
				}
				if cfg.OverlayStyle != "neon" {
					t.Errorf("cfg.OverlayStyle = %q, want %q", cfg.OverlayStyle, "neon")
				}
				// ValidateOverlayStyle is the real gate: rejects "neon", accepts the rest.
				if verr := config.ValidateOverlayStyle("neon"); verr == nil {
					t.Errorf("ValidateOverlayStyle(%q) = nil, want non-nil", "neon")
				}
				for _, ok := range []string{"", config.OverlayStyleBlack, config.OverlayStyleMatrix, config.OverlayStyleTerminal, config.OverlayStyleDVD, config.OverlayStyleGlass, config.OverlayStyleNone} {
					if verr := config.ValidateOverlayStyle(ok); verr != nil {
						t.Errorf("ValidateOverlayStyle(%q) = %v, want nil", ok, verr)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.yamlBody), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, created, err := td.loader.Load()
			tt.validateResp(t, cfg, created, err)
		})
	}
}

// ValidateOverlayStyle is the value gate main.go calls before any window is
// created. Every documented style (incl. "" => black and the new "terminal")
// must be accepted; an unknown value must error with a message naming the FULL
// valid set — terminal included — so the stderr template stays truthful.
func TestValidateOverlayStyle(t *testing.T) {
	valid := []string{
		"",
		config.OverlayStyleBlack,
		config.OverlayStyleMatrix,
		config.OverlayStyleTerminal,
		config.OverlayStyleDVD,
		config.OverlayStyleGlass,
		config.OverlayStyleNone,
	}
	for _, s := range valid {
		if err := config.ValidateOverlayStyle(s); err != nil {
			t.Errorf("ValidateOverlayStyle(%q) = %v, want nil", s, err)
		}
	}

	// terminal is explicitly a valid style (constant value round-trips).
	if config.OverlayStyleTerminal != "terminal" {
		t.Errorf("OverlayStyleTerminal = %q, want %q", config.OverlayStyleTerminal, "terminal")
	}
	// dvd is explicitly a valid style (constant value round-trips).
	if config.OverlayStyleDVD != "dvd" {
		t.Errorf("OverlayStyleDVD = %q, want %q", config.OverlayStyleDVD, "dvd")
	}

	// An unknown value errors and the message must name the full valid set,
	// including the newly-added terminal, so main.go's stderr stays accurate.
	err := config.ValidateOverlayStyle("neon")
	if err == nil {
		t.Fatalf("ValidateOverlayStyle(%q) = nil, want error", "neon")
	}
	for _, want := range []string{"black", "matrix", "terminal", "dvd", "glass", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing valid style %q", err.Error(), want)
		}
	}
}

// glass_blur is an optional *float64. An ABSENT key yields nil (=>
// NormalizeGlassBlur DefaultGlassBlur); a present value (int or float) round-trips.
// yaml.Strict() accepts the known key; ValidateGlassBlur is the value gate.
func TestLoader_Load_GlassBlur(t *testing.T) {
	tests := []struct {
		name     string
		yamlBody string
		validate func(t *testing.T, cfg config.Config)
	}{
		{
			name:     "glass_blur: 24 present (int)",
			yamlBody: "hotkey: Ctrl+Shift+Q\nglass_blur: 24\n",
			validate: func(t *testing.T, cfg config.Config) {
				if cfg.GlassBlur == nil {
					t.Fatalf("GlassBlur = nil, want non-nil *24")
				}
				if *cfg.GlassBlur != 24 {
					t.Errorf("*GlassBlur = %g, want 24", *cfg.GlassBlur)
				}
				if got := config.NormalizeGlassBlur(cfg.GlassBlur); got != 24 {
					t.Errorf("NormalizeGlassBlur = %g, want 24", got)
				}
			},
		},
		{
			name:     "glass_blur: 12.5 present (float)",
			yamlBody: "hotkey: Ctrl+Shift+Q\nglass_blur: 12.5\n",
			validate: func(t *testing.T, cfg config.Config) {
				if cfg.GlassBlur == nil || *cfg.GlassBlur != 12.5 {
					t.Fatalf("GlassBlur = %v, want *12.5", cfg.GlassBlur)
				}
			},
		},
		{
			name:     "glass_blur absent → nil → NormalizeGlassBlur default",
			yamlBody: "hotkey: Ctrl+Shift+Q\n",
			validate: func(t *testing.T, cfg config.Config) {
				if cfg.GlassBlur != nil {
					t.Errorf("GlassBlur = %v, want nil (absent key)", cfg.GlassBlur)
				}
				if got := config.NormalizeGlassBlur(cfg.GlassBlur); got != config.DefaultGlassBlur {
					t.Errorf("NormalizeGlassBlur(nil) = %g, want %g", got, config.DefaultGlassBlur)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.yamlBody), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := td.loader.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.validate(t, cfg)
		})
	}
}

// terminal_language is an optional string. An ABSENT/empty key yields "" (=>
// NormalizeTerminalLanguage go); a present value round-trips. yaml.Strict()
// accepts the known key; ValidateTerminalLanguage is the value gate (a junk value
// still Load()s, mirroring overlay_style / glass_blur).
func TestLoader_Load_TerminalLanguage(t *testing.T) {
	tests := []struct {
		name     string
		yamlBody string
		validate func(t *testing.T, cfg config.Config)
	}{
		{
			name:     "terminal_language: rust present",
			yamlBody: "hotkey: Ctrl+Shift+Q\nterminal_language: rust\n",
			validate: func(t *testing.T, cfg config.Config) {
				if cfg.TerminalLanguage != config.TerminalLangRust {
					t.Errorf("TerminalLanguage = %q, want %q", cfg.TerminalLanguage, config.TerminalLangRust)
				}
				if got := config.NormalizeTerminalLanguage(cfg.TerminalLanguage); got != config.TerminalLangRust {
					t.Errorf("NormalizeTerminalLanguage = %q, want rust", got)
				}
			},
		},
		{
			name:     "terminal_language absent → empty → go default",
			yamlBody: "hotkey: Ctrl+Shift+Q\n",
			validate: func(t *testing.T, cfg config.Config) {
				if cfg.TerminalLanguage != "" {
					t.Errorf("TerminalLanguage = %q, want empty (absent key)", cfg.TerminalLanguage)
				}
				if got := config.NormalizeTerminalLanguage(cfg.TerminalLanguage); got != config.TerminalLangGo {
					t.Errorf("NormalizeTerminalLanguage(%q) = %q, want go", "", got)
				}
			},
		},
		{
			name:     "terminal_language junk still Loads (ValidateTerminalLanguage is the gate)",
			yamlBody: "hotkey: Ctrl+Shift+Q\nterminal_language: ruby\n",
			validate: func(t *testing.T, cfg config.Config) {
				if cfg.TerminalLanguage != "ruby" {
					t.Errorf("TerminalLanguage = %q, want ruby (value not gated by Load)", cfg.TerminalLanguage)
				}
				if err := config.ValidateTerminalLanguage(cfg.TerminalLanguage); err == nil {
					t.Errorf("ValidateTerminalLanguage(ruby) = nil, want error")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.yamlBody), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := td.loader.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.validate(t, cfg)
		})
	}
}

// NormalizeGlassBlur: nil => DefaultGlassBlur, non-nil => value unchanged.
func TestNormalizeGlassBlur(t *testing.T) {
	if got := config.NormalizeGlassBlur(nil); got != config.DefaultGlassBlur {
		t.Errorf("NormalizeGlassBlur(nil) = %g, want %g", got, config.DefaultGlassBlur)
	}
	for _, v := range []float64{0, 8, 16, 42.5, 500} {
		v := v
		if got := config.NormalizeGlassBlur(&v); got != v {
			t.Errorf("NormalizeGlassBlur(&%g) = %g, want %g", v, got, v)
		}
	}
}

// ValidateGlassBlur accepts finite values in [0, maxGlassBlur]; rejects negative,
// too-large, NaN and Inf.
func TestValidateGlassBlur(t *testing.T) {
	valid := []float64{0, 0.5, 16, 500}
	for _, v := range valid {
		if err := config.ValidateGlassBlur(v); err != nil {
			t.Errorf("ValidateGlassBlur(%g) = %v, want nil", v, err)
		}
	}
	invalid := []float64{-1, -0.001, 500.001, 10000, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, v := range invalid {
		if err := config.ValidateGlassBlur(v); err == nil {
			t.Errorf("ValidateGlassBlur(%g) = nil, want error", v)
		}
	}
}

// Loader.Load() parses the new mute/focus toggles. `mute` is a *bool so an
// ABSENT key yields nil (=> NormalizeMute true: mute the session); an explicit
// `mute: false` yields a non-nil *false. `focus` is a plain bool defaulting to
// the Go zero value false (Focus/DND is opt-in). yaml.Strict() must ACCEPT both
// keys now that they are declared struct fields.
func TestLoader_Load_ParsesMuteFocus(t *testing.T) {
	tests := []struct {
		name        string
		yamlBody    string
		wantMute    bool // NormalizeMute(cfg.Mute)
		wantMuteNil bool // cfg.Mute == nil (absent key)
		wantFocus   bool
	}{
		{
			name:        "both keys absent → mute defaults true, focus false",
			yamlBody:    "hotkey: Ctrl+X\n",
			wantMute:    true,
			wantMuteNil: true,
			wantFocus:   false,
		},
		{
			name:        "mute: false explicit → NormalizeMute false, non-nil",
			yamlBody:    "hotkey: Ctrl+X\nmute: false\n",
			wantMute:    false,
			wantMuteNil: false,
			wantFocus:   false,
		},
		{
			name:        "mute: true explicit → NormalizeMute true, non-nil",
			yamlBody:    "hotkey: Ctrl+X\nmute: true\n",
			wantMute:    true,
			wantMuteNil: false,
			wantFocus:   false,
		},
		{
			name:        "focus: true → Focus true",
			yamlBody:    "hotkey: Ctrl+X\nfocus: true\n",
			wantMute:    true,
			wantMuteNil: true,
			wantFocus:   true,
		},
		{
			name:        "focus: false explicit → Focus false",
			yamlBody:    "hotkey: Ctrl+X\nfocus: false\n",
			wantMute:    true,
			wantMuteNil: true,
			wantFocus:   false,
		},
		{
			name:        "both set: mute false, focus true",
			yamlBody:    "hotkey: Ctrl+X\nmute: false\nfocus: true\n",
			wantMute:    false,
			wantMuteNil: false,
			wantFocus:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.yamlBody), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, created, err := td.loader.Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if created {
				t.Errorf("created = true, want false (file pre-existed)")
			}
			if (cfg.Mute == nil) != tt.wantMuteNil {
				t.Errorf("cfg.Mute == nil is %v, want %v", cfg.Mute == nil, tt.wantMuteNil)
			}
			if got := config.NormalizeMute(cfg.Mute); got != tt.wantMute {
				t.Errorf("NormalizeMute(cfg.Mute) = %v, want %v", got, tt.wantMute)
			}
			if cfg.Focus != tt.wantFocus {
				t.Errorf("cfg.Focus = %v, want %v", cfg.Focus, tt.wantFocus)
			}
		})
	}
}

// NormalizeMute encodes the nil=>true rule directly (unit-level, no IO): nil
// (absent key) => true, *false => false, *true => true.
func TestNormalizeMute(t *testing.T) {
	tr := true
	fa := false
	tests := []struct {
		name string
		in   *bool
		want bool
	}{
		{name: "nil => true (absent key default)", in: nil, want: true},
		{name: "*false => false", in: &fa, want: false},
		{name: "*true => true", in: &tr, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.NormalizeMute(tt.in); got != tt.want {
				t.Errorf("NormalizeMute(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// A freshly-created default config (hotkey only, no mute/focus keys) must
// normalize to mute=true / focus=false: the absent keys carry the defaults,
// and the yaml.Strict() round-trip in Load() still accepts the written file.
func TestLoader_Load_DefaultConfigNormalizesMuteFocus(t *testing.T) {
	td := newTestDeps(t)
	// First Load writes the default (hotkey-only) file.
	cfg, created, err := td.loader.Load()
	if err != nil {
		t.Fatalf("unexpected error on first Load: %v", err)
	}
	if !created {
		t.Fatalf("created = false on fresh path, want true")
	}
	// Default-written file omits mute/focus → defaults apply.
	if cfg.Mute != nil {
		t.Errorf("cfg.Mute = %v, want nil (key absent in default)", cfg.Mute)
	}
	if got := config.NormalizeMute(cfg.Mute); got != true {
		t.Errorf("NormalizeMute(cfg.Mute) = %v, want true", got)
	}
	if cfg.Focus != false {
		t.Errorf("cfg.Focus = %v, want false", cfg.Focus)
	}
	// Second Load re-parses the written file (yaml.Strict round-trip) and must
	// produce the same normalized defaults.
	cfg2, created2, err := td.loader.Load()
	if err != nil {
		t.Fatalf("unexpected error on second Load: %v", err)
	}
	if created2 {
		t.Errorf("created = true on second Load, want false")
	}
	if got := config.NormalizeMute(cfg2.Mute); got != true {
		t.Errorf("second Load NormalizeMute = %v, want true", got)
	}
	if cfg2.Focus != false {
		t.Errorf("second Load cfg.Focus = %v, want false", cfg2.Focus)
	}
}

// hot-reload is a permanent non-feature in v1. *Loader must NOT
// expose Watch/Reload/Subscribe/OnChange/WatchFile methods. This regression
// guard catches accidental additions silently breaking the contract.
func TestLoader_NoHotReload_NoWatchMethod(t *testing.T) {
	_ = config.NewLoader("/dev/null")
	rt := reflect.TypeFor[*config.Loader]()
	forbidden := []string{"Watch", "Reload", "Subscribe", "OnChange", "WatchFile"}
	for _, name := range forbidden {
		if _, ok := rt.MethodByName(name); ok {
			t.Errorf("*Loader has forbidden method %q (no hot-reload)", name)
		}
	}
}

// error path — Load() wraps non-ENOENT read errors with the path
// for diagnostics. We exercise the read-error branch by pointing the loader
// at a path whose parent component is a regular file: os.ReadFile surfaces
// ENOTDIR (not ErrNotExist), so the loader takes the read-error branch
// rather than the default-write branch.
func TestLoader_Load_FailsOnUnreadablePath(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "blocker/config.yml" — the parent "blocker" is a regular file, not a
	// directory. os.ReadFile returns syscall ENOTDIR which is *not* wrapped
	// as fs.ErrNotExist, so writeDefault is bypassed.
	bad := filepath.Join(blocker, "config.yml")
	loader := config.NewLoader(bad)
	cfg, created, err := loader.Load()
	if err == nil {
		t.Fatal("expected error for unreadable path, got nil")
	}
	if created {
		t.Errorf("created = true on error, want false")
	}
	if cfg.Hotkey != "" {
		t.Errorf("cfg.Hotkey = %q on error, want empty", cfg.Hotkey)
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error missing 'read config' wrap: %v", err)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error missing path %q in message: %v", bad, err)
	}
}

// error path — when MkdirAll cannot create the parent directory
// (because the would-be parent dir is itself read-only and missing),
// Load() returns a wrapped "write default config" error. We construct
// this by creating a read-only directory and asking for a config under
// a non-existent subdirectory of it: os.ReadFile fast-path returns
// ErrNotExist (so the writeDefault branch is taken), MkdirAll then
// fails on the read-only parent.
func TestLoader_Load_FailsOnUnwritableParent(t *testing.T) {
	tmp := t.TempDir()
	readOnlyParent := filepath.Join(tmp, "ro")
	if err := os.Mkdir(readOnlyParent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restore writable perms so t.TempDir cleanup can remove it.
		_ = os.Chmod(readOnlyParent, 0o700)
	})
	// "ro/missing-subdir/config.yml" — os.ReadFile gets ENOENT (wrapped as
	// fs.ErrNotExist) for the missing leaf, then writeDefault's MkdirAll
	// trips on the read-only parent.
	bad := filepath.Join(readOnlyParent, "missing-subdir", "config.yml")
	loader := config.NewLoader(bad)
	cfg, created, err := loader.Load()
	if err == nil {
		t.Fatal("expected error for unwritable parent, got nil")
	}
	if created {
		t.Errorf("created = true on error, want false")
	}
	if cfg.Hotkey != "" {
		t.Errorf("cfg.Hotkey = %q on error, want empty", cfg.Hotkey)
	}
	if !strings.Contains(err.Error(), "write default config") {
		t.Errorf("error missing 'write default config' wrap: %v", err)
	}
}

// Atomic write under concurrent dndmode start. Five goroutines
// race on the same fresh path; all must observe a valid Config and the
// final on-disk file must contain the default hotkey. Run with -race to
// catch data races in writeDefault.
func TestLoader_Load_AtomicWriteUnderConcurrentStart(t *testing.T) {
	td := newTestDeps(t)

	const N = 5
	var wg sync.WaitGroup
	results := make([]config.Config, N)
	errs := make([]error, N)

	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			loader := config.NewLoader(td.path)
			cfg, _, err := loader.Load()
			results[idx] = cfg
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
		if results[i].UnlockCode != config.DefaultUnlockCode {
			t.Errorf("goroutine %d unlock code = %q, want %q", i, results[i].UnlockCode, config.DefaultUnlockCode)
		}
	}

	// Final on-disk file must exist with the default unlock code + 0o600 perms.
	body, err := os.ReadFile(td.path)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if !strings.Contains(string(body), config.DefaultUnlockCode) {
		t.Errorf("final file missing default unlock code: %s", body)
	}
	info, err := os.Stat(td.path)
	if err != nil {
		t.Fatalf("stat final file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("final file mode = %#o, want 0o600", mode)
	}
}

// Path() returns the configured path verbatim (used by main for banner
// output — diagnostic-only, no behavioral effect).
func TestLoader_Path(t *testing.T) {
	want := "/tmp/dndmode-test/config.yml"
	loader := config.NewLoader(want)
	if got := loader.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// --- unlock code: validation, weakness heuristic, resolution ---------------

// steps builds a []hotkey.Spec of length n whose entries carry no modifiers —
// the shape of a passphrase-style code ("s w o r d"). Only the LENGTH matters
// to ValidateUnlockCode beyond n == 1, so the keycodes are arbitrary.
func steps(n int) []hotkey.Spec {
	out := make([]hotkey.Spec, n)
	for i := range out {
		out[i] = hotkey.Spec{KeyCode: uint16(i)}
	}
	return out
}

// ValidateUnlockCode gates length + the 1-step modifier requirement. The upper
// bound (hotkey.MaxSteps) is deliberately NOT re-checked here — ParseSequence
// owns it — so a 32-step code must pass.
func TestValidateUnlockCode(t *testing.T) {
	tests := []struct {
		name    string
		steps   []hotkey.Spec
		wantErr bool
	}{
		{name: "0 steps → error", steps: nil, wantErr: true},
		{name: "0 steps (empty slice) → error", steps: []hotkey.Spec{}, wantErr: true},
		{
			name:    "1 step WITH modifier → ok (legacy hotkey shape)",
			steps:   []hotkey.Spec{{Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd, KeyCode: 7}},
			wantErr: false,
		},
		{
			name:    "1 step WITHOUT modifier → error (unlocks on one keypress)",
			steps:   []hotkey.Spec{{KeyCode: 7}},
			wantErr: true,
		},
		{name: "2 steps → error", steps: steps(2), wantErr: true},
		{name: "3 steps → error", steps: steps(3), wantErr: true},
		{name: "4 steps (MinUnlockSteps) → ok", steps: steps(config.MinUnlockSteps), wantErr: false},
		{name: "6 steps → ok", steps: steps(6), wantErr: false},
		{name: "32 steps (MaxSteps, not re-checked here) → ok", steps: steps(hotkey.MaxSteps), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := config.ValidateUnlockCode(tt.steps)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateUnlockCode(%d steps) = nil, want error", len(tt.steps))
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateUnlockCode(%d steps) = %v, want nil", len(tt.steps), err)
			}
		})
	}
}

// IsWeakUnlockCode is a pure length threshold — including for length 1, the
// shipped default, which is exactly the case that must still warn.
func TestIsWeakUnlockCode(t *testing.T) {
	tests := []struct {
		name  string
		steps []hotkey.Spec
		want  bool
	}{
		{name: "1 step (the shipped default) → weak", steps: steps(1), want: true},
		{name: "4 steps → weak", steps: steps(4), want: true},
		{name: "5 steps → weak", steps: steps(5), want: true},
		{name: "6 steps (WeakUnlockSteps) → not weak", steps: steps(config.WeakUnlockSteps), want: false},
		{name: "9 steps → not weak", steps: steps(9), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.IsWeakUnlockCode(tt.steps); got != tt.want {
				t.Errorf("IsWeakUnlockCode(%d steps) = %v, want %v", len(tt.steps), got, tt.want)
			}
		})
	}
}

// ResolveUnlockCode implements the precedence table. Each row is asserted on
// the returned VERIFIER, the returned source and the weak flag, because
// callers phrase their diagnostics off the last two and match off the first.
//
// The verifier is probed through MaxLen and Match rather than by reaching for
// the steps, because the steps are exactly what the Verifier return type
// exists to withhold: main.go must not be able to print the secret it just
// resolved.
func TestResolveUnlockCode(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.Config
		wantLen    int
		wantSource string
		wantWeak   bool
		wantErr    bool
	}{
		{
			name:       "only unlock_code → primary path",
			cfg:        config.Config{UnlockCode: "s w o r d f i s h"},
			wantLen:    9,
			wantSource: config.UnlockSourceCode,
		},
		{
			name:       "only unlock_code, single chord → code of length 1, weak",
			cfg:        config.Config{UnlockCode: config.DefaultUnlockCode},
			wantLen:    1,
			wantSource: config.UnlockSourceCode,
			wantWeak:   true,
		},
		{
			name:       "only hotkey → works, source names the deprecated key, weak",
			cfg:        config.Config{Hotkey: "Ctrl+Option+Cmd+X"},
			wantLen:    1,
			wantSource: config.UnlockSourceHotkey,
			wantWeak:   true,
		},
		{
			name:       "unlock_code of exactly WeakUnlockSteps → not weak",
			cfg:        config.Config{UnlockCode: "s w o r d f"},
			wantLen:    config.WeakUnlockSteps,
			wantSource: config.UnlockSourceCode,
		},
		{
			name:       "unlock_code one step below the threshold → weak",
			cfg:        config.Config{UnlockCode: "s w o r d"},
			wantLen:    config.WeakUnlockSteps - 1,
			wantSource: config.UnlockSourceCode,
			wantWeak:   true,
		},
		{
			name:    "both → error (ambiguous secret)",
			cfg:     config.Config{UnlockCode: "s w o r d", Hotkey: "Ctrl+Cmd+X"},
			wantErr: true,
		},
		{
			name:    "neither → error",
			cfg:     config.Config{},
			wantErr: true,
		},
		{
			name:    "whitespace-only values count as absent",
			cfg:     config.Config{UnlockCode: "   ", Hotkey: "\t"},
			wantErr: true,
		},
		{
			name:       "unlock_code with a bad token → error, source still reported",
			cfg:        config.Config{UnlockCode: "s w nope d"},
			wantSource: config.UnlockSourceCode,
			wantErr:    true,
		},
		{
			name:       "unlock_code of 3 steps → rejected by ValidateUnlockCode",
			cfg:        config.Config{UnlockCode: "s w o"},
			wantSource: config.UnlockSourceCode,
			wantErr:    true,
		},
		{
			name:       "bare-key unlock_code of 1 step → rejected",
			cfg:        config.Config{UnlockCode: "x"},
			wantSource: config.UnlockSourceCode,
			wantErr:    true,
		},
		{
			name:       "modifier-only hotkey → error, source names hotkey",
			cfg:        config.Config{Hotkey: "Ctrl+Cmd"},
			wantSource: config.UnlockSourceHotkey,
			wantErr:    true,
		},
		{
			name:       "multi-step value under the deprecated hotkey key → rejected",
			cfg:        config.Config{Hotkey: "s w o r d"},
			wantSource: config.UnlockSourceHotkey,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			got, source, weak, err := config.ResolveUnlockCode(&cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveUnlockCode() = nil error, want error")
				}
				if got != nil {
					t.Errorf("verifier = %v on error, want nil", got)
				}
				if source != tt.wantSource {
					t.Errorf("source = %q on error, want %q", source, tt.wantSource)
				}
				if weak {
					t.Error("weak = true on error, want false")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveUnlockCode() = %v, want nil", err)
			}
			if _, ok := got.(*matcher.Sequence); !ok {
				t.Errorf("verifier = %T, want *matcher.Sequence for a plaintext source", got)
			}
			// A plaintext code admits exactly one window width, so MinLen ==
			// MaxLen == the step count; that is the only length the resolver
			// still exposes and it is not secret (the caller already holds the
			// config it came from).
			if got.MinLen() != tt.wantLen || got.MaxLen() != tt.wantLen {
				t.Errorf("verifier window = [%d, %d], want [%d, %d]",
					got.MinLen(), got.MaxLen(), tt.wantLen, tt.wantLen)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
			if weak != tt.wantWeak {
				t.Errorf("weak = %v, want %v", weak, tt.wantWeak)
			}
		})
	}
}

// A resolution error must never echo the secret back: the message names the
// KEY, and ParseSequence reports a failing step by position only.
//
// The assertion is per-TOKEN rather than against a hand-picked list of
// fragments. A list is trivially satisfiable by accident — an earlier version
// of this test searched for "s w nope d", "s w" and "w o r d", none of which
// the message could ever contain, while the token the parser actually
// interpolated ("nope") went unchecked and leaked for real. Every whitespace-
// and '+'-separated token of the input is a step of the user's passphrase, so
// every one of them is checked here.
//
// The salt/hash keys are held to the same rule even though base64 of a digest
// is not the secret. Two reasons: decodeUnlockDigest is the newest place a
// config VALUE reaches an error string, so it is the likeliest place for the
// rule to be forgotten; and a config in the wild can carry a hand-edited
// unlock_hash that is not a digest at all. Every field of the config is
// tokenised here, not just the one the case is named after.
func TestResolveUnlockCode_ErrorNeverEchoesSecret(t *testing.T) {
	validSalt, validHash := digestPair(t, mustParse(t, "s w o r d f"))

	cases := []struct {
		name string
		cfg  config.Config
	}{
		{"unknown token mid-code", config.Config{UnlockCode: "s w nope d"}},
		{"two keys in one step", config.Config{UnlockCode: "s w o r d f+i s h"}},
		{"duplicate modifier in a step", config.Config{UnlockCode: "s w o ctrl+ctrl+d f i"}},
		{"empty token in a step", config.Config{UnlockCode: "s w o r d f++i"}},
		{"too short after parsing", config.Config{UnlockCode: "s w o"}},
		{"legacy hotkey key", config.Config{Hotkey: "ctrl+option+zebra"}},
		{"legacy hotkey, whole value unparsable", config.Config{Hotkey: "s w o r d f i s h"}},
		{"salt is not base64", config.Config{
			UnlockSalt: "zebraquokkanarwhal!!", UnlockHash: validHash}},
		{"hash is not base64", config.Config{
			UnlockSalt: validSalt, UnlockHash: "axolotlpangolinseahorse!!"}},
		{"salt is valid base64 of the wrong width", config.Config{
			UnlockSalt: base64.StdEncoding.EncodeToString([]byte("quokka")), UnlockHash: validHash}},
		{"hash is valid base64 of the wrong width", config.Config{
			UnlockSalt: validSalt, UnlockHash: base64.StdEncoding.EncodeToString([]byte("narwhal"))}},
		{"half a pair: salt without hash", config.Config{UnlockSalt: validSalt}},
		{"half a pair: hash without salt", config.Config{UnlockHash: validHash}},
		{"ambiguous: hash pair alongside unlock_code", config.Config{
			UnlockCode: "zebra quokka narwhal axolotl",
			UnlockSalt: validSalt, UnlockHash: validHash}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := config.ResolveUnlockCode(&tt.cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()

			// EVERY value-carrying field is checked, not just the one this
			// case is named after: a message that named the wrong key's value
			// would still be a leak.
			//
			// The two field shapes tokenise differently, and mixing them would
			// weaken the pin rather than strengthen it. A step-shaped value
			// separates steps by whitespace and modifiers by '+', so both are
			// separators. In base64 '+' is a DATA character, so splitting on it
			// would shred the value into short fragments like "9w" that match
			// ordinary prose and turn this assertion into noise. Each field is
			// therefore split by its own grammar; every resulting token is
			// checked.
			var toks []string
			for _, stepShaped := range []string{tt.cfg.UnlockCode, tt.cfg.Hotkey} {
				toks = append(toks, strings.FieldsFunc(stepShaped, func(r rune) bool {
					return r == ' ' || r == '\t' || r == '+'
				})...)
			}
			for _, b64 := range []string{tt.cfg.UnlockSalt, tt.cfg.UnlockHash} {
				toks = append(toks, strings.Fields(b64)...)
			}
			for _, tok := range toks {
				// One-character tokens are excluded: a single letter matches
				// ordinary prose ("a", "s" in "steps") and would make the
				// assertion fire on messages that leak nothing. Every token
				// long enough to be searched for meaningfully is checked.
				if len(tok) < 2 {
					continue
				}
				if strings.Contains(msg, tok) {
					t.Errorf("error message echoes the secret token %q: %v", tok, err)
				}
			}
		})
	}
}

// mustParse is the fixture spelling of hotkey.ParseSequence: these tests care
// about what the resolver does with a valid code, not about the parser.
func mustParse(t *testing.T, code string) []hotkey.Spec {
	t.Helper()
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", code, err)
	}
	return steps
}

// The YAML layer must honour the same no-echo contract as the parser: a
// syntax error in config.yml is reported by [line:col] and category, never by
// quoting the offending source. goccy's pretty formatter prints the lines
// AROUND the error, and in this file that is nearly always
// `unlock_code: <the secret>` — so an unrelated typo (a misspelled key, a bad
// indent) would print the whole unlock code to stderr, on exactly the recovery
// path the README recommends. Mirrors
// TestResolveUnlockCode_ErrorNeverEchoesSecret one layer up: EVERY token of the
// secret is searched for, not a sample.
func TestLoader_Load_ParseErrorNeverEchoesSecret(t *testing.T) {
	const secret = "zebra quokka narwhal axolotl"

	cases := []struct {
		name string
		body string
	}{
		{"unknown key on a later line", "unlock_code: " + secret + "\nuntrusted_field: payload\n"},
		{"bad indent under the secret", "unlock_code: " + secret + "\n  bad: indent\n"},
		{"secret opens with a YAML indicator", "unlock_code: - " + secret + "\n"},
		{"unterminated quote around the secret", "unlock_code: \"" + secret + "\n"},
		{"tab in the secret line", "unlock_code:\t" + secret + "\n\tbad: tab\n"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}

			_, _, err := td.loader.Load()
			if err == nil {
				t.Fatal("expected a parse error")
			}
			msg := err.Error()
			for _, tok := range strings.Fields(secret) {
				if strings.Contains(msg, tok) {
					t.Errorf("parse error echoes the secret token %q: %v", tok, err)
				}
			}
			// The location must survive the redaction — without [line:col]
			// the diagnostic would be unusable, which is the pressure that
			// would push someone back to inclSource=true.
			if !regexp.MustCompile(`\[\d+:\d+\]`).MatchString(msg) {
				t.Errorf("error message missing line:col [L:C] format: %q", msg)
			}
		})
	}
}

// A config carrying unlock_code survives the YAML round-trip through Load()
// and resolves into the sequence it denotes.
func TestLoader_Load_UnlockCodeRoundTrip(t *testing.T) {
	td := newTestDeps(t)
	if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
		t.Fatal(err)
	}
	const code = "ctrl+s w o r d cmd+z"
	if err := os.WriteFile(td.path, []byte("unlock_code: "+code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, created, err := td.loader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if created {
		t.Errorf("created = true, want false (file pre-existed)")
	}
	if cfg.UnlockCode != code {
		t.Errorf("cfg.UnlockCode = %q, want %q", cfg.UnlockCode, code)
	}

	got, source, weak, rerr := config.ResolveUnlockCode(&cfg)
	if rerr != nil {
		t.Fatalf("ResolveUnlockCode: %v", rerr)
	}
	if got.MaxLen() != 6 {
		t.Errorf("verifier window = %d, want 6", got.MaxLen())
	}
	if source != config.UnlockSourceCode {
		t.Errorf("source = %q, want %q", source, config.UnlockSourceCode)
	}
	if weak {
		t.Error("weak = true for a 6-step code, want false (6 is the recommended floor)")
	}
	// The per-step modifiers are no longer returned, so they are asserted the
	// only way that still matters: by typing the code at the verifier. This is
	// a stronger check than the old field comparison — it exercises the mask
	// and the exact-equality rule the poller actually depends on.
	if !got.Match(keyEvents(mustParse(t, code))) {
		t.Error("the round-tripped verifier does not match the code it was built from")
	}
	if got.Match(keyEvents(mustParse(t, "Ctrl+z e b r a Cmd+q"))) {
		t.Error("the round-tripped verifier matched a different 6-step code")
	}
}

// The generated default config must round-trip through its own loader AND
// resolve — the template is the thing most users never edit, so a drift here
// would ship a config that cannot start.
func TestLoader_Load_GeneratedDefaultResolves(t *testing.T) {
	td := newTestDeps(t)
	cfg, created, err := td.loader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}

	got, source, weak, rerr := config.ResolveUnlockCode(&cfg)
	if rerr != nil {
		t.Fatalf("generated default config does not resolve: %v", rerr)
	}
	if got.MaxLen() != 1 {
		t.Errorf("verifier window = %d, want 1 (the default is a single chord)", got.MaxLen())
	}
	if source != config.UnlockSourceCode {
		t.Errorf("source = %q, want %q", source, config.UnlockSourceCode)
	}
	if !weak {
		t.Error("the shipped default must resolve as weak so an untouched config still warns")
	}
	if cfg.UnlockSalt != "" || cfg.UnlockHash != "" {
		t.Errorf("generated default carries unlock_salt=%q unlock_hash=%q, want both empty: "+
			"the template documents the pair in prose and must never emit it as an active key "+
			"(an active pair next to unlock_code is an ambiguous secret)",
			cfg.UnlockSalt, cfg.UnlockHash)
	}
}

// The generated config documents the `--set-password` pair, and documents it
// as COMMENT ONLY. Both halves of that are pinned here.
//
// The prose has to be there because the config file is the manual: a user who
// runs `--set-password` and later reads their config must find out from the
// file itself that the two base64 blobs are one secret, that they are mutually
// exclusive with unlock_code, and how to get back to a plaintext code.
//
// The active-key half is the security property. An `unlock_salt:` or
// `unlock_hash:` line at column 0 in the generated file would sit next to the
// active `unlock_code:` and make every freshly-created config an ambiguous
// secret that ResolveUnlockCode refuses — i.e. a dndmode that cannot start on
// a clean machine. Line-anchored regexps, not Contains, because every mention
// in the template is inside a comment and Contains would match those too.
func TestLoader_Load_GeneratedDefaultDocumentsHashPair(t *testing.T) {
	td := newTestDeps(t)
	if _, _, err := td.loader.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, err := os.ReadFile(td.path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	body := string(raw)

	for _, want := range []string{
		"# --- unlock_salt / unlock_hash",
		"--set-password",
		"unlock_code",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generated config never mentions %q; the pair must be documented where the user reads it:\n%s", want, body)
		}
	}

	for _, key := range []string{"unlock_salt", "unlock_hash"} {
		if regexp.MustCompile(`(?m)^` + key + `[ \t]*:`).MatchString(body) {
			t.Errorf("generated config has an ACTIVE %q key alongside unlock_code — "+
				"that is an ambiguous unlock secret and would refuse to start:\n%s", key, body)
		}
	}
}

// Nothing in defaultConfigTemplate may be a literal '%' except the single %s
// that carries the unlock code: the body goes through fmt.Appendf, so a stray
// one turns into `%!(NOVERB)` (or eats the next character as a verb) inside the
// file the user reads as documentation.
//
// The check is on the OUTPUT rather than on the template constant, which is
// unexported and out of reach from this black-box package — and the output is
// the stronger place to look anyway, since it catches both the malformed-verb
// rendering and a second verb that would consume an argument nobody passed.
// It is exact rather than a `%!` prefix scan because the default unlock code
// contains no '%' either, so a correctly-rendered file has none at all.
func TestLoader_Load_GeneratedDefaultHasNoStrayPercent(t *testing.T) {
	td := newTestDeps(t)
	if _, _, err := td.loader.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, err := os.ReadFile(td.path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if i := bytes.IndexByte(raw, '%'); i >= 0 {
		line := 1 + bytes.Count(raw[:i], []byte("\n"))
		t.Errorf("generated config contains '%%' at line %d — defaultConfigTemplate is fed through "+
			"fmt.Appendf and may hold no literal '%%' beyond its single %%s:\n%s", line, raw)
	}
}

// YAML scalars that look boolean in YAML 1.1 ("n", "no", "y", "yes", "on",
// "off") must arrive as STRINGS, because they are all legitimate single-step
// unlock codes. goccy/go-yaml decodes a scalar straight into a string field, so
// no quoting is required — this test pins that behavior; if a future goccy
// release starts coercing, the generated template must grow a quoting note.
func TestLoader_Load_BooleanLookingScalars(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "n", want: "n"},
		{in: "no", want: "no"},
		{in: "y", want: "y"},
		{in: "yes", want: "yes"},
		{in: "on", want: "on"},
		{in: "off", want: "off"},
		{in: "n o", want: "n o"},
		{in: "y e s n o", want: "y e s n o"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte("unlock_code: "+tt.in+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := td.loader.Load()
			if err != nil {
				t.Fatalf("Load(%q): %v", tt.in, err)
			}
			if cfg.UnlockCode != tt.want {
				t.Errorf("cfg.UnlockCode = %q, want %q (bool-looking scalar must stay a string)", cfg.UnlockCode, tt.want)
			}
		})
	}
}

// TestLoader_Load_LeadingPunctuationNeedsQuoting pins the YAML edge the key
// table walks straight into: `-`, `[`, `]`, `'` and backtick are documented
// as valid unlock-code keys, but they are also YAML indicator characters, so
// a code that STARTS with one is not a plain scalar at all.
//
// The failure this guards against is unusually unfriendly: the config never
// reaches hotkey.ParseSequence, the error is a raw YAML parse message
// pointing at a column, and startup is silent without --debug — so the user
// sees exit 1 and nothing else while looking at a value that matches the
// documented grammar character for character.
//
// The test therefore asserts BOTH halves: bare leading punctuation fails at
// the YAML layer (which is why the template and README tell you to quote it),
// and the quoted form round-trips intact. If a future goccy/go-yaml release
// starts accepting the bare form, the "unquoted" subtests fail and the
// quoting advice can be relaxed — that is the intended signal, not a
// regression.
func TestLoader_Load_LeadingPunctuationNeedsQuoting(t *testing.T) {
	// Every documented punctuation key that is also a YAML indicator in
	// first position. `=` `;` `,` `.` `/` `\` are deliberately absent:
	// they parse bare, and pinning them here would claim a problem that
	// does not exist.
	indicators := []string{"-", "[", "]", "'", "`"}

	for _, ch := range indicators {
		t.Run("unquoted "+ch, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte("unlock_code: "+ch+" a b c d\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := td.loader.Load(); err == nil {
				t.Fatalf("Load() with a bare leading %q unexpectedly succeeded; "+
					"if the YAML parser now accepts this, drop the quoting note from "+
					"defaultConfigTemplate and README instead of deleting this test", ch)
			}
		})

		t.Run("quoted "+ch, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(`unlock_code: "`+ch+` a b c d"`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := td.loader.Load()
			if err != nil {
				t.Fatalf("Load() with a quoted leading %q: %v (the quoting workaround "+
					"documented in defaultConfigTemplate must work)", ch, err)
			}
			if want := ch + " a b c d"; cfg.UnlockCode != want {
				t.Errorf("cfg.UnlockCode = %q, want %q", cfg.UnlockCode, want)
			}
		})
	}
}

// (unlock_salt / unlock_hash) — Loader.Load() parses the salted-digest pair
// written by --set-password. yaml.Strict() must ACCEPT both keys now that they
// are declared struct fields, and must still REJECT an unknown key standing
// next to them: adding fields widens the schema by exactly two names, it does
// not open the file to arbitrary keys.
//
// The values are deliberately NOT validated here. Strict guards unknown KEYS
// only, so junk base64 lands in the struct verbatim; ResolveUnlockCode is the
// real gate (Task 4). These cases pin that separation — a future value check
// smuggled into Load() would break them.
func TestLoader_Load_ParsesUnlockSaltHash(t *testing.T) {
	const (
		validSalt = "8Qk2vN1pRr7sT0uW3xYz4A=="                     // 16 bytes
		validHash = "n7Kx0Qe5RtY8uI3oP1aS2dF4gH6jK9lZ0xC7vB5nM8w=" // 32 bytes
	)

	tests := []struct {
		name     string
		yamlBody string
		wantSalt string
		wantHash string
		wantErr  bool
	}{
		{
			name:     "both keys → both parse verbatim",
			yamlBody: "unlock_salt: " + validSalt + "\nunlock_hash: " + validHash + "\n",
			wantSalt: validSalt,
			wantHash: validHash,
		},
		{
			name:     "keys absent → both empty (Go zero value)",
			yamlBody: "unlock_code: s w o r d f i s h\n",
			wantSalt: "",
			wantHash: "",
		},
		{
			name:     "salt without hash → parses; half a pair is ResolveUnlockCode's problem",
			yamlBody: "unlock_salt: " + validSalt + "\n",
			wantSalt: validSalt,
			wantHash: "",
		},
		{
			name:     "hash without salt → parses; half a pair is ResolveUnlockCode's problem",
			yamlBody: "unlock_hash: " + validHash + "\n",
			wantSalt: "",
			wantHash: validHash,
		},
		{
			name:     "junk values → parse fine; Strict guards KEYS, not VALUES",
			yamlBody: "unlock_salt: not-base64!!\nunlock_hash: also-not-base64!!\n",
			wantSalt: "not-base64!!",
			wantHash: "also-not-base64!!",
		},
		{
			name: "unknown key alongside the pair → still rejected by yaml.Strict",
			yamlBody: "unlock_salt: " + validSalt + "\nunlock_hash: " + validHash +
				"\nuntrusted_field: payload\n",
			wantErr: true,
		},
		{
			name: "near-miss key name alongside the pair → still rejected",
			yamlBody: "unlock_salt: " + validSalt + "\nunlock_hash: " + validHash +
				"\nunlock_length: 9\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(td.path, []byte(tt.yamlBody), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, created, err := td.loader.Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load() = nil error, want strict-mode error for the unknown key")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v, want nil", err)
			}
			if created {
				t.Errorf("created = true, want false (file pre-existed)")
			}
			if cfg.UnlockSalt != tt.wantSalt {
				t.Errorf("cfg.UnlockSalt = %q, want %q", cfg.UnlockSalt, tt.wantSalt)
			}
			if cfg.UnlockHash != tt.wantHash {
				t.Errorf("cfg.UnlockHash = %q, want %q", cfg.UnlockHash, tt.wantHash)
			}
		})
	}
}

// The salted-digest source: a valid unlock_salt / unlock_hash pair resolves
// to a *matcher.Digest that matches the very steps --set-password recorded,
// and to nothing else.
//
// Building the fixture through matcher.HashSteps rather than pasting a
// literal is deliberate: this test is the one place the config layer and the
// hashing layer have to agree on the preimage, so a fixture that could not
// have come out of HashSteps would pin the agreement to a stale constant.
func TestResolveUnlockCode_HashSource(t *testing.T) {
	steps := mustParse(t, "s w o r d f i s h")
	saltB64, hashB64 := digestPair(t, steps)

	cfg := config.Config{UnlockSalt: saltB64, UnlockHash: hashB64}
	got, source, weak, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		t.Fatalf("ResolveUnlockCode() = %v, want nil", err)
	}
	if _, ok := got.(*matcher.Digest); !ok {
		t.Errorf("verifier = %T, want *matcher.Digest", got)
	}
	if source != config.UnlockSourceHash {
		t.Errorf("source = %q, want %q", source, config.UnlockSourceHash)
	}
	if weak {
		t.Error("weak = true for a hash source, want false (a digest stores no length)")
	}
	// A digest betrays no length, so it must offer every window the grammar
	// admits — anything narrower would make some legitimate secret unenterable.
	if got.MinLen() != 1 || got.MaxLen() != hotkey.MaxSteps {
		t.Errorf("verifier window = [%d, %d], want [1, %d]",
			got.MinLen(), got.MaxLen(), hotkey.MaxSteps)
	}
	if !got.Match(keyEvents(steps)) {
		t.Error("the resolved digest does not match the steps it was built from")
	}
	if got.Match(keyEvents(mustParse(t, "s w o r d f i s"))) {
		t.Error("the resolved digest matched a prefix of the recorded steps")
	}
	if got.Match(keyEvents(mustParse(t, "z e b r a q u o k"))) {
		t.Error("the resolved digest matched an unrelated code of the same length")
	}
}

// The weak flag is identically false for a hash source, whatever the recorded
// code's length was — including lengths that WOULD warn as plaintext.
//
// This is the property that makes the flag safe to compute in the resolver at
// all. main.go prints the weak warning conditionally, so the warning's mere
// presence is a statement about the secret's length; for the hashed form the
// length is not on disk, so the honest answer is "never warn" rather than
// "warn if we can guess". A regression that derived the flag from, say, the
// digest's MaxLen would make every hashed config warn on every start.
func TestResolveUnlockCode_HashSourceNeverWeak(t *testing.T) {
	for _, code := range []string{
		"Ctrl+Option+Cmd+X", // 1 step — weak as plaintext
		"s w o r d",         // 5 steps — weak as plaintext
		"s w o r d f",       // 6 steps — exactly the threshold
		"s w o r d f i s h", // 9 steps — comfortably above it
	} {
		t.Run(code, func(t *testing.T) {
			steps := mustParse(t, code)

			// Sanity: the same steps as PLAINTEXT must reach the opposite
			// verdict for the short cases, or this test proves nothing.
			plain := config.Config{UnlockCode: code}
			_, _, plainWeak, err := config.ResolveUnlockCode(&plain)
			if err != nil {
				t.Fatalf("plaintext control: ResolveUnlockCode() = %v", err)
			}
			if plainWeak != config.IsWeakUnlockCode(steps) {
				t.Fatalf("plaintext control: weak = %v, want %v", plainWeak, config.IsWeakUnlockCode(steps))
			}

			saltB64, hashB64 := digestPair(t, steps)
			hashed := config.Config{UnlockSalt: saltB64, UnlockHash: hashB64}
			got, source, weak, err := config.ResolveUnlockCode(&hashed)
			if err != nil {
				t.Fatalf("ResolveUnlockCode() = %v, want nil", err)
			}
			if weak {
				t.Errorf("weak = true for the hashed form of a %d-step code, want false", len(steps))
			}
			if source != config.UnlockSourceHash {
				t.Errorf("source = %q, want %q", source, config.UnlockSourceHash)
			}
			if !got.Match(keyEvents(steps)) {
				t.Error("the resolved digest does not match the steps it was built from")
			}
		})
	}
}

// decodeUnlockDigest is unexported and is tested THROUGH ResolveUnlockCode:
// config_test is a black-box package, and its only caller is already
// exported, so exporting it to reach it would widen the package surface for
// nothing.
//
// yaml.Strict() only guards unknown KEYS, so any of these values parses out
// of the file happily and lands here — this is the real gate.
func TestResolveUnlockCode_HashSourceDecodeErrors(t *testing.T) {
	validSalt, validHash := digestPair(t, mustParse(t, "s w o r d f"))

	tests := []struct {
		name string
		cfg  config.Config
	}{
		{
			name: "salt is not base64",
			cfg:  config.Config{UnlockSalt: "!!not-base64!!", UnlockHash: validHash},
		},
		{
			name: "hash is not base64",
			cfg:  config.Config{UnlockSalt: validSalt, UnlockHash: "!!not-base64!!"},
		},
		{
			name: "salt is valid base64 but too short",
			cfg: config.Config{
				UnlockSalt: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, matcher.SaltLen-1)),
				UnlockHash: validHash,
			},
		},
		{
			name: "salt is valid base64 but too long",
			cfg: config.Config{
				UnlockSalt: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, matcher.SaltLen+1)),
				UnlockHash: validHash,
			},
		},
		{
			name: "hash is valid base64 but too short",
			cfg: config.Config{
				UnlockSalt: validSalt,
				UnlockHash: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31)),
			},
		},
		{
			name: "hash is valid base64 but too long",
			cfg: config.Config{
				UnlockSalt: validSalt,
				UnlockHash: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 33)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			got, source, weak, err := config.ResolveUnlockCode(&cfg)
			// A malformed value still SELECTED a source — one key was
			// unambiguously in play and merely unusable. That is what
			// distinguishes these from the half-pair and ambiguity errors,
			// which report no source at all.
			if source != config.UnlockSourceHash {
				t.Errorf("source = %q on a decode error, want %q", source, config.UnlockSourceHash)
			}
			if err == nil {
				t.Fatal("ResolveUnlockCode() = nil error, want error")
			}
			if got != nil {
				t.Errorf("verifier = %v on error, want nil", got)
			}
			if weak {
				t.Error("weak = true on error, want false")
			}
		})
	}
}

// A resolvable pair is one secret in two keys; half of it is a botched
// --set-password, not a fallback to some other source. The error names the
// key that IS set and the one that is missing, so the fix is obvious, and it
// reports NO source: half a pair never selected one.
func TestResolveUnlockCode_HalfPair(t *testing.T) {
	validSalt, validHash := digestPair(t, mustParse(t, "s w o r d f"))

	tests := []struct {
		name     string
		cfg      config.Config
		wantMsgs []string
	}{
		{
			name:     "salt without hash",
			cfg:      config.Config{UnlockSalt: validSalt},
			wantMsgs: []string{"unlock_salt", "unlock_hash"},
		},
		{
			name:     "hash without salt",
			cfg:      config.Config{UnlockHash: validHash},
			wantMsgs: []string{"unlock_salt", "unlock_hash"},
		},
		{
			// Precedence: the half-pair check runs ahead of the source count,
			// so this reports the actionable diagnostic rather than "ambiguous".
			name:     "salt without hash, alongside a valid unlock_code",
			cfg:      config.Config{UnlockCode: "s w o r d f", UnlockSalt: validSalt},
			wantMsgs: []string{"unlock_salt", "unlock_hash"},
		},
		{
			name:     "whitespace-only hash counts as absent",
			cfg:      config.Config{UnlockSalt: validSalt, UnlockHash: "  \t "},
			wantMsgs: []string{"unlock_salt", "unlock_hash"},
		},
		{
			name:     "whitespace-only salt counts as absent",
			cfg:      config.Config{UnlockSalt: "   ", UnlockHash: validHash},
			wantMsgs: []string{"unlock_salt", "unlock_hash"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			got, source, weak, err := config.ResolveUnlockCode(&cfg)
			if err == nil {
				t.Fatal("ResolveUnlockCode() = nil error, want error")
			}
			if got != nil {
				t.Errorf("verifier = %v on error, want nil", got)
			}
			if source != "" {
				t.Errorf("source = %q on a half pair, want \"\" (no key was selected)", source)
			}
			if weak {
				t.Error("weak = true on error, want false")
			}
			for _, want := range tt.wantMsgs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// Two secrets are no secret: every pairwise combination of the three sources,
// plus all three at once, is an error that names the offending KEYS.
//
// Every combination is spelled out rather than sampled because they do not
// share a code path in an obvious way — the hash source is detected by a pair
// of fields while the other two are single fields, so a regression that only
// counted the string-valued keys would pass a sampled test.
func TestResolveUnlockCode_AmbiguousSources(t *testing.T) {
	validSalt, validHash := digestPair(t, mustParse(t, "s w o r d f"))

	tests := []struct {
		name     string
		cfg      config.Config
		wantKeys []string
	}{
		{
			name:     "unlock_code + hotkey",
			cfg:      config.Config{UnlockCode: "s w o r d f", Hotkey: "Ctrl+Cmd+X"},
			wantKeys: []string{config.UnlockSourceCode, config.UnlockSourceHotkey},
		},
		{
			name: "unlock_code + hash pair",
			cfg: config.Config{
				UnlockCode: "s w o r d f",
				UnlockSalt: validSalt, UnlockHash: validHash,
			},
			wantKeys: []string{config.UnlockSourceCode, config.UnlockSourceHash},
		},
		{
			name: "hotkey + hash pair",
			cfg: config.Config{
				Hotkey:     "Ctrl+Option+Cmd+X",
				UnlockSalt: validSalt, UnlockHash: validHash,
			},
			wantKeys: []string{config.UnlockSourceHotkey, config.UnlockSourceHash},
		},
		{
			name: "all three",
			cfg: config.Config{
				UnlockCode: "s w o r d f",
				Hotkey:     "Ctrl+Option+Cmd+X",
				UnlockSalt: validSalt, UnlockHash: validHash,
			},
			wantKeys: []string{
				config.UnlockSourceCode, config.UnlockSourceHotkey, config.UnlockSourceHash,
			},
		},
		{
			name: "two sources where the OTHER one is invalid — still ambiguous, not parsed",
			cfg: config.Config{
				UnlockCode: "s w nope d",
				UnlockSalt: validSalt, UnlockHash: validHash,
			},
			wantKeys: []string{config.UnlockSourceCode, config.UnlockSourceHash},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			got, source, weak, err := config.ResolveUnlockCode(&cfg)
			if err == nil {
				t.Fatal("ResolveUnlockCode() = nil error, want error")
			}
			if got != nil {
				t.Errorf("verifier = %v on error, want nil", got)
			}
			if source != "" {
				t.Errorf("source = %q on an ambiguous config, want \"\"", source)
			}
			if weak {
				t.Error("weak = true on error, want false")
			}
			for _, key := range tt.wantKeys {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error %q does not name the offending key %q", err, key)
				}
			}
			// The resolver must not have mutated the caller's config.
			if !reflect.DeepEqual(cfg, tt.cfg) {
				t.Errorf("ResolveUnlockCode mutated the config: got %+v, want %+v", cfg, tt.cfg)
			}
		})
	}
}

// UnlockSourceHash names the key that actually carries the secret, and it must
// be distinct from the other two — callers switch on these strings, so a
// collision would silently merge two branches of the precedence table.
func TestUnlockSourceHash_Constant(t *testing.T) {
	if config.UnlockSourceHash != "unlock_hash" {
		t.Errorf("UnlockSourceHash = %q, want %q (it must name the config key verbatim)",
			config.UnlockSourceHash, "unlock_hash")
	}
	sources := []string{config.UnlockSourceCode, config.UnlockSourceHotkey, config.UnlockSourceHash}
	seen := make(map[string]bool, len(sources))
	for _, s := range sources {
		if seen[s] {
			t.Fatalf("duplicate unlock source constant %q", s)
		}
		seen[s] = true
	}
}

// TestLoader_LoadWithSource_ReturnsTheBytesItParsed pins the reason the method
// exists: the returned bytes must be the ones the returned Config was parsed
// from, on both paths.
//
// A session hashes them and re-checks that hash against config.yml under the
// publish lock before it raises the shield, so that a --set-password committing
// mid-startup is caught. If these bytes came from a SECOND read of the path
// instead, an atomic rename landing between the two reads would pair the old
// Config with the new file's digest, the re-check would find nothing changed,
// and the shield would go up answering to the superseded secret — the failure
// the check was added to catch, produced by the check itself.
func TestLoader_LoadWithSource_ReturnsTheBytesItParsed(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		td := newTestDeps(t)
		body := []byte("unlock_code: ctrl+option+cmd+x ctrl+option+cmd+y\nfocus: true\n")
		if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(td.path, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		cfg, raw, created, err := td.loader.LoadWithSource()
		if err != nil {
			t.Fatalf("LoadWithSource: %v", err)
		}
		if created {
			t.Error("created = true for a file that already existed")
		}
		if !bytes.Equal(raw, body) {
			t.Errorf("raw = %q, want the file's own bytes %q", raw, body)
		}
		if !cfg.Focus {
			t.Error("cfg came from different bytes than raw (focus lost)")
		}
	})

	t.Run("first run returns what it wrote", func(t *testing.T) {
		td := newTestDeps(t)

		cfg, raw, created, err := td.loader.LoadWithSource()
		if err != nil {
			t.Fatalf("LoadWithSource: %v", err)
		}
		if !created {
			t.Fatal("created = false on a fresh path")
		}
		if cfg.UnlockCode != config.DefaultUnlockCode {
			t.Errorf("cfg.UnlockCode = %q, want the default", cfg.UnlockCode)
		}
		onDisk, rerr := os.ReadFile(td.path)
		if rerr != nil {
			t.Fatalf("read back: %v", rerr)
		}
		if !bytes.Equal(raw, onDisk) {
			t.Error("the bytes returned on the first run differ from the ones published")
		}
	})

	t.Run("no bytes on the error paths", func(t *testing.T) {
		td := newTestDeps(t)
		if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Unknown key → yaml.Strict rejects it. A caller must not be handed
		// bytes it could fingerprint alongside a zero Config.
		if err := os.WriteFile(td.path, []byte("nope: 1\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, raw, _, err := td.loader.LoadWithSource()
		if err == nil {
			t.Fatal("LoadWithSource accepted an unknown key")
		}
		if raw != nil {
			t.Errorf("raw = %q on the parse-error path, want nil", raw)
		}
	})
}

// Load stays a thin wrapper over LoadWithSource: same Config, same created
// flag, same errors. The two must not drift into separate read paths — a second
// implementation is a second place for the split above to reopen.
func TestLoader_Load_AgreesWithLoadWithSource(t *testing.T) {
	td := newTestDeps(t)

	cfg1, raw, created1, err1 := td.loader.LoadWithSource()
	if err1 != nil {
		t.Fatalf("LoadWithSource: %v", err1)
	}
	if !created1 || len(raw) == 0 {
		t.Fatalf("first call: created = %v, len(raw) = %d", created1, len(raw))
	}

	cfg2, created2, err2 := td.loader.Load()
	if err2 != nil {
		t.Fatalf("Load: %v", err2)
	}
	if created2 {
		t.Error("Load reported created = true for a file LoadWithSource just wrote")
	}
	if cfg2.UnlockCode != cfg1.UnlockCode {
		t.Errorf("Load unlock_code = %q, LoadWithSource = %q", cfg2.UnlockCode, cfg1.UnlockCode)
	}
}
