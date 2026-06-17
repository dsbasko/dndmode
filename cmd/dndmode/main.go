//go:build darwin

// Command dndmode is a macOS Apple-Silicon CLI utility that locks the
// keyboard/trackpad behind a configurable hotkey while keeping the system
// awake (Phase 4-5 functionality). Phase 2 adds the per-screen overlay:
// cocoa.Init creates NSApp + screen observers, controller creates one
// black NSWindow per display on CGShieldingWindowLevel, and the main
// goroutine blocks on cocoa.RunApp(ctx) — the [NSApp run] loop — until
// ctx.Cancel triggers a synthetic stop event.
//
// Input is intentionally NOT blocked in Phase 2 (Cmd+Tab still works);
// CGEventTap arrives in Phase 4. PreFlight permission checks arrive in
// Phase 3. This is the visual-only milestone for AppKit/Cocoa debugging.
package main

import (
	// runtimepin's init() calls runtime.LockOSThread() to pin main goroutine
	// to OS thread #0 (m0). MUST be the first import — cocoa.Init / RunApp /
	// NSWindow ops depend on the invariant that main lives on m0.
	_ "github.com/dsbasko/dndmode/internal/runtimepin"

	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/cocoa"
	"github.com/dsbasko/dndmode/internal/macos/eventtap"
	"github.com/dsbasko/dndmode/internal/macos/focus"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
	"github.com/dsbasko/dndmode/internal/macos/powerassert"
	"github.com/dsbasko/dndmode/internal/state"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
	"github.com/dsbasko/dndmode/internal/supervisor"
)

const (
	// configRelPath is the user-relative config path. POSIX-stable via
	// os.UserHomeDir (mitigation).
	configRelPath = ".config/dndmode/config.yml"

	// Granular exit codes per the design notes (Phase 3 expansion). Slots 6/7
	// are Phase 5 (Shortcuts missing / runtime JSON
	// non-recoverable); slot 8 added by Phase 4 for top-level
	// panic recovery (per the design notes the design notes
	// originally suggested code 7, but exitRuntimeJSON owns that
	// slot in Phase 5, so the next-available 8 is the minimum-regression
	// resolution). Stderr wording per the UI spec Copywriting Contract.
	exitOK                  = 0 // success
	exitConfigErr           = 1 // P1 — bad YAML, modifier-only hotkey
	exitPlatformErr         = 2 // non-arm64, macOS < 14, IOKit fundamentals
	exitPermissionDenied    = 3 // SIGINT in polling loop
	exitSecureInputConflict = 4 // SecureEventInput active
	exitConcurrentInstance  = 5 // live-PID match on orphan IOPMAssertion
	exitFocusSetup          = 6 // Phase 5 required Shortcuts not found
	exitRuntimeJSON         = 7 // Phase 5 cannot delete stale runtime file
	exitInternalErr         = 8 // Phase 4 top-level recover() in run() (the design notes)
)

// cancelStopper adapts context.CancelFunc to the supervisor.Stopper
// interface. RequestStop is idempotent at the Stopper-call level (sync.Once
// in supervisor.fireStop guarantees single invocation), and ctx.cancel
// itself is idempotent.
type cancelStopper struct {
	cancel context.CancelFunc
}

func (c *cancelStopper) RequestStop(_ string) { c.cancel() }

func main() {
	os.Exit(run())
}

// run is the testable entry point (main calls os.Exit(run())). Acceptance
// tests fork the binary as a subprocess; unit-level testing of run()
// directly is not done because too much main-thread Cocoa state crosses
// boundaries.
//
// Step ordering (Phase 3 —..,; verbatim per the design notes):
//
// 1. Parse --debug flag (P1).
// 2. slog logger to stderr (P1 /).
// 3. signal.NotifyContext(ctx, SIGINT, SIGTERM, SIGHUP) (replaces the
//     plain-context cancel pattern used in Phase 2; SIGINT/SIGTERM/SIGHUP now
//     flow into ctx.Done directly).
//  4. RestoreState + defer Cleanup + stdout "cleaning up… done." banner (P1).
//  5. Resolve home dir.
// 6. Load config + parse hotkey.
//  7. stdout config banner.
// 8. permissions.CheckPlatform —; exit 2 on ErrNonArm64/ErrMacOSBelow14.
// 8.5. runtimepkg.IsLiveInstance (Phase 6, ordering) — cold-start
//     single-instance gate; alive peer → stderr template + exit 5 (mirrors Step
//     10.5 ErrConcurrentInstance dispatch). Runs AFTER platform check so
//     cross-arch / pre-Sonoma users surface exit 2 first; runtimeMgr is reused
// at Steps 10.5, 12, 13.3 (single Manager invariant per Phase 5).
// 9. permissions.WaitForGrants — polling; ctx.Canceled → exit 3.
// 9.5. focus.CheckShortcuts —..; ErrShortcutsMissing → exit 6.
// 10. permissions.IsSecureEventInputActive —; true → exit 4.
// 10.5. runtimepkg.RecoverFromCrash —..; live-PID → exit 5;
//     ErrFileDeletePersistent → exit 7; other → exit 2.
// 11. powerassert.CleanupOrphans —; ErrConcurrentInstance → exit 5.
//  12. rs.Push(runtimepkg.NewManager(...)) — Phase 5 replaces mock-runtime-file.
// 13. powerassert.Acquire("dndmode active") —; rs.Push(assertion).
//     13.3. runtimepkg.Manager.Write({pid, started_at, prior_focus=nil, assertion_id})
// atomic temp+rename per the design notes.
// 13.7. focus.Activate (warn on fail per) + rs.Push(focus.NewReleaser(...))
// slot #4 in LIFO.
// 14. cocoa.Init — (moved DOWN after permission checks).
// 15. cocoa.NewController CreateWindowsForAllScreens — P2; rs.Push(controller).
//  16. supervisor.New(log, stopper) + supervisor.Start(ctx) — swapped BEFORE
// eventtap (Phase 4) because InstallAll needs sup.ExitTrigger() as
//      its sink channel.
// 17. eventtap.InstallAll(spec, sup.ExitTrigger(), log) — Phase 4;
//      composes tap + watchdog + wake observer into a single Releaser;
//      rs.Push(tapRel). Replaces the Phase 3 mock-tap placeholder.
//  18. stdout "dndmode: active. press Ctrl-C.".
// 19. cocoa.RunApp(ctx) → sup.Wait() → return exitOK; defer Cleanup LIFO;
// top-level recover defer (registered FIRST inside run, Phase 4
//) catches any Cocoa-panic AFTER Cleanup, exits with exitInternalErr=8.
//
// cleanup execution order via push-stack LIFO unwind (Phase 4
// finalises tap-releaser): eventtap → windows → "dndmode active" → focus
// → runtime-file.
func run() int {
	// --- (Phase 4 the design notes): top-level recover defer ---
	// Registered FIRST inside `run()` so Go's LIFO defer unwind runs this
	// defer LAST — AFTER `defer stop()` (Step 3 signal.NotifyContext) and
	// AFTER `defer func() { rs.Cleanup(); ... }` (Step 3 RestoreState).
	// By the time `recover()` here returns non-nil:
	//   - rs.Cleanup has already drained the LIFO release stack (overlay
	//     hidden, IOPMAssertion released, runtime.json removed, Focus
	//     deactivated, eventtap released);
	//   - signal handlers are unregistered;
	//   - stdout cleanup banner already printed.
	// All that remains is the os.Exit with a distinct code so subprocess
	// acceptance tests (TestAcceptance_LIFE10_PanicRecover, added by plan
	//) can assert the panic-vs-normal path. debug.Stack() is
	// captured BEFORE Exit so the operator sees a faithful trace.
	//
	// Exit code = exitInternalErr (8). the design notes originally suggested
	// 7, but Phase 5 already owns slot 7 (exitRuntimeJSON /). 8 is
	// the minimum-regression resolution per the design notes.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "dndmode: PANIC: %v\n%s\n", r, debug.Stack())
			os.Exit(exitInternalErr)
		}
	}()

	// --- Step 1: Parse flags (stdlib flag) ---
	debugFlag := flag.Bool("debug", false, "enable debug-level logging on stderr")
	flag.Parse()

	// --- Step 2: slog logger to stderr ---
	level := slog.LevelInfo
	if *debugFlag {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// --- Step 3: RestoreState + deferred Cleanup with stdout banner ---
	rs := state.NewRestoreState(log)
	defer func() {
		if err := rs.Cleanup(); err != nil {
			log.Error("cleanup returned aggregated error", slog.Any("err", err))
		}
		// Single user-facing line on stdout. Printed AFTER Cleanup
		// completes — proof that defer chain ran.
		fmt.Fprintln(os.Stdout, "dndmode: cleaning up… done.")
	}()

	// --- Step 4: Resolve config path (mitigation) ---
	home, err := os.UserHomeDir()
	if err != nil {
		log.Error("resolve home dir", slog.Any("err", err))
		return exitPlatformErr
	}
	cfgPath := filepath.Join(home, configRelPath)

	// --- Step 5: Load config ---
	loader := config.NewLoader(cfgPath)
	cfg, created, err := loader.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitConfigErr
	}

	// --- Step 5b: Validate hotkey grammar ---
	if _, err := hotkey.Parse(cfg.Hotkey); err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: invalid hotkey %q: %v. Fix the hotkey grammar in ~/.config/dndmode/config.yml.\n", cfg.Hotkey, err)
		return exitConfigErr
	}

	// --- Step 5b.1: Validate overlay_style (QUICK-gh8, T-gh8-01) ---
	// yaml.Strict() rejects unknown KEYS but not unknown VALUES, so a junk
	// overlay_style value parses fine — value-validate it HERE, before any
	// window is created. overlayStyle (normalized: ""=>"black") is declared in
	// this scope so it is still visible at Step 15 (passed into NewController).
	if err := config.ValidateOverlayStyle(cfg.OverlayStyle); err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: invalid overlay_style %q: %v. Fix overlay_style in ~/.config/dndmode/config.yml (valid: black, matrix).\n", cfg.OverlayStyle, err)
		return exitConfigErr
	}
	overlayStyle := config.NormalizeOverlayStyle(cfg.OverlayStyle)

	// --- Step 6: Print banner (stdout-only) ---
	if created {
		fmt.Fprintf(os.Stdout, "dndmode: created default config at %s\n", cfgPath)
	}
	fmt.Fprintf(os.Stdout, "dndmode: config=%s hotkey=%s\n", cfgPath, cfg.Hotkey)

	// --- Step 3 (Phase 3): signal-driven ctx ---
	// signal.NotifyContext converts SIGINT/SIGTERM/SIGHUP into ctx cancellation.
	// defer stop() unregisters the signal handlers on shutdown.
	//
	// stopper wraps the cancel func returned by signal.NotifyContext.
	// supervisor.go retains its own signal.Notify(sigCh, ...) — double subscription is intentional:
	// Go runtime broadcasts signals to all registered channels; cancel is idempotent;
	// supervisor's fireStop is sync.Once-guarded (P1); both paths unregister on shutdown.
	// See <signal_subscription_rationale> for full rationale.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// --- Step 8 (Phase 3): Platform check (exit 2) ---
	ver := permissions.CurrentOSVersion()
	if err := permissions.CheckPlatform(permissions.CurrentArch(), ver); err != nil {
		switch {
		case errors.Is(err, permissions.ErrNonArm64):
			fmt.Fprintf(os.Stderr,
				"dndmode: requires macOS on Apple Silicon (arm64), got %s/%s.\n",
				runtime.GOOS, runtime.GOARCH)
		case errors.Is(err, permissions.ErrMacOSBelow14):
			fmt.Fprintf(os.Stderr,
				"dndmode: requires macOS 14 (Sonoma) or newer, got %d.%d.\n",
				ver.Major, ver.Minor)
		default:
			fmt.Fprintf(os.Stderr, "dndmode: platform check failed: %v. Re-run on macOS 14+ Apple Silicon.\n", err)
		}
		return exitPlatformErr
	}

	// --- Step 5c (Phase 6): single-instance enforcement ---
	// Cold-start check: bail fast if another live dndmode instance owns
	// runtime.json — AFTER platform check (Step 8 surfaces ErrNonArm64 /
	// ErrMacOSBelow14 with exit 2 first), BEFORE TCC prompts / IOKit
	// acquires. Distinct from Step 10.5 RecoverFromCrash (which handles
	// dead-PID resource release + sentinel-error dispatch); is the
	// LIVE-peer fail-fast gate returning a plain (alive bool, pid int, err
	// error) triple — no errors.Is sentinel dispatch (per the design notes
	// the design notes deviation).
	//
	// The Manager and LiveChecker constructed here are reused at Steps 10.5
	// 11 + 12 + 13.3 (single-instance discipline applies to all
	// seam constructors, not just runtimeMgr — Phase 5 promise made
	// general across powerassert.LiveChecker too).
	runtimeMgr := runtimepkg.NewManager(filepath.Join(home, ".config/dndmode/runtime.json"), log)
	liveChecker := powerassert.NewKernLiveChecker()
	if alive, peerPID, err := runtimepkg.IsLiveInstance(runtimeMgr, liveChecker, log); err != nil {
		// Read failure (corrupted file, permission denied) — not fatal here.
		// Step 10.5 RecoverFromCrash will surface persistent IO/permission
		// errors via ErrFileDeletePersistent → exit 7. stays
		// warn-not-fatal because corrupted state is recovery's domain.
		log.Warn("pre-check inconclusive", slog.Any("err", err))
	} else if alive {
		fmt.Fprintf(os.Stderr,
			"dndmode: another instance is already active (PID=%d). Send SIGTERM or wait for its exit, then re-run.\n",
			peerPID)
		return exitConcurrentInstance
	} else if peerPID > 0 {
		// dead-PID branch — runtime.json exists but snap.PID is dead.
		// returns this triple so the caller can log debug context
		// (per livecheck.go docstring "caller can log debug context but takes
		// no action itself"). Step 10.5 RecoverFromCrash owns the cleanup;
		// this log line lets an operator chasing "why did recovery fire"
		// trace the pre-observation.
		log.Debug("prior runtime.json with dead PID; recovery will clean it up", slog.Int("pid", peerPID))
	}

	// --- Step 9 (Phase 3): WaitForGrants ---
	// Indefinite polling — only ctx.Done() (SIGINT/SIGTERM/SIGHUP) prunes.
	// one-shot prompt Settings deep-link per missing permission at
	// entry; cycle interval 500ms per.
	promptFn := func() { permissions.PromptAccessibility() }
	chk := permissions.NewCgoChecker()
	link := permissions.NewDeepLinker()
	statusW := permissions.NewStatusWriter(os.Stdout)
	if err := permissions.WaitForGrants(ctx, chk, link, statusW, promptFn, log, 500*time.Millisecond); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, "dndmode: aborted while waiting for permissions.")
			return exitPermissionDenied
		}
		fmt.Fprintf(os.Stderr, "dndmode: wait for grants failed: %v. Check Console.app for TCC daemon errors and re-run.\n", err)
		return exitPlatformErr
	}

	// --- Step 9.5 (Phase 5..): Shortcuts presence check ---
	// Single source of truth for the runner — reused at Step 10.5
	// (RecoverFromCrash) and Step 13.7 (Activate). exec.CommandContext
	// inside the runner binds the subprocess to ctx so SIGINT during
	// any of these calls auto-kills the shortcuts CLI child.
	runner := focus.NewExecRunner()
	if err := focus.CheckShortcuts(ctx, runner); err != nil {
		if errors.Is(err, focus.ErrShortcutsMissing) {
			fmt.Fprintln(os.Stderr,
				"dndmode: required Shortcuts not found (need: dndmode-on, dndmode-off).\n\n"+
					"To create them:\n"+
					"  1. Open the Shortcuts app (⌘+Space → \"Shortcuts\").\n"+
					"  2. New Shortcut → search \"Set Focus\" → drag in.\n"+
					"  3. Set: Turn Do Not Disturb On Until Turned Off.\n"+
					"  4. Save as: dndmode-on\n"+
					"  5. Repeat steps 2-4 with \"Turn Do Not Disturb Off\", save as: dndmode-off\n\n"+
					"Then re-run dndmode.")
			return exitFocusSetup
		}
		fmt.Fprintf(os.Stderr, "dndmode: shortcuts check failed: %v. Inspect /usr/bin/shortcuts availability and re-run.\n", err)
		return exitPlatformErr
	}

	// --- Step 10 (Phase 3): SecureEventInput check (exit 4) ---
	if permissions.IsSecureEventInputActive() {
		fmt.Fprintln(os.Stderr,
			"dndmode: Secure Event Input is active (typically Terminal sudo prompt, password fields, or 1Password). Close those, then re-run.")
		return exitSecureInputConflict
	}

	// --- Step 10.5 (Phase 5..): Crash recovery from runtime.json ---
	// Reads ~/.config/dndmode/runtime.json (if present), validates liveness
	// via POSIX kill(pid, 0) (same seam as Phase 3 NewKernLiveChecker), and
	// on dead PID explicitly releases the stored assertion_id (precise —
	// not heuristic). BEFORE powerassert.CleanupOrphans so the
	// explicit-id path wins; CleanupOrphans remains as fallback for crashes
	// BEFORE Manager.Write fired (window between Step 13 and Step 13.3).
	// (runtimeMgr and liveChecker were constructed at Step 5c — reused here
	// per Phase 5 single-instance discipline; extends this to
	// powerassert.LiveChecker.)
	if err := runtimepkg.RecoverFromCrash(ctx, runtimeMgr,
		powerassert.NewCgoReleaser(),
		runner,
		liveChecker,
		log,
	); err != nil {
		if errors.Is(err, runtimepkg.ErrConcurrentInstance) {
			fmt.Fprintf(os.Stderr,
				"dndmode: %v. Send SIGTERM or wait for its exit, then re-run.\n", err)
			return exitConcurrentInstance
		}
		if errors.Is(err, runtimepkg.ErrFileDeletePersistent) {
			//: concrete user action — `rm -f <path>`
			// instead of the vague "Fix permissions and re-run". The
			// user-facing template names the absolute path twice on
			// purpose: once in the diagnostic, once in the rm command,
			// so copy-paste from terminal selection is unambiguous.
			fmt.Fprintf(os.Stderr,
				"dndmode: cannot delete stale runtime file (%s): %v.\n"+
					"Run: rm -f %s\n"+
					"Then re-run dndmode.\n",
				runtimeMgr.Path(), err, runtimeMgr.Path())
			return exitRuntimeJSON
		}
		fmt.Fprintf(os.Stderr, "dndmode: crash recovery failed: %v\n", err)
		return exitPlatformErr
	}

	// --- Step 11 (Phase 3): Orphan IOPMAssertion cleanup ---
	// errors.Is(err, ErrConcurrentInstance) → live-PID match (another dndmode
	// already holds the awake-lock) → exit 5; short-circuit: do NOT
	// release while uncertain. Enumerate-failure (non-ErrConcurrentInstance)
	// → exit 2 (IOKit fundamental). Release-failure is handled inside
	// CleanupOrphans (warn+continue, never propagated as a return value).
	if err := powerassert.CleanupOrphans(
		powerassert.NewCgoEnumerator(),
		powerassert.NewCgoReleaser(),
		liveChecker,
		log,
	); err != nil {
		if errors.Is(err, powerassert.ErrConcurrentInstance) {
			fmt.Fprintf(os.Stderr,
				"dndmode: %v. Send SIGTERM or wait for its exit, then re-run.\n", err)
			return exitConcurrentInstance
		}
		fmt.Fprintf(os.Stderr, "dndmode: orphan cleanup failed: %v. Inspect 'pmset -g assertions' for stuck entries and re-run.\n", err)
		return exitPlatformErr
	}

	// --- Step 12 (Phase 5 replaces P3 mock-runtime-file with real Manager) ---
	// Pushed BEFORE Manager.Write so Release is idempotent for the failure
	// window: if powerassert.Acquire (Step 13) fails, the deferred Cleanup
	// fires Release on a file that was never written — os.Remove on
	// ErrNotExist returns nil. runtimeMgr was constructed at Step 5c.
	rs.Push(runtimeMgr)

	// --- Step 13 (Phase 3): Acquire IOPMAssertion ---
	// Push immediately after successful create per P1 push-after-create
	// discipline. The real Assertion replaces the Phase 2 assertion-slot
	// mock placeholder in unwind — released 3rd, between controller
	// и runtime-mock. After Phase 3 the third releaser logs as
	// `releaser=dndmode active` (per Assertion.Name() == "dndmode active").
	assertion, err := powerassert.Acquire("dndmode active", log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: acquire awake-lock failed: %v. Check IOKit availability and re-run.\n", err)
		return exitPlatformErr
	}
	rs.Push(assertion) // released 3rd in LIFO (between controller и runtime-mock)

	// --- Step 13.3 (Phase 5): Write runtime.json with final assertion_id ---
	// Atomic temp+rename. Records pid + UTC start time + nil PriorFocus
	// (v1 never restores prior Focus) + the *real* assertion id
	// from Step 13 for crash recovery in the next launch.
	if err := runtimeMgr.Write(runtimepkg.Snapshot{
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		PriorFocus:  nil,
		AssertionID: assertion.ID(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: write runtime.json failed: %v. Inspect %s and re-run.\n", err, runtimeMgr.Path())
		return exitPlatformErr
	}

	// --- Step 13.7 (Phase 5): Activate Focus + push deactivate Releaser ---
	// Activate is best-effort: on failure log a warning,
	// do NOT block startup — the user gets DND-less mode but the rest of
	// dndmode (overlay, awake-lock) still works.
	// Push the Releaser regardless: deactivate must still run on Cleanup
	// even if Activate failed (idempotent via two-layer guard; Run("dndmode-off")
	// on an already-off Focus is a no-op).
	if err := focus.Activate(ctx, runner); err != nil {
		log.Warn("focus activate failed", slog.Any("err", err))
	}
	rs.Push(focus.NewReleaser(runner, log))

	// --- Step 14 (Phase 3): cocoa.Init — moved DOWN after permission checks ---
	// Rationale: TCC permission is binary-identity bound, not
	// process-state bound; if permission checks fail we exit BEFORE NSApp
	// setup — cleaner observability (ps / pmset). Also: assertion acquired
	// above is the cheapest resource — fail fast on IOKit errors before any
	// AppKit objects exist.
	if err := cocoa.Init(log); err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: cocoa init failed: %v. Check Console.app for AppKit asserts and re-run.\n", err)
		return exitPlatformErr
	}

	// --- Step 15 (Phase 3): Controller + per-screen overlay windows (P2) ---
	controller := cocoa.NewController(overlayStyle, log)
	if err := controller.CreateWindowsForAllScreens(); err != nil {
		if errors.Is(err, cocoa.ErrNoDisplays) {
			fmt.Fprintln(os.Stderr,
				"dndmode: no displays detected (lid closed without external monitor?). "+
					"Open the lid or connect a display, then re-run.")
		} else {
			fmt.Fprintf(os.Stderr, "dndmode: create overlay windows failed: %v. Reconnect displays and re-run.\n", err)
		}
		return exitPlatformErr
	}
	rs.Push(controller) // released 2nd in LIFO (Name == "windows")

	// --- Step 16 (Phase 4 supervisor; swapped before eventtap) ---
	// supervisor is created BEFORE eventtap.InstallAll (the original Phase 3
	// Step 16) because InstallAll requires `sup.ExitTrigger()` — the same
	// sink channel that both the matched-key poller and the
	// watchdog threshold-hit poller write to on a hotkey/dead-tap
	// event. Original Step 17 thus runs as the new Step 16, and eventtap
	// follows as the new Step 17. Push order onto rs remains LIFO
	// compatible: controller (windows) pushed at Step 15, eventtap (tap)
	// pushed at Step 17 — tap is released FIRST in the LIFO unwind, then
	// windows, then assertion ("dndmode active"), then focus, then runtime.
	//
	// stopper.cancel === signal.NotifyContext stop func (Step 3).
	// supervisor.Start drives a goroutine that listens to its own signal.Notify
	// channel and to ctx.Done — both paths converge on cancel; see Step 3
	// rationale comment for why this double subscription is safe.
	stopper := &cancelStopper{cancel: stop}
	sup := supervisor.New(log, stopper)
	sup.Start(ctx)

	// --- Step 17 (Phase 4 eventtap composite; replaces P3 mock-tap) ---
	// InstallAll wires three subsystems (CGEventTap, dispatch_source_t
	// watchdog, NSWorkspace wake observer) into a single Releaser that
	// releases in safe order: tap-disable + g_observed_tap=NULL
	// (Step 1) → CFRunLoopRemoveSource → CFRelease(source+tap) →
	// watchdog_stop → wake_remove. The atomic-null guard in
	// watchdog_darwin.m / wake_darwin.m closes the window
	// between Step 1 and Step 4-5 (handlers read g_observed_tap first and
	// no-op on NULL) without violating order.
	//
	// `sup.ExitTrigger()` is the sink — matched hotkey OR watchdog
	// threshold-hit both signal exit through it; supervisor then converges
	// on stopper.RequestStop → ctx.cancel → cocoa.RunApp returns →
	// sup.Wait → defer LIFO unwinds.
	//
	// Re-parse the hotkey here (already validated at Step 5b — Parse is
	// idempotent and cheap). The matcher.UserIntentionalMask pre-masking
	// happens inside Install per.
	spec, parseErr := hotkey.Parse(cfg.Hotkey)
	if parseErr != nil {
		// Unreachable: Step 5b already validated. Defensive only — keeps
		// the failure surface explicit instead of relying on global state.
		fmt.Fprintf(os.Stderr, "dndmode: re-parse hotkey failed: %v\n", parseErr)
		return exitConfigErr
	}
	tapRel, err := eventtap.InstallAll(spec, sup.ExitTrigger(), log)
	if err != nil {
		if errors.Is(err, eventtap.ErrTapInstallFailed) {
			fmt.Fprintf(os.Stderr,
				"dndmode: install CGEventTap failed: %v.\n"+
					"Likely causes: Accessibility revoked between PreFlight and now (re-grant via System Settings → Privacy & Security → Accessibility), or another app holds SecureEventInput (close sudo / password fields).\n", err)
			return exitPlatformErr
		}
		fmt.Fprintf(os.Stderr, "dndmode: install eventtap subsystems failed: %v\n", err)
		return exitPlatformErr
	}
	rs.Push(tapRel) // released FIRST in LIFO (Name == "eventtap")

	// --- Step 18.0 (Phase 4 — acceptance test hook) ---
	// Production-safe env-var-guarded panic injection. Used ONLY by
	// TestAcceptance_LIFE10_PanicRecover subprocess test. Default OFF
	// переменная не упоминается в README/docs; production пользователи не
	// увидят. При DNDMODE_TEST_PANIC=1 паника выстреливает после всех Push'ей,
	// что позволяет валидировать: (a) top-level recover в run() (
	// mitigation) catch'ит панику; (b) rs.Cleanup() уже отработал к моменту
	// os.Exit (через LIFO unwind defers); (c) exit code = exitInternalErr (8).
	if os.Getenv("DNDMODE_TEST_PANIC") == "1" {
		panic("test panic (DNDMODE_TEST_PANIC=1)")
	}

	// --- Step 18 (Phase 3): Active state banner (P2: AFTER controller create) ---
	fmt.Fprintln(os.Stdout, "dndmode: active. press Ctrl-C.")

	// --- Step 19 (Phase 3): Block on [NSApp run] until ctx-cancel or unexpected exit ---
	if err := cocoa.RunApp(ctx); err != nil {
		// P2: NSApp.run returned without ctx-cancellation (NSException,
		// AppKit assertion, [NSApp terminate:] from delegate, etc.).
		// Request a stop so the supervisor unwinds + Cleanup chain runs.
		stopper.RequestStop("cocoa exit: " + err.Error())
	}

	// Wait for supervisor goroutine to drain.
	sup.Wait()

	// distinguish a watchdog-triggered abnormal shutdown from a
	// normal matched-hotkey exit. The supervisor exit-trigger channel is
	// shared between the matched-key poller and the watchdog threshold
	// poller — both send a bare struct{}, so the supervisor cannot tell
	// the source from the signal alone. The watchdog flips its internal
	// latch to true BEFORE forwarding through the shared sink (see
	// watchdog_darwin.go), so by the time sup.Wait() returns the latch
	// is durably visible via the eventtap.WatchdogTrippedSinceLastStart()
	// read-only accessor. exitSecureInputConflict (4) is the reused slot
	// per the design notes: an abnormal platform stop. Without this branch,
	// watchdog-killed runs collapsed to exit 0 — operators saw a healthy
	// process and the next LiveChecker found no orphan, masking
	// the silent-disable failure.: replaced direct
	// .Load() on an exported atomic.Bool with the accessor function so
	// external packages cannot Store(true) and forge the exit-code
	// contract.
	if eventtap.WatchdogTrippedSinceLastStart() {
		return exitSecureInputConflict
	}

	// Return exitOK; defer chain runs (Cleanup LIFO + stdout cleanup banner).
	// LIFO release order (Phase 5 finalizes):
	// mock-tap → windows → "dndmode active" → focus → runtime-file.
	return exitOK
}
