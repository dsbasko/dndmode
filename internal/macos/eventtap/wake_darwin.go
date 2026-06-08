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
	"fmt"
	"log/slog"
	"unsafe"
)

// installWakeObserver registers the NSWorkspace wake + session-active
// observers that re-arm the CGEventTap after a system sleep / fast user
// switch (step 5). Bridges into `wake_observer_install`
// (wake_darwin.m) which captures the tap pointer and subscribes to both
// notifications via `[[NSWorkspace sharedWorkspace] notificationCenter]
// addObserverForName:` on `[NSOperationQueue mainQueue]`.
//
// MUST be called from the main goroutine — NSWorkspace notificationCenter
// is documented main-thread-only.
//
// Return codes from the C side (kept in lockstep with wake_darwin.m):
//
//	0 — success.
//	1 — tap is NULL (callers MUST pre-check; we also guard here).
//	2 — already installed (caller must Remove first; defensive).
//
// Any non-zero rc is surfaced as a plain `fmt.Errorf` (no sentinel) — the
// only realistic failure is a programming error in the install chain
// (main.go must guarantee a non-nil tap before invoking).
func installWakeObserver(tap unsafe.Pointer) error {
	if tap == nil {
		return fmt.Errorf("wake observer: tap is nil")
	}
	rc := C.wake_observer_install((C.CFMachPortRef)(tap))
	if rc != 0 {
		return fmt.Errorf("wake_observer_install: rc=%d", int(rc))
	}
	return nil
}

// removeWakeObserver unsubscribes the NSWorkspace observers and clears the
// cached tap pointer (step 5 teardown counterpart). Bridges into
// `wake_observer_remove` (wake_darwin.m), which removes both observer
// tokens and zeroes the static `g_observed_tap`.
//
// MUST be called from the main goroutine. Called by Releaser.Release as
// part of the LIFO teardown chain (step 5).
//
// Idempotent — safe to call when no observer is installed (the C side
// guards on `g_wake_token == nil` / `g_session_token == nil`).
func removeWakeObserver() {
	C.wake_observer_remove()
}

// InstallWakeObserver wires the NSWorkspace wake + session-active observer
// for the supplied CGEventTap pointer and returns a `stop` closure that
// tears the observer down (LIFO teardown contract — step 5).
//
// Composition (main.go):
//
//	wake, err := eventtap.InstallWakeObserver(tap, log)
//	if err != nil { ... }
//	releaser.Push(wake) // tap is released BEFORE wake observer (order)
//
// Mirrors the same shape as `StartWatchdog` so the supervisor
// can compose all eventtap teardown closures uniformly.
//
// Threading: MUST be invoked from the main goroutine. The returned `stop`
// closure MUST also be called on the main goroutine — both because
// NSWorkspace notificationCenter is main-thread-only and because Releaser
// already pins teardown via `cocoa.DispatchMain`.
//
// `log == nil` is tolerated — the function falls back to `slog.Default()`
// so callers that omit logging (smoke tests, early bring-up) do not nil-
// dereference inside the stop closure.
func InstallWakeObserver(tap unsafe.Pointer, log *slog.Logger) (stop func(), err error) {
	if log == nil {
		log = slog.Default()
	}
	if err := installWakeObserver(tap); err != nil {
		return nil, err
	}
	return func() {
		removeWakeObserver()
		log.Debug("wake observer removed")
	}, nil
}
