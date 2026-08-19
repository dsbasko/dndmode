//go:build darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/eventtap"
)

// Test_setPasswordFlags_conflictingFlag pins the mutual-exclusion rule:
// --set-password rewrites the config and exits, so every flag that configures a
// SESSION is refused alongside it, and the refusal names which one. --debug is
// absent from the struct by design — it changes only whether diagnostics are
// visible, and this command is silent enough to need it.
func Test_setPasswordFlags_conflictingFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags setPasswordFlags
		want  string
	}{
		{name: "no session flags is legal", flags: setPasswordFlags{}, want: ""},
		{name: "style conflicts", flags: setPasswordFlags{style: "black"}, want: "--style"},
		{name: "timer conflicts", flags: setPasswordFlags{timer: "30m"}, want: "--timer"},
		{name: "mute conflicts", flags: setPasswordFlags{mute: "false"}, want: "--mute"},
		{name: "focus conflicts", flags: setPasswordFlags{focus: "true"}, want: "--focus"},
		{
			name:  "several conflict, style is named first",
			flags: setPasswordFlags{style: "glass", timer: "5m", focus: "true"},
			want:  "--style",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.flags.conflictingFlag(); got != tt.want {
				t.Errorf("conflictingFlag() = %q, want %q", got, tt.want)
			}
		})
	}
}

// steps builds n distinct modifier-less steps. The keycodes are arbitrary; only
// the LENGTH matters to every rule under test here.
func steps(n int) []hotkey.Spec {
	out := make([]hotkey.Spec, n)
	for i := range out {
		out[i] = hotkey.Spec{KeyCode: uint16(i)}
	}
	return out
}

// Test_validateCapturedCode_MatchesValidateUnlockCode pins that the capture
// path accepts and rejects exactly what the startup path does: this function
// rephrases config.ValidateUnlockCode's verdict, it does not hold a second
// opinion about which codes are usable.
func Test_validateCapturedCode_MatchesValidateUnlockCode(t *testing.T) {
	t.Parallel()

	cases := [][]hotkey.Spec{
		{},
		steps(1),
		{{Modifiers: hotkey.ModCtrl, KeyCode: 7}},
		steps(2),
		steps(3),
		steps(config.MinUnlockSteps),
		steps(config.WeakUnlockSteps),
		steps(hotkey.MaxSteps),
	}

	for i, c := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			t.Parallel()

			wantErr := config.ValidateUnlockCode(c) != nil
			if gotErr := validateCapturedCode(c) != nil; gotErr != wantErr {
				t.Errorf("validateCapturedCode err=%v, ValidateUnlockCode err=%v", gotErr, wantErr)
			}
		})
	}
}

// Test_validateCapturedCode_NeverEchoesTheLength is the reason this function
// exists at all. config.ValidateUnlockCode embeds len(steps) twice; printed
// after a capture that text would put the width of the secret the user just
// typed into the scrollback. The rephrased message may name the PUBLIC
// thresholds (they are in the README and the config template) and nothing else,
// so the assertion is that no digit outside that pair survives — checked for
// every rejected length, not a sample.
func Test_validateCapturedCode_NeverEchoesTheLength(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		strconv.Itoa(config.MinUnlockSteps):  true,
		strconv.Itoa(config.WeakUnlockSteps): true,
	}

	for n := 0; n < config.MinUnlockSteps; n++ {
		err := validateCapturedCode(steps(n))
		if err == nil {
			t.Fatalf("len=%d: want a rejection, got nil", n)
		}
		msg := err.Error()
		for _, r := range msg {
			if r < '0' || r > '9' {
				continue
			}
			if !allowed[string(r)] {
				t.Errorf("len=%d: message leaks digit %q: %s", n, string(r), msg)
			}
		}
		// The captured length itself must never appear, not even when it
		// coincides with nothing else in the sentence.
		if n > 0 && !allowed[strconv.Itoa(n)] && strings.Contains(msg, strconv.Itoa(n)) {
			t.Errorf("len=%d: message contains the captured step count: %s", n, msg)
		}
	}
}

// Test_captureFailure pins that every capture outcome exits 1 — the config was
// not touched and the old code still works, whichever safeguard fired — and
// that no message carries a digit. The sentinels themselves are bare static
// strings (eventtap pins that separately); this checks the wrapping around
// them, including the pass-mismatch case, which is the one outcome that
// describes the two entries rather than one.
func Test_captureFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "escape cancels", err: eventtap.ErrCaptureCancelled},
		{name: "the two passes disagree", err: eventtap.ErrCaptureMismatch},
		{name: "too many steps", err: eventtap.ErrCaptureTooLong},
		{name: "ring lagged", err: eventtap.ErrCaptureLostEvents},
		{name: "ceiling fired", err: eventtap.ErrCaptureTimedOut},
		{name: "signal aborted the branch", err: context.Canceled},
		{name: "wrapped sentinel still matches", err: fmt.Errorf("install: %w", eventtap.ErrCaptureMismatch)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg, code := captureFailure(tt.err)
			if code != exitConfigErr {
				t.Errorf("exit code = %d, want %d", code, exitConfigErr)
			}
			if msg == "" {
				t.Fatal("message is empty; the operator learns nothing about which safeguard fired")
			}
			if strings.ContainsAny(msg, "0123456789") {
				t.Errorf("message carries a digit: %s", msg)
			}
		})
	}
}

// Test_captureFailure_DistinguishesOutcomes guards against a future refactor
// collapsing the switch into a single generic line: the whole point of
// enumerating the sentinels is that the operator can tell an Escape from a
// mismatch from a timeout without any of them naming a step.
func Test_captureFailure_DistinguishesOutcomes(t *testing.T) {
	t.Parallel()

	errs := []error{
		eventtap.ErrCaptureCancelled,
		eventtap.ErrCaptureMismatch,
		eventtap.ErrCaptureTooLong,
		eventtap.ErrCaptureLostEvents,
		eventtap.ErrCaptureTimedOut,
	}
	seen := make(map[string]error, len(errs))
	for _, err := range errs {
		msg, _ := captureFailure(err)
		if prev, dup := seen[msg]; dup {
			t.Errorf("%v and %v produce the same line: %s", prev, err, msg)
		}
		seen[msg] = err
	}
}

// Test_prepareConfigForCapture_CreatesMissingConfig covers the fs.ErrNotExist
// half of the Lstat guard: a first run has no config, and the guard must fall
// THROUGH to Load so the default gets written. The reflexive
// "Lstat failed → give up" spelling would give every new user a --set-password
// that exits 1 and prints nothing, and a machine that already has a config
// would never reveal it.
func Test_prepareConfigForCapture_CreatesMissingConfig(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "dndmode", "config.yml")

	loader, created, err := prepareConfigForCapture(cfgPath)
	if err != nil {
		t.Fatalf("prepareConfigForCapture on a missing config: %v", err)
	}
	if !created {
		t.Error("created = false; a missing config must be reported as freshly written")
	}
	if loader == nil {
		t.Fatal("loader is nil on the success path")
	}
	if _, serr := os.Stat(cfgPath); serr != nil {
		t.Fatalf("the default config was not written: %v", serr)
	}
}

// Test_prepareConfigForCapture_RejectsDanglingSymlink covers the other half.
// Load() reads through os.ReadFile, so a dangling link is indistinguishable
// from a missing file to it: it would take its "no file" branch and rename a
// default config OVER the link, destroying the entry without a word.
// SaveUnlockHash's own guard cannot help — by then there is no link left. So
// the check has to run first, and the link has to survive the refusal.
func Test_prepareConfigForCapture_RejectsDanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	target := filepath.Join(dir, "nowhere", "config.yml")
	if err := os.Symlink(target, cfgPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, _, err := prepareConfigForCapture(cfgPath); err == nil {
		t.Fatal("a dangling symlink was accepted; Load would have replaced it with a default config")
	}

	fi, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatalf("the symlink entry is gone after the refusal: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the refusal wrote the symlink target")
	}
}

// Test_prepareConfigForCapture_RejectsUnsurgeableConfig pins the dry run: a
// config whose YAML shape the line surgery cannot handle — here a uniformly
// indented root mapping — is rejected while rejecting it is still free, not
// after the user has typed a secret twice under a tap that owns the keyboard.
func Test_prepareConfigForCapture_RejectsUnsurgeableConfig(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	body := "  unlock_code: ctrl+option+cmd+x\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, _, err := prepareConfigForCapture(cfgPath); err == nil {
		t.Fatal("an indented root mapping passed the dry run; the surgery cannot rewrite it")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != body {
		t.Errorf("the dry run modified the file: %q", string(after))
	}
}

// Test_prepareConfigForCapture_AcceptsAnAlreadyHashedConfig pins that running
// the command twice works: a config already carrying unlock_salt / unlock_hash
// is a legal input, and the dry run must not mistake the absence of a
// plaintext unlock_code for a shape it cannot handle.
func Test_prepareConfigForCapture_AcceptsAnAlreadyHashedConfig(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	body := "unlock_salt: " + strings.Repeat("A", 22) + "==\n" +
		"unlock_hash: " + strings.Repeat("A", 43) + "=\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, created, err := prepareConfigForCapture(cfgPath); err != nil {
		t.Fatalf("an already-hashed config was rejected: %v", err)
	} else if created {
		t.Error("created = true for an existing config")
	}
}

// Test_runSetPasswordAt_NonTTYRejectedBeforeTheTap pins that a descriptor which
// is not a terminal is refused BEFORE anything tries to install an event tap or
// wait for an Accessibility grant. Capture without a terminal is meaningless —
// nobody reads the prompts, MakeRaw fails on a pipe — and a piped run that got
// as far as WaitForGrants would block forever on a grant it will never receive,
// silently, because every diagnostic on this path rides the debug gate.
//
// Reaching the check at all proves the ordering: the tap would need a signed
// binary and a granted Accessibility permission, so a test that installed one
// could not run under `go test` in the first place.
func Test_runSetPasswordAt_NonTTYRejectedBeforeTheTap(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	f, err := os.Create(filepath.Join(t.TempDir(), "not-a-tty"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out, errOut bytes.Buffer
	code := runSetPasswordAt(context.Background(), cfgPath, int(f.Fd()), &out, &errOut, false, nil)

	if code != exitConfigErr {
		t.Errorf("exit code = %d, want %d", code, exitConfigErr)
	}
	if !strings.Contains(errOut.String(), "interactive") {
		t.Errorf("stderr does not explain that the command is interactive: %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("the ungated writer was used on a failure path: %q", out.String())
	}
}

// Test_runSetPasswordAt_ConfigFailureIsSilentAndUngated pins the output
// contract on the other rejection path: a config the surgery cannot rewrite
// exits 1, says so through the GATED writer only, and leaves the ungated one —
// which carries exactly the two prompts and the success line over a whole
// successful run — completely untouched.
func Test_runSetPasswordAt_ConfigFailureIsSilentAndUngated(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("  unlock_code: ctrl+option+cmd+x\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	code := runSetPasswordAt(context.Background(), cfgPath, int(os.Stdin.Fd()), &out, &errOut, false, nil)

	if code != exitConfigErr {
		t.Errorf("exit code = %d, want %d", code, exitConfigErr)
	}
	if out.Len() != 0 {
		t.Errorf("the ungated writer was used on a failure path: %q", out.String())
	}
	if errOut.Len() == 0 {
		t.Error("the gated writer got nothing; --debug would explain nothing")
	}
}

// Test_setPasswordPrompt pins the two prompt lines. Both end in CRLF because
// both are written while the tty is raw — ONLCR is cleared by MakeRaw, so a
// bare "\n" would drop a line without returning the cursor and the second
// prompt would start under the tail of the first.
//
// The recommendation is UNCONDITIONAL, which is what makes printing it safe: it
// is the same sentence whatever the user types, so unlike the conditional
// weak-code warning it proves nothing about the new secret.
func Test_setPasswordPrompt(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	prompt := setPasswordPrompt(&buf)

	prompt(1)
	first := buf.String()
	switch {
	case !strings.HasSuffix(first, "\r\n"):
		t.Errorf("first prompt does not end in CRLF: %q", first)
	case !strings.Contains(first, "Esc cancels"):
		t.Errorf("first prompt does not name the voluntary exit: %q", first)
	case !strings.Contains(first, strconv.Itoa(config.WeakUnlockSteps)+" or more steps recommended"):
		t.Errorf("first prompt does not carry the unconditional recommendation: %q", first)
	}

	buf.Reset()
	prompt(2)
	second := buf.String()
	if !strings.HasSuffix(second, "\r\n") {
		t.Errorf("second prompt does not end in CRLF: %q", second)
	}
	if !strings.Contains(second, "again") {
		t.Errorf("second prompt does not ask for a confirmation: %q", second)
	}
}

// Test_resultUpdated_CarriesNeitherLengthNorValue pins the success line: it
// names the file that changed and nothing about the secret that changed in it.
func Test_resultUpdated_CarriesNeitherLengthNorValue(t *testing.T) {
	t.Parallel()

	line := fmt.Sprintf(resultUpdated, "/home/u/.config/dndmode/config.yml")
	if !strings.HasSuffix(line, "\r\n") {
		t.Errorf("the success line does not end in CRLF: %q", line)
	}
	// The path is the only variable part; strip it and nothing describing the
	// code may remain.
	rest := strings.ReplaceAll(line, "/home/u/.config/dndmode/config.yml", "")
	if strings.ContainsAny(rest, "0123456789") {
		t.Errorf("the success line carries a digit outside the path: %q", line)
	}
}
