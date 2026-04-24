//go:build darwin

package cocoa

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>

extern int  cocoa_is_main_thread(void);
extern void cocoa_dispatch_main(uintptr_t handle);
*/
import "C"

import "runtime/cgo"

// DispatchMain runs fn on the OS-main thread.
//
// If the caller is already on the main thread (verified via pthread_main_np()
// in cocoa_is_main_thread), fn runs INLINE — synchronously, before
// DispatchMain returns. This is the fast path used by controller.Release
//, which is invoked from cmd/dndmode/main.go's defer rs.Cleanup() and
// is therefore already on the main goroutine.
//
// Otherwise, fn is boxed into a runtime/cgo.Handle (Go 1.17+ canonical API
// for passing Go closures across the cgo boundary) and dispatch_async'd onto
// dispatch_get_main_queue(). DispatchMain returns immediately; fn runs later
// when the main runloop processes the queue. The handle is released by
// goCocoaDispatchCallback after fn finishes.
//
// MUST be safe to call from any goroutine. The async path itself does NOT
// require the caller to be on a specific OS thread; the async branch is the
// whole point.
//
// Caveat: if the main runloop is stopped between dispatch_async and block
// processing, the queued block may never run and the handle leaks. Phase 2
// mitigates by (a) calling debouncer.Stop() before Release, and (b) Release
// itself running inline (fast-path). See the design notes.
func DispatchMain(fn func()) {
	if C.cocoa_is_main_thread() != 0 {
		fn() // fast path
		return
	}
	h := cgo.NewHandle(fn)
	C.cocoa_dispatch_main(C.uintptr_t(h))
}

// goCocoaDispatchCallback is invoked from cocoa_dispatch_main's libdispatch
// block after the main runloop picks up the queued work. It un-boxes the Go
// closure, runs it, and releases the cgo.Handle exactly once.
//
// MUST run on the main thread (libdispatch ensures this via main queue).
//
//export goCocoaDispatchCallback
func goCocoaDispatchCallback(handle C.uintptr_t) {
	h := cgo.Handle(handle)
	fn := h.Value().(func())
	fn()
	h.Delete()
}
