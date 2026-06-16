//go:build darwin

package eventtap

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

extern int  eventtap_install_c(uint64_t flags, uint16_t keycode, CFMachPortRef *out_tap);
extern int  eventtap_register_worker_runloop(CFMachPortRef tap, CFRunLoopRef *out_loop);
extern void eventtap_uninstall_c(CFMachPortRef tap);
extern int  eventtap_is_enabled(CFMachPortRef tap);
extern void eventtap_enable(CFMachPortRef tap, int enable);

// eventtap_set_observed_tap is the canonical writer for the shared
// `volatile CFMachPortRef g_observed_tap` global that lives in
// watchdog_darwin.m and is read by both the watchdog GCD handler AND the
// NSWorkspace wake-observer blocks (wake_darwin.m). Step 1 of
// the Release order writes NULL via this setter, which closes the
// race window for in-flight handlers between Release Step 1
// and Step 4 (watchdog_stop) / Step 5 (wake_observer_remove).
extern void eventtap_set_observed_tap(CFMachPortRef tap);

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

// matched is the package-level latch flipped by the //export Go helper
// eventtap_matched, which is invoked from the CGEventTap C callback on its
// worker thread when (modifiers, keyCode) match the configured Spec.
//
// The poller goroutine (pollMatched in poller.go; 10ms ticker) reads
// matched.CompareAndSwap(true, false) and on success performs a non-blocking
// `select { case sink <- struct{}{}: default: }` to the supervisor's
// ExitTrigger channel.
//
// atomic.Bool is the ONLY storage primitive permitted in the //export Go
// callback per (nosplit invariant; the design notes forbidden-operations
// list). No mutex, no channel, no slog — a single atomic store is the
// entire body of eventtap_matched.
var matched atomic.Bool

// eventtap_matched is the cgo entry point invoked from the C-side
// `eventtap_callback` in tap_darwin.m when an incoming CGEvent matches the
// configured (modifiers, keyCode). It fires on the CGEventTap worker thread
// — NOT main, NOT a Go-scheduled goroutine — so the body MUST satisfy the
// callback contract:
//
//   - No Go memory allocation (would race against the Go scheduler GC barrier
//     while the worker thread is not Go-scheduled).
//   - No blocking primitive (channel send, mutex lock, condition var).
//   - No syscall, no slog, no panic.
//   - Single atomic store is verified safe in production by
//     pqrs-org/osx-event-observer-examples and lwouis/alt-tab-macos (both
//     ship at high event-rates without races under `-race` builds — see
// the design notes reference patterns).
//
// Per Threat, this body MUST stay EXACTLY
// `matched.Store(true)`. The acceptance gate in tap_test.go verifies this
// (grep + functional test) and refuses any extension. The poller goroutine
// on the Go side does ALL the post-match work (forward to sink, clear the
// latch, log).
//
//export eventtap_matched
func eventtap_matched() {
	matched.Store(true)
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

	// watchdogStop / wakeStop are the tear-down
	// closures returned by `StartWatchdog` and `InstallWakeObserver`
	// respectively. Set by `InstallAll`. The plain `Install` constructor
	// (surface) does NOT set these — they remain nil and the
	// nil-check in Release short-circuits them. Production callers MUST go
	// through `InstallAll`; `Install` is kept for the smoke-test path that
	// exercises tap + poller without the watchdog/wake composites.
	//
	// order: watchdogStop runs at Step 4 (after Step 3
	// CFRelease completes), wakeStop runs at Step 5 (last). Between Step 1
	// and Step 4, the watchdog GCD handler may still be in-flight on the
	// HIGH queue — the g_observed_tap=NULL atomic write at Step 1 turns
	// any such in-flight invocation into a no-op via the snapshot guard in
	// watchdog_darwin.m. Same for wake observer (Step 1 → Step 5 window).
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
// that closes the window: any in-flight watchdog GCD
//     handler or wake-observer notification block running between Step 1
//     and Step 4-5 reads g_observed_tap → sees NULL → returns immediately
//     without touching the (about-to-be-freed at Step 3) mach port.
// Step 1 — both calls happen here under the same mutex.
//  2. uninstallFn — CFRunLoopRemoveSource → CFRelease(source) →
//     CGEventTapEnable(tap, false) [defensive] → CFRelease(tap) →
//     CFRunLoopStop(worker_runloop). The C-side helper
// `eventtap_uninstall_c` bundles Steps 2 + 3 of atomically; Go
//     calls a single function (the C-side comment in tap_darwin.m
//     enumerates the sub-steps).
//  3. (subsumed in Step 2 — see above).
// 4. watchdogStop — stop closure. Cancels the GCD
//     dispatch_source_t timer AND closes the watchdog Go-side poller's
//     stop channel. After this returns, no future watchdog handler can
//     fire; any in-flight handler has already short-circuited via Step 1's
//     NULL write. Skipped if nil (set only by InstallAll, not by plain
// Install — keeps smoke-test surface intact).
// 5. wakeStop — stop closure. Removes both NSWorkspace
//     observers (DidWake + SessionDidBecomeActive) and re-NULLs
//     g_observed_tap defensively. Skipped if nil (same rationale).
//  6. close(stopPoller) — the matched-key poller goroutine exits its
//     ticker loop within pollInterval (10ms).
//  7. <-pollerDone — wait for the matched-key poller to fully unwind
//     before returning. Under `-race`, a still-running goroutine
//     accessing `matched` after Release would be flagged.
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

	// --- Steps 2 + 3: CFRunLoopRemoveSource CFRelease(source+tap)
	// + CFRunLoopStop, bundled in eventtap_uninstall_c. ---
	if r.uninstallFn != nil {
		r.uninstallFn()
	}

	// --- Step 4: stop the watchdog GCD timer + its poller goroutine.
	// In-flight handlers between Step 1 and here are already no-ops via
	// the g_observed_tap snapshot guard in watchdog_darwin.m. ---
	if r.watchdogStop != nil {
		r.watchdogStop()
		r.watchdogStop = nil
	}

	// --- Step 5: remove NSWorkspace wake / session-active observers.
	// In-flight main-queue blocks are already no-ops via the
	// g_observed_tap snapshot guard in wake_darwin.m. ---
	if r.wakeStop != nil {
		r.wakeStop()
		r.wakeStop = nil
	}

	// Stop the matched-key poller goroutine. The channel may be nil in
	// unit-test constructors that exercise only the disable/uninstall
	// path; the production Install path always populates it.
	if r.stopPoller != nil {
		// Close-only signalling: idempotency is irrelevant because
		// Release itself is already serialised by the mutex + released
		// guard, so this close runs exactly once per Releaser instance.
		close(r.stopPoller)
	}
	if r.pollerDone != nil {
		// Wait for the goroutine to actually exit. Under `-race`, a
		// still-running goroutine accessing `matched` after Release
		// returns would be flagged. The poller's loop returns within
		// pollInterval (10ms) of stopPoller close, so this wait is
		// bounded.
		<-r.pollerDone
	}

	// Drop references so a hypothetical re-call (which short-circuits via
	// `released.Load()` anyway) has nothing to invoke. The C-side state
	// is already torn down by uninstallFn.
	r.disableFn = nil
	r.clearObservedFn = nil
	r.uninstallFn = nil

	// Store AFTER teardown completes. Concurrent callers blocked on
	// mu.Lock will see released=true under mu and short-circuit; new
	// callers using the fast-path Load see the same after the Unlock
	// has happens-before published the Store.
	r.released.Store(true)
	return nil
}

// installTapOnly installs a CGEventTap at kCGHIDEventTap level configured
// to match the given hotkey.Spec. On a successful (modifiers, keyCode)
// match the C callback flips the package-level atomic.Bool `matched`; the
// poller goroutine reads it on a 10ms ticker and forwards a struct{} send
// to sink (capacity 1, non-blocking select-default).
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
// Pre-masking: spec.Modifiers is AND'ed with matcher.UserIntentionalMask
// BEFORE being passed to the C side, so the callback's static `expected_flags`
// global holds an already-masked value. The C callback then masks each
// incoming event's flags with USER_INTENTIONAL_MASK and compares the two
// masked values for equality — no system bits (CapsLock 0x10000, NumPad
// 0x200000, Help 0x400000, NX_NONCOALSESCEDMASK 0x100) affect the result
// (the design notes).
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
func installTapOnly(spec hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
	r, _, err := installInternal(spec, sink, log)
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
func installInternal(spec hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, C.CFMachPortRef, error) {
	if log == nil {
		log = slog.Default()
	}

	// Reset the matched latch in case this is a re-Install within the same
	// process (the unit-test path exercises this; production never re-installs
	// the tap, but a defensive reset costs one atomic store).
	matched.Store(false)

	// Pre-mask the configured modifiers with the user-intentional mask so
	// the C callback compares pre-masked vs pre-masked. matcher.UserIntentionalMask
	// is the single source of truth for which modifier bits represent user
	// intent (Cmd | Option | Ctrl | Shift | Fn — see matcher/matcher.go).
	masked := spec.Modifiers & matcher.UserIntentionalMask

	var cTap C.CFMachPortRef
	rc := C.eventtap_install_c(C.uint64_t(masked), C.uint16_t(spec.KeyCode), &cTap)
	if rc != 0 {
		var zero C.CFMachPortRef
		return nil, zero, fmt.Errorf("%w: rc=%d (likely Accessibility revoked, SecureEventInput active, or kernel out of mach ports)",
			ErrTapInstallFailed, int(rc))
	}

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
		C.eventtap_uninstall_c(cTap)
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
	//: hs.loop is captured by the C-side static
	// `g_worker_runloop`. The channel handshake itself is load-bearing
	// (synchronizes with the worker goroutine's runtime.LockOSThread()
	// completion) but the Go-side loop pointer is never re-used — the
	// C-side static is the canonical store. Underscore-binding makes
	// "we read the value to synchronize, then discard it" explicit
	// instead of pretending it's "captured for future diagnostic logging."
	_ = hs.loop

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
		C.eventtap_uninstall_c(cTap)
	}

	r := &Releaser{
		log:             log,
		stopPoller:      stopPoller,
		pollerDone:      pollerDone,
		disableFn:       disableFn,
		clearObservedFn: clearObservedFn,
		uninstallFn:     uninstallFn,
	}

	// Poller goroutine: reads `matched` on a 10ms ticker, on success
	// forwards a struct{} send to `sink` (non-blocking). The
	// goroutine exits cleanly when stopPoller is closed.
	go func() {
		defer close(pollerDone)
		pollMatched(stopPoller, &matched, sink, log)
	}()

	return r, cTap, nil
}

// InstallAll is the production composite that wires the three
// subsystems — CGEventTap, watchdog dispatch_source_t, and
// NSWorkspace wake observer — into a single Releaser whose
// `Release` follows the verbatim order:
//
//	Step 1: eventtap_enable(tap, 0) + eventtap_set_observed_tap(NULL)
//	Step 2: CFRunLoopRemoveSource  ┐
//	Step 3: CFRelease(source+tap)  ┘  (both bundled in eventtap_uninstall_c)
//	Step 4: watchdog_stop          (dispatch_source_cancel + Go poller drain)
//	Step 5: wake_observer_remove   (NSWorkspace observers + g_observed_tap=NULL)
//	(plus internal Steps 6/7: matched-key poller close + drain).
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
func InstallAll(spec hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
	if log == nil {
		log = slog.Default()
	}

	// Step A — install the tap itself via the package-private helper
	// that returns the cTap by value (so we can pass it to
	// helpers without storing it on a Releaser field — see the
	// Design note on the Releaser struct for the go-vet
	// / GC-safety rationale).
	r, cTap, err := installInternal(spec, sink, log)
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
