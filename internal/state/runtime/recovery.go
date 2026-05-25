//go:build darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/dsbasko/dndmode/internal/macos/focus"
	"github.com/dsbasko/dndmode/internal/macos/powerassert"
)

// minValidPID is the smallest PID value RecoverFromCrash will accept for a
// liveness probe. PID 1 is launchd (never a dndmode parent),
// PID 0 is the kernel sentinel that POSIX `kill(0, sig)` interprets as
// "broadcast to the caller's process group" (returns nil success →
// spurious "alive" verdict), and negative PIDs are POSIX "kill every
// process the caller can signal" (same DoS surface). Anything below 2
// is rejected as suspect — a crafted or corrupted runtime.json with
// such a value otherwise permanently bricks dndmode into exit 5 with no
// way forward except manual `rm`.
const minValidPID = 2

// RecoverFromCrash reads runtime.json and reconciles the state left by
// a SIGKILL'd previous dndmode. Composes Manager.Read
// Phase 3 powerassert seams (AssertionReleaser, LiveChecker) + plan
// focus seam (ShortcutsRunner). NO new interfaces — this is pure
// orchestration.
//
// Flow:
//
//  1. mgr.Read → fs.ErrNotExist (happy path: no prior crash) → return nil.
//  2. mgr.Read → other error (malformed JSON, permission, etc.) →
//     log.Warn + best-effort mgr.Release + return nil (continue
//     PreFlight; corrupted state is not fatal).
//  3. live.IsAlive(snap.PID) == true → return wrapped ErrConcurrentInstance
// (main.go maps via errors.Is to exit code 5; matches Phase 3
//     bail policy).
//  4. Dead PID branch — three best-effort + one strict step:
//     (a) rel.Release(snap.AssertionID) — IOPMAssertion explicit-id
//         release. On err: log.Warn, continue (kernel auto-reaps on
//         process exit anyway; Phase 3 CleanupOrphans heuristic
//         remains as a fallback later in PreFlight at Step 12).
//     (b) runner.Run(ctx, "dndmode-off") — best-effort Focus
// deactivation. On err: log.Warn, continue per the design notes.
//     (c) mgr.Release() — MUST succeed. Failure → wrapped
//         ErrFileDeletePersistent (main.go maps via errors.Is to exit
// code 7); the design notes makes this distinct from the live-PID
//         exit so the user-facing stderr template can explain why
//         dndmode can't auto-recover (read-only filesystem, ACL deny,
//         disk full preventing journal commit — manual `rm` required).
//
// the design notes invariant: RecoverFromCrash runs BEFORE
// powerassert.CleanupOrphans in main.go PreFlight. Explicit-id release
// via stored assertion_id is more precise than the Phase 3
// name+type+dead-PID heuristic; CleanupOrphans remains for crashes
// that happened BEFORE Manager.Write fired (window between
// powerassert.Acquire at Step 13 and runtime.Manager.Write at
// Step 13.3).
//
// Logger fallback: nil → slog.Default() (mirrors CleanupOrphans /
// powerassert.Acquire convention).
func RecoverFromCrash(
	ctx context.Context,
	mgr *Manager,
	rel powerassert.AssertionReleaser,
	runner focus.ShortcutsRunner,
	live powerassert.LiveChecker,
	log *slog.Logger,
) error {
	if log == nil {
		log = slog.Default()
	}

	snap, err := mgr.Read()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Happy path: no prior crash.
			return nil
		}
		// Malformed / permission / IO: warn + best-effort remove + continue.
		log.Warn("malformed runtime.json, skipping recovery", slog.Any("err", err))
		if relErr := mgr.Release(); relErr != nil {
			log.Warn("recovery: best-effort runtime.json remove failed",
				slog.String("path", mgr.Path()),
				slog.Any("err", relErr))
		}
		return nil
	}

	// validate snap.PID before passing it to live.IsAlive. The
	// production kernLiveChecker uses kill(pid, 0) which has pathological
	// semantics for pid<=0 (process-group broadcast / "kill every process
	// I can") and for pid==os.Getpid() of the recovering process (trivially
	// "alive"). All three cases force exit code 5 every launch — a
	// permanent DoS achievable by anyone able to write
	// ~/.config/dndmode/runtime.json (or by a power-cycle during the first
	// ever Write leaving a zero-value snapshot on disk).
	//
	// Treat suspect PIDs as dead: log a warn, attempt the file delete
	// (so the next launch can recover), and continue PreFlight. We do NOT
	// dispatch on AssertionID release / Focus deactivate because the
	// snapshot is untrusted; the Phase 3 powerassert.CleanupOrphans
	// fallback at Step 11 still picks up any genuine orphan IOPM assertion
	// via the name+type+dead-PID heuristic.
	if snap.PID < minValidPID || snap.PID == os.Getpid() {
		log.Warn("recovery: refusing to dispatch on suspect PID; treating as dead",
			slog.Int("pid", snap.PID),
			slog.Int("own_pid", os.Getpid()))
		if relErr := mgr.Release(); relErr != nil {
			return fmt.Errorf("%w (%s): %w", ErrFileDeletePersistent, mgr.Path(), relErr)
		}
		return nil
	}

	if live.IsAlive(snap.PID) {
		// Another dndmode instance is alive — bail with wrapped sentinel
		// so main.go can map via errors.Is to exit 5.
		return fmt.Errorf("%w (PID=%d)", ErrConcurrentInstance, snap.PID)
	}

	// Dead-PID branch: three best-effort steps + one strict.
	if err := rel.Release(snap.AssertionID); err != nil {
		log.Warn("recovery: release stored assertion failed",
			slog.Int("id", int(snap.AssertionID)),
			slog.Int("pid", snap.PID),
			slog.Any("err", err))
	} else {
		log.Info("recovery: released orphan assertion",
			slog.Int("id", int(snap.AssertionID)),
			slog.Int("pid", snap.PID))
	}

	if err := runner.Run(ctx, "dndmode-off"); err != nil {
		log.Warn("recovery: focus deactivate failed", slog.Any("err", err))
	}

	if err := mgr.Release(); err != nil {
		// Strict: file MUST be deletable. Wrap as ErrFileDeletePersistent
		// so main.go can errors.Is → exit code 7 → stderr template that
		// names the absolute path and suggests `rm -f <path>`.
		return fmt.Errorf("%w (%s): %v", ErrFileDeletePersistent, mgr.Path(), err)
	}

	return nil
}
