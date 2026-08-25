//go:build acceptance && darwin

package main_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/state/watch"
)

// Acceptance coverage for the background watch process's CONTROLS, and for
// the launcher's failure paths, on any arm64 host: none of these need an
// Accessibility grant, because none of them get as far as WaitForGrants. The
// full happy path — a --watch that registers its hotkey, detaches and later
// raises a shield — needs both TCC grants and is verified by hand.
//
// A `sleep` process stands in for a running watch process: the tests write
// the record a real one would, with the sleep's PID and its kernel start
// time, so --status and --kill see exactly what they would see in
// production.

func watchRecordPath(tmpHome string) string {
	return filepath.Join(tmpHome, ".config", "dndmode", "watch.json")
}

// startImpostor starts a long sleep and writes a watch record naming it.
// Returns the sleep's cmd (the caller kills it if the test does not).
func startImpostor(t *testing.T, tmpHome string) *exec.Cmd {
	t.Helper()
	sleep := exec.Command("/bin/sleep", "300")
	if err := sleep.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = sleep.Process.Kill()
		_ = sleep.Wait()
	})
	start, err := watch.NewKernProber().StartTime(sleep.Process.Pid)
	if err != nil {
		t.Fatalf("start time of sleep: %v", err)
	}
	mgr := watch.NewManager(watchRecordPath(tmpHome), nil)
	if err := mgr.Write(watch.Record{
		PID:            sleep.Process.Pid,
		StartedAt:      time.Now().UTC(),
		ProcStartedAt:  start,
		ActivateHotkey: "Ctrl+Option+Cmd+D",
		LogPath:        filepath.Join(tmpHome, ".config", "dndmode", "watch.log"),
	}); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return sleep
}

// runControl runs the raw binary (no --debug: control output is ungated by
// design) with the given flags and returns exit code, stdout, stderr.
func runControl(t *testing.T, tmpHome string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dndmodeBinary, args...)
	cmd.Env = append(os.Environ(), "HOME="+tmpHome)
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return code, stdout.String(), stderr.String()
}

func TestAcceptance_Status_NotWatching(t *testing.T) {
	tmpHome := t.TempDir()
	code, out, errs := runControl(t, tmpHome, "--status")
	if code != 9 {
		t.Errorf("exit = %d, want 9; stderr=%q", code, errs)
	}
	if !strings.Contains(out, "not watching") {
		t.Errorf("stdout = %q", out)
	}
	// A control command creates nothing.
	if _, err := os.Stat(filepath.Join(tmpHome, ".config")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--status created files under HOME: %v", err)
	}
}

func TestAcceptance_Kill_NotWatchingIsIdempotent(t *testing.T) {
	tmpHome := t.TempDir()
	code, out, errs := runControl(t, tmpHome, "--kill")
	if code != 0 {
		t.Errorf("exit = %d, want 0; stderr=%q", code, errs)
	}
	if !strings.Contains(out, "not watching") {
		t.Errorf("stdout = %q", out)
	}
}

func TestAcceptance_StatusThenKill_LiveRecord(t *testing.T) {
	tmpHome := t.TempDir()
	sleep := startImpostor(t, tmpHome)

	code, out, errs := runControl(t, tmpHome, "--status")
	if code != 0 {
		t.Fatalf("--status exit = %d, want 0; stdout=%q stderr=%q", code, out, errs)
	}
	if !strings.HasPrefix(out, "dndmode: watching\n") {
		t.Errorf("--status stdout does not open with the headline:\n%s", out)
	}
	for _, want := range []string{
		fmt.Sprintf("  pid     %d\n", sleep.Process.Pid),
		"  hotkey  Ctrl+Option+Cmd+D",
		"  shield  down\n",
		"  log     ~/.config/dndmode/watch.log\n",
		"  stop    dndmode --kill\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--status stdout missing %q:\n%s", want, out)
		}
	}

	// Reap concurrently: a real watch process is reparented to launchd, which
	// reaps it the instant it exits. Here the impostor is OUR child, and a
	// zombie still answers kill(pid, 0), so --kill would otherwise wait out
	// its whole timeout for a process that is in fact gone.
	waitCh := make(chan error, 1)
	go func() { waitCh <- sleep.Wait() }()

	code, out, errs = runControl(t, tmpHome, "--kill")
	if code != 0 {
		t.Fatalf("--kill exit = %d, want 0; stdout=%q stderr=%q", code, out, errs)
	}
	if !strings.Contains(out, "stopped watching") {
		t.Errorf("--kill stdout = %q", out)
	}
	// The impostor really got SIGTERM.
	waitErr := <-waitCh
	exitErr, ok := errors.AsType[*exec.ExitError](waitErr)
	if !ok {
		t.Fatalf("sleep exit: %v, want a signal death", waitErr)
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("sleep was not terminated by SIGTERM: %v", exitErr)
	}
	if _, err := os.Stat(watchRecordPath(tmpHome)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record still on disk after --kill: %v", err)
	}
	if code, _, _ := runControl(t, tmpHome, "--status"); code != 9 {
		t.Errorf("--status after --kill exit = %d, want 9", code)
	}
}

func TestAcceptance_StaleRecord(t *testing.T) {
	tmpHome := t.TempDir()
	sleep := startImpostor(t, tmpHome)
	// Kill the impostor behind the record's back — a SIGKILLed watch process.
	_ = sleep.Process.Kill()
	_ = sleep.Wait()

	code, out, _ := runControl(t, tmpHome, "--status")
	if code != 9 || !strings.HasPrefix(out, "dndmode: not watching\n") || !strings.Contains(out, "  stale   record for PID") {
		t.Errorf("--status on a stale record: exit=%d stdout=%q", code, out)
	}
	if _, err := os.Stat(watchRecordPath(tmpHome)); err != nil {
		t.Errorf("--status removed the record: %v", err)
	}

	code, out, _ = runControl(t, tmpHome, "--kill")
	if code != 0 || !strings.Contains(out, "cleared a stale record for PID") {
		t.Errorf("--kill on a stale record: exit=%d stdout=%q", code, out)
	}
	if _, err := os.Stat(watchRecordPath(tmpHome)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale record survived --kill: %v", err)
	}
}

func TestAcceptance_ControlFlagConflicts(t *testing.T) {
	tmpHome := t.TempDir()
	for _, args := range [][]string{
		{"--kill", "--status"},
		{"--status", "--watch"},
		{"--kill", "--timer", "30m"},
		{"--set-password", "--kill"},
		{"--set-password", "--status"},
	} {
		code, _, errs := runControl(t, tmpHome, append(args, "--debug")...)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1; stderr=%q", args, code, errs)
		}
		if !strings.Contains(errs, "cannot be combined") {
			t.Errorf("%v: stderr = %q", args, errs)
		}
	}
}

// TestAcceptance_Watch_AlreadyWatching drives the REAL launcher: `--watch`
// re-executes the binary detached, the background half finds the (impostor's)
// record at Step 5d and exits 5 before any permission prompt, and the launcher
// mirrors that code and lets the child's diagnostic through on the inherited
// stderr.
func TestAcceptance_Watch_AlreadyWatching(t *testing.T) {
	tmpHome := t.TempDir()
	sleep := startImpostor(t, tmpHome)

	code, out, errs := runControl(t, tmpHome, "--watch", "--debug")
	if code != 5 {
		t.Fatalf("exit = %d, want 5; stdout=%q stderr=%q", code, out, errs)
	}
	if !strings.Contains(errs, "already watching (PID") {
		t.Errorf("stderr = %q", errs)
	}
	// The impostor's record is untouched, and it is still alive.
	if err := syscall.Kill(sleep.Process.Pid, 0); err != nil {
		t.Errorf("a refused --watch harmed the running process: %v", err)
	}
	rec, err := watch.NewManager(watchRecordPath(tmpHome), nil).Read()
	if err != nil || rec.PID != sleep.Process.Pid {
		t.Errorf("record after refusal = %+v, %v", rec, err)
	}
}

// TestAcceptance_Watch_StyleNoneRefused: the same launcher path, failing at
// Step 5b.3 with the config-error code the background half chose.
func TestAcceptance_Watch_StyleNoneRefused(t *testing.T) {
	tmpHome := t.TempDir()
	code, _, errs := runControl(t, tmpHome, "--watch", "--style", "none", "--debug")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errs)
	}
	if !strings.Contains(errs, "--watch cannot be combined with --style none") {
		t.Errorf("stderr = %q", errs)
	}
	// Nothing was left behind by the failed background half.
	if _, err := os.Stat(watchRecordPath(tmpHome)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused --watch left a record: %v", err)
	}
}

// TestAcceptance_Watch_SilentFailureStillMirrorsCode: without --debug the
// background half's diagnostic is gated, but the exit code still comes back
// through the launcher.
func TestAcceptance_Watch_SilentFailureStillMirrorsCode(t *testing.T) {
	tmpHome := t.TempDir()
	code, out, errs := runControl(t, tmpHome, "--watch", "--style", "none")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out != "" || errs != "" {
		t.Errorf("silent mode printed something: stdout=%q stderr=%q", out, errs)
	}
}
