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
	"syscall"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/cocoa"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
	"github.com/dsbasko/dndmode/internal/macos/powerassert"
	"github.com/dsbasko/dndmode/internal/state"
	"github.com/dsbasko/dndmode/internal/supervisor"
)

const (
	// configRelPath is the user-relative config path. POSIX-stable via
	// os.UserHomeDir (mitigation).
	configRelPath = ".config/dndmode/config.yml"

	// Granular exit codes per CONTEXT D-16 (Phase 3 expansion). Reserved
	// slots 6/7 are for Phase 5 (FOC-01 Shortcuts missing / CRR-04 runtime
	// JSON non-recoverable). Stderr wording per UI-SPEC Copywriting Contract.
	exitOK                  = 0 // success
	exitConfigErr           = 1 // P1 — bad YAML, modifier-only hotkey
	exitPlatformErr         = 2 // non-arm64, macOS < 14, IOKit fundamentals
	exitPermissionDenied    = 3 // SIGINT in polling loop
	exitSecureInputConflict = 4 // SecureEventInput active
	exitConcurrentInstance  = 5 // live-PID match on orphan IOPMAssertion
	// Reserved: 6 = exitFocusSetup (Phase 5 FOC-01),
	//           7 = exitRuntimeJSON (Phase 5 CRR-04).
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
//	 1. Parse --debug flag (P1 D-03).
//	 2. slog logger to stderr (P1 D-01/D-02).
//	 3. signal.NotifyContext(ctx, SIGINT, SIGTERM, SIGHUP) (D-04 — replaces the
//	    plain-context cancel pattern used in Phase 2; SIGINT/SIGTERM/SIGHUP now
//	    flow into ctx.Done directly).
//	 4. RestoreState + defer Cleanup + stdout "cleaning up… done." banner (P1).
//	 5. Resolve home dir.
//	 6. Load config (CFG-01..03) + parse hotkey (CFG-04/05).
//	 7. stdout config banner.
//	 8. permissions.CheckPlatform — PERM-01/02; exit 2 on ErrNonArm64/ErrMacOSBelow14.
//	 9. permissions.WaitForGrants — PERM-03/04/05 polling; ctx.Canceled → exit 3.
//	10. permissions.IsSecureEventInputActive — PERM-06 D-15; true → exit 4.
//	11. powerassert.CleanupOrphans — POW-04 D-10/11/12; ErrConcurrentInstance → exit 5.
//	12. rs.Push(state.NewMockReleaser("mock-runtime-file")) — Phase 5 заменит.
//	13. powerassert.Acquire("dndmode active") — POW-01/02/03; rs.Push(assertion).
//	14. cocoa.Init — D-01 (moved DOWN after permission checks).
//	15. cocoa.NewController + CreateWindowsForAllScreens — P2 D-09; rs.Push(controller).
//	16. rs.Push(state.NewMockReleaser("mock-tap")) — Phase 4 заменит.
//	17. supervisor.New(log, stopper) + supervisor.Start(ctx).
//	18. stdout "dndmode: active. press Ctrl-C.".
//	19. cocoa.RunApp(ctx) → sup.Wait() → return exitOK; defer Cleanup LIFO (LIFE-06).
//
// LIFE-06 cleanup execution order via push-stack LIFO unwind:
// mock-tap → controller (windows) → assertion (dndmode active) → mock-runtime-file.
func run() int {
	// --- Step 1: Parse flags (stdlib flag) ---
	debug := flag.Bool("debug", false, "enable debug-level logging on stderr")
	flag.Parse()

	// --- Step 2: slog logger to stderr ---
	level := slog.LevelInfo
	if *debug {
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

	// --- Step 10 (Phase 3): SecureEventInput check (exit 4) ---
	if permissions.IsSecureEventInputActive() {
		fmt.Fprintln(os.Stderr,
			"dndmode: Secure Event Input is active (typically Terminal sudo prompt, password fields, or 1Password). Close those, then re-run.")
		return exitSecureInputConflict
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
		powerassert.NewKernLiveChecker(),
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

	// --- Step 12 (Phase 3 D-05): Push runtime-file mock (released LAST per LIFE-06) ---
	// Phase 5 заменит на real runtime.Manager (CRR-01..04).
	rs.Push(state.NewMockReleaser("mock-runtime-file"))

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
	controller := cocoa.NewController(log)
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

	// --- Step 16 (Phase 3 D-05): Push tap-mock (released FIRST per LIFE-06) ---
	// Phase 4 заменит на real CGEventTap releaser (INP-01..04).
	rs.Push(state.NewMockReleaser("mock-tap"))

	// --- Step 17 (Phase 3 D-05): cancelStopper + supervisor ---
	// stopper.cancel === signal.NotifyContext stop func (Step 3).
	// supervisor.Start drives a goroutine that listens to its own signal.Notify
	// channel and to ctx.Done — both paths converge on cancel; see Step 3
	// rationale comment for why this double subscription is safe.
	stopper := &cancelStopper{cancel: stop}
	sup := supervisor.New(log, stopper)
	sup.Start(ctx)

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

	// Return exitOK; defer chain runs (Cleanup LIFO + stdout cleanup banner).
	// LIFO release order: mock-tap → controller (windows) → assertion
	// (dndmode active) → mock-runtime-file (LIFE-06).
	return exitOK
}
