//go:build darwin

// Package hotkey parses macOS unlock codes into a sequence of Specs
// (modifier mask + virtual keyCode per step).
//
// A single step has the grammar "(<modifier>+)*<key>" with canonical modifier
// tokens: ctrl, option, cmd, shift (case-insensitive, no aliases in v1 —
// see the design notes discretion). Modifiers are optional inside a step, so
// both "s" and "ctrl+s" are valid steps. The token "fn" is also accepted but
// contributes nothing — see ignoredModifiers for why it cannot.
//
// An unlock code is a whitespace-separated sequence of at most MaxSteps steps:
//
//	s w o r d f i s h
//	ctrl+s w cmd+z
//	Ctrl+Option+Cmd+X        // legacy single-combination hotkey = code of length 1
//
// A physical space is only a separator; the space key itself is named by the
// token "space", so the two never collide.
//
// Parse is the legacy single-combination entry point (the deprecated `hotkey`
// config key): it is ParseStep plus the requirement of at least one modifier.
//
// Matching is by physical key position (kVK_* virtual keyCode), not by
// character — so RU/AZERTY layouts produce the same keyCode for the same
// physical key.
package hotkey

import (
	"errors"
	"fmt"
	"strings"
)

// ModFlag is a bitmask of macOS modifier flags. Values match CGEventFlags
// for direct comparison in the CGEventTap callback (Phase 4).
type ModFlag uint64

const (
	ModCtrl   ModFlag = 0x040000 // kCGEventFlagMaskControl
	ModOption ModFlag = 0x080000 // kCGEventFlagMaskAlternate
	ModCmd    ModFlag = 0x100000 // kCGEventFlagMaskCommand
	ModShift  ModFlag = 0x020000 // kCGEventFlagMaskShift

	// ModFn is kCGEventFlagMaskSecondaryFn. It is NEVER produced by ParseStep
	// and never compared against: macOS raises this bit for the whole
	// function-key group (F1-F12, arrows, Forward Delete, Home/End/Page…)
	// regardless of the physical Fn key, so it cannot express intent and is
	// stripped by matcher.UserIntentionalMask on both sides of the
	// comparison. The constant survives only to name the bit in that
	// argument — see the UserIntentionalMask doc comment.
	ModFn ModFlag = 0x800000
)

// MaxSteps is the upper bound on the number of steps in an unlock code.
// It is half of the C-side keystroke ring capacity (DND_RING_CAP), which
// leaves a 2x headroom so a complete code always fits inside the window the
// poller is able to read back.
//
// The length check lives here and only here — validation layers above must
// not duplicate it.
const MaxSteps = 32

// Spec represents a parsed step: a set of modifiers + exactly one key.
type Spec struct {
	Modifiers ModFlag
	KeyCode   uint16
}

// Sentinel errors. Use errors.Is to identify category.
var (
	ErrEmpty         = errors.New("hotkey: empty string")
	ErrModifierOnly  = errors.New("hotkey: modifier-only combinations are not allowed; specify exactly one non-modifier key")
	ErrInvalidHotkey = errors.New("hotkey: invalid syntax")
	ErrUnknownToken  = errors.New("hotkey: unknown token")
	ErrDuplicateMod  = errors.New("hotkey: duplicate modifier")
	ErrTooManySteps  = errors.New("hotkey: too many steps in unlock code")
)

var modifierTable = map[string]ModFlag{
	"ctrl":   ModCtrl,
	"option": ModOption,
	"cmd":    ModCmd,
	"shift":  ModShift,
}

// ignoredModifiers are modifier tokens that parse but contribute no bit to
// the resulting Spec. `fn` is the only member and it is here rather than in
// modifierTable because macOS raises kCGEventFlagMaskSecondaryFn for the
// entire function-key group on its own: a step could demand it but the user
// could not withhold it, and a step that omits it would be unmatchable on
// exactly those keys. matcher.UserIntentionalMask strips the bit from every
// recorded event for that reason, so emitting it here would produce a Spec
// that can never match. Accepting and dropping the token keeps configs
// written against the older grammar loading instead of aborting startup.
//
// Consequence worth knowing: `fn+x` is the same step as `x`. A ONE-step
// unlock code of `fn+x` therefore carries no modifier at all and is
// rejected by config.ValidateUnlockCode — which is the correct outcome, a
// bare key must not unlock the shield on a single press.
var ignoredModifiers = map[string]bool{
	"fn": true,
}

// ParseStep converts one step of an unlock code into a Spec, case-insensitive:
// "s", "ctrl+s", "Ctrl+Option+Cmd+X". Order of modifiers in input is
// irrelevant; output Modifiers is a normalized bitmask. Whitespace around
// tokens is trimmed.
//
// Unlike Parse, a step carrying zero modifiers is valid — that is exactly what
// makes a passphrase-style code ("s w o r d") expressible. Exactly one
// non-modifier key is still required. The `fn` token is accepted and dropped
// (see ignoredModifiers).
//
// Returns an error wrapping one of the sentinel errors (use errors.Is). No
// error path interpolates the offending token: the input is the user's
// unlock secret, and a diagnostic that echoed even one step of a mistyped
// passphrase would put a fragment of it in the terminal scrollback. The
// category plus the 1-based step position added by ParseSequence is what
// locates the problem.
func ParseStep(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}, ErrEmpty
	}

	tokens := strings.Split(s, "+")

	var spec Spec
	keyTokenSet := false
	seen := map[string]bool{}

	for _, t := range tokens {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			return Spec{}, fmt.Errorf("%w: empty token", ErrInvalidHotkey)
		}
		_, isMod := modifierTable[t]
		isMod = isMod || ignoredModifiers[t]
		if seen[t] {
			// A repeated NON-modifier is a repeated key, not a repeated
			// modifier: reporting it as ErrDuplicateMod would send a user
			// whose step contains no modifier at all looking for one.
			if !isMod {
				return Spec{}, fmt.Errorf("%w: the same key appears twice in one step", ErrInvalidHotkey)
			}
			return Spec{}, ErrDuplicateMod
		}
		seen[t] = true

		if mod, ok := modifierTable[t]; ok {
			spec.Modifiers |= mod
			continue
		}
		if isMod {
			// Accepted-and-dropped modifier (`fn`). Not a key, so it must
			// not fall through to the keyCodeTable lookup below.
			continue
		}
		// Non-modifier token: must resolve to a known key. If it does not,
		// surface ErrUnknownToken immediately — otherwise an unknown alias
		// like "alt" would be silently accepted as a key name and a later
		// real key (e.g. "x") would produce a misleading "two keys" error.
		code, ok := keyCodeTable[t]
		if !ok {
			return Spec{}, fmt.Errorf("%w (US-ANSI key names only, e.g. 'x', 'f1', 'space')",
				ErrUnknownToken)
		}
		if keyTokenSet {
			return Spec{}, fmt.Errorf("%w: more than one non-modifier key in a single step "+
				"(steps are separated by spaces, keys inside a step by '+')", ErrInvalidHotkey)
		}
		keyTokenSet = true
		spec.KeyCode = code
	}

	if !keyTokenSet {
		return Spec{}, ErrModifierOnly
	}
	return spec, nil
}

// Parse converts "ctrl+option+cmd+x" into a Spec, case-insensitive. It is
// ParseStep plus the legacy requirement of at least one modifier, and backs
// the deprecated single-combination `hotkey` config key.
//
// The error text names `fn` unconditionally — not because the input was
// inspected for it (that would leak a fragment of the secret; see ParseStep)
// but because `fn` used to be a real modifier in this table and a config
// written against that grammar is the likeliest way to reach this branch at
// all. A `hotkey: fn+x` that worked before now lands here, and without the
// hint the message ("need at least one modifier") reads as nonsense to
// someone looking at a value that plainly contains one.
//
// Returns an error wrapping one of the sentinel errors (use errors.Is).
func Parse(s string) (Spec, error) {
	spec, err := ParseStep(s)
	if err != nil {
		return Spec{}, err
	}
	if spec.Modifiers == 0 {
		return Spec{}, fmt.Errorf("%w: need at least one modifier (ctrl, option, cmd, shift) "+
			"and one key — note that 'fn' parses but is NOT a modifier, because macOS raises "+
			"its bit for the whole function-key group on its own", ErrInvalidHotkey)
	}
	return spec, nil
}

// ParseSequence converts a whitespace-separated unlock code ("s w o r d",
// "ctrl+s w cmd+z") into the sequence of steps it denotes. Any run of spaces
// and tabs collapses into a single separator, so leading, trailing and
// repeated whitespace is harmless.
//
// A failing step is reported by its 1-based position only — the step's own
// text is deliberately left out so a parse error never echoes a fragment of
// the user's secret. Sentinels stay reachable through errors.Is.
func ParseSequence(s string) ([]Spec, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, ErrEmpty
	}
	if len(fields) > MaxSteps {
		return nil, fmt.Errorf("%w: got %d steps, max %d", ErrTooManySteps, len(fields), MaxSteps)
	}

	steps := make([]Spec, 0, len(fields))
	for i, f := range fields {
		spec, err := ParseStep(f)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		steps = append(steps, spec)
	}
	return steps, nil
}
