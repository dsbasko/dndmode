//go:build darwin

// Package powerassert wraps the IOKit IOPMAssertion API for the dndmode
// awake-lock lifecycle. +.. are implemented here.
//
// The package exposes a single long-lived resource — an
// IOPMAssertionCreateWithName assertion whose type is selected at runtime
// (inverted polarity): kIOPMAssertPreventUserIdleDisplaySleep by default
// (display kept awake), or the legacy kIOPMAssertPreventUserIdleSystemSleep
// when allow_display_sleep:true (display may idle-off) — and an
// orphan-cleanup primitive for releasing assertions whose creator process
// has died (typical case: previous dndmode instance was killed with SIGKILL
// before its LIFO Cleanup chain could fire).
//
// The package layout mirrors the Phase 2 cocoa pattern (one package per
// Apple framework family). powerassert holds the IOKit + CoreFoundation
// bindings; permissions (sibling package) holds the Foundation /
// ApplicationServices / Carbon bindings. The split keeps `-framework`
// LDFLAGS minimal per cgo unit and isolates pure-C from the Obj-C
// runtime.
//
// # Threading invariants
//
//   - Acquire and Release are thread-safe at the Go level; the underlying
//     IOPMAssertion API is also thread-safe per Apple's IOPMLib.h.
// - CleanupOrphans is called exactly once during PreFlight
// before Acquire (Step 10 → Step 12).
//   - Assertion embeds two-layer idempotency (atomic.Bool fast-path +
//     sync.Once defense in depth) so concurrent Release callers see a
//     single underlying IOPMAssertionRelease call.
//
// # State.Releaser conformance
//
// *Assertion satisfies state.Releaser (Release() error + Name() string)
// without importing the state package, avoiding an import cycle. The
// satisfaction is enforced at compile time inside assertion.go via a
// blank-identifier interface assignment.
//
// # Sources
//
// - the design notes (8)
// - the design notes (..)
// - the design notes
//   - Apple — IOPMLib.h (verified against phracker/MacOSX-SDKs)
package powerassert
