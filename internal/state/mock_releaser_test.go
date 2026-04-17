//go:build darwin

package state_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dsbasko/dndmode/internal/state"
)

func TestMockReleaser_Release_AtomicIdempotency(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func() *state.MockReleaser
		validateResp func(t *testing.T, mr *state.MockReleaser)
	}{
		{
			name: "100 concurrent Release calls — error returned exactly once, Calls()=100, Done()=true",
			setupMocks: func() *state.MockReleaser {
				return state.NewMockReleaserWithError("ctr", errors.New("sentinel"))
			},
			validateResp: func(t *testing.T, mr *state.MockReleaser) {
				const N = 100
				var wg sync.WaitGroup
				errCount := atomic.Int64{}
				for i := 0; i < N; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if err := mr.Release(); err != nil {
							errCount.Add(1)
						}
					}()
				}
				wg.Wait()

				if mr.Calls() != int64(N) {
					t.Errorf("Calls() = %d, want %d", mr.Calls(), N)
				}
				if !mr.Done() {
					t.Error("Done() = false after concurrent Release, want true")
				}
				// Exactly one call should have produced the sentinel error
				// (the winner of CompareAndSwap on `done`); rest are no-ops.
				if errCount.Load() != 1 {
					t.Errorf("error returned %d times, want exactly 1 (idempotency violated)", errCount.Load())
				}
			},
		},
		{
			name: "Release on default MockReleaser returns nil and Calls() counts every invocation",
			setupMocks: func() *state.MockReleaser {
				return state.NewMockReleaser("plain")
			},
			validateResp: func(t *testing.T, mr *state.MockReleaser) {
				if err := mr.Release(); err != nil {
					t.Errorf("first Release returned %v, want nil", err)
				}
				if err := mr.Release(); err != nil {
					t.Errorf("second Release returned %v, want nil", err)
				}
				if mr.Calls() != 2 {
					t.Errorf("Calls() = %d, want 2", mr.Calls())
				}
				if !mr.Done() {
					t.Error("Done() = false after Release, want true")
				}
			},
		},
		{
			name: "Name returns constructor argument verbatim",
			setupMocks: func() *state.MockReleaser {
				return state.NewMockReleaser("my-named-releaser")
			},
			validateResp: func(t *testing.T, mr *state.MockReleaser) {
				if mr.Name() != "my-named-releaser" {
					t.Errorf("Name() = %q, want %q", mr.Name(), "my-named-releaser")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := tt.setupMocks()
			tt.validateResp(t, mr)
		})
	}
}
