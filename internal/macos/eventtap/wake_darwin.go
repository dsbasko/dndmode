//go:build darwin

package eventtap

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

extern int  wake_observer_install(CFMachPortRef tap);
extern void wake_observer_remove(void);
*/
import "C"

import (
	"unsafe"
)

// installWakeObserver registers the NSWorkspace wake + session-active
// observers that re-arm the CGEventTap after a system sleep / fast user
// switch (D-08 step 5). Wave 1 04-04 implements the full bridge into
// `wake_observer_install` (wake_darwin.m) which captures the tap pointer
// and subscribes to both notifications via
// `[[NSWorkspace sharedWorkspace] notificationCenter] addObserverForName:`.
//
// MUST be called from the main goroutine (NSWorkspace
// notificationCenter is documented main-thread-only).
//
// Wave 0 stub: no-op; returns nil so the Install caller's setup path can
// already chain wake-observer installation without conditional logic.
// Wave 1 04-04 replaces this body with the cgo bridge call and proper
// error mapping (the C side currently never fails — a NULL pointer guard
// will be added when the real subscription logic is wired).
func installWakeObserver(tap unsafe.Pointer) error {
	_ = tap
	// Reference the cgo binding so the C symbol stays linked through
	// dead-code elimination even before Wave 1 wires the real install
	// path. Mirrors the same idiom used in tap_darwin.go Install.
	if false {
		var t C.CFMachPortRef
		_ = C.wake_observer_install(t)
	}
	return nil
}

// removeWakeObserver unsubscribes the NSWorkspace observers and clears the
// cached tap pointer (D-08 step 5 teardown counterpart). Wave 1 04-04
// implements the bridge into `wake_observer_remove` (wake_darwin.m).
//
// MUST be called from the main goroutine. Called by Releaser.Release as
// part of the LIFO teardown chain.
//
// Idempotent — safe to call when no observer is installed (Wave 1 04-04's
// C side will guard on token==nil).
//
// Wave 0 stub: no-op.
func removeWakeObserver() {
	if false {
		C.wake_observer_remove()
	}
}
