//go:build acceptance && darwin

// Acceptance tests for Phase 1. Run via `make acceptance` (NOT `make test`).
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
