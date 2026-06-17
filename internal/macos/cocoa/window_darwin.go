//go:build darwin

package cocoa

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>
#include <stdlib.h>

extern void* cocoa_create_overlay_window(uint32_t displayID, const char* style, char** outErr);
extern void  cocoa_close_overlay_window(void* windowHandle);
extern long  cocoa_window_level(void* windowHandle);
extern int   cocoa_window_is_visible(void* windowHandle);
extern unsigned long cocoa_window_collection_behavior(void* windowHandle);
extern uint32_t cocoa_first_attached_display_id(int* outFound);
extern int   cocoa_is_main_thread(void);
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// createOverlayWindowStyled allocates and configures one full-screen NSWindow
// for the given CGDirectDisplayID with the requested overlay style ("black" =>
// plain opaque-black shield; "matrix" => a MatrixView digital-rain contentView
// over the same opaque-black base). The window keeps every shield guarantee
// regardless of style (QUICK-gh8). Returns the boxed NSWindow pointer (caller
// owns; pass to closeOverlayWindow exactly once). On failure returns
// (nil, error) with the C-side strdup'd message bridged via C.GoString +
// C.free.
//
// MUST be called from the main goroutine (NSWindow + NSScreen API requires
// main thread). Caller is controller.reconcile via cgoWindowFactory, which
// runs under DispatchMain (ensures main-thread invariant).
func createOverlayWindowStyled(displayID uint32, style string) (unsafe.Pointer, error) {
	cStyle := C.CString(style)
	defer C.free(unsafe.Pointer(cStyle))
	var cErr *C.char
	w := C.cocoa_create_overlay_window(C.uint32_t(displayID), cStyle, &cErr)
	if w == nil {
		msg := C.GoString(cErr)
		C.free(unsafe.Pointer(cErr))
		return nil, fmt.Errorf("create overlay window for displayID=%d: %s", displayID, msg)
	}
	return w, nil
}

// createOverlayWindow is a thin shim over createOverlayWindowStyled with the
// "black" style. It preserves the byte-for-byte plain-black path and keeps the
// window_smoketest_test.go callers (TestSmoke_NSWindow_*) working with zero
// edits. Both are package-private.
//
// MUST be called from the main goroutine (see createOverlayWindowStyled).
func createOverlayWindow(displayID uint32) (unsafe.Pointer, error) {
	return createOverlayWindowStyled(displayID, "black")
}

// closeOverlayWindow orders out + closes the NSWindow and releases the
// strong reference held since createOverlayWindow returned. nil handle is
// a no-op (caller convenience for cleanup-loop idempotency).
//
// MUST be called from the main goroutine. controller.Release routes this
// through DispatchMain.
func closeOverlayWindow(w unsafe.Pointer) {
	if w == nil {
		return
	}
	C.cocoa_close_overlay_window(w)
}

// windowLevel returns the level of the given NSWindow handle. Used by
// smoke tests to verify (window.level == CGShieldingWindowLevel()).
func windowLevel(w unsafe.Pointer) int {
	return int(C.cocoa_window_level(w))
}

// windowIsVisible returns true if the NSWindow is currently on screen.
// Used by smoke tests to verify (orderFrontRegardless took effect).
func windowIsVisible(w unsafe.Pointer) bool {
	return C.cocoa_window_is_visible(w) != 0
}

// isMainThreadForTest reports whether the current OS thread is the main
// thread. Test-only helper letting smoke tests t.Skip gracefully when they
// would otherwise abort the binary with NSInternalInconsistencyException
// ("NSWindow should only be instantiated on the main thread!"). Production
// code uses the DispatchMain helper from instead.
func isMainThreadForTest() bool {
	return C.cocoa_is_main_thread() != 0
}

// firstAttachedDisplayIDForTest returns the CGDirectDisplayID of the first
// attached NSScreen plus a found-bool. Test-only helper used by
// window_smoketest_test.go to acquire a real displayID without depending on
// internal/macos/cocoa/screens_darwin.go (parallel).
//
// Cgo cannot be used directly inside _test.go files of an internal cgo
// package (Go toolchain limitation), so this thin Go wrapper lives in the
// production file alongside the other cgo wrappers; the underlying C helper
// is itself documented test-only in window_darwin.m.
func firstAttachedDisplayIDForTest() (uint32, bool) {
	var found C.int
	id := C.cocoa_first_attached_display_id(&found)
	return uint32(id), found != 0
}

// windowCollectionBehavior returns the raw NSWindowCollectionBehavior
// bitmask. Used by smoke tests to verify: the four required flags
// (CanJoinAllSpaces|Stationary|FullScreenAuxiliary|IgnoresCycle) MUST all
// be set. Bitmask reference (NSWindow.h verified):
//
//	NSWindowCollectionBehaviorCanJoinAllSpaces    = 1 << 0  // 0x001
//	NSWindowCollectionBehaviorStationary          = 1 << 4  // 0x010
//	NSWindowCollectionBehaviorIgnoresCycle        = 1 << 6  // 0x040
//	NSWindowCollectionBehaviorFullScreenAuxiliary = 1 << 8  // 0x100
//	OR sum = 0x151 = 337
func windowCollectionBehavior(w unsafe.Pointer) uint64 {
	return uint64(C.cocoa_window_collection_behavior(w))
}
