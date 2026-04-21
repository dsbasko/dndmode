//go:build darwin

// Package cocoa wraps NSApplication + per-screen NSWindow overlays via cgo.
// It is the single entry point to AppKit/Cocoa/Quartz from the rest of the
// codebase; all Phase 2-5 system-level GUI work routes through here.
//
// # Threading invariants (CRITICAL)
//
//   - Init, RunApp, controller.CreateWindowsForAllScreens MUST be called from
//     the main goroutine. The main goroutine is locked to OS thread #0 by
//     internal/runtimepin/init() (blank-imported first in cmd/dndmode/main.go).
// - All NSWindow ops route through DispatchMain (Phase 2) which detects
//     via pthread_main_np() whether the caller is already on main; inline if
//     so, else dispatch_async to dispatch_get_main_queue().
//   - [NSApp postEvent:atStart:] and [NSApp stop:] are documented thread-safe
//     (Apple "Threading Programming Guide" + NSApplication.h:352) and may be
// called from any goroutine; the ctx-watcher goroutine uses this.
//
// # Synthetic NSEventTypeApplicationDefined subtype reservation table
//
// Each phase that posts synthetic stop/wake events MUST use a distinct
// subtype to avoid cross-phase collisions when multiple subsystems become
// concurrent (Phase 4 hotkey path + Phase 2 ctx-cancel path coexist).
//
//	Phase 2 (overlay stop):  0xDED
//	Phase 4 (hotkey match):  0xDF1   // reserved, not yet used
//	Phase 5 (focus restore): 0xDF5   // reserved, not yet used
//
// # Sources
//
// - the design notes
// - the design notes (.. decisions)
// - the design notes "Cocoa/AppKit Bridging"
package cocoa
