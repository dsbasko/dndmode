//go:build darwin

package hotkey

// keyCodeTable maps lowercase US-ANSI key names to macOS virtual keyCodes
// (kVK_* constants from <HIToolbox/Events.h>). The lookup is case-insensitive
// (caller must lowercase before lookup).
//
// Source: phracker/MacOSX-SDKs HIToolbox Events.h (canonical Carbon header).
//
//	https://github.com/phracker/MacOSX-SDKs/blob/master/MacOSX10.10.sdk/System/Library/Frameworks/Carbon.framework/Versions/A/Frameworks/HIToolbox.framework/Versions/A/Headers/Events.h
//
// Matching is by physical key position (kVK_*), not by character — so RU/AZERTY
// layouts work identically.
var keyCodeTable = map[string]uint16{
	// Letters A-Z (kVK_ANSI_A..Z) — 26 entries
	"a": 0x00, "s": 0x01, "d": 0x02, "f": 0x03, "h": 0x04, "g": 0x05,
	"z": 0x06, "x": 0x07, "c": 0x08, "v": 0x09, "b": 0x0B, "q": 0x0C,
	"w": 0x0D, "e": 0x0E, "r": 0x0F, "y": 0x10, "t": 0x11,
	"o": 0x1F, "u": 0x20, "i": 0x22, "p": 0x23, "l": 0x25, "j": 0x26,
	"k": 0x28, "n": 0x2D, "m": 0x2E,

	// Digits 0-9 (kVK_ANSI_0..9) — 10 entries
	"1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15, "5": 0x17, "6": 0x16,
	"7": 0x1A, "8": 0x1C, "9": 0x19, "0": 0x1D,

	// Function keys F1-F12 — 12 entries
	"f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76,
	"f5": 0x60, "f6": 0x61, "f7": 0x62, "f8": 0x64,
	"f9": 0x65, "f10": 0x6D, "f11": 0x67, "f12": 0x6F,

	// Whitespace / control keys — 7 entries (+1 alias enter, +1 alias esc)
	"space":         0x31,
	"return":        0x24,
	"enter":         0x24, // alias for return
	"tab":           0x30,
	"escape":        0x35,
	"esc":           0x35, // alias for escape
	"delete":        0x33, // backspace
	"forwarddelete": 0x75,

	// Arrow keys — 4 entries
	"left":  0x7B,
	"right": 0x7C,
	"down":  0x7D,
	"up":    0x7E,

	// Punctuation — 11 entries (US-ANSI positions)
	"-":  0x1B,
	"=":  0x18,
	"[":  0x21,
	"]":  0x1E,
	";":  0x29,
	"'":  0x27,
	",":  0x2B,
	".":  0x2F,
	"/":  0x2C,
	"\\": 0x2A,
	"`":  0x32,
}
