//go:build darwin

package state

import (
	"errors"
	"log/slog"
	"sync"
)

// RestoreState collects Releasers in the order they were acquired and runs
// them in reverse (LIFO) on Cleanup. Cleanup is double-layer idempotent
//:
//   - sync.Once wraps the body (no second Release-pass on repeat call).
//   - each Releaser is itself idempotent (e.g. MockReleaser uses atomic.Bool).
//
// Push is safe to call concurrently with Cleanup; if Cleanup has already
// started, the late Releaser is appended but won't be released (since the
// sync.Once body has already taken its snapshot and exited). Caller is
// responsible for invariant: only Push AFTER successful resource acquisition.
type RestoreState struct {
	mu        sync.Mutex
	releasers []Releaser
	once      sync.Once
	log       *slog.Logger
}

// NewRestoreState returns a RestoreState with the given logger. If log is
// nil, slog.Default() is used. Logger writes cleanup-step errors with the
// releaser name.
func NewRestoreState(log *slog.Logger) *RestoreState {
	if log == nil {
		log = slog.Default()
	}
	return &RestoreState{log: log}
}

// Push appends r to the LIFO release stack.
func (rs *RestoreState) Push(r Releaser) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.releasers = append(rs.releasers, r)
}

// Len returns the current number of pushed Releasers (for debug/test).
func (rs *RestoreState) Len() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.releasers)
}

// Cleanup runs all pushed Releasers in reverse order (LIFO). It
// NEVER short-circuits on error — every Releaser is invoked even if earlier
// ones fail. Errors are logged via slog.Error and aggregated through
// errors.Join. Cleanup is safe to call from multiple goroutines and from
// within recover(); the second call is a no-op (returns nil) thanks to
// sync.Once.
func (rs *RestoreState) Cleanup() error {
	var aggregated error
	rs.once.Do(func() {
		// Snapshot under lock; iterate without holding it (Releasers may
		// take non-trivial time and we don't want to block concurrent Push
		// from late goroutines that need to fail-fast).
		rs.mu.Lock()
		snap := make([]Releaser, len(rs.releasers))
		copy(snap, rs.releasers)
		rs.mu.Unlock()

		for i := len(snap) - 1; i >= 0; i-- {
			r := snap[i]
			if err := r.Release(); err != nil {
				rs.log.Error("cleanup step failed",
					slog.String("releaser", r.Name()),
					slog.Any("err", err))
				aggregated = errors.Join(aggregated, err)
			}
		}
	})
	return aggregated
}
