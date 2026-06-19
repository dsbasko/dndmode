//go:build darwin

package config_test

import (
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
		name         string
		yamlBody     string
		expectLineCol bool
		setupMocks   func(td *testDeps, body string)
		validateResp func(t *testing.T, td *testDeps, cfg config.Config, created bool, err error, expectLineCol bool)
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

// QUICK-gh8 — overlay_style is an optional string field. yaml.Strict() rejects
// unknown KEYS but does NOT validate VALUES, so a recognised key with any value
// parses cleanly; value validation is the caller's job (main.go via
// config.ValidateOverlayStyle). These 4 cases pin: (1) `matrix` round-trips,
// (2) `black` round-trips, (3) an ABSENT key leaves OverlayStyle == "" which
// NormalizeOverlayStyle maps to "black", (4) an invalid VALUE still Load()s but
// ValidateOverlayStyle rejects it while accepting "", "black", "matrix".
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
				for _, ok := range []string{"", config.OverlayStyleBlack, config.OverlayStyleMatrix, config.OverlayStyleGlass} {
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

// hot-reload is a permanent non-feature in v1. *Loader must NOT
// expose Watch/Reload/Subscribe/OnChange/WatchFile methods. This regression
// guard catches accidental additions silently breaking the contract.
func TestLoader_NoHotReload_NoWatchMethod(t *testing.T) {
	loader := config.NewLoader("/dev/null")
	rt := reflect.TypeOf(loader)
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

	for i := 0; i < N; i++ {
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
