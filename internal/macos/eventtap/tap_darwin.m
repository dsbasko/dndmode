//go:build darwin
// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>
#import <IOKit/hidsystem/ev_keymap.h>  // NX_SYSDEFINED (== 14) media-key event type
#import <dispatch/dispatch.h>  // dispatch_semaphore_t — the drain handshake below
#import <stdint.h>
#import <string.h>       // memcpy / memset for the keystroke ring
#import "tap_ring.h"     // DND_RING_CAP, dnd_keyrec_t — shared with tap_darwin.go

// The cgo-generated export header is deliberately NOT imported here. This
// translation unit makes ZERO calls into Go: the callback only appends to the
// static ring below, and every comparison happens on the poller goroutine
// (poller.go). Importing that header is the first step of re-introducing a Go
// call into the callback, which is exactly what the nosplit invariant
// documented on `eventtap_callback` forbids. watchdog_darwin.m does import it,
// and that is fine: its //export call fires from a GCD handler, never from a
// tap callback.
//
// `TestTapSource_DoesNotImportCgoExportHeader` (nosplit_gold_test.go) reads
// this file and fails if the directive comes back, so the absence is an
// assertion rather than a habit.

// USER_INTENTIONAL_MASK is the bit-for-bit twin of `matcher.UserIntentionalMask`
// on the Go side. The callback strips system bits (CapsLock 0x10000,
// NumPad 0x200000, SecondaryFn 0x800000, Help 0x400000,
// NX_NONCOALSESCEDMASK 0x100, …) before
// recording the press, because macOS sets those bits
// independently of user intent. Drift between this constant
// and `matcher.UserIntentionalMask` would silently break the unlock code for
// users with CapsLock or NumPad toggled. Two tests keep the twins honest:
// `TestUserIntentionalMask_MatchesMatcherPackage` (tap_test.go) pins the Go
// constant to a hex literal, and
// `TestUserIntentionalMask_CSourcePinsExactlyFourBits` (nosplit_gold_test.go)
// greps THIS definition for exactly the four names below. Dropping an
// OR-term here is the fail-deadly direction — the callback would record a
// zero bit the Go side still demands, making the unlock code unenterable —
// so it fails a unit-test rather than locking a customer out at runtime.
//
// kCGEventFlagMaskSecondaryFn is EXCLUDED on purpose: macOS raises it for
// every key of the function-key group (F1-F12, arrows, Forward Delete,
// Home/End/PageUp/PageDown) regardless of the physical Fn key, so treating
// it as user intent would make a step declared as a bare `up` unmatchable.
// See the UserIntentionalMask doc comment in internal/matcher/matcher.go for
// the full argument — this constant must not re-add the bit on its own.
//
// Values are taken from CGEventTypes.h (HIGH confidence per the design notes):
//   kCGEventFlagMaskShift        = 0x00020000
//   kCGEventFlagMaskControl      = 0x00040000
//   kCGEventFlagMaskAlternate    = 0x00080000  (Option)
//   kCGEventFlagMaskCommand      = 0x00100000
// OR-sum = 0x001E0000.
static const uint64_t USER_INTENTIONAL_MASK =
    (uint64_t)kCGEventFlagMaskShift     |
    (uint64_t)kCGEventFlagMaskControl   |
    (uint64_t)kCGEventFlagMaskAlternate |
    (uint64_t)kCGEventFlagMaskCommand;

// g_ring and g_seq are the keystroke ring: the callback (single writer, tap
// worker thread) appends one dnd_keyrec_t per non-autorepeat KeyDown, the
// poller goroutine (single reader) snapshots it every pollInterval and does
// the actual unlock-code comparison in pure Go (internal/matcher).
//
// g_seq is the monotonic count of key presses ever recorded, NOT an index:
// the slot for press number `s` is `g_ring[s & (DND_RING_CAP - 1)]`. The
// reader needs the absolute count to know which records are new since its
// previous tick, which a wrapped index could not tell it. Overflow is not a
// concern — at 100 presses/second uint64 wraps in ~5.8e9 years.
//
// NOT `volatile`: every access goes through __atomic_* builtins, which
// already imply the ordering and the "do not cache in a register" behaviour
// `volatile` is (incorrectly) reached for. Marking it volatile as well would
// suggest volatile is load-bearing here, and it is not — the RELEASE/ACQUIRE
// pair is.
static dnd_keyrec_t g_ring[DND_RING_CAP];
static uint64_t     g_seq = 0;

// g_tap, g_source, g_worker_runloop hold the per-process tap state. There is
// exactly one active CGEventTap per dndmode process (single Install
// per Releaser; second concurrent install would conflict on the same
// HID-level slot).
//
// g_worker_runloop is captured by `eventtap_register_worker_runloop` (called
// from the worker goroutine AFTER its `runtime.LockOSThread()`). The tap
// source is added to THIS run loop, NOT `CFRunLoopGetMain()`, so that
// CGEvent dispatch happens off the main thread and AppKit on the main thread
// stays responsive (the design notes).
//
// g_tap is the ONE of the three that is callback-visible: the self-heal
// branch of `eventtap_callback` reads it on the worker thread. Every access
// to it — here, in install, in uninstall, in the callback — therefore goes
// through `__atomic_*` with RELAXED ordering. Not decoration: the teardown
// path writes NULL on the drain-TIMEOUT branch too, i.e. precisely when the
// callback could NOT be proven idle, so a plain store there would be a data
// race against a plain load in the callback. RELAXED is sufficient because
// the pointer is the only thing being published — no other memory is ordered
// against it — and on arm64 it compiles to the same single instruction a
// plain access would, which keeps the callback's nosplit budget intact.
// Mixing a plain access in anywhere re-opens the race, so there are no plain
// accesses at all.
//
// g_source and g_worker_runloop stay plain: no callback reads either. They
// are touched only by install and by teardown, and the Go-side Release guard
// serialises those onto one goroutine.
static CFMachPortRef      g_tap            = NULL;
static CFRunLoopSourceRef g_source         = NULL;
static CFRunLoopRef       g_worker_runloop = NULL;

// eventtap_callback is the CGEventTap callback. It fires on the worker
// thread that runs `g_worker_runloop` (NOT main). Contract per
// (the design notes):
//
//   1. If `type == kCGEventTapDisabledByTimeout` OR
//      `kCGEventTapDisabledByUserInput` → inline `CGEventTapEnable(g_tap, true)`
// and propagate the event as-is (the design notes "event field is undocumented
// for these types; return as-is"; per UserInput is normal — the
//      callback always heals, the watchdog only counts silent disables).
//   2. If `type == kCGEventFlagsChanged` → return NULL (suppress without
// match-testing). Flag-only events have no keyCode.
//   3. If `type == kCGEventKeyDown` → drop autorepeat events, then append
//      (masked flags, keyCode) to the keystroke ring. That is the WHOLE
//      key-handling path: this callback compares nothing. The poller
//      goroutine snapshots the ring every pollInterval and runs the unlock
//      code comparison in pure Go (poller.go → internal/matcher).
//   4. Unconditional `return NULL` at the end — all keyboard / mouse / scroll
// events are swallowed ("all input blocked except the configured
//      unlock code"); the keys of the code are ALSO swallowed so they do
//      not leak into the underlying app (e.g. a stray Cmd+X reaching a text
//      editor).
//
// nosplit invariant: this body MUST NOT acquire Go locks, allocate Go
// memory, log, or call dispatch_async — and, as of the sequence-matcher
// wiring, MUST NOT call into Go AT ALL. It previously made exactly one Go
// call (`eventtap_matched()`, a single atomic store), which pre-fix
// experimentation in the design notes had established as the only callback
// shape that survives `-race` under load. Moving the comparison to Go took
// that count from one to zero, so the invariant is now strictly stronger
// than the shape that was validated. Do not add a Go call back.
//
// ENFORCED BY nosplit_gold_test.go — and, for the first time, actually
// enforced. An earlier revision of this comment claimed a "gold-grep in
// tap_test.go" backed it; no such test existed, and the invariant rested on
// code review alone for the whole life of that claim. Two tests now read this
// source text: `TestTapSource_DoesNotImportCgoExportHeader` (no
// `_cgo_export.h` directive anywhere in the file, which makes a Go call here
// fail to compile) and `TestEventTapCallback_CallsNoGoExports` (the body of
// this function calls none of the package's //export symbols, which holds
// even if some future non-callback code in this file needs the header).
//
// The ring append is what replaced it, and it is strictly weaker than the
// call it displaced — which is why it was chosen over any richer
// accumulation scheme:
//
//   - It touches only static C storage (`g_ring`, `g_seq`) that is allocated
//     once in BSS at process start — no malloc, no Go heap, no CFRetain.
//   - It takes no lock of any kind. Coordination with the reader is a single
//     RELEASE store paired with the reader's ACQUIRE load; there is exactly
//     one writer (this callback) so no writer-writer synchronisation exists
//     to deadlock on.
//   - It makes no Go call, no syscall, no allocation, and cannot block or
//     grow the stack, so the thread never needs the Go runtime while it is
//     inside this function.
//
// silent fail: no NSLog / printf / fprintf — wrong-key presses leave
// no observable side channel. `--debug` mode is deferred per the design notes.
static CGEventRef eventtap_callback(CGEventTapProxy proxy,
                                    CGEventType type,
                                    CGEventRef event,
                                    void *userInfo) {
    (void)proxy;
    (void)userInfo;

    // disable recovery. Inline re-enable from the callback's own
    // thread; propagate the event as-is per the design notes (the `event` field is
    // undocumented for these types but pqrs-org/Karabiner production code
    // returns it without issue — A7 assumption verified there).
    //
    // The RELAXED atomic load is load-bearing twice over, and both reasons
    // are about the teardown thread writing NULL concurrently (which it does
    // on the drain-timeout path, where this callback is by definition NOT
    // provably idle):
    //
    //   1. It is what makes that concurrent write well-defined instead of a
    //      C data race. A plain load here paired with the plain store in
    //      `eventtap_uninstall_c` is UB no matter how benign the values are.
    //   2. It pins the value to ONE load. With a plain non-volatile static
    //      the compiler is entitled to assume nothing else writes it and to
    //      rematerialise the load after the NULL check — turning the guarded
    //      call into `CGEventTapEnable(NULL, true)`. The local makes the
    //      check and the use read the same value by construction.
    //
    // RELAXED is the right strength: the only thing being published is the
    // pointer itself, no other memory is ordered against it, and the callback
    // must stay a handful of instructions (nosplit invariant above). On arm64
    // a relaxed load is the same `ldr` a plain load would emit.
    if (type == kCGEventTapDisabledByTimeout ||
        type == kCGEventTapDisabledByUserInput) {
        CFMachPortRef tap = __atomic_load_n(&g_tap, __ATOMIC_RELAXED);
        if (tap != NULL) {
            CGEventTapEnable(tap, true);
            // Teardown re-check — the second half of the same race, and the
            // half a NULL store alone cannot win. On the drain-TIMEOUT path
            // this callback is by definition possibly-live, and the load
            // above may have happened BEFORE `eventtap_uninstall_c` published
            // NULL. The enable that follows then lands AFTER teardown's own
            // final disable and after `CFRunLoopRemoveSource`, leaving an
            // ENABLED tap whose source is detached: the kernel keeps posting
            // events to a port nobody services and the machine's input stalls
            // until WindowServer times the tap out. Re-reading g_tap after the
            // enable closes it — a NULL here means teardown ran, so we undo
            // our own enable. Teardown publishes the NULL BEFORE its disable
            // precisely so this check cannot be the loser of that ordering.
            //
            // Dereferencing `tap` here is safe on both paths: the CFReleases
            // are SKIPPED whenever this callback could be live (drained != 0),
            // and when they do run the drain has already proven it is not.
            if (__atomic_load_n(&g_tap, __ATOMIC_RELAXED) == NULL) {
                CGEventTapEnable(tap, false);
            }
        }
        return event;
    }

    // FlagsChanged events carry only modifier-state transitions — no keyCode.
    // Suppress them without match-testing. Returning the event
    // would let modifier state leak to the app even when the rest of the tap
    // is blocking input.
    if (type == kCGEventFlagsChanged) {
        return NULL;
    }

    // record path: only KeyDown carries a keyCode worth recording.
    // KeyUp / mouse / scroll / NX_SYSDEFINED all fall through to the final
    // unconditional `return NULL` below.
    if (type == kCGEventKeyDown) {
        // autorepeat filter. A key held down produces a stream of KeyDown
        // events with kCGKeyboardEventAutorepeat != 0. Those are not
        // deliberate presses, and letting them into the ring would hand an
        // attacker a free brute-force primitive: hold `a` and the ring fills
        // with `a a a a …`, covering every repeated-character code at the
        // repeat rate instead of at typing speed. A genuine double tap
        // ("ll" in "hello") is unaffected — autorepeat only engages after
        // the system hold threshold, which no double tap reaches.
        if (CGEventGetIntegerValueField(event, kCGKeyboardEventAutorepeat) != 0) {
            return NULL;
        }

        uint64_t flags = (uint64_t)CGEventGetFlags(event) & USER_INTENTIONAL_MASK;
        int64_t  keycode = CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);

        // ring append. RELAXED load is sufficient for reading our own
        // counter — this callback is the only writer, so no other thread can
        // have advanced it. The RELEASE store is what publishes the record:
        // it orders the two field writes above it before the counter becomes
        // visible to the reader's ACQUIRE load in eventtap_snapshot.
        uint64_t s = __atomic_load_n(&g_seq, __ATOMIC_RELAXED);
        g_ring[s & (DND_RING_CAP - 1)].flags   = flags;
        g_ring[s & (DND_RING_CAP - 1)].keycode = (uint16_t)keycode;
        __atomic_store_n(&g_seq, s + 1, __ATOMIC_RELEASE);
    }

    //: ALWAYS return NULL after the record branch. Every
    // keyboard / mouse / scroll / media event is suppressed; the keys of the
    // unlock code are swallowed too so the code does not surface in any app.
    return NULL;
}

// eventtap_seq returns the number of key presses recorded so far. It is the
// cheap probe the poller runs on every tick: if the value has not moved
// since the previous tick there is nothing new to match and the poller skips
// the snapshot entirely, which is what keeps ~99% of ticks down to a single
// atomic load.
//
// ACQUIRE ordering pairs with the callback's RELEASE store so that a caller
// which observes `cur` is guaranteed to also observe the ring writes for
// every record with index < cur.
uint64_t eventtap_seq(void) {
    return __atomic_load_n(&g_seq, __ATOMIC_ACQUIRE);
}

// eventtap_snapshot copies the whole ring into `out` (which MUST have room
// for DND_RING_CAP records) and returns the press count as of the ACQUIRE
// load taken BEFORE the copy. There is deliberately no seqlock-style retry
// loop here.
//
// Correctness invariant: the RELEASE store in the callback publishes every
// record with index < cur; records with indices >= cur may be read torn, but
// they are never consumed. The reader only ever examines the half-open range
// [previous_cur, cur), and the writer does not revisit a published slot for
// another DND_RING_CAP presses.
//
// benign race by construction — a torn entry at index >= cur is never
// consumed. The memcpy reads slot `cur & (DND_RING_CAP-1)` without
// synchronisation while the callback may be mid-write into it; under the C
// memory model that is formally UB, and `go test -race` does not instrument
// C code so it will never flag it. On arm64 with the naturally-aligned
// uint64/uint16 fields of dnd_keyrec_t the loads and stores are single
// instructions and cannot tear in practice, and by the invariant above the
// value read from that slot is discarded regardless of what it contains.
// This is a deliberate, reviewed trade-off: do not re-open it by adding a
// retry loop, which would not fix the formal UB and would not change any
// observable behaviour. A seqlock retry was tried and removed — it did not
// catch the only dangerous case (a read landing inside an unfinished write
// still observes a stable counter on both sides), and the case it did catch
// is harmless here.
uint64_t eventtap_snapshot(dnd_keyrec_t *out) {
    uint64_t cur = __atomic_load_n(&g_seq, __ATOMIC_ACQUIRE);
    memcpy(out, g_ring, sizeof(g_ring));
    return cur;
}

// DND_DRAIN_TIMEOUT_NS bounds the callback-drain handshake below. A
// CFRunLoopWakeUp round-trip on an idle loop is microseconds, so 100ms is
// ~1000x headroom; the bound exists only so that a worker loop which is NOT
// running (the install-rollback paths, where the goroutine may have died
// before CFRunLoopRun) turns into a short stall instead of a permanent hang
// of the teardown chain — which would leave the overlay up and the keyboard
// under a disabled-but-not-released tap.
#define DND_DRAIN_TIMEOUT_NS (100ull * NSEC_PER_MSEC)

// eventtap_drain_worker_callbacks blocks until `eventtap_callback` is known
// not to be executing, so that a ring wipe cannot race the callback's writes.
//
// Why this is needed: `CGEventTapEnable(tap, false)` stops the kernel from
// posting NEW events to the tap, but Apple documents no drain guarantee for
// it — a callback that had already been dispatched keeps running on the
// worker thread. Its ring append is a pair of PLAIN stores (only the counter
// is atomic), so a `memset(g_ring, ...)` overlapping it is a genuine C data
// race, and a callback that stores its record after the memset repopulates
// the ring that was just wiped and leaves the counter non-zero. "Tap
// disabled" is therefore not the same as "no writer left".
//
// How it drains: the tap callback is a run-loop callout on `g_worker_runloop`
// and a block queued with CFRunLoopPerformBlock is executed by that SAME
// thread. A run-loop callout cannot be preempted by a queued block, so the
// block can only run once any in-flight callback has returned — observing
// the block's signal from another thread is therefore proof that the
// callback finished. CFRunLoopWakeUp is required because PerformBlock alone
// does not wake a sleeping loop.
//
// What it does NOT promise: this is a drain, not a barrier. It says nothing
// about a mach message already queued on the tap port that has not been
// delivered yet — such an event can still produce a callback afterwards.
// Only detaching the source removes that possibility, which is why
// `eventtap_uninstall_c` calls this AFTER `CFRunLoopRemoveSource`: there the
// drain is airtight, and it also guarantees no callback is inside
// `CGEventTapEnable(g_tap, true)` when the mach port is CFReleased on the
// next line. That is also why the Release Step 3 wipe does NOT use this
// function on its own: with the source still attached a drain cannot rule
// out a later callback, so that wipe runs ON the worker thread instead
// (`eventtap_wipe_ring_on_worker` below), where the thread — not a
// handshake — is what serialises it against the callback.
//
// RETURN VALUE — 0 means the drain is established, 1 means it timed out and
// the caller must treat a callback as possibly live. It is NOT decoration:
// every caller here branches on it, because "proceed anyway" means
// CFReleasing a mach port a callback may be sitting inside. Timing out is
// only reachable when the worker loop is not servicing blocks (the
// install-rollback paths, where the goroutine can die before CFRunLoopRun);
// a callback of a handful of stores cannot hold the thread for 100ms. The
// bounded wait exists so that case is a short stall rather than a permanent
// hang of the teardown chain — which would leave the overlay up and the
// keyboard under a disabled-but-not-released tap.
//
// Callable with no worker loop (before install, after uninstall NULLs it):
// returns 0 immediately — no loop means no callback can be dispatched, which
// is the same fact the handshake would have established.
//
// ARC note: the package compiles with -fobjc-arc (see the #cgo CFLAGS in
// tap_darwin.go), so the block's strong capture of `sem` keeps the semaphore
// alive for as long as the queued block exists. On the timeout path the
// block may still signal later, and it is signalling an object it owns —
// there is no dangling reference and no manual release to get wrong.
int eventtap_drain_worker_callbacks(void) {
    CFRunLoopRef loop = g_worker_runloop;
    if (loop == NULL) {
        return 0;
    }
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    CFRunLoopPerformBlock(loop, kCFRunLoopCommonModes, ^{
        dispatch_semaphore_signal(sem);
    });
    CFRunLoopWakeUp(loop);
    // dispatch_semaphore_wait returns non-zero on timeout (Apple docs). That
    // is the ONLY signal a caller gets that the block never ran, so it is
    // returned rather than dropped.
    if (dispatch_semaphore_wait(sem,
            dispatch_time(DISPATCH_TIME_NOW, (int64_t)DND_DRAIN_TIMEOUT_NS)) != 0) {
        return 1;
    }
    return 0;
}

// eventtap_wipe_ring clears the recorded key presses and resets the press
// counter. It exists so the tail of what the user typed — which ends with
// the unlock code itself — stops being resident in process memory the
// moment the tap goes down, rather than at `eventtap_uninstall_c` several
// CoreFoundation teardown steps later.
//
// CALL SITE CONTRACT: only where neither a tap callback NOR the poller can
// be running — install (before the tap exists), the tail of
// `eventtap_uninstall_c` (source detached, drain confirmed, tap released),
// and the body of `eventtap_wipe_ring_on_worker`, which is how Release
// Step 3 reaches it: ON the worker thread, where the callback cannot run
// concurrently because that same thread is what would run it. Those three
// are the ONLY call sites and they are the only implementation of "clear
// the ring": a fourth open-coded memset that forgets the counter reset
// would hand the poller a run of `{flags: 0, keycode: 0}` records, and
// keycode 0 is kVK_ANSI_A.
//
// Note what is NOT on that list: a plain call from the Go side while the tap
// source is still attached to the worker loop. That was the Step 3 shape
// before, and it was unsound for a reason no drain can fix — a mach message
// already queued on the tap port produces a callback the handshake did not
// and could not wait for, whose ring append is a pair of PLAIN stores
// overlapping the memset. Go through `eventtap_wipe_ring_on_worker` there.
//
// It MUST NOT be called from the
// poller: the poller runs concurrently with a live tap, so calling it there
// would introduce a SECOND writer to the ring and destroy the single-writer
// premise the whole correctness argument for `eventtap_snapshot` rests on.
//
// Both halves of the contract matter, and the READER half is the one that is
// easy to get wrong. Resetting the counter is NOT sufficient on its own:
// `eventtap_snapshot` takes its ACQUIRE load of `g_seq` and then memcpys the
// ring as two separate steps, so a wipe landing between them returns the
// pre-wipe count over post-wipe (zeroed) storage. The poller would then walk
// [lastSeq, cur) across a run of `{flags: 0, keycode: 0}` records — keycode
// 0 being kVK_ANSI_A — and an unlock code of bare `a` steps could spuriously
// match during teardown. The counter reset closes the case where the poller
// snapshots strictly AFTER the wipe (its `lastSeq` is past zero, so the
// half-open range collapses to empty and `matchAny` iterates zero times);
// draining the poller before the wipe is what closes the interleaved case.
// Keep both.
//
// The WRITER half of the contract is satisfied differently at each of the
// three sites, which is why this function does NOT try to satisfy it
// itself: at install no loop exists yet, at the uninstall tail the source
// is already detached and `eventtap_drain_worker_callbacks` has confirmed,
// and at Step 3 the wrapper puts the call on the writer's own thread. A
// drain bolted on here would be a no-op at two sites and the wrong
// mechanism at the third. Disabling the tap is NOT sufficient by itself —
// Apple documents no callback-drain guarantee for CGEventTapEnable, so an
// already-dispatched callback can still be mid-write and would repopulate
// the ring it just wiped.
void eventtap_wipe_ring(void) {
    memset(g_ring, 0, sizeof(g_ring));
    __atomic_store_n(&g_seq, 0, __ATOMIC_RELEASE);
}

// eventtap_wipe_ring_on_worker is the Release Step 3 wipe: it clears the
// ring FROM the worker thread instead of from the caller's.
//
// Why not "drain, then memset here": at Step 3 the tap source is still
// attached to the worker loop. A drain proves no callback is running at the
// instant it returns, but a mach message already queued on the tap port can
// still be delivered afterwards — CFRunLoopPerformBlock enqueues work for a
// future loop cycle, it is not a barrier over the loop's sources. The memset
// would then be racing a callback whose ring append is a pair of PLAIN
// stores: a genuine C data race, not merely a benign one, and no later clean
// wipe can retroactively undo undefined behaviour.
//
// Running the wipe as the block body removes the race instead of narrowing
// it. The callback is a run-loop callout on this same thread and a queued
// block is executed by that same thread, so the two CANNOT overlap — the
// serialisation comes from the thread, not from a handshake, and it holds
// for callbacks queued before AND after the block. What remains is
// repopulation (a callback dispatched after the block records a fresh press
// into the wiped ring), which is not UB, is not read by anyone — Release
// stopped and joined the poller before calling this — and is mopped up by
// the airtight wipe at the tail of `eventtap_uninstall_c`, taken there after
// the source is detached and the drain has confirmed.
//
// Returns 0 if the ring was wiped, 1 if the handshake timed out and it was
// NOT. On the 1 path the caller deliberately does nothing else: the point of
// this early wipe is to shorten the window in which the just-typed unlock
// code sits in process memory, and falling back to a direct memset would
// re-introduce exactly the data race this function exists to remove. The
// uninstall-time wipe a few CF calls later is an independent second chance
// — not a guarantee: it is itself gated on `drained == 0` there, so a run
// where BOTH handshakes time out leaves the ring resident and logs a WARN.
// Two chances beat one plus undefined behaviour.
//
// With no worker loop (install-rollback paths) the wipe is done inline and 0
// returned: no loop means no callback can be dispatched, so there is no
// writer to serialise against.
int eventtap_wipe_ring_on_worker(void) {
    CFRunLoopRef loop = g_worker_runloop;
    if (loop == NULL) {
        eventtap_wipe_ring();
        return 0;
    }
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    CFRunLoopPerformBlock(loop, kCFRunLoopCommonModes, ^{
        eventtap_wipe_ring();
        dispatch_semaphore_signal(sem);
    });
    CFRunLoopWakeUp(loop);
    if (dispatch_semaphore_wait(sem,
            dispatch_time(DISPATCH_TIME_NOW, (int64_t)DND_DRAIN_TIMEOUT_NS)) != 0) {
        return 1;
    }
    return 0;
}

// eventtap_install_c installs the CGEventTap at kCGHIDEventTap with
// kCGHeadInsertEventTap placement and the suppression-capable
// kCGEventTapOptionDefault (all three constants are grep-pinned by
// the acceptance criteria).
//
// Returns 0 on success; non-zero on failure (Go side wraps in
// ErrTapInstallFailed):
//
//   1 — CGEventTapCreate returned NULL. Triggers per errors.go comment:
//       Accessibility revoked, SecureEventInput active, or kernel out of mach
//       ports (Daniel Raffel TIL 2026-02-19).
//   2 — CFMachPortCreateRunLoopSource returned NULL. Extremely rare —
//       indicates CoreFoundation allocator exhaustion. We tear down the tap
//       and reset `g_tap` to NULL so a retry starts from a clean slate.
//
// Takes NO description of the unlock code. The C side never learns what the
// secret is: the callback records every non-autorepeat press into the ring
// and the Go poller does all the comparing against its `matcher.Sequence`.
// The former `flags` / `keycode` parameters were the last trace of the
// compare-in-C mechanism and are gone with it — a multi-step code has no
// single (flags, keycode) pair to hand over anyway.
//
// The function:
//   1. Empties the keystroke ring BEFORE creating the tap — once the source
//      is added to the run loop, the callback may fire on the very first
//      event, and the poller must not see a record from a previous session.
//   2. Builds the 15-bit event mask via CGEventMaskBit() over the table in
// the design notes (KeyDown/Up, FlagsChanged, all 9 mouse events, MouseMoved,
//      ScrollWheel, NX_SYSDEFINED for media keys).
//   3. Creates the tap with suppression-capable Default option (NOT
//      ListenOnly — that downgrades to Input Monitoring permission and we
//      need Accessibility to block events).
//   4. Creates the run-loop source (NOT added to the loop here — the worker
//      goroutine does that via `eventtap_register_worker_runloop` after it
//      calls `runtime.LockOSThread()` and obtains its own CFRunLoop).
//   5. Enables the tap — CGEventTapCreate returns a disabled tap; we MUST
//      explicitly enable before events flow.
int eventtap_install_c(CFMachPortRef *out_tap) {
    // Start every session with an empty ring. This happens BEFORE the tap
    // exists, so the callback cannot be racing us here. Zeroing matters
    // for more than tidiness: a stale record left over from a previous
    // Install would sit inside the first window the poller examines and
    // could complete an unlock code the user never finished typing. The
    // counter reset inside eventtap_wipe_ring is a RELEASE store, so the
    // memset is ordered before any subsequent ACQUIRE load by the reader.
    eventtap_wipe_ring();

    // 15 event types per the design notes block every keyboard, mouse,
    // scroll, and system-defined (media) event the tap can see.
    CGEventMask mask =
        CGEventMaskBit(kCGEventKeyDown)          |
        CGEventMaskBit(kCGEventKeyUp)            |
        CGEventMaskBit(kCGEventFlagsChanged)     |
        CGEventMaskBit(kCGEventLeftMouseDown)    |
        CGEventMaskBit(kCGEventLeftMouseUp)      |
        CGEventMaskBit(kCGEventLeftMouseDragged) |
        CGEventMaskBit(kCGEventRightMouseDown)   |
        CGEventMaskBit(kCGEventRightMouseUp)     |
        CGEventMaskBit(kCGEventRightMouseDragged)|
        CGEventMaskBit(kCGEventOtherMouseDown)   |
        CGEventMaskBit(kCGEventOtherMouseUp)     |
        CGEventMaskBit(kCGEventOtherMouseDragged)|
        CGEventMaskBit(kCGEventMouseMoved)       |
        CGEventMaskBit(kCGEventScrollWheel)      |
        CGEventMaskBit(NX_SYSDEFINED);            // 14 — media/function keys

    // Local first, published to the static via one atomic store. No callback
    // can run yet (the source below is not attached to any loop until the
    // worker goroutine registers it), so the ordering is not what matters
    // here — what matters is that `g_tap` has no plain accesses anywhere in
    // the file, because a single plain one re-opens the race the callback's
    // atomic load exists to close. See the declaration comment.
    CFMachPortRef tap = CGEventTapCreate(
        kCGHIDEventTap,                  // before WindowServer Bluetooth-safe
        kCGHeadInsertEventTap,           // front of chain — first to see events
        kCGEventTapOptionDefault,        // suppression-capable (NOT ListenOnly)
        mask,                            //
        eventtap_callback,
        NULL);                           // refcon — unused (uses statics)
    if (tap == NULL) {
        return 1;
    }
    __atomic_store_n(&g_tap, tap, __ATOMIC_RELAXED);

    g_source = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, tap, 0);
    if (g_source == NULL) {
        CFRelease(tap);
        __atomic_store_n(&g_tap, NULL, __ATOMIC_RELAXED);
        return 2;
    }

    // CGEventTapCreate returns the tap in a DISABLED state. Without this
    // call, the source can be added to a run loop but no callback ever
    // fires — and the silent failure looks identical to the post-install
    // success path until the first key press never matches.
    CGEventTapEnable(tap, true);

    *out_tap = tap;
    return 0;
}

// eventtap_register_worker_runloop is called from the worker goroutine AFTER
// it calls `runtime.LockOSThread()`. It captures `CFRunLoopGetCurrent()`
// (which is the fresh run loop of the locked OS thread — empty by default)
// and adds the tap source to it with kCFRunLoopCommonModes (the design notes
//).
//
// The captured `g_worker_runloop` is the handle that `eventtap_uninstall_c`
// uses to stop the loop on Release — `CFRunLoopStop` is documented
// thread-safe so it can be called from the main goroutine teardown path even
// though the loop runs on the worker thread.
//
// CFRetain, and it is load-bearing rather than defensive. A run loop is
// owned by its thread: CoreFoundation keeps it in thread-local storage and
// the TSD finalizer releases it when the thread dies. The worker thread dies
// on its own inside `eventtap_uninstall_c` — `CFRunLoopRemoveSource` there
// takes the LAST source off the loop (the gesture source was detached one
// step earlier), and the drain's `CFRunLoopWakeUp` makes the now-empty loop
// return from `CFRunLoopRun` on that same iteration. The worker goroutine
// then returns, Go reaps its LockOSThread thread, and without a reference of
// our own the loop is deallocated while `eventtap_uninstall_c` is still
// several CF calls away from its `CFRunLoopStop(g_worker_runloop)` — a
// use-after-free racing `CGEventTapEnable`'s WindowServer round-trip, which
// writes through a freed object rather than trapping on it. Holding a
// reference makes the pointer valid no matter when the thread exits;
// `CFRunLoopStop` on a loop that already finished is a no-op.
//
// `out_loop` mirrors `g_worker_runloop` back to Go so the Releaser struct
// can hold a Go-side opaque pointer alongside the C-side static, which lets
// the unit tests assert that the field is non-nil after Install.
//
// Returns 0 unconditionally — `CFRunLoopGetCurrent` cannot fail and
// `CFRunLoopAddSource` has no error return. The integer return type is
// preserved for symmetry with `eventtap_install_c` and future-proofing.
int eventtap_register_worker_runloop(CFMachPortRef tap, CFRunLoopRef *out_loop) {
    (void)tap; // tap is captured via the static `g_tap` set in install_c.

    g_worker_runloop = (CFRunLoopRef)CFRetain(CFRunLoopGetCurrent());
    if (g_source != NULL) {
        CFRunLoopAddSource(g_worker_runloop, g_source, kCFRunLoopCommonModes);
    }
    if (out_loop != NULL) {
        *out_loop = g_worker_runloop;
    }
    return 0;
}

// eventtap_uninstall_c is the symmetric teardown of `eventtap_install_c` +
// `eventtap_register_worker_runloop`. Order matches the design notes:
//
//   1. Tap is already disabled by the Go-side `Releaser.Release()` BEFORE
//      this call (disable-first ordering — keyboard recovers immediately
//      even if any of the steps below fail).
//   2. Remove the run-loop source from the worker loop — once removed, the
//      tap source mach-port stops being polled by the loop.
//   3. CFRelease the source (we own one reference; the run loop's reference
//      was dropped by RemoveSource above).
//   4. Disable + CFRelease the tap mach-port. CGEventTapEnable(tap, false)
//      is idempotent so the Go-side disable + this disable double-call is
//      safe; we re-call here for defensive symmetry.
//   5. Stop the worker run loop — Run() returns, the worker goroutine exits,
//      Go runtime reaps the locked OS thread.
//
// All globals are nulled out so a hypothetical re-Install starts from a
// clean slate.
//
// Idempotency: nil-checks on every CF release path mean it is safe to call
// this with a NULL tap or after a prior call — the Go-side two-layer guard
// already serialises Release callers, but the C side is defensive in case
// of future test fixtures that exercise this directly.
//
// RETURN VALUE: 0 on the normal path, 1 when the post-detach drain timed out
// and the source + tap were therefore LEFT RETAINED instead of released. The
// leak is the deliberate choice: a timeout means the callback cannot be
// proven idle, CFReleasing the mach port out from under one sitting in its
// `CGEventTapEnable(g_tap, true)` self-heal branch is a use-after-free that
// crashes the process, and one leaked mach port on a teardown path that runs
// once at exit costs nothing anyone can measure.
//
// The rule the timeout path follows is broader than the CFReleases, and it
// is the whole reason to read this function twice: on `drained != 0` NOTHING
// callback-visible is written non-atomically. The source + tap are left
// retained (above), the ring wipe is skipped (below — a memset overlapping
// the callback's plain ring stores is a data race, not a benign one), and
// the one write that does still happen, `g_tap = NULL`, goes through
// __atomic. What remains is the tap disabled, g_source / g_worker_runloop
// nulled (neither is read by any callback) and the run loop stopped via
// CFRunLoopStop, which Apple documents as thread-safe. So the consequences
// of a timeout are exactly two — a leaked mach port and a ring left
// resident — and the Go caller logs the 1 (at WARN from `uninstallFn`,
// where the worker loop is alive and a timeout is a fault; at DEBUG from
// the install-rollback call site, where the worker died before
// CFRunLoopRun and a timeout is the expected outcome) rather than letting
// either pass unobserved.
int eventtap_uninstall_c(CFMachPortRef tap) {
    if (g_worker_runloop != NULL && g_source != NULL) {
        CFRunLoopRemoveSource(g_worker_runloop, g_source, kCFRunLoopCommonModes);
    }
    // Drain BEFORE anything is released. With the source detached above, no
    // further callback can be dispatched, so once this returns 0 the callback
    // is provably not running and provably cannot start again. That buys two
    // things on one line: the CFRelease(tap) below cannot pull the mach port
    // out from under a callback sitting in its `CGEventTapEnable(g_tap, true)`
    // self-heal branch, and the eventtap_wipe_ring at the tail of this
    // function has no writer left to race.
    //
    // A non-zero return means the handshake timed out and neither of those
    // two facts is established, so the CFReleases AND the tail wipe are
    // SKIPPED — see the return-value paragraph above for why leaking beats
    // releasing, and the wipe's own comment for why a memset under a
    // possibly-live callback is not the harmless half of that trade.
    int drained = eventtap_drain_worker_callbacks();
    // Publish the teardown BEFORE the final disable, not after. The order of
    // these two statements is the whole correctness argument for the
    // callback's post-enable re-check: a callback that is still live on the
    // timeout path re-reads g_tap after its own CGEventTapEnable(true), and
    // if our NULL had not landed yet it would keep the tap enabled while our
    // disable had already gone by — an enabled tap with a detached source,
    // i.e. stalled input until WindowServer's own tap timeout. With the NULL
    // first, either the callback's re-check sees it and undoes the enable, or
    // the enable preceded our disable and the disable wins. Both orders end
    // with the tap disabled.
    //
    // g_tap is the callback-visible global, so the store is ATOMIC — on the
    // timeout path it is a genuinely concurrent write and the callback loads
    // the same location. See the declaration comment.
    __atomic_store_n(&g_tap, NULL, __ATOMIC_RELAXED);
    if (tap != NULL) {
        // Safe on both paths: disabling a retained mach port cannot free
        // anything, and it is idempotent per Apple's documentation.
        CGEventTapEnable(tap, false);
    }
    if (drained == 0) {
        if (g_source != NULL) {
            CFRelease(g_source);
        }
        if (tap != NULL) {
            CFRelease(tap);
        }
    }
    // g_tap was nulled above, before the disable — see the comment there for
    // why that ordering is load-bearing rather than cosmetic. It is nulled on
    // BOTH paths: whether the objects were released or deliberately leaked,
    // this process no longer owns a usable tap, and a hypothetical re-Install
    // must start from a clean slate rather than inherit a handle to the
    // abandoned one.
    //
    // g_source is left plain: no callback reads it (see the declaration
    // comment), and install/teardown are serialised onto one goroutine by the
    // Go-side Release guard.
    g_source = NULL;
    if (g_worker_runloop != NULL) {
        // CFRunLoopStop is documented thread-safe — safe to invoke from the
        // main goroutine while the worker goroutine is blocked in
        // CFRunLoopRun. The worker loop wakes, Run() returns, the goroutine
        // exits, and Go reaps its locked OS thread.
        //
        // It is also reached in the case where the loop has ALREADY returned
        // on its own: the drain above wakes a loop whose last source we just
        // removed, and an empty mode ends `CFRunLoopRun`. That is a no-op
        // here rather than a use-after-free only because
        // `eventtap_register_worker_runloop` CFRetained the loop — see the
        // paragraph there. The matching CFRelease is below, on BOTH the
        // drained and the timed-out path: the reference exists to keep this
        // pointer valid for the CFRunLoopStop call, and nothing reads
        // g_worker_runloop after it (no callback ever does).
        CFRunLoopStop(g_worker_runloop);
        CFRelease(g_worker_runloop);
        g_worker_runloop = NULL;
    }

    // Wipe the recorded key presses — but ONLY on the drained == 0 path,
    // exactly like the CFReleases above. With the source detached and the
    // drain confirmed, the callback is not running and cannot start again:
    // there is no writer left to race, which makes this the one wipe in the
    // process that is provably clean.
    // This is hygiene, not correctness — Release Step 3 already attempted
    // the wipe, and the install-time rollback paths never recorded anything —
    // but the ring holds the tail of what the user typed, including the
    // unlock code they just entered, and there is no reason to leave it
    // resident for the remainder of the process lifetime. Going through
    // eventtap_wipe_ring rather than an open-coded memset keeps the counter
    // reset attached to the wipe; see its call-site contract.
    //
    // An earlier revision ran this wipe unconditionally and argued the
    // asymmetry with the CFReleases from the failure mode: freeing a CF
    // object under a live callback corrupts memory it executes against,
    // whereas a memset of a naturally-aligned static array under one only
    // scrambles a record nobody reads. The second half of that is a
    // statement about arm64 code generation, not about C: the callback's
    // ring append is a pair of PLAIN stores, so an overlapping memset is a
    // data race and therefore undefined behaviour outright — the same
    // argument this file already makes in `eventtap_wipe_ring_on_worker`
    // when it refuses to fall back to a direct memset for identical
    // reasons. Making the wipe conditional is what keeps the two consistent,
    // and it is what puts this call inside eventtap_wipe_ring's stated
    // call-site contract ("only where neither a tap callback NOR the poller
    // can be running") instead of alongside it.
    //
    // What the timeout path gives up is bounded and small. On the only
    // routinely-reachable timeout (an install rollback whose worker died
    // before CFRunLoopRun) the ring is empty anyway — nothing was ever
    // recorded. On the other one (a live worker descheduled past the 100ms
    // bound) the secret stays resident for the remainder of the process
    // lifetime, which on every path that reaches Release is milliseconds:
    // the tap is torn down either at exit or on an install-rollback that
    // returns a fatal error. Trading undefined behaviour for that is the
    // right way round.
    if (drained == 0) {
        eventtap_wipe_ring();
    }
    return drained;
}

// eventtap_is_enabled wraps `CGEventTapIsEnabled` for the watchdog probe
//. Returns 1 if enabled, 0 if disabled. nil-tap returns 0.
//
// This is the only health probe Daniel Raffel's TIL identified as reliable
// against the silent-disable race (CGEventTapCreate returns non-NULL on a
// dead tap when the ad-hoc identity has been re-signed without a TCC
// refresh; only this call detects the dead state).
int eventtap_is_enabled(CFMachPortRef tap) {
    if (tap == NULL) {
        return 0;
    }
    return CGEventTapIsEnabled(tap) ? 1 : 0;
}

// eventtap_enable wraps `CGEventTapEnable` for the disable-first Release
// path and the watchdog re-enable path. The underlying
// Quartz call is documented idempotent so the Go-side concurrent-cleanup
// guard does not need to debounce repeated invocations.
void eventtap_enable(CFMachPortRef tap, int enable) {
    if (tap == NULL) {
        return;
    }
    CGEventTapEnable(tap, enable != 0);
}

// removed the eventtap_test_set_expected test-only setter. It had
// no caller (no Go-side `C.eventtap_test_set_expected` invocation in
// tap_test.go or anywhere else), so it was dead in both the production
// AND test binary. Keeping it always-compiled added a small but real
// attack surface: a process-injected adversary could rewrite the
// (expected_flags, expected_keycode) globals of the day at runtime without
// going through Accessibility / Install / a configured Spec, locking out
// the legitimate user with a custom hotkey. (Those globals are gone
// entirely now — the configured code lives only on the Go side — but the
// reasoning below still governs any future test-only C helper, and the
// same conclusion was reached again when a ring-injection helper was
// considered and rejected for the sequence matcher.) The "1KB binary
// savings" comment optimized the wrong variable (correctness/security >
// size). If a future test wants the setter back, prefer either (a) a build-tag
// gated `*_darwin_test.m` companion file that ships ONLY in test
// binaries, or (b) a `#ifdef DNDMODE_TEST_HELPERS` guard wired to a
// dedicated `#cgo test_helpers CFLAGS: -DDNDMODE_TEST_HELPERS`. Both
// keep the helper out of the production binary.
