//go:build darwin

package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	focusmocks "github.com/dsbasko/dndmode/internal/macos/focus/mocks"
	passertmocks "github.com/dsbasko/dndmode/internal/macos/powerassert/mocks"
	"github.com/dsbasko/dndmode/internal/state/runtime"
)

// recoveryDeps groups the gomock controller, three DI mocks (cross-
// package — powerassert/mocks + focus/mocks), a REAL *runtime.Manager
// bound to t.TempDir() (the design notes no afero), and a captured slog
// buffer for log-line assertions.
//
// Named recoveryDeps (not testDeps) to avoid collision with the
// testDeps struct already declared in manager_test.go for the
// Write/Read/Release suite within the same `runtime_test` package.
// Go forbids duplicate type declarations within one package.
type recoveryDeps struct {
	ctrl       *gomock.Controller
	mockRel    *passertmocks.MockAssertionReleaser
	mockRunner *focusmocks.MockShortcutsRunner
	mockLive   *passertmocks.MockLiveChecker
	mgr        *runtime.Manager
	tmpPath    string
	logBuf     *bytes.Buffer
	log        *slog.Logger
}

func newRecoveryDeps(t *testing.T) *recoveryDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &recoveryDeps{
		ctrl:       ctrl,
		mockRel:    passertmocks.NewMockAssertionReleaser(ctrl),
		mockRunner: focusmocks.NewMockShortcutsRunner(ctrl),
		mockLive:   passertmocks.NewMockLiveChecker(ctrl),
		mgr:        runtime.NewManager(path, logger),
		tmpPath:    path,
		logBuf:     logBuf,
		log:        logger,
	}
}

// writeSnapshot is a precondition helper for "file exists" test paths.
func writeSnapshot(t *testing.T, mgr *runtime.Manager, snap runtime.Snapshot) {
	t.Helper()
	if err := mgr.Write(snap); err != nil {
		t.Fatalf("Write precondition snapshot: %v", err)
	}
}

// TestRecoverFromCrash_NoFile_Nil — validation map ID 5-05-05. Fresh
// tmpdir, no Write performed. RecoverFromCrash returns nil; NONE of
// the mocks are invoked (gomock fails on unexpected calls — implicit
// assertion).
func TestRecoverFromCrash_NoFile_Nil(t *testing.T) {
	rd := newRecoveryDeps(t)
	ctx := context.Background()

	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash on missing file returned %v; want nil", err)
	}
}

// TestRecoverFromCrash_MalformedJSON_WarnRemove_Nil — validation map
// ID 5-05-06. Corrupted bytes on disk: warn-log + best-effort file
// removal + nil return (continue PreFlight). No mock invocations.
func TestRecoverFromCrash_MalformedJSON_WarnRemove_Nil(t *testing.T) {
	rd := newRecoveryDeps(t)
	if err := os.MkdirAll(filepath.Dir(rd.tmpPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(rd.tmpPath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile garbage: %v", err)
	}

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash on malformed JSON returned %v; want nil (warn + continue)", err)
	}
	if !strings.Contains(rd.logBuf.String(), "malformed runtime.json") {
		t.Errorf("log buffer missing 'malformed runtime.json'; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after malformed-JSON recovery: stat err = %v", err)
	}
}

// TestRecoverFromCrash_LivePID_ErrConcurrentInstance — validation map
// ID 5-05-04. Stored PID is alive AND snapshot is fresh (StartedAt within
// stalePIDWindow) → return wrapped ErrConcurrentInstance. mockRel +
// mockRunner MUST NOT be invoked (gomock fails on unexpected). File
// remains on disk (cleanup is user's manual step).
//
// Post- the snapshot MUST carry a fresh StartedAt — a zero/stale
// timestamp triggers the PID-recycling second-pass guard and falls
// through to the dead branch instead of bailing.
func TestRecoverFromCrash_LivePID_ErrConcurrentInstance(t *testing.T) {
	rd := newRecoveryDeps(t)
	const livePID = 12345
	// Fresh snapshot: within the 24h stalePIDWindow → bail on live PID.
	writeSnapshot(t, rd.mgr, runtime.Snapshot{
		PID:         livePID,
		StartedAt:   time.Now().UTC().Add(-1 * time.Minute),
		AssertionID: 0xabcd,
	})
	rd.mockLive.EXPECT().IsAlive(livePID).Return(true)

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err == nil {
		t.Fatal("RecoverFromCrash returned nil; want ErrConcurrentInstance-wrapped error")
	}
	if !errors.Is(err, runtime.ErrConcurrentInstance) {
		t.Errorf("errors.Is(err, ErrConcurrentInstance) = false; want true (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "PID=12345") {
		t.Errorf("err message %q does not contain PID=12345", err.Error())
	}
	// File still on disk: caller exit 5; manual rm is user's step.
	if _, err := os.Stat(rd.tmpPath); err != nil {
		t.Errorf("file should still exist on live-PID path: stat err = %v", err)
	}
}

// TestRecoverFromCrash_LivePID_StaleFile_TreatsAsDead — regression.
// Stored PID happens to be alive (e.g. macOS recycled the dead dndmode's
// PID into a Chrome renderer between launches), BUT the snapshot is
// older than 24h. Recovery treats this as PID recycling (NOT a real
// concurrent instance), falls through to the dead-PID branch: release
// assertion, deactivate Focus, delete file, return nil. Without this
// second-pass guard the user would be permanently stuck in exit 5 with
// no recovery path (the user-visible failure mode exists to
// avoid).
func TestRecoverFromCrash_LivePID_StaleFile_TreatsAsDead(t *testing.T) {
	rd := newRecoveryDeps(t)
	const reusedPID = 8451
	const assertionID uint32 = 0xabcd
	// Stale: 25h old — outside the 24h stalePIDWindow.
	writeSnapshot(t, rd.mgr, runtime.Snapshot{
		PID:         reusedPID,
		StartedAt:   time.Now().UTC().Add(-25 * time.Hour),
		AssertionID: assertionID,
	})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(reusedPID).Return(true),
		rd.mockRel.EXPECT().Release(assertionID).Return(nil),
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash on stale+live-PID returned %v; want nil (treat as PID recycling)", err)
	}
	if !strings.Contains(rd.logBuf.String(), "stale snapshot") {
		t.Errorf("log buffer missing 'stale snapshot' warn; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after stale-PID-recycling branch: stat err = %v", err)
	}
}

// TestRecoverFromCrash_LivePID_ZeroStartedAt_TreatsAsDead
// edge case. A corrupted/truncated runtime.json with zero StartedAt and
// a live PID is treated as stale (defense-in-depth — the snapshot is
// untrusted). Falls through to the dead-PID branch.
func TestRecoverFromCrash_LivePID_ZeroStartedAt_TreatsAsDead(t *testing.T) {
	rd := newRecoveryDeps(t)
	const reusedPID = 8451
	const assertionID uint32 = 0xabcd
	// Zero StartedAt: corrupted/truncated runtime.json.
	writeSnapshot(t, rd.mgr, runtime.Snapshot{
		PID:         reusedPID,
		AssertionID: assertionID,
	})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(reusedPID).Return(true),
		rd.mockRel.EXPECT().Release(assertionID).Return(nil),
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	ctx := context.Background()
	if err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log); err != nil {
		t.Fatalf("RecoverFromCrash on zero-StartedAt+live-PID returned %v; want nil", err)
	}
	if !strings.Contains(rd.logBuf.String(), "stale snapshot") {
		t.Errorf("log buffer missing 'stale snapshot' warn; got:\n%s", rd.logBuf.String())
	}
}

// TestRecoverFromCrash_DeadPID_ReleasesAssertion — validation map ID
// 5-05-01. Dead PID → InOrder: IsAlive(false) → Release(id) → Run("dndmode-off").
// All succeed; RecoverFromCrash returns nil; file removed; log contains
// "recovery: released orphan assertion".
func TestRecoverFromCrash_DeadPID_ReleasesAssertion(t *testing.T) {
	rd := newRecoveryDeps(t)
	const deadPID = 99999
	const assertionID uint32 = 0xabcd
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: deadPID, AssertionID: assertionID})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(deadPID).Return(false),
		rd.mockRel.EXPECT().Release(assertionID).Return(nil),
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash returned %v; want nil", err)
	}
	if !strings.Contains(rd.logBuf.String(), "recovery: released orphan assertion") {
		t.Errorf("log buffer missing 'recovery: released orphan assertion'; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after happy-path recovery: stat err = %v", err)
	}
}

// TestRecoverFromCrash_DeadPID_DeactivatesFocus — validation map ID
// 5-05-02. Focused on the runner.Run("dndmode-off") call. Largely a
// duplicate of #4 but kept separate per validation map for surface
// area documentation.
func TestRecoverFromCrash_DeadPID_DeactivatesFocus(t *testing.T) {
	rd := newRecoveryDeps(t)
	const deadPID = 99999
	const assertionID uint32 = 0xabcd
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: deadPID, AssertionID: assertionID})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(deadPID).Return(false),
		rd.mockRel.EXPECT().Release(assertionID).Return(nil),
		// Key expectation: runner.Run called with "dndmode-off" (NOT "dndmode-on").
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	ctx := context.Background()
	if err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log); err != nil {
		t.Fatalf("RecoverFromCrash returned %v; want nil", err)
	}
}

// TestRecoverFromCrash_DeadPID_DeletesFile — validation map ID 5-05-03.
// Verify file is removed at the end of the happy path.
func TestRecoverFromCrash_DeadPID_DeletesFile(t *testing.T) {
	rd := newRecoveryDeps(t)
	const deadPID = 99999
	const assertionID uint32 = 0xabcd
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: deadPID, AssertionID: assertionID})

	rd.mockLive.EXPECT().IsAlive(deadPID).Return(false)
	rd.mockRel.EXPECT().Release(assertionID).Return(nil)
	rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil)

	ctx := context.Background()
	if err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log); err != nil {
		t.Fatalf("RecoverFromCrash returned %v; want nil", err)
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed: stat err = %v", err)
	}
}

// TestRecoverFromCrash_DeadPID_ZeroAssertionID_SkipsRelease
// regression. snapshot has dead PID but AssertionID == 0 (either
// Manager.Write never landed, or runtime.json is corrupted to a zero
// snapshot field). Recovery MUST skip rel.Release(0) — calling
// IOPMAssertionRelease(0) is wasted work (misleading "released orphan
// id=0" log line) and IOKit log noise. Phase 3 CleanupOrphans at
// Step 11 handles any genuine orphan via name+type+dead-PID heuristic.
// mockRel MUST NOT be invoked (gomock fails on unexpected calls). The
// Focus deactivate + file delete steps still run.
func TestRecoverFromCrash_DeadPID_ZeroAssertionID_SkipsRelease(t *testing.T) {
	rd := newRecoveryDeps(t)
	const deadPID = 99999
	writeSnapshot(t, rd.mgr, runtime.Snapshot{
		PID:         deadPID,
		StartedAt:   time.Now().UTC().Add(-1 * time.Minute),
		AssertionID: 0, // explicit zero — the trigger.
	})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(deadPID).Return(false),
		// rd.mockRel.EXPECT().Release intentionally NOT registered.
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	ctx := context.Background()
	if err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log); err != nil {
		t.Fatalf("RecoverFromCrash returned %v; want nil", err)
	}
	if !strings.Contains(rd.logBuf.String(), "no assertion id stored") {
		t.Errorf("log buffer missing 'no assertion id stored' warn; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after zero-AssertionID recovery: stat err = %v", err)
	}
}

// TestRecoverFromCrash_DeadPID_AssertionReleaseFail_WarnContinue —
// validation map ID 5-05-09. AssertionReleaser.Release returns an
// error: log a warn and CONTINUE (best-effort; kernel auto-reaps on
// process exit anyway). RecoverFromCrash returns nil overall, file
// is still removed.
func TestRecoverFromCrash_DeadPID_AssertionReleaseFail_WarnContinue(t *testing.T) {
	rd := newRecoveryDeps(t)
	const deadPID = 99999
	const assertionID uint32 = 0xabcd
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: deadPID, AssertionID: assertionID})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(deadPID).Return(false),
		rd.mockRel.EXPECT().Release(assertionID).Return(errors.New("simulated IOPMAssertionRelease error")),
		// runner.Run STILL invoked — assertion-release failure does NOT short-circuit.
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	ctx := context.Background()
	if err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log); err != nil {
		t.Fatalf("RecoverFromCrash returned %v; want nil (best-effort)", err)
	}
	if !strings.Contains(rd.logBuf.String(), "recovery: release stored assertion failed") {
		t.Errorf("log buffer missing 'recovery: release stored assertion failed'; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file should still be removed despite assertion-release failure: stat err = %v", err)
	}
}

// TestRecoverFromCrash_DeadPID_ShortcutsFail_WarnContinue — validation
// map ID 5-05-07. runner.Run("dndmode-off") returns an error: log warn
// and CONTINUE (the design notes best-effort). nil overall.
func TestRecoverFromCrash_DeadPID_ShortcutsFail_WarnContinue(t *testing.T) {
	rd := newRecoveryDeps(t)
	const deadPID = 99999
	const assertionID uint32 = 0xabcd
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: deadPID, AssertionID: assertionID})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(deadPID).Return(false),
		rd.mockRel.EXPECT().Release(assertionID).Return(nil),
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(errors.New("simulated shortcuts run failure")),
	)

	ctx := context.Background()
	if err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log); err != nil {
		t.Fatalf("RecoverFromCrash returned %v; want nil (best-effort)", err)
	}
	if !strings.Contains(rd.logBuf.String(), "recovery: focus deactivate failed") {
		t.Errorf("log buffer missing 'recovery: focus deactivate failed'; got:\n%s", rd.logBuf.String())
	}
}

// TestRecoverFromCrash_SuspectPID_ZeroPID_TreatsAsDead — regression.
// snap.PID == 0 is the kernel sentinel that POSIX kill(0,0) interprets as
// "broadcast to my process group" → returns nil ("alive") → exit 5 every
// launch. The validation gate must reject this BEFORE the IsAlive call,
// treat the snapshot as dead, remove the file, and return nil so PreFlight
// continues. mockLive MUST NOT be invoked (gomock fails on unexpected calls
// — implicit assertion).
func TestRecoverFromCrash_SuspectPID_ZeroPID_TreatsAsDead(t *testing.T) {
	rd := newRecoveryDeps(t)
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: 0, AssertionID: 0xabcd})

	// No EXPECT calls: live.IsAlive, rel.Release, runner.Run must all be skipped.

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash on PID=0 returned %v; want nil (treat as dead, continue PreFlight)", err)
	}
	if !strings.Contains(rd.logBuf.String(), "refusing to dispatch on suspect PID") {
		t.Errorf("log buffer missing suspect-PID warn; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after suspect-PID branch: stat err = %v", err)
	}
}

// TestRecoverFromCrash_SuspectPID_OwnPID_TreatsAsDead — regression.
// snap.PID == os.Getpid() of the recovering process trivially passes
// kill(pid, 0) ("can signal myself") → "alive" → exit 5 every launch.
// Validation gate rejects.
func TestRecoverFromCrash_SuspectPID_OwnPID_TreatsAsDead(t *testing.T) {
	rd := newRecoveryDeps(t)
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: os.Getpid(), AssertionID: 0xabcd})

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash on PID==own returned %v; want nil", err)
	}
	if !strings.Contains(rd.logBuf.String(), "refusing to dispatch on suspect PID") {
		t.Errorf("log buffer missing suspect-PID warn; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after own-PID branch: stat err = %v", err)
	}
}

// TestRecoverFromCrash_SuspectPID_NegativePID_TreatsAsDead
// regression. snap.PID == -1 is POSIX "kill every process I can signal"
// (returns nil → "alive"). Validation gate rejects.
func TestRecoverFromCrash_SuspectPID_NegativePID_TreatsAsDead(t *testing.T) {
	rd := newRecoveryDeps(t)
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: -1, AssertionID: 0xabcd})

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err != nil {
		t.Fatalf("RecoverFromCrash on PID=-1 returned %v; want nil", err)
	}
	if !strings.Contains(rd.logBuf.String(), "refusing to dispatch on suspect PID") {
		t.Errorf("log buffer missing suspect-PID warn; got:\n%s", rd.logBuf.String())
	}
	if _, err := os.Stat(rd.tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file not removed after negative-PID branch: stat err = %v", err)
	}
}

// TestRecoverFromCrash_DeadPID_FileDeleteFail_ErrFileDeletePersistent —
// validation map ID 5-05-08. Induce os.Remove failure deterministically
// by chmod-ing the file's PARENT directory (not the t.TempDir root) to
// 0o500 (read+execute, no write).
//
// CRITICAL: Chmod-ing t.TempDir() root is unsafe — breaks Go's
// testing.T cleanup invariant. The test runner's RemoveAll fails on
// 0o500 dirs and either leaks tmpdirs or panics. Always nest the
// chmod target under a directory the test creates itself, and restore
// perms via t.Cleanup BEFORE the testing harness's RemoveAll fires.
func TestRecoverFromCrash_DeadPID_FileDeleteFail_ErrFileDeletePersistent(t *testing.T) {
	rd := newRecoveryDeps(t)
	// Build a nested parentDir we own under t.TempDir; chmod ONLY this
	// nested dir, never the t.TempDir root.
	parentDir := filepath.Join(t.TempDir(), "nested")
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		t.Fatalf("MkdirAll nested: %v", err)
	}
	rd.tmpPath = filepath.Join(parentDir, "runtime.json")
	rd.mgr = runtime.NewManager(rd.tmpPath, rd.log)

	const deadPID = 99999
	const assertionID uint32 = 0xabcd
	writeSnapshot(t, rd.mgr, runtime.Snapshot{PID: deadPID, AssertionID: assertionID})

	gomock.InOrder(
		rd.mockLive.EXPECT().IsAlive(deadPID).Return(false),
		rd.mockRel.EXPECT().Release(assertionID).Return(nil),
		rd.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil),
	)

	// Make the parent dir non-writable so os.Remove(runtime.json) fails.
	if err := os.Chmod(parentDir, 0o500); err != nil {
		t.Fatalf("Chmod parentDir 0o500: %v", err)
	}
	t.Cleanup(func() {
		// Restore perms BEFORE testing.T's RemoveAll fires, otherwise the
		// test runner leaves the temp tree behind (or panics on some
		// filesystems).
		_ = os.Chmod(parentDir, 0o700)
	})

	ctx := context.Background()
	err := runtime.RecoverFromCrash(ctx, rd.mgr, rd.mockRel, rd.mockRunner, rd.mockLive, rd.log)
	if err == nil {
		t.Fatal("RecoverFromCrash returned nil on os.Remove failure; want ErrFileDeletePersistent-wrapped")
	}
	if !errors.Is(err, runtime.ErrFileDeletePersistent) {
		t.Errorf("errors.Is(err, ErrFileDeletePersistent) = false; want true (err=%v)", err)
	}
	if !strings.Contains(err.Error(), rd.tmpPath) {
		t.Errorf("err message %q does not include path %q for the user-facing stderr template",
			err.Error(), rd.tmpPath)
	}
	// regression: underlying fs.ErrPermission must be preserved via
	// multi-%w wrap so callers can discriminate the failure subtype with
	// errors.Is. chmod 0o500 on the parent dir induces EACCES → maps to
	// fs.ErrPermission in Go's stdlib error tree.
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("errors.Is(err, fs.ErrPermission) = false; want true (inner cause must be preserved via %%w; err=%v)", err)
	}
}
