//go:build darwin

package powerassert

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// Assertion is the active IOPMAssertion handle and implements
// state.Releaser (Release() error + Name() string). It is produced by
// Acquire and consumed by main.go's RestoreState LIFO Cleanup chain
// (push position between runtime-mock and controller).
//
// Two-layer idempotency (verbatim mirror of Phase 2
// cocoa.Controller.Release pattern — controller_darwin.go:273-305 —
// per CONTEXT D-12 + Phase 3 PATTERNS.md §16):
//
//  1. atomic.Bool fast-path CAS: the second concurrent Release call
//     short-circuits with nil without touching releaseFn.
//  2. sync.Once around the body: defense in depth in case a future
//     refactor breaks the atomic.Bool path.
//
// The id field stores the Go-level uint32 (not C.IOPMAssertionID) so
// pure-Go unit tests can construct an Assertion via newAssertionWithDeps
// without cgo (assertion_test.go uses this).
type Assertion struct {
	id   uint32
	name string
	log  *slog.Logger

	// releaseFn is the injected release primitive — production wires
	// releaseRaw (cgo IOPMAssertionRelease bridge from pm_darwin.go),
	// tests wire a fakeReleaser to count calls without IOKit.
	releaseFn func(uint32) error

	released    atomic.Bool
	releaseOnce sync.Once
}

// Acquire creates an IOPMAssertion of type
// kIOPMAssertPreventUserIdleSystemSleep with the given name.
// Implements.
//
// MUST be called AFTER orphan cleanup (Step 10 before Step 12) and
// BEFORE cocoa.Init / window creation (assertion is the cheapest
// resource on the stack, so it's acquired first to fail fast on IOKit
// errors before any AppKit objects exist).
//
// The returned *Assertion implements state.Releaser; main.go pushes it
// onto the RestoreState LIFO chain. Two-layer idempotent Release
// (atomic.Bool + sync.Once) per CONTEXT D-12.
//
// Logger fallback: nil → slog.Default() (mirrors state.NewRestoreState
// and cocoa.NewController convention).
func Acquire(name string, log *slog.Logger) (*Assertion, error) {
	if log == nil {
		log = slog.Default()
	}
	id, err := acquireRaw(name)
	if err != nil {
		return nil, err
	}
	return &Assertion{
		id:        id,
		name:      name,
		log:       log,
		releaseFn: releaseRaw,
	}, nil
}

// newAssertionWithDeps is the test-internal constructor that lets unit
// tests inject a fake releaseFn (counting fakeReleaser, error injection,
// etc.) without invoking the real IOKit syscall. NOT exported.
//
// Production callers MUST use Acquire instead. The seam exists for the
// same reason Phase 2 controller_darwin.go exposes newControllerWithDeps:
// the two-layer idempotency contract must be tested without a live cgo
// dependency so the suite stays fast and works under HEADLESS CI.
func newAssertionWithDeps(id uint32, name string, log *slog.Logger, releaseFn func(uint32) error) *Assertion {
	if log == nil {
		log = slog.Default()
	}
	return &Assertion{
		id:        id,
		name:      name,
		log:       log,
		releaseFn: releaseFn,
	}
}

// Name implements state.Releaser. Returns the name passed to Acquire
// (e.g. "dndmode active"). The Phase 3 acceptance test parses stderr
// "released releaser=dndmode active" to verify the push order
// (P2 contract: runtime-mock → assertion → controller → tap-mock).
func (a *Assertion) Name() string { return a.name }

// Release implements state.Releaser. Two-layer idempotency mirrors the
// Phase 2 cocoa.Controller.Release pattern (CONTEXT D-12):
//
//  1. atomic.Bool fast path (single CAS) — second-call no-op.
//  2. sync.Once around the body — defense in depth.
//
// Apple's IOPMLib.h documents kernel auto-cleanup on process exit
//, so even if Release is never called, the kernel
// releases our assertion when the process dies. The orphan-cleanup
// path defends against the SIGKILL-during-Cleanup case
// where the user's machine retains a dangling assertion. Idempotency
// here defends against the symmetric case: multiple Cleanup attempts
// invoked by overlapping signal handlers / deferred unwinds.
//
// Error propagation: first Release returns whatever releaseFn returns;
// subsequent Release calls return nil unconditionally (idempotency
// applies to errors too — see TestAssertion_Release_PropagatesError).
func (a *Assertion) Release() error {
	if !a.released.CompareAndSwap(false, true) {
		return nil // first idempotency layer
	}
	var releaseErr error
	a.releaseOnce.Do(func() {
		if a.releaseFn == nil {
			return
		}
		if err := a.releaseFn(a.id); err != nil {
			releaseErr = err
		}
	})
	return releaseErr
}

// Compile-time check: *Assertion satisfies state.Releaser without
// importing the state package (would create an import cycle —
// cmd/dndmode/main.go is the only caller that holds *Assertion as
// state.Releaser). Mismatch surfaces here at build time. Mirrors
// controller_darwin.go:310-313 verbatim.
var _ interface {
	Release() error
	Name() string
} = (*Assertion)(nil)
