//go:build darwin

package powerassert

//go:generate mockgen -source=orphan.go -destination=mocks/orphan.go -package=mocks

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// Orphan describes a single IOPMAssertion eligible for cleanup. Returned by
// AssertionEnumerator.Enumerate as parallel (PID, ID) tuples — PID identifies
// the *creator* process (used for liveness probe in), ID is the
// IOPMAssertionID handle the C side surfaces from
// `IOPMCopyAssertionsByProcess` (key "AssertionId" — empirically verified in
// see design notes "Issue 1").
//
// The struct is exported so the package's unit-test suite (orphan_test.go,
// black-box `package powerassert_test`) can construct fixtures via
// generated mocks.
type Orphan struct {
	// PID is the PID of the process that originally created the
	// IOPMAssertion (the "creator PID" in IOPMCopyAssertionsByProcess
	// terms). Used by liveChecker.IsAlive to decide release vs bail.
	PID int
	// ID is the IOPMAssertionID handle. Pass to AssertionReleaser.Release
	// to invoke `IOPMAssertionRelease` on the underlying assertion.
	ID uint32
}

// AssertionEnumerator abstracts `IOPMCopyAssertionsByProcess` + filter for
// unit-test injection. Production impl (cgoEnumerator) wraps
// enumerateMatching from pm_darwin.go which calls the IOKit C API; tests
// inject a gomock-generated MockAssertionEnumerator to drive deterministic
// scenarios (empty enum, single dead orphan, mix of live + dead, enumerate
// failure).
//
// Mirrors the three-interface DI pattern from cocoa.Controller
// (controller_darwin.go lines 17-50): screenEnumerator / windowFactory /
// observerRegistrar — same shape, different framework.
type AssertionEnumerator interface {
	// Enumerate returns the assertions held by *other* processes whose
	// AssertionName == wantName AND (wantType == "" OR AssertionType ==
	// wantType). An empty wantType means "match any type" — used by
	// CleanupOrphans because the assertion type is now runtime-selected
	// (display- vs system-sleep) and an orphan may be of either type.
	// Excludes ownPID — pass os.Getpid() to avoid catching our own
	// freshly-created assertion on re-entry. Returns a wrapped C-IOReturn
	// error on IOPMCopyAssertionsByProcess failure; nil + empty slice on the
	// happy path "no matching orphans".
	Enumerate(wantName, wantType string, ownPID int) ([]Orphan, error)
}

// AssertionReleaser abstracts `IOPMAssertionRelease` for unit-test
// injection. Production impl (cgoReleaser) wraps releaseRaw from
// pm_darwin.go; tests inject a gomock-generated MockAssertionReleaser to
// count calls and inject failures (warn+continue path).
type AssertionReleaser interface {
	// Release invokes IOPMAssertionRelease on the given assertion ID.
	// Returns a wrapped C-IOReturn error on failure; nil on success.
	Release(id uint32) error
}

// LiveChecker abstracts `syscall.Kill(pid, 0)` POSIX liveness probe for
// unit-test injection. Production impl (kernLiveChecker) wraps syscall.Kill;
// tests inject a gomock-generated MockLiveChecker to deterministically
// drive live vs dead branches without spawning real subprocesses.
//
// POSIX `kill(2)` semantics for sig=0 are the canonical liveness probe:
// no signal is delivered, but errno is set as if the signal would have
// been delivered. nil/EPERM → alive; ESRCH → dead.
type LiveChecker interface {
	// IsAlive returns true if the process PID is alive OR exists but
	// owned by another user (EPERM — conservative). Returns false
	// only on ESRCH ("no such process") — that's the orphan signal.
	IsAlive(pid int) bool
}

// cgoEnumerator is the production AssertionEnumerator backed by
// enumerateMatching (pm_darwin.go → C powerassert_enumerate_matching).
type cgoEnumerator struct{}

// Enumerate wraps enumerateMatching, converting the parallel (pids, ids)
// slices into []Orphan tuples. Empty result is normal (no orphans = happy
// path); err is propagated unchanged so the caller can wrap with
// "enumerate assertions:" prefix.
func (cgoEnumerator) Enumerate(wantName, wantType string, ownPID int) ([]Orphan, error) {
	pids, ids, err := enumerateMatching(wantName, wantType, ownPID)
	if err != nil {
		return nil, err
	}
	orphans := make([]Orphan, 0, len(pids))
	for i := range pids {
		orphans = append(orphans, Orphan{PID: pids[i], ID: ids[i]})
	}
	return orphans, nil
}

// cgoReleaser is the production AssertionReleaser backed by releaseRaw
// (pm_darwin.go → C powerassert_release → IOPMAssertionRelease).
type cgoReleaser struct{}

// Release wraps releaseRaw. The Go-level idempotency guard lives on
// *Assertion (atomic.Bool + sync.Once in assertion.go); the orphan path
// never re-releases — a single Release attempt per orphan ID is all we
// need (warn+continue on failure, no retry).
func (cgoReleaser) Release(id uint32) error { return releaseRaw(id) }

// kernLiveChecker is the production LiveChecker backed by syscall.Kill.
type kernLiveChecker struct{}

// IsAlive performs the POSIX `kill(pid, 0)` liveness probe.
//
//   - nil  → alive (process exists, signal would have been delivered; our
// user or sufficient permission). Treat as alive (bail).
// - ESRCH → "no such process". DEAD — orphan eligible for release.
//   - EPERM → process exists but we lack permission (other user / sandbox).
// Conservative: treat as ALIVE — NEVER release an assertion we
//     are not certain is orphaned.
//   - Any other errno → conservative: alive (never release on uncertain).
//
// Zombie process note: kill(zombie_pid, 0) returns success (nil) — zombie
// still has a PID, just no resources. That is correct for us — zombies
// cannot hold IOPMAssertions (kernel auto-cleans on zombie reap), so
// hitting this branch in practice means a same-PID-recycled live process
// which is exactly the "alive — bail" case we want.
func (kernLiveChecker) IsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	// EPERM or any other errno → conservative: alive (never release on
	// uncertain — explicit policy "all three must match").
	return true
}

// NewCgoEnumerator returns the production AssertionEnumerator wrapping
// enumerateMatching (IOKit `IOPMCopyAssertionsByProcess`). Called from
// cmd/dndmode/main.go Step 10 to construct the dependency
// passed into CleanupOrphans.
func NewCgoEnumerator() AssertionEnumerator { return cgoEnumerator{} }

// NewCgoReleaser returns the production AssertionReleaser wrapping
// releaseRaw (IOKit `IOPMAssertionRelease`). Called from main.go Step 10
// alongside NewCgoEnumerator + NewKernLiveChecker.
func NewCgoReleaser() AssertionReleaser { return cgoReleaser{} }

// NewKernLiveChecker returns the production LiveChecker wrapping POSIX
// `syscall.Kill(pid, 0)` for liveness probing. Used by CleanupOrphans to
// distinguish dead orphans (ESRCH → release) from live concurrent
// instances (nil/EPERM → ErrConcurrentInstance bail).
func NewKernLiveChecker() LiveChecker { return kernLiveChecker{} }

// assertionName is the canonical AssertionName string used by both
// Acquire (assertion.go) and CleanupOrphans (orphan filter). MUST match
// the name passed to IOPMAssertionCreateWithName in production — keep in
// sync with cmd/dndmode/main.go Step 12.
//
// Kept unexported: main.go does not need direct access — it calls
// CleanupOrphans which uses the const internally and passes the name to
// Acquire via the explicit "dndmode active" literal (single source of
// truth lives in main.go's call site).
const (
	assertionName = "dndmode active"
	// assertionTypeAny is the empty type sentinel passed to Enumerate by
	// CleanupOrphans so the orphan match is TYPE-AGNOSTIC. The assertion
	// type is now runtime-selected (kIOPMAssertPreventUserIdleDisplaySleep
	// by default vs kIOPMAssertPreventUserIdleSystemSleep for
	// allow_display_sleep:true), so an orphan left by a prior run may be of
	// EITHER type. Matching keys on the unique name "dndmode active" alone;
	// the C-side pa_dict_has_match treats an empty want_type as "match any
	// type". (Previously this was a concrete "PreventUserIdleSystemSleep"
	// literal, valid only while the type was hard-coded.)
	assertionTypeAny = ""
)

// CleanupOrphans implements + Phase 3 decisions (triple
// identification heuristic), (live-PID bail with
// ErrConcurrentInstance), (warn+continue on Release failure).
//
// Call order: cmd/dndmode/main.go Step 10 — BEFORE powerassert.Acquire
// (Step 12). The own_pid passed to Enumerate is `os.Getpid()`, which
// excludes our own freshly-created assertion (Step 12 has not run yet,
// so the exclusion is conservative — covers the warm-restart corner
// case where a previous run from the same process somehow left an
// assertion record).
//
// Type-agnostic match: the assertion type is now runtime-selected
// (kIOPMAssertPreventUserIdleDisplaySleep by default vs
// kIOPMAssertPreventUserIdleSystemSleep for allow_display_sleep:true), so
// an orphan from a prior run may carry either type. Enumerate is called
// with the empty assertionTypeAny — matching keys on the unique name
// "dndmode active" and accepts any type.
//
// Flow (per orphan, in enumeration order):
//
//  1. live.IsAlive(o.PID)  → return wrapped ErrConcurrentInstance with
// PID, *short-circuit* — do NOT process remaining orphans (
//     conservative — never release while another instance might be
//     using one of the matching assertions).
//
//  2. rel.Release(o.ID) succeeds → log.Info "released orphan assertion".
//
//  3. rel.Release(o.ID) fails → log.Warn "release orphan failed" and
// CONTINUE to the next orphan ("MacBook not protected" is
//     worse than "one extra row in pmset"; never fail PreFlight on
//     transient IOKit errors).
//
// Logger fallback: nil → slog.Default() (mirrors NewController and
// state.NewRestoreState convention from Phase 1/2).
//
// Returns:
//   - nil on the happy path (zero orphans, or all matching orphans
//     successfully released, or some failed-to-release but warn'd).
//   - "enumerate assertions: %w" wrapping the underlying IOReturn-coded
//     error when AssertionEnumerator.Enumerate fails (caller maps to a
//     non-zero exit code; main.go default is exit 1).
//   - "ErrConcurrentInstance: PID=N" wrapping ErrConcurrentInstance when
//     a live PID match is found (main.go maps via errors.Is to exit
//     code 5).
func CleanupOrphans(
	enum AssertionEnumerator,
	rel AssertionReleaser,
	live LiveChecker,
	log *slog.Logger,
) error {
	if log == nil {
		log = slog.Default()
	}
	own := os.Getpid()
	// Type-agnostic match: pass the empty assertionTypeAny so an orphan of
	// EITHER runtime-selected type (display- vs system-sleep) is caught by
	// its unique name. The C-side pa_dict_has_match treats "" as match-any.
	orphans, err := enum.Enumerate(assertionName, assertionTypeAny, own)
	if err != nil {
		return fmt.Errorf("enumerate assertions: %w", err)
	}
	for _, o := range orphans {
		if live.IsAlive(o.PID) {
			// another dndmode instance is alive — bail with
			// wrapped sentinel so main.go can map via errors.Is to
			// exit code 5.
			return fmt.Errorf("%w: PID=%d", ErrConcurrentInstance, o.PID)
		}
		// dead PID + name+type match = orphan, release.
		if err := rel.Release(o.ID); err != nil {
			// warn + continue. NEVER fail PreFlight on a
			// transient IOKit release error — kernel will likely
			// reap on its own in macOS 14+ anyway.
			log.Warn("release orphan failed",
				slog.Int("id", int(o.ID)),
				slog.Int("pid", o.PID),
				slog.Any("err", err))
			continue
		}
		log.Info("released orphan assertion",
			slog.Int("pid", o.PID),
			slog.Int("id", int(o.ID)))
	}
	return nil
}
