//go:build darwin

package eventtap

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// testDeps groups dependencies for watchdogState policy tests. The pure-Go
// DI seam has no external dependencies (no logger, no cgo) — testDeps stays
// minimal to satisfy "Go Testing Conventions" while keeping each
// case self-contained.
type testDeps struct {
	state *watchdogState
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	return &testDeps{state: &watchdogState{}}
}

// probeResult is the canonical (reset, threshold) tuple returned by
// watchdogState.Probe. Tests use it via a validateResp closure per the
// convention; the result type lets us re-use a single
// assertion shape across multiple test cases.
type probeResult struct {
	reset     bool
	threshold bool
}

// TestWatchdog_Threshold_Triggers_AfterFiveConsecutiveFailures verifies the
// contract: exactly five consecutive `Probe(false)` calls (representing
// `CGEventTapIsEnabled == false` after a re-enable attempt) trip the
// threshold. The first four MUST return `threshold=false`; the fifth MUST
// return `threshold=true`. The counter mirrors the C-side `g_fail_count`
// (watchdog_darwin.m FAIL_THRESHOLD == 5) so the unit-test exhaustively
// pins the policy without touching cgo.
func TestWatchdog_Threshold_Triggers_AfterFiveConsecutiveFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setupMocks   func(d *testDeps)
		validateResp func(t *testing.T, got probeResult)
	}{
		{
			name: "probe_1_false_below_threshold",
			setupMocks: func(d *testDeps) {
				// no prior state — fresh watchdogState
			},
			validateResp: func(t *testing.T, got probeResult) {
				if got.threshold {
					t.Errorf("threshold=true at probe #1, want false (needs 5 consecutive failures)")
				}
				if got.reset {
					t.Errorf("reset=true at probe #1 of failure run, want false (only Probe(true) resets)")
				}
			},
		},
		{
			name: "probe_4_false_still_below_threshold",
			setupMocks: func(d *testDeps) {
				// pre-seed 3 failures so this call is the 4th
				for range 3 {
					d.state.Probe(false)
				}
			},
			validateResp: func(t *testing.T, got probeResult) {
				if got.threshold {
					t.Errorf("threshold=true at probe #4, want false (5 are needed)")
				}
			},
		},
		{
			name: "probe_5_false_trips_threshold",
			setupMocks: func(d *testDeps) {
				// pre-seed 4 failures so this call is the 5th
				for i := range 4 {
					if _, th := d.state.Probe(false); th {
						t.Fatalf("threshold tripped early at i=%d", i)
					}
				}
			},
			validateResp: func(t *testing.T, got probeResult) {
				if !got.threshold {
					t.Errorf("threshold=false at probe #5, want true (contract)")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps(t)
			tt.setupMocks(d)
			reset, threshold := d.state.Probe(false)
			tt.validateResp(t, probeResult{reset: reset, threshold: threshold})
		})
	}
}

// TestWatchdog_HealthyProbe_ResetsCounter verifies the "any healthy
// probe resets the counter to 0" contract. After 4 consecutive failures,
// one `Probe(true)` (representing `CGEventTapIsEnabled == true`) MUST reset
// the internal counter so the watchdog needs 5 NEW consecutive failures —
// NOT just one more — to trip the threshold.
func TestWatchdog_HealthyProbe_ResetsCounter(t *testing.T) {
	t.Parallel()

	d := newTestDeps(t)

	// Phase 1: feed 4 failures. None should trip.
	for i := range 4 {
		_, threshold := d.state.Probe(false)
		if threshold {
			t.Fatalf("threshold tripped at failure #%d, want untouched", i+1)
		}
	}

	// Phase 2: a single healthy probe. MUST return reset=true.
	reset, threshold := d.state.Probe(true)
	if !reset {
		t.Errorf("after Probe(true): reset=false, want true (healthy probe resets counter)")
	}
	if threshold {
		t.Errorf("after Probe(true): threshold=true, want false (counter freshly reset)")
	}

	// Phase 3: one failure — counter is now back at 1, not 5. MUST NOT trip.
	_, threshold = d.state.Probe(false)
	if threshold {
		t.Errorf("after 1st failure post-reset: threshold=true, want false (counter starts from 0)")
	}

	// Phase 4: 3 more failures — total 4 since reset; STILL no threshold.
	for i := range 3 {
		_, threshold = d.state.Probe(false)
		if threshold {
			t.Fatalf("threshold tripped at post-reset failure #%d, want untouched until 5th", i+2)
		}
	}

	// Phase 5: 5th failure since reset — NOW threshold trips.
	_, threshold = d.state.Probe(false)
	if !threshold {
		t.Errorf("after 5th failure post-reset: threshold=false, want true (counter restart)")
	}
}

// TestWatchdog_Threshold_Idempotent verifies that after the threshold is
// first tripped, subsequent `Probe(false)` calls return `threshold=false`.
// The watchdog signals the threshold exactly once: the GCD timer (
//) cancels itself after the first hit, but if any straggler probe
// fires before cancellation propagates we MUST NOT re-emit a watchdog
// trip into the sink channel — that would queue a second supervisor exit
// and confuse the unwind. (Historical note: prior to
// this contract was documented as "must not re-emit ErrWatchdogExitThreshold";
// the typed sentinel was deleted alongside 's atomic-bool fix
// see errors.go "Watchdog signalling contract" docstring for the full
// signalling-shape history.)
func TestWatchdog_Threshold_Idempotent(t *testing.T) {
	t.Parallel()

	d := newTestDeps(t)

	// Trip the threshold.
	for i := range 4 {
		if _, th := d.state.Probe(false); th {
			t.Fatalf("threshold tripped early at i=%d", i)
		}
	}
	_, threshold := d.state.Probe(false)
	if !threshold {
		t.Fatalf("threshold did not trip on 5th failure (setup precondition failed)")
	}

	// Subsequent probes MUST return threshold=false. Test both
	// failure and healthy probes — both must be silent post-threshold.
	for i, isEnabled := range []bool{false, false, true, false, true} {
		_, th := d.state.Probe(isEnabled)
		if th {
			t.Errorf("probe #%d post-threshold (isEnabled=%v): threshold=true, want false (idempotent contract)",
				i+1, isEnabled)
		}
	}
}

// TestWatchdog_State_ZeroValueSafe verifies that `var s watchdogState`
// produces a usable zero value — no required initialiser, no panic on
// first Probe. This matters because the production wiring
// allocates `watchdogState` as a struct literal inside the GCD timer
// closure: any zero-value init quirk would manifest only at runtime.
func TestWatchdog_State_ZeroValueSafe(t *testing.T) {
	t.Parallel()

	var s watchdogState

	reset, threshold := s.Probe(true)
	if !reset {
		t.Errorf("zero-value Probe(true): reset=false, want true (healthy probe always resets)")
	}
	if threshold {
		t.Errorf("zero-value Probe(true): threshold=true, want false")
	}
	if s.failCount != 0 {
		t.Errorf("zero-value failCount after Probe(true) = %d, want 0", s.failCount)
	}
}

// pollerTestDeps groups dependencies for pollWatchdogThreshold goroutine
// tests. Mirrors testDeps for watchdogState (above) but adds the
// channel/atomic plumbing that the poller goroutine needs.
// "Go Testing Conventions" convention.
type pollerTestDeps struct {
	flag *atomic.Bool
	sink chan struct{}
	stop chan struct{}
	log  *slog.Logger
}

func newPollerTestDeps(t *testing.T) *pollerTestDeps {
	t.Helper()
	return &pollerTestDeps{
		flag: &atomic.Bool{},
		sink: make(chan struct{}, 1),
		stop: make(chan struct{}),
		// Discard logger: log line is verified verbatim in the C
		// side + acceptance smoke tests; this Go-side unit test
		// exercises the atomic + channel contract only.
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestWatchdog_PollThreshold_TripsWatchdogTrippedAndSink is the
// regression guard introduced in. It pins the new contract
// an earlier fix introduced:
//
//	When the watchdog poller observes the threshold-hit latch flipped to
//	true, it MUST (a) flip the package-level `watchdogTripped` latch to
//	true BEFORE sending on the sink channel, AND (b) deliver a single
//	struct{} on the sink, AND (c) be readable via
//	WatchdogTrippedSinceLastStart() from external code.
//
// The exact failure mode was meant to prevent — silent collapse to
// exit code 0 when the watchdog has actually tripped — is exercised here
// without standing up cgo / GCD / a live CGEventTap. A future maintainer
// who drops the `watchdogTripped.Store(true)` line from
// pollWatchdogThreshold, or who refactors main.go and drops the
// `WatchdogTrippedSinceLastStart()` branch, will regress THIS test.
//
// Pre- there was zero coverage of this contract — `watchdog_test.go`
// exercised only the pure-Go `watchdogState.Probe` policy, and
// `acceptance_test.go` had no parallel for. See the design notes
// for the "fixes-without-tests" rationale.
func TestWatchdog_PollThreshold_TripsWatchdogTrippedAndSink(t *testing.T) {
	// NOT t.Parallel() — this test mutates the package-level
	// `watchdogTripped` global. The other tests in this file touch only
	// stack-local `watchdogState`, so serialising this one is sufficient
	// for race-freedom without forcing them onto a serial schedule.

	tests := []struct {
		name         string
		setupMocks   func(d *pollerTestDeps)
		validateResp func(t *testing.T, d *pollerTestDeps, sinkRecv bool, sinkDuration time.Duration)
	}{
		{
			name: "threshold_flag_set_to_true_trips_watchdogTripped_and_sends_on_sink",
			setupMocks: func(d *pollerTestDeps) {
				// Flip the threshold-hit latch — the GCD-block surrogate
				// for production's `eventtap_watchdog_failed` //export
				// callback.
				d.flag.Store(true)
			},
			validateResp: func(t *testing.T, d *pollerTestDeps, sinkRecv bool, sinkDuration time.Duration) {
				if !sinkRecv {
					t.Errorf("sink did not receive within timeout (waited %s); want one struct{} send", sinkDuration)
				}
				if !watchdogTripped.Load() {
					t.Errorf("watchdogTripped=false after threshold trip, want true (contract: store-before-send)")
				}
				if !WatchdogTrippedSinceLastStart() {
					t.Errorf("WatchdogTrippedSinceLastStart()=false after trip, want true (accessor contract)")
				}
			},
		},
		{
			name: "stop_without_flag_set_exits_cleanly_no_sink_send",
			setupMocks: func(d *pollerTestDeps) {
				// Do NOT flip the flag — the healthy-tap common case.
				// Close stop after a couple of ticker periods so the
				// poller has had a chance to spin its loop.
				go func() {
					time.Sleep(3 * watchdogPollInterval)
					close(d.stop)
				}()
			},
			validateResp: func(t *testing.T, d *pollerTestDeps, sinkRecv bool, sinkDuration time.Duration) {
				if sinkRecv {
					t.Errorf("sink received a value despite flag never flipping (violation)")
				}
				if watchdogTripped.Load() {
					t.Errorf("watchdogTripped=true after clean stop, want false (no threshold trip occurred)")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the package-level latch before EACH subtest to keep
			// them order-independent. Mirrors what StartWatchdog does in
			// production. Use t.Cleanup so a panicking subtest still
			// resets for the next one (subtest order is sequential here
			// — t.Parallel deliberately omitted to avoid concurrent
			// mutation of the shared latch).
			watchdogTripped.Store(false)
			t.Cleanup(func() { watchdogTripped.Store(false) })

			d := newPollerTestDeps(t)
			tt.setupMocks(d)

			// Launch poller goroutine.
			done := make(chan struct{})
			go func() {
				defer close(done)
				pollWatchdogThreshold(d.stop, d.flag, d.sink, d.log)
			}()

			// Wait for sink delivery within a bounded window. The poller
			// ticks at watchdogPollInterval (100ms); 10× cap is generous
			// for the trip path AND leaves the no-trip subtest enough
			// time to exit via its scheduled `close(d.stop)`.
			timeout := 10 * watchdogPollInterval
			start := time.Now()
			var sinkRecv bool
			select {
			case <-d.sink:
				sinkRecv = true
			case <-time.After(timeout):
				sinkRecv = false
			}
			sinkDuration := time.Since(start)

			// For the trip case the poller is single-shot and returns by
			// itself; for the no-trip case `close(d.stop)` is scheduled
			// inside setupMocks. Either way the goroutine exits within
			// the cleanup window — wait for it so the race detector
			// doesn't flag a leaked goroutine across t.Run boundaries.
			if !sinkRecv {
				// no-trip path: poller is still running, wait for the
				// scheduled close(d.stop) to land.
				select {
				case <-done:
				case <-time.After(2 * timeout):
					t.Fatalf("poller goroutine did not exit within %s after stop close", 2*timeout)
				}
			} else {
				// trip path: ensure single-shot exit happens.
				select {
				case <-done:
				case <-time.After(timeout):
					t.Fatalf("poller did not return after sink delivery (single-shot contract violated)")
				}
			}

			tt.validateResp(t, d, sinkRecv, sinkDuration)
		})
	}
}

// TestWatchdog_PollThreshold_SinkFull_DoesNotDeadlock verifies that a full
// sink channel (race against a matched-key send from poller.go) MUST NOT
// deadlock the watchdog poller. The non-blocking `select { default: }`
// inside pollWatchdogThreshold is the load-bearing primitive; this test
// pins the "sink full → drop signal, set watchdogTripped, return"
// contract documented in the inline comment.
//
// This is a sibling regression to same family of " fixed
// but undefended" gaps. A future maintainer who removes the
// `select { default: }` non-blocking guard (mistakenly thinking the
// supervisor always drains promptly) would regress to a deadlock that
// hangs the supervisor unwind path — silent in CI until the production
// race fires.
func TestWatchdog_PollThreshold_SinkFull_DoesNotDeadlock(t *testing.T) {
	// NOT t.Parallel() — see sibling test rationale.

	watchdogTripped.Store(false)
	t.Cleanup(func() { watchdogTripped.Store(false) })

	d := newPollerTestDeps(t)
	// Pre-fill the sink so the watchdog's send-attempt hits `default:`.
	d.sink <- struct{}{}
	d.flag.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pollWatchdogThreshold(d.stop, d.flag, d.sink, d.log)
	}()

	// The poller must exit (single-shot) even though the sink was full.
	// If the non-blocking guard were removed, this would hang.
	select {
	case <-done:
	case <-time.After(10 * watchdogPollInterval):
		// Best-effort cleanup so other tests aren't poisoned: close
		// stop and bail. A leaked goroutine here is itself the failure
		// mode the test guards against.
		close(d.stop)
		t.Fatalf("poller did not exit when sink was full (non-blocking send guard regressed)")
	}

	// watchdogTripped must still be set — full sink means the supervisor
	// will not see the abnormal-exit signal, but main.go reads the
	// accessor AFTER sup.Wait() returns regardless, so the latch is the
	// single source of truth for the exit-code branch.
	if !watchdogTripped.Load() {
		t.Errorf("watchdogTripped=false after full-sink trip, want true (latch is the source of truth)")
	}
}

// TestStartWatchdog_AfterUncleanTeardown_Rejected pins the guard that makes
// `teardownUnclean` a property of the watchdog constructor rather than of its
// current call graph.
//
// `installInternal` already refuses on this latch, and it is today the only
// caller of `StartWatchdog` — but `StartWatchdog` is exported, so "no second
// watchdog session can open after an unclean drain" was an invariant of the
// call graph, not of the function. That distinction matters here more than
// anywhere else in the package: a session that opens after a timed-out cancel
// drain inherits a GCD handler with no bound on when it resumes, and that
// handler's `eventtap_watchdog_failed()` flips a package-global latch carrying
// no session identity — so the fresh poller would read a stale trip as its own
// and drive main.go into a spurious abnormal exit, i.e. an unlock nobody asked
// for.
//
// The check must also run BEFORE the two latch resets: clearing
// `watchdogTripped` is observable (main.go maps it to the exit code), so a
// rejected Start has to leave both latches exactly as it found them.
//
// NOT t.Parallel: it mutates process-global latches.
func TestStartWatchdog_AfterUncleanTeardown_Rejected(t *testing.T) {
	prevTeardown := teardownUnclean.Load()
	prevTripped := watchdogTripped.Load()
	prevHit := watchdogThresholdHit.Load()
	t.Cleanup(func() {
		teardownUnclean.Store(prevTeardown)
		watchdogTripped.Store(prevTripped)
		watchdogThresholdHit.Store(prevHit)
	})

	// Seed both latches true so a reset is observable as a cleared latch.
	watchdogTripped.Store(true)
	watchdogThresholdHit.Store(true)
	teardownUnclean.Store(true)

	sink := make(chan struct{}, 1)

	// A nil tap on purpose, for two reasons. It keeps the test honest about
	// ordering: the latch check has to come before startWatchdog's own
	// `tap == nil` guard for ErrTeardownUnclean to be what comes back. And it
	// keeps the FAILURE clean — a bogus non-nil pointer would reach
	// `watchdog_start` if the guard were ever removed and take the whole test
	// binary down with a SIGSEGV inside cgo instead of reporting which
	// assertion broke.
	stop, err := StartWatchdog(nil, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, ErrTeardownUnclean) {
		t.Errorf("StartWatchdog after unclean teardown: error = %v, want ErrTeardownUnclean", err)
	}
	if stop != nil {
		t.Error("StartWatchdog after unclean teardown returned a non-nil stop closure alongside the error")
		stop()
	}

	// Latch resets must not have happened — they are the first observable
	// effect of a session that was supposed to be refused.
	if !watchdogTripped.Load() {
		t.Error("rejected StartWatchdog cleared watchdogTripped; the reject path must not touch it")
	}
	if !watchdogThresholdHit.Load() {
		t.Error("rejected StartWatchdog cleared watchdogThresholdHit; the reject path must not touch it")
	}

	// A guard that bailed out after spawning the poller would show up here.
	select {
	case <-sink:
		t.Error("rejected StartWatchdog still signalled the exit sink")
	default:
	}

	// One-way within the process: nothing on the reject path may clear it.
	if !teardownUnclean.Load() {
		t.Error("teardownUnclean was cleared by a rejected StartWatchdog; the latch must stay set")
	}
}

// TestStartWatchdog_SerialisesAgainstStopTeardown pins the mutex that turns
// the `teardownUnclean` guard from a check-then-act into a barrier.
//
// The guard alone is not enough, and the gap is not hypothetical ordering
// pedantry — it is baked into the teardown sequence. `watchdog_stop` nils
// `g_watchdog` (freeing the C-side "already started" slot) BEFORE its bounded
// cancel-handler wait, and the Go side latches `teardownUnclean` only after
// that wait reports a timeout. Between those two instants a concurrent
// `StartWatchdog` finds the latch false AND the slot free, and opens a second
// session while a handler of the first is still in flight — which is exactly
// the state that lets a stale `eventtap_watchdog_failed()` trip the fresh
// poller's session-less latch and unlock a machine nobody unlocked.
//
// What is actually asserted: with `watchdogLifecycle` held by someone else,
// StartWatchdog neither returns nor performs its FIRST observable act (the
// latch resets). That is the whole claim — the lock is taken before any
// state is touched, not somewhere in the middle. Directly driving a real
// stop closure into its drain window would need a live CGEventTap and
// Accessibility, so the mutex is exercised from the side that pure Go can
// reach; the stop closure's own Lock is one statement and reviewed as such.
//
// NOT t.Parallel: it mutates process-global latches and the lifecycle mutex.
func TestStartWatchdog_SerialisesAgainstStopTeardown(t *testing.T) {
	prevTeardown := teardownUnclean.Load()
	prevTripped := watchdogTripped.Load()
	prevHit := watchdogThresholdHit.Load()
	t.Cleanup(func() {
		teardownUnclean.Store(prevTeardown)
		watchdogTripped.Store(prevTripped)
		watchdogThresholdHit.Store(prevHit)
	})

	// Latch state a rejected-or-parked Start must leave untouched, and a
	// running one is guaranteed to clear — so "did StartWatchdog get past
	// the lock?" is observable without waiting for its return value.
	watchdogTripped.Store(true)
	watchdogThresholdHit.Store(true)
	// Deliberately NOT set: this test is about the window in which the latch
	// is still false and a second session is therefore still permitted. With
	// it set, StartWatchdog would bail on the guard and prove nothing about
	// ordering.
	teardownUnclean.Store(false)

	// Stand in for a stop closure that is inside `stopWatchdog` — the C slot
	// already free, the verdict not yet written.
	watchdogLifecycle.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			watchdogLifecycle.Unlock()
		}
	}
	// Releasing from a different goroutine than the one that acquired is
	// legal for sync.Mutex (no owner tracking); the cleanup exists so a
	// failed assertion cannot wedge every later test in the package.
	t.Cleanup(unlock)

	sink := make(chan struct{}, 1)
	// Nil tap for the same reason as the rejection test: StartWatchdog's own
	// `startWatchdog` nil-guard returns before any C call, so a Start that
	// DOES get through the lock fails loudly instead of taking the test
	// binary down inside cgo.
	done := make(chan error, 1)
	go func() {
		stop, err := StartWatchdog(nil, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if stop != nil {
			stop()
		}
		done <- err
	}()

	// Generous enough that a StartWatchdog which does not take the mutex is
	// certain to have finished, short enough to stay a cheap unit test.
	select {
	case err := <-done:
		unlock()
		t.Fatalf("StartWatchdog returned (%v) while the lifecycle mutex was held; it must serialise against an in-progress teardown", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The parked Start must not have touched a thing. A Lock placed after
	// the latch resets — or after the guard — would show up right here.
	if !watchdogTripped.Load() {
		t.Error("watchdogTripped cleared while the lifecycle mutex was held; StartWatchdog must take the lock before its latch resets")
	}
	if !watchdogThresholdHit.Load() {
		t.Error("watchdogThresholdHit cleared while the lifecycle mutex was held; StartWatchdog must take the lock before its latch resets")
	}

	// Hand the session over, exactly as a completed stop closure would.
	unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Error("StartWatchdog(nil tap) returned a nil error; the nil-tap guard regressed")
		}
		if errors.Is(err, ErrTeardownUnclean) {
			t.Errorf("StartWatchdog rejected on a latch this test left false: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartWatchdog did not return after the lifecycle mutex was released; the lock is never dropped (deadlock)")
	}
}
