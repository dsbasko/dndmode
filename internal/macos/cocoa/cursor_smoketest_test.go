//go:build darwin && manual

package cocoa

import (
	"os"
	"testing"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// TestSmoke_Cursor_HideShow_Roundtrip exercises the cgo call path for the
// active-overlay cursor hide: hideCursor() immediately followed by
// showCursor(), asserting only that neither panics.
//
// Cursor VISIBILITY cannot be asserted programmatically (the WindowServer
// owns the actual pointer state), so this smoke only proves the cgo wiring
// is correct and non-crashing. Real visual validation is the manual run in
// the success criteria (build dndmode, see the arrow vanish under
// the black shield, reappear on unlock) — which also validates the
// NSApplicationActivationPolicyProhibited hypothesis the unit tests (using a
// fake) cannot.
//
// This test lives in package cocoa (NOT cocoa_test) because it must call the
// unexported hideCursor/showCursor wrappers: cgo cannot be invoked directly
// from a _test.go file (Go toolchain limitation, see window_darwin.go), but a
// same-package test CAN call Go wrappers that do the cgo internally.
//
// HEADLESS=1 → t.Skip (smoke requires a GUI session). showCursor() is called
// LAST so a real GUI run never strands a hidden cursor.
func TestSmoke_Cursor_HideShow_Roundtrip(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cursor roundtrip panicked: %v", r)
		}
	}()
	hideCursor()
	showCursor() // leave the cursor SHOWN at the end.
}
