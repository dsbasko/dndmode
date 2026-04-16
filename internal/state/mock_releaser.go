//go:build darwin

package state

import "sync/atomic"

// MockReleaser is a hand-written test fixture and the Phase 1 main.go demo
// Releaser. It is idempotent via atomic.Bool and records call count for
// test assertions.
//
// For tests requiring EXPECT() / Times() / InOrder() semantics, use the
// gomock-generated mock in internal/state/mocks instead.
//
// MockReleaser is intentionally exported (not in test_test.go) because
// cmd/dndmode/main.go pushes 4 instances into RestoreState as Phase 1
// demo Releasers — they imitate the Phase 2-5 push order without any
// real system effects.
type MockReleaser struct {
	name      string
	done      atomic.Bool
	calls     atomic.Int64
	releaseFn func() error // optional override for failure-injection tests
}

// NewMockReleaser creates a MockReleaser with the given name. Release()
// returns nil on first call and is a no-op on subsequent calls.
func NewMockReleaser(name string) *MockReleaser {
	return &MockReleaser{name: name}
}

// NewMockReleaserWithError creates a MockReleaser whose first Release()
// call returns the provided error. Subsequent calls return nil (idempotency
// applies to side effects, not errors — but the second call is a no-op).
func NewMockReleaserWithError(name string, err error) *MockReleaser {
	return &MockReleaser{
		name:      name,
		releaseFn: func() error { return err },
	}
}

// Name implements Releaser.
func (m *MockReleaser) Name() string { return m.name }

// Release implements Releaser. Atomic compare-and-swap on `done` ensures
// exactly-once side-effect semantics under concurrent invocation.
func (m *MockReleaser) Release() error {
	m.calls.Add(1)
	if !m.done.CompareAndSwap(false, true) {
		return nil
	}
	if m.releaseFn != nil {
		return m.releaseFn()
	}
	return nil
}

// Calls returns the total number of times Release() was invoked across all
// goroutines (atomic — safe to call concurrently).
func (m *MockReleaser) Calls() int64 { return m.calls.Load() }

// Done reports whether Release() has completed at least once (atomic).
func (m *MockReleaser) Done() bool { return m.done.Load() }
