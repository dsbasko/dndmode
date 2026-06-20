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
// Two-layer idempotency (post- fix, replaces the
// pre-fix atomic.Bool + one-shot guard which had a serialization race):
//
//  1. atomic.Bool fast-path: AFTER the first caller has fully completed
//     releaseFn and stored released=true, subsequent calls observe
//     released==true without acquiring the mutex — cheap short-circuit.
//  2. sync.Mutex slow-path: concurrent callers entering before the first
//     has finished BLOCK on mu.Lock() until releaseFn returns. Under the
//     mutex they double-check released — if true, they return nil
//     without invoking releaseFn; if false (genuinely the first caller),
//     they invoke releaseFn and store released=true.
//
// Key contract change vs pre-fix: a concurrent caller no longer returns
// before releaseFn completes. This matters when the Cleanup chain in
// main.go retries on partial failures — the caller now reliably knows
// "the underlying release primitive is done" when Release returns,
// regardless of which goroutine won the race.
//
// Error contract preserved: the first caller (the one whose releaseFn
// invocation succeeded or failed) returns whatever releaseFn returned;
// concurrent callers return nil unconditionally (idempotency applies to
// errors — see TestAssertion_Release_PropagatesError).
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

	// released is the fast-path hint — set to true AFTER releaseFn has
	// fully completed under mu. atomic.Load lets repeat callers avoid
	// the mutex entirely once the operation is permanently done.
	released atomic.Bool

	// mu serializes concurrent Release callers. Pre- the design
	// relied on a one-shot Do guard for serialization, but the
	// atomic.Bool was flipped BEFORE entering the guard body, so
	// concurrent callers short-circuited to nil while the winner was
	// still inside releaseFn (the pre-fix bug). A plain Mutex is the
	// simplest fix: every caller takes the lock, double-checks the
	// hint flag, and only the actual winner reaches releaseFn.
	mu sync.Mutex
}

// Acquire creates an IOPMAssertion with the given name, selecting its type
// at runtime from allowDisplaySleep (inverted polarity). Implements
//.
//
//   - allowDisplaySleep == false (default / config key absent): the
//     assertion is kIOPMAssertPreventUserIdleDisplaySleep — the display is
//     kept awake (and the system stays awake as a side effect), so the
//     external monitor does NOT idle-off while the operator is away.
//   - allowDisplaySleep == true: the assertion is the legacy
//     kIOPMAssertPreventUserIdleSystemSleep — only system idle-sleep is
//     blocked, the display may idle-off.
//
// MUST be called AFTER orphan cleanup (Step 10 before Step 12) and
// BEFORE cocoa.Init / window creation (assertion is the cheapest
// resource on the stack, so it's acquired first to fail fast on IOKit
// errors before any AppKit objects exist).
//
// The returned *Assertion implements state.Releaser; main.go pushes it
// onto the RestoreState LIFO chain. Two-layer idempotent Release
// (atomic.Bool fast-path + sync.Mutex slow-path) per the design notes +.
//
// Logger fallback: nil → slog.Default() (mirrors state.NewRestoreState
// and cocoa.NewController convention).
func Acquire(name string, allowDisplaySleep bool, log *slog.Logger) (*Assertion, error) {
	if log == nil {
		log = slog.Default()
	}
	id, err := acquireRaw(name, allowDisplaySleep)
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

// ID returns the underlying IOPMAssertionID (uint32). Used by
// internal/state/runtime/manager.go to record the id in runtime.json
// so a crashed dndmode's orphan can be released by exact id during
// next-launch recovery (Phase 5), instead of relying on
// the Phase 3 name+type+dead-PID heuristic. Read-only — the value
// is set at Acquire and never mutated.
func (a *Assertion) ID() uint32 { return a.id }

// Release implements state.Releaser. Two-layer idempotency (fix,
//):
//
//  1. atomic.Bool fast-path Load — once released is durably true, any
//     repeat caller returns nil instantly without touching the mutex.
//     This optimizes the common "Cleanup already finished, defer chain
//     touches us again" case.
//
//  2. sync.Mutex slow-path — concurrent first-time callers serialize
//     here. The winner double-checks released under the mutex (covers
//     the case where the fast-path Load happened before another
//     goroutine had stored), invokes releaseFn, stores released=true,
//     and releases mu. Losers block on mu.Lock() until the winner is
//     done, then see released==true under mu and return nil without
//     invoking releaseFn.
//
// Key contract (vs pre- CAS + one-shot guard): concurrent callers
// NO LONGER return before releaseFn completes. The IOKit release is
// genuinely done by the time ANY caller returns.
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
	// Fast path: hint flag. Cheap Load — once released is durably set
	// (after the winner stored it under mu), any repeat caller skips
	// the mutex entirely.
	if a.released.Load() {
		return nil
	}
	// Slow path: serialize concurrent first-time callers via the mutex.
	a.mu.Lock()
	defer a.mu.Unlock()
	// Double-check under the mutex — another goroutine may have won
	// between our Load and our Lock.
	if a.released.Load() {
		return nil
	}
	var releaseErr error
	if a.releaseFn != nil {
		releaseErr = a.releaseFn(a.id)
	}
	// Store AFTER releaseFn completes. Concurrent callers blocked on
	// mu.Lock will see released=true under mu and short-circuit; new
	// callers using the fast-path Load see the same after the Unlock
	// has happens-before published the Store.
	a.released.Store(true)
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
