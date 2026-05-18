//go:build acceptance && darwin

// Acceptance tests for Phase 1-3. Run via `make acceptance` (NOT `make test`).
// These tests build the dndmode binary once in TestMain, then fork the
// binary as a subprocess with a tmp HOME, send signals, and verify exit
// codes + stdout/stderr per the design notes Validation Architecture.
//
// The binary is built once (cache-friendly) and reused across tests because
// `go run` would (a) require HOME pointing at tmpHome which makes the Go
// module cache spill into the test tmpdir (chmod-denied during cleanup) and
// (b) make signal delivery awkward — SIGINT to `go run` does NOT propagate
// to the child `dndmode` reliably. Building once and exec'ing the binary
// directly sidesteps both issues.
//
// Phase 3 manual-only PreFlight paths (NOT automated here — see
// the design notes Manual-Only Verifications):
//
// - exit 3 (polling SIGINT) — requires
//     `tccutil reset Accessibility com.dsbasko.dndmode` to put the binary
//     back into the not-yet-trusted state before launch.
// - exit 4 (SecureEventInput active) — requires an open sudo
//     prompt or password field in another tab while dndmode launches.
// - exit 5 (concurrent instance) — requires two dndmode
//     processes started in parallel.
//
// All three are exercised by docs/manual-test.md in Phase 6 (Polish).
package main_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// dndmodeBinary is the path to the dndmode binary built once by TestMain.
var dndmodeBinary string

// TestMain compiles the dndmode binary into a temp file shared across all
// acceptance tests. Doing the build here (rather than `go run` per test)
// avoids polluting per-test t.TempDir with the Go module cache and gives us
// a real PID to signal directly.
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "dndmode-acceptance-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: mktmp: %v\n", err)
		os.Exit(2)
	}
	defer os.RemoveAll(tmpDir)

	dndmodeBinary = filepath.Join(tmpDir, "dndmode")

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: resolve repo root: %v\n", err)
		os.Exit(2)
	}

	build := exec.Command("go", "build", "-o", dndmodeBinary, "./cmd/dndmode")
	build.Dir = repoRoot
	build.Stderr = os.Stderr
	build.Stdout = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: build dndmode: %v\n", err)
		os.Exit(2)
	}

	os.Exit(m.Run())
}

// dndmodeCmd starts the prebuilt dndmode binary with HOME pointing at
// tmpHome. Returns the cmd, captured stdout buffer, captured stderr buffer.
// Caller is responsible for Wait or Kill.
func dndmodeCmd(t *testing.T, ctx context.Context, tmpHome string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
	t.Helper()

	cmd := exec.CommandContext(ctx, dndmodeBinary)
	// Replace HOME only — config.NewLoader uses os.UserHomeDir which honors
	// $HOME on Darwin. We DO NOT replace the rest of the environment so that
	// the Go toolchain caches stay where they are (the binary is already
	// built — no compile happens at test runtime).
	cmd.Env = append(os.Environ(), "HOME="+tmpHome)

	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	return cmd, stdout, stderr
}

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer. Needed because
// cmd.Stdout/Stderr is written from a goroutine inside os/exec while the
// test goroutine concurrently polls .String() in waitForStdout.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestAcceptance_DefaultConfigCreatedOnMissing(t *testing.T) {
	tmpHome := t.TempDir()
	cfgPath := filepath.Join(tmpHome, ".config", "dndmode", "config.yml")

	// Sanity: file does not exist initially.
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("config exists before test: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, _ := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		t.Fatalf("did not see active banner in stdout within 10s; got:\n%s", stdout.String())
	}

	// Default config file must exist now.
	if _, err := os.Stat(cfgPath); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("default config not created at %s: %v", cfgPath, err)
	}

	// Banner must contain config path and default hotkey.
	out := stdout.String()
	if !strings.Contains(out, "dndmode: config=") {
		t.Errorf("stdout missing 'dndmode: config=': %s", out)
	}
	if !strings.Contains(out, "hotkey=Ctrl+Option+Cmd+X") {
		t.Errorf("stdout missing default hotkey: %s", out)
	}

	// Clean shutdown.
	signalAndWait(t, cmd, syscall.SIGINT, 5*time.Second)

	// File must contain Ctrl+Option+Cmd+X.
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(body), "Ctrl+Option+Cmd+X") {
		t.Errorf("config file missing default hotkey value: %s", body)
	}
}

func TestAcceptance_SIGINT_ExitZeroWithCleanupBanner(t *testing.T) {
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, _ := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		t.Fatalf("did not see active banner: %s", stdout.String())
	}

	signalAndWait(t, cmd, syscall.SIGINT, 5*time.Second)

	out := stdout.String()
	if !strings.Contains(out, "active. press Ctrl-C.") {
		t.Errorf("stdout missing active banner: %s", out)
	}
	if !strings.Contains(out, "cleaning up… done.") {
		t.Errorf("stdout missing cleanup banner: %s", out)
	}
}

func TestAcceptance_DoubleSIGINT_NoOp(t *testing.T) {
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, _ := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		t.Fatalf("did not see active banner: %s", stdout.String())
	}

	// First SIGINT.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("first SIGINT: %v", err)
	}
	// Second SIGINT 100ms later (during cleanup window).
	time.Sleep(100 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGINT) // may fail if process already gone; that's fine

	// Wait for exit.
	waitWithTimeout(t, cmd, 5*time.Second)

	out := stdout.String()
	cleanupCount := strings.Count(out, "cleaning up… done.")
	if cleanupCount != 1 {
		t.Errorf("cleanup banner appeared %d times, want exactly 1 (idempotency): %s", cleanupCount, out)
	}

	if cmd.ProcessState.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", cmd.ProcessState.ExitCode())
	}
}

func TestAcceptance_InvalidYAML_ExitOneWithLineCol(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".config", "dndmode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	garbage := "hotkey: ctrl+x\n  bad: indent\n"
	if err := os.WriteFile(cfgPath, []byte(garbage), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, _, stderr := dndmodeCmd(t, ctx, tmpHome)
	err := cmd.Run() // Run blocks until exit; we expect non-zero
	if err == nil {
		t.Fatal("expected non-zero exit, got nil")
	}

	exitCode := cmd.ProcessState.ExitCode()
	if exitCode != 1 {
		t.Errorf("exit code = %d, want 1", exitCode)
	}

	stderrStr := stderr.String()
	re := regexp.MustCompile(`\[\d+:\d+\]`)
	if !re.MatchString(stderrStr) {
		t.Errorf("stderr missing [L:C] pretty-error format: %s", stderrStr)
	}
}

func TestAcceptance_ModifierOnlyHotkey_ExitOne(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".config", "dndmode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("hotkey: Ctrl+Cmd\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, _, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit for modifier-only hotkey, got nil")
	}

	if exitCode := cmd.ProcessState.ExitCode(); exitCode != 1 {
		t.Errorf("exit code = %d, want 1 (modifier-only rejection)", exitCode)
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "invalid hotkey") {
		t.Errorf("stderr missing 'invalid hotkey' marker: %s", stderrStr)
	}
}

// TestAcceptance_LIFE06_PushOrder verifies (P2 D-13 + Phase 3 D-02) that the
// LIFO Cleanup chain releases resources in the exact order REQUIREMENTS.md
// LIFE-06 mandates: tap (first) → windows → assertion → runtime-file (last).
//
// Phase 1 verifier missed this because it only checked "did anything fail"
// — not the ordering. This test parses stderr looking for the per-Releaser
// success-log emitted by RestoreState.Cleanup (`released releaser=<name>`)
// and asserts strings.Index ordering.
//
// Phase 3 update: the third releaser is now a real powerassert.Assertion
// with Name() == "dndmode active" (per POW-01 + plan 03-03). The
// substring match here changed from `releaser=mock-assertion` to
// `releaser=dndmode active` accordingly. Phase 2 had this third slot
// occupied by state.NewMockReleaser("mock-assertion") — Phase 3 D-02
// replaces that placeholder with the real IOPMAssertion releaser.
//
// The acceptance binary uses the production dndmode (built once in TestMain)
// which routes through the real cocoa.Controller via main.go Push order.
// We only need a brief active session and a SIGINT — no GUI verification.
func TestAcceptance_LIFE06_PushOrder(t *testing.T) {
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// On HEADLESS CI without GUI: cocoa.Init may still succeed (NSApp
	// sharedApplication usually works without a connected display) but
	// CreateWindowsForAllScreens with 0 displays returns ErrNoDisplays
	// → exit 2. In that case the test cannot proceed; skip cleanly.
	// Phase 3 path: if AX/IM are not granted on the host the binary will
	// sit in WaitForGrants — we detect that via "dndmode: waiting" on stdout.
	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		stderrSnap := stderr.String()
		stdoutSnap := stdout.String()
		switch {
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached on this host (HEADLESS or lid-closed); test requires GUI session")
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; test requires both granted upfront")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		}
		t.Fatalf("did not see active banner; stdout:\n%s\nstderr:\n%s", stdoutSnap, stderrSnap)
	}

	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)

	s := stderr.String()
	// LIFE-06 cleanup execution order (LIFO unwind of P2 D-13 push order,
	// updated by Phase 3 D-02 — assertion slot is now a REAL powerassert.Assertion):
	//   1. mock-tap          (Phase 4 — currently mocked; first to release)
	//   2. windows           (controller — close all NSWindow)
	//   3. dndmode active    (REAL IOPMAssertion — Phase 3 replaces P2 mock-assertion)
	//   4. mock-runtime-file (Phase 5 — currently mocked; last to release)
	posTap := strings.Index(s, `releaser=mock-tap`)
	posWin := strings.Index(s, `releaser=windows`)
	posAssert := strings.Index(s, `releaser=dndmode active`)
	posRuntime := strings.Index(s, `releaser=mock-runtime-file`)

	if posTap < 0 || posWin < 0 || posAssert < 0 || posRuntime < 0 {
		t.Fatalf("missing release log entries (need all 4 'released' info logs):\n"+
			"  posTap=%d posWin=%d posAssert(dndmode active)=%d posRuntime=%d\n"+
			"stderr:\n%s",
			posTap, posWin, posAssert, posRuntime, s)
	}

	if !(posTap < posWin && posWin < posAssert && posAssert < posRuntime) {
		t.Errorf("cleanup order violated:\n"+
			"  tap@%d  windows@%d  dndmode-active@%d  runtime-file@%d\n"+
			"  expected: tap < windows < dndmode-active < runtime-file (P2 D-13 + Phase 3 D-02 LIFO unwind)\n"+
			"stderr:\n%s",
			posTap, posWin, posAssert, posRuntime, s)
	}
}

// TestAcceptance_Phase2_OverlayBootstrapsAndShutsDown is the Phase 2
// happy-path: dndmode starts, creates overlay windows, ctx-cancel via
// SIGINT triggers cocoa.RunApp to return cleanly, exit 0, cleanup banner
// appears on stdout. Verifies Phase 2 wire-up didn't break the existing
// Phase 1 acceptance contract.
func TestAcceptance_Phase2_OverlayBootstrapsAndShutsDown(t *testing.T) {
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		stderrSnap := stderr.String()
		stdoutSnap := stdout.String()
		switch {
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached; Phase 2 happy-path requires GUI session")
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; Phase 2 happy-path requires both granted upfront")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		}
		t.Fatalf("did not see active banner; stderr:\n%s", stderrSnap)
	}

	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)

	out := stdout.String()
	if !strings.Contains(out, "active. press Ctrl-C.") {
		t.Errorf("stdout missing active banner: %s", out)
	}
	if !strings.Contains(out, "cleaning up… done.") {
		t.Errorf("stdout missing cleanup banner: %s", out)
	}

	// stderr should contain release log for "windows" (real controller).
	if !strings.Contains(stderr.String(), `releaser=windows`) {
		t.Errorf("stderr missing 'released releaser=windows' (controller didn't run): %s", stderr.String())
	}
}

// TestAcceptance_Phase3_PreFlight_HappyPath exercises the full Phase 3
// PreFlight on a dev machine where AX + IM are already granted and at least
// one display is attached. The test:
//
//  1. starts dndmode,
//  2. waits up to 10s for the "active. press Ctrl-C." banner on stdout,
//  3. sends SIGINT, waits for clean exit,
//  4. asserts exit code 0,
//  5. asserts stderr contains `releaser=dndmode active` (proves the real
// powerassert.Assertion was released — Phase 3 contract),
//  6. asserts stderr does NOT contain `releaser=mock-assertion`
//     (regression guard for someone re-introducing the P2 mock).
//
// Skip strategy: any of the Phase 3 short-circuit paths visible in the
// stdout/stderr snapshots triggers a clean t.Skip — the happy-path test
// requires a pre-granted dev host.
func TestAcceptance_Phase3_PreFlight_HappyPath(t *testing.T) {
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		stderrSnap := stderr.String()
		stdoutSnap := stdout.String()
		switch {
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; Phase 3 happy-path requires both granted upfront")
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached; Phase 3 happy-path requires GUI session")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		case strings.Contains(stderrSnap, "another instance is holding"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		}
		t.Fatalf("did not see active banner; stdout:\n%s\nstderr:\n%s", stdoutSnap, stderrSnap)
	}

	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, `releaser=dndmode active`) {
		t.Errorf("stderr missing real assertion release (Phase 3 D-02 should produce 'releaser=dndmode active'): %s", stderrStr)
	}
	if strings.Contains(stderrStr, `releaser=mock-assertion`) {
		t.Errorf("stderr contains 'releaser=mock-assertion' (Phase 3 should have replaced the P2 mock with real powerassert.Assertion): %s", stderrStr)
	}
}

// TestAcceptance_Phase3_NoMockAssertion_Regression is a static (no
// subprocess) regression guard against someone re-adding the Phase 2
// placeholder `state.NewMockReleaser("mock-assertion")` to cmd/dndmode/main.go.
// ordering would still pass if you simply moved the mock up — but
// the Phase 3 contract requires the real powerassert.Assertion in that
// slot. This test reads main.go from disk and fails if the literal string
// `"mock-assertion"` (Go source quoted) appears anywhere in the file.
//
// The test runs on any host (no GUI, no TCC state needed) and is therefore
// the canonical Phase 3 regression check for CI without permission setup.
func TestAcceptance_Phase3_NoMockAssertion_Regression(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	mainGoPath := filepath.Join(repoRoot, "cmd", "dndmode", "main.go")
	mainGo, err := os.ReadFile(mainGoPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainGoPath, err)
	}
	const forbidden = `"mock-assertion"`
	if strings.Contains(string(mainGo), forbidden) {
		t.Errorf("%s still contains the literal string %s — Phase 3 should have replaced"+
			"state.NewMockReleaser(\"mock-assertion\") with a real powerassert.Assertion via Acquire(\"dndmode active\")",
			mainGoPath, forbidden)
	}
}

// --- helpers ---

func waitForStdout(buf *syncBuffer, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func signalAndWait(t *testing.T, cmd *exec.Cmd, sig os.Signal, timeout time.Duration) {
	t.Helper()
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitWithTimeout(t, cmd, timeout)
	if cmd.ProcessState.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", cmd.ProcessState.ExitCode())
	}
}

func waitWithTimeout(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("process did not exit within %v", timeout)
	}
}
