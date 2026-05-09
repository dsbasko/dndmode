//go:build darwin

package powerassert

import (
	"errors"
	"sync/atomic"
	"testing"
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
