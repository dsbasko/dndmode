//go:build darwin

package runtime

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
)

// Manager owns the lifecycle of a single runtime.json path:
// atomic Write, fs.ErrNotExist-sentinel Read, idempotent Release.
// Implements state.Releaser without importing the state package (avoids
// import cycle — same trick as powerassert.Assertion).
//
// Construction does NOT touch the filesystem (Phase 1 push-after-
// create discipline). All fs operations happen inside Write / Read /
// Release.
type Manager struct {
	path string
	log  *slog.Logger

	// released is the fast-path hint for Release idempotency. Set to
	// true AFTER os.Remove has fully completed under mu. atomic.Load
	// lets repeat callers avoid the mutex.
	released atomic.Bool

	// mu serializes concurrent Release callers. Same rationale as
	// powerassert.Assertion.mu post- the CAS+sync.Once pattern
	// has a race where caller #2 observes released=true while caller
	// #1 is still inside os.Remove. A plain Mutex makes every caller
	// block until the winner finishes.
	mu sync.Mutex
}

// NewManager constructs a *Manager bound to the given absolute path
// (typically `<HOME>/.config/dndmode/runtime.json` derived in
// cmd/dndmode/main.go Step 10.5). Logger fallback: nil →
// slog.Default(). NO filesystem side effects — Phase 1 discipline.
func NewManager(path string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{path: path, log: log}
}

// Path returns the absolute path the Manager was constructed with.
// Used by main.go when building the ErrFileDeletePersistent
// stderr template per the design notes ("cannot delete <path>; rm -f <path>
// and retry").
func (m *Manager) Path() string { return m.path }

// Name implements state.Releaser. Returns the constant string
// "runtime-file" so 's acceptance test
// (TestAcceptance_LIFE06_PushOrder) can parse `released
// releaser=runtime-file` from stderr and verify the slot-#5 position
// at the end of the LIFO Cleanup chain.
func (m *Manager) Name() string { return "runtime-file" }

// Write serializes the given Snapshot to JSON and atomically replaces
// the on-disk file. Crash-safe pattern (the design notes, the design notes
// 3):
//
//  1. Defensive MkdirAll of the parent directory at 0o700. Mirrors
//     internal/config/config.go configDirPerm; handles the edge case
//     where the user removed ~/.config/dndmode/ between Phase 1
//     config.Load and this call (e.g. `make clean`).
//  2. json.MarshalIndent — pretty-printed for diagnostic readability.
//  3. Write to `<path>.tmp.<pid>` (same-dir temp; avoids EXDEV across
// filesystem boundaries — the design notes).
//  4. os.Rename — APFS guarantees atomic same-volume replace. SIGKILL
//     between WriteFile and Rename leaves the original (or nothing,
//     for the first Write) intact; no partial observable state.
//  5. On any error during temp-file Write or Rename: best-effort
//     os.Remove of the temp file, then return the wrapped error.
func (m *Manager) Write(s Snapshot) error {
	dir := filepath.Dir(m.path)
	// 0o700: user-private — matches configDirPerm from Phase 1
	// internal/config/config.go:28. //nolint:gosec // G302: intentional
	// user-private perm.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir runtime dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime snapshot: %w", err)
	}
	tmpPath := m.path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write runtime temp file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, m.path); err != nil {
		// Best-effort cleanup of the temp file on rename failure.
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename runtime temp file %q -> %q: %w", tmpPath, m.path, err)
	}
	return nil
}

// Read deserializes the runtime.json content. Returns:
//   - (s, nil) on the happy path.
//   - (zero-Snapshot, fs.ErrNotExist) DIRECTLY when the file is missing
//     — caller (recovery.go) checks `errors.Is(err, fs.ErrNotExist)` as
//     the "nothing to recover" sentinel.
//   - (zero-Snapshot, wrapped) for any other error (permission, IO,
//     malformed JSON). Caller logs warn and falls through to best-
//     effort cleanup.
func (m *Manager) Read() (Snapshot, error) {
	var s Snapshot
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Snapshot{}, fs.ErrNotExist
		}
		return Snapshot{}, fmt.Errorf("read runtime file %q: %w", m.path, err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal runtime file %q: %w", m.path, err)
	}
	return s, nil
}

// Release implements state.Releaser. Removes the runtime.json file
// with two-layer idempotency:
//
//  1. Fast path: atomic.Bool Load — once released is durably true, any
//     repeat caller returns nil instantly without touching the mutex.
//  2. Slow path: sync.Mutex serialization — concurrent first-time
//     callers serialize here. The winner double-checks released under
//     mu, invokes os.Remove, stores released=true, and Unlocks.
//
// Release-before-write idempotency: os.Remove returning fs.ErrNotExist
// is treated as success (nil return). recovery.go relies on this so it
// can call Release on a stale-or-missing runtime.json without
// branching on existence.
//
// On any OTHER os.Remove error (permission denied, mount read-only,
// etc.), returns the wrapped error so main.go can map via errors.Is
// to ErrFileDeletePersistent / exit code 7.
//
// Mirrors powerassert.Assertion.Release post- the CAS Once
// pattern had a race where caller #2 observed released=true while
// caller #1 was still inside os.Remove. The Mutex ensures every caller
// blocks until the os.Remove completes.
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
		return fmt.Errorf("remove runtime file %q: %w", m.path, err)
	}
	m.released.Store(true)
	return nil
}

// Compile-time check: *Manager satisfies state.Releaser without
// importing the state package (would create an import cycle —
// cmd/dndmode/main.go is the only caller that holds *Manager as
// state.Releaser). Mismatch surfaces here at build time. Mirrors
// powerassert/assertion.go lines 187-190 verbatim.
var _ interface {
	Release() error
	Name() string
} = (*Manager)(nil)
