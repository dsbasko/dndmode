//go:build darwin

package eventtap

/*
#cgo CFLAGS: -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Foundation -framework CoreGraphics

#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

extern int  watchdog_start(CFMachPortRef tap);
extern void watchdog_stop(void);
*/
import "C"

import (
	"sync/atomic"
	"unsafe"
)

// watchdogFailThreshold mirrors `FAIL_THRESHOLD` in watchdog_darwin.m
// (5 consecutive `CGEventTapIsEnabled == false` probes at 5s cadence
// = 25s wall-clock before the watchdog declares the tap dead and signals
// the supervisor via the sink channel).
//
// Kept as a package-level untyped const so the pure-Go DI seam tests in
// watchdog_test.go can reference it (and the C side reads the equivalent
// `static const int` literal — both sources of truth are explicitly
// pinned to 5 with a cross-reference comment).
const watchdogFailThreshold = 5

// watchdogThresholdHit is the package-level latch flipped by the //export
// Go helper eventtap_watchdog_failed, which is invoked from the C-side
// GCD timer block (watchdog_darwin.m) when the consecutive-failure counter
// reaches FAIL_THRESHOLD. wires the C side; the poller
// goroutine (Wave 1 04-02) reads this latch alongside `matched` and, on
// true, forwards ErrWatchdogExitThreshold through the sink channel.
//
// As with `matched`, atomic.Bool is the only storage primitive
// permitted in the //export callback body per nosplit invariant.
var watchdogThresholdHit atomic.Bool

// eventtap_watchdog_failed is the cgo entry point invoked from the GCD
// timer block in watchdog_darwin.m when the consecutive-failure counter
// reaches the threshold. It fires on the GCD high-priority dispatch queue
//, NOT main and NOT a Go-scheduled goroutine — so the body MUST
// satisfy the same nosplit invariant as eventtap_matched: a single atomic
// store, nothing else.
//
//export eventtap_watchdog_failed
func eventtap_watchdog_failed() {
	watchdogThresholdHit.Store(true)
}

// watchdogState is the pure-Go DI seam for the consecutive-failure
// counter policy. It is intentionally NOT
// goroutine-safe: in production it is touched ONLY from the GCD timer
// block (single serial queue per dispatch_source_t — Apple guarantees no
// concurrent invocation), and in unit tests it is touched ONLY from the
// test goroutine. A concurrent caller would be a programming error, not
// a runtime condition.
//
// The seam exists to satisfy the Phase 4 validation requirement that
// be unit-testable without standing up cgo / GCD / a live
// CGEventTap. The C side (watchdog_darwin.m) implements the same
// arithmetic in static-int form; smoke-test in verifies the
// two stay in sync.
//
// Field `failCount` is unexported per Phase 4 plan acceptance criteria —
// callers MUST go through Probe.
type watchdogState struct {
	// failCount is the number of consecutive `Probe(false)` calls since
	// the most recent `Probe(true)` (or since zero-value init). Resets
	// to 0 on any healthy probe.
	failCount int

	// thresholdHit is the one-shot latch — true once Probe has returned
	// `threshold=true` for the first time. Subsequent calls return
	// `(false, false)` regardless of input so the watchdog signal stays
	// idempotent (Test 3: TestWatchdog_Threshold_Idempotent).
	thresholdHit bool
}

// Probe records a single watchdog cycle and returns the (reset, threshold)
// tuple. Semantics from:
//
//   - If `thresholdHit` is already true → return `(false, false)`
//     unconditionally. Idempotent contract; further pumping a dead tap
//     must not enqueue duplicate ErrWatchdogExitThreshold signals.
//
//   - Else if `isEnabled` is true → reset `failCount` to 0, return
// `(true, false)`. Mirrors "UserInput disable is normal" — any
//     healthy probe clears state.
//
//   - Else (a failure) → increment `failCount`. If it reaches
//     `watchdogFailThreshold` (== 5), set `thresholdHit` and return
//     `(false, true)`. Otherwise return `(false, false)`.
//
// The seam is consumed by 's GCD timer callback, which:
//
//  1. Probes `CGEventTapIsEnabled(tap)`.
//  2. If false, calls `CGEventTapEnable(tap, true)` and probes again.
//  3. Calls `state.Probe(isEnabledAfterReenable)`.
//  4. If returned `threshold=true` → invokes `eventtap_watchdog_failed()`
//     (the //export above) and `dispatch_source_cancel(g_watchdog)`.
func (s *watchdogState) Probe(isEnabled bool) (reset bool, thresholdReached bool) {
	if s.thresholdHit {
		// One-shot contract — already signalled, stay silent.
		return false, false
	}
	if isEnabled {
		s.failCount = 0
		return true, false
	}
	s.failCount++
	if s.failCount >= watchdogFailThreshold {
		s.thresholdHit = true
		return false, true
	}
	return false, false
}

// startWatchdog wires the Go-side cgo bridge into `watchdog_start`
// (watchdog_darwin.m), which creates the GCD timer source + event
// handler. Wave 1 04-03 fills the C-side body; this Go side stays as a
// thin pass-through. Returns nil on success; non-nil error if the C side
// reports a GCD allocation failure (in practice GCD does not fail under
// normal load — the return type is preserved for symmetry with the rest
// of the package).
//
// MUST be called from the main goroutine because the C side queries
// `dispatch_get_global_queue(...)` which is thread-safe but the run-loop
// invariants for the broader Install path require main-thread setup.
//
// Wave 0 stub: returns nil. Wave 1 04-03 replaces the body.
func startWatchdog(tap unsafe.Pointer) error {
	_ = tap
	// Reference the cgo binding so dead-code elimination keeps the C
	// symbol linked through Wave 0 even before Wave 1 wires production
	// install logic.
	if false {
		var t C.CFMachPortRef
		_ = C.watchdog_start(t)
	}
	return nil
}

// stopWatchdog wires the Go-side cgo bridge into `watchdog_stop`
// (watchdog_darwin.m), which cancels and nils the dispatch_source_t.
// Called by Releaser.Release as part of the LIFO teardown chain.
//
// Idempotent — safe to call when no watchdog has been started.
//
// Wave 0 stub: no-op.
func stopWatchdog() {
	if false {
		C.watchdog_stop()
	}
}
