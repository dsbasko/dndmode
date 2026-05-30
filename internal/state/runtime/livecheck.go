//go:build darwin

package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/dsbasko/dndmode/internal/macos/powerassert"
)

// IsLiveInstance reports whether another live dndmode instance owns
// ~/.config/dndmode/runtime.json (cold-start gate, called from
// cmd/dndmode/main.go Step 5c — between config validation and the
// platform check; well before any TCC prompts or IOKit acquires).
//
// Returns:
//
//   - (false, 0,   nil) — no runtime.json on disk (happy cold start).
//   - (true,  pid, nil) — runtime.json exists, snap.PID is alive per
//     the injected LiveChecker (Phase 3 POSIX kill(pid, 0) seam).
// Caller MUST emit the stderr template and return exit 5
// (exitConcurrentInstance — same code as Phase 3 Phase 5).
//   - (false, pid, nil) — runtime.json exists but snap.PID is DEAD.
//     Caller falls through to Step 10.5 RecoverFromCrash, which owns
//     the dead-PID resource-release flow (assertion explicit-id release,
// file delete, Focus deactivate). returns this triple so the
//     caller can log debug context but takes no action itself.
//   - (false, 0,   non-nil) — read failure (malformed JSON, permission
//     denied, IO error). Caller MUST log warn and continue — Step 10.5
//     RecoverFromCrash will surface persistent IO/permission errors
//     with a stronger sentinel (ErrFileDeletePersistent → exit 7).
// deliberately stays warn-not-fatal: a corrupt file left by
// a crashed predecessor is recovery's domain, not 's.
//
// Defensive: a snapshot with PID <= 0 (corrupted/missing field) is
// treated as not-alive, with a warn log — IsAlive is NOT invoked.
// POSIX kill(0, sig) means "broadcast to caller's process group"
// (returns nil success → spurious "alive" verdict), and negative PIDs
// mean "every process the caller can signal" (DoS surface). Phase 5
// RecoverFromCrash has a stricter minValidPID=2 guard for the recovery
// path; only needs the lighter <=0 short-circuit because
// recovery handles the rest at Step 10.5.
//
// What explicitly does NOT do (separation of concerns vs
// RecoverFromCrash):
//
//   - Never deletes runtime.json (deletion is recovery's domain).
//   - Never calls mgr.Release() (release is recovery's domain).
//   - Never releases any IOPMAssertion (powerassert.CleanupOrphans
//     and recovery share that responsibility).
//   - Never propagates a sentinel error — read failures are returned
//     as wrapped fmt.Errorf so the caller can log without dispatch.
//     This is the ONE PreFlight step in main.go that does not use
// errors.Is sentinel dispatch (per the design notes the design notes Pattern
//     S4 deviation).
//
// Reuses Phase 5 Manager.Read (stateless, sync, bounded) + Phase 3
// powerassert.LiveChecker. No new abstractions.
//
// Logger fallback: nil → slog.Default() (same convention as recovery.go
// and powerassert.Acquire). Caller in main.go passes the structured
// stderr logger.
func IsLiveInstance(mgr *Manager, live powerassert.LiveChecker, log *slog.Logger) (alive bool, pid int, err error) {
	if log == nil {
		log = slog.Default()
	}

	snap, err := mgr.Read()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Happy path: no prior runtime.json → no live peer.
			return false, 0, nil
		}
		// Malformed JSON, permission denied, IO. Surface to caller for
		// warn-log; RecoverFromCrash at Step 10.5 handles deeper cleanup.
		return false, 0, fmt.Errorf("read runtime.json: %w", err)
	}

	if snap.PID <= 0 {
		// Corrupted snapshot (no PID, or negative). Skip IsAlive — POSIX
		// kill(0/-N, sig) has confusing semantics; treat as not-alive.
		log.Warn("snapshot has invalid PID", slog.Int("pid", snap.PID))
		return false, 0, nil
	}

	if !live.IsAlive(snap.PID) {
		// Dead PID. Caller falls through; recovery (Step 10.5) cleans up.
		return false, snap.PID, nil
	}

	// Live peer. Caller bails with exit 5.
	return true, snap.PID, nil
}
