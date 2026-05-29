//go:build darwin

package runtime_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	passertmocks "github.com/dsbasko/dndmode/internal/macos/powerassert/mocks"
	"github.com/dsbasko/dndmode/internal/state/runtime"
)

// liveCheckDeps groups the gomock controller, the powerassert LiveChecker
// mock, a REAL *runtime.Manager bound to t.TempDir(), and a captured slog
// buffer for log-line assertions. Mirrors recoveryDeps shape from
// recovery_test.go but trimmed to what IsLiveInstance needs (no Focus
// runner, no Releaser, no ctx propagation — is a stateless read).
//
// Named liveCheckDeps (not testDeps) to avoid collision with the testDeps
// already declared in manager_test.go within the same `runtime_test`
// package.
type liveCheckDeps struct {
	ctrl     *gomock.Controller
	mockLive *passertmocks.MockLiveChecker
	mgr      *runtime.Manager
	tmpPath  string
	logBuf   *bytes.Buffer
	log      *slog.Logger
}

func newLiveCheckDeps(t *testing.T) *liveCheckDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &liveCheckDeps{
		ctrl:     ctrl,
		mockLive: passertmocks.NewMockLiveChecker(ctrl),
		mgr:      runtime.NewManager(path, logger),
		tmpPath:  path,
		logBuf:   logBuf,
		log:      logger,
	}
}

// TestIsLiveInstance_NoFile_ReturnsNotAlive validates the happy cold-start
// path: no runtime.json on disk → no live peer, no error, IsAlive must
// NOT be called (gomock controller fails on unexpected calls — implicit
// assertion).
func TestIsLiveInstance_NoFile_ReturnsNotAlive(t *testing.T) {
	d := newLiveCheckDeps(t)
	// no writeSnapshot — file does not exist
	// no mockLive.EXPECT() — IsAlive must NOT be invoked

	alive, pid, err := runtime.IsLiveInstance(d.mgr, d.mockLive, d.log)
	if err != nil {
		t.Fatalf("IsLiveInstance on missing file returned err=%v; want nil", err)
	}
	if alive {
		t.Errorf("alive = true; want false (no file)")
	}
	if pid != 0 {
		t.Errorf("pid = %d; want 0 (no file)", pid)
	}
}

// TestIsLiveInstance_LivePID_ReturnsAlive validates the bail path:
// runtime.json exists with a PID, LiveChecker says alive → return
// (true, pid, nil). The file MUST remain on disk — does NOT delete.
func TestIsLiveInstance_LivePID_ReturnsAlive(t *testing.T) {
	d := newLiveCheckDeps(t)
	const livePID = 12345
	writeSnapshot(t, d.mgr, runtime.Snapshot{
		PID:         livePID,
		StartedAt:   time.Now().UTC().Add(-1 * time.Minute),
		AssertionID: 0xabcd,
	})
	d.mockLive.EXPECT().IsAlive(livePID).Return(true)

	alive, pid, err := runtime.IsLiveInstance(d.mgr, d.mockLive, d.log)
	if err != nil {
		t.Fatalf("IsLiveInstance returned err=%v; want nil", err)
	}
	if !alive {
		t.Errorf("alive = false; want true (live PID)")
	}
	if pid != livePID {
		t.Errorf("pid = %d; want %d", pid, livePID)
	}
	if _, statErr := os.Stat(d.tmpPath); statErr != nil {
		t.Errorf("runtime.json removed after IsLiveInstance; should not delete: %v", statErr)
	}
}

// TestIsLiveInstance_DeadPID_ReturnsNotAlive validates that dead-PID
// detection is reported back to the caller without side effects —
// recovery (Step 10.5) handles file deletion + assertion release;
// only reads.
func TestIsLiveInstance_DeadPID_ReturnsNotAlive(t *testing.T) {
	d := newLiveCheckDeps(t)
	const deadPID = 99999
	writeSnapshot(t, d.mgr, runtime.Snapshot{
		PID:         deadPID,
		StartedAt:   time.Now().UTC().Add(-1 * time.Hour),
		AssertionID: 0xdead,
	})
	d.mockLive.EXPECT().IsAlive(deadPID).Return(false)

	alive, pid, err := runtime.IsLiveInstance(d.mgr, d.mockLive, d.log)
	if err != nil {
		t.Fatalf("IsLiveInstance returned err=%v; want nil", err)
	}
	if alive {
		t.Errorf("alive = true; want false (dead PID)")
	}
	if pid != deadPID {
		t.Errorf("pid = %d; want %d", pid, deadPID)
	}
	if _, statErr := os.Stat(d.tmpPath); statErr != nil {
		t.Errorf("runtime.json removed after IsLiveInstance; should not delete: %v", statErr)
	}
}

// TestIsLiveInstance_Malformed_ReturnsErr validates that parse failures
// surface to the caller (main.go logs warn + continues; recovery deals
// with the residue). File is NOT deleted — that's recovery's
// responsibility.
func TestIsLiveInstance_Malformed_ReturnsErr(t *testing.T) {
	d := newLiveCheckDeps(t)
	if err := os.MkdirAll(filepath.Dir(d.tmpPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(d.tmpPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile garbage: %v", err)
	}
	// No mockLive.EXPECT() — IsAlive must NOT be called when read fails.

	alive, pid, err := runtime.IsLiveInstance(d.mgr, d.mockLive, d.log)
	if err == nil {
		t.Fatalf("IsLiveInstance on malformed JSON returned nil err; want non-nil")
	}
	if alive {
		t.Errorf("alive = true; want false (malformed JSON)")
	}
	if pid != 0 {
		t.Errorf("pid = %d; want 0 (malformed JSON)", pid)
	}
	if err.Error() == "" {
		t.Errorf("err string is empty; should describe read failure")
	}
	if _, statErr := os.Stat(d.tmpPath); statErr != nil {
		t.Errorf("runtime.json removed after malformed read: %v", statErr)
	}
}

// TestIsLiveInstance_InvalidPID_ReturnsNotAlive validates the defensive
// guard against zero/negative PIDs in a corrupted runtime.json — skip
// the IsAlive call entirely to avoid POSIX kill(0/-N, sig) edge cases.
func TestIsLiveInstance_InvalidPID_ReturnsNotAlive(t *testing.T) {
	d := newLiveCheckDeps(t)
	writeSnapshot(t, d.mgr, runtime.Snapshot{
		PID:         0,
		StartedAt:   time.Now().UTC(),
		AssertionID: 0,
	})
	// No mockLive.EXPECT() — IsAlive must NOT be invoked on invalid PID.

	alive, pid, err := runtime.IsLiveInstance(d.mgr, d.mockLive, d.log)
	if err != nil {
		t.Fatalf("IsLiveInstance returned err=%v; want nil", err)
	}
	if alive {
		t.Errorf("alive = true; want false (zero PID)")
	}
	if pid != 0 {
		t.Errorf("pid = %d; want 0", pid)
	}
	logs := d.logBuf.String()
	if !strings.Contains(logs, "invalid PID") {
		t.Errorf("log buffer missing 'invalid PID' warn; got: %q", logs)
	}
}
