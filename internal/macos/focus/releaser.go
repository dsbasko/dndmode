//go:build darwin

package focus

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Releaser is the state.Releaser wrapper for the Focus / DND deactivate
// step in the LIFO cleanup chain. main.go pushes a
// *Releaser onto RestoreState at Step 13.7 after focus.Activate succeeds;
// the LIFO unwind releases it 4th (between assertion=#3 and
// runtime-file=#5).
//
// Two-layer idempotency (mirrors powerassert.Assertion post-, see
// internal/macos/powerassert/assertion.go lines 42-65):
//
//  1. atomic.Bool fast-path: AFTER the first caller has fully completed
//     runner.Run and stored released=true, subsequent calls observe
//     released==true without acquiring the mutex.
//  2. sync.Mutex slow-path: concurrent callers entering before the first
//     has finished BLOCK on mu.Lock() until runner.Run returns. Under
//     the mutex they double-check released — if true, they return nil
//     without invoking runner.Run; if false (genuinely the first caller),
//     they invoke runner.Run, store released=true, and Unlock.
//
// KEY DEVIATION from powerassert.Assertion.Release: when runner.Run
// returns an error, Releaser.Release LOGS A WARN AND RETURNS NIL — it
// does NOT propagate the error to the LIFO unwind. the design notes
// best-effort: a Focus deactivation failure must NEVER abort
// the rest of the Cleanup chain (assertion release, runtime-file
// removal, etc.). Kernel auto-cleanup handles the IOPM side; the
// user's worst case is that the Focus icon stays in the menu bar
// until next reboot — non-fatal UX tradeoff.
type Releaser struct {
	runner ShortcutsRunner
	log    *slog.Logger

	// released is the fast-path hint — set to true AFTER runner.Run has
	// fully completed under mu. atomic.Load lets repeat callers avoid
	// the mutex entirely once the operation is permanently done.
	released atomic.Bool

	// mu serializes concurrent Release callers. Same rationale as
	// powerassert.Assertion.mu: the CAS+sync.Once pattern has a race
	// where caller #2 observes released=true while caller #1 is still
	// inside the slow runner.Run subprocess. A plain Mutex avoids this
	// by making every caller block until the winner finishes.
	mu sync.Mutex

	// timeout is the bound applied to runner.Run via a fresh
	// context.WithTimeout. Hardcoded to 5s — Apple's shortcuts CLI
	// usually returns under 1s; 5s gives generous headroom while
	// keeping process-exit-blocked-by-Cleanup bounded.
	timeout time.Duration
}

// NewReleaser constructs a *Releaser bound to the given runner. Logger
// fallback: nil → slog.Default() (matches powerassert.Acquire and
// state.NewRestoreState conventions). Timeout is fixed at 5s — the
// caller's ctx is NOT inherited because main.go's signal-context is
// already cancelled by the time defer Cleanup fires (a child subprocess
// spawned with a cancelled parent ctx is SIGKILL'd before it can run).
func NewReleaser(runner ShortcutsRunner, log *slog.Logger) *Releaser {
	if log == nil {
		log = slog.Default()
	}
	return &Releaser{
		runner:  runner,
		log:     log,
		timeout: 5 * time.Second,
	}
}

// Name implements state.Releaser. Returns the constant string "focus"
// so 's acceptance test (TestAcceptance_LIFE06_PushOrder) can
// parse `released releaser=focus` from stderr and verify the slot-#4
// position between assertion ("dndmode active") and runtime-file
// ("runtime-file").
func (r *Releaser) Name() string { return "focus" }

// Release implements state.Releaser. Two-layer idempotency
// best-effort semantics (see type doc above).
//
// Notable deviations from powerassert.Assertion.Release:
//   - Builds a FRESH ctx via context.WithTimeout — the caller's ctx is
//     already cancelled at defer Cleanup time, so inheriting it would
//     SIGKILL the shortcuts subprocess before it can run.
//   - SWALLOWS the runner.Run error after logging a warn, instead of
//     propagating it. Cleanup chain never aborts on Focus failure.
//
// Always returns nil. The error path is observable only via the log
// buffer ("focus deactivate failed" entry at WARN level).
func (r *Releaser) Release() error {
	// Fast path: hint flag. Once released is durably set after the
	// winner stored it under mu, any repeat caller skips the mutex.
	if r.released.Load() {
		return nil
	}
	// Slow path: serialize concurrent first-time callers via the mutex.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check under the mutex — another goroutine may have won
	// between our Load and our Lock.
	if r.released.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.runner.Run(ctx, "dndmode-off"); err != nil {
		//: warn + swallow. Cleanup must continue.
		r.log.Warn("focus deactivate failed", slog.Any("err", err))
	}
	// Store AFTER runner.Run completes (whether success or warn-and-
	// swallow). Concurrent callers blocked on mu.Lock will see
	// released=true under mu and short-circuit; new callers using the
	// fast-path Load see the same after the Unlock has happens-before
	// published the Store.
	r.released.Store(true)
	return nil
}

// Compile-time check: *Releaser satisfies state.Releaser without
// importing the state package (would create an import cycle —
// cmd/dndmode/main.go is the only caller that holds *Releaser as
// state.Releaser). Mismatch surfaces here at build time. Mirrors
// powerassert/assertion.go lines 187-190 verbatim.
var _ interface {
	Release() error
	Name() string
} = (*Releaser)(nil)
