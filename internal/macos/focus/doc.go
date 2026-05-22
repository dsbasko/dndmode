//go:build darwin

// Package focus wraps the Apple Shortcuts CLI (`/usr/bin/shortcuts`) for the
// dndmode Focus / Do Not Disturb lifecycle. are implemented in this
// package across..05-03:
//
// - CheckShortcuts: pre-flight verification that both
// `dndmode-on` and `dndmode-off` user shortcuts exist.
// - Activate: invoke `shortcuts run dndmode-on` at lifecycle
// Step 13.7.
// - Deactivate: invoke `shortcuts run dndmode-off` from the
// state.Releaser chain on shutdown.
// - Releaser: idempotent *Releaser satisfying state.Releaser
// without importing the state package.
//
// The package layout mirrors the Phase 3 powerassert pattern (one package per
// Apple framework family). focus holds the Shortcuts CLI subprocess seam;
// permissions / powerassert (sibling packages) hold the cgo bridges to TCC
// and IOKit respectively.
//
// # Threading invariants
//
//   - ShortcutsRunner methods (List / Run) are safe to call from any
//     goroutine. The production execShortcutsRunner uses
//     exec.CommandContext which is goroutine-safe; cancellation of ctx
//     kills the subprocess via SIGKILL (Go's os/exec contract).
// - CheckShortcuts is invoked exactly once at PreFlight
//     Step 9.5 — before powerassert.Acquire — so an early "missing
//     shortcut" failure short-circuits without holding any IOKit
// resource (never enter Active state with missing dndmode-off).
// - Activate / Deactivate are not concurrent in the
//     production flow: Activate runs on the main goroutine just before
//     RestoreState.Cleanup is registered; Deactivate runs inside the
//     LIFO Cleanup chain. The DI seam (ShortcutsRunner) plus the
//     Releaser's atomic.Bool + sync.Mutex idempotency (mirrors
// powerassert.Assertion design) defend against the rare
//     overlapping-signal-handler case.
//
// # State.Releaser conformance
//
// *Releaser will satisfy state.Releaser (Release() error
// Name() string) without importing the state package — same compile-time
// blank-identifier interface assignment trick used in
// powerassert/assertion.go lines 187-190. This file documents the
// contract up-front so downstream and can wire the
// type into the RestoreState LIFO chain without a discovery step.
//
// # Sources
//
// - the design notes (
// Shortcuts CLI invocation; exit-code investigation)
// - the design notes
// - the design notes
//   - Apple — shortcuts(1) man page (Sonoma / Sequoia)
package focus
