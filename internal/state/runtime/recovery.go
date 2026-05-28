//go:build darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

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

// stalePIDWindow bounds how old a snapshot may be before a live-PID match
// is treated as a PID-recycling false positive rather than a real
// concurrent instance. macOS recycles PIDs aggressively
// (`kern.maxproc` ≈ 1024–4096), so a 24h-old snapshot whose stored PID
// happens to match a currently-live process is overwhelmingly likely to
// be the kernel having reassigned a dead dndmode's PID to an unrelated
// long-running process (Chrome renderer, Spotlight indexer, system
// daemon). Without this second-pass guard, RecoverFromCrash bails on PID
// match alone and the user gets a permanent exit-5 loop with no
// actionable recovery — exactly the failure mode exists to
// avoid. Recovery prefers proceeding (release the assertion, deactivate
// Focus, delete the file) over bricking. A snapshot with a zero or
// future StartedAt is treated as stale as well — both indicate a
// corrupt/truncated runtime.json and the recovery path is the safer
// fallback than exit 5.
const stalePIDWindow = 24 * time.Hour

// recoveryFocusTimeout bounds the best-effort `shortcuts run dndmode-off`
// subprocess used during recovery. Recovery runs on a FRESH
// ctx (not the caller's ctx) for the same reason focus.Releaser.Release
// builds a fresh ctx in the Cleanup chain: SIGINT arriving during
// PreFlight cancels the caller's ctx, and exec.CommandContext SIGKILLs
// the subprocess before it can call the Focus framework — leaving stale
// Focus On state from the previous crashed run. 5s mirrors
// focus.Releaser.timeout: Apple's shortcuts CLI usually returns under
// 1s; 5s gives generous headroom while keeping recovery time-bounded.
const recoveryFocusTimeout = 5 * time.Second

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
//
// ctx is retained in the signature for future caller-cancellation
// short-circuits (e.g. SIGINT while we're mid-Read of a large file on
// a slow disk) but is NOT propagated to the Focus deactivate subprocess
// see recoveryFocusTimeout for rationale. The underscore
// is conventional Go for "intentionally unused parameter held for API
// stability".
func RecoverFromCrash(
	_ context.Context,
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
		// PID-reuse race second-pass guard. kill(pid, 0) alone is
		// insufficient — on a workstation where the kernel recycles PIDs
		// aggressively, the dead dndmode's PID may have been reassigned to
		// any unrelated process (Chrome renderer, Spotlight indexer, etc.)
		// between the SIGKILL and this launch. If the snapshot is stale
		// (StartedAt > stalePIDWindow in the past, OR zero, OR in the
		// future — all indicating either a recycled PID or a corrupted
		// timestamp), prefer proceeding over bailing. A genuinely-live
		// concurrent dndmode will have a fresh StartedAt within the
		// window and will still hit the ErrConcurrentInstance branch.
		now := time.Now().UTC()
		stale := snap.StartedAt.IsZero() ||
			snap.StartedAt.After(now) ||
			now.Sub(snap.StartedAt) > stalePIDWindow
		if stale {
			log.Warn("recovery: live PID + stale snapshot, likely PID recycling; treating as dead",
				slog.Int("pid", snap.PID),
				slog.Time("started_at", snap.StartedAt),
				slog.Duration("age", now.Sub(snap.StartedAt)))
			// Fall through to the dead-PID branch below.
		} else {
			// Fresh snapshot + live PID — almost certainly a real concurrent
			// instance. Bail with wrapped sentinel so main.go can map via
			// errors.Is to exit 5.
			return fmt.Errorf("%w (PID=%d)", ErrConcurrentInstance, snap.PID)
		}
	}

	// Dead-PID branch: three best-effort steps + one strict.
	// skip the IOPMAssertionRelease call when AssertionID == 0
	// either Manager.Write never landed (crashed between
	// powerassert.Acquire and Manager.Write) or runtime.json is corrupted
	// to a zero-value Snapshot. In both cases the Phase 3
	// CleanupOrphans heuristic (name+type+dead-PID) at Step 11 in main.go
	// is the correct path for any genuine orphan assertion. Calling
	// IOPMAssertionRelease(0) is wasted work at best ("recovery: released
	// orphan assertion id=0" is misleading — no orphan was actually
	// released) and IOKit log spam at worst.
	if snap.AssertionID == 0 {
		log.Warn("recovery: no assertion id stored, skipping IOPMAssertion release (Phase 3 CleanupOrphans is the fallback)",
			slog.Int("pid", snap.PID))
	} else if err := rel.Release(snap.AssertionID); err != nil {
		log.Warn("recovery: release stored assertion failed",
			slog.Int("id", int(snap.AssertionID)),
			slog.Int("pid", snap.PID),
			slog.Any("err", err))
	} else {
		log.Info("recovery: released orphan assertion",
			slog.Int("id", int(snap.AssertionID)),
			slog.Int("pid", snap.PID))
	}

	// build a FRESH ctx for runner.Run — the caller's ctx may
	// already be cancelled if SIGINT arrived during PreFlight, and a
	// cancelled ctx SIGKILLs the shortcuts subprocess before it can call
	// into the Focus framework, leaving stale Focus On state. Mirrors
	// focus.Releaser.Release (releaser.go:112) which addresses the
	// symmetric Cleanup-time scenario for the same reason.
	focusCtx, cancel := context.WithTimeout(context.Background(), recoveryFocusTimeout)
	if err := runner.Run(focusCtx, "dndmode-off"); err != nil {
		log.Warn("recovery: focus deactivate failed", slog.Any("err", err))
	}
	cancel()

	if err := mgr.Release(); err != nil {
		// Strict: file MUST be deletable. Wrap as ErrFileDeletePersistent
		// so main.go can errors.Is → exit code 7 → stderr template that
		// names the absolute path and suggests `rm -f <path>`.
		// %w for both wrap operands (Go 1.20+ multi-%w) so callers
		// can errors.Is(err, fs.ErrPermission) etc. to discriminate the
		// underlying filesystem failure subtype.
		return fmt.Errorf("%w (%s): %w", ErrFileDeletePersistent, mgr.Path(), err)
	}

	return nil
}
