//go:build darwin

package audiomute

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Releaser is the state.Releaser wrapper for the session-audio-unmute step
// in the LIFO cleanup chain. main.go pushes a *Releaser onto
// RestoreState at Step 13.7 (after SetMuted(true) succeeds), placed AFTER
// the focus releaser push so the LIFO unwind releases it BEFORE focus —
// both are independent best-effort silencing steps that must unwind before
// the assertion (slot #3) and runtime-file (slot #5).
//
// Structural clone of focus.Releaser. Two-layer idempotency (mirrors
// powerassert.Assertion + focus.Releaser):
//
//  1. atomic.Bool fast-path: AFTER the first caller has fully completed the
//     unmute and stored released=true, subsequent calls observe
//     released==true without acquiring the mutex.
//  2. sync.Mutex slow-path: concurrent callers entering before the first
//     has finished BLOCK on mu.Lock() until the unmute returns. Under the
//     mutex they double-check released — if true, they return nil without
//     re-invoking SetMuted; if false (genuinely the first caller), they
//     perform the unmute, store released=true, and Unlock.
//
// KEY DEVIATION from the focus clone — the priorMuted gate: Release calls
// SetMuted(ctx, false) ONLY when priorMuted == false (audio was unmuted at
// session start, so we restore that). When priorMuted == true the audio was
// already muted before dndmode touched it, so Release marks itself released
// WITHOUT touching audio — leaving the user's pre-existing mute intact.
//
// Like focus.Releaser, a SetMuted error LOGS A WARN AND RETURNS NIL — never
// propagated to the LIFO unwind (best-effort: an unmute failure must not
// abort the rest of the Cleanup chain; the user's worst case is audio stays
// muted until they toggle it manually).
type Releaser struct {
	runner VolumeRunner
	log    *slog.Logger

	// priorMuted records whether audio was already muted when the session
	// started. true => Release leaves audio muted (no SetMuted call);
	// false => Release restores sound via SetMuted(false).
	priorMuted bool

	// released is the fast-path hint — set to true AFTER the unmute has
	// fully completed under mu. atomic.Load lets repeat callers avoid the
	// mutex entirely once the operation is permanently done.
	released atomic.Bool

	// mu serializes concurrent Release callers. Same rationale as
	// focus.Releaser.mu: a plain Mutex makes every caller block until the
	// winner finishes, avoiding the CAS+sync.Once early-return race.
	mu sync.Mutex

	// timeout bounds SetMuted via a fresh context.WithTimeout. Hardcoded to
	// 5s — osascript usually returns under 1s; the parent ctx is NOT
	// inherited because it is already cancelled by the time defer Cleanup
	// fires (a child subprocess spawned with a cancelled parent ctx is
	// SIGKILL'd before it can run).
	timeout time.Duration
}

// NewReleaser constructs a *Releaser bound to the given runner. priorMuted
// captures the audio state recorded at session start (runtime.json
// prior_muted). Logger fallback: nil → slog.Default() (matches
// focus.NewReleaser and powerassert.Acquire conventions).
func NewReleaser(runner VolumeRunner, priorMuted bool, log *slog.Logger) *Releaser {
	if log == nil {
		log = slog.Default()
	}
	return &Releaser{
		runner:     runner,
		log:        log,
		priorMuted: priorMuted,
		timeout:    5 * time.Second,
	}
}

// Name implements state.Releaser. Returns the constant string "audiomute"
// so the acceptance test (TestAcceptance_LIFE06_PushOrder) can
// parse `released releaser=audiomute` from stderr and verify its slot
// between windows and focus.
func (r *Releaser) Name() string { return "audiomute" }

// Release implements state.Releaser. Two-layer idempotency + best-effort
// semantics (see type doc above). Always returns nil; the error path is
// observable only via the log buffer ("audio unmute failed" at WARN).
func (r *Releaser) Release() error {
	// Fast path: once released is durably set after the winner stored it
	// under mu, any repeat caller skips the mutex.
	if r.released.Load() {
		return nil
	}
	// Slow path: serialize concurrent first-time callers via the mutex.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check under the mutex — another goroutine may have won between
	// our Load and our Lock.
	if r.released.Load() {
		return nil
	}
	// priorMuted gate: only restore sound if audio was unmuted at start.
	// When it was already muted, leave it muted and just mark released.
	if !r.priorMuted {
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		defer cancel()
		if err := r.runner.SetMuted(ctx, false); err != nil {
			// Best-effort: warn + swallow. Cleanup must continue.
			r.log.Warn("audio unmute failed", slog.Any("err", err))
		}
	}
	// Store AFTER the unmute completes (whether success, warn-and-swallow,
	// or skipped because priorMuted). Concurrent callers blocked on mu.Lock
	// will see released=true under mu and short-circuit.
	r.released.Store(true)
	return nil
}

// Compile-time check: *Releaser satisfies state.Releaser without importing
// the state package (would create an import cycle). Mirrors
// focus/releaser.go.
var _ interface {
	Release() error
	Name() string
} = (*Releaser)(nil)
