//go:build darwin

package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Record is the watch.json schema. Written once by the watch process at the
// moment it is ready to receive presses, read by `--status` / `--kill` and by
// the next `--watch`, removed by the process on a clean exit.
//
//   - PID: os.Getpid() of the watch process.
//   - StartedAt: wall-clock time the record was written (UTC, RFC3339).
//     Informational — `--status` prints it and an uptime derived from it.
//   - ProcStartedAt: the process start time as the KERNEL reports it, so a
//     reused PID can be told apart from the process that wrote the record.
//     Zero (omitted from the JSON) when the process could not read its own
//     start time; Inspect then falls back to the PID alone.
//   - ActivateHotkey: the combination the process is waiting for, in the
//     spelling the user wrote. Not a secret (see internal/macos/globalhotkey);
//     `--status` prints it.
//   - LogPath: where the process sends its stdout/stderr once detached.
//
// The JSON tags are a stable contract between dndmode versions on the same
// machine: a `--kill` from a newer binary must still find the record an older
// `--watch` wrote.
type Record struct {
	PID            int       `json:"pid"`
	StartedAt      time.Time `json:"started_at"`
	ProcStartedAt  time.Time `json:"proc_started_at,omitzero"`
	ActivateHotkey string    `json:"activate_hotkey"`
	LogPath        string    `json:"log,omitempty"`
}

// Manager owns the lifecycle of a single watch.json path: atomic Write,
// fs.ErrNotExist-sentinel Read, idempotent Release. It implements
// state.Releaser without importing the state package, the same way
// internal/state/runtime.Manager does, so the watch process can push it onto
// its cleanup stack and have the record removed on the way out.
//
// Construction touches nothing on disk.
type Manager struct {
	path string
	log  *slog.Logger

	released atomic.Bool
	mu       sync.Mutex
}

// NewManager binds a Manager to the given absolute path. Logger fallback:
// nil → slog.Default().
func NewManager(path string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{path: path, log: log}
}

// Path returns the absolute path the Manager was constructed with.
func (m *Manager) Path() string { return m.path }

// Name implements state.Releaser.
func (m *Manager) Name() string { return "watch-file" }

// Write serializes rec and atomically replaces the file: same-directory temp
// file plus rename, so a crash mid-write leaves either the previous record or
// none, never a torn one. It also re-arms Release, so a Manager that already
// released a stale record can publish a fresh one and have it removed again on
// exit — the same re-arm internal/state/runtime.Manager.Write documents.
func (m *Manager) Write(rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir watch dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal watch record: %w", err)
	}
	tmpPath := m.path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write watch temp file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, m.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename watch temp file %q -> %q: %w", tmpPath, m.path, err)
	}
	m.released.Store(false)
	return nil
}

// Read deserializes the record. A missing file returns fs.ErrNotExist
// DIRECTLY so callers can errors.Is on it; every other failure (permission,
// IO, malformed JSON) comes back wrapped.
func (m *Manager) Read() (Record, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, fs.ErrNotExist
		}
		return Record{}, fmt.Errorf("read watch file %q: %w", m.path, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("unmarshal watch file %q: %w", m.path, err)
	}
	return rec, nil
}

// Release implements state.Releaser: removes the file, idempotently. A file
// that is already gone counts as released.
func (m *Manager) Release() error {
	if m.released.Load() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.released.Load() {
		return nil
	}
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove watch file %q: %w", m.path, err)
	}
	m.released.Store(true)
	return nil
}

// Compile-time check: *Manager satisfies state.Releaser without importing the
// state package (import cycle — cmd/dndmode is the only caller that holds it
// as a state.Releaser).
var _ interface {
	Release() error
	Name() string
} = (*Manager)(nil)
