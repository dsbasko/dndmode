//go:build darwin || linux

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
)

const (
	// configRelPath is the user-relative config path. POSIX-stable via
	// os.UserHomeDir (mitigation).
	configRelPath = ".config/dndmode/config.yml"

	// runtimeRelPath is the user-relative runtime.json path — the single
	// source for that name. Step 5c reads it through $HOME; the
	// --set-password branch resolves it as a sibling of the config path it
	// was handed, so both must agree on the file name or the two live-peer
	// checks would be looking at different files.
	runtimeRelPath = ".config/dndmode/runtime.json"

	// Granular exit codes per the design notes (Phase 3 expansion). Slots 6/7
	// are Phase 5 (Shortcuts missing / runtime JSON
	// non-recoverable); slot 8 added by Phase 4 for top-level
	// panic recovery (per the design notes the design notes
	// originally suggested code 7, but exitRuntimeJSON owns that
	// slot in Phase 5, so the next-available 8 is the minimum-regression
	// resolution). Stderr wording per the UI spec Copywriting Contract.
	exitOK                  = 0 // success
	exitConfigErr           = 1 // P1 — bad YAML, invalid, ambiguous or too-short unlock code, bad flag value
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
// unlock code (security stance: reveal nothing — see config.Config.Debug and
// ).
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
// --timer) returns 0 with no error, meaning "no deadline: run until the unlock code or
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
//
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
// cancel — the ctx cancel from Step 3, NOT the NotifyContext stop, so the timer
// never unregisters a signal handler — so expiry drives the EXACT clean
// shutdown path as the unlock code or a signal: ctx cancels → cocoa.RunApp's
// watcher posts the synthetic stop event and [NSApp run] returns nil (full mode)
// / the caffeinate select wakes on ctx.Done() (none mode) → LIFO Cleanup → exit
// 0. A non-positive d disarms the timer (returns a no-op func) so callers can
// pass an unresolved --timer unconditionally. The returned func stops the timer
// and MUST be deferred by the caller: an early exit (unlock code/signal before expiry)
// would otherwise leave a stray AfterFunc goroutine armed to fire cancel after
// run() has already returned. cancel is idempotent, so a benign race between a
// near-simultaneous signal and the timer is harmless. time.AfterFunc runs its
// callback on its own goroutine; context.CancelFunc is documented thread-safe.
func armTimer(d time.Duration, cancel context.CancelFunc, log *slog.Logger) func() {
	if d <= 0 {
		return func() {}
	}
	t := time.AfterFunc(d, func() {
		log.Info("auto-disable timer elapsed; shutting down", slog.Duration("after", d))
		cancel()
	})
	return func() { t.Stop() }
}

func main() {
	os.Exit(run())
}

// cliFlags is the parsed command line. Every field is a pointer into the
// FlagSet, exactly what flag.Bool / flag.String return; run() dereferences
// them after Parse.
type cliFlags struct {
	debug, setPassword, watch, kill, status *bool
	style, mute, focus, timer               *string
}

// defineFlags registers every flag on fs and returns the pointers. It is the
// single place flags are declared, so usage_test can walk the FlagSet and
// check that --help (usageText) names each one.
func defineFlags(fs *flag.FlagSet) cliFlags {
	var fl cliFlags
	// --debug un-silences ALL console output for the run (banners + diagnostics +
	// debug-level logging). Default OFF = silent; only the exit code speaks.
	fl.debug = fs.Bool("debug", false, "enable all console output (banners + diagnostics + debug logging); default: silent, exit codes only")
	// --style overrides overlay_style from the YAML config (QUICK-gh8 follow-up):
	// when non-empty it WINS over cfg.OverlayStyle so an operator can pick a look
	// for a single run without editing ~/.config/dndmode/config.yml. Empty (the
	// default) means "use whatever the config says". Validated at Step 5b.1 with
	// the same ValidateOverlayStyle gate as the config value.
	fl.style = fs.String("style", "", "override overlay_style for this run (black|matrix|terminal[:go|python|typescript|rust|ys|sql|psql|mysql|oracle|varaq|elvish|php]|dvd|gold|bamboo|glass[:radius]|none); empty = use config")
	// --mute / --focus override the config keys for a single run (same
	// precedence as --style: non-empty WINS over YAML, empty = use config).
	// Tri-state strings ("" | "true" | "false") rather than flag.Bool because a
	// plain bool flag cannot express "absent → fall back to config" — a missing
	// --mute must mean "use cfg.Mute (default true)", not "false". Parsed via
	// strconv.ParseBool at Step 8b; junk → exitConfigErr (mirrors invalid --style).
	fl.mute = fs.String("mute", "", "override mute for this run (true|false); empty = use config")
	fl.focus = fs.String("focus", "", "override focus/DND for this run (true|false); empty = use config")
	// --timer sets a per-run auto-disable deadline: after the given Go duration
	// (time.ParseDuration grammar — "30m", "1h30m", "90s") elapses while dndmode is
	// ACTIVE, dndmode tears down and exits 0 exactly as if the unlock code were
	// pressed. Empty (the default) means no deadline — run until the unlock code or a
	// signal (SIGINT/SIGTERM/SIGHUP). Per-run ONLY: there is intentionally no config
	// key (a persistent auto-off default would surprise; typing --timer is the
	// deliberate opt-in). Works for EVERY overlay_style, including none/caffeinate,
	// because the timer merely triggers the same ctx-cancel shutdown both modes
	// already await (see armTimer). Parsed at Step 5b.2 via parseTimer; junk /
	// non-positive → exitConfigErr (mirrors invalid --style), naming --timer.
	fl.timer = fs.String("timer", "", "auto-disable after this long, then exit 0 (Go duration, e.g. 30m, 1h30m, 90s); empty = run until the unlock code or a signal")
	// --set-password is a one-shot maintenance command, not a session modifier:
	// it captures a new unlock sequence from real keystrokes (twice, so a typo
	// cannot lock the user out), rewrites unlock_code in the config as a salted
	// unlock_salt / unlock_hash pair, and exits. Dispatched at Step 2.5 below,
	// BEFORE Step 3, so it never builds a RestoreState and never touches Focus,
	// audio, the IOPMAssertion or the single-instance lock. Every other session
	// flag is refused alongside it (setPasswordFlags.conflictingFlag).
	fl.setPassword = fs.Bool("set-password", false, "capture a new unlock sequence and store it hashed in the config, then exit")
	// --watch turns the one-shot session into a resident trigger: instead of
	// locking immediately, dndmode registers the activate_hotkey combination
	// with Carbon, waits, and raises a full session every time it is pressed —
	// returning to waiting after each unlock.
	//
	// It runs in the BACKGROUND: the command re-executes this binary detached
	// from the terminal, waits until that process is ready, and returns
	// (daemon.go). The background process is an ordinary one — `ps` shows it,
	// --status describes it, --kill or SIGTERM ends it — and there is still no
	// launchd job: nothing restarts it and nothing starts it at login.
	//
	// While it waits the process holds nothing: no shield, no tap, no power
	// assertion, no runtime.json, no Focus. It does not need Accessibility to
	// wait, either — RegisterEventHotKey is matched by the OS, so dndmode never
	// observes a keystroke that is not its own trigger. Accessibility IS still
	// required for the sessions that follow, which is why the shell checks for
	// it up front (Step 9) rather than at the first press.
	fl.watch = fs.Bool("watch", false, "start a background process that waits for the activate_hotkey combination and locks on each press; see --status and --kill")
	// --kill / --status are the controls for that background process. Like
	// --set-password they are commands rather than session flags: dispatched
	// at Step 2.6 below, before Step 3, so they build no RestoreState and
	// refuse every session flag (controlFlags.conflict).
	fl.kill = fs.Bool("kill", false, "stop the background --watch process, then exit 0 (also 0 when none was running)")
	fl.status = fs.Bool("status", false, "report the background --watch process, then exit: 0 when it is running, 9 when it is not")
	return fl
}
