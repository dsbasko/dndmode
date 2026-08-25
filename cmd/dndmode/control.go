//go:build darwin

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dsbasko/dndmode/internal/macos/powerassert"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
	watchpkg "github.com/dsbasko/dndmode/internal/state/watch"
)

// --kill and --status are control commands for the background watch process.
// Like --set-password they are dispatched from run() before Step 3, so they
// build no RestoreState, print no cleanup banner and touch nothing but the
// two state files they read (and, for --kill, the process those files name).
//
// Their output is UNGATED. The debug gate exists so a terminal left visible
// under a glass/none shield cannot leak hints about the unlock secret; these
// commands print a PID, an uptime and the activation combination, none of
// which is secret (see internal/macos/globalhotkey), and a status command that
// answers with silence has no purpose. Only the slog diagnostics stay gated.

// exitNotWatching is what --status returns when no watch process is running,
// so `if dndmode --status; then …` works in a script. It is a distinct code
// rather than 1 because 1 already means "bad flag or config" — and `dndmode
// --status --watch` is exactly that, which must not read as "not watching".
const exitNotWatching = 9

// controlFlags carries the flags --kill and --status refuse to be combined
// with. Every session flag configures a session these commands never start;
// --watch and --set-password are commands in their own right; and the two
// control commands exclude each other because "stop it and tell me about it"
// has two honest answers depending on the order.
//
// --debug is deliberately absent, as it is from setPasswordFlags: it changes
// only whether the slog lines reach the terminal.
type controlFlags struct {
	style, timer, mute, focus string
	watch, setPassword        bool
	kill, status              bool
}

// conflict returns the control command being run ("--kill" or "--status")
// and the first flag it cannot be combined with, or "" when the combination
// is legal. --kill wins the name when both are set, so the message reads
// "--kill cannot be combined with --status".
func (f controlFlags) conflict() (self, other string) {
	self = "--status"
	if f.kill {
		self = "--kill"
	}
	switch {
	case f.kill && f.status:
		return self, "--status"
	case f.setPassword:
		return self, "--set-password"
	case f.watch:
		return self, "--watch"
	case f.style != "":
		return self, "--style"
	case f.timer != "":
		return self, "--timer"
	case f.mute != "":
		return self, "--mute"
	case f.focus != "":
		return self, "--focus"
	default:
		return self, ""
	}
}

// controlDeps is everything runStatus / runKill touch outside their
// arguments, so both are unit-tested against a temp directory and fakes.
type controlDeps struct {
	watch   *watchpkg.Manager
	runtime *runtimepkg.Manager
	prober  watchpkg.Prober
	live    powerassert.LiveChecker
	sig     watchpkg.Signaller
	now     func() time.Time
	stop    watchpkg.StopOptions
}

// runWatchControl is the dispatch target for --kill / --status: it checks the
// flag combination, resolves the state paths under $HOME and hands over to
// the command. outW/errW are the ungated stdout/stderr.
func runWatchControl(fl controlFlags, outW, errW io.Writer, log *slog.Logger) int {
	self, other := fl.conflict()
	if other != "" {
		_, _ = fmt.Fprintf(errW,
			"dndmode: %s cannot be combined with %s — it manages the background watch process and exits without starting a session.\n",
			self, other)
		return exitConfigErr
	}

	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: resolve home dir: %v.\n", err)
		return exitPlatformErr
	}
	deps := controlDeps{
		watch:   watchpkg.NewManager(filepath.Join(home, watchRecordRelPath), log),
		runtime: runtimepkg.NewManager(filepath.Join(home, runtimeRelPath), log),
		prober:  watchpkg.NewKernProber(),
		live:    powerassert.NewKernLiveChecker(),
		sig:     watchpkg.NewKernSignaller(),
		now:     time.Now,
	}
	if fl.kill {
		return runKill(deps, outW, errW)
	}
	return runStatus(deps, outW, errW)
}

// The output of --status, --kill and the --watch banner shares one shape: a
// "dndmode: <state>" headline, then indented "key   value" rows. Plain text,
// no colour — it lands in scrollback, in scripts and in watch.log alike, and
// the headline alone already answers the question.

// kvLine writes one indented "key   value" row under a headline. Keys are at
// most seven characters so the values line up.
func kvLine(w io.Writer, key, value string) {
	_, _ = fmt.Fprintf(w, "  %-7s %s\n", key, value)
}

// runStatus describes the watch process and, when one is running, whether it
// currently has a shield up. Exit 0 when running, exitNotWatching otherwise,
// exitRuntimeJSON when the record exists but cannot be trusted either way.
func runStatus(d controlDeps, outW, errW io.Writer) int {
	st, err := watchpkg.Inspect(d.watch, d.prober)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot tell whether a watch process is running: %v.\nInspect %s; if it is garbage, remove it.\n", err, d.watch.Path())
		return exitRuntimeJSON
	}
	switch {
	case st.Running:
		rec := st.Record
		_, _ = fmt.Fprintln(outW, "dndmode: watching")
		kvLine(outW, "pid", strconv.Itoa(rec.PID))
		kvLine(outW, "uptime", fmt.Sprintf("%s  (since %s)", formatUptime(d.now().Sub(rec.StartedAt)), formatSince(rec.StartedAt)))
		if rec.ActivateHotkey != "" {
			kvLine(outW, "hotkey", rec.ActivateHotkey+"  — press it to lock")
		}
		kvLine(outW, "shield", shieldValue(d, rec.PID))
		if rec.LogPath != "" {
			kvLine(outW, "log", abbreviateHome(rec.LogPath))
		}
		kvLine(outW, "stop", "dndmode --kill")
		return exitOK
	case st.Stale:
		_, _ = fmt.Fprintln(outW, "dndmode: not watching")
		kvLine(outW, "stale", fmt.Sprintf("record for PID %d, which is gone — the next --watch or --kill clears it", st.Record.PID))
		kvLine(outW, "start", "dndmode --watch")
		return exitNotWatching
	default:
		_, _ = fmt.Fprintln(outW, "dndmode: not watching")
		kvLine(outW, "start", "dndmode --watch")
		return exitNotWatching
	}
}

// shieldValue reads runtime.json to say whether the watch process (watchPID)
// has a session up right now. A session belongs to the watch process when the
// snapshot names its PID; a live snapshot naming another PID is a separate,
// one-shot dndmode. Anything unreadable or dead reads as "down" — runtime.json
// is deleted on a clean exit and its stale forms are crash recovery's job.
func shieldValue(d controlDeps, watchPID int) string {
	snap, err := d.runtime.Read()
	switch {
	case err != nil || snap.PID <= 0 || !d.live.IsAlive(snap.PID):
		return "down"
	case snap.PID == watchPID:
		return "UP — locked since " + formatSince(snap.StartedAt)
	default:
		return fmt.Sprintf("down — a separate dndmode session is active (PID %d)", snap.PID)
	}
}

// runKill stops the watch process. Exit 0 whenever "not running" is true on
// return — including when it already was — so a script can run it
// unconditionally; non-zero only when the process could not be stopped.
func runKill(d controlDeps, outW, errW io.Writer) int {
	res, err := watchpkg.Stop(d.watch, d.prober, d.sig, d.stop)
	switch {
	case errors.Is(err, watchpkg.ErrStopTimeout):
		_, _ = fmt.Fprintf(errW,
			"dndmode: the watch process (PID %d) was asked to stop but is still running. If it is stuck: kill -9 %d\n",
			res.Record.PID, res.Record.PID)
		return exitPlatformErr
	case errors.Is(err, watchpkg.ErrSignalFailed):
		_, _ = fmt.Fprintf(errW, "dndmode: cannot stop the watch process: %v\n", err)
		return exitPlatformErr
	case err != nil:
		if res.Outcome == watchpkg.StaleRemoved {
			_, _ = fmt.Fprintf(errW, "dndmode: not watching, but the stale record could not be removed: %v\nRun: rm -f %s\n", err, d.watch.Path())
			return exitRuntimeJSON
		}
		_, _ = fmt.Fprintf(errW, "dndmode: cannot tell whether a watch process is running, so nothing was signalled: %v.\nInspect %s; if it is garbage, remove it.\n", err, d.watch.Path())
		return exitRuntimeJSON
	}
	switch res.Outcome {
	case watchpkg.Stopped:
		_, _ = fmt.Fprintf(outW, "dndmode: stopped watching (PID %d)\n", res.Record.PID)
	case watchpkg.StaleRemoved:
		_, _ = fmt.Fprintf(outW, "dndmode: not watching — cleared a stale record for PID %d\n", res.Record.PID)
	default:
		_, _ = fmt.Fprintln(outW, "dndmode: not watching")
	}
	return exitOK
}

// formatSince renders a timestamp in local time, to the minute — "since when"
// is a glance, not a log line.
func formatSince(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04")
}

// formatUptime renders a duration the way `uptime` does — two units at most,
// the smaller ones dropped as the span grows — and never negative: a record
// from the future (clock skew) reads as 0s rather than as a minus sign.
func formatUptime(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	h := int(d.Hours())
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", h, int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	}
}

// abbreviateHome replaces a leading $HOME with "~" for display. The record
// stores the absolute path; only what the user reads is shortened.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}
