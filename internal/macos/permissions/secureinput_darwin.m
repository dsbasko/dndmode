// +build darwin

#import <Carbon/Carbon.h>

// permissions_secure_event_input_enabled returns 1 if any process is
// currently holding a SecureEventInput claim, 0 otherwise. Wraps Carbon's
// IsSecureEventInputEnabled — no parameters, no CF state, no Objective-C
// objects.
//
// IsSecureEventInputEnabled is marked deprecated in the Carbon headers
// (Carbon itself is end-of-line), but the function continues to ship and
// work on macOS 14 and 15. Production users (Karabiner-Elements,
// Hammerspoon, Alfred) confirm via release notes. No public SPI replacement
// exists; if Apple ever removes it, the fallback is to parse
// `ioreg -l -w 0 | grep SecureInput` (Phase 6 README path).
//
//. The deprecation warning is silenced via cgo CFLAGS rather than
// a per-call pragma to avoid Objective-C readability noise.
int permissions_secure_event_input_enabled(void) {
    return IsSecureEventInputEnabled() ? 1 : 0;
}
