//go:build darwin

package cocoa

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework CoreGraphics -framework Foundation -framework ApplicationServices

extern void cocoa_hide_cursor(void);
extern void cocoa_show_cursor(void);
*/
import "C"

// hideCursor hides the system mouse cursor while the overlay is active via
// CGDisplayHideCursor(CGMainDisplayID()) (see cursor_darwin.m for the decided
// mechanism rationale). Cosmetic only: removes the stray arrow the
// WindowServer draws on top of the black shield. Calling the hide/show
// roundtrip does not panic; visibility cannot be asserted programmatically.
func hideCursor() { C.cocoa_hide_cursor() }

// showCursor restores the system mouse cursor on overlay teardown, balancing
// a prior hideCursor. Wraps CGDisplayShowCursor(CGMainDisplayID()).
func showCursor() { C.cocoa_show_cursor() }

// cursorHider is the DI seam for the active-overlay cursor hide. Production
// uses cgoCursorHider (CoreGraphics display-level hide); tests inject a fake
// that counts Hide/Show calls so Controller wiring is verifiable without a
// GUI session. Mirrors the screenEnumerator / windowFactory /
// observerRegistrar / mainDispatcher seams in controller_darwin.go.
type cursorHider interface {
	Hide()
	Show()
}

// cgoCursorHider is the production implementation backed by hideCursor /
// showCursor (cursor_darwin.m).
type cgoCursorHider struct{}

func (cgoCursorHider) Hide() { hideCursor() }
func (cgoCursorHider) Show() { showCursor() }
