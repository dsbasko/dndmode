//go:build darwin

package cocoa

import (
	"os"
	"testing"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// firstAttachedDisplayID is a test-local alias for
// firstAttachedDisplayIDForTest (defined in window_darwin.go). It exists so
// the smoke tests below read naturally and so a future replacement (e.g. the
// production-grade enumerateScreens() landing in) can be wired in
// by editing this single helper.
func firstAttachedDisplayID() (uint32, bool) {
	return firstAttachedDisplayIDForTest()
}

// skipUnlessMainThread t.Skip's the test if the current OS thread is not the
// main (m0) thread. NSWindow's contract is that it MUST be allocated on the
// main thread, otherwise AppKit aborts the entire process via
// NSInternalInconsistencyException — which would corrupt the test binary
// before any further test could run. A live round-trip smoke that actually
// creates an NSWindow needs the DispatchMain helper from plus
// NSApp.run() from to route work to the main thread; both arrive
// in /2 and the controller-level smoke in covers that path
// end-to-end. The smoke tests in this plan validate the cgo bridging surface
// (compilation, error path, nil-safety, HEADLESS skip, runtime
// collectionBehavior bitmask) and skip cleanly when invoked off-main.
func skipUnlessMainThread(t *testing.T) {
	t.Helper()
	if !isMainThreadForTest() {
		t.Skip("smoke test requires main thread; routed via DispatchMain in RunApp in; controller-level smoke covers live path in")
	}
}

// TestSmoke_NSWindow_CreateClose_Single validates (low-level layer):
// alloc + close roundtrip on a single displayID, asserting:
//   - createOverlayWindow returns non-nil handle, no error
//   - windowLevel returns CGShieldingWindowLevel-equivalent value (large
//     positive integer; we cannot import the constant from Go — instead
//     assert > NSScreenSaverWindowLevel (1000) which is the minimum we'd
//     accept; CGShieldingWindowLevel is computed ~2147483628)
//   - windowIsVisible returns true (orderFrontRegardless took effect)
//   - closeOverlayWindow does not panic and (smoke heuristic) reduces
//     visibility on subsequent isVisible-style tools (we don't probe again
//     post-close because the handle is dangling per __bridge_transfer
//     semantics — Go side just stops using it)
//
// HEADLESS=1 env triggers t.Skip (cgo smoke requires WindowServer / GUI).
// Off-main goroutine invocation also triggers t.Skip (see skipUnlessMainThread).
func TestSmoke_NSWindow_CreateClose_Single(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	skipUnlessMainThread(t)
	id, ok := firstAttachedDisplayID()
	if !ok {
		t.Skip("no displays attached")
	}

	w, err := createOverlayWindow(id)
	if err != nil {
		t.Fatalf("createOverlayWindow(%d): %v", id, err)
	}
	if w == nil {
		t.Fatalf("createOverlayWindow returned nil handle without error")
	}

	const minShieldLevel = 1000 // NSScreenSaverWindowLevel; shield must be >= this
	if got := windowLevel(w); got < minShieldLevel {
		t.Errorf("windowLevel = %d, want >= %d (CGShieldingWindowLevel)", got, minShieldLevel)
	}

	if !windowIsVisible(w) {
		t.Errorf("windowIsVisible = false, want true (orderFrontRegardless)")
	}

	// Close — must not panic; calling on the same handle twice is undefined
	// (we test nil-safety separately, not double-close-of-valid-handle).
	closeOverlayWindow(w)
}

// TestSmoke_NSWindow_NilClose_NoOp ensures closeOverlayWindow with nil is
// a silent no-op (Go-side guard + C-side guard). Safe on any thread.
func TestSmoke_NSWindow_NilClose_NoOp(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("closeOverlayWindow(nil) panicked: %v", r)
		}
	}()
	closeOverlayWindow(nil)
}

// TestSmoke_NSWindow_BadDisplayID_ReturnsError verifies the failure path:
// passing a displayID that doesn't match any NSScreen returns a non-nil
// error with a meaningful message ("no NSScreen matches displayID").
//
// Reading [NSScreen screens] is documented thread-safe (Apple docs:
// "Most of the methods that retrieve information... are safe to call from
// any thread"), and we never reach NSWindow alloc on the bogus path, so
// this test does not require the main thread.
func TestSmoke_NSWindow_BadDisplayID_ReturnsError(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	const bogusID uint32 = 0xFFFFFFFF
	w, err := createOverlayWindow(bogusID)
	if err == nil {
		if w != nil {
			closeOverlayWindow(w)
		}
		t.Fatalf("createOverlayWindow(bogus=0x%X) returned nil error", bogusID)
	}
	if w != nil {
		t.Errorf("createOverlayWindow returned non-nil handle along with error: %v", err)
		closeOverlayWindow(w)
	}
}

// TestSmoke_NSWindow_CollectionBehavior validates collectionBehavior
// at runtime (WARNING fix): the NSWindow MUST have all four required
// flags set after createOverlayWindow returns. Without runtime verification
// only a grep on the .m source proves the constants are present — but a
// future refactor could accidentally clear them or use OR-replace assignment
// in the wrong order.
//
// Required bitmask (NSWindow.h verified, AppKit constants):
//
//	NSWindowCollectionBehaviorCanJoinAllSpaces    = 1 << 0  = 0x001
//	NSWindowCollectionBehaviorStationary          = 1 << 4  = 0x010
//	NSWindowCollectionBehaviorIgnoresCycle        = 1 << 6  = 0x040
//	NSWindowCollectionBehaviorFullScreenAuxiliary = 1 << 8  = 0x100
//	REQUIRED = 0x151 = 337
//
// We assert via bitwise AND that ALL four required bits are set; we do NOT
// assert exact equality because AppKit may set additional flags internally
// (e.g. defaults vary between macOS versions) — the contract is "AT LEAST
// these four", not "EXACTLY these four".
//
// Requires the main thread (NSWindow alloc).
func TestSmoke_NSWindow_CollectionBehavior(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	skipUnlessMainThread(t)
	id, ok := firstAttachedDisplayID()
	if !ok {
		t.Skip("no displays attached")
	}

	w, err := createOverlayWindow(id)
	if err != nil {
		t.Fatalf("createOverlayWindow(%d): %v", id, err)
	}
	defer closeOverlayWindow(w)

	const (
		canJoinAllSpaces    uint64 = 1 << 0 // 0x001
		stationary          uint64 = 1 << 4 // 0x010
		ignoresCycle        uint64 = 1 << 6 // 0x040
		fullScreenAuxiliary uint64 = 1 << 8 // 0x100
		required            uint64 = canJoinAllSpaces | stationary | ignoresCycle | fullScreenAuxiliary // 0x151
	)

	got := windowCollectionBehavior(w)
	if got&required != required {
		missing := required &^ got
		t.Errorf("collectionBehavior = 0x%X; required bits 0x%X missing (full required mask 0x%X). "+
			"demands all 4 flags: CanJoinAllSpaces|Stationary|IgnoresCycle|FullScreenAuxiliary",
			got, missing, required)
	}
}
