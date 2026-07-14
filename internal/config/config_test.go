//go:build darwin

package config_test

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
)

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
			name:       "fresh path → creates parent dir (0o700) + file (0o600) with default hotkey",
			setupMocks: func(td *testDeps) {},
			validateResp: func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !created {
					t.Errorf("created = false, want true")
				}
				if cfg.Hotkey != config.DefaultHotkey {
					t.Errorf("cfg.Hotkey = %q, want %q", cfg.Hotkey, config.DefaultHotkey)
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
				if !strings.Contains(string(body), "hotkey:") {
					t.Errorf("written file missing 'hotkey:' key: %s", body)
				}
				if !strings.Contains(string(body), config.DefaultHotkey) {
					t.Errorf("written file missing default hotkey value: %s", body)
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
		if results[i].Hotkey != config.DefaultHotkey {
			t.Errorf("goroutine %d hotkey = %q, want %q", i, results[i].Hotkey, config.DefaultHotkey)
		}
	}

	// Final on-disk file must exist with default hotkey + 0o600 perms.
	body, err := os.ReadFile(td.path)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if !strings.Contains(string(body), config.DefaultHotkey) {
		t.Errorf("final file missing default hotkey: %s", body)
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
