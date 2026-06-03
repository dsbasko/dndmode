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

// watchdog_start creates a GCD timer source with 5s interval +
// 1s leeway and installs the timer block. Wave 1 04-03 implements:
//
//   1. Get a HIGH-priority queue: `dispatch_queue_t q = dispatch_get_global_queue(
//      DISPATCH_QUEUE_PRIORITY_HIGH, 0);`.
//   2. Create timer: `g_watchdog = dispatch_source_create(
//      DISPATCH_SOURCE_TYPE_TIMER, 0, 0, q);`.
//   3. Set timer params: 5s interval, 1s leeway, start at +5s.
//   4. Set event handler: probe `CGEventTapIsEnabled(tap)`; if false →
//      `CGEventTapEnable(tap, true)`, then probe again; if still false →
//      `g_fail_count++`; on threshold → call `eventtap_watchdog_failed()`
//      and `dispatch_source_cancel(g_watchdog)`. On healthy probe →
//      `g_fail_count = 0`.
//   5. `dispatch_resume(g_watchdog)`.
//
// Returns 0 on success. The unit-test side of D-09 policy is fully covered
// by the pure-Go `watchdogState` DI seam — this C implementation only
// repeats the same arithmetic; smoke-test in 04-03 verifies the cgo binding.
//
// Wave 0 is a no-op.
int watchdog_start(CFMachPortRef tap) {
    (void)tap;
    return 0;
}

// watchdog_stop tears down the GCD timer. Wave 1 04-03 implements:
//
//   1. `if (g_watchdog) { dispatch_source_cancel(g_watchdog); g_watchdog = nil; }`.
//      Setting to `nil` (NOT calling manual GCD-source release) is the
//      correct ARC teardown for `dispatch_source_t` — Pitfall 2.
//   2. Reset `g_fail_count = 0` so a hypothetical re-Install starts fresh.
//
// Idempotent — safe to call when `g_watchdog == nil` (e.g. Release-twice
// races coming from the two-layer guard).
//
// Wave 0 is a no-op.
void watchdog_stop(void) {
}
