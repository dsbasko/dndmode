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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
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

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		stdoutSnap := stdout.String()
		stderrSnap := stderr.String()
		// Phase 3/5 PreFlight short-circuits prevent reaching the active
		// banner. This test's intent is the default-config-creation flow
		// — orthogonal to Phase 3/5 gates. Skip cleanly so the host can
		// run the test once Shortcuts / permissions are configured.
		switch {
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; default-config test requires both granted upfront")
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached; default-config test requires GUI session")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing (Phase 5 PreFlight gate); create them and re-run")
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		}
		t.Fatalf("did not see active banner in stdout within 10s; got:\n%s\nstderr:\n%s", stdoutSnap, stderrSnap)
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

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		stdoutSnap := stdout.String()
		stderrSnap := stderr.String()
		switch {
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; SIGINT-cleanup test requires both granted upfront")
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached; SIGINT-cleanup test requires GUI session")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing (Phase 5 PreFlight gate); create them and re-run")
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		}
		t.Fatalf("did not see active banner: stdout=%s stderr=%s", stdoutSnap, stderrSnap)
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

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmd.Process.Kill()
		stdoutSnap := stdout.String()
		stderrSnap := stderr.String()
		switch {
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; double-SIGINT test requires both granted upfront")
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached; double-SIGINT test requires GUI session")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing (Phase 5 PreFlight gate); create them and re-run")
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		}
		t.Fatalf("did not see active banner: stdout=%s stderr=%s", stdoutSnap, stderrSnap)
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

// TestAcceptance_InvalidStyleFlag_ExitOne verifies the --style flag is
// value-validated through the same ValidateOverlayStyle gate as overlay_style
// (Step 5b.1): a junk value exits 1 BEFORE any PreFlight permission check, and
// stderr names the FLAG (not the config file) as the source so the operator
// knows where to fix it. Mirrors TestAcceptance_ModifierOnlyHotkey_ExitOne.
func TestAcceptance_InvalidStyleFlag_ExitOne(t *testing.T) {
	tmpHome := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, _, stderr := dndmodeCmd(t, ctx, tmpHome)
	cmd.Args = append(cmd.Args, "--style=neon")
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit for invalid --style, got nil")
	}

	if exitCode := cmd.ProcessState.ExitCode(); exitCode != 1 {
		t.Errorf("exit code = %d, want 1 (invalid --style value)", exitCode)
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "invalid --style") {
		t.Errorf("stderr missing 'invalid --style' marker: %s", stderrStr)
	}
}

// TestAcceptance_StyleFlag_OverridesConfig verifies the --style flag WINS over
// overlay_style in config.yml (Step 5b.1 precedence): config asks for black,
// the flag asks for glass, and the startup banner — emitted at Step 6, BEFORE
// the Step 8+ platform/permission/display gates — reports
// `overlay_style=glass (flag)`. Because the banner precedes WaitForGrants, no
// PreFlight skip branches are needed: the assertion is observable regardless of
// host TCC / display state.
func TestAcceptance_StyleFlag_OverridesConfig(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".config", "dndmode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("hotkey: Ctrl+Option+Cmd+X\noverlay_style: black\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	cmd.Args = append(cmd.Args, "--style=glass")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if !waitForStdout(stdout, "overlay_style=glass (flag)", 10*time.Second) {
		t.Fatalf("banner missing 'overlay_style=glass (flag)' (flag must override config overlay_style=black):\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
}

// TestAcceptance_StyleNone_CaffeinateOnly is the overlay_style=none happy path:
// dndmode degrades to a thin caffeinate(8) wrapper. Crucially it reaches the
// active banner WITHOUT Accessibility / Input Monitoring grants, WITHOUT the
// dndmode-on/off Shortcuts, and WITHOUT any display — none mode skips every one
// of those PreFlight gates (main.go Step 8a branches before them). So unlike the
// other acceptance tests this one needs NO skip branches: on any arm64 macOS 14+
// host it must reach active. It also asserts a real caffeinate child is spawned
// and that SIGINT tears it down (released releaser=caffeinate) with a clean
// exit 0 + cleanup banner.
func TestAcceptance_StyleNone_CaffeinateOnly(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".config", "dndmode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("hotkey: Ctrl+Option+Cmd+X\noverlay_style: none\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForStdout(stdout, "active (caffeinate-only", 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("did not see caffeinate-only active banner (none mode must skip all PreFlight gates):\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}

	// A real caffeinate child of the dndmode process must be holding the
	// awake-lock while none mode is active — and (default config) with -d so the
	// display stays awake too.
	if args := caffeinateChildArgs(t, cmd.Process.Pid); args == "" {
		t.Errorf("no caffeinate child found for dndmode pid %d (none mode must spawn caffeinate)", cmd.Process.Pid)
	} else if !strings.Contains(args, " -d") {
		t.Errorf("caffeinate child argv %q missing -d (default must keep display awake)", args)
	}

	// SIGINT → clean exit 0 (asserted inside signalAndWait).
	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)

	if out := stdout.String(); !strings.Contains(out, "cleaning up… done.") {
		t.Errorf("stdout missing cleanup banner: %s", out)
	}
	if errOut := stderr.String(); !strings.Contains(errOut, "released releaser=caffeinate") {
		t.Errorf("stderr missing 'released releaser=caffeinate' (caffeinate teardown): %s", errOut)
	}
}

// TestAcceptance_StyleNone_AllowDisplaySleep_DropsDFlag verifies the
// allow_display_sleep config toggle actually threads through
// runCaffeinateOnly → caffeinate.Start into the live child's argv: with
// allow_display_sleep:true the -d flag must be dropped (display may idle off)
// while -i/-s remain (system stays awake). This catches a cfg→Start propagation
// regression that the pure buildArgs unit test cannot.
func TestAcceptance_StyleNone_AllowDisplaySleep_DropsDFlag(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".config", "dndmode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("hotkey: Ctrl+Option+Cmd+X\noverlay_style: none\nallow_display_sleep: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fail := func(format string, a ...any) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf(format, a...)
	}

	if !waitForStdout(stdout, "active (caffeinate-only", 10*time.Second) {
		fail("did not see caffeinate-only banner:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	args := caffeinateChildArgs(t, cmd.Process.Pid)
	if args == "" {
		fail("no caffeinate child found for dndmode pid %d", cmd.Process.Pid)
	}
	if strings.Contains(args, " -d") {
		t.Errorf("caffeinate child argv %q contains -d, but allow_display_sleep:true must drop it", args)
	}
	if !strings.Contains(args, " -i") || !strings.Contains(args, " -s") {
		t.Errorf("caffeinate child argv %q missing -i/-s (system must stay awake even when display sleep is allowed)", args)
	}

	// Clean shutdown (also confirms none+allow_display_sleep exits cleanly).
	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)
}

// TestAcceptance_StyleNone_ChildDeath_ExitsNonZero verifies the unexpected-death
// contract: if the caffeinate child is killed out from under dndmode (external
// kill / -w watch firing) while ctx is still live, dndmode must NOT masquerade
// as healthy — it logs "caffeinate exited unexpectedly" and exits non-zero
// (exitPlatformErr=2), mirroring the full path's watchdog-trip discipline.
func TestAcceptance_StyleNone_ChildDeath_ExitsNonZero(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".config", "dndmode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("hotkey: Ctrl+Option+Cmd+X\noverlay_style: none\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fail := func(format string, a ...any) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf(format, a...)
	}

	if !waitForStdout(stdout, "active (caffeinate-only", 10*time.Second) {
		fail("did not see caffeinate-only banner:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	childPID := caffeinateChildPID(cmd.Process.Pid, 3*time.Second)
	if childPID == 0 {
		fail("no caffeinate child found for dndmode pid %d", cmd.Process.Pid)
	}
	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		fail("kill caffeinate child %d: %v", childPID, err)
	}

	// dndmode should now observe proc.Done() with a live ctx and exit non-zero.
	waitWithTimeout(t, cmd, 10*time.Second)

	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Errorf("exit code = %d, want 2 (exitPlatformErr on unexpected caffeinate death)", code)
	}
	if errOut := stderr.String(); !strings.Contains(errOut, "caffeinate exited unexpectedly") {
		t.Errorf("stderr missing 'caffeinate exited unexpectedly': %s", errOut)
	}
	if out := stdout.String(); !strings.Contains(out, "cleaning up… done.") {
		t.Errorf("stdout missing cleanup banner: %s", out)
	}
}

// caffeinateChildPID polls pgrep for a caffeinate process parented by ppid and
// returns its pid (0 if none appears within timeout).
func caffeinateChildPID(ppid int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("pgrep", "-P", fmt.Sprint(ppid), "caffeinate").Output()
		if s := strings.TrimSpace(string(out)); s != "" {
			if pid, err := strconv.Atoi(strings.SplitN(s, "\n", 2)[0]); err == nil {
				return pid
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0
}

// caffeinateChildArgs returns the full argv of the caffeinate child of ppid
// (empty string if none appears / on error). Used to assert flag wiring end to
// end (e.g. that allow_display_sleep drops -d).
func caffeinateChildArgs(t *testing.T, ppid int) string {
	t.Helper()
	pid := caffeinateChildPID(ppid, 3*time.Second)
	if pid == 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestAcceptance_LIFE06_PushOrder verifies (P2 + Phase 3 + Phase 4
//) that the LIFO Cleanup chain releases resources in the exact order
// the design notes mandates: tap (first) → windows → assertion →
// focus → runtime-file (last).
//
// Phase 1 verifier missed this because it only checked "did anything fail"
// — not the ordering. This test parses stderr looking for the per-Releaser
// success-log emitted by RestoreState.Cleanup (`released releaser=<name>`)
// and asserts strings.Index ordering.
//
// Phase 3 update: the third releaser is now a real powerassert.Assertion
// with Name() == "dndmode active" (per). Phase 5 adds
// focus.Releaser between assertion and runtime-file.
//
// Phase 4 update (this revision): the first releaser is now a
// real eventtap composite Releaser with Name() == "eventtap" (per plan
// InstallAll wire-up). The substring match here updated to
// `releaser=eventtap` accordingly. Phase 3 had this first slot occupied
// by a placeholder mock Releaser — Phase 4 replaces that
// placeholder with eventtap.InstallAll producing a composite
// Releaser that internally tears down (per LIFO):
// CGEventTapEnable(tap, 0) + eventtap_set_observed_tap(NULL) →
// CFRunLoopRemoveSource → CFRelease(source+tap) → dispatch_source_cancel
// (watchdog stop) → wake_observer_remove.
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
	// Phase 5 paths: missing dndmode-on/dndmode-off Shortcuts → exit 6 (the
	// new PreFlight gate at Step 9.5); concurrent live dndmode → exit 5.
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
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing; create them in Shortcuts.app and re-run")
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		}
		t.Fatalf("did not see active banner; stdout:\n%s\nstderr:\n%s", stdoutSnap, stderrSnap)
	}

	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)

	s := stderr.String()
	// cleanup execution order (LIFO unwind of push order in main.go,
	// finalised by Phase 4 — first slot now a REAL eventtap composite
	// Releaser). Actual main.go push order (verifiable via `grep -n
	// rs.Push cmd/dndmode/main.go`) is:
	//   Step 12  → runtimeMgr     (runtime-file)
	//   Step 13  → assertion      ("dndmode active")
	//   Step 13.7→ focus.Releaser (focus)
	//   Step 15  → controller     (windows)
	//   Step 17  → tapRel         (eventtap)
	// LIFO unwind (last pushed = first released):
	// 1. eventtap (REAL CGEventTap composite — Phase 4
	//                         replaces the P3 placeholder Releaser; internal
	// Release order per: tap-disable
	//                         g_observed_tap=NULL → CFRunLoopRemoveSource →
	//                         CFRelease(source+tap) → watchdog_stop →
	//                         wake_remove)
	//   2. windows           (controller — close all NSWindow)
	// 3. focus (REAL focus.Releaser — Phase 5)
	//   4. "dndmode active"  (REAL IOPMAssertion — Phase 3 replaces P2 mock)
	// 5. runtime-file (REAL runtime.Manager — Phase 5
	//                         replaces P3 mock-runtime-file)
	//
	// NB: Phase 5 wire-up placed `rs.Push(focus.NewReleaser)`
	// AFTER `rs.Push(assertion)`, which inverts the previous Phase 3
	// docstring ordering. This test used to expect tap → windows → assertion
	// → focus → runtime; Phase 4 corrects it to the actual LIFO
	// observed at runtime.
	//
	// slog TextHandler quotes string values containing spaces, so the
	// real Assertion releaser logs as `releaser="dndmode active"` (with
	// quotes). Single-word names (eventtap, windows, focus, runtime-file)
	// are unquoted.
	posTap := strings.Index(s, `releaser=eventtap`)
	posWin := strings.Index(s, `releaser=windows`)
	posFocus := strings.Index(s, `releaser=focus`)
	posAssert := strings.Index(s, `releaser="dndmode active"`)
	posRuntime := strings.Index(s, `releaser=runtime-file`)

	if posTap < 0 || posWin < 0 || posFocus < 0 || posAssert < 0 || posRuntime < 0 {
		t.Fatalf("missing release log entries (need all 5 'released' info logs):\n"+
			"  posTap(eventtap)=%d posWin=%d posFocus=%d posAssert(dndmode active)=%d posRuntime=%d\n"+
			"stderr:\n%s",
			posTap, posWin, posFocus, posAssert, posRuntime, s)
	}

	if !(posTap < posWin && posWin < posFocus && posFocus < posAssert && posAssert < posRuntime) {
		t.Errorf("cleanup order violated:\n"+
			"  eventtap@%d  windows@%d  focus@%d  dndmode-active@%d  runtime-file@%d\n"+
			"  expected: eventtap < windows < focus < dndmode-active < runtime-file (LIFO unwind, Phase 4)\n"+
			"stderr:\n%s",
			posTap, posWin, posFocus, posAssert, posRuntime, s)
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
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing (Phase 5 PreFlight gate); create them and re-run")
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
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
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing (Phase 5 PreFlight gate); create them and re-run")
		}
		t.Fatalf("did not see active banner; stdout:\n%s\nstderr:\n%s", stdoutSnap, stderrSnap)
	}

	signalAndWait(t, cmd, syscall.SIGINT, 10*time.Second)

	stderrStr := stderr.String()
	// slog TextHandler quotes string values containing spaces, so the real
	// Assertion releaser logs as `releaser="dndmode active"` (with quotes).
	if !strings.Contains(stderrStr, `releaser="dndmode active"`) {
		t.Errorf("stderr missing real assertion release (Phase 3 should produce 'releaser=\"dndmode active\"'): %s", stderrStr)
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

// TestAcceptance_Phase5_NoMockRuntimeFile_Regression is a static (no
// subprocess) regression guard against someone re-adding the Phase 3
// placeholder `state.NewMockReleaser("mock-runtime-file")` to
// cmd/dndmode/main.go. ordering would still pass if you simply
// moved the mock to the bottom of the stack — but the Phase 5
// contract requires the real runtime.Manager in that slot.
//
// The test reads main.go from disk and fails if the literal string
// `"mock-runtime-file"` (Go source quoted) appears anywhere in the
// file. Comments referring to the old name unquoted are allowed —
// they document history, not Go literals.
//
// Runs on any host (no GUI, no TCC state needed) and is therefore the
// canonical Phase 5 regression check for CI without permission setup.
func TestAcceptance_Phase5_NoMockRuntimeFile_Regression(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	mainGoPath := filepath.Join(repoRoot, "cmd", "dndmode", "main.go")
	mainGo, err := os.ReadFile(mainGoPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainGoPath, err)
	}
	const forbidden = `"mock-runtime-file"`
	if strings.Contains(string(mainGo), forbidden) {
		t.Errorf("%s still contains the literal string %s — Phase 5 should have replaced"+
			"state.NewMockReleaser(\"mock-runtime-file\") with a real runtime.Manager via runtimepkg.NewManager(...)",
			mainGoPath, forbidden)
	}
}

// TestAcceptance_CrashScenario verifies Phase 5 end-to-end
// (validation map ID 5-06-02). Manual prereq: pre-granted AX/IM
// permissions on the test binary cdhash; `dndmode-on` / `dndmode-off`
// Shortcuts exist; at least one display attached. Skips cleanly if any
// prereq is unmet, mirroring TestAcceptance_LIFE06_PushOrder.
//
// Steps:
//
//  1. Fork subprocess A; wait for "active. press Ctrl-C." → proves
//     Step 13.3 (Manager.Write) and Step 13.7 (focus.Activate) ran.
//  2. Verify runtime.json exists under tmpHome/.config/dndmode/.
//  3. SIGKILL subprocess A (bypasses defer Cleanup → runtime.json +
//     IOPM orphan + Focus On remain).
//  4. Verify runtime.json still on disk post-Wait.
//  5. Fork subprocess B (same tmpHome → reads the SAME runtime.json).
//  6. Expect stderr B contains "recovery: released orphan assertion".
//  7. Expect "active. press Ctrl-C." → recovery succeeded.
//  8. Re-read runtime.json; assert snapshot.PID == cmdB.Process.Pid
//     (MANDATORY t.Fatalf on mismatch — NOT a skip).
//  9. SIGINT B; expect exit 0; runtime.json deleted by Manager.Release.
func TestAcceptance_CrashScenario(t *testing.T) {
	tmpHome := t.TempDir()
	runtimeJSON := filepath.Join(tmpHome, ".config", "dndmode", "runtime.json")

	// --- Subprocess A ---
	ctxA, cancelA := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelA()
	cmdA, stdoutA, stderrA := dndmodeCmd(t, ctxA, tmpHome)
	if err := cmdA.Start(); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	if !waitForStdout(stdoutA, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmdA.Process.Kill()
		stderrSnap := stderrA.String()
		stdoutSnap := stdoutA.String()
		switch {
		case strings.Contains(stdoutSnap, "dndmode: waiting"):
			t.Skip("AX or IM not granted on this host; CrashScenario requires both granted upfront")
		case strings.Contains(stderrSnap, "no displays detected"):
			t.Skip("no displays attached; CrashScenario requires GUI session")
		case strings.Contains(stderrSnap, "Secure Event Input"):
			t.Skip("SecureEventInput active on host; close it and re-run")
		case strings.Contains(stderrSnap, "required Shortcuts not found"):
			t.Skip("dndmode-on / dndmode-off Shortcuts missing; create them in Shortcuts.app and re-run")
		case strings.Contains(stderrSnap, "another instance is holding") || strings.Contains(stderrSnap, "another instance is already active"):
			t.Skip("another dndmode instance is holding the awake-lock; SIGTERM it and re-run")
		}
		t.Fatalf("A did not activate: stdout=%s stderr=%s", stdoutSnap, stderrSnap)
	}

	// Pre-SIGKILL: runtime.json must exist (Step 13.3 fired).
	if _, err := os.Stat(runtimeJSON); err != nil {
		_ = cmdA.Process.Kill()
		t.Fatalf("runtime.json missing before SIGKILL: %v (Step 13.3 did not run?)", err)
	}

	// SIGKILL — bypasses defer Cleanup; runtime.json + orphan IOPM + Focus remain.
	if err := cmdA.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL A: %v", err)
	}
	_, _ = cmdA.Process.Wait()

	// Post-Wait: runtime.json still on disk.
	if _, err := os.Stat(runtimeJSON); err != nil {
		t.Fatalf("runtime.json gone after SIGKILL (should remain — A's Cleanup was bypassed): %v", err)
	}

	// --- Subprocess B — recovery path ---
	ctxB, cancelB := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelB()
	cmdB, stdoutB, stderrB := dndmodeCmd(t, ctxB, tmpHome)
	if err := cmdB.Start(); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	if !waitForStdout(stdoutB, "active. press Ctrl-C.", 10*time.Second) {
		_ = cmdB.Process.Kill()
		t.Fatalf("B did not activate after recovery: stdout=%s stderr=%s",
			stdoutB.String(), stderrB.String())
	}
	if !strings.Contains(stderrB.String(), "recovery: released orphan assertion") {
		t.Errorf("stderr B missing recovery log line (RecoverFromCrash dead-PID branch did not fire): %s",
			stderrB.String())
	}

	// MANDATORY PID-match: re-read runtime.json and assert
	// snapshot.PID == cmdB.Process.Pid. A mismatch is a BUG (not a skip):
	// either recovery didn't delete the prior file before B's Write,
	// B's Write didn't fire, or temp+rename produced a stale read.
	rawSnap, err := os.ReadFile(runtimeJSON)
	if err != nil {
		t.Fatalf("re-read runtime.json after B active: %v (Step 13.3 in B did not run?)", err)
	}
	var snap runtimepkg.Snapshot
	if err := json.Unmarshal(rawSnap, &snap); err != nil {
		t.Fatalf("unmarshal runtime.json from B: %v (corrupt write?)", err)
	}
	if snap.PID != cmdB.Process.Pid {
		t.Fatalf("runtime.json PID mismatch after recovery: got %d, want %d (cmdB) — Step 13.3 in B may not have overwritten the prior snapshot",
			snap.PID, cmdB.Process.Pid)
	}

	// Clean shutdown — exit code 0 + runtime.json deleted via Manager.Release.
	signalAndWait(t, cmdB, syscall.SIGINT, 10*time.Second)
	if cmdB.ProcessState.ExitCode() != 0 {
		t.Errorf("B exit code = %d, want 0", cmdB.ProcessState.ExitCode())
	}
	if _, err := os.Stat(runtimeJSON); !os.IsNotExist(err) {
		t.Errorf("runtime.json still exists after B clean exit (Manager.Release did not delete): %v", err)
	}
}

// TestAcceptance_LIFE12_Stderr is a static-grep regression test that asserts
// the stderr template in cmd/dndmode/main.go Step 5c stays in sync
// with the LOCKED the design notes wording. If a future commit alters the
// wording (e.g., translates to Russian, drops the PID interpolation, drops
// the actionable next-step), this test fails — protects user-facing
// stability across rebuilds.
//
// Mirror of TestAcceptance_Phase5_NoMockRuntimeFile_Regression and
// TestAcceptance_Phase3_NoMockAssertion_Regression patterns — both use
// static file-grep against main.go to lock down architectural invariants.
func TestAcceptance_LIFE12_Stderr(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	// Invariant 1: Step 5c block must exist and be commented as such.
	if !strings.Contains(body, "Step 5c (Phase 6)") {
		t.Errorf("main.go missing 'Step 5c (Phase 6)' comment marker; wire-up may have regressed")
	}

	// Invariant 2: IsLiveInstance must be invoked.
	if !strings.Contains(body, "runtimepkg.IsLiveInstance(") {
		t.Errorf("main.go missing runtimepkg.IsLiveInstance call; helper not wired")
	}

	// Invariant 3: stderr template must match the design notes LOCKED wording.
	const wantTemplate = "dndmode: another instance is already active (PID=%d). Send SIGTERM or wait for its exit, then re-run."
	if !strings.Contains(body, wantTemplate) {
		t.Errorf("main.go missing stderr template:\n want: %q\n (found in main.go: %v)", wantTemplate, strings.Contains(body, "another instance is already active"))
	}

	// Invariant 4: alive=true path must return exitConcurrentInstance (= 5),
	// reusing the existing constant — no new exit code.
	idx := strings.Index(body, "another instance is already active (PID=%d)")
	if idx < 0 {
		t.Fatalf("template not found (covered by Invariant 3)")
	}
	tail := body[idx:]
	if len(tail) > 400 {
		tail = tail[:400]
	}
	if !strings.Contains(tail, "return exitConcurrentInstance") {
		t.Errorf("alive-path does not return exitConcurrentInstance (must reuse exit code 5, not introduce new code)")
	}

	// Invariant 5: read-failure path must log warn + continue (NOT exit).
	if !strings.Contains(body, `log.Warn(" pre-check inconclusive"`) {
		t.Errorf("read-failure path does not log warn ' pre-check inconclusive' (must be warn-not-fatal per the design notes)")
	}

	// Invariant 6 (Phase 4 closes the placeholder boundary):
	// the real eventtap composite Releaser MUST be wired into the LIFO push
	// stack via eventtap.InstallAll. Phase 3 had a placeholder mock Releaser
	// in slot 1; replaced it. This positive-presence check is
	// the canonical Phase 4 regression guard.
	if !strings.Contains(body, `eventtap.InstallAll(`) {
		t.Errorf("Phase 4 boundary regressed — eventtap.InstallAll not invoked; first slot missing real composite Releaser")
	}
	// Regression guard for the Phase 3 placeholder name. Build the literal
	// dynamically so the source file does NOT contain the bare placeholder
	// string (Phase 4 acceptance criterion: zero refs to that
	// literal in this file — protects the v1.1 release boundary).
	mockReleaserCall := `state.NewMockReleaser("` + "mock" + `-tap")`
	if strings.Contains(body, mockReleaserCall) {
		t.Errorf("Phase 4 boundary regressed — placeholder %s still present; "+
			"must have replaced it with eventtap.InstallAll", mockReleaserCall)
	}

	// Invariant 7 (fix regression guard): the bare `eventtap.Install(`
	// helper MUST NOT be invoked from production code. The historical bare
	// Install returned a *Releaser with nil watchdogStop + nil wakeStop and
	// silently bypassed both the silent-disable watchdog and the wake-after-sleep
	// re-arm observer. unexported it (renamed to `installTapOnly`);
	// this substring check is a belt-and-suspenders guard against a future
	// maintainer re-exporting it OR a fresh public API that re-introduces
	// the same shape.
	//
	// We rebuild the `eventtap.Install(` literal dynamically so this source
	// file itself does not contain the bare token (otherwise the literal
	// inside this comment / error message would self-trigger if main.go
	// were ever inlined into this test for any reason). The suffix `(`
	// disambiguates the bare form from `eventtap.InstallAll(` — the byte
	// right after `eventtap.Install` in `eventtap.InstallAll(` is `A`, not
	// `(`, so strings.Index for `eventtap.Install(` matches ONLY the bare
	// form.
	forbidden := "eventtap.Install" + "("
	cursor := 0
	for {
		off := strings.Index(body[cursor:], forbidden)
		if off < 0 {
			break
		}
		absolute := cursor + off
		t.Errorf("fix regressed: bare Install call found in main.go at byte offset %d."+
			"Use eventtap.InstallAll for production wire-up; the bare helper is unexported "+
			"(installTapOnly) and reserved for the manual smoke test.",
			absolute)
		// Continue scanning so the test reports every offending site, not
		// just the first.
		cursor = absolute + len(forbidden)
	}
}

// TestAcceptance_LIFE10_PanicRecover validates Phase 4 mitigation
// top-level recover() in run() ensures restoreState.Cleanup() completes
// before os.Exit even on a panic between the final rs.Push (eventtap) and
// cocoa.RunApp (i.e. on any Cocoa-adjacent crash post-setup).
//
// Subprocess test: parent forks the prebuilt dndmode binary with
// DNDMODE_TEST_PANIC=1 env-var injected. main.go Step 18.0 reads the
// env-var (only present in test environments per Phase 4) and
// triggers panic("test panic (DNDMODE_TEST_PANIC=1)") AFTER all rs.Push
// and AFTER supervisor.Start, BEFORE the active banner / cocoa.RunApp.
//
// Asserts: (1) exit code 8 (exitInternalErr — per the design notes),
// (2) stderr contains "dndmode: PANIC:" + the panic message,
// (3) ~/.config/dndmode/runtime.json was deleted (Cleanup ran via defer).
//
// Skip cases mirror other acceptance tests: missing AX/IM, no displays,
// SecureEventInput active, missing Shortcuts, concurrent instance.
// Skip is "host condition" — the panic injection point sits AFTER all
// PreFlight gates, so any of these surface before Step 18.0 fires and
// must short-circuit before the assertions run.
func TestAcceptance_LIFE10_PanicRecover(t *testing.T) {
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, stdout, stderr := dndmodeCmd(t, ctx, tmpHome)
	cmd.Env = append(cmd.Env, "DNDMODE_TEST_PANIC=1")

	runErr := cmd.Run()

	stdoutStr := stdout.String()
	stderrStr := stderr.String()

	// Host-condition skip cases — must come BEFORE assertions because any
	// PreFlight gate short-circuits before Step 18.0 panic injection fires.
	switch {
	case strings.Contains(stderrStr, "no displays detected"):
		t.Skip("no displays attached (HEADLESS); test requires GUI session")
	case strings.Contains(stdoutStr, "dndmode: waiting"):
		t.Skip("AX or IM not granted on this host; test requires both granted upfront")
	case strings.Contains(stderrStr, "Secure Event Input"):
		t.Skip("SecureEventInput active on host; close it and re-run")
	case strings.Contains(stderrStr, "required Shortcuts not found"):
		t.Skip("dndmode-on / dndmode-off Shortcuts missing; create them and re-run")
	case strings.Contains(stderrStr, "another instance is holding") ||
		strings.Contains(stderrStr, "another instance is already active"):
		t.Skip("another dndmode instance is active; SIGTERM it and re-run")
	}

	// Assertion 1: non-nil ExitError (panic recovered → os.Exit(8)).
	if runErr == nil {
		t.Fatalf("expected non-zero exit (panic recovered → os.Exit(8)); got nil error\n"+
			"stdout:\n%s\nstderr:\n%s", stdoutStr, stderrStr)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("runErr is not *exec.ExitError: %T %v\nstdout:\n%s\nstderr:\n%s",
			runErr, runErr, stdoutStr, stderrStr)
	}

	// Assertion 2: exit code 8 (exitInternalErr per Phase 4 the design notes).
	if got := exitErr.ExitCode(); got != 8 {
		t.Errorf("exit code = %d; want 8 (exitInternalErr); stdout:\n%s\nstderr:\n%s",
			got, stdoutStr, stderrStr)
	}

	// Assertion 3: stderr contains the dndmode: PANIC: prefix from the recover
	// defer in run() — proves the recover handler fired (not a raw goroutine
	// panic that bypassed it).
	if !strings.Contains(stderrStr, "dndmode: PANIC:") {
		t.Errorf("stderr missing 'dndmode: PANIC:' prefix (recover defer did not fire); stderr:\n%s",
			stderrStr)
	}

	// Assertion 4: stderr contains the deterministic panic message from Step
	// 18.0 — proves the env-var-gated injection reached the panic
	// statement, not a different runtime panic.
	if !strings.Contains(stderrStr, "test panic (DNDMODE_TEST_PANIC=1)") {
		t.Errorf("stderr missing 'test panic (DNDMODE_TEST_PANIC=1)' message; stderr:\n%s",
			stderrStr)
	}

	// Assertion 5: runtime.json must have been deleted by rs.Cleanup (the
	// runtime-file Releaser at the bottom of the LIFO stack). If the file
	// survives, either Cleanup did not run or the Manager.Release path
	// regressed — mitigation is broken because the recover defer
	// must run AFTER Cleanup (LIFO unwind: recover defer registered FIRST,
	// runs LAST per Go semantics).
	runtimeJSONPath := filepath.Join(tmpHome, ".config", "dndmode", "runtime.json")
	if _, err := os.Stat(runtimeJSONPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("runtime.json was NOT deleted by Cleanup chain after panic "+
			"(mitigation broken — Cleanup defer did not unwind before recover defer):"+
			"stat err=%v\nstderr:\n%s", err, stderrStr)
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
