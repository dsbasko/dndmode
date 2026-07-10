//go:build darwin
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

// g_observed_tap is the canonical "current tap" pointer shared between the
// watchdog GCD block (this file) and the NSWorkspace wake-observer blocks
// (wake_darwin.m). The pointer is:
//
//   - WRITTEN from the main goroutine (via `eventtap_set_observed_tap`)
//     during Install (seed = real tap) and during Release Step 1 (NULL).
//   - READ from the GCD high-priority worker thread (watchdog handler) and
//     from `[NSOperationQueue mainQueue]` (wake / session-active blocks).
//
// `volatile` is REQUIRED because writer and reader live on different threads
// and the compiler must not cache the value in a register. On darwin/arm64
// an aligned pointer load/store is atomic per Apple's memory model — see
// "Multithreading Programming Guide" + libplatform/atomic.h notes; a single
// `volatile CFMachPortRef` is therefore sufficient for the cross-thread
// signal here (no __c11_atomic_load / OSAtomicCompareAndSwapPtr ceremony).
//
// Threat model: Release order per is:
//   Step 1: eventtap_enable(tap, false) + eventtap_set_observed_tap(NULL)
//   Step 2: CFRunLoopRemoveSource
//   Step 3: CFRelease(source) + CFRelease(tap)
//   Step 4: watchdog_stop (dispatch_source_cancel + drain)
//   Step 5: wake_observer_remove
// Between Step 1 and Step 4, the watchdog GCD handler may still be IN-FLIGHT
// (Apple's libdispatch does not synchronously drain handlers across
// `dispatch_source_cancel`). Without this guard, that in-flight handler
// would call `CGEventTapIsEnabled(g_tap)` AFTER Step 3 has released the
// mach port — UB. With this guard, the handler snapshots `g_observed_tap`
// at the top, sees NULL (because Step 1 wrote NULL atomically), and
// returns immediately. Race window closed without violating order.
//
// The same guard protects the wake-observer blocks (wake_darwin.m) against
// the symmetric race in the window between Step 1 and Step 5 — both
// blocks `extern` this same global. There is exactly one definition of
// `g_observed_tap` in the binary (here); wake_darwin.m declares `extern`.
volatile CFMachPortRef g_observed_tap = NULL;

// gesturetap_reenable lives in gesturetap_darwin.m (session-level gesture
// tap). Both the watchdog handler below and the wake-observer blocks
// (wake_darwin.m) call it right after their g_observed_tap NULL-guard so
// the gesture tap self-heals on the same cadence as the main tap. The
// gesture tap keeps no failure counter of its own: the silent-disable
// failure mode kills both taps together, and the main tap's counter is the
// exit signal.
extern void gesturetap_reenable(void);

// eventtap_set_observed_tap is the single writer for g_observed_tap.
// Called from Go (via cgo) at two moments:
//
//   - InstallAll Step (post wake-observer install): seed with the real tap.
//   - Release Step 1 (immediately after eventtap_enable(tap, 0)): write NULL.
//
// Plain volatile pointer store is atomic on darwin/arm64 (see g_observed_tap
// comment above). No barrier / no atomic intrinsic needed: readers only
// branch on (snapshot == NULL), and a stale non-NULL read at most causes
// one extra CGEventTapIsEnabled probe on a still-valid tap — safe.
void eventtap_set_observed_tap(CFMachPortRef tap) {
    g_observed_tap = tap;
}

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
        // guard (FIRST line — invariant):
        // snapshot g_observed_tap BEFORE any CG* call. The handler may
        // still be in-flight on the GCD HIGH queue after Release Step 1
        // wrote NULL but before Step 4's dispatch_source_cancel drained
        // pending invocations. If we see NULL → no-op; otherwise we hold
        // a local snapshot that remains valid for the lifetime of this
        // block (Release Step 4 waits for cancel to drain handlers, and
        // CFRelease at Step 3 cannot happen before Step 4 because the
        // Releaser.mu mutex serialises the whole chain).
        //
        // Volatile read here pairs with the volatile write in
        // `eventtap_set_observed_tap` — both ends documented in the
        // g_observed_tap declaration comment above.
        CFMachPortRef tap_snap = g_observed_tap;
        if (tap_snap == NULL) {
            return;
        }

        // Heal the session-level gesture tap on the same probe cadence.
        // Placed AFTER the guard above so a Release in progress (NULL
        // written at Step 1) also suppresses late gesture re-enables.
        // Idempotent no-op when the gesture tap is healthy.
        gesturetap_reenable();

        // Legacy defensive null check on the captured `tap` parameter is
        // now subsumed by the snapshot above — the captured `tap` was
        // never nil'd from Go side (CFMachPortRef passed by value at
        // create time), so this check would always pass. Kept as a
        // belt-and-suspenders cheap branch for future readers who may
        // re-introduce dynamic `tap` rebinding.
        (void)tap; // silence unused-warning if future refactors drop the capture

        // healthy probe → reset counter. This covers BOTH the normal
        // case AND (kCGEventTapDisabledByUserInput re-enable performed
        // by the tap's own inline callback in).
        //
        // All CG* calls below use `tap_snap` (the snapshot), NOT the
        // captured `tap` parameter — the snapshot is the post-guard
        // single source of truth.
        if (CGEventTapIsEnabled(tap_snap)) {
            g_fail_count = 0;
            return;
        }

        // Tap is disabled — attempt to re-enable.
        CGEventTapEnable(tap_snap, true);

        // Re-probe. If re-enable succeeded → also a healthy state, reset.
        if (CGEventTapIsEnabled(tap_snap)) {
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

    // Seed g_observed_tap BEFORE dispatch_resume so the very first handler
    // fire (5s from now per dispatch_source_set_timer above) sees a
    // non-NULL snapshot. InstallAll also explicitly calls
    // `eventtap_set_observed_tap(tap)` after wake_observer_install for
    // belt-and-suspenders (idempotent — re-sets the same value); this
    // line keeps watchdog_start self-sufficient for the existing
    // smoke-test path and for any future caller that exercises
    // the watchdog without going through InstallAll.
    g_observed_tap = tap;

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

// fix: the test-only `watchdog_test_get_fail_count`
// getter was removed from this production binary in iteration 2 of the
// Phase 4 code-review fix loop, mirroring the removal of
// the sibling `eventtap_test_set_expected` helper.
//
// Rationale (same as): the function had ZERO Go-side callers, ZERO
// `_test.go` references, and ZERO `.m`-side callers — repo-wide grep at
// a later review confirmed it was dead in both the production AND the
// test binary. Phase 3 explicitly acknowledged this:
// "Not used yet — pure-Go DI seam (`watchdogState.Probe`) already covers
// unit-test acceptance."
//
// Attack surface was smaller than the sibling (read-only — an
// injector could observe `g_fail_count` but not corrupt the watchdog
// state), but the same "correctness/security > size" rationale applies:
// dead test-only code in a production binary serves no purpose and
// widens the symbol table available to a process-injection adversary.
//
// If a future smoke test wants to observe `g_fail_count` end-to-end, it
// should be added in a `*_darwin_test.m` companion file with `//go:build
// manual` parity (the build-tag-gated alternative proposed in 's
// "alternative" fix), not by reviving this dead note.
