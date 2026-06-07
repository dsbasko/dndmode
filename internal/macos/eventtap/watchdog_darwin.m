// +build darwin

#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <dispatch/dispatch.h>
#import "_cgo_export.h"  // generated header for //export eventtap_watchdog_failed

// FAIL_THRESHOLD is the consecutive-failure ceiling.: 5 consecutive
// `CGEventTapIsEnabled == false` probes (5 × 5s = 25s wall-clock). Healthy
// probes reset the counter. Kept as a C `#define`-equivalent `const int`
// so the value is grep-able from both Go and C without preprocessor games.
// The mirror constant on the Go side (`watchdogFailThreshold = 5`) is
// referenced by the pure-Go DI seam's tests in this plan.
static const int FAIL_THRESHOLD = 5;

// g_watchdog is the GCD timer source.: high-priority dispatch queue
// (`DISPATCH_QUEUE_PRIORITY_HIGH`) to keep the 5s probe cadence even under
// process pressure. Created in `watchdog_start`, cancelled and released in
// `watchdog_stop`. ARC manages the `dispatch_source_t` lifetime
// PROHIBITS explicit manual-release of GCD sources because the package
// compiles with `-fobjc-arc` (see Go-side `#cgo CFLAGS`). Manual release
// of a GCD source under ARC is a compile error; teardown is `nil`-assignment
// + `dispatch_source_cancel` ONLY.
static dispatch_source_t g_watchdog = nil;

// g_fail_count is the consecutive-failure counter. Manipulated ONLY from
// the GCD timer block (which is serially dispatched on a single GCD queue
// per source), so no atomic primitive is required. The pure-Go DI seam
// (`watchdogState` in `watchdog_darwin.go`) is the canonical policy
// implementation tested by unit tests; the C copy here is kept minimal
// because the GCD block has no Go-side allocation budget.
static int g_fail_count = 0;

// watchdog_start creates a GCD timer source on DISPATCH_QUEUE_PRIORITY_HIGH
// with a 5s interval + 500ms leeway and installs the event handler
// block that probes `CGEventTapIsEnabled(tap)` every cycle.
//
// Handler policy:
//   1. Defensive `tap == NULL` guard — if Release has nil'd the tap port
//      between cancel and the in-flight handler invocation, bail.
// 2. If `CGEventTapIsEnabled(tap)` → reset `g_fail_count = 0` (
// any healthy probe resets the counter, including UserInput
//      disables that the tap's own inline callback re-enabled).
//   3. Otherwise: `CGEventTapEnable(tap, true)` + re-probe. If the re-enable
//      succeeded → also reset counter.
//   4. Only if re-enable FAILED → `g_fail_count++`. On reaching
// `FAIL_THRESHOLD` (5 consecutive failures = 25s wall-clock per),
// invoke the Go-exported watchdog-failed helper (from) whose
//      body is exactly `watchdogThresholdHit.Store(true)`. The Go-side
//      poller (`pollWatchdogThreshold`) reads that atomic and forwards a
//      single sink-send + stderr log; re-invocation is safe because the
//      poller's `CompareAndSwap(true, false)` is single-shot.
//
// Lifecycle invariants per the design notes + /3:
//   - `dispatch_source_create` returns a SUSPENDED source. `dispatch_resume`
//     is MANDATORY before the first fire. Releasing a suspended source is
// undefined behavior (EXC_BAD_INSTRUCTION) —.
//   - Under `-fobjc-arc` (see watchdog_darwin.go `#cgo CFLAGS`),
// manual GCD-object release is FORBIDDEN —. Teardown is
//     `dispatch_source_cancel` + nil-assignment.
//
// Return codes:
//   0 = success
//   1 = tap parameter is NULL
//   2 = watchdog already started (caller must `watchdog_stop` first)
//   3 = `dispatch_source_create` returned NULL (GCD out of sources)
int watchdog_start(CFMachPortRef tap) {
    if (tap == NULL) {
        return 1;
    }
    if (g_watchdog != nil) {
        // Defensive: a healthy caller (Go-side `startWatchdog`) must call
        // `watchdog_stop` before re-arming. Surface as rc=2 rather than
        // leaking the existing source.
        return 2;
    }

    // any fresh start begins with a clean counter.
    g_fail_count = 0;

    // HIGH-priority global queue — watchdog probe must run even
    // under process load (otherwise Apple's silent-disable race window
    // widens beyond the 25s budget).
    dispatch_queue_t q = dispatch_get_global_queue(DISPATCH_QUEUE_PRIORITY_HIGH, 0);

    g_watchdog = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, q);
    if (g_watchdog == nil) {
        return 3;
    }

    // the design notes: 5s interval + 500ms leeway (10% of period — Apple's
    // documented recommendation for power efficiency on non-critical
    // periodic timers). First fire at NOW + 5s — we don't probe immediately
    // because Install just succeeded and the tap is known-good.
    dispatch_source_set_timer(g_watchdog,
        dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC),
        5 * NSEC_PER_SEC,
        500 * NSEC_PER_MSEC);

    dispatch_source_set_event_handler(g_watchdog, ^{
        // Defensive null check: the source is cancelled before this block
        // can be torn down, but a fire that was already enqueued may still
        // run. The tap pointer was captured by value at create-time, so
        // it cannot be re-nil'd from Go-side — but Release-during-fire is
        // still possible, in which case CGEventTapIsEnabled on a freed
        // port is UB. We guard via the cancel-then-nil sequence in
        // `watchdog_stop`.
        if (tap == NULL) {
            return;
        }

        // healthy probe → reset counter. This covers BOTH the normal
        // case AND (kCGEventTapDisabledByUserInput re-enable performed
        // by the tap's own inline callback in).
        if (CGEventTapIsEnabled(tap)) {
            g_fail_count = 0;
            return;
        }

        // Tap is disabled — attempt to re-enable.
        CGEventTapEnable(tap, true);

        // Re-probe. If re-enable succeeded → also a healthy state, reset.
        if (CGEventTapIsEnabled(tap)) {
            g_fail_count = 0;
            return;
        }

        // Re-enable failed: this is the counter-incrementing path.
        g_fail_count++;
        if (g_fail_count >= FAIL_THRESHOLD) {
            // signal Go-side latch. The exported function's body is
            // exactly `watchdogThresholdHit.Store(true)` (fixed
            // invariant) — safe to call from a
            // GCD worker thread because atomic.Store is nosplit.
            //
            // Idempotent: subsequent fires keep the latch true; the Go
            // poller's `CompareAndSwap(true, false)` ensures exactly one
            // sink-send + one log line per threshold-trip.
            eventtap_watchdog_failed();
        }
    });

    // the design notes step 4 — MANDATORY before first fire.: an
    // unresumed source cannot be safely released; always resume before
    // storing the source in a global that may be torn down.
    dispatch_resume(g_watchdog);

    return 0;
}

// watchdog_stop tears down the GCD timer source. Idempotent — safe to call
// when no watchdog is currently active (e.g. Install failed before
// watchdog_start, or Release runs twice via the two-layer guard).
//
// Teardown sequence (the design notes step 5):
//   1. `dispatch_source_cancel` — stops the timer; the event handler will
//      NOT fire again after this returns. (An in-flight handler invocation
//      may still complete on the GCD queue — the tap-pointer defensive
//      null check in the handler covers this race.)
//   2. `g_watchdog = nil` — releases the strong ARC reference, allowing
//      libdispatch to deallocate the source. Under `-fobjc-arc`, manual
// GCD-object release is FORBIDDEN (compile error) —.
//   3. `g_fail_count = 0` — reset state so a subsequent `watchdog_start`
//      in the same process (e.g. a test fixture) begins fresh.
void watchdog_stop(void) {
    if (g_watchdog == nil) {
        return;
    }
    dispatch_source_cancel(g_watchdog);
    // ARC: nil-assignment releases the strong ref. NO manual GCD release.
    g_watchdog = nil;
    g_fail_count = 0;
}

// watchdog_test_get_fail_count exposes the C-side counter for cgo smoke
// tests (build-tag gated, not part of the default `go test` run). Not
// declared in the Go-side cgo preamble — accessed only via test-only
// extern declarations in `*_smoketest_test.go` files. Wave 1 04-03
// includes it so a future smoke test can observe counter behavior end
// to end without instrumenting the dispatch block.
int watchdog_test_get_fail_count(void) {
    return g_fail_count;
}
