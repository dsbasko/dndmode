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
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
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

// watchdogPollInterval is how often the Go-side `pollWatchdogThreshold`
// goroutine checks the `watchdogThresholdHit` atomic latch. The watchdog
// itself ticks every 5s with a 500ms leeway, so a 100ms poll cadence on
// the Go side adds < 100ms latency to the 25s detection window —
// negligible vs the watchdog's own period. We deliberately use a coarser
// cadence than the matched-key poller (which ticks at 10ms because every
// extra ms of latency translates directly to user-visible unlock latency)
// because the threshold-hit path is the slow-failure case, not the hot
// path; 100ms × 10× = 10× less CPU spent on no-op Loads.
const watchdogPollInterval = 100 * time.Millisecond

// watchdogThresholdHit is the package-level latch flipped by the //export
// Go helper eventtap_watchdog_failed, which is invoked from the C-side
// GCD timer block (watchdog_darwin.m) when the consecutive-failure counter
// reaches FAIL_THRESHOLD. wires the C side; the poller
// goroutine reads this latch and, on true, flips
// `watchdogTripped` to true and forwards a bare `struct{}` through the
// shared sink channel (typed sentinel forwarding
// was replaced with the atomic.Bool + bare-channel pair; see errors.go
// docstring for full history).
//
// As with `matched`, atomic.Bool is the only storage primitive
// permitted in the //export callback body per nosplit invariant.
var watchdogThresholdHit atomic.Bool

// watchdogTripped is the package-level latch read by cmd/dndmode/main.go
// (via the WatchdogTrippedSinceLastStart accessor) AFTER `sup.Wait()`
// returns, so the process can distinguish a watchdog-triggered abnormal
// exit (exit code 4 — the design notes abnormal-platform-stop) from a normal
// matched-hotkey exit (exit code 0).
//
// fix: previously exported as `WatchdogTripped atomic.Bool`,
// which let ANY goroutine in ANY package call `eventtap.WatchdogTripped.Store(true)`
// and corrupt the dndmode exit-code contract without going through the
// watchdog GCD timer. The threat model mirrored: a writable global
// keyed by exported name lets any in-process actor (including a careless
// future maintainer or a process-injected adversary) flip the latch out of
// band. Unexporting + adding a read-only accessor closes the writable-from-
// outside-package attack surface while keeping the cross-package
// signalling path (eventtap → main.go) intact.
//
// Lifecycle:
//
//   - Cleared at StartWatchdog (every fresh Start resets to false).
//   - Set to true by pollWatchdogThreshold immediately before forwarding
//     the threshold signal through the shared sink channel.
//   - Read by main.go via WatchdogTrippedSinceLastStart() after sup.Wait()
//     to choose between exitOK and the abnormal-exit code.
//
// We deliberately use a separate atomic.Bool rather than a typed envelope
// on supervisor.ExitTrigger to keep the Supervisor API surface unchanged
// (option (b) of 's two suggested fixes). The shared sink channel
// continues to carry struct{} signals; this latch encodes the source. The
// long-term option (a) — typed `ExitReason` channel on supervisor — was
// deferred on a minimal-patch basis; if it lands, the latch + accessor go
// away in favour of supervisor.LastExitReason().
var watchdogTripped atomic.Bool

// WatchdogTrippedSinceLastStart reports whether the watchdog has tripped
// (i.e. observed FAIL_THRESHOLD consecutive `CGEventTapIsEnabled == false`
// probes and forwarded the signal through the sink channel) since the most
// recent StartWatchdog call. cmd/dndmode/main.go reads this AFTER
// `sup.Wait()` returns to dispatch between exitOK (0) and exit code 4
// (the design notes abnormal-platform-stop).
//
// fix: read-only accessor that replaces the previously exported
// mutable `WatchdogTripped atomic.Bool` so callers outside this package
// cannot Store(true) into the dndmode exit-code contract. The internal
// `watchdogTripped` latch remains writable only by `pollWatchdogThreshold`
// (the single Go-side writer) and reset by `StartWatchdog`.
//
// Safe to call from any goroutine — atomic.Load is goroutine-safe.
func WatchdogTrippedSinceLastStart() bool {
	return watchdogTripped.Load()
}

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
//     must not enqueue duplicate watchdogTripped-flip signals into the
// sink channel (was previously typed
//     ErrWatchdogExitThreshold; see errors.go docstring).
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
// handler. Returns nil on success; non-nil error if the C side
// reports a failure (rc != 0).
//
// Return-code mapping from watchdog_darwin.m:
//   - 0 = success → nil
//   - 1 = tap NULL → caller-supplied error before this wrapper would also
//         have caught it via the `tap == nil` Go-side guard; preserved
//         for defence-in-depth
//   - 2 = watchdog already started (double-start without stop) — defensive
//   - 3 = GCD allocation failure (dispatch_source_create returned NULL)
//
// MUST be called from the main goroutine because the broader Install path
// holds invariants on main-thread setup ordering. The C
// side itself queries `dispatch_get_global_queue` which is thread-safe,
// but the caller chain requires main.
func startWatchdog(tap unsafe.Pointer) error {
	if tap == nil {
		return fmt.Errorf("watchdog: tap is nil")
	}
	rc := C.watchdog_start(C.CFMachPortRef(tap))
	if rc != 0 {
		return fmt.Errorf("watchdog_start: rc=%d", int(rc))
	}
	return nil
}

// stopWatchdog wires the Go-side cgo bridge into `watchdog_stop`
// (watchdog_darwin.m), which cancels and nils the dispatch_source_t.
// Called by Releaser.Release as part of the LIFO teardown chain.
//
// Idempotent — safe to call when no watchdog has been started.
func stopWatchdog() {
	C.watchdog_stop()
}

// StartWatchdog installs the GCD watchdog timer and spawns the Go-side
// threshold poller goroutine. Returns a `stop` closure that tears down
// both halves (poller goroutine + GCD source) in the correct order, and
// an error if the C-side `watchdog_start` failed.
//
// Wiring contract (composed with tap.Install in main.go):
//
//   - The `tap` parameter MUST be the `CFMachPortRef` returned by the
// successful `eventtap_install_c` call in. Passing a freed or
//     nil tap is a programming error (returns an error eagerly).
//
//   - The `sink` channel MUST be `supervisor.ExitTrigger()` — the same
// channel that the matched-key poller writes to. The
//     watchdog forwards a single struct{} send on threshold-hit; the
//     supervisor treats it identically to a matched key (LIFO unwind
// via Release order, exit code 4 per actual exit code
//     resolution lives in supervisor, not here).
//
//   - The `log` parameter MAY be nil — falls back to slog.Default()
//     (mirrors `cocoa.NewController` and `powerassert.Acquire`).
//
// The returned `stop` closure does:
//
//  1. Close the poller's internal stop channel — the goroutine exits its
//     ticker loop on the next iteration (<= 100ms).
//  2. Call `stopWatchdog()` — cancels the GCD source and nils the global
//     in watchdog_darwin.m. After this returns, the timer block will not
//     fire again (an in-flight invocation may still complete on the GCD
//     queue, but it sees `g_watchdog == nil` is irrelevant — the handler
//     captured `tap` by value, and the defensive null-check in the block
//     covers the freed-tap race).
//
// IMPORTANT: the closure does NOT release the underlying tap port —
// that ownership stays with `tap.Install`'s Releaser. main.go
// composes both: `stop()` first, then `tapReleaser.Release()`. Inverted
// order is unsafe (GCD handler in-flight may still call CGEventTapIsEnabled
// on a freed port).
//
// Safe to call from any goroutine, but expected to run from main (Install
// chain).
func StartWatchdog(tap unsafe.Pointer, sink chan<- struct{}, log *slog.Logger) (stop func(), err error) {
	if log == nil {
		log = slog.Default()
	}
	// Reset latch on every fresh Start — supports test fixtures and the
	// theoretical Stop-then-Start cycle, even though production has a
	// single Start per process lifetime (Install runs once). Reset
	// watchdogTripped alongside so an aborted prior watchdog
	// run cannot cause a fresh launch (in tests) to be misread as
	// abnormal.: both latches are now unexported; the
	// public accessor is WatchdogTrippedSinceLastStart().
	watchdogThresholdHit.Store(false)
	watchdogTripped.Store(false)

	if err := startWatchdog(tap); err != nil {
		return nil, err
	}

	stopPoller := make(chan struct{})
	go pollWatchdogThreshold(stopPoller, &watchdogThresholdHit, sink, log)

	stop = func() {
		// Stop the poller FIRST so it cannot observe a stale latch flip
		// between watchdog_stop and the close below. (Even if the GCD
		// block managed to flip the latch after we stop the C side, the
		// poller goroutine is already on its way out — a benign late
		// Store(true) just goes nowhere.)
		close(stopPoller)
		stopWatchdog()
	}
	return stop, nil
}

// pollWatchdogThreshold is the Go-side goroutine that bridges the C-side
// atomic latch (`watchdogThresholdHit`, written by the GCD block via the
// //export eventtap_watchdog_failed callback) to the sink channel and
// stderr log line.
//
// Single-shot semantics: as soon as `flag.CompareAndSwap(true, false)`
// observes a flipped latch, the goroutine
//
// 1. Logs the message verbatim ("eventtap watchdog: tap dead after
//     5 re-enable failures, exiting to restore input").
//  2. Performs a non-blocking sink send (`select { default: }`) so that a
//     full supervisor channel (race against a matched-key send) does not
//     deadlock the watchdog.
//  3. Returns — the poller does NOT process subsequent latch flips.
//     Repeated threshold-hits from a still-running GCD block (in the
//     window between Store(true) here and stopWatchdog's
//     dispatch_source_cancel) are dropped silently. The supervisor only
//     needs ONE signal to unwind; duplicates would queue a second exit
// and confuse the LIFO sequence.
//
// The `stop` channel terminates the goroutine cleanly even when the
// threshold was never hit (the common case — a healthy tap that never
// gets disabled). `time.Ticker.Stop` runs in defer.
//
// The `flag` parameter is taken by pointer for testability — production
// passes `&watchdogThresholdHit`, but unit tests (if added in)
// could pass a local atomic.Bool to exercise the poller without standing
// up cgo.
//
// SAFETY: this goroutine is NOT pinned to an OS thread via
// runtime.LockOSThread because it only does atomic Loads + channel
// operations + stderr writes — none of which require thread affinity.
//
// history: a prior version of this comment claimed "the matched-
// key poller in DOES use LockOSThread because its 10ms ticker
// contends with GCD blocks." That was wrong — the matched-key poller
// (tap_darwin.go installInternal, pollMatched goroutine) does NOT
// LockOSThread either, for the same reason this one doesn't: atomic
// reads + non-blocking channel sends don't require thread affinity.
// The CGEventTap WORKER goroutine (the one that runs CFRunLoopRun on
// the C-side run loop) DOES LockOSThread — easy to confuse with the
// poller, but they are distinct goroutines.
func pollWatchdogThreshold(stop <-chan struct{}, flag *atomic.Bool, sink chan<- struct{}, log *slog.Logger) {
	ticker := time.NewTicker(watchdogPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if flag.CompareAndSwap(true, false) {
			// verbatim — must match watchdog_test.go acceptance
			// (and the errors.go "Watchdog signalling contract" docstring,
			// which absorbed the deletion of the typed
			// ErrWatchdogExitThreshold sentinel).
			log.Error("eventtap watchdog: tap dead after 5 re-enable failures, exiting to restore input")
			// signal the abnormal-exit source BEFORE sending
			// to the shared sink. The sink channel is shared with the
			// matched-hotkey poller, so the supervisor cannot tell
			// which source fired. main.go reads
			// WatchdogTrippedSinceLastStart() AFTER sup.Wait() returns
			// and maps true → exitPlatformErr (instead of exitOK),
			// restoring the the design notes abnormal-platform-stop
			// contract. Store-before-send is critical: the supervisor
			// may observe the sink signal and run RequestStop →
			// ctx.cancel → cocoa.RunApp returns → sup.Wait returns →
			// main.go reads the latch — all of that races us if we
			// stored AFTER the send. Storing before the send + Go's
			// happens-before on a channel op published by a single
			// writer ensures main sees true.: the
			// latch is now unexported (`watchdogTripped`); this
			// goroutine is the only Go-side writer.
			watchdogTripped.Store(true)
			// Non-blocking send — a full sink (race with matched-key
			// send) MUST NOT deadlock the watchdog. The supervisor
			// only needs one signal to start unwinding either way.
			select {
			case sink <- struct{}{}:
			default:
			}
			// Single-shot: stop processing further latch flips.
			return
		}
	}
}
