//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/matcher"
	"github.com/dsbasko/dndmode/internal/state"
)

// watchSessionDeps carries what one watch activation needs. It is a struct
// rather than nine parameters because three of its fields are POINTERS into
// the shell's state, and that is the part worth making obvious: the session
// closure reads cfg, unlockVerifier and cfgFingerprint from the shell scope,
// so refreshing them here is what makes a re-read take effect at all.
type watchSessionDeps struct {
	loader         *config.Loader
	cfgPath        string
	cfg            *config.Config
	unlockVerifier *matcher.Verifier
	cfgFingerprint *[32]byte
	log            *slog.Logger
	outW           io.Writer
	errW           io.Writer
	session        func(context.Context, context.CancelFunc, *state.RestoreState, bool) int
	comboLabel     string
	// beep is injectable so tests can exercise the failure path without
	// making the machine chirp. Nil means platformBeep — production never sets
	// it, which keeps the real path one line away from the doc comment on
	// fail() rather than behind a wiring indirection.
	beep func()
}

// runOneWatchSession raises one shield and returns when it comes down.
//
// It never propagates a failure as a process exit: a session that cannot
// start leaves watch mode waiting for the next press. That choice is what
// makes the noise below mandatory rather than decorative — the user pressed
// the combination and is about to walk away from a machine that did not lock.
func runOneWatchSession(parent context.Context, d watchSessionDeps) {
	// Re-read the config on EVERY activation. This is a correctness
	// requirement, not a convenience: `dndmode --set-password` run from another
	// terminal while watch mode waits would otherwise leave the in-memory
	// verifier pointing at the code the user just replaced — and the shield
	// would then answer only to a secret they have been told is gone. That is
	// the same fail-deadly the Step 13.3 fingerprint check exists to prevent,
	// arriving by a different route.
	//
	// It is also not "hot reload" in the sense config.Loader disclaims: there
	// is no watcher and no subscription, and Load still happens exactly once
	// per session. What changed is the number of sessions.
	//
	// Only the SECRET is refreshed. Look and behaviour (overlay style, mute,
	// focus, timer) stay as resolved at startup, because those were merged with
	// command-line flags whose precedence cannot be re-derived from the file
	// alone — and getting them stale costs a wrong colour, not a lockout.
	if cfg, raw, _, err := d.loader.LoadWithSource(); err != nil {
		d.fail("config is no longer readable: %v", err)
		return
	} else if verifier, _, _, rerr := config.ResolveUnlockCode(&cfg); rerr != nil {
		d.fail("config no longer resolves to a usable unlock code: %v", rerr)
		return
	} else {
		d.cfg.UnlockCode = cfg.UnlockCode
		d.cfg.Hotkey = cfg.Hotkey
		d.cfg.UnlockSalt = cfg.UnlockSalt
		d.cfg.UnlockHash = cfg.UnlockHash
		*d.unlockVerifier = verifier
		*d.cfgFingerprint = configFingerprintOf(raw)
	}

	// A child of the watch context and a FRESH RestoreState. Both are
	// per-session by necessity: cancelling this ctx must end the shield without
	// ending the process, and RestoreState.Cleanup is sync.Once-guarded, so
	// reusing the shell's would quietly decline to release anything the second
	// time round.
	sessCtx, sessCancel := context.WithCancel(parent)
	defer sessCancel()
	sessRS := state.NewRestoreState(d.log)

	code := d.session(sessCtx, sessCancel, sessRS, false /*ownsRunLoop*/)

	// The session's own LIFO unwind — the shell's deferred Cleanup covers only
	// the shell's stack, and would not run until process exit in any case.
	if err := sessRS.Cleanup(); err != nil {
		d.log.Error("session cleanup reported errors", slog.Any("err", err))
	}

	if code != exitOK {
		d.fail("session ended with exit code %d", code)
		return
	}
	if parent.Err() == nil {
		_, _ = fmt.Fprintln(d.outW, watchingBanner(d.comboLabel))
	}
}

// fail reports an activation that did not produce a shield, loudly and on two
// channels.
//
// The sound is the point. The printed line goes to a terminal that may be
// behind other windows, and the failure mode being guarded against is a user
// who pressed the combination, assumed the machine locked, and left. NSBeep is
// safe to use here for the same reason the message may name the combination:
// nothing about activation is secret. Nothing on the UNLOCK path may make a
// sound, and does not.
func (d watchSessionDeps) fail(format string, args ...any) {
	_, _ = fmt.Fprintf(d.errW, "dndmode: NOT LOCKED — "+format+"\n", args...)
	_, _ = fmt.Fprintf(d.errW, "dndmode: still watching. press %s to try again.\n", d.comboLabel)
	if d.beep != nil {
		d.beep()
		return
	}
	platformBeep()
}
