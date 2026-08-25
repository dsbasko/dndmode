//go:build darwin

package watch_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/state/watch"
)

func TestManager_WriteReadRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mgr := watch.NewManager(filepath.Join(dir, "nested", "watch.json"), nil)

	if _, err := mgr.Read(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read before Write: err = %v, want fs.ErrNotExist", err)
	}

	start := time.Date(2026, 8, 26, 10, 0, 0, 123456000, time.UTC)
	rec := watch.Record{
		PID:            4242,
		StartedAt:      start.Add(time.Second),
		ProcStartedAt:  start,
		ActivateHotkey: "Ctrl+Option+Cmd+D",
		LogPath:        "/tmp/watch.log",
	}
	if err := mgr.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fi, err := os.Stat(mgr.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("record perm = %o, want 0600", fi.Mode().Perm())
	}

	got, err := mgr.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.PID != rec.PID || got.ActivateHotkey != rec.ActivateHotkey || got.LogPath != rec.LogPath {
		t.Errorf("Read = %+v, want %+v", got, rec)
	}
	if !got.StartedAt.Equal(rec.StartedAt) || !got.ProcStartedAt.Equal(rec.ProcStartedAt) {
		t.Errorf("times did not round-trip: got %v / %v", got.StartedAt, got.ProcStartedAt)
	}

	// No temp file left behind.
	entries, err := os.ReadDir(filepath.Dir(mgr.Path()))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries after Write, want 1 (no temp file)", len(entries))
	}

	if err := mgr.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(mgr.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record still on disk after Release: %v", err)
	}
	// Idempotent.
	if err := mgr.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	if mgr.Name() != "watch-file" {
		t.Errorf("Name = %q", mgr.Name())
	}
}

// TestManager_WriteAfterReleaseReArms mirrors the runtime.Manager pin: a
// --watch that removed a stale record must still remove its OWN record on
// exit, so the release latch has to be reset by Write.
func TestManager_WriteAfterReleaseReArms(t *testing.T) {
	t.Parallel()
	mgr := watch.NewManager(filepath.Join(t.TempDir(), "watch.json"), nil)

	if err := mgr.Write(watch.Record{PID: 1}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mgr.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := mgr.Write(watch.Record{PID: 2}); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if err := mgr.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if _, err := os.Stat(mgr.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record survived the second Release: %v", err)
	}
}

// TestRecord_OmitsZeroProcStart pins the omitzero on proc_started_at: a
// process that could not read its own start time must not write a bogus
// epoch value that Inspect would then compare against and call stale.
func TestRecord_OmitsZeroProcStart(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(watch.Record{PID: 7, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["proc_started_at"]; ok {
		t.Errorf("zero proc_started_at was serialized: %s", data)
	}
	if _, ok := m["log"]; ok {
		t.Errorf("empty log path was serialized: %s", data)
	}
}

func TestManager_Read_Malformed(t *testing.T) {
	t.Parallel()
	mgr := watch.NewManager(filepath.Join(t.TempDir(), "watch.json"), nil)
	if err := os.WriteFile(mgr.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := mgr.Read()
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read of malformed record: err = %v, want a non-ErrNotExist error", err)
	}
}
