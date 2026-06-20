//go:build darwin

// Package config loads and writes the dndmode YAML configuration. The
// config schema in v1 is intentionally minimal (just `hotkey`); migration
// to nested/versioned schema is deferred (the design notes).
//
// Hot-reload is NOT supported: Load() is invoked exactly once at
// PreFlight. Loader has no Watch/Reload/Subscribe methods by design.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

const (
	// DefaultHotkey is the hotkey written to a freshly-created config.yml
	//. User can edit the file post-creation.
	DefaultHotkey = "Ctrl+Option+Cmd+X"

	// OverlayStyleBlack is the v1 default look: a plain opaque-black shield.
	// An absent/empty overlay_style normalizes to this (NormalizeOverlayStyle).
	OverlayStyleBlack = "black"
	// OverlayStyleMatrix renders animated green digital rain over the opaque
	// black shield (cosmetic only; every window guarantee is unchanged).
	OverlayStyleMatrix = "matrix"
	// OverlayStyleGlass makes the shield TRANSPARENT and frosts it: an
	// NSVisualEffectView blurs whatever is behind the window (frosted glass).
	// Unlike black/matrix it is intentionally non-opaque — the desktop shows
	// through, blurred — so it trades the no-bleed-through guarantee for the
	// look. Input is still fully blocked (CGEventTap); only the visuals differ.
	OverlayStyleGlass = "glass"
	// OverlayStyleNone is the odd one out: it is NOT a look at all. It turns
	// dndmode into a thin /usr/bin/caffeinate(8) wrapper — NO Focus/DND, NO
	// keyboard/trackpad blocking (so no Accessibility permission is required),
	// and NO overlay window on any display. The only thing it does is hold a
	// system-awake assertion for as long as dndmode runs. Useful when the user
	// just wants to keep the machine awake for a background agent without
	// locking the screen. Exit is via Ctrl-C / SIGTERM only (there is no hotkey
	// in this mode because there is no event tap to observe one).
	OverlayStyleNone = "none"

	// configDirPerm is 0o700 — owner read/write/execute only (
	// mitigation: world cannot read user config).
	configDirPerm fs.FileMode = 0o700
	// configFilePerm is 0o600 — owner read/write only.
	configFilePerm fs.FileMode = 0o600
)

// Config is the v1 dndmode configuration schema. Add fields cautiously —
// forward-compat trojan keys are rejected by yaml.Strict().
type Config struct {
	Hotkey string `yaml:"hotkey"`
	// OverlayStyle selects the overlay look. Absent/empty => "black" (v1
	// default, via NormalizeOverlayStyle); the only valid non-empty values are
	// "black", "matrix", "glass" and "none" ("none" = caffeinate-only mode,
	// no overlay/DND/input-block — see OverlayStyleNone). The VALUE is validated by the caller
	// (main.go via ValidateOverlayStyle), NOT by yaml.Strict() — Strict only
	// guards unknown KEYS, so a known key with a junk value parses fine (QUICK-gh8).
	OverlayStyle string `yaml:"overlay_style"`
	// AllowDisplaySleep has INVERTED polarity: the Go zero value false
	// (default / key absent) keeps the display awake via the IOPMAssertion
	// type kIOPMAssertPreventUserIdleDisplaySleep; true restores the legacy
	// kIOPMAssertPreventUserIdleSystemSleep behavior (display may idle-off).
	// Parsed automatically by yaml.Strict() in Load() — no Load() body change.
	AllowDisplaySleep bool `yaml:"allow_display_sleep"`
}

// NormalizeOverlayStyle is the single source of the empty=>black rule: it
// returns OverlayStyleBlack when s == "" (a fresh config omits overlay_style)
// and returns s unchanged otherwise. Callers normalize once and thread the
// result downstream (main.go -> NewController).
func NormalizeOverlayStyle(s string) string {
	if s == "" {
		return OverlayStyleBlack
	}
	return s
}

// ValidateOverlayStyle accepts "" (treated as black), "black", "matrix",
// "glass" and "none"; anything else returns a non-nil error whose message is
// suitable for embedding in main.go's stderr template. yaml.Strict() cannot
// catch a bad VALUE (only unknown keys), so this is the real gate before any
// window is created (T-gh8-01). "none" is accepted here but routes to the
// caffeinate-only path in main.go — it never reaches the overlay controller.
func ValidateOverlayStyle(s string) error {
	switch s {
	case "", OverlayStyleBlack, OverlayStyleMatrix, OverlayStyleGlass, OverlayStyleNone:
		return nil
	default:
		return fmt.Errorf("unknown overlay_style %q (valid: black, matrix, glass, none)", s)
	}
}

// Loader reads a single YAML file at a fixed path. NewLoader does not touch
// disk; only Load() performs IO. Loader is single-use semantically; calling
// Load() multiple times will re-read the file each time, but this is NOT a
// hot-reload mechanism: production callers invoke Load() once at
// PreFlight only.
type Loader struct {
	path string
}

// NewLoader constructs a Loader for the given absolute path. The path is
// not validated until Load() is called.
func NewLoader(path string) *Loader {
	return &Loader{path: path}
}

// Path returns the configured path (for diagnostics / banner output).
func (l *Loader) Path() string { return l.path }

// Load returns the parsed config. If the file does not exist, it writes a
// default config to disk (creating parent dirs as needed) and returns the
// default with created=true. On YAML syntax error, returns a wrapped error
// whose message contains the goccy-formatted line:col + source snippet
//.
func (l *Loader) Load() (Config, bool, error) {
	raw, err := os.ReadFile(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		def := Config{Hotkey: DefaultHotkey}
		if werr := writeDefault(l.path, def); werr != nil {
			return Config{}, false, fmt.Errorf("write default config: %w", werr)
		}
		return def, true, nil
	case err != nil:
		return Config{}, false, fmt.Errorf("read config %s: %w", l.path, err)
	}

	var cfg Config
	// yaml.Strict() rejects unknown YAML keys (mitigation:
	// forward-compat trojan keys cannot smuggle behavior into v1).
	if perr := yaml.UnmarshalWithOptions(raw, &cfg, yaml.Strict()); perr != nil {
		// goccy pretty errors with line:col + source snippet.
		// color=false in v1 (P1.6 — TTY detection deferred to Phase 6).
		pretty := yaml.FormatError(perr, false /*colored*/, true /*inclSource*/)
		return Config{}, false, fmt.Errorf("parse config %s:\n%s", l.path, pretty)
	}
	return cfg, false, nil
}

// writeDefault creates the parent directory (0o700) and writes the default
// config via atomic tmp+rename (protects against concurrent dndmode
// starts; the loser of the rename race still ends up with a valid file).
//
// The tmp file name is generated via os.CreateTemp, which guarantees a
// per-call unique suffix even when multiple goroutines (or two processes
// with the same PID after fork) race on the same path. os.CreateTemp also
// opens the file with 0o600 perms by default, so the published file
// inherits the correct mode through Rename. macOS APFS makes the final
// rename atomic, so readers always observe a fully-written file.
func writeDefault(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirPerm); err != nil {
		return fmt.Errorf("mkdir parent %s: %w", dir, err)
	}
	// V1 schema is a single string field, so we hand-format the YAML rather
	// than calling yaml.Marshal. This keeps writeDefault free of a defensive
	// "marshal error" branch that is unreachable for Config{Hotkey: string}
	// and shrinks the surface that has to be tested. yaml.Strict() in Load
	// re-parses our output round-trip, so any drift would surface there.
	body := []byte(fmt.Sprintf("hotkey: %s\n", cfg.Hotkey))
	base := filepath.Base(path)
	tmpFile, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmp := tmpFile.Name()
	if _, werr := tmpFile.Write(body); werr != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp %s: %w", tmp, werr)
	}
	// Ignore Close error: tmpFile was opened write-only, all bytes are
	// already in the kernel buffer; subsequent Rename will succeed even if
	// Close reports a stale-FD style error. Keeping the non-fatal close
	// keeps the hot path linear and the function easy to reason about.
	_ = tmpFile.Close()
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, rerr)
	}
	return nil
}
