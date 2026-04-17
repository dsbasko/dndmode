//go:build darwin

package state_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/dsbasko/dndmode/internal/state"
	"github.com/dsbasko/dndmode/internal/state/mocks"
	"go.uber.org/mock/gomock"
)

type testDeps struct {
	rs     *state.RestoreState
	logger *slog.Logger
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &testDeps{
		rs:     state.NewRestoreState(logger),
		logger: logger,
	}
}

// recordingReleaser wraps MockReleaser to record release order across
// multiple Releasers (for LIFO test).
type recordingReleaser struct {
	*state.MockReleaser
	order *[]string
	mu    *sync.Mutex
}

func newRecordingReleaser(name string, order *[]string, mu *sync.Mutex) *recordingReleaser {
	return &recordingReleaser{
		MockReleaser: state.NewMockReleaser(name),
		order:        order,
		mu:           mu,
	}
}

func (r *recordingReleaser) Release() error {
	r.mu.Lock()
	*r.order = append(*r.order, r.Name())
	r.mu.Unlock()
	return r.MockReleaser.Release()
}

func TestRestoreState_Cleanup_LIFO(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func(td *testDeps) (releaseOrder *[]string, mu *sync.Mutex)
		validateResp func(t *testing.T, releaseOrder []string)
	}{
		{
			name: "three releasers run in reverse-push order",
			setupMocks: func(td *testDeps) (*[]string, *sync.Mutex) {
				order := []string{}
				mu := &sync.Mutex{}
				td.rs.Push(newRecordingReleaser("r1-pushed-first", &order, mu))
				td.rs.Push(newRecordingReleaser("r2", &order, mu))
				td.rs.Push(newRecordingReleaser("r3-pushed-last", &order, mu))
				return &order, mu
			},
			validateResp: func(t *testing.T, releaseOrder []string) {
				want := []string{"r3-pushed-last", "r2", "r1-pushed-first"}
				if len(releaseOrder) != len(want) {
					t.Fatalf("got %d release calls, want %d: %v", len(releaseOrder), len(want), releaseOrder)
				}
				for i, name := range want {
					if releaseOrder[i] != name {
						t.Errorf("release[%d] = %q, want %q (LIFO violated)", i, releaseOrder[i], name)
					}
				}
			},
		},
		{
			name: "empty stack — Cleanup is no-op",
			setupMocks: func(td *testDeps) (*[]string, *sync.Mutex) {
				order := []string{}
				return &order, &sync.Mutex{}
			},
			validateResp: func(t *testing.T, releaseOrder []string) {
				if len(releaseOrder) != 0 {
					t.Errorf("empty stack triggered %d releases: %v", len(releaseOrder), releaseOrder)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			orderPtr, _ := tt.setupMocks(td)
			if err := td.rs.Cleanup(); err != nil {
				t.Fatalf("Cleanup returned error: %v", err)
			}
			tt.validateResp(t, *orderPtr)
		})
	}
}

func TestRestoreState_Cleanup_AggregatesErrors(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func(td *testDeps) (e1, e2 error)
		validateResp func(t *testing.T, gotErr error, e1, e2 error)
	}{
		{
			name: "two failing releasers + one ok → both errors in joined",
			setupMocks: func(td *testDeps) (error, error) {
				e1 := errors.New("err-from-r1")
				e2 := errors.New("err-from-r3")
				td.rs.Push(state.NewMockReleaserWithError("r1", e1))
				td.rs.Push(state.NewMockReleaser("r2-ok"))
				td.rs.Push(state.NewMockReleaserWithError("r3", e2))
				return e1, e2
			},
			validateResp: func(t *testing.T, gotErr error, e1, e2 error) {
				if gotErr == nil {
					t.Fatal("expected aggregated error, got nil")
				}
				if !errors.Is(gotErr, e1) {
					t.Errorf("errors.Is(joined, e1) = false, want true")
				}
				if !errors.Is(gotErr, e2) {
					t.Errorf("errors.Is(joined, e2) = false, want true")
				}
				// Both messages should be embedded
				if !strings.Contains(gotErr.Error(), "err-from-r1") {
					t.Errorf("joined error missing 'err-from-r1': %v", gotErr)
				}
				if !strings.Contains(gotErr.Error(), "err-from-r3") {
					t.Errorf("joined error missing 'err-from-r3': %v", gotErr)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			e1, e2 := tt.setupMocks(td)
			err := td.rs.Cleanup()
			tt.validateResp(t, err, e1, e2)
		})
	}
}

func TestRestoreState_Cleanup_Idempotent(t *testing.T) {
	td := newTestDeps(t)
	mr := state.NewMockReleaser("only")
	td.rs.Push(mr)

	// Call Cleanup twice in succession.
	if err := td.rs.Cleanup(); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := td.rs.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}

	// MockReleaser.Calls() reports total Release() invocations. Cleanup
	// should NOT have re-invoked Release on second call (sync.Once).
	if mr.Calls() != 1 {
		t.Errorf("Release called %d times after double Cleanup, want 1 (violated)", mr.Calls())
	}
	if !mr.Done() {
		t.Error("MockReleaser.Done() = false after Cleanup, want true")
	}
}

func TestRestoreState_Cleanup_ConcurrentSafe(t *testing.T) {
	td := newTestDeps(t)
	mr := state.NewMockReleaser("concurrent")
	td.rs.Push(mr)

	const N = 10
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = td.rs.Cleanup()
		}()
	}
	wg.Wait()

	if mr.Calls() != 1 {
		t.Errorf("Release called %d times under %d concurrent Cleanup, want 1 (sync.Once violated)", mr.Calls(), N)
	}
}

func TestRestoreState_Push_AfterCleanup_LateReleaserNotInvoked(t *testing.T) {
	td := newTestDeps(t)
	early := state.NewMockReleaser("early")
	td.rs.Push(early)

	if err := td.rs.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	late := state.NewMockReleaser("late-pushed-after-cleanup")
	td.rs.Push(late)

	// Second Cleanup is a no-op (sync.Once already fired); late Releaser
	// must NOT be invoked.
	if err := td.rs.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}

	if late.Calls() != 0 {
		t.Errorf("late Releaser invoked %d times, want 0 (sync.Once contract)", late.Calls())
	}
	if early.Calls() != 1 {
		t.Errorf("early Releaser invoked %d times, want 1", early.Calls())
	}
}

func TestRestoreState_Cleanup_WithGomockReleaser_VerifiesExactCallCount(t *testing.T) {
	// Demonstrates gomock-generated mock usage for tests requiring
	// EXPECT() / Times() / InOrder() semantics that hand-written
	// MockReleaser doesn't support.
	ctrl := gomock.NewController(t)
	mock := mocks.NewMockReleaser(ctrl)

	// Expect Release() called exactly once, Name() may be called any time.
	mock.EXPECT().Release().Return(nil).Times(1)
	mock.EXPECT().Name().Return("gomock-releaser").AnyTimes()

	td := newTestDeps(t)
	td.rs.Push(mock)

	if err := td.rs.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Second Cleanup must NOT trigger a second Release() (sync.Once).
	// gomock will fail the test if Times(1) is exceeded.
	if err := td.rs.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}
