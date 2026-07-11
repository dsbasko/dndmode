//go:build darwin

package cocoa

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>
#include <stddef.h>

extern int      cocoa_screens_register_observers(void);
extern int      cocoa_screens_unregister_observers(void);
extern size_t   cocoa_enumerate_screens(uint32_t* outIDs, size_t maxIDs);
extern uint64_t cocoa_screens_geometry_signature(void);
extern int      cocoa_test_get_screen_register_count(void);
extern int      cocoa_test_get_cg_register_count(void);
*/
import "C"

import (
	"sync/atomic"
	"unsafe"
)

// maxScreens caps the local stack buffer used by EnumerateScreensCount and
// downstream callers (controller.reconcile). Realistic max for a Mac with
// Thunderbolt daisy-chains is ~6-8; 16 leaves headroom without heap allocs.
const maxScreens = 16

// activeOnScreensChanged is the package-level callback registry. controller
// stores its OnScreensChanged here on construction; clears (sets nil) on
// Release. atomic.Pointer is used because the read path
// (goCocoaOnScreensChanged) is hot — every screen-reconfig event — while the
// write path (controller registration) is cold (Init / Release once).
//
// the design notes — canonical Atomic-Pointer Callback Registration
// pattern; race-free without lock contention.
var activeOnScreensChanged atomic.Pointer[func()]

// setOnScreensChanged installs the package-level callback invoked by
// goCocoaOnScreensChanged on every screen-reconfig event. controller.Release
// passes nil to detach.
//
// MUST be called from the main goroutine (controller construction +
// controller.Release both run there per /).
func setOnScreensChanged(cb *func()) {
	activeOnScreensChanged.Store(cb)
}

// goCocoaOnScreensChanged is the //export Go callback dispatched from
// screens_darwin.m via dispatch_async(main_queue, ...). By the time it runs
// we are on OS thread #0 (main queue dispatch guarantee + runtimepin
// LockOSThread invariant — see internal/runtimepin).
//
// It loads the active controller callback and invokes it. If no controller
// is currently registered (e.g. between Release and process exit), the call
// is a silent no-op.
//
//export goCocoaOnScreensChanged
func goCocoaOnScreensChanged() {
	cb := activeOnScreensChanged.Load()
	if cb == nil {
		return
	}
	(*cb)()
}

// registerScreenObservers wraps cocoa_screens_register_observers for
// internal use by Init. Returns 0 on success, the CGError on failure.
func registerScreenObservers() int {
	return int(C.cocoa_screens_register_observers())
}

// unregisterScreenObservers wraps cocoa_screens_unregister_observers for
// internal use by controller.Release. Returns 0 on success, the CGError on
// failure.
func unregisterScreenObservers() int {
	return int(C.cocoa_screens_unregister_observers())
}

// EnumerateScreensCount returns the number of attached NSScreens (i.e.
// len([NSScreen screens])). Used by smoke tests to assert
// controller.WindowCount() matches.
//
// MUST be called from the main goroutine; under the hood it invokes the
// Cocoa NSScreen API which requires main-thread access.
func EnumerateScreensCount() int {
	var ids [maxScreens]C.uint32_t
	n := C.cocoa_enumerate_screens(
		(*C.uint32_t)(unsafe.Pointer(&ids[0])),
		C.size_t(maxScreens),
	)
	return int(n)
}

// enumerateScreens is the internal helper used by controller.reconcile to
// fetch the current displayID list. Returns a freshly allocated []uint32
// (length == count). Safe to call repeatedly; allocates per call.
func enumerateScreens() []uint32 {
	var ids [maxScreens]C.uint32_t
	n := int(C.cocoa_enumerate_screens(
		(*C.uint32_t)(unsafe.Pointer(&ids[0])),
		C.size_t(maxScreens),
	))
	if n == 0 {
		return nil
	}
	out := make([]uint32, n)
	for i := 0; i < n; i++ {
		out[i] = uint32(ids[i])
	}
	return out
}

// screensGeometrySignature returns a 64-bit change-detector over the full
// geometry (displayID + frame + backing scale) of all attached screens. The
// controller compares it across reconcile events to skip rebuilding a live
// overlay on a no-op screen-reconfig (notably the menu-bar visibleFrame change
// from the activation-policy flip at overlay start, which must NOT tear down the
// glass CABackdropLayer blur). See cocoa_screens_geometry_signature in
// screens_darwin.m for why it reads frame (not visibleFrame).
//
// MUST be called from the main goroutine (NSScreen main-thread invariant).
func screensGeometrySignature() uint64 {
	return uint64(C.cocoa_screens_geometry_signature())
}

// testScreenRegisterCount returns the cumulative number of times the
// NSNotificationCenter NSApplicationDidChangeScreenParameters observer was
// registered in this process. Test-only helper for dual-observer
// verification (cocoa.Init must trigger BOTH observers exactly once).
//
// Implementation sits in screens_darwin.m as a static int counter incremented
// inside cocoa_screens_register_observers; never reset between calls. Tests
// snapshot before+after and assert delta. Safe to call from any goroutine
// (read-only int read; guarantees writes happen on main thread).
func testScreenRegisterCount() int {
	return int(C.cocoa_test_get_screen_register_count())
}

// testCGRegisterCount returns the cumulative number of times
// CGDisplayRegisterReconfigurationCallback succeeded in this process.
// Companion to testScreenRegisterCount — together they prove dual-observer
// registration. See screens_darwin.m for the underlying counter.
func testCGRegisterCount() int {
	return int(C.cocoa_test_get_cg_register_count())
}
