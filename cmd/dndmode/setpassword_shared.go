//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
)

// setPasswordFlags carries the flags --set-password refuses to be combined
// with. It exists so the mutual-exclusion rule is one pure function over four
// strings rather than a chain of pointer dereferences inside run().
//
// --debug is deliberately absent: it does not change what the command DOES,
// only whether its diagnostics reach the terminal, and diagnosing a
// --set-password that exits 1 without a word is exactly what it is for.
type setPasswordFlags struct {
	style  string
	timer  string
	mute   string
	focus  string
	watch  bool
	kill   bool
	status bool
}

// conflictingFlag returns the name of the first flag that cannot be combined
// with --set-password, or "" when the combination is legal.
//
// Every one of them configures a dndmode SESSION — the overlay look, the
// auto-disable deadline, audio muting, Focus. --set-password starts no
// session: it rewrites one key in the config file and exits. Silently ignoring
// them would be worse than refusing, because `dndmode --set-password --timer
// 30m` reads like "capture, then run for 30 minutes" and would instead capture
// and quit.
//
// --watch is refused for a sharper version of the same reason. It reads as
// "set a new code, then stand by with it", and the two halves would fight
// over the keyboard anyway: the capture holds a suppressing tap across both
// of its passes, which is exactly what a waiting watch would be arming
// sessions into. Doing one and then the other is two commands, and it is
// clearer as two.
//
// --kill and --status are refused because they are commands too: each one
// does its thing and exits, and there is no order in which "stop the watch
// process" or "describe it" combines with "capture a new code" that is
// clearer than running them separately.
func (f setPasswordFlags) conflictingFlag() string {
	switch {
	case f.style != "":
		return "--style"
	case f.timer != "":
		return "--timer"
	case f.mute != "":
		return "--mute"
	case f.focus != "":
		return "--focus"
	case f.watch:
		return "--watch"
	case f.kill:
		return "--kill"
	case f.status:
		return "--status"
	default:
		return ""
	}
}

// Prompt and result lines, terminated with CRLF rather than LF.
//
// All three are written while the tty is in raw mode: term.MakeRaw clears
// ONLCR, so a bare "\n" moves the cursor down WITHOUT returning it to column
// zero and the second prompt starts under the tail of the first. The success
// line is printed before the deferred restore too (the restore has to outlive
// the config write — see runSetPasswordAt), so it needs the same treatment.
const (
	promptFirstPass = "Type your unlock sequence, then press Return. " +
		"Esc cancels. Nothing is echoed. %d or more steps recommended.\r\n"
	promptSecondPass = "Type it again to confirm.\r\n"
	resultUpdated    = "dndmode: unlock code updated in %s\r\n"
)

// setPasswordPrompt builds the callback eventtap.CaptureConfirmed invokes
// between its two passes. It writes to the UNGATED writer: these two lines and
// the success line are the only output of this command that is not routed
// through the debug gate, and a prompt nobody can see would leave the operator
// staring at a dead terminal whose keyboard has stopped responding.
//
// The recommendation is unconditional — the same sentence for a three-step
// code and for a thirty-step one — which is what makes it safe to print. The
// conditional weak-code warning main() shows at startup is deliberately NOT
// reproduced here: see the note in runSetPasswordAt.
//
// The threshold is interpolated from config.WeakUnlockSteps rather than spelled
// as a digit so the advice cannot drift away from the constant the rest of the
// project judges codes by.
func setPasswordPrompt(w io.Writer) func(pass int) {
	return func(pass int) {
		if pass == 1 {
			_, _ = fmt.Fprintf(w, promptFirstPass, config.WeakUnlockSteps)
			return
		}
		_, _ = fmt.Fprint(w, promptSecondPass)
	}
}

// validateCapturedCode is config.ValidateUnlockCode's verdict rephrased
// without the step count.
//
// The stock message embeds len(steps) twice ("unlock code of %d steps is too
// short … so a %d-step code is exhausted in minutes"). On the STARTUP path that
// is harmless — it rides the debug gate and describes a secret the user can
// read in their own file. Printed right after a capture it would put the length
// of the secret that was just typed into the terminal scrollback, into tmux
// capture buffers and onto any screen share that happens to be running, which
// is the leak this whole feature exists to close. The user already knows how
// many keys they pressed; the terminal must not.
//
// The thresholds themselves stay in the text: MinUnlockSteps and
// WeakUnlockSteps are public, documented in the README and in the config
// template, and a refusal that does not say what would be accepted just makes
// the user guess with the keyboard held hostage.
//
// The verdict is delegated rather than re-derived: config.ValidateUnlockCode
// stays the single source of truth for WHICH codes are acceptable, and this
// function only decides how to say no.
func validateCapturedCode(steps []hotkey.Spec) error {
	if err := config.ValidateUnlockCode(steps); err == nil {
		return nil
	}
	switch {
	case len(steps) == 0:
		return errors.New("nothing was captured: an unlock code needs at least one step")
	case len(steps) == 1:
		return fmt.Errorf(
			"a single-step unlock code must carry at least one modifier (e.g. Ctrl+Option+Cmd+X); "+
				"a bare key would unlock on the first thing a bystander types — "+
				"use %d or more steps instead",
			config.MinUnlockSteps)
	default:
		return fmt.Errorf(
			"that unlock code is too short: every keypress is a fresh match attempt, "+
				"so use at least %d steps (%d or more recommended)",
			config.MinUnlockSteps, config.WeakUnlockSteps)
	}
}

// Tails for the peer refusals. They are constants rather than literals at the
// call sites because the same probe fires three times — on arrival, again under
// the publish lock just before the tap goes in, and once more immediately
// before the rename — and they have to agree on what the user is being told to
// do.
const (
	peerBeforeCapture = "Capturing a new unlock code now would head-insert an input tap in front of " +
		"that session and swallow the keystrokes meant to unlock it. " +
		"Make sure no dndmode is running, then re-run."
	peerAfterCapture = "The config was NOT changed and your old unlock code still works — " +
		"a session that started during the capture is matching against it. " +
		"Re-run --set-password once that session has ended."
	// lockBeforeCapture names no peer, because the lock does not say who holds
	// it: a session publishing runtime.json and a second --set-password already
	// capturing look identical from here, and either is a reason not to install
	// a tap.
	lockBeforeCapture = "Capturing a new unlock code now would head-insert an input tap in front of " +
		"whatever else is claiming the keyboard. " +
		"Wait for the other dndmode to finish, then re-run."
)

// probeLivePeer asks runtime.json whether another dndmode owns the keyboard.
// It returns ("", exitOK) only when the answer is a definite no; the caller
// prints "dndmode: <detail>. <tail>" and returns the code otherwise.
//
// Both non-OK outcomes are refusals, and that is the whole point of the
// helper. run() Step 5c can afford to warn-and-continue on an unreadable
// runtime.json because Step 10.5 RecoverFromCrash re-examines the same file a
// moment later and turns a persistent IO or permission failure into exit 7.
// This branch has no such second gate: everything after the probe installs a
// kCGHIDEventTap whose callback returns NULL for every event, so "I could not
// tell whether a session is live" has to mean "do not install it". An
// inconclusive read that failed open would put the capture's tap in front of a
// live session's and eat the unlock code its owner is typing — the machine
// looks dead to the one person allowed back in, for as long as the capture
// ceilings allow.
//
// Read-only in every branch: this command never writes runtime.json, never
// takes ownership and never deletes a stale file. Deciding an unreadable file
// is stale is recovery's job, and recovery only runs on the session path.
//
// # An unusable PID is inconclusive, not "no peer"
//
// IsLiveInstance collapses two very different answers into the same
// (false, 0, nil) triple: "there is no runtime.json" and "there is one, but its
// PID field is missing, zero or negative, so kill(pid, 0) was never asked". The
// second is defensible on the session path — kill(0, sig) means "my whole
// process group" and kill(-N, sig) is a broadcast, so probing such a PID is
// worse than not probing it, and Step 10.5 RecoverFromCrash re-reads the file
// moments later anyway. Here it is the SAME failure as unparseable JSON: a file
// exists, some dndmode or something pretending to be one wrote it, and its
// liveness cannot be established. Letting it through means the tap goes in
// front of a session that may well be running.
//
// So the snapshot is read once more, ahead of the delegation, purely to tell
// those two cases apart — Manager.Read is stateless and sync, and the extra
// read costs one open of a file this branch is about to make a decision on. A
// read error is NOT handled here: it falls through to IsLiveInstance, which
// already turns fs.ErrNotExist into the happy path and everything else into the
// wrapped error the switch below reports as exit 7. One phrasing per outcome.
func probeLivePeer(mgr *runtimepkg.Manager, log *slog.Logger) (detail string, code int) {
	if snap, rerr := mgr.Read(); rerr == nil && snap.PID <= 0 {
		log.Warn("peer pre-check inconclusive: snapshot has invalid PID", slog.Int("pid", snap.PID))
		return fmt.Sprintf(
			"%s names no usable PID, so a running dndmode cannot be ruled out", mgr.Path()), exitRuntimeJSON
	}

	alive, peerPID, err := runtimepkg.IsLiveInstance(mgr, runtimepkg.NewKernLiveChecker(), log)
	switch {
	case err != nil:
		log.Warn("peer pre-check inconclusive", slog.Any("err", err))
		return fmt.Sprintf(
			"cannot read %s, so a running dndmode cannot be ruled out: %v", mgr.Path(), err), exitRuntimeJSON
	case alive:
		return fmt.Sprintf("another instance is already active (PID=%d)", peerPID), exitConcurrentInstance
	default:
		return "", exitOK
	}
}

// prepareConfigForCapture runs everything that can reject a config BEFORE
// anyone is asked to type: the symlink inspection, the load (which creates a
// default on first run) and a dry run of the line surgery.
//
// # Lstat runs ahead of Load, and both of its outcomes are named
//
// Load() reads through os.ReadFile, so a config.yml that is a DANGLING symlink
// looks exactly like a missing file to it: it takes the fs.ErrNotExist branch,
// writes a default config and renames it over the link, destroying the link
// entry without a word. SaveUnlockHash carries its own dangling-link guard,
// but by then Load has already run and there is no link left to refuse. So the
// check has to happen here, first.
//
// The other branch matters just as much. fs.ErrNotExist from Lstat is the
// ordinary first run, and it MUST fall through to Load so the default config
// gets created. A reflexive `if _, err := os.Lstat(p); err != nil { return … }`
// would give every new user a --set-password that exits 1 and prints nothing
// (all of this branch's errors ride the debug gate), and no test running on a
// machine that already has a config would ever notice.
//
// # The dry run is only the structural half of the verification
//
// config.VerifyStructure, not the full pre-rename check: at this point there
// are no captured steps by construction and the salt/hash are placeholders, so
// "does this digest recognize what was typed" is unsatisfiable and folding it
// in would make the dry run fail on every single invocation.
//
// What it does catch is a config whose YAML shape the surgery cannot handle —
// a quoted key, a uniformly indented root mapping, flow style. Such a file has
// to be rejected while rejecting it is still cheap. The alternative is finding
// out after the user has typed a secret twice under a tap that owns the
// keyboard, which is the worst possible moment.
func prepareConfigForCapture(cfgPath string) (*config.Loader, config.Config, bool, error) {
	var zero config.Config

	fi, err := os.Lstat(cfgPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Ordinary first run — fall through to Load, which creates the default.
	case err != nil:
		return nil, zero, false, fmt.Errorf("cannot inspect config %s: %w", cfgPath, err)
	case fi.Mode()&os.ModeSymlink != 0:
		if _, serr := os.Stat(cfgPath); serr != nil {
			return nil, zero, false, fmt.Errorf(
				"config %s is a symlink whose target cannot be read: %w — "+
					"repoint or remove the link, then re-run", cfgPath, serr)
		}
	}

	loader := config.NewLoader(cfgPath)
	// cfg travels back to the caller for ONE field: Debug. This branch runs
	// ahead of run()'s Step 5, so `debug: true` in the file has not raised the
	// output gate yet and every diagnostic below would be discarded on a
	// machine whose owner asked for output in the config rather than on the
	// command line. Nothing else here reads cfg — the surgery works on raw
	// bytes on purpose.
	cfg, created, err := loader.Load()
	if err != nil {
		return nil, zero, false, err
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, cfg, created, fmt.Errorf("read config %s: %w", cfgPath, err)
	}
	// Placeholders of the real widths, so the dry run exercises the same
	// decode gate RewriteSecretAsHash applies to the values it will really
	// write. They are never persisted: the rewritten bytes are inspected and
	// dropped.
	saltB64 := base64.StdEncoding.EncodeToString(make([]byte, matcher.SaltLen))
	hashB64 := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))
	newRaw, err := config.RewriteSecretAsHash(raw, saltB64, hashB64)
	if err != nil {
		return nil, cfg, created, fmt.Errorf("cannot rewrite %s: %w", cfgPath, err)
	}
	if verr := config.VerifyStructure(raw, newRaw, saltB64, hashB64); verr != nil {
		return nil, cfg, created, fmt.Errorf("cannot rewrite %s: %w", cfgPath, verr)
	}
	return loader, cfg, created, nil
}
