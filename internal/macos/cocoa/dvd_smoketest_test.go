//go:build darwin && manual

package cocoa

import (
	"os"
	"testing"
	"time"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// TestSmoke_DVD_CreateClose exercises the cgo wiring + lifecycle for the dvd
// overlay style: create a style="dvd" overlay window (which installs a DVDView
// contentView that builds its logo paths and starts its ~30 FPS timer on
// window-attach), let at least one animation tick fire, then close it (which tears
// the view down, stopping the timer in viewWillMoveToWindow:nil / dealloc and
// releasing the two CGPaths).
//
// Obj-C drawing cannot be asserted programmatically (the WindowServer owns the
// pixels), so this smoke only proves the dvd wiring + lifecycle do not crash and
// the handle round-trips. Real VISUAL validation is the manual run (build dndmode
// with overlay_style: dvd, see the DVD-VIDEO logo bounce on every screen, change
// color on each edge hit, flash on a corner, and confirm clean teardown on hotkey
// exit).
//
// This test lives in package cocoa (NOT cocoa_test) so it can call the unexported
// createOverlayWindowStyled / closeOverlayWindow wrappers: cgo cannot be invoked
// directly from a _test.go file, but a same-package test CAN call the Go wrappers
// that do the cgo internally.
//
// HEADLESS=1 -> t.Skip (smoke requires a GUI session / WindowServer). Off-main
// invocation also skips (skipUnlessMainThread, from window_smoketest_test.go).
func TestSmoke_DVD_CreateClose(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	skipUnlessMainThread(t)
	id, ok := firstAttachedDisplayID()
	if !ok {
		t.Skip("no displays attached")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("dvd create/close panicked: %v", r)
		}
	}()

	w, err := createOverlayWindowStyled(id, "dvd", 0, "")
	if err != nil {
		t.Fatalf("createOverlayWindowStyled(%d, dvd): %v", id, err)
	}
	if w == nil {
		t.Fatalf("createOverlayWindowStyled returned nil handle without error")
	}

	// Let at least one animation tick (~33ms at 30 FPS) fire before teardown.
	time.Sleep(150 * time.Millisecond)

	closeOverlayWindow(w) // must not panic; DVDView stops its timer + releases paths.
}
