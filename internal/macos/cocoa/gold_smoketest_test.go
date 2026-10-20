//go:build darwin && manual

package cocoa

import (
	"os"
	"testing"
	"time"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// TestSmoke_Gold_CreateClose exercises the cgo wiring + lifecycle for the gold
// overlay style: create a style="gold" overlay window (which installs a GoldView
// contentView that generates its candle history and starts its ~30 FPS timer on
// window-attach), let at least one animation tick fire, then close it (which tears
// the view down, stopping the timer in viewWillMoveToWindow:nil / dealloc and
// freeing the price state).
//
// Obj-C drawing cannot be asserted programmatically (the WindowServer owns the
// pixels), so this smoke only proves the gold wiring + lifecycle do not crash and
// the handle round-trips. Real VISUAL validation is the manual run (build dndmode
// with overlay_style: gold, see a full candle chart on every screen from the first
// frame, the newest candle forming at the right edge while the chart slides left,
// the price climbing, and confirm clean teardown on unlock).
//
// This test lives in package cocoa (NOT cocoa_test) so it can call the unexported
// createOverlayWindowStyled / closeOverlayWindow wrappers: cgo cannot be invoked
// directly from a _test.go file, but a same-package test CAN call the Go wrappers
// that do the cgo internally.
//
// HEADLESS=1 -> t.Skip (smoke requires a GUI session / WindowServer). Off-main
// invocation also skips (skipUnlessMainThread, from window_smoketest_test.go).
func TestSmoke_Gold_CreateClose(t *testing.T) {
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
			t.Fatalf("gold create/close panicked: %v", r)
		}
	}()

	w, err := createOverlayWindowStyled(id, "gold", 0, "")
	if err != nil {
		t.Fatalf("createOverlayWindowStyled(%d, gold): %v", id, err)
	}
	if w == nil {
		t.Fatalf("createOverlayWindowStyled returned nil handle without error")
	}

	// Let at least one animation tick (~33ms at 30 FPS) fire before teardown.
	time.Sleep(150 * time.Millisecond)

	closeOverlayWindow(w) // must not panic; GoldView stops its timer + frees its state.
}
