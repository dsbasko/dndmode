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
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/audiomute"
	"github.com/dsbasko/dndmode/internal/macos/caffeinate"
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

// gatedWriter forwards writes to w only while *on is true; otherwise it
// silently discards them (reporting a full write so callers never see a short
// write or error). It backs dndmode's debug output gate: run() builds one over
// os.Stdout and one over os.Stderr, both reading the SAME *bool, and points the
// slog logger at the stderr one. Flipping that single flag — raised by the
// --debug flag (Step 1) or `debug: true` in config (Step 5) — switches every
// banner, diagnostic, and log line on or off at once. Default OFF: dndmode is
// silent so a visible terminal (overlay_style none/glass) never leaks the
// unlock hotkey (security stance: reveal nothing — see config.Config.Debug and
//).
type gatedWriter struct {
	w  io.Writer
	on *bool
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	if !*g.on {
		return len(p), nil
	}
	return g.w.Write(p)
}

// resolveBoolFlag implements the --mute/--focus tri-state precedence: an empty
// flag value means "use the config default", a non-empty value is parsed with
// strconv.ParseBool (accepting 1/t/T/TRUE/true/0/f/F/FALSE/false). A junk value
// returns the ParseBool error so the caller can emit a source-naming stderr
// line and exit 1 — mirroring the invalid---style handling at Step 5b.1.
func resolveBoolFlag(flagVal string, configDefault bool) (bool, error) {
	if flagVal == "" {
		return configDefault, nil
	}
	return strconv.ParseBool(flagVal)
}

// parseTimer resolves the --timer flag's raw string into an auto-disable
// duration. An empty string (the common case — the operator did not pass
// --timer) returns 0 with no error, meaning "no deadline: run until hotkey or
// signal". A non-empty value is parsed with time.ParseDuration, so it takes the
// same grammar as any Go duration ("30m", "1h30m", "90s"); a parse failure or a
// non-positive result (0 or negative — nonsensical for a countdown) returns an
// error whose message main() embeds in a --timer-naming stderr line before
// exiting 1, mirroring how resolveBoolFlag / config.ValidateOverlayStyle surface
// bad flag values. Kept pure (no side effects) so it is unit-tested directly
// (Test_parseTimer) like resolveBoolFlag.
func parseTimer(flagVal string) (time.Duration, error) {
	if flagVal == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(flagVal)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive (e.g. 30m, 1h30m, 90s)")
	}
	return d, nil
}

// parseStyleFlag splits a --style value into its base style plus an optional
// ":<suffix>" whose meaning depends on the base:
//   - "glass:24"        => ("glass", *24, "", nil)  -- CIGaussianBlur radius override
//   - "terminal:python" => ("terminal", nil, "python", nil)  -- source language
// A bare style => (s, nil, "", nil). A suffix on any base other than glass or
// terminal, a non-numeric / out-of-range glass radius, or an unknown terminal
// language is an error. The base style itself is NOT validated here — the caller
// runs config.ValidateOverlayStyle so its error text can name the source (flag vs
// config). Kept pure so it is unit-tested directly (Test_parseStyleFlag) like
// parseTimer / resolveBoolFlag.
func parseStyleFlag(s string) (string, *float64, string, error) {
	base, suffix, hasSuffix := strings.Cut(s, ":")
	if !hasSuffix {
		return s, nil, "", nil
	}
	switch base {
	case config.OverlayStyleGlass:
		v, perr := strconv.ParseFloat(strings.TrimSpace(suffix), 64)
		if perr != nil {
			return "", nil, "", fmt.Errorf("blur radius %q must be a number", suffix)
		}
		if verr := config.ValidateGlassBlur(v); verr != nil {
			return "", nil, "", verr
		}
		return base, &v, "", nil
	case config.OverlayStyleTerminal:
		lang := strings.TrimSpace(suffix)
		if verr := config.ValidateTerminalLanguage(lang); verr != nil {
			return "", nil, "", verr
		}
		return base, nil, lang, nil
	default:
		return "", nil, "", fmt.Errorf("the ':<...>' suffix is only valid with glass or terminal (got %q)", base)
	}
}

// armTimer starts a one-shot timer that auto-disables dndmode after d by calling
// stop — the signal.NotifyContext cancel — so expiry drives the EXACT clean
// shutdown path as the unlock hotkey or a signal: ctx cancels → cocoa.RunApp's
// watcher posts the synthetic stop event and [NSApp run] returns nil (full mode)
// / the caffeinate select wakes on ctx.Done() (none mode) → LIFO Cleanup → exit
// 0. A non-positive d disarms the timer (returns a no-op func) so callers can
// pass an unresolved --timer unconditionally. The returned func stops the timer
// and MUST be deferred by the caller: an early exit (hotkey/signal before expiry)
// would otherwise leave a stray AfterFunc goroutine armed to fire stop after
// run() has already returned. stop is idempotent (supervisor sync.Once +
// ctx.cancel), so a benign race between a near-simultaneous signal and the timer
// is harmless. time.AfterFunc runs its callback on its own goroutine; stop is
// documented thread-safe.
func armTimer(d time.Duration, stop func(), log *slog.Logger) func() {
	if d <= 0 {
		return func() {}
	}
	t := time.AfterFunc(d, func() {
		log.Info("auto-disable timer elapsed; shutting down", slog.Duration("after", d))
		stop()
	})
	return func() { t.Stop() }
}

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
// 1. Parse --debug flag (P1) + --style overlay-style override flag.
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
//     its sink channel.
// 17. eventtap.InstallAll(spec, sup.ExitTrigger(), log) — Phase 4;
//     composes tap + watchdog + wake observer into a single Releaser;
//     rs.Push(tapRel). Replaces the Phase 3 mock-tap placeholder.
//  18. stdout "dndmode: active. press Ctrl-C.".
// 19. cocoa.RunApp(ctx) → sup.Wait() → return exitOK; defer Cleanup LIFO;
// top-level recover defer (registered FIRST inside run, Phase 4
//) catches any Cocoa-panic AFTER Cleanup, exits with exitInternalErr=8.
//
// cleanup execution order via push-stack LIFO unwind (Phase 4
// finalises tap-releaser): eventtap → windows → "dndmode active" → focus
// → runtime-file.
func run() int {
	// --- Debug output gate (see gatedWriter + config.Config.Debug) ---
	// EVERY user-facing console write — stdout banners, stderr diagnostics, the
	// slog logger, and the panic trace below — is routed through outW/errW and
	// suppressed while debugOn is false. debugOn starts false (SILENT default: a
	// visible terminal under overlay_style none/glass must never leak the unlock
	// hotkey — security stance) and is raised by the --debug flag (Step 1)
	// then `debug: true` in config (Step 5). Both writers hold &debugOn, so
	// raising it un-gates everything written afterwards; it is never lowered
	// (debug is additive). Declared BEFORE the recover defer so that defer can
	// reference errW — it remains the FIRST defer registered (invariant:
	// runs LAST on unwind), since these declarations are not defers.
	var debugOn bool
	outW := &gatedWriter{w: os.Stdout, on: &debugOn}
	errW := &gatedWriter{w: os.Stderr, on: &debugOn}

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
			_, _ = fmt.Fprintf(errW, "dndmode: PANIC: %v\n%s\n", r, debug.Stack())
			os.Exit(exitInternalErr)
		}
	}()

	// --- Step 1: Parse flags (stdlib flag) ---
	// --debug un-silences ALL console output for the run (banners + diagnostics +
	// debug-level logging). Default OFF = silent; only the exit code speaks.
	debugFlag := flag.Bool("debug", false, "enable all console output (banners + diagnostics + debug logging); default: silent, exit codes only")
	// --style overrides overlay_style from the YAML config (QUICK-gh8 follow-up):
	// when non-empty it WINS over cfg.OverlayStyle so an operator can pick a look
	// for a single run without editing ~/.config/dndmode/config.yml. Empty (the
	// default) means "use whatever the config says". Validated at Step 5b.1 with
	// the same ValidateOverlayStyle gate as the config value.
	styleFlag := flag.String("style", "", "override overlay_style for this run (black|matrix|terminal[:go|python|typescript|rust]|glass[:radius]|none); empty = use config")
	// --mute / --focus override the config keys for a single run (same
	// precedence as --style: non-empty WINS over YAML, empty = use config).
	// Tri-state strings ("" | "true" | "false") rather than flag.Bool because a
	// plain bool flag cannot express "absent → fall back to config" — a missing
	// --mute must mean "use cfg.Mute (default true)", not "false". Parsed via
	// strconv.ParseBool at Step 8b; junk → exitConfigErr (mirrors invalid --style).
	muteFlag := flag.String("mute", "", "override mute for this run (true|false); empty = use config")
	focusFlag := flag.String("focus", "", "override focus/DND for this run (true|false); empty = use config")
	// --timer sets a per-run auto-disable deadline: after the given Go duration
	// (time.ParseDuration grammar — "30m", "1h30m", "90s") elapses while dndmode is
	// ACTIVE, dndmode tears down and exits 0 exactly as if the unlock hotkey were
	// pressed. Empty (the default) means no deadline — run until the hotkey or a
	// signal (SIGINT/SIGTERM/SIGHUP). Per-run ONLY: there is intentionally no config
	// key (a persistent auto-off default would surprise; typing --timer is the
	// deliberate opt-in). Works for EVERY overlay_style, including none/caffeinate,
	// because the timer merely triggers the same ctx-cancel shutdown both modes
	// already await (see armTimer). Parsed at Step 5b.2 via parseTimer; junk /
	// non-positive → exitConfigErr (mirrors invalid --style), naming --timer.
	timerFlag := flag.String("timer", "", "auto-disable after this long, then exit 0 (Go duration, e.g. 30m, 1h30m, 90s); empty = run until hotkey/signal")
	flag.Parse()
	// Raise the output gate for the whole run when --debug is set. `debug: true`
	// in config can also raise it after Load (Step 5); either source enables
	// output. Never lowered — debug is additive.
	debugOn = *debugFlag

	// --- Step 2: slog logger — writes through the gated errW ---
	// Level is always Debug; the errW gate (not the level) decides visibility, so
	// enabling debug from EITHER --debug or `debug: true` surfaces the same lines.
	// When debugOn is false errW discards everything, so the logger stays silent.
	log := slog.New(slog.NewTextHandler(errW, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// --- Step 3: RestoreState + deferred Cleanup with stdout banner ---
	rs := state.NewRestoreState(log)
	defer func() {
		if err := rs.Cleanup(); err != nil {
			log.Error("cleanup returned aggregated error", slog.Any("err", err))
		}
		// Single user-facing line on stdout. Printed AFTER Cleanup
		// completes — proof that defer chain ran.
		_, _ = fmt.Fprintln(outW, "dndmode: cleaning up… done.")
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
		_, _ = fmt.Fprintln(errW, err)
		return exitConfigErr
	}
	// `debug: true` in config raises the output gate too (the --debug flag, Step
	// 1, is the per-run equivalent; either source enables output). Applied right
	// after a successful Load so the Step 6 banner and everything downstream honor
	// it. A config PARSE error above is governed by --debug alone (config not yet
	// read at that point) — run `dndmode --debug` to diagnose a silently-failing
	// config.
	if cfg.Debug {
		debugOn = true
	}

	// --- Step 5b: Validate hotkey grammar ---
	if _, err := hotkey.Parse(cfg.Hotkey); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid hotkey %q: %v. Fix the hotkey grammar in ~/.config/dndmode/config.yml.\n", cfg.Hotkey, err)
		return exitConfigErr
	}

	// --- Step 5b.1: Resolve + validate overlay style (QUICK-gh8, T-gh8-01) ---
	// Precedence: the --style flag (Step 1), when non-empty, OVERRIDES
	// cfg.OverlayStyle — a per-run look selection that ignores the YAML config
	// entirely. Empty flag falls back to the config value. Either source is
	// value-validated HERE, before any window is created: yaml.Strict() rejects
	// unknown KEYS but not unknown VALUES, so a junk style would otherwise parse
	// fine. The error template names the offending SOURCE (flag vs config file)
	// so the operator knows where to fix it. overlayStyle (normalized:
	// ""=>"black") is declared in this scope so it is still visible at Step 15
	// (passed into NewController). styleSource feeds the Step 6 banner.
	overlayStyle := cfg.OverlayStyle
	styleSource := "config"
	var blurOverride *float64 // set only by a --style glass:N suffix
	var langOverride string   // set only by a --style terminal:<lang> suffix
	if *styleFlag != "" {
		base, bo, lo, perr := parseStyleFlag(*styleFlag)
		if perr != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: invalid --style %q: %v.\n", *styleFlag, perr)
			return exitConfigErr
		}
		overlayStyle = base
		blurOverride = bo
		langOverride = lo
		styleSource = "flag"
	}
	if err := config.ValidateOverlayStyle(overlayStyle); err != nil {
		if styleSource == "flag" {
			_, _ = fmt.Fprintf(errW, "dndmode: invalid --style %q: %v.\n", overlayStyle, err)
		} else {
			_, _ = fmt.Fprintf(errW, "dndmode: invalid overlay_style %q: %v. Fix overlay_style in ~/.config/dndmode/config.yml (valid: black, matrix, terminal, glass, none).\n", overlayStyle, err)
		}
		return exitConfigErr
	}
	overlayStyle = config.NormalizeOverlayStyle(overlayStyle)

	// Resolve the glass blur radius (only meaningful for overlay_style glass): a
	// --style glass:N suffix (blurOverride, already validated in parseStyleFlag)
	// wins; otherwise the config glass_blur value (nil => DefaultGlassBlur via
	// NormalizeGlassBlur), validated HERE so a junk glass_blur in YAML fails fast
	// like an invalid style. Threaded into NewController at Step 15.
	glassBlur := config.NormalizeGlassBlur(cfg.GlassBlur)
	if blurOverride != nil {
		glassBlur = *blurOverride
	} else if err := config.ValidateGlassBlur(glassBlur); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid glass_blur %g: %v. Fix glass_blur in %s.\n", glassBlur, err, cfgPath)
		return exitConfigErr
	}

	// Resolve the terminal source language (only meaningful for overlay_style
	// terminal): the --style terminal:<lang> suffix (langOverride, already
	// validated in parseStyleFlag) wins; otherwise the config terminal_language
	// value, validated HERE so a junk value in YAML fails fast like an invalid
	// style. Normalized ("" => go). Threaded into NewController at Step 15;
	// ignored for every non-terminal style. Mirrors the glass_blur resolution.
	terminalLanguage := cfg.TerminalLanguage
	if langOverride != "" {
		terminalLanguage = langOverride
	} else if err := config.ValidateTerminalLanguage(terminalLanguage); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid terminal_language %q: %v. Fix terminal_language in %s.\n", terminalLanguage, err, cfgPath)
		return exitConfigErr
	}
	terminalLanguage = config.NormalizeTerminalLanguage(terminalLanguage)

	// --- Step 5b.2: Resolve + validate the auto-disable timer (flag-only) ---
	// The timer is a per-run flag with no config key, so there is nothing to fall
	// back to: an empty flag means "no deadline". A non-empty value is validated
	// HERE — before the signal ctx, platform check, none-mode branch, and every
	// permission prompt — so a typo (e.g. --timer 5x) fails fast with exit 1 instead
	// of after the user has already granted Accessibility. timerDur == 0 disarms the
	// timer; armTimer (full path Step 18 / runCaffeinateOnly) no-ops on it. The
	// stderr template names --timer as the source, mirroring the invalid --style
	// branch above.
	timerDur, err := parseTimer(*timerFlag)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid --timer %q: %v.\n", *timerFlag, err)
		return exitConfigErr
	}

	// --- Step 6: Print banner (stdout-only) ---
	if created {
		_, _ = fmt.Fprintf(outW, "dndmode: created default config at %s\n", cfgPath)
	}
	_, _ = fmt.Fprintf(outW, "dndmode: config=%s hotkey=%s overlay_style=%s (%s)\n", cfgPath, cfg.Hotkey, overlayStyle, styleSource)
	if overlayStyle == config.OverlayStyleGlass {
		_, _ = fmt.Fprintf(outW, "dndmode: glass_blur=%g\n", glassBlur)
	} else if overlayStyle == config.OverlayStyleTerminal {
		_, _ = fmt.Fprintf(outW, "dndmode: terminal_language=%s\n", terminalLanguage)
	}
	if timerDur > 0 {
		_, _ = fmt.Fprintf(outW, "dndmode: timer=%s — auto-disable after this long\n", timerDur)
	}

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
			_, _ = fmt.Fprintf(errW,
				"dndmode: requires macOS on Apple Silicon (arm64), got %s/%s.\n",
				runtime.GOOS, runtime.GOARCH)
		case errors.Is(err, permissions.ErrMacOSBelow14):
			_, _ = fmt.Fprintf(errW,
				"dndmode: requires macOS 14 (Sonoma) or newer, got %d.%d.\n",
				ver.Major, ver.Minor)
		default:
			_, _ = fmt.Fprintf(errW, "dndmode: platform check failed: %v. Re-run on macOS 14+ Apple Silicon.\n", err)
		}
		return exitPlatformErr
	}

	// --- Step 8a (overlay_style=none): caffeinate-only fast path ---
	// "none" is not a look — it short-circuits the entire locking pipeline. We
	// branch here, AFTER the platform check (so a wrong-arch / pre-Sonoma host
	// still surfaces exit 2) but BEFORE every permission / Shortcuts / IOKit
	// step: none mode needs NO Accessibility grant, NO dndmode-on/off Shortcuts,
	// NO IOPMAssertion, NO overlay window, NO event tap. It only holds
	// caffeinate. The deferred rs.Cleanup + top-level recover (registered at the
	// top of run) still apply, so the caffeinate child is torn down and the
	// stdout cleanup banner still prints on exit.
	if overlayStyle == config.OverlayStyleNone {
		return runCaffeinateOnly(ctx, stop, cfg.AllowDisplaySleep, timerDur, rs, log, outW, errW)
	}

	// --- Step 8b: resolve effective mute / focus (flag overrides config) ---
	// Resolved AFTER the none-mode branch on purpose: caffeinate-only mode never
	// touches audio or Focus (the user may still be at the machine), so its flag
	// values are irrelevant and must not gate the fast path. Both follow the
	// --style precedence: a non-empty flag WINS over YAML, empty falls back to
	// config (config.NormalizeMute applies the nil⇒true rule on the YAML side).
	// Junk flag values exit 1 (exitConfigErr) with a source-naming stderr line,
	// exactly like an invalid --style.
	effectiveMute, err := resolveBoolFlag(*muteFlag, config.NormalizeMute(cfg.Mute))
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid --mute %q: %v.\n", *muteFlag, err)
		return exitConfigErr
	}
	effectiveFocus, err := resolveBoolFlag(*focusFlag, cfg.Focus)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid --focus %q: %v.\n", *focusFlag, err)
		return exitConfigErr
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
		_, _ = fmt.Fprintf(errW,
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
	// Gate the permission-wait status like every other banner: the real os.Stdout
	// only when debug is on (so NewStatusWriter's *os.File TTY detection still
	// gives the \r-repaint UX), io.Discard otherwise so a waiting run stays fully
	// silent. Passing the gatedWriter here instead would defeat TTY detection.
	var statusOut = io.Discard
	if debugOn {
		statusOut = os.Stdout
	}
	statusW := permissions.NewStatusWriter(statusOut)
	if err := permissions.WaitForGrants(ctx, chk, link, statusW, promptFn, log, 500*time.Millisecond); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_, _ = fmt.Fprintln(errW, "dndmode: aborted while waiting for permissions.")
			return exitPermissionDenied
		}
		_, _ = fmt.Fprintf(errW, "dndmode: wait for grants failed: %v. Check Console.app for TCC daemon errors and re-run.\n", err)
		return exitPlatformErr
	}

	// --- Step 9.5 (Phase 5..): Shortcuts presence check ---
	// Single source of truth for the runner — reused at Step 10.5
	// (RecoverFromCrash) and Step 13.7 (Activate). exec.CommandContext
	// inside the runner binds the subprocess to ctx so SIGINT during
	// any of these calls auto-kills the shortcuts CLI child.
	// Focus is now OPT-IN (effectiveFocus): the Shortcuts presence gate (and its
	// exit-6 branch) runs ONLY when the user enabled focus. With focus disabled
	// — the new default — dndmode never shells out to /usr/bin/shortcuts, so a
	// host without the dndmode-on/off Shortcuts no longer blocks startup.
	runner := focus.NewExecRunner()
	if effectiveFocus {
		if err := focus.CheckShortcuts(ctx, runner); err != nil {
			if errors.Is(err, focus.ErrShortcutsMissing) {
				_, _ = fmt.Fprintln(errW,
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
			_, _ = fmt.Fprintf(errW, "dndmode: shortcuts check failed: %v. Inspect /usr/bin/shortcuts availability and re-run.\n", err)
			return exitPlatformErr
		}
	}

	// --- Step 10 (Phase 3): SecureEventInput check (exit 4) ---
	if permissions.IsSecureEventInputActive() {
		_, _ = fmt.Fprintln(errW,
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
		audiomute.NewExecRunner(),
		liveChecker,
		log,
	); err != nil {
		if errors.Is(err, runtimepkg.ErrConcurrentInstance) {
			_, _ = fmt.Fprintf(errW,
				"dndmode: %v. Send SIGTERM or wait for its exit, then re-run.\n", err)
			return exitConcurrentInstance
		}
		if errors.Is(err, runtimepkg.ErrFileDeletePersistent) {
			//: concrete user action — `rm -f <path>`
			// instead of the vague "Fix permissions and re-run". The
			// user-facing template names the absolute path twice on
			// purpose: once in the diagnostic, once in the rm command,
			// so copy-paste from terminal selection is unambiguous.
			_, _ = fmt.Fprintf(errW,
				"dndmode: cannot delete stale runtime file (%s): %v.\n"+
					"Run: rm -f %s\n"+
					"Then re-run dndmode.\n",
				runtimeMgr.Path(), err, runtimeMgr.Path())
			return exitRuntimeJSON
		}
		_, _ = fmt.Fprintf(errW, "dndmode: crash recovery failed: %v\n", err)
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
			_, _ = fmt.Fprintf(errW,
				"dndmode: %v. Send SIGTERM or wait for its exit, then re-run.\n", err)
			return exitConcurrentInstance
		}
		_, _ = fmt.Fprintf(errW, "dndmode: orphan cleanup failed: %v. Inspect 'pmset -g assertions' for stuck entries and re-run.\n", err)
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
	// The assertion TYPE is selected from cfg.AllowDisplaySleep (inverted
	// polarity): default false → PreventUserIdleDisplaySleep (display kept
	// awake); true → legacy PreventUserIdleSystemSleep (display may idle-off).
	assertion, err := powerassert.Acquire("dndmode active", cfg.AllowDisplaySleep, log)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: acquire awake-lock failed: %v. Check IOKit availability and re-run.\n", err)
		return exitPlatformErr
	}
	rs.Push(assertion) // released 3rd in LIFO (between controller и runtime-mock)

	// --- Step 13.3 (Phase 5): Resolve prior mute state, then write runtime.json ---
	// When effectiveMute, query the CURRENT system-audio mute state BEFORE
	// building the Snapshot literal — the recorded prior_muted must reflect the
	// state from before we touch audio (Step 13.7). On a GetMuted error: warn +
	// skip the whole mute step (priorMuted stays nil). NEVER mute without a
	// recorded prior state, otherwise exit could leave the user's audio muted
	// forever. priorMuted == nil ⇒ audio is never touched at Step 13.7.
	muteRunner := audiomute.NewExecRunner()
	var priorMuted *bool
	if effectiveMute {
		if got, gerr := muteRunner.GetMuted(ctx); gerr != nil {
			log.Warn("query system mute state failed; skipping audio mute", slog.Any("err", gerr))
		} else {
			priorMuted = &got
		}
	}

	// Atomic temp+rename. Records pid + UTC start time + nil PriorFocus
	// (v1 never restores prior Focus) + the *real* assertion id
	// from Step 13 for crash recovery + prior_muted for audio restore.
	if err := runtimeMgr.Write(runtimepkg.Snapshot{
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		PriorFocus:   nil,
		AssertionID:  assertion.ID(),
		PriorMuted:   priorMuted,
		FocusEnabled: &effectiveFocus,
	}); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: write runtime.json failed: %v. Inspect %s and re-run.\n", err, runtimeMgr.Path())
		return exitPlatformErr
	}

	// --- Step 13.7 (Phase 5): Focus (opt-in) + audio mute lifecycle ---
	// Focus is opt-in (effectiveFocus). When enabled, Activate is best-effort
	//: on failure log a warning, do NOT block startup. Push the
	// Releaser regardless: deactivate must still run on Cleanup even if Activate
	// failed (idempotent two-layer guard; Run("dndmode-off") on an already-off
	// Focus is a no-op).
	if effectiveFocus {
		if err := focus.Activate(ctx, runner); err != nil {
			log.Warn("focus activate failed", slog.Any("err", err))
		}
		rs.Push(focus.NewReleaser(runner, log))
	}

	// Audio mute (best-effort): only when a prior state was recorded above
	// (priorMuted != nil). Push the Releaser REGARDLESS of SetMuted's outcome,
	// mirroring the focus releaser above (which is pushed even when Activate
	// fails): SetMuted can apply the mute and STILL return an error — ctx
	// cancelled right after osascript changed the volume, or a non-zero exit
	// after a partial effect. If we skipped the push on error, that leaked mute
	// would survive forever (normal Cleanup deletes runtime.json, so crash
	// recovery never sees it either). The Releaser is idempotent and its
	// priorMuted gate makes SetMuted(false) a no-op when audio was genuinely
	// left unmuted, so pushing after a real failure costs at most one harmless
	// unmute on Cleanup. Pushed AFTER the focus push so the LIFO unwind releases
	// audiomute BEFORE focus — both are independent best-effort silencing steps;
	// what matters is both unwind before the assertion (slot #3) and
	// runtime-file (slot #5).
	if priorMuted != nil {
		if err := muteRunner.SetMuted(ctx, true); err != nil {
			log.Warn("system audio mute failed", slog.Any("err", err))
		}
		rs.Push(audiomute.NewReleaser(muteRunner, *priorMuted, log))
	}

	// --- Step 14 (Phase 3): cocoa.Init — moved DOWN after permission checks ---
	// Rationale: TCC permission is binary-identity bound, not
	// process-state bound; if permission checks fail we exit BEFORE NSApp
	// setup — cleaner observability (ps / pmset). Also: assertion acquired
	// above is the cheapest resource — fail fast on IOKit errors before any
	// AppKit objects exist.
	if err := cocoa.Init(log); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cocoa init failed: %v. Check Console.app for AppKit asserts and re-run.\n", err)
		return exitPlatformErr
	}

	// --- Step 15 (Phase 3): Controller + per-screen overlay windows (P2) ---
	controller := cocoa.NewController(overlayStyle, glassBlur, terminalLanguage, log)
	if err := controller.CreateWindowsForAllScreens(); err != nil {
		if errors.Is(err, cocoa.ErrNoDisplays) {
			_, _ = fmt.Fprintln(errW,
				"dndmode: no displays detected (lid closed without external monitor?). "+
					"Open the lid or connect a display, then re-run.")
		} else {
			_, _ = fmt.Fprintf(errW, "dndmode: create overlay windows failed: %v. Reconnect displays and re-run.\n", err)
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
		_, _ = fmt.Fprintf(errW, "dndmode: re-parse hotkey failed: %v\n", parseErr)
		return exitConfigErr
	}
	tapRel, err := eventtap.InstallAll(spec, sup.ExitTrigger(), log)
	if err != nil {
		if errors.Is(err, eventtap.ErrTapInstallFailed) {
			_, _ = fmt.Fprintf(errW,
				"dndmode: install CGEventTap failed: %v.\n"+
					"Likely causes: Accessibility revoked between PreFlight and now (re-grant via System Settings → Privacy & Security → Accessibility), or another app holds SecureEventInput (close sudo / password fields).\n", err)
			return exitPlatformErr
		}
		_, _ = fmt.Fprintf(errW, "dndmode: install eventtap subsystems failed: %v\n", err)
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

	// --- Step 17.9: Arm the per-run auto-disable timer (any overlay_style) ---
	// When --timer was given (timerDur > 0), start the countdown NOW — the moment
	// dndmode is fully active — not at launch, so the time the operator spent
	// granting permissions never eats into the deadline. On expiry armTimer cancels
	// ctx (via stop, the signal.NotifyContext cancel), driving the SAME clean
	// shutdown as the unlock hotkey or SIGINT (Step 19's cocoa.RunApp returns nil →
	// sup.Wait → deferred LIFO Cleanup → exit 0). defer disarms it so an EARLY exit
	// (hotkey/signal before expiry) leaves no stray AfterFunc goroutine armed.
	stopTimer := armTimer(timerDur, stop, log)
	defer stopTimer()

	// --- Step 18 (Phase 3): Active state banner (P2: AFTER controller create) ---
	_, _ = fmt.Fprintln(outW, "dndmode: active. press Ctrl-C.")

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

// runCaffeinateOnly is the overlay_style=none execution path: dndmode degrades
// to a thin /usr/bin/caffeinate(8) wrapper that holds a system-awake assertion
// for as long as it runs, and nothing else — no Focus/DND, no keyboard/trackpad
// block (hence no Accessibility prompt and no Shortcuts requirement), no overlay
// window, no event tap. Exit is via SIGINT/SIGTERM/SIGHUP only (there is no
// hotkey without an event tap to observe one).
//
// caffeinate is pushed onto the SAME RestoreState as the full mode, so teardown
// rides the existing LIFO Cleanup ("released releaser=caffeinate") and the
// stdout cleanup banner printed by run's deferred cleanup. The select also
// watches the child: if caffeinate dies unexpectedly (external kill, or its
// `-w` watch firing) we stop waiting and let Cleanup run rather than block
// forever holding nothing.
// outW/errW are the caller's debug-gated stdout/stderr writers (silent unless
// --debug or `debug: true`); the caffeinate-only path prints its active banner
// and any start error through them so none mode honors the same gate.
//
// stop is the signal.NotifyContext cancel; timerDur (from --timer) arms the same
// auto-disable countdown as the full path — on expiry armTimer cancels ctx and
// the select below wakes on ctx.Done() for a clean exit 0. timerDur == 0 disarms.
func runCaffeinateOnly(ctx context.Context, stop func(), allowDisplaySleep bool, timerDur time.Duration, rs *state.RestoreState, log *slog.Logger, outW, errW io.Writer) int {
	proc, err := caffeinate.Start(ctx, os.Getpid(), allowDisplaySleep, log)
	if err != nil {
		// A signal can cancel ctx in the small window before caffeinate.Start
		// forks the child; exec.CommandContext then returns context.Canceled
		// WITHOUT launching anything. That is a normal user-initiated shutdown,
		// not a missing-binary failure — exit 0 (mirrors how WaitForGrants /
		// RecoverFromCrash special-case context.Canceled in the full path).
		if errors.Is(err, context.Canceled) {
			return exitOK
		}
		_, _ = fmt.Fprintf(errW,
			"dndmode: start caffeinate failed: %v. Ensure /usr/bin/caffeinate exists and re-run.\n", err)
		return exitPlatformErr
	}
	rs.Push(proc) // released on Cleanup (LIFO); Name() == "caffeinate"

	// Arm the per-run auto-disable timer for none mode too (--timer works for any
	// overlay_style). Same mechanism as the full path: on expiry stop() cancels
	// ctx, the select below wakes on ctx.Done(), Cleanup tears down caffeinate →
	// exit 0. Disarmed on return so an early SIGINT leaves no armed timer.
	stopTimer := armTimer(timerDur, stop, log)
	defer stopTimer()

	_, _ = fmt.Fprintln(outW, "dndmode: active (caffeinate-only, no overlay). press Ctrl-C.")

	select {
	case <-ctx.Done():
		// Normal shutdown (SIGINT/SIGTERM/SIGHUP → signal.NotifyContext cancel).
		return exitOK
	case <-proc.Done():
		// caffeinate exited. Two sources are indistinguishable from the channel
		// alone: (a) our OWN ctx-cancel killed it (exec.CommandContext tears the
		// child down on cancel, closing Done at the same instant ctx.Done fires —
		// Go picks a ready select case at random), or (b) a GENUINE unexpected
		// death (external kill, or the -w watch firing) while we still hold the
		// awake-lock. ctx.Err() disambiguates: a live ctx means case (b).
		if ctx.Err() != nil {
			return exitOK // case (a): clean shutdown that won the select race
		}
		// case (b): the SOLE function of none mode (keeping the Mac awake) is
		// gone. Surface it in the log AND via a non-zero exit so a backgrounded
		// none-mode run does not masquerade as healthy — mirrors the full path's
		// watchdog-trip exit-code discipline. exitPlatformErr is reused as
		// the "abnormal awake-subsystem stop" code.
		log.Warn("caffeinate exited unexpectedly", slog.Any("err", proc.Err()))
		return exitPlatformErr
	}
}
