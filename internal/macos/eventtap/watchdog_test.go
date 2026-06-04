//go:build darwin

package eventtap

import (
	"testing"
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
		name       string
		setupMocks func(d *testDeps)
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
				for i := 0; i < 3; i++ {
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
				for i := 0; i < 4; i++ {
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
		tt := tt
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
	for i := 0; i < 4; i++ {
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
	for i := 0; i < 3; i++ {
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
// fires before cancellation propagates we MUST NOT re-emit
// `ErrWatchdogExitThreshold` to the sink channel — that would queue a
// second supervisor exit and confuse the LIFE-07 unwind.
func TestWatchdog_Threshold_Idempotent(t *testing.T) {
	t.Parallel()

	d := newTestDeps(t)

	// Trip the threshold.
	for i := 0; i < 4; i++ {
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
