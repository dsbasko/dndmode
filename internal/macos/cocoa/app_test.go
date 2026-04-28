//go:build darwin

package cocoa

import (
	"context"
	"errors"
	"testing"

	_ "github.com/dsbasko/dndmode/internal/runtimepin"
)

// TestStopSubtype_MatchesDocReservation pins the subtype constant to the
// value documented in doc.go. If anyone changes one without the other,
// this test fails — preventing Phase 4/5 from accidentally colliding.
func TestStopSubtype_MatchesDocReservation(t *testing.T) {
	const wantPhase2 = 0xDED
	if stopSubtype != wantPhase2 {
		t.Errorf("stopSubtype = 0x%X, want 0x%X (Phase 2 reservation per doc.go)",
			stopSubtype, wantPhase2)
	}
}

// TestRunApp_BeforeInit_ReturnsErrNotInitialized verifies the WARNING
// guard: calling RunApp without prior successful Init() returns the
// ErrNotInitialized sentinel rather than crashing inside [NSApp run].
//
// CRITICAL ordering note: this test MUST run BEFORE any other test that
// invokes Init(); once initDone.Store(true) fires inside the package-level
// sync.Once, it stays true for the rest of the process and no further test
// can re-create the "before Init" state. Go's `testing` runs tests in
// source order within a single file; this test is placed FIRST among
// app_test.go tests for that reason. The "Test" prefix + alphabetical
// ordering is NOT relied upon — physical placement is.
//
// We use a context.Background() that we never cancel; the guard MUST
// short-circuit before any [NSApp run] invocation.
func TestRunApp_BeforeInit_ReturnsErrNotInitialized(t *testing.T) {
	if initDone.Load() {
		// Another test in this binary already invoked Init() — guard cannot
		// be tested in this run. Skip cleanly so we don't produce a false
		// positive (would block on [NSApp run] otherwise).
		t.Skip("initDone already true; this test must run before any Init() call (re-order tests if you see this)")
	}
	err := RunApp(context.Background())
	if !errors.Is(err, ErrNotInitialized) {
		t.Errorf("RunApp before Init: err = %v, want errors.Is ErrNotInitialized", err)
	}
}

// TestInit_Idempotent verifies the sync.Once guarantee: calling Init twice
// does not double-register observers.
//
// We cannot directly observe registerScreenObservers's call count without
// instrumentation, but we CAN verify that the second Init returns the same
// error value as the first (initErr is captured once inside sync.Once.Do).
//
// Note: this test mutates package-level state (initOnce). It runs before
// any smoke test that depends on a fresh cocoa state. Phase 2 design
// accepts this — Init is genuinely once-per-process.
func TestInit_Idempotent(t *testing.T) {
	// First call.
	err1 := Init(nil)
	// Second call must return the same value (whether nil or not).
	err2 := Init(nil)
	if err1 != err2 {
		t.Errorf("Init second call returned different error: first=%v second=%v",
			err1, err2)
	}
}

// TestInit_RegistersBothObservers verifies (dual-observer contract):
// cocoa.Init() per must register BOTH the NSNotificationCenter
// NSApplicationDidChangeScreenParameters observer AND the
// CGDisplayRegisterReconfigurationCallback. Phase 2 mandates
// the dual subscription — a single observer is insufficient to catch all
// hot-plug edge cases (fullscreen reconfigs miss NSNotif; CGDisplay can
// fire before [NSScreen screens] is updated).
//
// Implementation: testScreenRegisterCount() + testCGRegisterCount() (Go
// helpers added to screens_darwin.go in) read cumulative counters
// maintained as static int in screens_darwin.m. Counters increment exactly
// once per cocoa_screens_register_observers() success. Because Init() is
// guarded by sync.Once, AFTER the first Init() call both counters MUST
// equal exactly 1 (assuming no prior Init() in this test process — the
// counters never reset; we snapshot before+after to compute the delta and
// allow other tests in the same process to have already invoked Init).
//
// Run order matters: Go's test runner executes tests in source order by
// default within a single file. This test snapshots before its Init call
// and asserts a delta of exactly 1 for each counter (or 0 if a previous
// test already ran Init — sync.Once → no second registration).
func TestInit_RegistersBothObservers(t *testing.T) {
	beforeNS := testScreenRegisterCount()
	beforeCG := testCGRegisterCount()

	if err := Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	deltaNS := testScreenRegisterCount() - beforeNS
	deltaCG := testCGRegisterCount() - beforeCG

	// Two scenarios are valid:
	//   (a) This is the first Init() in the test process: both deltas == 1.
	//   (b) An earlier test already invoked Init(): sync.Once short-circuits,
	//       so both deltas == 0.
	// Either way the deltas MUST match each other (BOTH observers registered
	// or NEITHER) — that is the dual-observer guarantee.
	if deltaNS != deltaCG {
		t.Errorf("dual-observer guarantee violated:"+
			"NSNotif delta=%d, CGDisplay delta=%d (must be equal — both register or neither)",
			deltaNS, deltaCG)
	}
	if deltaNS != 0 && deltaNS != 1 {
		t.Errorf("NSNotif observer registered %d times in this Init call; want 0 (sync.Once) or 1 (first call)", deltaNS)
	}
	if deltaCG != 0 && deltaCG != 1 {
		t.Errorf("CGDisplay observer registered %d times in this Init call; want 0 (sync.Once) or 1 (first call)", deltaCG)
	}

	// Absolute counters MUST be >= 1 by now: SOME Init somewhere in the test
	// binary registered both observers exactly once (this test or earlier).
	if got := testScreenRegisterCount(); got < 1 {
		t.Errorf("testScreenRegisterCount = %d after Init(); want >= 1 (NSNotif observer never registered)", got)
	}
	if got := testCGRegisterCount(); got < 1 {
		t.Errorf("testCGRegisterCount = %d after Init(); want >= 1 (CGDisplay observer never registered)", got)
	}
}
