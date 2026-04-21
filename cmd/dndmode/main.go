//go:build darwin

// Command dndmode is a macOS Apple-Silicon CLI utility that locks the
// keyboard/trackpad behind a configurable hotkey while keeping the system
// awake (Phase 4-5 functionality). Phase 1 is a transparent process model
// skeleton: it loads (or creates) ~/.config/dndmode/config.yml, prints a
// banner, enters an "active" sleep, and on SIGINT/SIGTERM/SIGHUP runs the
// LIFO cleanup chain over in-memory mock Releasers and exits 0.
//
// No system effects in Phase 1: no overlay, no input blocking, no power
// assertion, no Focus integration. All of those are added incrementally
// in Phases 2-5; Phase 1 validates the Go-runtime foundation in isolation.
package main

import (
	// runtimepin's init() calls runtime.LockOSThread() to pin main goroutine
	// to OS thread #0 (m0). This MUST be the first import — Phase 2 NSApp.run()
	// depends on the invariant that main lives on m0 (PITFALLS #24, RESEARCH §1).
	_ "github.com/dsbasko/dndmode/internal/runtimepin"

	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/state"
	"github.com/dsbasko/dndmode/internal/supervisor"
)

const (
	// configRelPath is the user-relative config path. POSIX-stable via
	// os.UserHomeDir (T-01-01 mitigation: not parsing $HOME directly).
	configRelPath = ".config/dndmode/config.yml"

	exitOK        = 0
	exitConfigErr = 1
	exitFatalErr  = 2
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

// run is the testable entry point (main calls os.Exit(run())). Phase 1
// does not unit-test run() directly — coverage comes from acceptance tests
// (-tags=acceptance) that fork a subprocess.
func run() int {
	// --- Step 1: Parse flags (D-03 stdlib flag, no pflag/cobra in v1) ---
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
		return exitFatalErr
	}
	cfgPath := filepath.Join(home, configRelPath)

	// --- Step 5: Load config ---
	loader := config.NewLoader(cfgPath)
	cfg, created, err := loader.Load()
	if err != nil {
		// CFG-03: pretty error with line:col already formatted by Loader.
		fmt.Fprintln(os.Stderr, err)
		return exitConfigErr
	}

	// --- Step 5b: Validate hotkey grammar (CFG-04, CFG-05). Phase 4 will
	// reuse the parsed Spec for CGEventTap matching; Phase 1 only needs the
	// validation side-effect so a modifier-only or unknown-key config fails
	// fast with exit 1 (ROADMAP SC4). ---
	if _, err := hotkey.Parse(cfg.Hotkey); err != nil {
		fmt.Fprintf(os.Stderr, "dndmode: invalid hotkey %q: %v\n", cfg.Hotkey, err)
		return exitConfigErr
	}

	// --- Step 6: Print banner (stdout-only) ---
	if created {
		fmt.Fprintf(os.Stdout, "dndmode: created default config at %s\n", cfgPath)
	}
	fmt.Fprintf(os.Stdout, "dndmode: config=%s hotkey=%s\n", cfgPath, cfg.Hotkey)

	// --- Step 7: Push Phase-1 mock Releasers in the order Phase 4-5 will ---
	// push real ones. LIFO Cleanup will release "mock-runtime-file" first,
	// "mock-tap" last (mirroring REQUIREMENTS LIFE-06 ordering).
	rs.Push(state.NewMockReleaser("mock-tap"))
	rs.Push(state.NewMockReleaser("mock-windows"))
	rs.Push(state.NewMockReleaser("mock-assertion"))
	rs.Push(state.NewMockReleaser("mock-runtime-file"))

	// --- Step 8: Supervisor + ctx ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopper := &cancelStopper{cancel: cancel}
	sup := supervisor.New(log, stopper)
	sup.Start(ctx)

	// --- Step 9: Active state banner ---
	fmt.Fprintln(os.Stdout, "dndmode: active. press Ctrl-C.")

	// --- Step 10-11: Block until shutdown trigger fires ---
	<-ctx.Done()
	sup.Wait()

	// --- Step 12: Return; defer chain runs (Cleanup + cleaning-up banner) ---
	return exitOK
}
