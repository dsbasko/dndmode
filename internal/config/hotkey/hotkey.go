//go:build darwin

// Package hotkey parses macOS hotkey strings into a Spec (modifier mask +
// virtual keyCode). The grammar is "<modifier>(+<modifier>)*+<key>" with
// canonical modifier tokens: ctrl, option, cmd, shift, fn (case-insensitive,
// no aliases in v1 — see the design notes discretion).
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
	ModFn     ModFlag = 0x800000 // kCGEventFlagMaskSecondaryFn
)

// Spec represents a parsed hotkey: a set of modifiers + exactly one key.
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
)

var modifierTable = map[string]ModFlag{
	"ctrl":   ModCtrl,
	"option": ModOption,
	"cmd":    ModCmd,
	"shift":  ModShift,
	"fn":     ModFn,
}

// Parse converts "ctrl+option+cmd+x" into a Spec, case-insensitive.
// Order of modifiers in input is irrelevant; output Modifiers is a
// normalized bitmask. Whitespace around tokens is trimmed.
//
// Returns an error wrapping one of the sentinel errors (use errors.Is).
func Parse(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}, ErrEmpty
	}

	tokens := strings.Split(s, "+")
	if len(tokens) < 2 {
		return Spec{}, fmt.Errorf("%w: need at least one modifier and one key", ErrInvalidHotkey)
	}

	var spec Spec
	keyToken := ""
	keyTokenSet := false
	seen := map[string]bool{}

	for _, t := range tokens {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			return Spec{}, fmt.Errorf("%w: empty token", ErrInvalidHotkey)
		}
		if seen[t] {
			return Spec{}, fmt.Errorf("%w: %q", ErrDuplicateMod, t)
		}
		seen[t] = true

		if mod, ok := modifierTable[t]; ok {
			spec.Modifiers |= mod
			continue
		}
		// Non-modifier token: must resolve to a known key. If it does not,
		// surface ErrUnknownToken immediately — otherwise an unknown alias
		// like "alt" would be silently accepted as a key name and a later
		// real key (e.g. "x") would produce a misleading "two keys" error.
		code, ok := keyCodeTable[t]
		if !ok {
			return Spec{}, fmt.Errorf("%w: %q (US-ANSI key names only, e.g. 'x', 'f1', 'space')",
				ErrUnknownToken, t)
		}
		if keyTokenSet {
			return Spec{}, fmt.Errorf("%w: more than one non-modifier key (%q and %q)",
				ErrInvalidHotkey, keyToken, t)
		}
		keyToken = t
		keyTokenSet = true
		spec.KeyCode = code
	}

	if !keyTokenSet {
		return Spec{}, ErrModifierOnly
	}
	if spec.Modifiers == 0 {
		// Defensive: unreachable given len(tokens) >= 2 + keyTokenSet.
		return Spec{}, fmt.Errorf("%w: at least one modifier required", ErrInvalidHotkey)
	}
	return spec, nil
}
