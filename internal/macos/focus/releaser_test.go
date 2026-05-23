//go:build darwin

package focus_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/macos/focus"
)

// TestReleaser_Release_FirstCall_InvokesRunner verifies that the first
// invocation of Release calls runner.Run(ctx, "dndmode-off") exactly
// once. Mirrors TestAssertion_Release_FirstCall_InvokesReleaser.
//
// Validation map ID: 5-03-03.
func TestReleaser_Release_FirstCall_InvokesRunner(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	var lastName atomic.Value
	fr := &fakeRunner{
		runFn: func(_ context.Context, name string) error {
			calls.Add(1)
			lastName.Store(name)
			return nil
		},
	}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := focus.NewReleaser(fr, log)

	if err := r.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if got, _ := lastName.Load().(string); got != "dndmode-off" {
		t.Errorf("runner.Run called with name=%q, want %q", got, "dndmode-off")
	}
}

// TestReleaser_Release_SecondCall_NoOp verifies two-layer idempotency:
// second + third Release() must return nil WITHOUT re-invoking the
// runner (atomic.Bool fast-path engages).
//
// Validation map ID: 5-03-04.
func TestReleaser_Release_SecondCall_NoOp(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	fr := &fakeRunner{
		runFn: func(_ context.Context, _ string) error {
			calls.Add(1)
			return nil
		},
	}
	r := focus.NewReleaser(fr, nil) // exercise nil-logger fallback to slog.Default()

	if err := r.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("after 1st Release, calls = %d, want 1", got)
	}
	if err := r.Release(); err != nil {
		t.Errorf("second Release: %v (must be nil — idempotent no-op)", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("after 2nd Release, calls = %d, want 1 (atomic.Bool fast-path must engage)", got)
	}
	if err := r.Release(); err != nil {
		t.Errorf("third Release: %v (must be nil — idempotent no-op)", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("after 3rd Release, calls = %d, want 1 (gate must stay closed)", got)
	}
}

// TestReleaser_Release_RunnerError_ReturnsNilWithWarn — KEY DEVIATION
// from powerassert.Assertion: when runner.Run fails, Releaser.Release
// returns nil AND emits a slog.Warn line. the design notes best-effort
// the Cleanup chain must never fail because Focus failed to deactivate
// (kernel auto-cleanups assertion; Focus icon stays visible until next
// reboot at worst — non-fatal UX).
func TestReleaser_Release_RunnerError_ReturnsNilWithWarn(t *testing.T) {
	t.Parallel()

	runnerErr := errors.New("shortcuts run dndmode-off: exit 1")
	fr := &fakeRunner{
		runFn: func(_ context.Context, _ string) error {
			return runnerErr
		},
	}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := focus.NewReleaser(fr, log)

	if err := r.Release(); err != nil {
		t.Errorf("Release returned %v; want nil (best-effort — must swallow error)", err)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "focus deactivate failed") {
		t.Errorf("log buffer does not contain 'focus deactivate failed'; got:\n%s", logs)
	}
	if !strings.Contains(logs, "WARN") {
		t.Errorf("log buffer does not contain WARN level marker; got:\n%s", logs)
	}
}

// TestReleaser_Release_ConcurrentCallers_SerializeViaMutex verifies the
// idempotency contract: 10 goroutines invoking r.Release()
// concurrently produce exactly 1 runner.Run invocation, and ALL callers
// return AFTER the slow-runner finishes (sync.Mutex serialization).
//
// Deviation from powerassert.Assertion: ALL callers return nil (not
// just the non-winners) because Releaser.Release swallows the error
// per. The serialization invariant remains: every returnedAt[i]
// >= releaseFinishedAt.
//
// Anti-flake: 20 iterations × 10 goroutines = 200 race opportunities
// per run; WaitGroup start-barrier + 5ms slowD makes the race window
// wide enough to catch any early-return bug.
//
// Validation map ID: 5-03-04 (concurrency dimension).
func TestReleaser_Release_ConcurrentCallers_SerializeViaMutex(t *testing.T) {
	t.Parallel()
	const numGoroutines = 10
	const iterations = 20
	const slowD = 5 * time.Millisecond

	for iter := 0; iter < iterations; iter++ {
		var calls atomic.Int64
		var releaseFinishedAt atomic.Int64
		fr := &fakeRunner{
			runFn: func(_ context.Context, _ string) error {
				calls.Add(1)
				time.Sleep(slowD)
				releaseFinishedAt.Store(time.Now().UnixNano())
				return nil
			},
		}
		r := focus.NewReleaser(fr, nil)

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
				results[i] = r.Release()
				returnedAt[i] = time.Now().UnixNano()
			}()
		}
		start.Done()
		done.Wait()

		// Exactly one runner.Run invocation — mutex must serialize.
		if got := calls.Load(); got != 1 {
			t.Fatalf("iter=%d: runner calls = %d, want 1 (mutex must serialize concurrent callers)",
				iter, got)
		}

		// ALL callers must return nil (deviation from Assertion — swallow).
		for i, r := range results {
			if r != nil {
				t.Errorf("iter=%d: caller %d returned err = %v, want nil (best-effort)",
					iter, i, r)
			}
		}

		// SERIALIZATION invariant: every caller returned AFTER runner.Run finished.
		finishedAt := releaseFinishedAt.Load()
		if finishedAt == 0 {
			t.Fatalf("iter=%d: runner.Run never reached the timestamp store (impossible if calls==1)", iter)
		}
		for i, rt := range returnedAt {
			if rt < finishedAt {
				t.Errorf("iter=%d: caller %d returned %dns BEFORE runner.Run finished (delta=%dns) — mutex must block until runner.Run completes",
					iter, i, finishedAt-rt, finishedAt-rt)
			}
		}
	}
}

// TestReleaser_Name_ReturnsFocus verifies the acceptance-test
// contract: the Releaser's Name() must be the literal string "focus".
// 's acceptance test parses stderr lines like
// `released releaser=focus` to verify push order between assertion
// (slot #3) and runtime-file (slot #5).
func TestReleaser_Name_ReturnsFocus(t *testing.T) {
	t.Parallel()
	r := focus.NewReleaser(&fakeRunner{}, nil)
	if got := r.Name(); got != "focus" {
		t.Errorf("Name() = %q, want %q", got, "focus")
	}
}

// TestReleaser_Release_NewReleaserNilLogger_UsesDefault verifies that
// NewReleaser(runner, nil) does not panic — the nil logger falls back
// to slog.Default() (mirrors powerassert.Acquire and state.NewRestoreState
// conventions from Phase 1/2/3).
func TestReleaser_Release_NewReleaserNilLogger_UsesDefault(t *testing.T) {
	t.Parallel()
	fr := &fakeRunner{
		runFn: func(_ context.Context, _ string) error {
			return errors.New("synthetic err to exercise warn path with default logger")
		},
	}
	r := focus.NewReleaser(fr, nil)
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("Release panicked with nil logger: %v", rec)
		}
	}()
	if err := r.Release(); err != nil {
		t.Errorf("Release returned %v; want nil", err)
	}
}
