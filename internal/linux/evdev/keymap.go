//go:build linux

package evdev

import (
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// UnknownKeyCode is the KeyCode recorded for a key press that has no entry
// in macKeyCode. It is outside the kVK_* range, so it never equals a step of
// a parsed unlock code: an unrecognised key BREAKS the tail like any wrong
// key would, instead of being silently skipped.
const UnknownKeyCode uint16 = 0xFFFF

// macKeyCode maps Linux evdev key codes (KEY_* from
// <linux/input-event-codes.h>) to the macOS virtual key codes (kVK_*) the
// unlock-code grammar is defined in.
//
// Translating at the input boundary is what lets hotkey, matcher and config
// stay byte-for-byte shared with macOS: a step parsed from "s" is kVK_ANSI_S
// on both platforms, and a digest written by `--set-password` on one machine
// verifies on the other. Both tables are keyed by PHYSICAL position (US-ANSI
// layout), so RU/AZERTY layouts behave identically on both.
var macKeyCode = map[uint16]uint16{
	// Letters.
	30: 0x00, 48: 0x0B, 46: 0x08, 32: 0x02, 18: 0x0E, 33: 0x03, 34: 0x05,
	35: 0x04, 23: 0x22, 36: 0x26, 37: 0x28, 38: 0x25, 50: 0x2E, 49: 0x2D,
	24: 0x1F, 25: 0x23, 16: 0x0C, 19: 0x0F, 31: 0x01, 20: 0x11, 22: 0x20,
	47: 0x09, 17: 0x0D, 45: 0x07, 21: 0x10, 44: 0x06,

	// Digits 1-9, 0.
	2: 0x12, 3: 0x13, 4: 0x14, 5: 0x15, 6: 0x17, 7: 0x16, 8: 0x1A,
	9: 0x1C, 10: 0x19, 11: 0x1D,

	// F1-F12.
	59: 0x7A, 60: 0x78, 61: 0x63, 62: 0x76, 63: 0x60, 64: 0x61,
	65: 0x62, 66: 0x64, 67: 0x65, 68: 0x6D, 87: 0x67, 88: 0x6F,

	// Whitespace and editing.
	57:  0x31, // space
	28:  0x24, // enter → return
	15:  0x30, // tab
	1:   0x35, // esc
	14:  0x33, // backspace → delete
	111: 0x75, // delete → forwarddelete

	// Arrows.
	105: 0x7B, 106: 0x7C, 108: 0x7D, 103: 0x7E,

	// Punctuation (US-ANSI positions).
	12: 0x1B, // -
	13: 0x18, // =
	26: 0x21, // [
	27: 0x1E, // ]
	39: 0x29, // ;
	40: 0x27, // '
	51: 0x2B, // ,
	52: 0x2F, // .
	53: 0x2C, // /
	43: 0x2A, // backslash
	41: 0x32, // `

	// Keys outside the grammar that still have a kVK_* equivalent: they can
	// be captured by --set-password into a digest, and they match by
	// position like everything else.
	86: 0x0A, // KEY_102ND → kVK_ISO_Section
	96: 0x4C, // KEY_KPENTER → kVK_ANSI_KeypadEnter
}

// modifierBit maps the evdev modifier keys onto the macOS modifier flags the
// grammar uses: ctrl → Ctrl, shift → Shift, option → Alt (both Alt keys,
// AltGr included), cmd → Super. Nothing else is a modifier.
var modifierBit = map[uint16]hotkey.ModFlag{
	29:  hotkey.ModCtrl,   // KEY_LEFTCTRL
	97:  hotkey.ModCtrl,   // KEY_RIGHTCTRL
	42:  hotkey.ModShift,  // KEY_LEFTSHIFT
	54:  hotkey.ModShift,  // KEY_RIGHTSHIFT
	56:  hotkey.ModOption, // KEY_LEFTALT
	100: hotkey.ModOption, // KEY_RIGHTALT
	125: hotkey.ModCmd,    // KEY_LEFTMETA
	126: hotkey.ModCmd,    // KEY_RIGHTMETA
}

// ignoredKey lists key codes that never enter the keystroke history: lock
// keys and Fn. On macOS these arrive as flag changes, never as key-downs,
// so they cannot be steps there either; treating them the same here keeps a
// stray CapsLock from breaking a half-typed code.
var ignoredKey = map[uint16]bool{
	58:  true, // KEY_CAPSLOCK
	69:  true, // KEY_NUMLOCK
	70:  true, // KEY_SCROLLLOCK
	464: true, // KEY_FN
}

// recordable reports whether a press of code belongs in the keystroke
// history. Mouse and gamepad buttons (BTN_*, 0x100 and up) and media keys
// without a kVK_* equivalent are excluded, mirroring macOS, where clicks are
// not key-downs and media keys arrive as system-defined events. Everything
// else on the main keyboard block is recorded, known or not.
func recordable(code uint16) bool {
	if _, ok := modifierBit[code]; ok || ignoredKey[code] {
		return false
	}
	if _, ok := macKeyCode[code]; ok {
		return true
	}
	return code > 0 && code < 128
}

// toKeyEvent builds the history record for a press of code under mods.
func toKeyEvent(code uint16, mods hotkey.ModFlag) matcher.KeyEvent {
	kc, ok := macKeyCode[code]
	if !ok {
		kc = UnknownKeyCode
	}
	return matcher.KeyEvent{Modifiers: mods & matcher.UserIntentionalMask, KeyCode: kc}
}
