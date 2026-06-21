//go:build darwin

// External test (package audiomute_test) driving *Releaser through the
// gomock-generated MockVolumeRunner — the CONSUMER-facing seam. ExecRunner's
// own stdout-parsing branches are covered by the internal runner_test.go.

package audiomute_test

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

	"go.uber.org/mock/gomock"

	"github.com/dsbasko/dndmode/internal/macos/audiomute"
	"github.com/dsbasko/dndmode/internal/macos/audiomute/mocks"
)

// releaserTestDeps groups the releaser-under-test with its mock runner and a
// captured log buffer so cases can assert both the SetMuted invocations and
// the warn-on-error log line.
type releaserTestDeps struct {
	releaser   *audiomute.Releaser
	mockRunner *mocks.MockVolumeRunner
	logBuf     *bytes.Buffer
}

// newReleaserTestDeps wires a MockVolumeRunner + a Debug-level text logger
// into a *Releaser constructed with the given priorMuted value.
func newReleaserTestDeps(t *testing.T, priorMuted bool, setupMocks func(*mocks.MockVolumeRunner)) *releaserTestDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockRunner := mocks.NewMockVolumeRunner(ctrl)
	if setupMocks != nil {
		setupMocks(mockRunner)
	}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &releaserTestDeps{
		releaser:   audiomute.NewReleaser(mockRunner, priorMuted, log),
		mockRunner: mockRunner,
		logBuf:     logBuf,
	}
}

// TestReleaser_Release_PriorUnmuted_RestoresSound verifies the first Release
// with priorMuted=false calls SetMuted(ctx, false) exactly once.
func TestReleaser_Release_PriorUnmuted_RestoresSound(t *testing.T) {
	t.Parallel()

	var gotMuted atomic.Bool
	gotMuted.Store(true) // sentinel; SetMuted(false) must flip it
	deps := newReleaserTestDeps(t, false, func(m *mocks.MockVolumeRunner) {
		m.EXPECT().SetMuted(gomock.Any(), false).
			DoAndReturn(func(_ context.Context, muted bool) error {
				gotMuted.Store(muted)
				return nil
			}).Times(1)
	})

	if err := deps.releaser.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if gotMuted.Load() {
		t.Errorf("SetMuted called with muted=true, want false (restore sound)")
	}
}

// TestReleaser_Release_PriorMuted_LeavesMuted verifies the priorMuted gate:
// when audio was already muted at start, Release must NOT call SetMuted.
func TestReleaser_Release_PriorMuted_LeavesMuted(t *testing.T) {
	t.Parallel()

	// No SetMuted expectation => gomock fails the test if it is ever called.
	deps := newReleaserTestDeps(t, true, nil)

	if err := deps.releaser.Release(); err != nil {
		t.Errorf("Release: %v (must be nil)", err)
	}
}

// TestReleaser_Release_SecondCall_NoOp verifies two-layer idempotency: a
// second + third Release must return nil WITHOUT re-invoking the runner
// (atomic.Bool fast-path engages).
func TestReleaser_Release_SecondCall_NoOp(t *testing.T) {
	t.Parallel()

	deps := newReleaserTestDeps(t, false, func(m *mocks.MockVolumeRunner) {
		m.EXPECT().SetMuted(gomock.Any(), false).Return(nil).Times(1)
	})

	for i := 1; i <= 3; i++ {
		if err := deps.releaser.Release(); err != nil {
			t.Errorf("Release #%d: %v (must be nil — idempotent)", i, err)
		}
	}
}

// TestReleaser_Release_RunnerError_ReturnsNilWithWarn verifies best-effort
// semantics: when SetMuted fails, Release returns nil AND emits a slog.Warn
// line. The Cleanup chain must never fail because audio could not be unmuted.
func TestReleaser_Release_RunnerError_ReturnsNilWithWarn(t *testing.T) {
	t.Parallel()

	runnerErr := errors.New("osascript set muted false: exit 1")
	deps := newReleaserTestDeps(t, false, func(m *mocks.MockVolumeRunner) {
		m.EXPECT().SetMuted(gomock.Any(), false).Return(runnerErr).Times(1)
	})

	if err := deps.releaser.Release(); err != nil {
		t.Errorf("Release returned %v; want nil (best-effort — must swallow error)", err)
	}
	logs := deps.logBuf.String()
	if !strings.Contains(logs, "audio unmute failed") {
		t.Errorf("log buffer does not contain 'audio unmute failed'; got:\n%s", logs)
	}
	if !strings.Contains(logs, "WARN") {
		t.Errorf("log buffer does not contain WARN level marker; got:\n%s", logs)
	}
}

// TestReleaser_Release_ConcurrentCallers_SerializeViaMutex verifies the
// idempotency contract under concurrency: 10 goroutines invoking Release()
// concurrently produce exactly 1 SetMuted invocation, and ALL callers return
// AFTER the slow runner finishes (sync.Mutex serialization).
func TestReleaser_Release_ConcurrentCallers_SerializeViaMutex(t *testing.T) {
	t.Parallel()
	const numGoroutines = 10
	const iterations = 20
	const slowD = 5 * time.Millisecond

	for iter := 0; iter < iterations; iter++ {
		var calls atomic.Int64
		var releaseFinishedAt atomic.Int64
		deps := newReleaserTestDeps(t, false, func(m *mocks.MockVolumeRunner) {
			m.EXPECT().SetMuted(gomock.Any(), false).
				DoAndReturn(func(_ context.Context, _ bool) error {
					calls.Add(1)
					time.Sleep(slowD)
					releaseFinishedAt.Store(time.Now().UnixNano())
					return nil
				}).Times(1)
		})

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
				results[i] = deps.releaser.Release()
				returnedAt[i] = time.Now().UnixNano()
			}()
		}
		start.Done()
		done.Wait()

		if got := calls.Load(); got != 1 {
			t.Fatalf("iter=%d: SetMuted calls = %d, want 1 (mutex must serialize)", iter, got)
		}
		for i, r := range results {
			if r != nil {
				t.Errorf("iter=%d: caller %d returned err = %v, want nil", iter, i, r)
			}
		}
		finishedAt := releaseFinishedAt.Load()
		if finishedAt == 0 {
			t.Fatalf("iter=%d: SetMuted never reached the timestamp store (impossible if calls==1)", iter)
		}
		for i, rt := range returnedAt {
			if rt < finishedAt {
				t.Errorf("iter=%d: caller %d returned BEFORE SetMuted finished (delta=%dns) — mutex must block",
					iter, i, finishedAt-rt)
			}
		}
	}
}

// TestReleaser_Name_ReturnsAudiomute verifies the acceptance-test
// contract: Name() must be the literal string "audiomute".
func TestReleaser_Name_ReturnsAudiomute(t *testing.T) {
	t.Parallel()
	deps := newReleaserTestDeps(t, true, nil)
	if got := deps.releaser.Name(); got != "audiomute" {
		t.Errorf("Name() = %q, want %q", got, "audiomute")
	}
}

// TestReleaser_NewReleaserNilLogger_UsesDefault verifies NewReleaser(runner,
// _, nil) does not panic — the nil logger falls back to slog.Default().
func TestReleaser_NewReleaserNilLogger_UsesDefault(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockRunner := mocks.NewMockVolumeRunner(ctrl)
	mockRunner.EXPECT().SetMuted(gomock.Any(), false).
		Return(errors.New("synthetic err to exercise warn path with default logger")).Times(1)
	r := audiomute.NewReleaser(mockRunner, false, nil)

	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("Release panicked with nil logger: %v", rec)
		}
	}()
	if err := r.Release(); err != nil {
		t.Errorf("Release returned %v; want nil", err)
	}
}
