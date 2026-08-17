//go:build darwin

package eventtap

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

// tap_ring.h is the shared declaration of the keystroke ring — the same
// header tap_darwin.m includes. It is what makes `C.dnd_keyrec_t` exist on
// this side; without it the snapshot below would need a hand-written second
// copy of the record layout and of the capacity, and a silent drift between
// the two would either truncate every snapshot or memcpy past the end of the
// Go buffer. ring_guard_test.go pins DND_RING_CAP against the Go-side
// `ringCap` for the same reason.
#include "tap_ring.h"

extern int  eventtap_install_c(CFMachPortRef *out_tap);
extern int  eventtap_register_worker_runloop(CFMachPortRef tap, CFRunLoopRef *out_loop);
extern int  eventtap_uninstall_c(CFMachPortRef tap);
extern int  eventtap_is_enabled(CFMachPortRef tap);
extern void eventtap_enable(CFMachPortRef tap, int enable);

// The keystroke-ring readers. `eventtap_seq` is the cheap per-tick probe
// (a bare ACQUIRE load); `eventtap_snapshot` copies the whole ring into a
// caller-provided buffer with room for DND_RING_CAP records and returns the
// press count the copy describes. `eventtap_wipe_ring` clears the ring and
// the counter and is callable ONLY from the Release path — see its contract
// in tap_darwin.m.
extern uint64_t eventtap_seq(void);
extern uint64_t eventtap_snapshot(dnd_keyrec_t *out);
extern void     eventtap_wipe_ring(void);

// eventtap_drain_worker_callbacks and eventtap_wipe_ring_on_worker — the two
// writer-quiescence primitives — are declared in tap_ring.h (included above)
// rather than here, because gesturetap_darwin.m calls the drain too and one
// shared prototype is what makes a signature change fail to compile in all
// three places instead of in none. Both return 0 on success and 1 on a
// handshake timeout; both returns are acted on at their Go call sites.

// eventtap_set_observed_tap is the canonical writer for the shared
// `CFMachPortRef g_observed_tap` global that lives in watchdog_darwin.m and
// is read by both the watchdog GCD handler AND the NSWorkspace
// wake-observer blocks (wake_darwin.m). Every access on both ends goes
// through `__atomic_*`. Release Step 1 writes NULL via this setter, which
// makes any handler STARTING after that point a no-op; handlers already
// in flight are handled by Step 2 (watchdog_stop drains them) rather than
// by this write, which cannot reach a pointer already loaded.
extern void eventtap_set_observed_tap(CFMachPortRef tap);

// gesturetap_* is the session-level trackpad-gesture suppression tap
// (gesturetap_darwin.m) — the second tap that closes the Mission Control /
// App Exposé / Spaces / Launchpad swipe leak. Dock gestures are synthesized
// by WindowServer PAST the HID tap point and exist only as session-level
// CGS events (types 29/30), so the kCGHIDEventTap in tap_darwin.m can never
// see them. Installed by installInternal onto the SAME worker run loop as
// the main tap source (hs.loop), torn down via the Releaser closures with a
// strict gesture-before-main uninstall order (see Releaser field docs).
extern int  gesturetap_install_c(CFRunLoopRef loop);
extern void gesturetap_disable_c(void);
extern int  gesturetap_uninstall_c(void);

// cf_to_void_ptr is the package-private C helper that converts a cgo
// opaque pointer (CFMachPortRef) to a `void *` (which cgo maps to Go's
// `unsafe.Pointer`). Defined inline because a direct
// `unsafe.Pointer(C.CFMachPortRef)` cast in Go trips `go vet -unsafeptr`
// even though the conversion is well-defined for cgo opaque handles —
// CFMachPort is reference-counted by CoreFoundation, the Go GC never
// sees it, and the standard "uintptr-pointer caveat" does not apply.
// Routing the conversion through C via this helper keeps vet quiet
// without losing type safety. Used by InstallAll to pass the tap pointer
// to (StartWatchdog) and (InstallWakeObserver) which
// both accept `unsafe.Pointer` (kept that way so they don't impose a
// cgo dependency on test fixtures that may want to fake the tap).
static inline void *cf_to_void_ptr(CFMachPortRef tap) {
    return (void *)tap;
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// There is deliberately NO //export function in this file. The CGEventTap
// callback used to call one (`eventtap_matched`, whose body was a single
// atomic store into a package-level latch); the comparison now happens on
// the poller goroutine, so the callback calls into Go zero times. That is
// the strongest form of the nosplit invariant documented on
// `eventtap_callback` in tap_darwin.m, and re-adding an //export used from
// the callback would weaken it. The Go→C direction (seq / snapshot below)
// is unaffected: those run on the poller goroutine, which is an ordinary
// Go-scheduled goroutine making an ordinary cgo call.

// teardownUnclean latches the teardown outcome that makes the C-side statics
// unsafe to reuse: a drain that timed out with a C-side callback still
// possibly running. Two paths set it — the tap's own drain handshake when it
// times out while the worker run loop is ALIVE (i.e. where
// `eventtap_callback` could not be proven idle), and the watchdog's
// cancel-handler drain in `StartWatchdog`'s stop closure. The argument below
// is written for the first; see that closure for the second, which differs
// only in WHICH stale writer the latch fences off.
//
// The C side answers such a timeout by touching nothing callback-visible
// non-atomically — it leaks the mach port instead of releasing it and skips
// its tail ring wipe — but it still nulls `g_worker_runloop` / `g_source`
// and returns, so nothing in C remembers that a writer may be outstanding. A
// later `installInternal` would then run `eventtap_wipe_ring()` (a memset of
// the shared ring) and publish a fresh `g_tap` while that old callback is
// mid-append: the memset would race the callback's pair of PLAIN stores —
// the exact C data race the timeout handling exists to avoid — and the new
// non-NULL `g_tap` would additionally defeat the old callback's post-enable
// teardown re-check, which reads that same location and treats non-NULL as
// "my tap is still current".
//
// So the latch is one-way and process-wide: once a teardown could not prove
// the callback idle, this process may not install a tap again. That costs
// nothing in production — `InstallAll` runs exactly once per process
// (cmd/dndmode/main.go) and this path ends in an exit either way — and it
// converts an unobservable data race into an explicit ErrTeardownUnclean.
//
// Deliberately NOT set on the worker-handshake rollback path, which is the
// same distinction the WARN / DEBUG split at those two call sites already
// draws: there the worker goroutine died before `CFRunLoopRun`, so the loop
// dispatches no callbacks at all and a timeout means "nobody is servicing
// blocks", not "a writer may be live".
//
// The watchdog path latches for the same shape of reason on a different
// stale writer: a GCD probe handler that outlived its bounded cancel drain
// re-reads `g_gesture_tap` inside `gesturetap_reenable` and can flip the
// package-global `watchdogThresholdHit`, so a second session would inherit
// both — a gesture port the stale handler never retained, and a threshold
// notification carrying no session identity. Blocking re-install is what
// makes those unreachable; the C-side generation and pointer-identity guards
// in watchdog_darwin.m narrow the window but cannot close it, because the
// deschedule can always land after the last check.
var teardownUnclean atomic.Bool

// seq returns the number of key presses the C callback has recorded since
// Install. It is the poller's cheap per-tick probe: an unchanged value means
// no new keystroke, so the snapshot (and its ring-sized memcpy) can be
// skipped entirely.
func seq() uint64 {
	return uint64(C.eventtap_seq())
}

// newSnapshotFn returns the poller's snapshot function: it copies the C-side
// keystroke ring into buf and returns the press count that copy describes —
// the `to` bound of the half-open range [previous, to) the caller may safely
// consume.
//
// buf MUST have room for ringCap records and is written in place: the poller
// allocates it once, before its loop, and reuses it on every tick, so this
// path stays allocation-free per the contract in matcher.go. A short buffer
// is a programming error, not a runtime condition — the C memcpy is sized
// from DND_RING_CAP and would write past the end of a smaller Go slice, so
// the length is checked here and panics rather than corrupting the heap.
// The only production caller passes `make([]matcher.KeyEvent, ringCap)`.
//
// The C records land in a staging [ringCap]C.dnd_keyrec_t and are converted
// field-by-field: the Go and C layouts are pinned to the same widths by
// ring_guard_test.go, but converting explicitly rather than reinterpreting
// the memory keeps the whole thing free of unsafe and survives any future
// padding change in either language.
//
// That staging array is why this is a constructor and not a plain function.
// Declared inside the call it is 1 KiB that escapes to the heap on every
// invocation (`&cring[0]` crosses the cgo boundary, so escape analysis has
// to assume the worst) — an allocation on the one path that the "no
// allocations in hot path" contract exists to protect. Bound to the closure
// it is allocated once at Install time instead.
//
// The returned function is NOT safe for concurrent use: the staging array is
// shared across calls. There is exactly one caller — the poller goroutine
// created alongside it in installInternal.
func newSnapshotFn() func(buf []matcher.KeyEvent) uint64 {
	cring := new([ringCap]C.dnd_keyrec_t)
	return func(buf []matcher.KeyEvent) uint64 {
		if len(buf) < ringCap {
			panic("eventtap: snapshot buffer shorter than ringCap")
		}
		cur := uint64(C.eventtap_snapshot(&cring[0]))
		for i := range cring {
			buf[i] = keyEventFromRecord(uint64(cring[i].flags), uint16(cring[i].keycode))
		}
		// Zero the staging array as soon as its contents are converted.
		// Bound to the closure, it outlives every call, so without this it
		// would hold a full copy of the recorded keystrokes (ending with the
		// unlock code) for as long as the tap is installed — and past
		// eventtap_wipe_ring, which only clears the C side. The memset is
		// 1 KiB against a 1 KiB memcpy already performed on this line, and
		// only on ticks where the counter actually moved.
		clear(cring[:])
		return cur
	}
}

// keyEventFromRecord is the field mapping between one C ring record and its
// Go mirror: `flags` (already masked with USER_INTENTIONAL_MASK by the
// callback) becomes Modifiers, `keycode` becomes KeyCode.
//
// Split out of snapshot as a plain function with plain integer parameters so
// the mapping is unit-testable: cgo is not usable in _test.go files at all,
// so a test can never construct a C.dnd_keyrec_t to feed the conversion.
// Everything that can be checked without cgo — which field goes where, and
// that neither width truncates — is checked here instead.
//
// The Modifiers value is NOT re-masked. matcher.Sequence.MatchTail masks the
// event side itself precisely so it does not have to trust the C side, and
// masking twice here would hide a C-side regression from the Go tests rather
// than expose it.
func keyEventFromRecord(flags uint64, keycode uint16) matcher.KeyEvent {
	return matcher.KeyEvent{
		Modifiers: hotkey.ModFlag(flags),
		KeyCode:   keycode,
	}
}

// Releaser is the active CGEventTap handle and implements state.Releaser
// (Release() error + Name() string). It is produced by Install and consumed
// by main.go's RestoreState LIFO Cleanup chain (tap is released first
// to restore input, then the watchdog timer cancels, then the wake observer
// unsubscribes).
//
// Two-layer idempotency mirrors powerassert.Assertion (assertion.go lines
// 163-188): atomic.Bool fast-path Load short-circuits repeat Cleanup
// invocations; sync.Mutex slow-path serialises concurrent first-time
// callers so that NO caller returns before the underlying CGEventTapEnable
// / CFRelease / dispatch_source_cancel chain completes. This matters in
// the shutdown path where the ctx-watcher goroutine and the
// supervisor cleanup chain may both invoke Release nearly simultaneously.
//
// Raw `CFMachPortRef` / `CFRunLoopRef` указатели НЕ хранятся как поля
// Releaser'а: они закапсулированы в `disableFn` / `uninstallFn` closures,
// которые Install создаёт в момент успешного `eventtap_install_c`. Это
// позволяет
//
//   1. Избежать `go vet` warning "possible misuse of unsafe.Pointer" при
//      конверсии `unsafe.Pointer(C.CFMachPortRef)` (cgo pointer types —
//      специальный случай; vet не отличает легитимное хранение opaque
//      handle от арифметики над указателем).
//   2. Сохранить cgo-pointer ownership внутри closure'ов, привязанных к
//      install-time call frame — `Release` не может случайно «обнулить»
//      указатель до того, как Cleanup отработает.
//
// `source` (CFRunLoopSourceRef) тоже не хранится в Go — `eventtap_uninstall_c`
// использует C-side static global `g_source`.
type Releaser struct {

	// log is the slog.Logger used for diagnostics during Release.
	// Mirror of powerassert.Acquire's logger-fallback convention:
	// nil → slog.Default().
	log *slog.Logger

	// design note: the install-time CFMachPortRef is NOT stored
	// on the Releaser. The closure-encapsulation pattern (disableFn
	// / clearObservedFn / uninstallFn) keeps cgo opaque handles inside
	// closure captures so:
	//
	//   1. `go vet -unsafeptr` stays clean (storing
	//      `unsafe.Pointer(C.CFMachPortRef)` on a struct field trips the
	//      heuristic even though the use is idiomatic for cgo opaque
	//      handles; uintptr storage would sidestep vet but requires a
	//      vet-flagged conversion at every use site).
	//   2. InstallAll passes cTap to StartWatchdog / InstallWakeObserver
	//      directly inside its own scope (the package-private
	//      `installInternal` helper exposes cTap to InstallAll without
	//      crossing the public API boundary).
	//
	// As a result, this struct has NO `tap` field — the C side owns the
	// canonical tap reference (via the static globals `g_tap` in
	// tap_darwin.m and `g_observed_tap` in watchdog_darwin.m).

	// stopPoller signals the poller goroutine to exit cleanly. Closed by
	// Release; the goroutine selects on it and returns. Cap is 0 (a
	// signalling channel — close-only semantics, no payload).
	stopPoller chan struct{}

	// pollerDone is closed by the poller goroutine when it exits. Release
	// waits on it after closing stopPoller so the cleanup chain returns
	// only after the goroutine has actually unwound — important under
	// `-race` where a still-running goroutine would surface as a leak.
	pollerDone chan struct{}

	// disableFn / clearObservedFn / uninstallFn are the DI seams that let
	// unit tests substitute fakes for the C-side bridge calls. Production
	// wires them at Install time (see Install) or via the test-internal
	// `newReleaserWithDeps` constructor (tap_test.go). NOT exported —
	// production callers MUST go through Install / InstallAll.
	//
	// Order in `Release` corresponds to Step 1 (disableFn
	// clearObservedFn) and Steps 2-3 (uninstallFn handles
	// CFRunLoopRemoveSource → CFRelease(source) → CFRelease(tap) inside
	// the existing C-side `eventtap_uninstall_c`).
	disableFn       func()
	clearObservedFn func() // writes NULL to g_observed_tap atomically
	uninstallFn     func()

	// gestureDisableFn / gestureUninstallFn tear down the SECOND tap — the
	// session-level trackpad-gesture suppressor (gesturetap_darwin.m) that
	// closes the Mission Control / App Exposé / Spaces swipe leak the HID
	// tap cannot see. Ordering constraints inside Release:
	//
	//   - gestureDisableFn runs in Step 1 right after disableFn +
	//     clearObservedFn — trackpad gestures recover together with the
	//     keyboard even if later CF teardown fails;
	//   - gestureUninstallFn runs at Step 4, BEFORE uninstallFn: both tap
	//     sources share the worker run loop, and uninstallFn ends with
	//     CFRunLoopStop — after that the worker thread exits and the loop
	//     ref dangles, so the gesture source must leave the loop first.
	//
	// nil in test constructors that don't exercise the gesture path
	// (Release nil-guards both).
	gestureDisableFn   func()
	gestureUninstallFn func()

	// wipeRingFn clears the C-side keystroke ring (and its press counter).
	// It runs at the END of Step 3, once BOTH taps are disabled (Step 1) and
	// the poller — the ring's only reader — has been joined: the ring holds
	// the tail of what the user typed, ending with the unlock code itself,
	// and there is no reason to leave it resident for the rest of the
	// process lifetime.
	//
	// "Both taps are disabled" is necessary but not sufficient: a callback
	// dispatched before the disable landed is still running, a queued mach
	// message can produce another one after any handshake this side could
	// wait on, and both write the ring with plain stores. The production
	// closure therefore does not memset from the Go goroutine at all — it
	// calls eventtap_wipe_ring_on_worker(), which performs the wipe on the
	// worker thread where the callback cannot run concurrently with it. See
	// the comment at the closure and the full contract in tap_darwin.m.
	//
	// Release is the ONLY permitted call site. Calling it from the poller
	// would make the poller a second writer to the ring while the tap is
	// still live, which is precisely the premise `eventtap_snapshot`'s
	// correctness argument rules out (see its comment in tap_darwin.m).
	//
	// nil in test constructors that don't exercise the ring (Release
	// nil-guards it).
	wipeRingFn func()

	// watchdogStop / wakeStop are the tear-down
	// closures returned by `StartWatchdog` and `InstallWakeObserver`
	// respectively. Set by `InstallAll`. The plain `Install` constructor
	// (surface) does NOT set these — they remain nil and the
	// nil-check in Release short-circuits them. Production callers MUST go
	// through `InstallAll`; `Install` is kept for the smoke-test path that
	// exercises tap + poller without the watchdog/wake composites.
	//
	// Order: BOTH run at Step 2 — after the Step 1 disables, before every
	// CFRelease in Steps 4-5. They are the two subsystems that call
	// CGEventTapEnable on the taps, so leaving them running across the
	// teardown is what makes a freed mach port reachable; the Step 1
	// g_observed_tap NULL write narrows that window but cannot close it,
	// because a handler that already loaded the pointer never re-reads it.
	// watchdogStop drains in-flight GCD handlers synchronously (see
	// StartWatchdog); wakeStop relies on main-thread confinement (see the
	// extern comment in wake_darwin.m). An earlier revision ran these at
	// Steps 4-5, i.e. after the CFReleases — the exact inversion
	// StartWatchdog's docstring calls unsafe.
	watchdogStop func()
	wakeStop     func()

	// released is the fast-path hint flag — set to true AFTER the cgo
	// teardown chain has fully completed under mu. atomic.Load lets repeat
	// callers (e.g. ctx-watcher + Cleanup chain hitting Release in close
	// succession) skip the mutex entirely once teardown is permanently done.
	released atomic.Bool

	// mu serialises concurrent Release callers. Pre- style sync.Once
	// + atomic.Bool had a serialization race (callers returning before the
	// underlying release completed); the Mutex pattern documented in
	// powerassert/assertion.go is the canonical fix.
	mu sync.Mutex
}

// Name implements state.Releaser. Returns "eventtap" — used by main.go's
// LIFO Cleanup logger for "released releaser=eventtap" line which the
// acceptance test (Phase 1) parses to verify push order. Replaces
// the Phase 3 "mock-tap" placeholder once wire-up lands.
func (r *Releaser) Name() string { return "eventtap" }

// Release implements state.Releaser. Two-layer idempotency mirrors
// powerassert.Assertion.Release verbatim (fix pattern):
//
//  1. atomic.Bool fast-path Load — once released is durably true, any
//     repeat caller returns nil instantly without touching the mutex.
//  2. sync.Mutex slow-path — concurrent first-time callers serialise here;
//     the winner double-checks released under the mutex, performs the cgo
//     teardown, stores released=true, and releases mu. Losers block on
//     mu.Lock() until the winner is done, then see released==true under
//     mu and return nil without invoking the teardown.
//
// Teardown order (VERBATIM from the design notes, the design notes):
//
//  1. disableFn — CGEventTapEnable(tap, false) — keyboard recovers
//     immediately even if any subsequent step fails.
//     clearObservedFn — eventtap_set_observed_tap(NULL) — atomic write
//     that turns any watchdog GCD handler or wake-observer block STARTING
//     from here on into a no-op. It is a guard, not a lifetime mechanism:
//     a handler that already loaded the pointer is unaffected by a later
//     store, which is what Step 2 exists for.
//     gestureDisableFn — same for the session-level gesture tap, so
//     trackpad gestures recover together with the keyboard.
//  2. watchdogStop, then wakeStop — the self-healers come down BEFORE
//     anything is released, because both exist to call CGEventTapEnable on
//     ports the steps below free. watchdogStop cancels the GCD source and
//     BLOCKS until its cancel handler has run (libdispatch schedules that
//     only after the last event-handler invocation returns), so afterwards
//     no handler is running and none can start. wakeStop removes the two
//     NSWorkspace observers; their blocks are main-queue and this function
//     runs on the main goroutine, so none can be in flight. Both are nil
//     for a plain installTapOnly Releaser (smoke tests) and skipped.
//     This ordering is the inversion fix: these two used to run LAST,
//     after the CFReleases, contradicting StartWatchdog's own docstring.
//  3. close(stopPoller) + <-pollerDone — the unlock-code poller exits its
//     ticker loop within pollInterval (10ms) and is drained to a full
//     stop. This MUST run BEFORE wipeRingFn, not at the end of Release:
//     the poller is the ring's only reader, and a memset landing between
//     eventtap_snapshot's ACQUIRE load and its memcpy would hand the
//     poller a run of zeroed records (keycode 0 is kVK_ANSI_A). Draining
//     first also keeps `-race` quiet — a goroutine still reading the C
//     ring after Release returns would be flagged. Skipped if the channels
//     are nil (unit-test constructors that exercise only the
//     disable/uninstall path).
//     wipeRingFn — eventtap_wipe_ring_on_worker() — runs once both taps
//     are down and the ring's reader is gone; the recorded keystrokes
//     (which end with the unlock code) stop being resident. The wipe is
//     performed BY the worker thread rather than by this goroutine,
//     because the tap source is still attached here and a callback
//     dispatched from an already-queued mach message would otherwise
//     overlap the memset.
//  4. gestureUninstallFn — CFRunLoopRemoveSource → drain → CFRelease of
//     the gesture source + tap. MUST precede step 5, whose CFRunLoopStop
//     ends the worker loop both sources share.
//  5. uninstallFn — CFRunLoopRemoveSource → drain → CFRelease(source) →
//     CGEventTapEnable(tap, false) [defensive] → CFRelease(tap) →
//     CFRunLoopStop(worker_runloop), all bundled in the C-side
//     `eventtap_uninstall_c` (its comment enumerates the sub-steps).
func (r *Releaser) Release() error {
	// Fast path: hint flag. Cheap Load — once released is durably set
	// (after the winner stored it under mu), any repeat caller skips
	// the mutex entirely.
	if r.released.Load() {
		return nil
	}
	// Slow path: serialise concurrent first-time callers via the mutex.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check under the mutex — another goroutine may have won
	// between our Load and our Lock.
	if r.released.Load() {
		return nil
	}

	// --- Step 1: disable tap + atomic-null guard write ---
	// disableFn calls CGEventTapEnable(tap, false). Effective on the
	// kernel side immediately — keyboard recovers. clearObservedFn
	// writes NULL to g_observed_tap (volatile pointer store, atomic on
	// darwin/arm64). Both happen ON THE SAME LINE pair so a re-ordering
	// reviewer cannot accidentally separate them.
	if r.disableFn != nil {
		r.disableFn()
	}
	if r.clearObservedFn != nil {
		r.clearObservedFn()
	}
	// Gesture tap disable belongs to the same Step 1 contract: input
	// (including trackpad gestures) recovers immediately, before any CF
	// teardown below runs. The clearObservedFn NULL-write above also stops
	// the watchdog / wake observers from re-enabling the gesture tap via
	// gesturetap_reenable (their g_observed_tap guard runs first).
	if r.gestureDisableFn != nil {
		r.gestureDisableFn()
	}

	// --- Step 2: stop the self-healers, BEFORE anything is released. ---
	//
	// Both of them exist to call CGEventTapEnable on the two taps, so both
	// must be provably gone before the CF teardown below frees the mach
	// ports they hold pointers to. The g_observed_tap NULL write above is a
	// guard, not a lifetime mechanism: it stops a handler that has not read
	// the pointer yet and does nothing for one that read it a microsecond
	// earlier and was then descheduled — an already-loaded pointer is not
	// affected by a later store.
	//
	// watchdogStop therefore drains: it cancels the GCD source and blocks
	// until the source's cancel handler has run, which libdispatch schedules
	// only after the last event-handler invocation has returned. wakeStop
	// removes the two NSWorkspace observers; its blocks run on the main
	// queue and this whole function runs on the main goroutine, so no block
	// can be in flight while we are here (see the extern comment in
	// wake_darwin.m).
	//
	// This ordering is also what StartWatchdog's docstring has always
	// promised — "stop() first, then the tap Releaser; inverted order is
	// unsafe (a GCD handler in-flight may still call CGEventTapIsEnabled on
	// a freed port)". The composite Release used to run these two LAST, at
	// Steps 4-5 after the CFReleases, which is exactly the inversion that
	// docstring warns about.
	//
	// Both are nil for a plain installTapOnly Releaser (smoke tests) — only
	// InstallAll wires them.
	if r.watchdogStop != nil {
		r.watchdogStop()
		r.watchdogStop = nil
	}
	if r.wakeStop != nil {
		r.wakeStop()
		r.wakeStop = nil
	}

	// --- Step 3: drain the ring's reader, then wipe the ring. ---
	// Stop the unlock-code poller BEFORE the wipe below. The wipe is the
	// only place in the process that writes the ring from outside the tap
	// callback, so it must not overlap the one goroutine that reads it:
	// eventtap_snapshot takes its ACQUIRE load of g_seq and then memcpys
	// the ring in two separate steps, and a memset landing between them
	// hands the poller a zeroed window still labelled with the old count.
	// Every zeroed record reads as {flags: 0, keycode: 0} — keycode 0 is
	// kVK_ANSI_A — so a code of bare `a` steps would "match" during
	// teardown and log a spurious exit signal. Draining the reader first
	// makes the single-writer premise eventtap_snapshot documents hold for
	// the wipe as well, at a cost of at most one pollInterval (10ms).
	//
	// Ordering within Step 3 is deliberate: both taps are already disabled
	// above, so input has ALREADY recovered — this wait delays only the
	// wipe and the CF teardown, never the user's keyboard.
	//
	// The channels may be nil in unit-test constructors that exercise only
	// the disable/uninstall path; the production Install path always
	// populates them.
	if r.stopPoller != nil {
		// Close-only signalling: idempotency is irrelevant because
		// Release itself is already serialised by the mutex + released
		// guard, so this close runs exactly once per Releaser instance.
		close(r.stopPoller)
	}
	if r.pollerDone != nil {
		// Wait for the goroutine to actually exit. Under `-race`, a
		// still-running goroutine reading the C ring after Release
		// returns would be flagged. The poller's loop returns within
		// pollInterval (10ms) of stopPoller close, so this wait is
		// bounded. It cannot deadlock: the poller never takes this
		// mutex, and its only send (to `sink`) is non-blocking.
		<-r.pollerDone
	}

	// Closing Step 3: the poller join above removed the ring's READER, so
	// wipe it. It did NOT remove the writer — disabling a tap is not a
	// drain, and a mach message already queued on the tap port can still
	// produce a callback afterwards. That is why the wipe does not memset
	// from this goroutine: wipeRingFn calls eventtap_wipe_ring_on_worker,
	// which runs the memset as a run-loop block ON the worker thread, where
	// it cannot overlap the callback's plain stores (see the contract in
	// tap_darwin.m).
	//
	// Deliberately here and not later — the whole point is to shorten the
	// window in which the just-typed unlock code is readable in process
	// memory, and the CF teardown below can take arbitrarily long (or fail
	// outright). eventtap_uninstall_c wipes again at Step 5, but only if its
	// own drain succeeded (`if (drained == 0)` there): if BOTH this
	// handshake and that drain time out, the ring stays resident and Step 5
	// logs the "keystroke ring left resident" WARN. Two independent chances,
	// not a guarantee.
	if r.wipeRingFn != nil {
		r.wipeRingFn()
	}

	// --- Step 4: gesture tap teardown MUST precede the main uninstall.
	// uninstallFn ends with CFRunLoopStop on the SHARED worker run loop;
	// once the worker goroutine unwinds, the loop ref the gesture source
	// is attached to dangles (see gesturetap_uninstall_c ordering
	// contract). ---
	if r.gestureUninstallFn != nil {
		r.gestureUninstallFn()
	}

	// --- Step 5: CFRunLoopRemoveSource → drain → CFRelease(source+tap)
	// + CFRunLoopStop, bundled in eventtap_uninstall_c. Safe to free the
	// mach ports here precisely because Step 2 established that no watchdog
	// handler and no wake block can still be holding them. ---
	if r.uninstallFn != nil {
		r.uninstallFn()
	}

	// (The unlock-code poller was already stopped and drained at Step 3,
	// before the ring wipe — see the comment there for why it cannot be
	// left running across a memset of the ring it reads.)

	// Drop references so a hypothetical re-call (which short-circuits via
	// `released.Load()` anyway) has nothing to invoke. The C-side state
	// is already torn down by uninstallFn.
	r.disableFn = nil
	r.clearObservedFn = nil
	r.uninstallFn = nil
	r.gestureDisableFn = nil
	r.gestureUninstallFn = nil
	r.wipeRingFn = nil

	// Store AFTER teardown completes. Concurrent callers blocked on
	// mu.Lock will see released=true under mu and short-circuit; new
	// callers using the fast-path Load see the same after the Unlock
	// has happens-before published the Store.
	r.released.Store(true)
	return nil
}

// installTapOnly installs a CGEventTap at kCGHIDEventTap level and starts
// the poller that watches for the given unlock code. The C callback records
// every non-autorepeat key press into a static ring; the poller goroutine
// snapshots that ring on a 10ms ticker, matches every tail of it against the
// code in pure Go (matcher.Sequence), and on a match forwards a struct{} send
// to sink (capacity 1, non-blocking select-default).
//
// `steps` is the whole unlock code, in order. A legacy single combination is
// just a code of length 1, so there is one matching path in the codebase
// rather than two; the C side never learns the code at all.
//
// An empty `steps` is rejected with ErrEmptyUnlockCode BEFORE the tap is
// created: a zero-length matcher.Sequence would match the empty tail of every
// snapshot, i.e. unlock on the first tick without a single keypress.
// config.ValidateUnlockCode already rejects it upstream, so this is a
// defence-in-depth gate on the package boundary, not the primary check.
//
// fix: previously exposed as exported `Install`, but the returned
// `*Releaser` had nil `watchdogStop` + nil `wakeStop` — Release() silently
// short-circuited past both, leaving the production caller with NO
// silent-disable recovery and NO post-wake re-arm. doc.go advertised
// `Install` as THE entry point; a future maintainer reading the docs
// could reasonably write `eventtap.Install(...)` from main.go, see the
// binary compile + run, and lose protection on the first user MacBook
// that goes to sleep or hits a TCC race. The package boundary is unsafe.
// Renamed to unexported `installTapOnly` to make the smoke-test-only
// surface clear — production callers MUST go through `InstallAll`. The
// smoke test that exercises this path is now an internal_test (same
// package), so it retains access to this helper without re-exporting it.
//
// Logger fallback: nil → slog.Default() (mirrors powerassert.Acquire +
// state.NewRestoreState + cocoa.NewController convention).
//
// Pre-masking: every step's Modifiers is AND'ed with
// matcher.UserIntentionalMask before the matcher.Sequence is built, so the
// configured side of every comparison is already masked. The C callback masks each incoming
// event's flags with the twin USER_INTENTIONAL_MASK before recording it, and
// MatchTail masks the recorded value again for good measure — no system bits
// (CapsLock 0x10000, NumPad 0x200000, Help 0x400000, NX_NONCOALSESCEDMASK
// 0x100) affect the result (the design notes). The masking on the configured
// side matters because MatchTail compares for EXACT equality: an unmasked
// stray bit in the Spec would make the code unenterable.
//
// Worker thread pattern (the design notes): a dedicated goroutine is spawned
// inside installTapOnly. It calls runtime.LockOSThread() (no Unlock — the
// Go runtime reaps the OS thread when the goroutine exits via
// CFRunLoopStop), captures its run loop via
// eventtap_register_worker_runloop, adds the tap source to it, then
// blocks on CFRunLoopRun() until Release calls CFRunLoopStop on the
// captured loop pointer.
//
// MUST be called from the main goroutine. The main goroutine is locked to
// OS thread #0 by internal/runtimepin/init(); installTapOnly itself does
// NOT touch AppKit but the watchdog and wake observer
// — which are installed by wire-up AFTER this call
// do, so the convention is preserved end-to-end.
//
// Error path: a non-zero return code from eventtap_install_c is wrapped via
// fmt.Errorf("%w: rc=%d ...", ErrTapInstallFailed, rc) so callers can use
// `errors.Is(err, ErrTapInstallFailed)` to identify the category. The three
// known triggers documented in errors.go (Accessibility revoked,
// SecureEventInput active, kernel out of mach ports) are all surfaced as
// the same sentinel; the rc field distinguishes between
// CGEventTapCreate-returned-NULL (rc=1) and
// CFMachPortCreateRunLoopSource-returned-NULL (rc=2).
//
// wire-up in cmd/dndmode/main.go uses InstallAll (the composite)
// rather than this raw helper — the composite wires watchdog + wake
// observer in addition to the bare tap. installTapOnly remains for
// smoke tests (`-tags manual`) that exercise the tap subsystem in
// isolation without the GCD timer / NSWorkspace observer overhead.
//
// The watchdog and wake are separate Releasers that own
// their own GCD timer / notification token respectively; they are NOT
// bundled here so the three plans can land in parallel and so the smoke
// test stays minimal. Production callers MUST use InstallAll.
func installTapOnly(steps []hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
	r, _, err := installInternal(steps, sink, log)
	return r, err
}

// installInternal is the package-private install-and-return-tap helper
// shared by `Install` (public, drops tap) and `InstallAll` (composite,
// needs tap for StartWatchdog + InstallWakeObserver). Returning
// `C.CFMachPortRef` rather than `unsafe.Pointer` keeps `go vet -unsafeptr`
// quiet — both consumers of the tap (helpers and the
// `eventtap_set_observed_tap` setter) accept conversions at the call
// site, and the tap value is never stored on a struct field that would
// trip the heuristic.
//
// Logger fallback, latch reset, and mask pre-computation are identical
// to the original `Install` body — extracted verbatim during.
func installInternal(steps []hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, C.CFMachPortRef, error) {
	if log == nil {
		log = slog.Default()
	}

	// Reject the empty code before touching CoreGraphics — see the
	// ErrEmptyUnlockCode paragraph on installTapOnly for why a zero-step
	// Sequence is an immediate unlock rather than a never-matching one.
	if len(steps) == 0 {
		var zero C.CFMachPortRef
		return nil, zero, ErrEmptyUnlockCode
	}

	// Refuse to reuse C-side state a previous teardown could not quiesce.
	// Checked here — before `eventtap_install_c`, whose FIRST act is the
	// `eventtap_wipe_ring()` memset — because that memset is the write that
	// would race a still-live callback's plain ring stores. See the
	// `teardownUnclean` doc comment for why the latch is one-way and why
	// this costs nothing in production.
	if teardownUnclean.Load() {
		var zero C.CFMachPortRef
		return nil, zero, ErrTeardownUnclean
	}

	// Pre-mask every step's modifiers with the user-intentional mask so the
	// matcher compares pre-masked against pre-masked.
	// matcher.UserIntentionalMask is the single source of truth for which
	// modifier bits represent user intent (Cmd | Option | Ctrl | Shift — and
	// deliberately NOT Fn, which macOS raises on the whole function-key group
	// by itself; see the mask's doc comment in matcher/matcher.go before
	// "restoring" it here).
	//
	// The copy is deliberate: the caller's slice is not retained, so a
	// caller that reuses or mutates its backing array after Install cannot
	// change the code the poller is matching against.
	masked := make([]hotkey.Spec, len(steps))
	for i, st := range steps {
		masked[i] = hotkey.Spec{
			Modifiers: st.Modifiers & matcher.UserIntentionalMask,
			KeyCode:   st.KeyCode,
		}
	}

	// The unlock verifier. Built once, here, from the masked copy —
	// matcher.Sequence is immutable after construction and Match is pure, so
	// the poller goroutine can use it without any synchronisation. A ring
	// reset happens inside eventtap_install_c below, so the poller starts
	// against an empty ring no matter what a previous Install left behind.
	//
	// pollSequence takes a matcher.Verifier, not a *matcher.Sequence: the
	// hashed secret written by `--set-password` arrives as a *matcher.Digest
	// through the same parameter. Constructing the Sequence HERE, from the
	// []hotkey.Spec this function still takes, is deliberately temporary —
	// it keeps InstallAll's signature (and therefore main.go and both test
	// files) untouched while the poller moves onto the interface. The
	// []hotkey.Spec parameter is replaced by a Verifier one task later,
	// together with the ErrEmptyUnlockCode guard above, which then has to be
	// expressed as `v == nil || v.MaxLen() == 0` because `len(steps)` will
	// no longer be in scope.
	var seqMatcher matcher.Verifier = matcher.NewSequence(masked)

	var cTap C.CFMachPortRef
	rc := C.eventtap_install_c(&cTap)
	if rc != 0 {
		var zero C.CFMachPortRef
		return nil, zero, fmt.Errorf("%w: rc=%d (likely Accessibility revoked, SecureEventInput active, or kernel out of mach ports)",
			ErrTapInstallFailed, int(rc))
	}

	// Poller baseline, sampled HERE and nowhere else.
	//
	// `eventtap_install_c` above zeroed the ring and its counter, and the
	// tap source it created has NOT been added to any run loop yet — the
	// worker goroutine below does that. So this is the last instant in the
	// whole install path at which the counter is provably quiescent, and
	// the value read is provably 0.
	//
	// It is read here rather than inside `pollSequence` because the poller
	// goroutine starts at the END of this function: between the worker's
	// `CFRunLoopAddSource` and the poller's first statement lie the gesture
	// tap install, the channel and closure setup, the `go` statement, and
	// an unbounded scheduling delay. A `seq()` taken at the far end of that
	// gap would treat every press recorded inside it as belonging to a
	// previous session, and a full unlock code typed there would be
	// swallowed with no diagnostic — the mode is silent on a failed match
	// by design, so the only symptom would be a machine that did not
	// unlock. Sampling before the tap can fire removes the gap instead of
	// narrowing it.
	baseSeq := seq()

	// Worker goroutine: locks an OS thread, captures its CFRunLoop, adds
	// the tap source to it, then blocks on CFRunLoopRun until Release calls
	// CFRunLoopStop on the captured loop. The goroutine exits when the run
	// loop returns; Go runtime reaps the locked OS thread automatically
	// (no UnlockOSThread needed — the runtime detects goroutine exit on a
	// locked thread and tears the thread down).
	//
	// fix: the registration rc is checked instead of discarded. The
	// C side currently always returns 0, but the integer return type is
	// preserved "for symmetry with eventtap_install_c and future-proofing"
	// (tap_darwin.m comment). A future change that makes registration
	// fallible would have silently regressed under the prior `_ = rc`
	// discard: the goroutine would push a zero-valued CFRunLoopRef (NULL)
	// onto runLoopCh and proceed to CFRunLoopRun(), which returns
	// immediately when called with no sources. The Go side would have a
	// NULL workerLoop and the teardown chain would try CFRunLoopStop on
	// NULL. We now push NULL explicitly on rc != 0 AND check on the
	// receiver side, so a future rc != 0 surfaces as ErrTapInstallFailed
	// with a distinct rc tag instead of mysterious teardown UB.
	type workerHandshake struct {
		loop C.CFRunLoopRef
		rc   C.int
	}
	// rc sentinel: distinct from any value the C side returns so the
	// install caller can format a panic-specific error message. The C
	// `eventtap_register_worker_runloop` returns 0=success or small
	// positive rc (1/2/...) per tap_darwin.m; a large negative sentinel
	// is reserved for "goroutine panicked before handshake".
	const workerPanicSentinel C.int = -1
	runLoopCh := make(chan workerHandshake, 1)
	go func() {
		// fix: panic-safe defer. The original review proposed
		// "wrap the install goroutine with a recover that still pushes a
		// value through the handshake so a panic in runtime.LockOSThread
		// / cgo bridge / register_worker_runloop does not deadlock the
		// install path." An earlier version implemented only the rc-check half of
		// this defer closes the second half.
		//
		// Without this defer: a panic in any of the body's calls
		// (runtime.LockOSThread is a syscall; cgo bridge can panic on
		// stack overflow; defensive against future additions of fallible
		// Go-side calls here) would propagate via Go's default
		// goroutine-panic mechanism — the goroutine exits WITHOUT sending
		// on runLoopCh, and the main goroutine blocks forever on
		// `hs := <-runLoopCh`. main.go hangs at Step 17 with no
		// diagnostic.
		//
		// With this defer: panic → recover → push sentinel handshake
		// (rc=workerPanicSentinel) → main side sees rc != 0 → returns
		// wrapped ErrTapInstallFailed. The supervisor unwinds normally.
		// The panic value itself is logged via slog at Error level so
		// the original stack is not lost — matches the top-level
		// recover pattern in cmd/dndmode/main.go.
		defer func() {
			if r := recover(); r != nil {
				log.Error("eventtap worker-install goroutine panicked",
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())))
				// Buffered channel cap=1 + this defer is the ONLY late
				// sender after recover, so the send cannot block.
				// (A normal-path send earlier on the happy line already
				// consumed the buffer slot? No — happy path returns
				// BEFORE this defer fires only because there is no
				// panic, so this send and the happy send are mutually
				// exclusive. The recover-then-send is racefree.)
				runLoopCh <- workerHandshake{rc: workerPanicSentinel}
			}
		}()
		runtime.LockOSThread()
		// Intentionally no defer UnlockOSThread — see comment above.
		var loop C.CFRunLoopRef
		rc := C.eventtap_register_worker_runloop(cTap, &loop)
		if rc != 0 {
			// Push a sentinel-only handshake; do NOT call CFRunLoopRun
			// because no source was added. Goroutine exits and the Go
			// runtime reaps its locked OS thread.
			var zero C.CFRunLoopRef
			runLoopCh <- workerHandshake{loop: zero, rc: rc}
			return
		}
		runLoopCh <- workerHandshake{loop: loop, rc: 0}
		// Blocks until Release → eventtap_uninstall_c → CFRunLoopStop.
		C.CFRunLoopRun()
	}()
	hs := <-runLoopCh
	if hs.rc != 0 {
		// Worker run-loop registration failed. Tear down the
		// already-created tap so the kernel-side mach port doesn't leak.
		//
		// The teardown rc is checked, not discarded: a non-zero means the
		// drain handshake timed out and eventtap_uninstall_c deliberately
		// left the source + tap RETAINED instead of releasing them (see its
		// return-value docblock). That is the exact opposite of the "doesn't
		// leak" this call was made for, so it must not pass silently. DEBUG
		// matches uninstallFn below and the reachability here: on this path
		// the worker goroutine died before CFRunLoopRun, so it services no
		// blocks and the timeout is the EXPECTED outcome rather than a fault.
		if rc := C.eventtap_uninstall_c(cTap); rc != 0 {
			log.Debug("eventtap: teardown drain timed out during worker-handshake rollback; tap left retained rather than released")
		}
		var zero C.CFMachPortRef
		if hs.rc == workerPanicSentinel {
			// distinguish goroutine panic from C-side rc!= 0.
			// The panic stack itself was already logged inside the
			// goroutine's recover defer above; here we surface the
			// category in the error returned to main.go so the
			// top-level recover doesn't double-log.
			return nil, zero, fmt.Errorf("%w: worker goroutine panicked before run-loop handshake", ErrTapInstallFailed)
		}
		return nil, zero, fmt.Errorf("%w: worker run-loop registration rc=%d", ErrTapInstallFailed, int(hs.rc))
	}
	// Second tap: session-level trackpad-gesture suppressor
	// (gesturetap_darwin.m). hs.loop — the worker run loop the handshake
	// just delivered — hosts the gesture tap source too, so both taps are
	// serviced by the same locked OS thread and no extra goroutine is
	// needed. The HID tap above cannot see dock gestures at all:
	// WindowServer synthesizes them from the raw multitouch stream and
	// hands them to the Dock at session level (CGS types 29/30); without
	// this tap a 3/4-finger swipe opens Mission Control over the shield.
	//
	// Failure is FATAL like the main tap (same triggers: Accessibility
	// revoked, SecureEventInput, mach ports): silently degrading would
	// leave the gesture hole open with no observable signal, violating the
	// "all input blocked" contract. Rollback mirrors the handshake-failure
	// path above — eventtap_uninstall_c tears down the main tap and stops
	// the worker loop; the failed gesture install cleaned its own state.
	if rc := C.gesturetap_install_c(hs.loop); rc != 0 {
		// Unlike the handshake-failure path above, the worker here is ALIVE
		// and inside CFRunLoopRun, so a drain timeout is not the expected
		// outcome — it means a live callback could not be proven idle and
		// the main tap's source + mach port were left retained. WARN rather
		// than DEBUG for that reason, and the rc is folded into the returned
		// error so the diagnostic survives even at a log level that drops
		// the line.
		rollbackNote := ""
		if urc := C.eventtap_uninstall_c(cTap); urc != 0 {
			log.Warn("eventtap: teardown drain timed out during gesture-install rollback; main tap left retained rather than released")
			rollbackNote = "; rollback drain timed out, main tap left retained"
			// Worker loop alive + unprovable callback ⇒ the C-side statics
			// must not be reused. Unlike the handshake-rollback site above
			// (worker dead, nothing dispatches), a timeout here really can
			// mean a live writer. See the `teardownUnclean` doc comment.
			teardownUnclean.Store(true)
		}
		var zero C.CFMachPortRef
		return nil, zero, fmt.Errorf("%w: gesture tap rc=%d (session-level dock-gesture suppression)%s", ErrTapInstallFailed, int(rc), rollbackNote)
	}

	stopPoller := make(chan struct{})
	pollerDone := make(chan struct{})

	// Capture cTap by value into the closures so the disable/uninstall
	// functions stay bound to the install-time mach port — Release nilling
	// closures does not affect what these closures hold.
	disableFn := func() {
		C.eventtap_enable(cTap, C.int(0))
	}
	// clearObservedFn is the Step 1 atomic-null-guard
	// write. Captures NOTHING — the C-side global lives in
	// watchdog_darwin.m and is keyed by file scope, not by tap value. A
	// zero-valued C.CFMachPortRef (an opaque-pointer typedef whose zero
	// value is the NULL pointer) is the canonical "no current tap" signal
	// that the watchdog handler / wake-observer blocks snapshot-check at
	// the top of their bodies. Go's cgo type system rejects `C.CFMachPortRef(nil)`
	// (mismatched-types error), so we use a zero-value variable.
	clearObservedFn := func() {
		var zero C.CFMachPortRef
		C.eventtap_set_observed_tap(zero)
	}
	uninstallFn := func() {
		// rc == 1 means the post-detach drain handshake timed out, so the C
		// side deliberately left the source + tap retained rather than
		// CFRelease them under a callback it could not prove idle, and
		// SKIPPED its tail ring wipe for the same reason (see
		// eventtap_uninstall_c). Two consequences, both worth a line: a
		// leaked mach port, and the recorded keystrokes — which end with the
		// unlock code the user just typed — left resident in process memory
		// for the remainder of the process lifetime.
		//
		// WARN, not DEBUG. This closure is only ever constructed AFTER the
		// worker handshake succeeded, i.e. the worker goroutine is alive and
		// inside CFRunLoopRun, so a loop that services blocks in
		// microseconds failing to service one in 100ms is a genuine fault,
		// not the expected outcome. (The install-rollback call site, where
		// the worker died before CFRunLoopRun and a timeout IS expected, is
		// a different call site with its own DEBUG line.) Release still
		// returns nil: there is nothing the caller can do about it, and the
		// process is on its way out either way.
		if rc := C.eventtap_uninstall_c(cTap); rc != 0 {
			log.Warn("eventtap: teardown drain timed out; tap left retained rather than released and the keystroke ring left resident")
			// Third consequence, and the one the C side cannot express on
			// its own: a callback that could not be proven idle may still be
			// appending to the ring, so no later install may memset it or
			// publish a new g_tap. See the `teardownUnclean` doc comment.
			teardownUnclean.Store(true)
		}
	}
	// Gesture-tap closures capture NOTHING — the C side owns the canonical
	// state via the gesturetap_darwin.m statics (same file-scope keying as
	// clearObservedFn). Ordering relative to the main-tap closures is
	// enforced by Release, not here.
	gestureDisableFn := func() {
		C.gesturetap_disable_c()
	}
	gestureUninstallFn := func() {
		// Same contract as uninstallFn above, including the level: this
		// closure too exists only on a path where the worker loop is alive
		// and servicing blocks, so a timeout is a fault rather than the
		// expected outcome. The gesture tap records nothing, so the only
		// consequence here is the leaked source + mach port.
		if rc := C.gesturetap_uninstall_c(); rc != 0 {
			log.Warn("eventtap: gesture-tap teardown drain timed out; tap left retained rather than released")
			// The gesture tap records nothing, but its drain runs the SAME
			// handshake against the SAME worker loop the main tap's callback
			// runs on — and it runs FIRST in Release. A timeout here is
			// therefore evidence that loop is not servicing blocks, which is
			// exactly the state in which the main callback cannot be proven
			// idle either. Latch on it rather than waiting for the main
			// tap's own timeout a step later.
			teardownUnclean.Store(true)
		}
	}
	// wipeRingFn captures nothing — the ring is a file-scope static in
	// tap_darwin.m, keyed by file scope exactly like the gesture-tap
	// closures above. Release invokes it at the end of Step 3, AFTER the
	// poller is closed and joined — that ordering is the safety property,
	// not a detail: a memset landing between eventtap_snapshot's ACQUIRE
	// load and its memcpy hands a live poller a run of {flags: 0,
	// keycode: 0} records, and keycode 0 is kVK_ANSI_A, i.e. a spurious
	// match for a bare-`a` code during teardown. Do not move it earlier.
	//
	// The wipe runs ON the worker thread, not here. Release has already
	// stopped the ring's READER (the poller) by the time this is called, but
	// the WRITER is a tap callback on the worker thread and the tap source is
	// still attached to the worker loop at Step 3 — CGEventTapEnable(tap,
	// false) carries no drain guarantee, and a mach message already queued on
	// the tap port can produce a callback after any handshake this side could
	// wait on. A memset issued from this goroutine would therefore be racing
	// a pair of plain stores: a real C data race, which no later clean wipe
	// can retroactively undo.
	//
	// eventtap_wipe_ring_on_worker sidesteps it instead of narrowing the
	// window: the callback is a run-loop callout and the wipe is a block on
	// that same run loop, so the THREAD serialises them and they cannot
	// overlap at all. A callback dispatched after the block can still
	// repopulate the ring, which is not a race and not read by anyone, and
	// the airtight wipe inside eventtap_uninstall_c (source detached, drain
	// confirmed) mops it up moments later.
	//
	// rc == 1 means the run-loop handshake timed out and the ring was NOT
	// wiped here. Deliberately no fallback memset: falling back would
	// re-introduce precisely the data race this call exists to avoid. The
	// wipe at the tail of eventtap_uninstall_c is the second chance, but it
	// is NOT unconditional — it too is skipped when that call's own drain
	// times out, for the same data-race reason. A timeout in both places
	// leaves the secret resident, which is why the uninstall path logs its
	// timeout at WARN.
	//
	// Cost on the normal path is a run-loop wake round-trip — microseconds,
	// and it delays only the wipe: both taps were disabled several statements
	// earlier, so the user's keyboard is already back.
	wipeRingFn := func() {
		if rc := C.eventtap_wipe_ring_on_worker(); rc != 0 {
			log.Debug("eventtap: early ring wipe deferred to uninstall (worker handshake timed out)")
		}
	}

	r := &Releaser{
		log:                log,
		stopPoller:         stopPoller,
		pollerDone:         pollerDone,
		disableFn:          disableFn,
		clearObservedFn:    clearObservedFn,
		uninstallFn:        uninstallFn,
		gestureDisableFn:   gestureDisableFn,
		gestureUninstallFn: gestureUninstallFn,
		wipeRingFn:         wipeRingFn,
	}

	// Poller goroutine: probes the keystroke counter on a 10ms ticker,
	// snapshots the ring only when it has moved, matches in pure Go, and on
	// a match forwards a struct{} send to `sink` (non-blocking) and
	// returns. It also exits cleanly when stopPoller is closed.
	//
	// `seq` and the snapshot closure are passed as function values rather
	// than called directly inside pollSequence: that keeps the poller itself
	// free of cgo, so poller_test.go can drive the whole matching path
	// against a fake ring without standing up a CGEventTap.
	//
	// newSnapshotFn is called HERE, once, rather than per tick: it owns a
	// staging buffer that must not be shared with any other goroutine, and
	// this poller is its only user.
	snapshotFn := newSnapshotFn()
	go func() {
		defer close(pollerDone)
		pollSequence(stopPoller, baseSeq, seq, snapshotFn, seqMatcher, sink, log)
	}()

	return r, cTap, nil
}

// InstallAll is the production composite that wires the three
// subsystems — CGEventTap, watchdog dispatch_source_t, and
// NSWorkspace wake observer — into a single Releaser whose
// `Release` follows the verbatim order:
//
//	Step 1: eventtap_enable(tap, 0) + eventtap_set_observed_tap(NULL)
//	        + gesturetap_disable_c
//	Step 2: watchdog_stop          (cancel + synchronous cancel-handler drain)
//	        wake_observer_remove   (NSWorkspace observers + g_observed_tap=NULL)
//	Step 3: unlock-code poller close/drain + eventtap_wipe_ring_on_worker
//	Step 4: gesturetap_uninstall_c (gesture source off the shared worker loop)
//	Step 5: CFRunLoopRemoveSource → drain → CFRelease(source+tap)
//	        + CFRunLoopStop        (bundled in eventtap_uninstall_c)
//
// The healers (Step 2) come down BEFORE the CFReleases (Steps 4-5), not
// after: both call CGEventTapEnable on the ports those steps free, and the
// Step 1 NULL write cannot stop a handler that already loaded the pointer.
//
// calls this from cmd/dndmode/main.go Step 16 (after controller,
// before sup.Wait). The single returned Releaser is pushed onto
// RestoreState; LIFO order ensures it is released FIRST among the
// Phase 4 push stack — appropriate, because tap teardown is the only one
// that restores user-facing input.
//
// Error path is roll-back-on-failure (threat — partial
// initialisation must not leak):
//
//   - empty `steps` → return (nil, ErrEmptyUnlockCode) before any resource is
//     touched (see the sentinel's docstring: a zero-step code matches the
//     empty tail and would unlock on the first keypress).
//   - `Install` failure → return (nil, wrapped err). Nothing acquired.
//   - `StartWatchdog` failure → call r.Release() to tear down the tap +
//     poller, then return wrapped err.
//   - `InstallWakeObserver` failure → call wdStop() to tear down the
// watchdog first (LIFO: watchdog before wake), then r.Release()
//     to tear down the tap, then return wrapped err.
//
// The wake-observer error path explicitly tears down watchdog FIRST and
// tap LAST because that mirrors the success-path release order for
// the resources actually acquired at the point of failure — keeping a
// single mental model regardless of whether teardown is triggered by
// normal Cleanup or by InstallAll's own rollback.
//
// MUST be called from the main goroutine: `Install`, `StartWatchdog`, and
// `InstallWakeObserver` all carry main-thread requirements (cocoa
// pinning, NSWorkspace notificationCenter). main.go's `internal/runtimepin`
// init() pins the main goroutine to OS thread #0 — this invariant is
// preserved end-to-end.
//
// Logger fallback: nil → slog.Default() (mirrors all other Install-shaped
// constructors in this codebase).
func InstallAll(steps []hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
	if log == nil {
		log = slog.Default()
	}

	// Step A — install the tap itself via the package-private helper
	// that returns the cTap by value (so we can pass it to
	// helpers without storing it on a Releaser field — see the
	// Design note on the Releaser struct for the go-vet
	// / GC-safety rationale).
	r, cTap, err := installInternal(steps, sink, log)
	if err != nil {
		// Nothing acquired; propagate the wrapped error so callers can
		// `errors.Is(err, ErrTapInstallFailed)` for exit-code dispatch.
		return nil, err
	}

	// `cf_to_void_ptr` is the C-side conversion helper (defined in this
	// file's cgo preamble). A direct `unsafe.Pointer(cTap)` cast trips
	// `go vet -unsafeptr` even though the use is idiomatic for cgo opaque
	// handles; routing through C keeps vet quiet without weakening type
	// safety (see the helper's docstring for full rationale).
	tapPtr := C.cf_to_void_ptr(cTap)

	// Step B — start the watchdog (dispatch_source_t timer Go-side
	// threshold poller). On failure: roll back the tap to keep the
	// keyboard responsive.
	wdStop, err := StartWatchdog(tapPtr, sink, log)
	if err != nil {
		// Best-effort rollback. Release error is propagated only if the
		// watchdog error doesn't already explain the failure; we keep the
		// watchdog error as the primary because that's the root cause the
		// caller diagnoses against.
		if relErr := r.Release(); relErr != nil {
			log.Warn("eventtap install rollback: tap release after watchdog failure",
				slog.Any("watchdog_err", err), slog.Any("release_err", relErr))
		}
		return nil, fmt.Errorf("eventtap: start watchdog: %w", err)
	}

	// Step C — install the NSWorkspace wake observer. On failure:
	// stop the watchdog FIRST (LIFO: watchdog before wake, even on
	// rollback), then release the tap.
	wkStop, err := InstallWakeObserver(tapPtr, log)
	if err != nil {
		wdStop()
		//: belt-and-suspenders explicit NULL write.
		// `r.Release()` below calls `clearObservedFn` which performs
		// the SAME `eventtap_set_observed_tap(zero)` write — both are
		// idempotent volatile-pointer stores of NULL, so the second is
		// a no-op. Kept here for self-contained rollback semantics
		// readable without cross-referencing Release. Cheap (single
		// store) and documents the invariant "no observer references
		// this tap by the time we return wrapped error" inline at the
		// rollback site itself.
		var zero C.CFMachPortRef
		C.eventtap_set_observed_tap(zero)
		if relErr := r.Release(); relErr != nil {
			log.Warn("eventtap install rollback: tap release after wake-observer failure",
				slog.Any("wake_err", err), slog.Any("release_err", relErr))
		}
		return nil, fmt.Errorf("eventtap: install wake observer: %w", err)
	}

	// Step D — seed g_observed_tap with the real tap. `watchdog_start`
	// already wrote it inside StartWatchdog above (defensive belt — for
	// the smoke-test path that exercises the watchdog in
	// isolation), so this is an idempotent re-write of the same value.
	// Explicit here makes InstallAll the single source of truth for the
	// "tap is currently observed by both watchdog + wake" invariant.
	C.eventtap_set_observed_tap(cTap)

	// Wire the stop closures onto the Releaser so the unified Release
	// path runs them at Steps 4 and 5.
	r.watchdogStop = wdStop
	r.wakeStop = wkStop

	return r, nil
}

// newReleaserWithDeps is the test-internal constructor that lets unit tests
// inject fake disable/uninstall closures (counting calls + recording order)
// without invoking the real cgo bridge. NOT exported.
//
// Production callers MUST use Install instead. The seam exists for the same
// reason powerassert/assertion.go:99 exposes newAssertionWithDeps: the
// two-layer idempotency contract + the disable-first ordering invariant
// must be testable without a live CGEventTap so the suite stays fast
// and works without Accessibility grants.
//
// stopPoller/pollerDone may be nil if the test only exercises the
// disable/uninstall ordering and skips the poller path; Release nil-checks
// both before close/wait.
func newReleaserWithDeps(disableFn, uninstallFn func(), stopPoller, pollerDone chan struct{}, log *slog.Logger) *Releaser {
	if log == nil {
		log = slog.Default()
	}
	return &Releaser{
		log:         log,
		stopPoller:  stopPoller,
		pollerDone:  pollerDone,
		disableFn:   disableFn,
		uninstallFn: uninstallFn,
	}
}

// Compile-time check: *Releaser satisfies the state.Releaser interface shape
// without importing the state package (would create an import cycle —
// cmd/dndmode/main.go is the only caller that holds *Releaser as
// state.Releaser). Mismatch surfaces here at build time. Mirrors
// powerassert/assertion.go:195-198 verbatim.
var _ interface {
	Release() error
	Name() string
} = (*Releaser)(nil)
