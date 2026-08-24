//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/eventtap"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
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
		// --watch is refused for a sharper reason than the rest: both it and
		// the capture want the keyboard through a suppressing tap, so the two
		// would be arming sessions into each other's input.
		{name: "watch conflicts", flags: setPasswordFlags{watch: true}, want: "--watch"},
		{
			name:  "several conflict, style is named first",
			flags: setPasswordFlags{style: "glass", timer: "5m", focus: "true"},
			want:  "--style",
		},
		{
			name:  "watch alongside another, the other is named first",
			flags: setPasswordFlags{timer: "5m", watch: true},
			want:  "--timer",
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

// Test_promptFirstPass_CarriesTheAcceptanceMarker keeps the acceptance suite
// honest.
//
// acceptance_test.go lives in an external test package and cannot import
// `package main`, so it greps subprocess output for a hand-written literal. Its
// prompt assertions are NEGATIVE ("this refusal path printed no prompt"), which
// is the shape that rots without a sound: reword the prompt, the grep stops
// matching, the assertion stops being able to fail, and a regression that
// prints a prompt on a refusal path sails through. This test is the tripwire —
// it fails the moment the constant and the literal disagree.
func Test_promptFirstPass_CarriesTheAcceptanceMarker(t *testing.T) {
	t.Parallel()

	const marker = "Type your unlock sequence" // keep in sync with capturePromptMarker
	if !strings.Contains(promptFirstPass, marker) {
		t.Errorf("promptFirstPass = %q no longer contains %q; "+
			"update capturePromptMarker in acceptance_test.go or the assertions there can never fail",
			promptFirstPass, marker)
	}
}

// Test_captureFailure pins the split that decides the exit code: an outcome
// that describes the INPUT exits 1 (the config was not touched and the old code
// still works, whichever safeguard fired), an outcome that describes the
// MACHINE exits 2. Reporting a refused tap as exit 1 would send the operator
// hunting for a typo in a config file that is in fact fine — and main.go maps
// the identical eventtap.ErrTapInstallFailed to exit 2 on the session path, so
// the two commands must not disagree about one failure.
//
// The no-digit rule covers the input outcomes only. It exists so a message
// cannot leak how many steps were typed; a kernel return code in a tap error
// describes the machine, not the secret.
//
// The sentinels themselves are bare static strings (eventtap pins that
// separately); this checks the wrapping around them, including the
// pass-mismatch case, which is the one outcome that describes the two entries
// rather than one.
func Test_captureFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		want       int
		aboutInput bool // describes what was typed, so it may not carry a digit
	}{
		{name: "escape cancels", err: eventtap.ErrCaptureCancelled, want: exitConfigErr, aboutInput: true},
		{name: "the two passes disagree", err: eventtap.ErrCaptureMismatch, want: exitConfigErr, aboutInput: true},
		{name: "too many steps", err: eventtap.ErrCaptureTooLong, want: exitConfigErr, aboutInput: true},
		{name: "ring lagged", err: eventtap.ErrCaptureLostEvents, want: exitConfigErr, aboutInput: true},
		{name: "ceiling fired", err: eventtap.ErrCaptureTimedOut, want: exitConfigErr, aboutInput: true},
		{name: "signal aborted the branch", err: context.Canceled, want: exitConfigErr, aboutInput: true},
		{
			name:       "wrapped sentinel still matches",
			err:        fmt.Errorf("install: %w", eventtap.ErrCaptureMismatch),
			want:       exitConfigErr,
			aboutInput: true,
		},
		{name: "the machine refused the tap", err: eventtap.ErrTapInstallFailed, want: exitPlatformErr},
		{
			name: "a wrapped tap refusal still matches",
			err:  fmt.Errorf("%w: rc=-1", eventtap.ErrTapInstallFailed),
			want: exitPlatformErr,
		},
		{name: "anything else is the machine too", err: errors.New("run loop went away"), want: exitPlatformErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg, code := captureFailure(tt.err)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
			if msg == "" {
				t.Fatal("message is empty; the operator learns nothing about which safeguard fired")
			}
			if tt.aboutInput && strings.ContainsAny(msg, "0123456789") {
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

	loader, _, created, err := prepareConfigForCapture(cfgPath)
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

	if _, _, _, err := prepareConfigForCapture(cfgPath); err == nil {
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

	if _, _, _, err := prepareConfigForCapture(cfgPath); err == nil {
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

	if _, _, created, err := prepareConfigForCapture(cfgPath); err != nil {
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
	debugOff := false
	code := runSetPasswordAt(context.Background(), cfgPath, int(f.Fd()), &out, &errOut, &debugOff, nil)

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

// Test_runSetPasswordAt_ConfigDebugRaisesTheGate pins that `debug: true` in the
// config file un-silences this branch, exactly as the --debug flag does.
//
// The branch runs BEFORE run()'s Step 5, which is where a session applies
// cfg.Debug, so the gate is still down when it starts and it has to raise the
// gate itself off the config it loads. Getting this wrong hurts more here than
// anywhere else: a user who configured debug in the file rather than on the
// command line would get a --set-password that fails in complete silence, and
// most of its diagnostics fire when the keyboard is already dead.
//
// The gate is what is checked, not the output: errW in this test is a plain
// buffer with no gate around it, so only the *bool the real gatedWriter holds
// can show whether the gate went up.
func Test_runSetPasswordAt_ConfigDebugRaisesTheGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "debug true raises it", body: "debug: true\nunlock_code: ctrl+option+cmd+x\n", want: true},
		{name: "an ordinary config leaves it down", body: "unlock_code: ctrl+option+cmd+x\n", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfgPath := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(cfgPath, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			// A regular file stands in for the terminal, so the run stops at the
			// tty refusal — which is AFTER the load, i.e. after the only line
			// under test, and before anything tries to install a tap.
			f, err := os.Create(filepath.Join(t.TempDir(), "not-a-tty"))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer func() { _ = f.Close() }()

			var out, errOut bytes.Buffer
			debugOn := false
			code := runSetPasswordAt(context.Background(), cfgPath, int(f.Fd()), &out, &errOut, &debugOn, nil)
			if code != exitConfigErr {
				t.Fatalf("exit code = %d, want %d", code, exitConfigErr)
			}
			if debugOn != tt.want {
				t.Errorf("debug gate = %v, want %v", debugOn, tt.want)
			}
		})
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
	debugOff := false
	code := runSetPasswordAt(context.Background(), cfgPath, int(os.Stdin.Fd()), &out, &errOut, &debugOff, nil)

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

// Test_runSetPasswordAt_RefusesBesideALiveInstance pins the live-peer gate.
//
// The capture installs its own kCGHIDEventTap with kCGHeadInsertEventTap and
// suppresses everything it sees, so a capture started while a session holds the
// shield would sit in FRONT of that session's tap and eat the unlock code the
// owner types — leaving a machine that looks dead to the only person allowed to
// wake it. The refusal is therefore not a nicety: it is the difference between
// "re-run later" and "the shield cannot be lifted for the next two minutes".
//
// Exit 5 is lifted verbatim from run() Step 5c. The INCONCLUSIVE read is where
// this branch deliberately parts company with it: Step 5c may warn and carry on
// because Step 10.5 RecoverFromCrash re-reads the same file moments later and
// turns a persistent failure into exit 7, while nothing downstream of the probe
// here re-examines anything — the next thing that happens is a suppressing tap.
// So "I cannot tell whether a session is live" has to exit 7 rather than
// proceed, and this test is what stops it drifting back to a warning.
//
// "Inconclusive" covers more than unparseable bytes, which is why three of the
// four cases below are well-formed JSON. A snapshot whose pid is absent, zero
// or negative parses perfectly and still leaves liveness unestablished, because
// IsLiveInstance refuses to hand such a value to kill(pid, 0) — and it reports
// that refusal with the same triple it uses for "no runtime.json at all". The
// file being on disk is the whole difference, and it has to be honoured here.
func Test_runSetPasswordAt_RefusesBesideALiveInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantPID  bool
		wantCode int
	}{
		// This process is unquestionably alive, so kill(pid, 0) succeeds.
		{
			name:     "live peer",
			body:     fmt.Sprintf(`{"pid":%d,"started_at":"2026-08-25T00:00:00Z"}`, os.Getpid()),
			wantPID:  true,
			wantCode: exitConcurrentInstance,
		},
		// PID 0 is the corrupted-snapshot case IsLiveInstance short-circuits
		// without probing (kill(0, sig) means "my whole process group"), and it
		// reports the result as the same (false, 0, nil) triple it uses for "no
		// file at all". On this branch those two are not interchangeable: a
		// runtime.json IS on disk, something wrote it, and nothing has
		// established that its owner is gone. That is the unparseable-JSON case
		// wearing different clothes, so it takes the same exit 7 rather than
		// falling through to the tty refusal and, in a real terminal, to a tap.
		{
			name:     "invalid pid refuses",
			body:     `{"pid":0,"started_at":"2026-08-25T00:00:00Z"}`,
			wantCode: exitRuntimeJSON,
		},
		// Negative PIDs are the more dangerous half of the same corruption —
		// kill(-N, sig) signals every process the caller can reach — and they
		// arrive through the identical IsLiveInstance short-circuit, so they
		// have to take the identical refusal.
		{
			name:     "negative pid refuses",
			body:     `{"pid":-1,"started_at":"2026-08-25T00:00:00Z"}`,
			wantCode: exitRuntimeJSON,
		},
		// A JSON object that parses but carries no pid key at all: Go leaves
		// the field at its zero value, which is indistinguishable from an
		// explicit 0 and must not be treated any more leniently.
		{
			name:     "missing pid refuses",
			body:     `{"started_at":"2026-08-25T00:00:00Z"}`,
			wantCode: exitRuntimeJSON,
		},
		// Unparseable runtime.json: a live peer can neither be confirmed nor
		// ruled out. Must abort BEFORE the tty check, i.e. with exit 7 rather
		// than the exit 1 the fall-through case produces.
		{
			name:     "inconclusive read refuses",
			body:     "{not json",
			wantCode: exitRuntimeJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yml")
			if err := os.WriteFile(cfgPath, []byte("unlock_code: ctrl+option+cmd+x\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			// Sibling of the config, which is how the branch resolves it — the
			// same file run() Step 5c reads through $HOME.
			runtimePath := filepath.Join(dir, filepath.Base(runtimeRelPath))
			if err := os.WriteFile(runtimePath, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write runtime.json: %v", err)
			}
			// A regular file stands in for the terminal: the fall-through case
			// has to stop at the tty refusal rather than try to install a tap.
			f, err := os.Create(filepath.Join(dir, "not-a-tty"))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer func() { _ = f.Close() }()

			var out, errOut bytes.Buffer
			debugOff := false
			code := runSetPasswordAt(context.Background(), cfgPath, int(f.Fd()), &out, &errOut, &debugOff, discardLogger())

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
			if out.Len() != 0 {
				t.Errorf("the ungated writer was used on a failure path: %q", out.String())
			}
			if tt.wantPID && !strings.Contains(errOut.String(), strconv.Itoa(os.Getpid())) {
				t.Errorf("the refusal does not name the peer PID: %q", errOut.String())
			}
			// Every refusal on this path has to say what to do next, and every
			// one of them has to say it before anything installs a tap.
			if !strings.Contains(errOut.String(), peerBeforeCapture) {
				t.Errorf("the refusal does not carry the pre-capture guidance: %q", errOut.String())
			}
		})
	}
}

// Test_runSetPasswordAt_ConfigDebugRaisesTheGateBeforeTheError pins the
// ORDERING of the gate against the config-failure branch.
//
// prepareConfigForCapture hands a loaded cfg back on every failure that happens
// after Load precisely so `debug: true` in the file can un-silence those
// failures too. If the gate were raised only after the error branch, the one
// user who asked for diagnostics in the config — and whose config is the one
// that cannot be rewritten — would get a completely silent exit 1.
//
// The quoted key is what makes the fixture fail late: the surgery matches
// secret keys at column zero, so `"unlock_code"` survives the rewrite, the
// structural check sees a plaintext code still in the file and refuses. Load,
// and therefore cfg.Debug, has already succeeded by then.
func Test_runSetPasswordAt_ConfigDebugRaisesTheGateBeforeTheError(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	body := "debug: true\n\"unlock_code\": ctrl+option+cmd+x\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	debugOn := false
	code := runSetPasswordAt(context.Background(), cfgPath, int(os.Stdin.Fd()), &out, &errOut, &debugOn, discardLogger())

	if code != exitConfigErr {
		t.Fatalf("exit code = %d, want %d", code, exitConfigErr)
	}
	if !debugOn {
		t.Error("the gate stayed down on a post-Load config failure; `debug: true` would explain nothing")
	}
	if errOut.Len() == 0 {
		t.Error("the gated writer got nothing")
	}
	if out.Len() != 0 {
		t.Errorf("the ungated writer was used on a failure path: %q", out.String())
	}
}

// discardLogger keeps IsLiveInstance's warn lines out of the test output. The
// branch's own diagnostics go to errW, which the tests assert on directly.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Test_probeLivePeer pins the three verdicts the peer probe can reach, and in
// particular that the inconclusive one is a REFUSAL.
//
// The probe is called twice — once before the capture and once immediately
// before the rename — so its verdicts are the only thing standing between
// --set-password and two failures that both end behind a shield: a tap
// head-inserted in front of a live session's, and a config republished with a
// secret that session will never accept. A read failure that returned exitOK
// would defeat both at once.
func Test_probeLivePeer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string // "" means: write no runtime.json at all
		write    bool
		wantCode int
	}{
		{name: "no runtime file", write: false, wantCode: exitOK},
		{
			name:     "dead pid",
			body:     fmt.Sprintf(`{"pid":%d,"started_at":"2026-08-25T00:00:00Z"}`, reapedPID(t)),
			write:    true,
			wantCode: exitOK,
		},
		{
			name:     "live pid",
			body:     fmt.Sprintf(`{"pid":%d,"started_at":"2026-08-25T00:00:00Z"}`, os.Getpid()),
			write:    true,
			wantCode: exitConcurrentInstance,
		},
		{
			name:     "unreadable file is a refusal, not a warning",
			body:     "}{",
			write:    true,
			wantCode: exitRuntimeJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), filepath.Base(runtimeRelPath))
			if tt.write {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatalf("write runtime.json: %v", err)
				}
			}

			detail, code := probeLivePeer(runtimepkg.NewManager(path, discardLogger()), discardLogger())

			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d (detail %q)", code, tt.wantCode, detail)
			}
			if tt.wantCode == exitOK && detail != "" {
				t.Errorf("a clear verdict carried a detail: %q", detail)
			}
			if tt.wantCode != exitOK && detail == "" {
				t.Error("a refusal carried no detail; the user would get a bare exit code")
			}
		})
	}
}

// Test_peerAfterCapture_PromisesTheConfigIsIntact pins the one thing the
// post-capture refusal MUST say.
//
// By the time it fires the user has typed the new code twice and has every
// reason to assume it took effect. If the message does not state that the file
// was left alone, they will walk away believing the new code is live, start a
// session, and find the shield answering only to the old one. The message also
// must not name a length or a value: it is printed after a capture, which is
// exactly the scrollback this feature exists to keep clean.
func Test_peerAfterCapture_PromisesTheConfigIsIntact(t *testing.T) {
	t.Parallel()

	if !strings.Contains(peerAfterCapture, "NOT changed") {
		t.Errorf("the post-capture refusal does not say the config survived: %q", peerAfterCapture)
	}
	if !strings.Contains(peerAfterCapture, "old unlock code still works") {
		t.Errorf("the post-capture refusal does not say which code is live: %q", peerAfterCapture)
	}
	for _, digit := range "0123456789" {
		if strings.ContainsRune(peerAfterCapture, digit) {
			t.Errorf("the post-capture refusal carries a digit, which could only describe the secret: %q", peerAfterCapture)
		}
	}
}

// reapedPID returns the PID of a process that has already exited and been
// waited for, so kill(pid, 0) answers ESRCH.
//
// A hardcoded number will not do: PID 1 is launchd and 99999 is inside the
// macOS PID range, so both can be alive on somebody's machine and would turn
// this into a flake that only fires on the maintainer's laptop.
func reapedPID(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a throwaway process: %v", err)
	}
	return cmd.Process.Pid
}

// Test_runSetPasswordAt_ReservesBeforeInstallingATap is a gold test on the
// SOURCE TEXT of setpassword.go, and it is textual for the same reason the
// nosplit gold tests in internal/macos/eventtap are: the property is an
// ORDERING inside a function that no test can execute. Everything past the tty
// check needs a granted Accessibility permission and a real keyboard, so the
// lock-then-capture sequence is unreachable under `go test` — but getting it
// backwards is silent, and the failure it produces lands on a locked-out user.
//
// The invariant: the publish lock is taken BEFORE eventtap.CaptureConfirmed
// installs the capture tap, and released no earlier than the save.
//
// Why it has to hold. The capture tap is head-inserted at kCGHIDEventTap and
// returns NULL for every event, so a capture that starts beside a live session
// sits in FRONT of that session's tap and swallows the unlock code its owner is
// typing — the shield stays up and the machine looks dead until a capture
// ceiling fires. The arrival probe cannot rule that out on its own:
// WaitForGrants sits between the two and is unbounded by construction, so a
// session is free to publish and install its tap inside that window. Only a
// reservation held from ahead of the tap through the rename makes the session
// refuse to start instead, and keeps two --set-password runs from capturing at
// once and racing their saves.
func Test_runSetPasswordAt_ReservesBeforeInstallingATap(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("setpassword.go")
	if err != nil {
		t.Fatalf("read setpassword.go: %v", err)
	}
	text := string(src)

	acquire := strings.Index(text, "acquirePublishLock(")
	if acquire < 0 {
		t.Fatal("setpassword.go no longer calls acquirePublishLock; the capture is unreserved")
	}
	if extra := strings.Index(text[acquire+1:], "acquirePublishLock("); extra >= 0 {
		t.Error("setpassword.go takes the publish lock more than once; flock is per-open, " +
			"so a second acquire on a lock this process already holds would refuse as busy")
	}

	capture := strings.Index(text, "eventtap.CaptureConfirmed(")
	if capture < 0 {
		t.Fatal("setpassword.go no longer calls eventtap.CaptureConfirmed; update this test")
	}
	if acquire > capture {
		t.Error("the publish lock is taken AFTER the capture tap goes in: a session that starts " +
			"during WaitForGrants would have its unlock code swallowed by that tap")
	}

	save := strings.Index(text, "SaveUnlockHash(")
	if save < 0 {
		t.Fatal("setpassword.go no longer calls SaveUnlockHash; update this test")
	}
	if release := strings.Index(text, "releaseLock()"); release < 0 || release > save {
		t.Error("the publish lock is not released by a defer registered ahead of the save; " +
			"the reservation has to outlive the rename it protects")
	}
}
