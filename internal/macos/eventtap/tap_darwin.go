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
extern int  eventtap_test_set_expected(uint64_t flags, uint16_t keycode);
*/
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
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

	// stopPoller signals the poller goroutine to exit cleanly. Closed by
	// Release; the goroutine selects on it and returns. Cap is 0 (a
	// signalling channel — close-only semantics, no payload).
	stopPoller chan struct{}

	// pollerDone is closed by the poller goroutine when it exits. Release
	// waits on it after closing stopPoller so the cleanup chain returns
	// only after the goroutine has actually unwound — important under
	// `-race` where a still-running goroutine would surface as a leak.
	pollerDone chan struct{}

	// disableFn / uninstallFn are the DI seams that let unit tests
	// substitute fakes for the C-side bridge calls. Production wires them
	// to the real `eventtap_enable(tap, false)` + `eventtap_uninstall_c(tap)`
	// at Install time (see Install) or via the test-internal
	// `newReleaserWithDeps` constructor (tap_test.go). NOT exported —
	// production callers MUST go through Install.
	disableFn   func()
	uninstallFn func()

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
// Teardown order (D-08 disable-first invariant from RESEARCH §8):
//
//  1. disableFn — CGEventTapEnable(tap, false). The tap is disabled FIRST so
//     the keyboard recovers immediately even if any subsequent step fails.
//     The callback can no longer fire after this returns.
//  2. uninstallFn — CFRunLoopRemoveSource → CFRelease(source) →
//     CGEventTapEnable(tap, false) [defensive] → CFRelease(tap) →
//     CFRunLoopStop(worker_runloop). The C-side helper bundles these for
//     atomicity; Go just calls one function.
//  3. close(stopPoller) — the poller goroutine selects on stopPoller in its
//     ticker loop and returns.
//  4. <-pollerDone — wait for the poller to fully unwind before returning.
//     Under `-race`, a still-running goroutine accessing `matched` after
//     Release would be flagged.
//
// Watchdog (plan 04-03) and wake observer (plan 04-04) Release paths live
// on their own Releaser types pushed separately onto the RestoreState LIFO
// chain — this Releaser owns only tap + poller.
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

	// D-08 disable-first ordering: the tap is disabled BEFORE any other
	// teardown step. If subsequent CFRelease or CFRunLoopStop panics or
	// hangs, the keyboard is already restored because CGEventTapEnable
	// took effect immediately on the kernel side.
	if r.disableFn != nil {
		r.disableFn()
	}
	if r.uninstallFn != nil {
		r.uninstallFn()
	}

	// Stop the poller goroutine. The channel may be nil in unit-test
	// constructors that exercise only the disable/uninstall path; the
	// production Install path always populates it.
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

	// No-op: raw cgo pointers are no longer stored on the Releaser (they
	// live inside disableFn/uninstallFn closures). The closures themselves
	// nil out via the C-side `g_tap = NULL` inside `eventtap_uninstall_c`,
	// so a hypothetical re-call would just no-op at the C layer.
	r.disableFn = nil
	r.uninstallFn = nil

	// Store AFTER teardown completes. Concurrent callers blocked on
	// mu.Lock will see released=true under mu and short-circuit; new
	// callers using the fast-path Load see the same after the Unlock
	// has happens-before published the Store.
	r.released.Store(true)
	return nil
}

// Install installs a CGEventTap at kCGHIDEventTap level configured to match
// the given hotkey.Spec. On a successful (modifiers, keyCode) match the C
// callback flips the package-level atomic.Bool `matched`; the poller
// goroutine reads it on a 10ms ticker and forwards a struct{} send to sink
// (capacity 1, non-blocking select-default — D-02 / D-04).
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
// inside Install. It calls runtime.LockOSThread() (no Unlock — the Go
// runtime reaps the OS thread when the goroutine exits via CFRunLoopStop),
// captures its run loop via eventtap_register_worker_runloop, adds the tap
// source to it, then blocks on CFRunLoopRun() until Release calls
// CFRunLoopStop on the captured loop pointer.
//
// MUST be called from the main goroutine. The main goroutine is locked to
// OS thread #0 by internal/runtimepin/init(); Install itself does NOT touch
// AppKit but the watchdog (plan 04-03) and wake observer (plan 04-04) — which
// are installed by Wave 2 wire-up AFTER this call — do, so the convention
// is preserved end-to-end.
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
// Wave 2 wire-up in cmd/dndmode/main.go will:
//
//	tapReleaser, err := eventtap.Install(matcher.Spec(), supervisor.ExitTrigger(), log)
//	if errors.Is(err, eventtap.ErrTapInstallFailed) { return exitPlatformErr }
//	rs.Push(tapReleaser)
//
// The watchdog and wake are separate Releasers that own
// their own GCD timer / notification token respectively; they are NOT
// bundled here so the three plans can land in parallel.
func Install(spec hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
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
		return nil, fmt.Errorf("%w: rc=%d (likely Accessibility revoked, SecureEventInput active, or kernel out of mach ports)",
			ErrTapInstallFailed, int(rc))
	}

	// Worker goroutine: locks an OS thread, captures its CFRunLoop, adds
	// the tap source to it, then blocks on CFRunLoopRun until Release calls
	// CFRunLoopStop on the captured loop. The goroutine exits when the run
	// loop returns; Go runtime reaps the locked OS thread automatically
	// (no UnlockOSThread needed — the runtime detects goroutine exit on a
	// locked thread and tears the thread down).
	runLoopCh := make(chan C.CFRunLoopRef, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally no defer UnlockOSThread — see comment above.
		var loop C.CFRunLoopRef
		_ = C.eventtap_register_worker_runloop(cTap, &loop)
		runLoopCh <- loop
		// Blocks until Release → eventtap_uninstall_c → CFRunLoopStop.
		C.CFRunLoopRun()
	}()
	workerLoop := <-runLoopCh

	stopPoller := make(chan struct{})
	pollerDone := make(chan struct{})

	// Capture cTap by value into the closures so the disable/uninstall
	// functions stay bound to the install-time mach port — Release nilling
	// `r.tap` does not affect what these closures hold.
	disableFn := func() {
		C.eventtap_enable(cTap, C.int(0))
	}
	uninstallFn := func() {
		C.eventtap_uninstall_c(cTap)
	}

	_ = workerLoop // captured for diagnostic logging by future smoke tests; not stored on Releaser to avoid go-vet unsafe.Pointer warning

	r := &Releaser{
		log:         log,
		stopPoller:  stopPoller,
		pollerDone:  pollerDone,
		disableFn:   disableFn,
		uninstallFn: uninstallFn,
	}

	// Poller goroutine: reads `matched` on a 10ms ticker, on success
	// forwards a struct{} send to `sink` (non-blocking). The
	// goroutine exits cleanly when stopPoller is closed.
	go func() {
		defer close(pollerDone)
		pollMatched(stopPoller, &matched, sink, log)
	}()

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
