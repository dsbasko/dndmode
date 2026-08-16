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

// THE CONSECUTIVE-FAILURE COUNTER IS NOT A GLOBAL. It used to be
// (`static int g_fail_count`), reset by `watchdog_start` and `watchdog_stop`
// and read-modify-written by the handler under RELAXED atomics. The atomics
// made those accesses well-DEFINED; they did not make them CORRECT. A
// handler that outlived its own session's BOUNDED stop drain and resumed
// after a subsequent `watchdog_start` would go on to reset or increment the
// NEW session's counter — delaying the exit signal by up to a full 25s
// threshold window, or hastening it. No amount of ordering strength fixes
// that, because the bug is shared ownership, not tearing.
//
// The counter is therefore a `__block int` declared inside `watchdog_start`
// and captured by that session's event-handler block (see below). Three
// properties follow, none of which the global had:
//
//   - SESSION-LOCAL. Each `watchdog_start` creates fresh storage. A stale
//     handler can only ever mutate the counter of the session it belongs
//     to, which by then nobody reads.
//   - SINGLE-WRITER, so plain `int` arithmetic is correct with no atomics:
//     libdispatch never invokes a source's event handler concurrently with
//     itself, and nothing outside that block touches the variable. The
//     start/stop resets are gone too — fresh storage starts at 0.
//   - LIFETIME BY CONSTRUCTION. The `__block` storage is owned by the
//     heap-copied handler block, which libdispatch keeps alive until the
//     source is deallocated — strictly after the cancel handler, which
//     itself runs only after the last handler invocation returns.
//
// The pure-Go DI seam (`watchdogState` in `watchdog_darwin.go`) remains the
// canonical policy implementation tested by unit tests; the C copy is kept
// minimal because the GCD block has no Go-side allocation budget.

// g_observed_tap is the canonical "current tap" pointer shared between the
// watchdog GCD block (this file) and the NSWorkspace wake-observer blocks
// (wake_darwin.m). The pointer is:
//
//   - WRITTEN from the main goroutine (via `eventtap_set_observed_tap`)
//     during Install (seed = real tap) and during Release Step 1 (NULL).
//   - READ from the GCD high-priority worker thread (watchdog handler) and
//     from `[NSOperationQueue mainQueue]` (wake / session-active blocks).
//
// Every access goes through `__atomic_*` with RELAXED ordering — the same
// treatment `g_tap` gets in tap_darwin.m, for the same reason. This global
// used to be `volatile`, which is NOT the same thing: volatile forbids the
// compiler from caching the value in a register but carries no memory-model
// guarantee at all, so a volatile store racing a volatile load is still a C
// data race, i.e. undefined behaviour, however atomic the arm64 instruction
// underneath happens to be. RELAXED is the right strength because the
// pointer is the only thing published — no other memory is ordered against
// it — and on arm64 it compiles to the same single instruction a plain
// access would.
//
// LIFETIME — the part the NULL guard does NOT solve. An atomic NULL write
// stops a handler that has not yet loaded the pointer; it does nothing for
// one that loaded it a microsecond earlier and was then descheduled. That
// handler resumes holding a raw CFMachPortRef and dereferences it. Two
// mechanisms make that safe, and both are load-bearing:
//
//   1. ORDER. `Releaser.Release` stops the healers — `watchdog_stop` then
//      `wake_observer_remove` — BEFORE it tears down either tap, and
//      `watchdog_stop` below DRAINS in-flight handlers rather than merely
//      cancelling the source. On the normal path no handler can be running
//      by the time any `CFRelease` happens at all. This also matches what
//      `StartWatchdog`'s docstring has always promised ("stop() first, then
//      tapReleaser.Release(); inverted order is unsafe").
//   2. RETAIN. `watchdog_start` CFRetains the tap and the source's cancel
//      handler CFReleases it. libdispatch runs a cancel handler only after
//      the last event-handler invocation has returned, so the retain
//      outlives every handler by construction: even if the drain in (1)
//      times out — a wedged WindowServer IPC is the only plausible way — a
//      late handler dereferences a live mach port rather than freed memory.
//      The SAME retain is taken on the gesture tap (via
//      `gesturetap_retain_current`), because the handler dereferences that
//      port too through `gesturetap_reenable`. (1) alone does not cover
//      either tap precisely because the drain is bounded; treating it as
//      sufficient for the gesture tap is the bug this pair closes.
//
// An earlier revision of this comment argued the guard alone sufficed,
// "because Release Step 4 waits for cancel to drain handlers, and CFRelease
// at Step 3 cannot happen before Step 4". Both halves were false:
// `dispatch_source_cancel` is asynchronous and drains nothing, and the
// CFReleases really did run at Step 3, i.e. BEFORE the watchdog was stopped.
// The order was inverted and the drain added; the claim holds now because
// the code makes it hold, not because the comment asserts it.
//
// There is exactly one definition of `g_observed_tap` in the binary (here);
// wake_darwin.m declares `extern`.
CFMachPortRef g_observed_tap = NULL;

// DND_WATCHDOG_DRAIN_TIMEOUT_NS bounds the cancel-handler wait in
// `watchdog_stop`. It is deliberately 5x the worker-loop drain bound in
// tap_darwin.m (100ms): this one waits on a handler that may be inside a
// `CGEventTapIsEnabled` / `CGEventTapEnable` round-trip to WindowServer,
// not on a run-loop wake-up. The bound exists only so a wedged WindowServer
// turns teardown into a half-second stall instead of a permanent hang —
// both taps are disabled and the user's input is already back by the time
// Release reaches the watchdog, so the stall costs nothing user-visible.
#define DND_WATCHDOG_DRAIN_TIMEOUT_NS (500ull * NSEC_PER_MSEC)

// g_watchdog_done is signalled by the source's cancel handler, which
// libdispatch runs only after the last event-handler invocation has
// returned. It is what turns `watchdog_stop` from "ask the source to stop"
// (which is all `dispatch_source_cancel` does — it is asynchronous and
// makes no promise about a handler already running) into "no handler is
// running and none can start again", which is the property the teardown
// chain needs before it may CFRelease either tap.
static dispatch_semaphore_t g_watchdog_done = nil;

// g_watchdog_gen is the session counter that makes a handler belonging to an
// ALREADY-STOPPED source recognise itself as stale. `watchdog_start` bumps
// it and captures the new value in its block; `watchdog_stop` bumps it
// again, so every handler of the cancelled source mismatches from that
// instant on and returns before touching anything.
//
// Why the semaphore drain in `watchdog_stop` is not enough on its own: that
// wait is BOUNDED (DND_WATCHDOG_DRAIN_TIMEOUT_NS), and `watchdog_stop` nils
// `g_watchdog` before it — deliberately, so teardown cannot wedge — which
// means a subsequent `watchdog_start` passes its "already started" guard
// while a handler of the previous source may still be executing.
//
// SCOPE — what this counter is and is NOT. It is a single check at the top
// of the handler, so on its own it only proves "no restart had happened by
// the time I read it". A handler descheduled AFTER the check and resumed
// after a restart passes it and carries on, which is why the generation is
// explicitly NOT the mechanism that protects the two things such a handler
// could otherwise damage. Those are closed structurally instead:
//
//   - The failure counter is `__block`-local to each session (see the note
//     where `g_fail_count` used to live), so a stale handler's arithmetic
//     lands on storage nobody reads.
//   - Every tap access — the probe, the re-enable, and the post-enable
//     teardown re-check — compares `g_observed_tap` against the session's
//     OWN captured `tap` by pointer IDENTITY, not merely against NULL. A
//     NULL-only re-check was the actual hole: a restart republishes a
//     non-NULL pointer, so the stale handler read the NEW session's tap as
//     proof that its OWN, long-disowned tap was still current and left it
//     enabled. That is the same identity rule `gesturetap_reenable` already
//     carries, for the same reason.
//
// What the generation still buys is a cheap early-out that keeps a stale
// handler from doing pointless work (and from calling `gesturetap_reenable`
// against a session it does not belong to) in the overwhelmingly common
// case where the restart already happened before it resumed. Defence in
// depth, not the load-bearing check — do not weaken the identity guards on
// the strength of it.
//
// WHERE THE RESTART ITSELF IS FENCED OFF. Everything above is written for a
// restart that C cannot prevent on its own, and it narrows rather than closes
// the window: a handler descheduled after its last check is beyond the reach
// of any check. The Go side closes it by removing the restart. When
// `watchdog_stop` reports a timed-out drain (rc 1), `StartWatchdog`'s stop
// closure latches `teardownUnclean`, and `installInternal` then refuses every
// later install with ErrTeardownUnclean — and that function is the only
// caller of `gesturetap_install_c` and the only path that reaches another
// `StartWatchdog`. So a stale handler can never coexist with a second
// session, which is what makes the two effects no guard here covers —
// `gesturetap_reenable`'s fresh load of `g_gesture_tap`, and the
// session-less `eventtap_watchdog_failed` latch — unreachable rather than
// merely unlikely.
//
// The ORDER that argument needs is supplied on the Go side too, and this
// function is why it has to be. `g_watchdog` is nil'd a few lines into
// `watchdog_stop`, i.e. BEFORE the drain whose timeout produces rc 1, so the
// "already started" guard in `watchdog_start` stops refusing well before
// `teardownUnclean` is written. A Start landing in that gap would pass both
// checks. Go closes the gap with a lifecycle mutex (`watchdogLifecycle`) held
// across the whole of `StartWatchdog` and the whole of its stop closure, so
// no Start can observe the latch until this function's verdict has been
// recorded. Do not move the nil-assignment after the wait to "fix" this here:
// it is before the wait deliberately, so a wedged WindowServer cannot leave
// teardown holding a slot it can never free.
//
// The guards below stay: this file must remain correct
// under its own reading, and they are what keeps a stale handler from
// disturbing its own session's teardown.
//
// RELAXED atomics, like every other shared global in this file: the counter
// is the only thing published and nothing is ordered against it. Unsigned,
// so the theoretical wrap after 2^32 starts / stops is defined rather than
// UB — and a collision would need a handler descheduled across 2^32 of them.
static unsigned g_watchdog_gen = 0;

// gesturetap_reenable lives in gesturetap_darwin.m (session-level gesture
// tap). Both the watchdog handler below and the wake-observer blocks
// (wake_darwin.m) call it right after their g_observed_tap NULL-guard so
// the gesture tap self-heals on the same cadence as the main tap. The
// gesture tap keeps no failure counter of its own: the silent-disable
// failure mode kills both taps together, and the main tap's counter is the
// exit signal.
//
// It reads `g_gesture_tap` FRESH at call time rather than taking a pointer,
// so the identity guard the handler ran a few lines earlier does not carry
// into it: whatever port is published when it runs is the port it enables.
// That is safe for our two callers only because the published port can never
// belong to a session other than the caller's — the wake observers are
// main-thread-confined against `wake_observer_remove`, and a watchdog handler
// that outlived its bounded drain cannot be followed by a second session at
// all (see the "WHERE THE RESTART ITSELF IS FENCED OFF" note on
// g_watchdog_gen). Do not add a caller that lacks one of those two
// properties; it would be enabling — and dereferencing — a port it does not
// own and did not retain.
extern void gesturetap_reenable(void);

// gesturetap_retain_current also lives in gesturetap_darwin.m. Returns a +1
// reference to the gesture tap's mach port (NULL when none is installed);
// `watchdog_start` takes it and the source's cancel handler drops it. See
// the "LIFETIME" note above gesturetap_reenable for why the handler's
// g_observed_tap guard and the teardown ORDER are together NOT enough for
// that second tap: this drain is bounded, and a timeout lets teardown
// CFRelease the gesture port while a descheduled handler still holds it.
extern CFMachPortRef gesturetap_retain_current(void);

// eventtap_set_observed_tap is the single writer for g_observed_tap.
// Called from Go (via cgo) at two moments:
//
//   - InstallAll Step (post wake-observer install): seed with the real tap.
//   - Release Step 1 (immediately after eventtap_enable(tap, 0)): write NULL.
//
// RELAXED atomic store, pairing with the RELAXED atomic loads in the
// watchdog handler below and in the wake-observer blocks. Not decoration:
// the Release-path write is genuinely concurrent with those readers, and a
// plain (or merely `volatile`) store against a plain load is a C data race
// regardless of how benign the values are. Readers only branch on
// (snapshot == NULL), so no other memory needs ordering against it — see
// the g_observed_tap declaration comment for the full argument, including
// why the NULL write is a guard and not a lifetime mechanism.
void eventtap_set_observed_tap(CFMachPortRef tap) {
    __atomic_store_n(&g_observed_tap, tap, __ATOMIC_RELAXED);
}

// watchdog_start creates a GCD timer source on DISPATCH_QUEUE_PRIORITY_HIGH
// with a 5s interval + 500ms leeway and installs the event handler
// block that probes `CGEventTapIsEnabled(tap)` every cycle.
//
// Handler policy:
//   1. Generation early-out, then an IDENTITY guard — `g_observed_tap` must
//      still be THIS session's `tap`. NULL (Release ran) and a different
//      pointer (a later session republished) are both "not ours"; bail.
// 2. If `CGEventTapIsEnabled(tap)` → reset `fail_count = 0` (
// any healthy probe resets the counter, including UserInput
//      disables that the tap's own inline callback re-enabled).
//   3. Otherwise: `CGEventTapEnable(tap, true)`, re-run the identity guard
//      (undoing our own enable on a mismatch), then re-probe. If the
//      re-enable succeeded → also reset counter.
//   4. Only if re-enable FAILED → `fail_count++`. On reaching
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

    // Open a new session. Every handler created below carries `my_gen` and
    // compares it against this global before it does anything, so a handler
    // left over from a PRIOR source — reachable only when that source's stop
    // drain timed out — no longer shares state with this one. See the
    // g_watchdog_gen declaration for what such a handler would otherwise
    // corrupt. Bumped BEFORE the counter reset below so the stale handler is
    // already fenced off when we clear the value it might have been writing.
    unsigned my_gen = __atomic_add_fetch(&g_watchdog_gen, 1, __ATOMIC_RELAXED);

    // This session's consecutive-failure counter. `__block` so the handler
    // below mutates the ONE instance created by THIS call rather than a
    // process-wide global a prior session's stranded handler could still be
    // writing — see the long note where `g_fail_count` used to live. No
    // reset needed anywhere (fresh storage is zero) and no atomics needed
    // (the source's event handler is its only accessor and libdispatch
    // never runs one concurrently with itself).
    __block int fail_count = 0;

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
        // Generation guard — FIRST, and a cheap early-out only. This handler
        // may belong to a source that `watchdog_stop` cancelled while this
        // very invocation was descheduled past its bounded drain, in which
        // case a new `watchdog_start` already owns the globals below and
        // nothing this body does should be applied. A mismatch means exactly
        // that, and the only correct action is to do nothing.
        //
        // It is NOT sufficient on its own — it is one check, and the
        // deschedule can land after it. The identity guards below are what
        // actually make this body safe; see the g_watchdog_gen declaration.
        if (__atomic_load_n(&g_watchdog_gen, __ATOMIC_RELAXED) != my_gen) {
            return;
        }

        // Liveness + IDENTITY guard, before any CG* call (invariant).
        // `g_observed_tap` is the published "current tap"; `tap` is the one
        // THIS session owns and CFRetained. Three cases collapse into one
        // comparison:
        //
        //   - equal   → our session is live; proceed.
        //   - NULL    → Release Step 1 ran; no-op, exactly as before.
        //   - other   → a LATER session republished its own tap while we
        //               were descheduled. Not ours; no-op.
        //
        // The third case is why this is `!= tap` and not `== NULL`. A
        // NULL-only guard reads a restart's fresh pointer as proof that OUR
        // long-disowned tap is still current, which is precisely the reading
        // that turns the post-enable re-check below from a safety net into a
        // rubber stamp. `gesturetap_reenable` already carries this same
        // identity rule for the same reason.
        //
        // ABA is impossible here rather than merely unlikely: `tap` stays
        // CFRetained by this source for as long as any of its handlers can
        // run, so its address cannot be recycled into a later session's tap
        // while we are executing.
        //
        // RELAXED atomic load, pairing with the store in
        // `eventtap_set_observed_tap`. The load is a guard, not a lifetime
        // mechanism — lifetime comes from the CFRetain in watchdog_start and
        // the teardown ORDER, both documented on the g_observed_tap
        // declaration. Do not weaken either on the strength of this guard.
        if (__atomic_load_n(&g_observed_tap, __ATOMIC_RELAXED) != tap) {
            return;
        }

        // Heal the session-level gesture tap on the same probe cadence.
        // Placed AFTER the guard above so a Release in progress (NULL
        // written at Step 1) also suppresses late gesture re-enables.
        // Idempotent no-op when the gesture tap is healthy.
        //
        // NOTE the guard above does NOT carry into this call: it re-reads
        // g_gesture_tap itself, so if we were descheduled here it would act
        // on whatever port is published when it resumes. The reason that is
        // not a cross-session use-after-free is that there is no later
        // session to publish one — see the extern declaration's note and
        // "WHERE THE RESTART ITSELF IS FENCED OFF" on g_watchdog_gen.
        gesturetap_reenable();

        // healthy probe → reset counter. This covers BOTH the normal
        // case AND (kCGEventTapDisabledByUserInput re-enable performed
        // by the tap's own inline callback in).
        //
        // All CG* calls below use `tap` — the session's own retained port,
        // which the guard above just proved is the published one. Reading
        // g_observed_tap again for each call would only re-open the window
        // the identity check exists to close.
        //
        // `fail_count` is this session's `__block` local: plain assignment,
        // no atomics, because this block is its only accessor and never runs
        // concurrently with itself.
        if (CGEventTapIsEnabled(tap)) {
            fail_count = 0;
            return;
        }

        // Tap is disabled — attempt to re-enable.
        CGEventTapEnable(tap, true);

        // Teardown / restart re-check. The enable above is the one action in
        // this handler that a stale session makes actively harmful rather
        // than merely wasted: if Release ran while we were descheduled, we
        // have just re-enabled a tap whose run-loop source is about to be
        // (or has already been) detached, so the kernel resumes posting
        // events to a port nobody services and input stalls until
        // WindowServer times the tap out.
        //
        // IDENTITY again, for the case a plain NULL check misses entirely: a
        // `watchdog_stop` + `watchdog_start` pair can complete inside this
        // window, and then `g_observed_tap` is non-NULL again — but it is
        // the NEW session's port, and ours is detached and must not be left
        // enabled. Undo our own enable and leave.
        //
        // Safe to touch `tap` on this path: the CFRetain taken in
        // watchdog_start is still outstanding (the cancel handler that drops
        // it cannot have run while this handler is executing).
        if (__atomic_load_n(&g_observed_tap, __ATOMIC_RELAXED) != tap) {
            CGEventTapEnable(tap, false);
            return;
        }

        // Re-probe. If re-enable succeeded → also a healthy state, reset.
        if (CGEventTapIsEnabled(tap)) {
            fail_count = 0;
            return;
        }

        // Re-enable failed: this is the counter-incrementing path.
        if (++fail_count >= FAIL_THRESHOLD) {
            // signal Go-side latch. The exported function's body is
            // exactly `watchdogThresholdHit.Store(true)` (fixed
            // invariant) — safe to call from a
            // GCD worker thread because atomic.Store is nosplit.
            //
            // Idempotent: subsequent fires keep the latch true; the Go
            // poller's `CompareAndSwap(true, false)` ensures exactly one
            // sink-send + one log line per threshold-trip.
            //
            // The latch carries NO session identity — it is one
            // package-global atomic.Bool — and this call sits behind the
            // handler's last identity check, so a deschedule right here is
            // beyond every guard in this file. What makes that harmless is
            // that the only reader is the poller of a session that cannot
            // exist: a drain this handler outlived latched
            // `teardownUnclean` on the Go side, so no later `StartWatchdog`
            // runs and no later poller is spawned. Our own session's poller
            // was already joined before that latch was set, so a store from
            // here reaches nobody.
            eventtap_watchdog_failed();
        }
    });

    // Own a reference to the tap for as long as a handler can run against
    // it. The balancing CFRelease lives in the cancel handler below, which
    // libdispatch invokes only after the LAST event-handler invocation has
    // returned — so the retain covers every handler by construction,
    // including one that snapshotted the pointer and was then descheduled
    // past the whole teardown chain. Without it, `CFRelease(tap)` in
    // eventtap_uninstall_c could free a mach port a live handler is about to
    // probe; the g_observed_tap NULL guard cannot prevent that, because a
    // pointer already loaded into a register is not affected by a later
    // store. Taken here rather than in InstallAll so watchdog_start stays
    // self-sufficient for the smoke-test path.
    CFRetain(tap);

    // Same reasoning, second tap. The handler calls gesturetap_reenable(),
    // which loads g_gesture_tap and dereferences it — and unlike the main
    // tap, nothing else in the process keeps that port alive for a handler
    // that snapshotted the pointer and was then descheduled. The teardown
    // ORDER alone does not cover it: `watchdog_stop`'s drain is bounded by
    // DND_WATCHDOG_DRAIN_TIMEOUT_NS and, on expiry, only logs a WARN before
    // `Releaser.Release` continues into gesturetap_uninstall_c, which
    // CFReleases that port. This reference closes that path — it is dropped
    // in the cancel handler below, which libdispatch runs only after the
    // last handler invocation returns, so it outlives every handler
    // regardless of how the drain went.
    //
    // NULL when the caller installed no gesture tap (the smoke-test path
    // that exercises the watchdog in isolation); the cancel handler
    // nil-checks accordingly. InstallAll always installs the gesture tap
    // inside installInternal, i.e. strictly before this call.
    CFMachPortRef gesture_held = gesturetap_retain_current();

    // The cancel handler is the drain half of the teardown handshake:
    // `dispatch_source_cancel` alone is asynchronous and promises nothing
    // about a handler already running, whereas this block is guaranteed to
    // run after the last one finishes. Signalling the semaphore from here
    // is what lets `watchdog_stop` block until "no handler is running and
    // none can start again" is actually true. `done` is captured strongly
    // by the block (ARC), so it stays alive even after watchdog_stop nils
    // the static — the block must never dereference a nil semaphore.
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    g_watchdog_done = done;
    dispatch_source_set_cancel_handler(g_watchdog, ^{
        CFRelease(tap);
        if (gesture_held != NULL) {
            CFRelease(gesture_held);
        }
        dispatch_semaphore_signal(done);
    });

    // Seed g_observed_tap BEFORE dispatch_resume so the very first handler
    // fire (5s from now per dispatch_source_set_timer above) sees a
    // non-NULL snapshot. InstallAll also explicitly calls
    // `eventtap_set_observed_tap(tap)` after wake_observer_install for
    // belt-and-suspenders (idempotent — re-sets the same value); this
    // line keeps watchdog_start self-sufficient for the existing
    // smoke-test path and for any future caller that exercises
    // the watchdog without going through InstallAll.
    __atomic_store_n(&g_observed_tap, tap, __ATOMIC_RELAXED);

    // the design notes step 4 — MANDATORY before first fire.: an
    // unresumed source cannot be safely released; always resume before
    // storing the source in a global that may be torn down.
    dispatch_resume(g_watchdog);

    return 0;
}

// watchdog_stop tears down the GCD timer source AND waits for any in-flight
// handler to finish. Idempotent — safe to call when no watchdog is currently
// active (e.g. Install failed before watchdog_start, or Release runs twice
// via the two-layer guard).
//
// RETURN VALUE: 0 when the drain is established — no handler is running and
// none can start again — and 1 when the bounded wait expired first. The
// caller logs the 1 (Go side, WARN). It is not a value anyone can act on
// beyond that: the CFRetain / cancel-handler-CFRelease pair taken in
// watchdog_start keeps the tap's mach port alive for a late handler either
// way, so a timeout costs a slightly longer-lived port and nothing else.
//
// Teardown sequence:
//   0. Bump `g_watchdog_gen` — every handler of this source is stale from
//      this instant, so one that outlives the bounded wait in step 3 can no
//      longer touch a later session's state. Cancellation cannot express
//      that on its own: it is asynchronous, and the source pointer is gone
//      by the time the next `watchdog_start` checks for one.
//   1. `dispatch_source_cancel` — no FURTHER timer fire after this returns.
//      It says nothing about an invocation already running: libdispatch's
//      cancel is asynchronous, which is exactly why step 3 exists.
//   2. `g_watchdog = nil` — drops our strong ARC reference. libdispatch
//      holds its own until the cancel handler has run, so this cannot
//      deallocate the source out from under the pending cancellation.
//      Under `-fobjc-arc` manual GCD-object release is FORBIDDEN (compile
//      error), so nil-assignment is the only spelling.
//   3. Wait on the cancel-handler semaphore. libdispatch runs a cancel
//      handler only after the last event-handler invocation has returned,
//      so observing this signal IS the proof the teardown chain needs
//      before it may CFRelease either tap. Bounded by
//      DND_WATCHDOG_DRAIN_TIMEOUT_NS — see that constant for why a stall
//      here is preferable to a hang and why it is invisible to the user.
//
// There is deliberately no failure-counter reset step. There used to be one
// (and a matching seed in `watchdog_start`), which is what a process-wide
// `g_fail_count` required and what made its value racy across a timed-out
// drain. The counter is now `__block`-local to each session, so a fresh
// `watchdog_start` gets fresh zeroed storage and a stranded handler can only
// reach the dead session's copy. See the note where the global used to live.
int watchdog_stop(void) {
    if (g_watchdog == nil) {
        return 0;
    }
    // Close the session BEFORE the cancel. From this store on, every handler
    // of this source — the one that may be running right now as well as any
    // libdispatch has already dispatched — fails its generation check and
    // returns without touching a single global. That is what keeps a
    // subsequent `watchdog_start` isolated from a handler this function's
    // BOUNDED drain below failed to wait out: `g_watchdog` is nil'd a few
    // lines down, so the "already started" guard in `watchdog_start` does
    // NOT stand in for this. See the g_watchdog_gen declaration.
    __atomic_fetch_add(&g_watchdog_gen, 1, __ATOMIC_RELAXED);
    dispatch_semaphore_t done = g_watchdog_done;
    dispatch_source_cancel(g_watchdog);
    // ARC: nil-assignment releases the strong ref. NO manual GCD release.
    g_watchdog = nil;
    g_watchdog_done = nil;
    if (done == nil) {
        // Defensive: a source that exists without its semaphore would mean
        // watchdog_start was interrupted between the two assignments, which
        // is straight-line code and therefore impossible. Nothing to wait
        // on, so report the honest thing rather than blocking forever.
        return 1;
    }
    if (dispatch_semaphore_wait(done,
            dispatch_time(DISPATCH_TIME_NOW, (int64_t)DND_WATCHDOG_DRAIN_TIMEOUT_NS)) != 0) {
        return 1;
    }
    // Drain confirmed — no handler is running and none can start again.
    return 0;
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
// injector could observe the failure count but not corrupt the watchdog
// state), but the same "correctness/security > size" rationale applies:
// dead test-only code in a production binary serves no purpose and
// widens the symbol table available to a process-injection adversary.
//
// The counter it read is no longer reachable from file scope at all: it is
// now a `__block` local owned by each session's handler block. A future
// smoke test wanting to observe it end-to-end would need a `*_darwin_test.m`
// companion file with `//go:build manual` parity (the build-tag-gated
// alternative proposed in 's "alternative" fix), not a revival of this
// dead note.
