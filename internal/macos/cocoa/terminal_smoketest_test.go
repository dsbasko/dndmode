//go:build darwin && manual

package cocoa

import (
	"os"
	"testing"
	"time"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// TestSmoke_Terminal_CreateClose exercises the cgo wiring + lifecycle for the
// terminal overlay style: create a style="terminal" overlay window (which
// installs a TerminalView contentView that starts its ~30 FPS timer on
// window-attach), let at least one animation tick fire, then close it (which
// tears the view down, stopping + releasing the timer and freeing the buffers
// in viewWillMoveToWindow:nil / dealloc).
//
// Obj-C drawing cannot be asserted programmatically (the WindowServer owns the
// pixels), so this smoke only proves the terminal wiring + lifecycle do not
// crash and the handle round-trips. Real VISUAL validation is the manual run in
// the plan's Post-Completion (build dndmode with overlay_style: terminal, see
// the scrolling source + blinking caret + syntax colors on every screen,
// confirm clean teardown on hotkey exit).
//
// This test lives in package cocoa (NOT cocoa_test) so it can call the
// unexported createOverlayWindowStyled / closeOverlayWindow wrappers: cgo
// cannot be invoked directly from a _test.go file (Go toolchain limitation,
// see window_darwin.go), but a same-package test CAN call the Go wrappers that
// do the cgo internally.
//
// HEADLESS=1 -> t.Skip (smoke requires a GUI session / WindowServer). Off-main
// invocation also skips (skipUnlessMainThread, from window_smoketest_test.go).
func TestSmoke_Terminal_CreateClose(t *testing.T) {
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
			t.Fatalf("terminal create/close panicked: %v", r)
		}
	}()

	// Exercise every terminal language (each selects a different corpus table +
	// tokenizer syntax); "" is the bare-terminal Go default.
	for _, lang := range []string{"", "go", "python", "typescript", "rust"} {
		w, err := createOverlayWindowStyled(id, "terminal", 0, lang)
		if err != nil {
			t.Fatalf("createOverlayWindowStyled(%d, terminal:%q): %v", id, lang, err)
		}
		if w == nil {
			t.Fatalf("createOverlayWindowStyled(terminal:%q) returned nil handle without error", lang)
		}

		// Let at least one animation tick (~33ms at 30 FPS) fire before teardown.
		time.Sleep(150 * time.Millisecond)

		closeOverlayWindow(w) // must not panic; TerminalView stops+releases its timer.
	}
}
