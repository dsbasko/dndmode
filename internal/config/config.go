//go:build darwin

// Package config loads and writes the dndmode YAML configuration. The
// config schema in v1 is intentionally minimal (the unlock secret plus a
// handful of look/behavior toggles); migration to nested/versioned schema is
// deferred (the design notes).
//
// The unlock secret has three storable forms: `unlock_code` (a
// whitespace-separated sequence of steps), the deprecated
// single-combination `hotkey`, and the `unlock_salt` + `unlock_hash` pair
// written by --set-password. They are resolved into one matcher.Verifier by
// ResolveUnlockCode, which is the single source of truth for the precedence
// table — setting more than one of them is a startup error.
//
// Hot-reload is NOT supported: Load() is invoked exactly once at
// PreFlight. Loader has no Watch/Reload/Subscribe methods by design.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
	"github.com/goccy/go-yaml"
)

const (
	// DefaultHotkey is the legacy name of the value written to a
	// freshly-created config.yml. Kept as the historical spelling; the key it
	// is written under is now `unlock_code` (see DefaultUnlockCode).
	DefaultHotkey = "Ctrl+Option+Cmd+X"

	// DefaultUnlockCode is the unlock code written to a freshly-created
	// config.yml. It is the historical single-combination hotkey, i.e. an
	// unlock code of length 1 — deliberately kept so `brew upgrade` does not
	// change the muscle memory of existing users. It is WEAK by design
	// (IsWeakUnlockCode reports true for it), which is exactly why a user who
	// never opened the file still gets a warning under --debug.
	DefaultUnlockCode = DefaultHotkey

	// DefaultActivateHotkey is the combination `dndmode --watch` waits for
	// when activate_hotkey is absent. It mirrors the shape of
	// DefaultUnlockCode on purpose — same three modifiers, different letter
	// (D for dndmode) — so the two are easy to tell apart in muscle memory
	// while neither collides with a macOS system shortcut.
	//
	// Unlike the unlock code this value is NOT a secret. It is printed at
	// startup, documented in the README, and a bystander who presses it can
	// raise the shield on an unattended machine — exactly the exposure
	// Ctrl+Cmd+Q already carries. What they cannot do is lower it.
	DefaultActivateHotkey = "Ctrl+Option+Cmd+D"

	// MinUnlockSteps is the shortest MULTI-step unlock code accepted. Codes of
	// 2-3 steps are rejected outright: the sliding-window match makes every
	// keypress a fresh attempt (de Bruijn-sequence attack), so a 3-step code
	// falls in ~8 minutes to an automated 100/sec typist while still creating
	// the illusion of a passphrase. A single step is a separate, legacy case
	// (see ValidateUnlockCode) and must carry at least one modifier.
	MinUnlockSteps = 4

	// WeakUnlockSteps is the recommended length threshold: anything shorter is
	// accepted but reported by IsWeakUnlockCode, which main.go surfaces as a
	// --debug-only warning. At an alphabet of ~36 a 6-step code needs ~2.2e9
	// keypresses to exhaust — 250 days at 100/sec — while 4 steps fall in
	// under 5 hours.
	WeakUnlockSteps = 6

	// UnlockSourceCode / UnlockSourceHotkey / UnlockSourceHash name the config
	// key an unlock code was resolved from. Returned by ResolveUnlockCode so
	// callers can phrase diagnostics in terms of the key the user actually
	// wrote. UnlockSourceHash names the salted-digest pair written by
	// --set-password: the key that carries the secret is unlock_hash, with
	// unlock_salt as its inseparable other half.
	UnlockSourceCode   = "unlock_code"
	UnlockSourceHotkey = "hotkey"
	UnlockSourceHash   = "unlock_hash"

	// OverlayStyleBlack is the v1 default look: a plain opaque-black shield.
	// An absent/empty overlay_style normalizes to this (NormalizeOverlayStyle).
	OverlayStyleBlack = "black"
	// OverlayStyleMatrix renders animated green digital rain over the opaque
	// black shield (cosmetic only; every window guarantee is unchanged).
	OverlayStyleMatrix = "matrix"
	// OverlayStyleTerminal renders a scrolling stream of syntax-highlighted
	// pseudo-source over the opaque black shield: lines type themselves out
	// behind a blinking caret, then jump-scroll up as new lines arrive. Like
	// matrix it is cosmetic only — setOpaque:YES, pure #000000 fill, every
	// blocking guarantee (HID event tap, shield level, no bleed-through) is
	// byte-for-byte identical to black. Ambient only: it never reacts to input.
	OverlayStyleTerminal = "terminal"
	// OverlayStyleDVD renders a stylized "DVD VIDEO" logo bouncing edge-to-edge
	// over the opaque black shield — the old-DVD-player screensaver. The logo
	// drifts diagonally, reflects off every edge, cycles to the next neon color on
	// each bounce, and flashes white when it lands exactly in a corner. Like
	// matrix/terminal it is cosmetic only — setOpaque:YES, pure #000000 fill, every
	// blocking guarantee is byte-for-byte identical to black. One bouncing logo per
	// display. Ambient only: it never reacts to input.
	OverlayStyleDVD = "dvd"
	// OverlayStyleGlass makes the shield TRANSPARENT and frosts it: an
	// NSVisualEffectView blurs whatever is behind the window (frosted glass).
	// Unlike black/matrix/terminal it is intentionally non-opaque — the desktop shows
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

	// TerminalLangGo / Python / TypeScript / Rust / Yopta are the languages the
	// `terminal` overlay style can render, selected by the --style terminal:<lang>
	// flag suffix (mirrors --style glass:N). Each maps to its own compiled-in
	// source corpus with language-appropriate syntax highlighting. A bare
	// `terminal` (no suffix) defaults to Go.
	//
	// TerminalLangYopta is YoptaScript (yopta.space) — a joke Russian dialect of
	// JavaScript, and the only corpus that is not ASCII. Its short spelling `ys`
	// is deliberate: the language's own file extension is `.yopta`, but the flag
	// suffix has to be typed at 3am with the machine about to be shielded.
	TerminalLangGo         = "go"
	TerminalLangPython     = "python"
	TerminalLangTypeScript = "typescript"
	TerminalLangRust       = "rust"
	TerminalLangYopta      = "ys"

	// legacyTerminalLangYopta is the spelling YoptaScript shipped under before it
	// became `ys` — it read as an abbreviation of the language's own name rather
	// than of YoptaScript, which is what the rename fixes.
	//
	// It stays ACCEPTED, and that is a safety decision rather than politeness.
	// terminal_language is validated at startup, so rejecting the old spelling
	// would make `dndmode` refuse to start on a config that worked yesterday —
	// exit code and no shield — over the name of a cosmetic corpus. Accepting it
	// costs one branch in NormalizeTerminalLanguage; refusing it costs the user
	// their lock. It is deliberately NOT advertised: defaultConfigTemplate,
	// --style's usage line and the validation error all name `ys` only, and
	// MigrateFile rewrites the old spelling out of an existing config the first
	// time it runs (see legacyValues).
	//
	// Nothing downstream may branch on it. Normalization folds it into
	// TerminalLangYopta at the single funnel main.go already calls, so the C side
	// (term_lang_from_string) knows one spelling; the pin is
	// TestNormalizeTerminalLanguage_NeverEmitsALegacySpelling.
	legacyTerminalLangYopta = "yc"
	// DefaultTerminalLanguage is the language a bare `terminal` renders (mirrors
	// DefaultGlassBlur for the glass param).
	DefaultTerminalLanguage = TerminalLangGo

	// DefaultGlassBlur is the CIGaussianBlur radius (in points) used for
	// overlay_style "glass" when glass_blur is absent and no --style glass:N
	// override is given. ~16 keeps large shapes recognizable while text stays
	// unreadable. Mirrors kGlassBlurRadius in window_darwin.m.
	DefaultGlassBlur = 16.0
	// maxGlassBlur caps glass_blur at a sane upper bound: beyond this the whole
	// desktop is an undifferentiated wash and CIGaussianBlur only gets slower.
	maxGlassBlur = 500.0

	// configDirPerm is 0o700 — owner read/write/execute only (
	// mitigation: world cannot read user config).
	configDirPerm fs.FileMode = 0o700
)

// Config is the v1 dndmode configuration schema. Add fields cautiously —
// forward-compat trojan keys are rejected by yaml.Strict().
type Config struct {
	// Hotkey is the DEPRECATED single-combination unlock key ("ctrl+option+cmd+x").
	// It still works — a legacy hotkey is simply an unlock code of length 1 —
	// but it is no longer written to fresh configs and setting it TOGETHER with
	// UnlockCode is a startup error (the unlock secret must be unambiguous).
	// Resolved through ResolveUnlockCode, never read directly by callers.
	Hotkey string `yaml:"hotkey"`
	// UnlockCode is the unlock secret: a whitespace-separated sequence of at
	// most hotkey.MaxSteps steps ("s w o r d f i s h", "ctrl+s w cmd+z"). The
	// grammar is a SUPERSET of Hotkey's, so "Ctrl+Option+Cmd+X" is a valid
	// unlock code of length 1. Like OverlayStyle the VALUE is not validated by
	// yaml.Strict() (which only guards unknown KEYS) — ResolveUnlockCode +
	// ValidateUnlockCode are the real gate, called from main.go before any
	// window is created.
	UnlockCode string `yaml:"unlock_code"`
	// UnlockSalt is the base64 (StdEncoding) per-config random salt of the
	// hashed unlock secret written by --set-password, matcher.SaltLen bytes
	// when decoded. It carries no secret on its own and is meaningless without
	// UnlockHash: half a pair is a resolve error, not a fallback.
	// Like UnlockCode the VALUE is not validated by yaml.Strict() (which only
	// guards unknown KEYS, so junk base64 parses fine) — ResolveUnlockCode is
	// the real gate, called from main.go before any window is created.
	UnlockSalt string `yaml:"unlock_salt"`
	// UnlockHash is the base64 (StdEncoding) SHA-256 digest of the unlock
	// sequence, 32 bytes when decoded — see matcher.HashSteps for the preimage.
	// Set together with UnlockSalt by --set-password so the plaintext secret
	// never reaches the config file. The stored digest deliberately does NOT
	// record the sequence LENGTH, so nothing downstream can report or leak it.
	// The VALUE is not validated by yaml.Strict() — ResolveUnlockCode decodes
	// and length-checks both halves, and its errors name the KEY without ever
	// echoing the value.
	UnlockHash string `yaml:"unlock_hash"`
	// ActivateHotkey is the combination `--watch` waits for while idle: a
	// SINGLE step ("Ctrl+Option+Cmd+D"), never a sequence, because Carbon's
	// RegisterEventHotKey matches "modifiers + one key" and nothing else.
	//
	// Three states, and the empty one is not the same as absent: an ABSENT
	// key means DefaultActivateHotkey (so `brew upgrade` gives existing
	// configs a working watch mode), while an explicitly EMPTY value means
	// "watch mode is off" and makes --watch refuse to start. That asymmetry
	// is what gives a user a way to disable the mode without deleting the
	// documentation around it.
	//
	// "Empty" means the empty STRING — `activate_hotkey: ""`. A bare
	// `activate_hotkey:` with nothing after it is YAML null, which unmarshals
	// to a nil pointer and is therefore indistinguishable from an absent key,
	// i.e. it selects the default. The template says so at the line it
	// suggests; there is no way to tell the two apart at this layer, and
	// inventing one would mean rejecting a config shape YAML considers
	// perfectly ordinary.
	//
	// Ignored entirely without --watch. Like OverlayStyle the VALUE is not
	// validated by yaml.Strict() (which only guards unknown KEYS) —
	// ResolveActivateHotkey is the real gate.
	//
	// A *string for the same reason Mute is a *bool: a plain string cannot
	// tell an absent key from an empty one, and here those two mean opposite
	// things (default vs. disabled).
	ActivateHotkey *string `yaml:"activate_hotkey"`
	// OverlayStyle selects the overlay look. Absent/empty => "black" (v1
	// default, via NormalizeOverlayStyle); the only valid non-empty values are
	// "black", "matrix", "terminal", "dvd", "glass" and "none" ("none" = caffeinate-only
	// mode, no overlay/DND/input-block — see OverlayStyleNone). The VALUE is validated by the caller
	// (main.go via ValidateOverlayStyle), NOT by yaml.Strict() — Strict only
	// guards unknown KEYS, so a known key with a junk value parses fine (QUICK-gh8).
	OverlayStyle string `yaml:"overlay_style"`
	// GlassBlur is the CIGaussianBlur radius (in points) for overlay_style
	// "glass". It is a *float64 so an ABSENT key defaults to DefaultGlassBlur via
	// NormalizeGlassBlur (mirrors the Mute *bool nil-default pattern). Only
	// meaningful for glass; ignored for black/matrix/terminal/none. Per-run override: the
	// --style glass:N flag suffix (main.go). Validated by ValidateGlassBlur.
	GlassBlur *float64 `yaml:"glass_blur"`
	// TerminalLanguage selects the source language for overlay_style "terminal":
	// "go" (default / absent), "python", "typescript", "rust" or "ys". Only meaningful
	// for terminal; ignored for every other style. Per-run override: the
	// --style terminal:<lang> flag suffix (main.go) WINS over this. A plain string
	// so an ABSENT/empty key defaults to Go via NormalizeTerminalLanguage;
	// validated by ValidateTerminalLanguage (mirrors the GlassBlur gate).
	TerminalLanguage string `yaml:"terminal_language"`
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
	// otherwise leak the unlock code to a bystander — the security stance
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
// "terminal", "dvd", "glass" and "none"; anything else returns a non-nil error whose
// message is suitable for embedding in main.go's stderr template. yaml.Strict()
// cannot catch a bad VALUE (only unknown keys), so this is the real gate before
// any window is created (T-gh8-01). "none" is accepted here but routes to the
// caffeinate-only path in main.go — it never reaches the overlay controller.
func ValidateOverlayStyle(s string) error {
	switch s {
	case "", OverlayStyleBlack, OverlayStyleMatrix, OverlayStyleTerminal, OverlayStyleDVD, OverlayStyleGlass, OverlayStyleNone:
		return nil
	default:
		return fmt.Errorf("unknown overlay_style %q (valid: black, matrix, terminal, dvd, glass, none)", s)
	}
}

// ValidateUnlockCode gates the LENGTH and shape of an already-parsed unlock
// code, mirroring ValidateOverlayStyle: yaml.Strict() cannot catch a bad VALUE,
// so this is the real gate before any window is created.
//
//   - 0 steps        => error (nothing to match)
//   - 1 step         => accepted ONLY with at least one modifier. This is the
//     legacy `hotkey` semantics; a bare key would unlock on a
//     single keypress, i.e. on the first thing a bystander types.
//   - 2-3 steps      => error. Too weak to be worth the illusion of a
//     passphrase (see MinUnlockSteps).
//   - MinUnlockSteps..hotkey.MaxSteps => accepted.
//
// The upper bound is NOT re-checked here: hotkey.ParseSequence owns it and
// duplicating the limit would create a second source of truth.
//
// Error messages never echo a step — a diagnostic must not leak the secret.
func ValidateUnlockCode(steps []hotkey.Spec) error {
	switch {
	case len(steps) == 0:
		return fmt.Errorf("unlock code is empty: specify at least one step")
	case len(steps) == 1:
		if steps[0].Modifiers == 0 {
			return fmt.Errorf(
				"a 1-step unlock code must carry at least one modifier (e.g. Ctrl+Option+Cmd+X); "+
					"a bare key would unlock on a single keypress — use %d or more steps instead",
				MinUnlockSteps)
		}
		return nil
	case len(steps) < MinUnlockSteps:
		return fmt.Errorf(
			"unlock code of %d steps is too short: every keypress is a fresh match attempt, "+
				"so a %d-step code is exhausted in minutes — use at least %d steps (%d or more recommended)",
			len(steps), len(steps), MinUnlockSteps, WeakUnlockSteps)
	default:
		return nil
	}
}

// Sentinel errors for the activation combination. Unlike every diagnostic
// around the unlock secret, these MAY be wrapped with the offending value by
// callers: activate_hotkey is public by construction (see
// DefaultActivateHotkey), so echoing it costs nothing and saves the user a
// trip to the config file.
var (
	// ErrActivateHotkeyDisabled reports an explicitly empty activate_hotkey:
	// the user turned watch mode off. Distinct from a parse failure because
	// it is a deliberate configuration, not a mistake — --watch refuses to
	// start and says so plainly, while a plain `dndmode` run is unaffected.
	ErrActivateHotkeyDisabled = errors.New("activate_hotkey is empty: watch mode is disabled")

	// ErrActivateHotkeyCollision reports that the activation combination
	// would ALSO satisfy the unlock secret. That is a hole rather than a
	// quirk: the combination that raises the shield is published in the
	// README, so a shield raised by it could be lowered by anyone who read
	// the docs.
	ErrActivateHotkeyCollision = errors.New("activate_hotkey is identical to the unlock code: the combination that raises the shield would also lower it")
)

// ResolveActivateHotkey turns the activate_hotkey key into the single step
// `--watch` registers with Carbon, applying the absent/empty asymmetry
// described on Config.ActivateHotkey: absent => DefaultActivateHotkey,
// explicitly empty => ErrActivateHotkeyDisabled.
//
// unlock is the verifier ResolveUnlockCode already produced, and passing it
// is what makes the collision check possible at all. The check runs through
// Verifier.Match rather than by comparing steps, which matters twice over.
// It works for BOTH storable forms without asking which one is in play —
// the project rule is that nothing below ResolveUnlockCode branches on the
// shape of the secret — and for the hashed form it detects the collision
// without the hash ever being reversed: a one-step tail built from the
// PUBLIC activation combination either hashes to the stored digest or does
// not.
//
// In practice a hashed secret cannot collide, since --set-password enforces
// MinUnlockSteps. The check is kept anyway because that is a property of a
// sibling code path rather than of this one, and a fail-deadly guard that
// costs one Match call should not depend on a distant invariant holding.
func ResolveActivateHotkey(cfg *Config, unlock matcher.Verifier) (hotkey.Spec, error) {
	raw := DefaultActivateHotkey
	if cfg.ActivateHotkey != nil {
		raw = *cfg.ActivateHotkey
	}
	if strings.TrimSpace(raw) == "" {
		return hotkey.Spec{}, ErrActivateHotkeyDisabled
	}

	// Parse, not ParseStep, and the distinction is load-bearing: Parse is the
	// single-combination grammar and it already REFUSES a modifier-less
	// combination. That refusal is what keeps a bare `d` — which would raise
	// the shield on every press of that key in every application — out of
	// watch mode, so this function deliberately does not re-check it rather
	// than carry an unreachable second opinion. The dependency is pinned by
	// TestParse_RejectsModifierless_WatchModeDependsOnIt.
	spec, err := hotkey.Parse(raw)
	if err != nil {
		return hotkey.Spec{}, fmt.Errorf("activate_hotkey %q is not a valid combination: %w", raw, err)
	}
	if activateCollidesWithUnlock(spec, unlock) {
		return hotkey.Spec{}, ErrActivateHotkeyCollision
	}
	return spec, nil
}

// activateCollidesWithUnlock reports whether a single press of the
// activation combination would satisfy the unlock verifier.
//
// The MinLen guard is an early-out, not the decision: a verifier that needs
// more than one step cannot be satisfied by a one-event tail, so there is
// nothing to ask it. Everything else is left to Match, which is the only
// component entitled to know what the secret looks like.
//
// A nil verifier collides with nothing — the empty-verifier case is caught
// upstream by ResolveUnlockCode and by the MaxLen()==0 guard in eventtap,
// and duplicating that judgement here would only add a second opinion.
func activateCollidesWithUnlock(spec hotkey.Spec, unlock matcher.Verifier) bool {
	if unlock == nil || unlock.MinLen() != 1 {
		return false
	}
	return unlock.Match([]matcher.KeyEvent{{
		Modifiers: spec.Modifiers,
		KeyCode:   spec.KeyCode,
	}})
}

// IsWeakUnlockCode reports whether an accepted unlock code is shorter than the
// recommended WeakUnlockSteps. main.go surfaces it as a --debug-only warning
// (never on the silent path — a warning is still a hint about the secret).
//
// Length 1 counts as weak DELIBERATELY, even though it is the shipped default:
// the single-combination hotkey is exactly the shape whose brute-forceability
// motivated unlock codes, so "it came from the template" must not exempt it.
//
// This does NOT reach a user who installed via brew and never passed --debug —
// nothing does, by design: every console write is gated (see gatedWriter in
// main.go) so a visible terminal under overlay_style none/glass cannot leak
// hints about the secret. The warning is for the operator who is already
// looking, not a first-run nag; the README carries the message for everyone
// else.
func IsWeakUnlockCode(steps []hotkey.Spec) bool {
	return len(steps) < WeakUnlockSteps
}

// decodeUnlockDigest turns the stored base64 `unlock_salt` / `unlock_hash`
// pair into a *matcher.Digest. It is the only place raw config base64
// reaches the matcher package.
//
// It lives here rather than beside the fields it decodes because this is the
// file that holds its only caller: exported one task earlier it would have
// been `unused` to the linter, and — config_test.go being a black-box
// package — untestable except through ResolveUnlockCode anyway, which is
// exactly how its tests are written.
//
// Every error names the KEY and never the VALUE. A base64 blob is not the
// secret itself, but the no-echo rule is enforced uniformly across this
// package precisely so no future reader has to re-derive which diagnostics
// are safe to interpolate into; base64.CorruptInputError reports a byte
// offset only, and matcher.NewDigest reports the expected and actual widths
// only.
func decodeUnlockDigest(saltB64, hashB64 string) (*matcher.Digest, error) {
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("unlock_salt is not valid base64: %w", err)
	}
	sum, err := base64.StdEncoding.DecodeString(hashB64)
	if err != nil {
		return nil, fmt.Errorf("unlock_hash is not valid base64: %w", err)
	}
	d, err := matcher.NewDigest(salt, sum)
	if err != nil {
		return nil, fmt.Errorf("invalid unlock_salt/unlock_hash: %w", err)
	}
	return d, nil
}

// ResolveUnlockCode is the single source of truth for the unlock-secret
// precedence table. It collapses the three storable forms of the secret into
// one matcher.Verifier so no branching on "which key was it" survives further
// down the call chain — a legacy hotkey is just a code of length 1, and a
// stored digest is just a verifier that happens not to know its own length.
//
//	only unlock_salt + unlock_hash : the --set-password form, a *matcher.Digest
//	only unlock_code               : primary plaintext path, a *matcher.Sequence
//	only hotkey                    : works, caller emits a deprecation warning under --debug
//	two sources or more            : ERROR — an ambiguous unlock secret is not resolvable
//	neither                        : ERROR
//	half of the salt/hash pair     : ERROR — half a pair carries no secret
//
// The half-pair check runs FIRST, ahead of the source count, because it is
// the more actionable diagnostic: a config with `unlock_code` plus a stray
// `unlock_salt` is a botched --set-password, and "you have half a pair" says
// what to do about it while "your secret is ambiguous" does not.
//
// The second return value is the config key the code came from
// (UnlockSourceCode / UnlockSourceHotkey / UnlockSourceHash), so callers can
// name it in diagnostics. It is empty when no single key was in play —
// nothing set, several set, or half a pair set — and it IS filled in on a
// decode/parse error, where one source was unambiguously in play and merely
// malformed.
//
// The third return value is the WEAK flag: true when the resolved code is
// shorter than the recommended WeakUnlockSteps. It is computed here, while
// the steps are still in hand, because they do not survive the return — a
// Verifier does not expose them (and must not), and re-parsing the config to
// recover them would put a second copy of the precedence table in the
// caller. For a digest source it is identically false by construction: a
// digest stores no length, so there is nothing to be weak about that this
// package could observe.
//
// Errors never contain the value of any key.
func ResolveUnlockCode(cfg *Config) (matcher.Verifier, string, bool, error) {
	code := strings.TrimSpace(cfg.UnlockCode)
	legacy := strings.TrimSpace(cfg.Hotkey)
	salt := strings.TrimSpace(cfg.UnlockSalt)
	sum := strings.TrimSpace(cfg.UnlockHash)

	if (salt == "") != (sum == "") {
		present, missing := UnlockSourceHash, "unlock_salt"
		if salt != "" {
			present, missing = "unlock_salt", UnlockSourceHash
		}
		return nil, "", false, fmt.Errorf(
			"config sets %s without %s; the two are one secret — "+
				"re-run `dndmode --set-password` to write both",
			present, missing)
	}
	hashed := salt != "" // sum != "" too, per the check above.

	// One call, not two: setUnlockKeys already answers "how many" by how long
	// its result is, and computing the same three booleans twice invites the
	// count and the names to drift apart.
	if keys := setUnlockKeys(hashed, code != "", legacy != ""); len(keys) > 1 {
		return nil, "", false, fmt.Errorf(
			"config sets more than one unlock secret (%s); "+
				"keep exactly one — the unlock secret must be unambiguous",
			strings.Join(keys, " and "))
	}

	switch {
	case hashed:
		d, err := decodeUnlockDigest(salt, sum)
		if err != nil {
			return nil, UnlockSourceHash, false, err
		}
		// Weak is false and not IsWeakUnlockCode(anything): the length the
		// digest commits to is inside the hash and deliberately absent from
		// disk, so no caller of this function can ever warn about it.
		return d, UnlockSourceHash, false, nil
	case code != "":
		steps, err := hotkey.ParseSequence(code)
		if err != nil {
			return nil, UnlockSourceCode, false, fmt.Errorf("invalid unlock_code: %w", err)
		}
		if verr := ValidateUnlockCode(steps); verr != nil {
			return nil, UnlockSourceCode, false, fmt.Errorf("invalid unlock_code: %w", verr)
		}
		return newMaskedSequence(steps), UnlockSourceCode, IsWeakUnlockCode(steps), nil
	case legacy != "":
		// Parse (not ParseStep): the legacy key keeps its legacy requirement of
		// at least one modifier, which is also what ValidateUnlockCode demands
		// of any 1-step code.
		spec, err := hotkey.Parse(legacy)
		if err != nil {
			return nil, UnlockSourceHotkey, false, fmt.Errorf("invalid hotkey: %w", err)
		}
		steps := []hotkey.Spec{spec}
		return newMaskedSequence(steps), UnlockSourceHotkey, IsWeakUnlockCode(steps), nil
	default:
		return nil, "", false, fmt.Errorf(
			"config sets none of unlock_code, unlock_hash or hotkey: add an unlock_code line " +
				"or run `dndmode --set-password` " +
				"(see the generated ~/.config/dndmode/config.yml for the grammar)")
	}
}

// newMaskedSequence builds the plaintext Verifier from steps whose modifiers
// have been AND'ed with matcher.UserIntentionalMask.
//
// The masking used to live in eventtap.installInternal, which was the place
// the Sequence was constructed; it moved here with the construction itself
// when that function started taking a Verifier. Losing it would be a silent
// lockout rather than a compile error: matcher.Sequence.MatchTail masks the
// EVENT side and then compares for exact equality, so an unmasked system bit
// on the CONFIGURED side makes the code unenterable. hotkey.ParseStep does
// not currently emit any bit outside the mask, which is exactly why this is
// defence in depth and has to be carried rather than dropped as redundant.
//
// A *Digest needs no counterpart: matcher.encodeStep masks inside the hash
// preimage, on both the recording and the matching side.
func newMaskedSequence(steps []hotkey.Spec) *matcher.Sequence {
	masked := make([]hotkey.Spec, len(steps))
	for i, st := range steps {
		masked[i] = hotkey.Spec{
			Modifiers: st.Modifiers & matcher.UserIntentionalMask,
			KeyCode:   st.KeyCode,
		}
	}
	return matcher.NewSequence(masked)
}

// setUnlockKeys names the unlock keys the config actually set, in precedence
// order, so the ambiguity error can tell the user which lines to look at.
// It returns KEY NAMES only — never a value.
func setUnlockKeys(hashed, coded, legacy bool) []string {
	var keys []string
	if hashed {
		keys = append(keys, UnlockSourceHash)
	}
	if coded {
		keys = append(keys, UnlockSourceCode)
	}
	if legacy {
		keys = append(keys, UnlockSourceHotkey)
	}
	return keys
}

// NormalizeTerminalLanguage maps "" => the default terminal language (Go),
// mirroring NormalizeOverlayStyle. A bare `--style terminal` (no :suffix) and an
// absent value both normalize here; callers thread the result downstream.
//
// It is ALSO the one place a legacy spelling is folded into its current one
// (`yc` => `ys`). Both sources of the value — the config key and the --style
// terminal:<lang> suffix — pass through here before anything else sees the
// string, which is what lets the rest of the program, C included, know exactly
// one spelling per language. Pin:
// TestNormalizeTerminalLanguage_NeverEmitsALegacySpelling.
func NormalizeTerminalLanguage(s string) string {
	switch s {
	case "":
		return DefaultTerminalLanguage
	case legacyTerminalLangYopta:
		return TerminalLangYopta
	default:
		return s
	}
}

// ValidateTerminalLanguage accepts "" (treated as the default, Go), the five
// supported languages and the deprecated `yc` spelling of YoptaScript; anything
// else returns a non-nil error suitable for main.go's stderr template. Gates the
// --style terminal:<lang> flag suffix.
//
// The error text lists the CURRENT spellings only. A user who reaches it typed
// something that is neither, and offering them a deprecated name to copy would
// hand out the one spelling this release is trying to retire. `yopta` stays
// rejected for the reason it always was: it is a second live spelling, not a
// legacy one, and accepting it would let the two drift.
func ValidateTerminalLanguage(s string) error {
	switch s {
	case "", TerminalLangGo, TerminalLangPython, TerminalLangTypeScript,
		TerminalLangRust, TerminalLangYopta, legacyTerminalLangYopta:
		return nil
	default:
		return fmt.Errorf("unknown terminal language %q (valid: go, python, typescript, rust, ys)", s)
	}
}

// NormalizeGlassBlur is the single source of the nil=>DefaultGlassBlur rule for
// the glass blur radius (mirrors NormalizeMute): a config that omits glass_blur
// (Config.GlassBlur == nil) defaults to DefaultGlassBlur; an explicit value is
// returned unchanged. Callers normalize once and thread the float downstream
// (main.go -> NewController -> cocoa_create_overlay_window).
func NormalizeGlassBlur(v *float64) float64 {
	if v == nil {
		return DefaultGlassBlur
	}
	return *v
}

// ValidateGlassBlur gates the glass blur radius: it must be finite, non-negative
// and no larger than maxGlassBlur. Applies to BOTH the config glass_blur value
// and the --style glass:N flag suffix. 0 is accepted (no blur, though that makes
// glass pointless); negative, NaN/Inf, or absurdly large values are rejected
// with a message suitable for main.go's stderr.
func ValidateGlassBlur(v float64) error {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return fmt.Errorf("glass blur radius must be a finite number")
	case v < 0:
		return fmt.Errorf("glass blur radius %g must be >= 0", v)
	case v > maxGlassBlur:
		return fmt.Errorf("glass blur radius %g exceeds max %g", v, maxGlassBlur)
	default:
		return nil
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
// whose message contains the goccy-formatted line:col location — and NOT the
// offending source line (see the FormatError call for why).
//
// It is LoadWithSource with the bytes dropped; a caller that has to prove later
// that config.yml is still the file it parsed must use that one instead.
func (l *Loader) Load() (Config, bool, error) {
	cfg, _, created, err := l.LoadWithSource()
	return cfg, created, err
}

// LoadWithSource is Load plus the EXACT bytes the returned Config was parsed
// from — on the first run, the bytes just written rather than a read-back of
// them.
//
// The two must come from one read, and that is the whole reason this method
// exists. A session fingerprints config.yml at startup and compares the
// fingerprint again under the publish lock before it publishes runtime.json, so
// that a --set-password committing mid-startup is caught instead of silently
// overridden (cmd/dndmode/publishlock.go). Taking that fingerprint from a
// SECOND read of the same path would defeat it: SaveUnlockHash publishes by
// atomic rename, so a rename landing between the two reads leaves the session
// holding a verifier parsed from the OLD file and a fingerprint taken from the
// NEW one. The later comparison then succeeds — nothing has changed since the
// fingerprint — and the shield goes up answering to a secret config.yml no
// longer names, which is the exact outcome the fingerprint was added to
// prevent, reached through the fingerprint itself.
//
// The bytes are returned rather than a digest so the hash function stays in one
// place, next to the re-check that has to agree with it.
func (l *Loader) LoadWithSource() (Config, []byte, bool, error) {
	raw, err := os.ReadFile(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		def := Config{UnlockCode: DefaultUnlockCode}
		body, werr := writeDefault(l.path, def)
		if werr != nil {
			return Config{}, nil, false, fmt.Errorf("write default config: %w", werr)
		}
		return def, body, true, nil
	case err != nil:
		return Config{}, nil, false, fmt.Errorf("read config %s: %w", l.path, err)
	}

	var cfg Config
	// yaml.Strict() rejects unknown YAML keys (mitigation:
	// forward-compat trojan keys cannot smuggle behavior into v1).
	if perr := yaml.UnmarshalWithOptions(raw, &cfg, yaml.Strict()); perr != nil {
		// goccy pretty errors, line:col ONLY — inclSource=false is a security
		// decision, not a formatting one. The source snippet quotes the lines
		// AROUND the error, and in this file the line above almost any error is
		// `unlock_code: <the secret>`, so a typo in an unrelated key would print
		// the whole unlock code to stderr. That is the same leak the parser
		// diagnostics avoid by never echoing a token (see hotkey.ParseStep), and
		// it is reached by the recovery path the README itself recommends
		// ("startup dies with a YAML error → run with --debug"). Redacting the
		// snippet instead was rejected: the value can sit on its own line under a
		// broken mapping, where no line-prefix rule can find it. [line:col] is
		// what actually locates the typo; the user has the file open anyway.
		// color=false in v1 (P1.6 — TTY detection deferred to Phase 6).
		pretty := yaml.FormatError(perr, false /*colored*/, false /*inclSource*/)
		return Config{}, nil, false, fmt.Errorf("parse config %s:\n%s", l.path, pretty)
	}
	return cfg, raw, false, nil
}

// defaultConfigTemplate is the fully-commented config.yml written on first
// run. It documents EVERY config field with its purpose, default,
// and accepted values so the user can self-serve without opening the README.
//
// Only `unlock_code` is an ACTIVE key; every other field is shown commented-out
// at its default value — with one deliberate exception: the `unlock_salt` /
// `unlock_hash` block is documented in prose with NO commented-out sample line.
// Those two keys have no meaningful default to show, their values are base64
// blobs nobody can type, and a `# unlock_salt: ...` line would read as an
// invitation to hand-edit a secret that only `--set-password` may write.
// This is load-bearing, not cosmetic: an absent key is
// what carries the documented default (mute nil => true via NormalizeMute,
// focus false, overlay_style "" => black, allow_display_sleep/debug false), so
// uncommenting a line only ever *overrides* a default rather than re-stating
// it. It also keeps the yaml.Strict() round-trip in Load() parsing the written
// file as unlock_code-only (comments are ignored by the parser) — which matters
// twice over here, because an ACTIVE `hotkey` line alongside `unlock_code`
// would be rejected by ResolveUnlockCode as an ambiguous secret. The single %s
// is the unlock code (DefaultUnlockCode unless a caller overrides it).
//
// NOTE: no literal '%' may appear below except the single %s — the template is
// fed through fmt.Sprintf. `timer` is intentionally absent: it is a per-run
// --timer flag only, never a config key.
const defaultConfigTemplate = `# dndmode configuration
# Location: ~/.config/dndmode/config.yml  (auto-created on first run)
#
# Every field except 'unlock_code' is OPTIONAL. Uncomment a line and change its
# value to override the default shown next to it. Unknown keys are REJECTED
# (strict parsing): a typo aborts startup with an error pointing at the line.
# Most fields also have a per-run CLI flag that overrides the file for that
# launch only.

# --- unlock_code (REQUIRED) --------------------------------------------------
# The secret that unlocks and exits the locked state. It is a SEQUENCE of
# steps typed one after another — a passphrase, not a single chord.
#
# Grammar: steps separated by spaces; each step is "(<mod>+)*<key>".
#   Modifiers (case-insensitive): ctrl, option, cmd, shift
#   Keys: a-z, 0-9, f1-f12, space, return (alias enter), tab, escape (alias
#         esc), delete, forwarddelete, left, right, up, down,
#         and the punctuation - = [ ] ; ' , . / \ backtick
#   Modifiers inside a step are OPTIONAL, so both 's' and 'ctrl+s' are steps.
#   A literal space is only a SEPARATOR; the space key itself is 'space'.
#   'fn' is still accepted in a step but carries NO meaning: macOS raises the
#   Fn bit for every F-key, arrow and Forward Delete on its own, so it cannot
#   express intent. Write 'up', not 'fn+up'.
#
# QUOTING: this is a YAML value, so if the code STARTS with one of - [ ] '
# or a backtick, wrap the whole value in double quotes — unquoted, YAML reads
# those as list/quote syntax and startup fails with a parse error, not with a
# dndmode message. Anywhere but the first character they are fine bare. Also
# note that ' #' (space then hash) starts a YAML comment and would silently
# truncate the rest of the code.
#   unlock_code: "- a b c d"           # leading punctuation: quote it
#
# Examples:
#   unlock_code: s w o r d f i s h     # a passphrase
#   unlock_code: ctrl+s w o r d cmd+z  # mixed
#   unlock_code: Ctrl+Option+Cmd+X     # a single chord = a code of length 1
#
# Length rules (dndmode matches the TAIL of everything typed, so every single
# keypress is a fresh attempt — short codes fall fast):
#   1 step      : allowed only WITH modifiers (the legacy hotkey shape). Weak.
#   2-3 steps   : REJECTED — too weak to be worth the illusion of a passphrase.
#   4-32 steps  : accepted. 6 or more is STRONGLY recommended: at 6 steps a
#                 brute force needs ~250 days at 100 keypresses/sec, at 4 it
#                 needs under 5 hours.
# Change the default below before you rely on it — it ships the same chord on
# every machine.
#
# Matched by PHYSICAL key position, not by the character produced: on a RU
# layout 'unlock_code: s w o r d' is typed with the keys ы ц о р в.
# Every step is matched EXACTLY: a modifier you happen to be holding (e.g. Cmd)
# breaks a step declared without it. CapsLock, NumPad and Fn bits are ignored,
# so CapsLock can never lock you out and 'up' or 'down' work as plain steps.
#
# AVOID f1-f12. With the macOS default "Use F1, F2, etc. keys as standard
# function keys" turned OFF, the F-row is delivered as a system media key, NOT
# as a key press — dndmode never sees it. A code containing 'f1' parses fine,
# starts with no warning, and then CANNOT BE TYPED while locked unless you hold
# Fn (or flip that switch in System Settings > Keyboard). The arrows and
# forwarddelete are NOT affected: those are real key presses that merely carry
# the Fn bit, which is stripped.
unlock_code: %s

# --- unlock_salt / unlock_hash (OPTIONAL, machine-written) -------------------
# The hashed form of the unlock secret. Neither key is present in a generated
# config, and neither is meant to be typed or edited by hand.
#
# 'dndmode --set-password' captures a new sequence from REAL keystrokes (twice,
# so a typo cannot be stored), then DELETES the plaintext unlock_code line (and
# a deprecated hotkey line, if there is one) and writes the pair where it was,
# with its own explanatory comment. After that the
# plaintext secret is no longer anywhere in this file: neither stored value
# spells the sequence out and neither states its length.
#
# That hides the code from a glance, a 'cat' and a synced backup. It is NOT
# offline-attack resistance: the digest is a single salted SHA-256, not a
# memory-hard KDF, so anyone who holds this file can enumerate short key
# sequences against it and get the code back. Pick a long one — and note that
# the old plaintext bytes may still linger on disk or in a snapshot.
#
# The pair is ONE secret and is MUTUALLY EXCLUSIVE with unlock_code (and with
# the deprecated hotkey below): a config carrying the pair AND a plaintext key
# is an ambiguous unlock secret and startup fails with exit 1. Half a pair —
# one of the two keys without the other — fails the same way.
#
# To change the code, run 'dndmode --set-password' again. To go back to a
# plaintext code, delete both lines and add an unlock_code line.

# --- hotkey (DEPRECATED) -----------------------------------------------------
# The pre-sequence single-combination key. Still read, so upgrading does not
# break an existing config, but setting BOTH it and unlock_code is an error
# (an ambiguous unlock secret is not resolvable). Migrate by renaming the key —
# with one caveat: spaces separate STEPS in unlock_code, so any spaces around the
# '+' must go ('Ctrl + X' is legal here, but reads as three steps below).
# hotkey: Ctrl+Option+Cmd+X

# --- activate_hotkey (watch mode only) ---------------------------------------
# The combination 'dndmode --watch' waits for while idle. Press it and the
# shield goes up exactly as if you had run 'dndmode'; type your unlock code and
# it comes back down, returning to waiting. Ignored without --watch.
#
# This is NOT a secret, and it is the opposite of unlock_code in every way that
# matters. It is printed at startup and documented in the README, so anyone
# nearby can raise the shield on your unattended machine — the same exposure
# Ctrl+Cmd+Q already has. What they cannot do is lower it: that still needs
# your unlock code.
#
# Grammar: ONE chord, "(<mod>+)*<key>" — never a sequence. A sequence is
# impossible here rather than merely unsupported: macOS matches this hotkey
# itself (Carbon RegisterEventHotKey), which is precisely what lets dndmode
# wait for hours without watching your keystrokes or holding Accessibility.
# At least one modifier is REQUIRED — a bare key would raise the shield on
# every press of it, in every application.
#
# Absent (this line commented out) means the default below. An explicitly
# EMPTY STRING means watch mode is OFF and --watch refuses to start. The quotes
# are required — a bare 'activate_hotkey:' with nothing after it is YAML null,
# which reads as absent and therefore as the default:
#   activate_hotkey: ""                # disables --watch
#
# It must differ from unlock_code, or the published combination that raises
# the shield would also lower it; startup refuses that outright.
#
# If macOS or another app (Raycast, Alfred, Karabiner) already owns the
# combination, --watch says so at startup instead of silently never firing.
# Changing this value takes effect on the next --watch start.
# activate_hotkey: Ctrl+Option+Cmd+D

# --- overlay_style -----------------------------------------------------------
# Look of the full-screen shield that covers every attached display.
#   black  : opaque black shield (default). Nothing bleeds through.
#   matrix : animated green "digital rain" over the black shield (cosmetic
#            only; every blocking guarantee is identical to black).
#   terminal : scrolling stream of syntax-highlighted pseudo-source that types
#            itself out behind a blinking caret over the black shield (cosmetic
#            only; opaque, every blocking guarantee is identical to black).
#            Language is set by terminal_language below (default go); the
#            --style terminal:<lang> flag overrides it for a single run.
#   dvd    : a "DVD VIDEO" logo bounces around the black shield, changing color
#            on every edge hit (the old-DVD-player screensaver). Cosmetic only;
#            opaque, every blocking guarantee is identical to black.
#   glass  : TRANSPARENT frosted glass — the blurred desktop shows through.
#            Trades the no-bleed-through guarantee for the look; keyboard and
#            trackpad are still fully blocked. Blur strength = glass_blur below.
#            Captures + blurs the desktop, so it needs the Screen Recording
#            permission; without it, falls back to a plain system frost.
#   none   : awake-only mode. NO overlay, NO input blocking, NO Focus, NO audio
#            mute — dndmode just holds the machine awake (like caffeinate).
#            Needs no Accessibility permission; exit with Ctrl-C only (there is
#            no unlock code because there is no event tap to observe it).
# Per-run override: --style <value>. For glass the radius can be appended:
#   --style glass:24 overrides glass_blur for this run only (--style glass uses
#   the glass_blur value below, or its default).
# overlay_style: black

# --- glass_blur --------------------------------------------------------------
# Blur radius (in points) for overlay_style 'glass' — how strongly the desktop
# behind the shield is blurred. Only used by 'glass'; ignored otherwise.
#   Lower  (~8)  : sharper — more detail, text starts to become legible.
#   Default (16) : shapes recognizable, text unreadable.
#   Higher (~30) : everything dissolves into a smooth frost.
# Per-run override: the --style glass:<radius> flag (e.g. --style glass:24).
# glass_blur: 16

# --- terminal_language -------------------------------------------------------
# Source language rendered by overlay_style 'terminal': go (default), python,
# typescript, rust or ys. Each has its own compiled-in corpus + syntax
# highlighting. Only used by 'terminal'; ignored otherwise.
#   ys : YoptaScript (yopta.space) — JavaScript as spoken by the gopniks of the
#        Russian courtyard. Same scrolling terminal, entirely in Cyrillic, and
#        entirely unprintable. Whoever wanders up to your unattended MacBook gets
#        to read that instead of your work.
# Per-run override: the --style terminal:<lang> flag (e.g. --style terminal:rust).
# terminal_language: go

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
#           banner would otherwise leak the unlock code to a bystander.
#   true  : un-silence the full startup / cleanup banners and debug logging.
# Per-run equivalent: the --debug flag (either source enables output).
# debug: false
`

// writeDefault creates the parent directory (0o700) and writes the default
// config via the atomic tmp+rename in writeAtomic.
//
// It returns the bytes it published so LoadWithSource can hand the caller the
// first-run file WITHOUT reading it back: a read-back would be a second look at
// a path a concurrent --set-password may already have replaced, which is the
// very split LoadWithSource exists to close.
func writeDefault(path string, cfg Config) ([]byte, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirPerm); err != nil {
		return nil, fmt.Errorf("mkdir parent %s: %w", dir, err)
	}
	// We hand-format the YAML from a documented template rather than calling
	// yaml.Marshal: Marshal would drop the comments (the whole point of the
	// generated file is the inline field documentation) and would emit every
	// zero-value key uncommented, which would flip the absent-key defaults
	// (mute, focus, ...). Only `unlock_code` is interpolated; all other fields
	// stay commented so their defaults come from key-absence — including the
	// deprecated `hotkey`, which MUST stay commented or ResolveUnlockCode would
	// reject the generated file as an ambiguous secret. yaml.Strict() in Load
	// re-parses our output round-trip, so any drift would surface there.
	body := fmt.Appendf(nil, defaultConfigTemplate, cfg.UnlockCode)
	if err := writeAtomic(path, body); err != nil {
		return nil, err
	}
	return body, nil
}

// writeAtomic publishes body at path via tmp+rename (protects against
// concurrent dndmode starts; the loser of the rename race still ends up with a
// valid file). Shared by writeDefault and SaveUnlockHash so there is exactly
// one place in this package that publishes a config file.
//
// The tmp file name is generated via os.CreateTemp, which guarantees a
// per-call unique suffix even when multiple goroutines (or two processes
// with the same PID after fork) race on the same path. os.CreateTemp also
// opens the file with 0o600 perms by default, so the published file
// inherits the correct mode through Rename. macOS APFS makes the final
// rename atomic, so readers always observe a fully-written file.
//
// The tmp file is created in path's OWN directory, never in TMPDIR: rename is
// only atomic within a filesystem, and SaveUnlockHash resolves symlinks that
// may well point at another volume.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
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
