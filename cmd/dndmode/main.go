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
	"path/filepath"
	"runtime"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/cocoa"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
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
// Step ordering (Phase 2 — D-09 + D-13 fix):
//
//	1.  Parse --debug flag
//	2.  Construct slog logger (stderr, Info or Debug level)
//	3.  RestoreState + defer Cleanup + cleanup banner
//	4.  Resolve home dir
//	5.  Load config (CFG-01..03) + parse hotkey (CFG-04, 05)
//	6.  Print config banner + active banner sequence
//	6b. cocoa.Init (D-04: NSApp + observers)
//	6c. controller := NewController + CreateWindowsForAllScreens (D-09 cold-start)
//	7.  Push 4 Releasers in REVERSE-LIFE-06 order (D-13 fix)
//	8.  ctx + cancelStopper (hoisted for D-02 RunApp error access) + supervisor
//	9.  stdout "active. press Ctrl-C." banner (D-09: AFTER controller create)
//	10. cocoa.RunApp(ctx) — blocks on [NSApp run] until stop or unexpected exit
//	11. sup.Wait()
//	12. return exitOK; defer chain runs (Cleanup LIFO + stdout cleanup banner)
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
		fmt.Fprintf(os.Stderr, "dndmode: invalid hotkey %q: %v\n", cfg.Hotkey, err)
		return exitConfigErr
	}

	// --- Step 6: Print banner (stdout-only) ---
	if created {
		fmt.Fprintf(os.Stdout, "dndmode: created default config at %s\n", cfgPath)
	}
	fmt.Fprintf(os.Stdout, "dndmode: config=%s hotkey=%s\n", cfgPath, cfg.Hotkey)

	// --- Step 7 (Phase 3 D-05): ctx (hoisted; Task 1b replaces with signal.NotifyContext per D-04) ---
	// ctx is needed by Step 8 WaitForGrants (D-08 indefinite polling, ctx-only
	// cancel). Task 1b will swap context.WithCancel for signal.NotifyContext
	// and move the cancelStopper construction adjacent to it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
			fmt.Fprintf(os.Stderr, "dndmode: platform check failed: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "dndmode: wait for grants failed: %v\n", err)
		return exitPlatformErr
	}

	// --- Step 10 (Phase 3): SecureEventInput check (exit 4) ---
	if permissions.IsSecureEventInputActive() {
		fmt.Fprintln(os.Stderr,
			"dndmode: Secure Event Input is active (typically Terminal sudo prompt, password fields, or 1Password). Close those, then re-run.")
		return exitSecureInputConflict
	}

	// --- Step 6b: cocoa.Init (D-04) — main-goroutine setup of NSApp + screen observers ---
	if err := cocoa.Init(log); err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: cocoa init failed: %v\n", err)
		return exitPlatformErr
	}

	// --- Step 6c: Construct controller + create overlay windows (D-09, D-14) ---
	controller := cocoa.NewController(log)
	if err := controller.CreateWindowsForAllScreens(); err != nil {
		if errors.Is(err, cocoa.ErrNoDisplays) {
			fmt.Fprintln(os.Stderr,
				"dndmode: no displays detected (lid closed without external monitor?). "+
					"Open the lid or connect a display, then re-run.")
		} else {
			fmt.Fprintf(os.Stderr, "dndmode: create overlay windows: %v\n", err)
		}
		return exitPlatformErr
	}

	// --- Step 7: Push Releasers in REVERSE-LIFE-06 order (D-13 fix) ---
	// LIFE-06 mandates cleanup execution order:
	//   1. CGEventTap disable+remove (Phase 4 — currently mocked).
	//   2. NSWindow controller close all (THIS PHASE — real controller).
	//   3. Screen observers detach (folded into controller.Release).
	//   4. shortcuts run dndmode-off (Phase 5 — currently mocked).
	//   5. IOPMAssertion release (Phase 3 — currently mocked).
	//   6. runtime.json delete (Phase 5 — currently mocked).
	//
	// Stack semantics: Push order = creation order; LIFO unwind reverses.
	// To get LIFE-06 execution order tap → windows → assertion → runtime,
	// we Push in the OPPOSITE order: runtime-file → assertion → controller
	// → mock-tap. Then Cleanup pops mock-tap first, controller second,
	// mock-assertion third, mock-runtime-file last. Phase 1 had this
	// inverted (mock-tap pushed first → released last) — fixed here.
	rs.Push(state.NewMockReleaser("mock-runtime-file")) // released LAST  (LIFE-06 #6)
	rs.Push(state.NewMockReleaser("mock-assertion"))    // released 3rd   (LIFE-06 #5)
	rs.Push(controller)                                 // released 2nd   (LIFE-06 #2; Name == "windows")
	rs.Push(state.NewMockReleaser("mock-tap"))          // released FIRST (LIFE-06 #1)

	// --- Step 8: cancelStopper (hoisted for D-02) + supervisor ---
	// ctx + cancel are created above (Step 7); Task 1b replaces them with
	// signal.NotifyContext per D-04.
	stopper := &cancelStopper{cancel: cancel}
	sup := supervisor.New(log, stopper)
	sup.Start(ctx)

	// --- Step 9: Active state banner (D-09: AFTER controller create) ---
	fmt.Fprintln(os.Stdout, "dndmode: active. press Ctrl-C.")

	// --- Step 10: Block on [NSApp run] until ctx-cancel or unexpected exit ---
	if err := cocoa.RunApp(ctx); err != nil {
		// D-02: NSApp.run returned without ctx-cancellation (NSException,
		// AppKit assertion, [NSApp terminate:] from delegate, etc.).
		// Request a stop so the supervisor unwinds + Cleanup chain runs.
		stopper.RequestStop("cocoa exit: " + err.Error())
	}

	// --- Step 11: Wait for supervisor goroutine to drain ---
	sup.Wait()

	// --- Step 12: Return; defer chain runs (Cleanup + cleaning-up banner) ---
	return exitOK
}
