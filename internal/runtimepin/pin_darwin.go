//go:build darwin

// Package runtimepin pins the main goroutine to OS thread #0 (m0) so that
// AppKit/NSApplication.run() (Phase 2+) can run on the process startup thread.
//
// MUST be imported via blank import from cmd/dndmode/main.go BEFORE any other
// package that may touch Cocoa. Removing the import silently breaks Phase 2.
//
// Rationale: runtime.LockOSThread() pins a goroutine to its current OS-thread.
// For main goroutine to be pinned to thread #0 (m0), the call MUST happen in
// an init() function of an imported package — at that point all init routines
// run on the main thread, so locking pins main's goroutine for the lifetime
// of the process.
//
// Sources:
//   - https://github.com/golang/go/issues/23112
//   - https://go.dev/wiki/LockOSThread
package runtimepin

import "runtime"

func init() {
	runtime.LockOSThread()
}
