//go:build darwin

package powerassert

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReleaser is a test-injectable releaseFn used to assert idempotency
// without invoking the real IOKit IOPMAssertionRelease syscall. calls is
// atomic so the same fixture can be reused in goroutine-fanout tests
// (though current tests are single-goroutine).
type fakeReleaser struct {
	calls atomic.Int64
	err   error
}

func (f *fakeReleaser) Release(id uint32) error {
	f.calls.Add(1)
	return f.err
}

// TestAssertion_Release_FirstCall_InvokesReleaser verifies the
// production path: first Release() must reach the injected releaseFn
// exactly once and flip the released gate.
func TestAssertion_Release_FirstCall_InvokesReleaser(t *testing.T) {
	t.Parallel()

	fake := &fakeReleaser{}
	a := newAssertionWithDeps(12345, "dndmode active", nil, fake.Release)

	if err := a.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("fake.calls = %d, want 1", got)
	}
	if !a.released.Load() {
		t.Errorf("a.released = false, want true after Release")
	}
}

// TestAssertion_Release_SecondCall_NoOp verifies two-layer idempotency:
// second + third Release() must return nil WITHOUT invoking the underlying
// releaseFn (atomic.Bool CAS gate engages on call #2). Mirrors
// controller_test.go:TestController_Release_Idempotent shape.
func TestAssertion_Release_SecondCall_NoOp(t *testing.T) {
	t.Parallel()

	fake := &fakeReleaser{}
	a := newAssertionWithDeps(0xDEAD, "dndmode active", nil, fake.Release)

	if err := a.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("after 1st Release, fake.calls = %d, want 1", got)
	}
	if err := a.Release(); err != nil {
		t.Errorf("second Release: %v (must be nil — idempotent no-op)", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("after 2nd Release, fake.calls = %d, want 1 (atomic.Bool gate must block)", got)
	}
	if err := a.Release(); err != nil {
		t.Errorf("third Release: %v (must be nil — idempotent no-op)", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("after 3rd Release, fake.calls = %d, want 1 (gate must stay closed)", got)
	}
}

// TestAssertion_Release_PropagatesError verifies that the FIRST call
// surfaces the underlying releaseFn error and that SUBSEQUENT calls
// return nil (idempotency applies to the error path too — we don't
// re-raise on second Release).
func TestAssertion_Release_PropagatesError(t *testing.T) {
	t.Parallel()

	want := errors.New("simulated rc=0xdeadbeef")
	fake := &fakeReleaser{err: want}
	a := newAssertionWithDeps(0xCAFE, "dndmode active", nil, fake.Release)

	if err := a.Release(); !errors.Is(err, want) {
		t.Errorf("first Release err = %v, want %v", err, want)
	}
	if err := a.Release(); err != nil {
		t.Errorf("second Release err = %v, want nil (idempotent — error NOT re-raised)", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("fake.calls = %d, want 1 (atomic.Bool gate must block re-invoke)", got)
	}
}

// TestAssertion_Release_ConcurrentCallers_SerializeViaMutex verifies
// fix: 10 goroutines invoking a.Release() concurrently must produce exactly
// 1 releaseFn invocation, and exactly 1 non-nil error return (the first
// caller — the one that won the mutex — gets the real err from releaseFn;
// the other 9 are serialized via sync.Mutex and find released=true under
// the mutex, returning nil without invoking releaseFn).
//
// Pre-fix (CAS-based + sync.Once) behavior was racy:
//   - First caller did CAS(false→true), then *started* releaseFn.
//   - Second caller saw released==true, returned nil IMMEDIATELY — before
//     the first caller had finished releaseFn.
//   - Result: caller #2 might use the released resource (assume it's gone)
//     while caller #1 is still in IOPMAssertionRelease — a use-after-free
//     window in the Cleanup chain.
//
// Two assertions in this test:
//  1. invocation/err invariants (passes on both pre-fix and post-fix):
//     exactly 1 releaseFn call, exactly 1 non-nil err.
//  2. SERIALIZATION invariant (FAILS on pre-fix CAS, passes on mutex):
//     among concurrent callers, NONE returns before releaseFn completes.
//     We measure this via a slow releaseFn (blocks for slowD) and check
//     that every return time is >= releaseFnStart + slowD.
//
// Anti-flake: 20 iterations × 10 goroutines = 200 race opportunities per
// run. WaitGroup start-barrier + a 5ms artificial releaseFn delay ensures
// the race window is wide enough to catch any early-return bug.
func TestAssertion_Release_ConcurrentCallers_SerializeViaMutex(t *testing.T) {
	t.Parallel()
	const numGoroutines = 10
	const iterations = 20
	const slowD = 5 * time.Millisecond
	sentinelErr := errors.New("simulated release rc=0xfeedface")

	for iter := 0; iter < iterations; iter++ {
		fake := &fakeReleaser{err: sentinelErr}
		// Slow releaseFn — sleeps slowD then returns sentinelErr; captures
		// the actual finish time so the test can assert every concurrent
		// caller returned AFTER the slow releaseFn completed.
		var releaseFinishedAt atomic.Int64 // unix nanos
		slowRelease := func(id uint32) error {
			time.Sleep(slowD)
			err := fake.Release(id)
			releaseFinishedAt.Store(time.Now().UnixNano())
			return err
		}
		a := newAssertionWithDeps(0xBEEF, "dndmode active", nil, slowRelease)

		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		done.Add(numGoroutines)
		results := make([]error, numGoroutines)
		returnedAt := make([]int64, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			i := i
			go func() {
				defer done.Done()
				start.Wait()
				results[i] = a.Release()
				returnedAt[i] = time.Now().UnixNano()
			}()
		}
		start.Done()
		done.Wait()

		// Exactly one releaseFn invocation — mutex must serialize.
		if got := fake.calls.Load(); got != 1 {
			t.Fatalf("iter=%d: fake.calls = %d, want 1 (mutex must serialize concurrent callers)", iter, got)
		}

		// Exactly one non-nil err return (first caller through the mutex).
		nonNilCount := 0
		for _, r := range results {
			if r != nil {
				if !errors.Is(r, sentinelErr) {
					t.Errorf("iter=%d: unexpected err = %v, want %v", iter, r, sentinelErr)
				}
				nonNilCount++
			}
		}
		if nonNilCount != 1 {
			t.Errorf("iter=%d: non-nil errs = %d, want exactly 1 (first caller gets err, rest get nil)", iter, nonNilCount)
		}

		// SERIALIZATION invariant: every caller returned AFTER releaseFn
		// finished. Pre-fix CAS code violates this — caller #2 saw
		// released=true and returned nil INSTANTLY while caller #1 was
		// still in time.Sleep(slowD).
		finishedAt := releaseFinishedAt.Load()
		if finishedAt == 0 {
			t.Fatalf("iter=%d: releaseFn never reached the timestamp store (impossible if fake.calls==1)", iter)
		}
		for i, rt := range returnedAt {
			if rt < finishedAt {
				t.Errorf("iter=%d: caller %d returned %dns BEFORE releaseFn finished (delta=%dns) — mutex must block until releaseFn completes",
					iter, i, finishedAt-rt, finishedAt-rt)
			}
		}
	}
}

// TestAssertion_Name_ReturnsConstructorName verifies Name() returns the
// exact string passed at construction time — acceptance test
// parses stderr "released releaser=<name>" so this string MUST
// be stable.
func TestAssertion_Name_ReturnsConstructorName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want string
	}{
		{name: "production name", want: "dndmode active"},
		{name: "test fixture name", want: "test-foo"},
		{name: "empty string", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAssertionWithDeps(0, tc.want, nil, nil)
			if got := a.Name(); got != tc.want {
				t.Errorf("Name() = %q, want %q", got, tc.want)
			}
		})
	}
}
