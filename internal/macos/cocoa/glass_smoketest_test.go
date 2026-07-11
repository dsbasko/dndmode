//go:build darwin && manual

package cocoa

import (
	"os"
	"testing"
	"time"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// TestSmoke_Glass_CreateClose exercises the cgo wiring + lifecycle for the glass
// overlay style: create a style="glass" overlay window (which makes the window
// non-opaque and installs an NSVisualEffectView behind-window blur contentView),
// let it settle, then close it.
//
// Obj-C drawing cannot be asserted programmatically (the WindowServer owns the
// pixels), so this smoke only proves the glass wiring + lifecycle do not crash
// and the handle round-trips. Real VISUAL validation is the manual run (build
// dndmode with overlay_style: glass, confirm the desktop shows through frosted /
// blurred on every screen and a clean teardown on hotkey exit).
//
// Lives in package cocoa (NOT cocoa_test) so it can call the unexported
// createOverlayWindowStyled / closeOverlayWindow Go wrappers (cgo cannot be
// invoked directly from a _test.go file; a same-package test calls the wrappers
// that do the cgo internally).
//
// HEADLESS=1 -> t.Skip (smoke requires a GUI session / WindowServer). Off-main
// invocation also skips (skipUnlessMainThread, from window_smoketest_test.go).
func TestSmoke_Glass_CreateClose(t *testing.T) {
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
			t.Fatalf("glass create/close panicked: %v", r)
		}
	}()

	w, err := createOverlayWindowStyled(id, "glass", 16)
	if err != nil {
		t.Fatalf("createOverlayWindowStyled(%d, glass): %v", id, err)
	}
	if w == nil {
		t.Fatalf("createOverlayWindowStyled returned nil handle without error")
	}

	time.Sleep(150 * time.Millisecond)

	closeOverlayWindow(w) // must not panic.
}
