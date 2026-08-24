//go:build darwin

package cocoa

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Cocoa

extern void cocoa_beep(void);
*/
import "C"

// Beep plays the system alert sound (NSBeep).
//
// It exists for exactly one caller: watch mode, when a press of the
// activation combination does NOT result in a shield. That is the one failure
// in dndmode where silence is dangerous rather than merely unhelpful — the
// user pressed the combination, believes the machine is locked, and walks
// away from an open laptop. The terminal line saying otherwise may well be
// behind another window.
//
// This is deliberately NOT used anywhere near the unlock path. Silence on
// wrong input is a security property there: a sound would tell a bystander
// that dndmode is listening and that a keystroke registered. Refusing to
// activate leaks nothing, because the activation combination is public.
//
// Safe from any goroutine; NSBeep is thread-safe. A machine with alert sounds
// muted hears nothing, which is why the printed line is the primary signal
// and this is the backup rather than the other way round.
func Beep() { C.cocoa_beep() }
