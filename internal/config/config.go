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
	// Mute is a *bool so an ABSENT key can default to TRUE: the Go zero value
	// of a plain bool would force default-false (or an inverted key name like
	// AllowDisplaySleep). nil => true via NormalizeMute, an explicit
	// `mute: false` => false. Default-true mutes system audio for the session
	// (saved/restored) so notification sounds stay silent without touching
	// Focus/DND — see the package-level rationale and NormalizeMute.
	Mute *bool `yaml:"mute"`
	// Focus default false matches the Go zero value (plain bool). Focus/DND is
	// now OPT-IN: enabling it runs the Shortcuts bootstrap + `dndmode-on`/`-off`,
	// which syncs across the user's Apple devices via iCloud. The audio mute
	// above replaces Focus's only local contribution (silencing sounds).
	Focus bool `yaml:"focus"`
	// Debug default false makes dndmode SILENT: it emits NOTHING to stdout or
	// stderr (no banners, no diagnostics, no slog logging) and communicates
	// outcome only through the process exit code. `debug: true` un-silences the
	// full console output. Rationale: with overlay_style `none` or `glass` the
	// terminal stays visible while dndmode is active, so the startup banner would
	// otherwise leak the unlock hotkey to a bystander — the security stance
	// is "reveal nothing" unless the operator explicitly opts into
	// debugging. The --debug CLI flag is the per-run equivalent; either source
	// enables output. Absent key => false via the Go zero value; yaml.Strict()
	// accepts it now that it is a declared field.
	Debug bool `yaml:"debug"`
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

// NormalizeMute is the single source of the nil=>true rule for the mute
// toggle, mirroring NormalizeOverlayStyle: a freshly-created config omits the
// `mute` key (Config.Mute == nil), which must default to TRUE (mute system
// audio for the session). An explicit `mute: false` yields a non-nil *false
// and disables muting. Callers normalize once and thread the bool downstream.
func NormalizeMute(m *bool) bool {
	if m == nil {
		return true
	}
	return *m
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

// defaultConfigTemplate is the fully-commented config.yml written on first
// run. It documents EVERY config field with its purpose, default,
// and accepted values so the user can self-serve without opening the README.
//
// Only `hotkey` is an ACTIVE key; every other field is shown commented-out at
// its default value. This is load-bearing, not cosmetic: an absent key is
// what carries the documented default (mute nil => true via NormalizeMute,
// focus false, overlay_style "" => black, allow_display_sleep/debug false), so
// uncommenting a line only ever *overrides* a default rather than re-stating
// it. It also keeps the yaml.Strict() round-trip in Load() parsing the written
// file as hotkey-only (comments are ignored by the parser). The single %s is
// the hotkey value (DefaultHotkey unless a caller overrides it).
//
// NOTE: no literal '%' may appear below except the one %s — the template is
// fed through fmt.Sprintf. `timer` is intentionally absent: it is a per-run
// --timer flag only, never a config key.
const defaultConfigTemplate = `# dndmode configuration
# Location: ~/.config/dndmode/config.yml  (auto-created on first run)
#
# Every field except 'hotkey' is OPTIONAL. Uncomment a line and change its
# value to override the default shown next to it. Unknown keys are REJECTED
# (strict parsing): a typo aborts startup with an error pointing at the line.
# Most fields also have a per-run CLI flag that overrides the file for that
# launch only.

# --- hotkey (REQUIRED) -------------------------------------------------------
# Key combination that unlocks and exits the locked state.
# Grammar: "<mod>+<mod>+...+<key>" — one or more modifiers plus exactly one key.
#   Modifiers (case-insensitive): ctrl, option, cmd, shift, fn
#   Keys: a-z, 0-9, f1-f12, space, return (alias enter), tab, escape (alias
#         esc), delete, forwarddelete, left, right, up, down,
#         and the punctuation - = [ ] ; ' , . / \ backtick
# Matched by PHYSICAL key position, so RU / AZERTY layouts behave identically.
# Modifier-only combinations are rejected (you must include one real key).
hotkey: %s

# --- overlay_style -----------------------------------------------------------
# Look of the full-screen shield that covers every attached display.
#   black  : opaque black shield (default). Nothing bleeds through.
#   matrix : animated green "digital rain" over the black shield (cosmetic
#            only; every blocking guarantee is identical to black).
#   glass  : TRANSPARENT frosted glass — the blurred desktop shows through.
#            Trades the no-bleed-through guarantee for the look; keyboard and
#            trackpad are still fully blocked.
#   none   : awake-only mode. NO overlay, NO input blocking, NO Focus, NO audio
#            mute — dndmode just holds the machine awake (like caffeinate).
#            Needs no Accessibility permission; exit with Ctrl-C only (there is
#            no hotkey because there is no event tap to observe it).
# Per-run override: --style <value>
# overlay_style: black

# --- allow_display_sleep -----------------------------------------------------
# INVERTED toggle controlling the DISPLAY (the system stays awake either way).
#   false : keep the display awake too (default).
#   true  : let the display dim / sleep while background work keeps running —
#           saves the panel when you only need the machine, not the screen.
# allow_display_sleep: false

# --- mute --------------------------------------------------------------------
# System audio muting for the session.
#   true  : mute on start, restore the prior volume on exit (default). Audio
#           already muted before start is left muted — the session never
#           unmutes what it did not mute.
#   false : leave the volume untouched.
# Ignored entirely in overlay_style 'none'. Per-run override: --mute=true|false
# mute: true

# --- focus -------------------------------------------------------------------
# Do Not Disturb Focus (opt-in).
#   false : leave Focus untouched (default).
#   true  : toggle the 'dndmode-on' / 'dndmode-off' Shortcuts, which sync DND
#           across your Apple devices via iCloud. Those two Shortcuts must
#           already exist (see README "First-run setup") or startup aborts with
#           exit code 6.
# Ignored entirely in overlay_style 'none'. Per-run override: --focus=true|false
# focus: false

# --- debug -------------------------------------------------------------------
# Console output gate.
#   false : SILENT (default). Nothing is printed to stdout / stderr; outcome is
#           reported through the exit code only. This is a security default —
#           in 'none' / 'glass' mode the terminal stays visible, so a startup
#           banner would otherwise leak the unlock hotkey to a bystander.
#   true  : un-silence the full startup / cleanup banners and debug logging.
# Per-run equivalent: the --debug flag (either source enables output).
# debug: false
`

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
	// We hand-format the YAML from a documented template rather than calling
	// yaml.Marshal: Marshal would drop the comments (the whole point of the
	// generated file is the inline field documentation) and would emit every
	// zero-value key uncommented, which would flip the absent-key defaults
	// (mute, focus, ...). Only `hotkey` is interpolated; all other fields stay
	// commented so their defaults come from key-absence. yaml.Strict() in Load
	// re-parses our output round-trip, so any drift would surface there.
	body := fmt.Appendf(nil, defaultConfigTemplate, cfg.Hotkey)
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
