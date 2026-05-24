//go:build darwin

// Package runtime owns the lifecycle of `~/.config/dndmode/runtime.json` —
// the per-process snapshot file that the dndmode lifecycle writes after
// acquiring the IOPM assertion (Step 13.3 in cmd/dndmode/main.go) and
// deletes during the LIFO Cleanup chain (slot #5, after focus.Releaser).
//
// The file enables (crash recovery): a SIGKILL'd dndmode
// leaves a verifiable record of its PID + AssertionID + StartedAt +
// PriorFocus snapshot so the next launch can release the orphaned IOPM
// assertion by exact id (rather than the Phase 3 name+type+dead-PID
// heuristic, which is correct but less precise) and clean up Focus
// state if applicable.
//
// Note on package name: even though `runtime` is a stdlib package, this
// import path is `github.com/dsbasko/dndmode/internal/state/runtime` —
// the import path disambiguates the package fully. cmd/dndmode/main.go
// imports this as `runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"`
// to avoid shadowing stdlib `runtime` (only main.go actually imports
// both; everywhere else only one is in scope).
//
// # Threading invariants
//
//   - Manager.Write / Manager.Read / Manager.Release are safe to call
//     from any goroutine. Write + Read are not internally synchronized
//     (the production flow calls them serially: Write at Step 13.3,
//     Read once at Step 10.5 by recovery), but the implementation does
//     not race on Manager state — only the released atomic.Bool +
//     sync.Mutex serialize concurrent Release callers.
//   - Manager.Release embeds two-layer idempotency (atomic.Bool
//     fast-path + sync.Mutex slow-path) so overlapping Cleanup
//     invocations from signal handlers / defer chains see exactly one
// underlying os.Remove call (mirror powerassert.Assertion post).
//   - Atomic write: Manager.Write uses `<path>.tmp.<pid>` + os.Rename
//     for crash-resistance on APFS same-volume rename. SIGKILL between
//     WriteFile and Rename leaves the original file intact (no partial
// observable state). the design notes.
//
// # State.Releaser conformance
//
// *Manager satisfies state.Releaser (Release() error + Name() string)
// without importing the state package, avoiding an import cycle. The
// satisfaction is enforced at compile time inside manager.go via a
// blank-identifier interface assignment (mirror powerassert/assertion.go
// lines 187-190).
//
// # Sources
//
// - the design notes (
// runtime.json schema + temp-file path + atomic
//     rename)
// - the design notes (schema,
// atomic write, ErrFileDeletePersistent UX, t.TempDir)
// - the design notes (
//)
// - internal/config/config.go (Phase 1
//     push-after-create discipline; configDirPerm 0o700)
package runtime
