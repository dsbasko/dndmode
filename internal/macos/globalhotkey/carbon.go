//go:build darwin

package globalhotkey

import (
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// Carbon modifier masks, from <Carbon/HIToolbox/Events.h>. These are NOT the
// CGEventFlags values hotkey.ModFlag carries — Carbon predates Quartz and
// uses its own, much smaller, bit layout. Converting between the two is the
// entire job of this file, and it lives on the Go side (rather than being
// done inline in the .m) because C in this project is not unit-testable:
// cgo is unavailable from _test.go, so anything expressed in C is verified
// only by a human pressing keys.
const (
	carbonCmdKey     uint32 = 0x0100
	carbonShiftKey   uint32 = 0x0200
	carbonOptionKey  uint32 = 0x0800
	carbonControlKey uint32 = 0x1000
)

// carbonModifiers converts a hotkey.ModFlag bitmask into Carbon's modifier
// encoding, returning ErrNoModifier if nothing user-intentional survives.
//
// matcher.UserIntentionalMask is applied FIRST, and that ordering is the
// point of the function rather than a detail. macOS raises modifier bits on
// its own — CapsLock, NumPad, Help, and above all SecondaryFn (Fn), which it
// sets for the entire function-key group whether or not the physical key is
// held. Carbon has no encoding for any of them, so a stray bit could not be
// expressed even if we wanted to: it would either be dropped silently or,
// worse, collide with a Carbon mask that means something else entirely. The
// project-wide rule covers exactly this — if a bit is set by the system, it
// is stripped from BOTH sides of the comparison rather than demanded.
//
// The mask is also what makes `fn+d` and `d` the same request here, which is
// why the emptiness check has to run after it and not before.
func carbonModifiers(m hotkey.ModFlag) (uint32, error) {
	m &= matcher.UserIntentionalMask
	if m == 0 {
		return 0, ErrNoModifier
	}

	var out uint32
	for flag, carbon := range map[hotkey.ModFlag]uint32{
		hotkey.ModCtrl:   carbonControlKey,
		hotkey.ModOption: carbonOptionKey,
		hotkey.ModCmd:    carbonCmdKey,
		hotkey.ModShift:  carbonShiftKey,
	} {
		if m&flag != 0 {
			out |= carbon
		}
	}
	return out, nil
}
