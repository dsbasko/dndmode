//go:build darwin

package runtime_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/state/runtime"
)

// testDeps groups a *Manager bound to a per-test t.TempDir() path with
// the absolute path itself so test bodies can stat / read the file
// directly. Per the design notes, no afero — real filesystem semantics
// matter (APFS atomic rename, file mode bits).
type testDeps struct {
	mgr  *runtime.Manager
	path string
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	return &testDeps{mgr: runtime.NewManager(path, nil), path: path}
}

// canonicalSnapshot returns a Snapshot fixture used across Write-path
// tests. Deterministic for diff-friendly failure output.
func canonicalSnapshot() runtime.Snapshot {
	return runtime.Snapshot{
		PID:         12345,
		StartedAt:   time.Date(2026, 5, 14, 9, 42, 13, 0, time.UTC),
		PriorFocus:  nil,
		AssertionID: 67890,
	}
}

// TestManager_Write_ProducesValidJSON — validation map ID 5-04-02.
// Write a Snapshot, ReadFile + json.Unmarshal verifies all four fields
// round-trip through the on-disk JSON.
func TestManager_Write_ProducesValidJSON(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	want := canonicalSnapshot()

	if err := td.mgr.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(td.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got runtime.Snapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v; raw=%s", err, raw)
	}
	if got.PID != want.PID || got.AssertionID != want.AssertionID {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt: got %v, want %v", got.StartedAt, want.StartedAt)
	}
	if got.PriorFocus != nil {
		t.Errorf("PriorFocus: got %v, want nil", got.PriorFocus)
	}
}

// TestManager_Write_AtomicReplace — validation map ID 5-04-03. Two
// successive Writes: only the second survives, and NO `.tmp.*`
// stragglers are left in the parent dir (atomic rename consumed it).
func TestManager_Write_AtomicReplace(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	a := canonicalSnapshot()
	a.PID = 1001
	b := canonicalSnapshot()
	b.PID = 2002

	if err := td.mgr.Write(a); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := td.mgr.Write(b); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	raw, err := os.ReadFile(td.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got runtime.Snapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.PID != b.PID {
		t.Errorf("final pid: got %d, want %d (B should replace A atomically)", got.PID, b.PID)
	}

	// No .tmp.* stragglers in dir.
	entries, err := os.ReadDir(filepath.Dir(td.path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "runtime.json.tmp") {
			t.Errorf("leftover temp file in dir: %s", e.Name())
		}
	}
}

// TestManager_Write_FileMode_0o600 — validation map ID 5-04-04. The
// runtime.json file must be created with user-only rw permissions
// (mirror config.yml; user-private secrets policy).
func TestManager_Write_FileMode_0o600(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if err := td.mgr.Write(canonicalSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(td.path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file perm = %#o, want 0o600", got)
	}
}

// TestManager_Write_NoParentDir_MkdirAlls — the design notes.
// If the parent dir is missing (e.g. user `rm -rf ~/.config/dndmode`),
// Write must defensively MkdirAll(0o700) and succeed. We use the mask
// check `& 0o700 == 0o700` rather than equality because a hostile
// umask (umask 077) would clear group/world bits but not the user rwx
// triad we care about — equality fails spuriously while mask-check
// confirms the contract.
func TestManager_Write_NoParentDir_MkdirAlls(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	parent := filepath.Dir(td.path)
	if err := os.RemoveAll(parent); err != nil {
		t.Fatalf("RemoveAll parent: %v", err)
	}
	if _, err := os.Stat(parent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("parent unexpectedly present after RemoveAll: %v", err)
	}

	if err := td.mgr.Write(canonicalSnapshot()); err != nil {
		t.Fatalf("Write after RemoveAll(parent): %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("Stat parent after Write: %v", err)
	}
	if got := info.Mode().Perm() & 0o700; got != 0o700 {
		t.Errorf("parent dir perm & 0o700 = %#o, want 0o700 (user rwx)", got)
	}
}

// TestManager_Read_NotExist_ReturnsErrNotExist — Read on a fresh tmpdir
// (no Write yet) returns an error that satisfies
// `errors.Is(err, fs.ErrNotExist)`. recovery.go uses this
// sentinel as the "no prior runtime — nothing to recover" happy path.
func TestManager_Read_NotExist_ReturnsErrNotExist(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)

	_, err := td.mgr.Read()
	if err == nil {
		t.Fatal("Read returned nil err on missing file; want fs.ErrNotExist-wrapped error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err is not fs.ErrNotExist: %v", err)
	}
}

// TestManager_Read_MalformedJSON_Wrapped — write garbage bytes to
// runtime.json (simulating disk corruption); Read returns a non-nil err
// that is NOT fs.ErrNotExist. recovery.go logs warn + best-effort
// removes the file.
func TestManager_Read_MalformedJSON_Wrapped(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(td.path, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile garbage: %v", err)
	}

	_, err := td.mgr.Read()
	if err == nil {
		t.Fatal("Read returned nil err on malformed JSON; want wrapped parse error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("malformed-JSON error misclassified as fs.ErrNotExist: %v", err)
	}
}

// TestManager_Read_NilPriorFocus_Unmarshals — write canonical Snapshot
// (nil PriorFocus), Read, verify nil pointer survives.
func TestManager_Read_NilPriorFocus_Unmarshals(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if err := td.mgr.Write(canonicalSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := td.mgr.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.PriorFocus != nil {
		t.Errorf("PriorFocus = %v, want nil", got.PriorFocus)
	}
}

// TestManager_Read_StringPriorFocus_Unmarshals — manually write a
// v2-style snapshot with "prior_focus": "Work"; verify Read parses it
// into a non-nil *string. Forward-compat guard.
func TestManager_Read_StringPriorFocus_Unmarshals(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if err := os.MkdirAll(filepath.Dir(td.path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := []byte(`{
  "pid": 100,
  "started_at": "2026-05-14T09:42:13Z",
  "prior_focus": "Work",
  "assertion_id": 200
}`)
	if err := os.WriteFile(td.path, doc, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := td.mgr.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.PriorFocus == nil {
		t.Fatal("PriorFocus = nil; want pointer to \"Work\"")
	}
	if *got.PriorFocus != "Work" {
		t.Errorf("*PriorFocus = %q, want %q", *got.PriorFocus, "Work")
	}
}

// TestManager_Release_DeletesFile — validation map ID 5-04-05. Write,
// Release, os.Stat → fs.ErrNotExist.
func TestManager_Release_DeletesFile(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if err := td.mgr.Write(canonicalSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := td.mgr.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, err := os.Stat(td.path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file still exists after Release: stat err = %v", err)
	}
}

// TestManager_Release_MissingFile_Nil — validation map ID 5-04-07.
// Release before Write (no file ever existed) returns nil. The
// fs.ErrNotExist branch is "release-before-write idempotency" — a
// recovery flow that calls Release on a path that was never written
// must succeed silently.
func TestManager_Release_MissingFile_Nil(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if err := td.mgr.Release(); err != nil {
		t.Errorf("Release on missing file returned err = %v; want nil", err)
	}
}

// TestManager_Release_Idempotent_ConcurrentCallers — validation map ID
// 5-04-06. 10 goroutines call Release concurrently; assert ALL THREE
// invariants:
//
//	(a) all 10 callers return nil,
//	(b) released flag flipped (probed via N+1th Release that returns
//	    nil with no fs activity — proxy because the field is unexported),
//	(c) the file is ACTUALLY gone (os.Stat → fs.ErrNotExist) — without
//	    this side-effect check, a broken implementation that swallowed
//	    the error inside the mutex would pass (a) and (b).
//
// 20 iterations × 10 goroutines (= 200 race opportunities per run).
func TestManager_Release_Idempotent_ConcurrentCallers(t *testing.T) {
	// NOT t.Parallel — this test is heavier (200 goroutines × 20 iters)
	// and the per-iter t.TempDir() allocation should be sequenced for
	// clearer profiling.
	const numGoroutines = 10
	const iterations = 20

	for iter := 0; iter < iterations; iter++ {
		td := newTestDeps(t)
		if err := td.mgr.Write(canonicalSnapshot()); err != nil {
			t.Fatalf("iter=%d: Write: %v", iter, err)
		}

		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		done.Add(numGoroutines)
		errs := make([]error, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			i := i
			go func() {
				defer done.Done()
				start.Wait()
				errs[i] = td.mgr.Release()
			}()
		}
		start.Done()
		done.Wait()

		// (a) all callers returned nil.
		for i, e := range errs {
			if e != nil {
				t.Errorf("iter=%d: caller %d returned err = %v; want nil", iter, i, e)
			}
		}

		// (b) released flag flipped — N+1th Release returns nil silently.
		if err := td.mgr.Release(); err != nil {
			t.Errorf("iter=%d: N+1th Release returned %v; want nil (flag must be flipped)", iter, err)
		}

		// (c) file is actually gone.
		_, err := os.Stat(td.path)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("iter=%d: file still exists after concurrent Release: stat err = %v",
				iter, err)
		}
	}
}

// TestManager_Name_ReturnsRuntimeFile verifies the acceptance
// test contract: Name() == "runtime-file".
func TestManager_Name_ReturnsRuntimeFile(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if got := td.mgr.Name(); got != "runtime-file" {
		t.Errorf("Name() = %q, want %q", got, "runtime-file")
	}
}

// TestManager_Path_ReturnsConstructorPath verifies Path() exposes the
// absolute path passed to NewManager — main.go uses this in the
// ErrFileDeletePersistent stderr template.
func TestManager_Path_ReturnsConstructorPath(t *testing.T) {
	t.Parallel()
	td := newTestDeps(t)
	if got := td.mgr.Path(); got != td.path {
		t.Errorf("Path() = %q, want %q", got, td.path)
	}
}
