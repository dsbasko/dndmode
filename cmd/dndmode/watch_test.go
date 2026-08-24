//go:build darwin

package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
	"github.com/dsbasko/dndmode/internal/state"
)

// These cover the watch loop's per-activation logic, which is the part of
// watch mode that can be exercised without a run loop, a keyboard or an
// Accessibility grant: config refresh, cleanup, and how a failed activation is
// reported. The Carbon side is covered by the smoke tests in
// internal/macos/globalhotkey.

type watchHarness struct {
	dir      string
	cfgPath  string
	out, err bytes.Buffer
	beeps    int

	// sessionCalls records the ownsRunLoop argument of each session
	// invocation, so a test can assert both how often a session ran and that
	// it was never handed the run loop it does not own.
	sessionCalls []bool
	sessionCode  int
}

func newWatchHarness(t *testing.T, configBody string) *watchHarness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return &watchHarness{dir: dir, cfgPath: path, sessionCode: exitOK}
}

func (h *watchHarness) deps(cfg *config.Config, v *matcher.Verifier, fp *[32]byte) watchSessionDeps {
	return watchSessionDeps{
		loader:         config.NewLoader(h.cfgPath),
		cfgPath:        h.cfgPath,
		cfg:            cfg,
		unlockVerifier: v,
		cfgFingerprint: fp,
		log:            slog.New(slog.NewTextHandler(&h.err, nil)),
		outW:           &h.out,
		errW:           &h.err,
		comboLabel:     config.DefaultActivateHotkey,
		beep:           func() { h.beeps++ },
		session: func(_ context.Context, _ context.CancelFunc, _ *state.RestoreState, ownsRunLoop bool) int {
			h.sessionCalls = append(h.sessionCalls, ownsRunLoop)
			return h.sessionCode
		},
	}
}

// loadInto primes the shell-side state the way run() would have.
func (h *watchHarness) loadInto(t *testing.T) (config.Config, matcher.Verifier, [32]byte) {
	t.Helper()
	cfg, raw, _, err := config.NewLoader(h.cfgPath).LoadWithSource()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	v, _, _, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return cfg, v, configFingerprintOf(raw)
}

func TestRunOneWatchSession_RunsSessionAndReturnsToWaiting(t *testing.T) {
	h := newWatchHarness(t, "unlock_code: s w o r d f i s h\n")
	cfg, v, fp := h.loadInto(t)

	runOneWatchSession(t.Context(), h.deps(&cfg, &v, &fp))

	if len(h.sessionCalls) != 1 {
		t.Fatalf("session ran %d times, want 1", len(h.sessionCalls))
	}
	if h.sessionCalls[0] {
		t.Error("session was told it owns the run loop; in watch mode the shell owns it")
	}
	if h.beeps != 0 {
		t.Errorf("a successful session beeped %d times", h.beeps)
	}
	if got := h.out.String(); !strings.Contains(got, "watching") {
		t.Errorf("did not announce a return to waiting; stdout=%q", got)
	}
}

// TestRunOneWatchSession_PicksUpANewUnlockCode is the load-bearing one. A
// --set-password from another terminal rewrites the config while watch mode
// waits; if the refresh does not happen, the next shield answers only to the
// secret the user was told had been replaced.
func TestRunOneWatchSession_PicksUpANewUnlockCode(t *testing.T) {
	h := newWatchHarness(t, "unlock_code: s w o r d f i s h\n")
	cfg, v, fp := h.loadInto(t)

	before := v
	beforeFP := fp

	// Someone runs --set-password (or edits the file) while we wait.
	if err := os.WriteFile(h.cfgPath, []byte("unlock_code: c o r r e c t h o r s e\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	runOneWatchSession(t.Context(), h.deps(&cfg, &v, &fp))

	if len(h.sessionCalls) != 1 {
		t.Fatalf("session ran %d times, want 1", len(h.sessionCalls))
	}
	if v == before {
		t.Fatal("the verifier was not refreshed: the shield would answer to the replaced secret")
	}
	if fp == beforeFP {
		t.Error("the fingerprint was not refreshed: the session would refuse itself at Step 13.3")
	}

	// The refreshed verifier must match the NEW code and reject the old one.
	newSteps, err := hotkey.ParseSequence("c o r r e c t h o r s e")
	if err != nil {
		t.Fatalf("parse new: %v", err)
	}
	if !v.Match(stepsToEvents(newSteps)) {
		t.Error("refreshed verifier does not match the new unlock code")
	}
	oldSteps, err := hotkey.ParseSequence("s w o r d f i s h")
	if err != nil {
		t.Fatalf("parse old: %v", err)
	}
	if v.Match(stepsToEvents(oldSteps)) {
		t.Error("refreshed verifier still matches the OLD unlock code")
	}
}

// TestRunOneWatchSession_FailedSessionIsLoud pins the rule that a press which
// did not produce a shield must never be quiet: the dangerous outcome is a
// user who assumed it worked and walked away.
func TestRunOneWatchSession_FailedSessionIsLoud(t *testing.T) {
	h := newWatchHarness(t, "unlock_code: s w o r d f i s h\n")
	h.sessionCode = exitPlatformErr
	cfg, v, fp := h.loadInto(t)

	runOneWatchSession(t.Context(), h.deps(&cfg, &v, &fp))

	if h.beeps != 1 {
		t.Errorf("beeped %d times on a failed activation, want 1", h.beeps)
	}
	if got := h.err.String(); !strings.Contains(got, "NOT LOCKED") {
		t.Errorf("failure was not announced; stderr=%q", got)
	}
}

// TestRunOneWatchSession_UnusableConfigDoesNotStartASession covers the case
// where the file was edited into something that no longer resolves: raising a
// shield with no usable secret would be a shield nothing can lower.
func TestRunOneWatchSession_UnusableConfigDoesNotStartASession(t *testing.T) {
	h := newWatchHarness(t, "unlock_code: s w o r d f i s h\n")
	cfg, v, fp := h.loadInto(t)

	if err := os.WriteFile(h.cfgPath, []byte("unlock_code: not+a+key\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	runOneWatchSession(t.Context(), h.deps(&cfg, &v, &fp))

	if len(h.sessionCalls) != 0 {
		t.Errorf("a session started on a config that does not resolve (%d calls)", len(h.sessionCalls))
	}
	if h.beeps != 1 {
		t.Errorf("beeped %d times, want 1", h.beeps)
	}
	if got := h.err.String(); !strings.Contains(got, "NOT LOCKED") {
		t.Errorf("failure was not announced; stderr=%q", got)
	}
}

func stepsToEvents(steps []hotkey.Spec) []matcher.KeyEvent {
	evs := make([]matcher.KeyEvent, len(steps))
	for i, s := range steps {
		evs[i] = matcher.KeyEvent{Modifiers: s.Modifiers, KeyCode: s.KeyCode}
	}
	return evs
}
