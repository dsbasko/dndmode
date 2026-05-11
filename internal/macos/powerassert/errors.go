//go:build darwin

package powerassert

import "errors"

// ErrConcurrentInstance is returned by CleanupOrphans when an
// IOPMAssertion matching our Name+Type is held by a process whose PID is
// still alive — i.e. another dndmode instance is already running and has
// acquired the awake-lock (Phase 3).
//
// cmd/dndmode/main.go dispatches on this sentinel via
// errors.Is(err, powerassert.ErrConcurrentInstance) and maps it to exit
// code 5 (exitConcurrentInstance), printing a user-facing stderr message
// suggesting `kill -INT <pid>` or waiting for the other instance to exit.
//
// The sentinel is exported here (rather than inside orphan.go in plan
//) so dependent packages — including this plan's smoke test and
// the eventual main.go wire-up can reference it without
// waiting for the orphan-cleanup implementation to land.
var ErrConcurrentInstance = errors.New("another dndmode instance is holding the awake-lock")
