//go:build darwin

package permissions

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Carbon

#include <stdint.h>

extern int permissions_secure_event_input_enabled(void);
*/
import "C"

// IsSecureEventInputActive reports true if ANY process on the system
// currently holds a SecureEventInput claim. Backed by Carbon's
// IsSecureEventInputEnabled.
//
// Typical owners observed in practice: Terminal/iTerm2 displaying a sudo
// password prompt, the macOS password fields (login window, FileVault,
// Wi-Fi password), 1Password's autofill flow, GnuPG pinentry, etc.
//
// When this returns true, CGEventTap suppression simply cannot intercept
// keystrokes regardless of TCC state — Apple deliberately routes those
// events to the SecureEventInput owner via a private channel that bypasses
// HID-tap. Therefore cmd/dndmode/main.go MUST treat a positive return as
// fatal pre-flight: print a single actionable line to stderr and exit with
// code 4. Owner detection (ioreg parsing) is deferred to
// Phase 6 README-level troubleshoot.
//
//. SAFE from any goroutine. The Carbon API is technically
// "deprecated" in the SDK headers but remains shipping and working as of
// macOS 14/15 — Karabiner, Hammerspoon, Alfred, and friends all rely on it
// in production. No replacement public API exists.
func IsSecureEventInputActive() bool {
	return C.permissions_secure_event_input_enabled() != 0
}
