//go:build darwin

package eventtap

/*
#cgo CFLAGS: -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Foundation -framework CoreGraphics

#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

extern int  watchdog_start(CFMachPortRef tap);
extern int  watchdog_stop(void);
*/
import "C"

import (
	"fmt"
	"log/slog"
	"sync"
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
// (watchdog_darwin.m), which cancels the dispatch_source_t AND waits for
// any in-flight handler invocation to finish. Called by Releaser.Release as
// part of the LIFO teardown chain, BEFORE either tap is released.
//
// Returns true when the drain was established (no handler is running and
// none can start again), false when the bounded wait expired first. The
// caller surfaces a false at WARN: it is the one signal that the ordering
// guarantee the rest of the teardown chain leans on did not hold this time.
// It is not a fatal condition for the CURRENT teardown — watchdog_start
// CFRetains BOTH the main tap and the gesture tap for the source's lifetime
// and drops those references only from the cancel handler, so a late handler
// dereferences live mach ports regardless — so nothing aborts on it. The
// caller does latch `teardownUnclean` on a false, which forbids any LATER
// install in this process: a stale handler is harmless against its own
// session's state but not against a second session's, and no C-side guard
// can cover a deschedule that lands after its last check. See the WARN site
// in StartWatchdog's stop closure for the two cross-session effects.
//
// Idempotent — safe to call when no watchdog has been started (returns
// true: no source means no handler).
func stopWatchdog() bool {
	return C.watchdog_stop() == 0
}

// watchdogLifecycle serialises a watchdog session's OPEN against its CLOSE.
// Held for the whole of `StartWatchdog` and for the whole of the `stop`
// closure it returns, so the two can never interleave.
//
// It exists because the `teardownUnclean` guard in StartWatchdog is a
// check-then-act, and the act it guards against becomes available BEFORE the
// latch that forbids it is set. `watchdog_stop` nils `g_watchdog` — freeing
// the C-side "already started" slot — and only THEN performs its bounded
// cancel-handler wait; the Go side latches `teardownUnclean` after that wait
// reports a timeout. A concurrent `StartWatchdog` landing inside that window
// reads the latch as false, finds the C slot free, and opens a second session
// while a handler of the first is still in flight — precisely the state the
// latch was added to make impossible. The stale handler's
// `eventtap_watchdog_failed()` flips a package-global latch carrying no
// session identity, so the fresh poller would read that trip as its own and
// drive main.go into a spurious abnormal exit; `gesturetap_reenable()` would
// likewise act on the new session's port, which the stale handler never
// retained. Both are the effects documented on `g_watchdog_gen` as
// "structurally unreachable because no second session can exist" — this mutex
// is what keeps that sentence true when Start and stop run on different
// goroutines.
//
// Scope is the COMPLETE transition, not just the drain: StartWatchdog's latch
// resets and poller spawn are as much a part of opening a session as
// `watchdog_start` is, and stop's poller close+join is as much a part of
// closing one as `watchdog_stop` is. A mutex around only the C calls would
// leave the previous poller's `CompareAndSwap` racing the next session's
// `Store(false)`.
//
// Deadlock-free and bounded: neither critical section blocks on the other,
// `pollWatchdogThreshold` never touches this mutex, and the two waits inside
// (poller join <= watchdogPollInterval, cancel drain <=
// DND_WATCHDOG_DRAIN_TIMEOUT_NS) are both bounded, so a contending
// StartWatchdog waits at most ~600ms. No lock-order inversion with
// `Releaser.mu`: Release takes its own mutex and then this one via the stop
// closure, and StartWatchdog never takes Releaser.mu at all.
var watchdogLifecycle sync.Mutex

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
//  1. Close the poller's internal stop channel and then WAIT for the
//     goroutine to exit (<= 100ms, one ticker period). The wait is not
//     cosmetic: the poller's latches and sink are package-globals that the
//     next StartWatchdog resets and reuses, so a goroutine still in flight
//     after stop returned could consume the new session's threshold signal
//     or flip the exit-code latch it just cleared. Mirrors the tap
//     Releaser's own stopPoller/pollerDone pair.
//  2. Call `stopWatchdog()` — cancels the GCD source, nils the global in
//     watchdog_darwin.m, and BLOCKS until the source's cancel handler has
//     run. libdispatch invokes that handler only after the last event
//     handler has returned, so once this closure returns true no handler is
//     running and none can start again. A false means the bounded wait
//     expired instead; it is logged at WARN and latches `teardownUnclean`
//     (so this process may not install a tap — and therefore may not open a
//     second watchdog session — again), but nothing aborts, because the
//     CFRetains watchdog_start holds on the main tap AND on the gesture tap
//     keep both ports alive for a late handler either way.
//
// IMPORTANT: the closure does NOT release the underlying tap port —
// that ownership stays with the Releaser built by `installInternal`.
// `Releaser.Release` runs this closure BEFORE the CF teardown of either
// tap, which is what makes "handler drained" mean "safe to CFRelease".
// Inverted order is unsafe (a GCD handler in-flight may still call
// CGEventTapIsEnabled on a freed port) — it is also what the composite
// Release used to do, and fixing it is why this closure now drains.
//
// Returns ErrTeardownUnclean — before touching either latch or the C side
// — when a previous teardown in this process could not prove its callback
// idle. `installInternal` already refuses on the same latch, so in the
// composed path this check never fires; it is here because `StartWatchdog`
// is exported and therefore is its own entry point into a fresh watchdog
// session, and the two things a session opens (the `watchdogThresholdHit` /
// `watchdogTripped` reset below, and the GCD source in `watchdog_start`)
// are exactly the state a stale handler from the previous session can still
// reach. Guarding the constructor itself, rather than only its one current
// caller, keeps that argument true of the function instead of true of the
// call graph.
//
// That guard is a check-then-act, so it is not self-sufficient: the C side
// frees its session slot BEFORE the bounded drain whose timeout sets the
// latch, leaving a window in which the latch reads false and a second session
// is nonetheless installable. `watchdogLifecycle` closes it by serialising
// this whole function against the whole `stop` closure — see that mutex's doc
// comment for the two cross-session effects a Start inside the window would
// re-open.
//
// Safe to call from any goroutine, but expected to run from main (Install
// chain). A call concurrent with a `stop` in progress does not race — it
// blocks on `watchdogLifecycle` until that teardown has finished writing its
// verdict, then sees the settled latch.
func StartWatchdog(tap unsafe.Pointer, sink chan<- struct{}, log *slog.Logger) (stop func(), err error) {
	if log == nil {
		log = slog.Default()
	}

	// Held across the ENTIRE open — guard, latch resets, `watchdog_start`,
	// poller spawn — so no `stop` closure can be mid-teardown while any of
	// it runs. Without it the guard below is a check-then-act against a
	// window `watchdog_stop` deliberately opens: it frees the C-side session
	// slot before its bounded drain, and `teardownUnclean` is only latched
	// after that drain reports a timeout. See the mutex's doc comment.
	watchdogLifecycle.Lock()
	defer watchdogLifecycle.Unlock()

	// Checked BEFORE the latch resets below: clearing `watchdogTripped` is
	// itself observable (main.go maps it to the abnormal exit code), so a
	// rejected Start must leave both latches exactly as it found them.
	if teardownUnclean.Load() {
		return nil, ErrTeardownUnclean
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
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		pollWatchdogThreshold(stopPoller, &watchdogThresholdHit, sink, log)
	}()

	stop = func() {
		// Held across the ENTIRE close, for the same reason StartWatchdog
		// holds it across the entire open: the two halves of this closure
		// (poller join, C-side drain) each publish state the next session
		// resets and reuses, and the `teardownUnclean` store below is the
		// only thing standing between a timed-out drain and a second
		// session. Taking the mutex here rather than around `stopWatchdog`
		// alone is what makes "no Start can observe the latch before this
		// closure has finished writing it" a property of the code and not of
		// the caller. See the mutex's doc comment.
		watchdogLifecycle.Lock()
		defer watchdogLifecycle.Unlock()

		// Stop the poller FIRST so it cannot observe a stale latch flip
		// between watchdog_stop and the close below. (Even if the GCD
		// block managed to flip the latch after we stop the C side, the
		// poller goroutine is already on its way out — a benign late
		// Store(true) just goes nowhere.)
		close(stopPoller)
		// Then JOIN it, mirroring what the tap Releaser does with its own
		// poller (tap_darwin.go Step 3). Closing the channel only asks the
		// goroutine to leave; it can still be between its ticker wake-up and
		// its CompareAndSwap, and every piece of state it touches from there
		// is package-global and reused by the NEXT StartWatchdog:
		// `watchdogThresholdHit` (whose CAS it would consume, swallowing the
		// new session's threshold trip), `watchdogTripped` (which the new
		// StartWatchdog just cleared, and which main.go maps to the abnormal
		// exit code), and `sink` (the PREVIOUS supervisor's channel). Waiting
		// here makes "stop returned" mean the goroutine is gone, which is the
		// only thing that makes those globals safe to reset.
		//
		// Bounded by watchdogPollInterval (100ms) and deadlock-free: the
		// poller takes no locks and its only send is non-blocking.
		<-pollerDone
		if !stopWatchdog() {
			// Latch the process as un-reinstallable, exactly as the tap's
			// own drain-timeout paths do — see the `teardownUnclean`
			// docstring in tap_darwin.go, which this case now shares.
			//
			// A timeout here means a GCD handler of this session may still
			// be in flight with no bound on when it resumes. Two of the
			// things such a handler does are safe against its OWN session
			// (watchdog_start CFRetains both ports for the source's
			// lifetime, and its failure counter is `__block`-local) but not
			// against a LATER one, because both reach state that a restart
			// republishes:
			//
			//   - `gesturetap_reenable()` loads `g_gesture_tap` fresh at
			//     call time, so a stale handler that resumed after a
			//     re-Install would enable — and could dereference — the NEW
			//     session's gesture port, which it never retained.
			//   - `eventtap_watchdog_failed()` flips a package-global latch
			//     carrying no session identity, so a stale handler reaching
			//     its fifth local failure would trip the NEW session's
			//     poller into a spurious watchdog exit.
			//
			// Neither is reachable from a C-side guard: both windows open
			// after the handler's last generation / identity check, and no
			// single check can cover a deschedule that lands behind it. What
			// closes them is that both need a second session to exist at
			// all, and this latch guarantees none can: `installInternal`
			// refuses with ErrTeardownUnclean and is the only caller of
			// `gesturetap_install_c`, and `StartWatchdog` re-checks the same
			// latch itself, so the exported constructor cannot open a second
			// watchdog session behind installInternal's back either. The
			// stale handler is then left with the ports it retained and a
			// latch whose poller was already joined above.
			//
			// The store below happens under `watchdogLifecycle`, which this
			// closure took on entry. That is what makes the latch a real
			// barrier rather than a late announcement: the C-side session
			// slot fell free inside `stopWatchdog` (it nils `g_watchdog`
			// before its bounded wait), so between there and here a
			// concurrent `StartWatchdog` would have found both the guard
			// false and the slot available. Holding the mutex across the
			// whole closure means any such Start is still parked at the lock
			// and will read the latch already set.
			//
			// WARN, not DEBUG: the teardown chain that runs after this
			// closure CFReleases both taps on the strength of "no handler
			// can be running", and this line is the only place that
			// assumption is ever observably violated. Both ports stay alive
			// for a late handler either way (only the source's cancel
			// handler drops those retains), so this is a diagnostic plus a
			// latch, not a failure the caller must act on.
			teardownUnclean.Store(true)
			log.Warn("eventtap watchdog: cancel-handler drain timed out; a probe may still be in flight, tap is not reinstallable in this process")
		}
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
// (tap_darwin.go installInternal, pollSequence goroutine) does NOT
// LockOSThread either: its cgo calls into the keystroke ring are plain
// ACQUIRE loads and a memcpy — no thread affinity required, the same way
// this one's atomic reads + non-blocking channel sends don't.
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
