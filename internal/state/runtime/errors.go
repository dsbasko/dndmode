//go:build darwin

package runtime

import "errors"

// ErrConcurrentInstance is returned by RecoverFromCrash when
// runtime.json's stored PID is still alive (kill(pid, 0) returned nil or
// EPERM under conservative-alive policy). Mirrors the dispatch contract
// of powerassert.ErrConcurrentInstance — main.go dispatches via
// `errors.Is(err, runtime.ErrConcurrentInstance)` and maps to exit
// code 5 (`exitConcurrentInstance`).
//
// IMPORTANT — this is a DISTINCT error VALUE from
// `powerassert.ErrConcurrentInstance`. The two sentinels live in
// different packages and have slightly different user-facing strings:
//
//   - powerassert.ErrConcurrentInstance: "another dndmode instance is
//     holding the awake-lock"  (Phase 3 wording)
//   - runtime.ErrConcurrentInstance:    "another dndmode instance is
//     holding the awake-lock"  (this sentinel; intentionally matches
//     the Phase 3 wording so user-facing stderr is consistent across
//     both detection paths)
//
// The DISPATCH contract is `errors.Is` on the VALUE, NOT a string
// match. main.go's exit-code router checks both sentinels separately
// (`errors.Is(err, powerassert.ErrConcurrentInstance) ||
// errors.Is(err, runtime.ErrConcurrentInstance)`) and renders the same
// "another instance" stderr template regardless of which one matched.
// Even if the two wordings drifted in a future refactor, the dispatch
// would remain correct.
var ErrConcurrentInstance = errors.New("another dndmode instance is holding the awake-lock")

// ErrFileDeletePersistent is returned by RecoverFromCrash
// when `os.Remove(runtime.json)` fails with an error that is NOT
// `fs.ErrNotExist`. main.go dispatches via errors.Is and
// maps to exit code 7 (`exitFileDeletePersistent`), printing a stderr
// template that includes the absolute path (Manager.Path()) and a
// suggestion to manually `rm` the file — per CONTEXT D-12, this is a
// non-recoverable filesystem condition (mounted read-only, ACL deny,
// disk full preventing journal commit) and the user must intervene.
//
// Disposition: rare. The expected case is that os.Remove succeeds or
// returns ErrNotExist (treated as success — release-before-write
// idempotency). ErrFileDeletePersistent is the catch-all for the
// remaining permission/filesystem errors.
var ErrFileDeletePersistent = errors.New("cannot delete stale runtime file")
