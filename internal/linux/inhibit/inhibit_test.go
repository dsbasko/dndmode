//go:build linux

package inhibit

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

// fakeLookPath resolves only the names in have.
func fakeLookPath(have ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		if slices.Contains(have, name) {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestBuildArgv(t *testing.T) {
	t.Setenv("XDG_CURRENT_DESKTOP", "ubuntu:GNOME")
	defer func(old func(string) (string, error)) { lookPath = old }(lookPath)

	lookPath = fakeLookPath("tail", "sh", "systemd-inhibit", "gnome-session-inhibit")
	argv, err := buildArgv(42, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--what=idle:sleep", "idle:suspend", "42", "--inhibit-only", "/usr/bin/sh"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv %v lacks %q", argv, want)
		}
	}
	if argv[0] != "/usr/bin/systemd-inhibit" {
		t.Errorf("chain head = %s, want systemd-inhibit", argv[0])
	}

	argv, _ = buildArgv(42, true)
	if !slices.Contains(argv, "--what=sleep") || !slices.Contains(argv, "suspend") {
		t.Errorf("allowDisplaySleep argv %v must not inhibit idle", argv)
	}

	t.Setenv("XDG_CURRENT_DESKTOP", "KDE")
	argv, _ = buildArgv(42, false)
	if !slices.Contains(argv, "--pid=42") || slices.Contains(argv, "/usr/bin/gnome-session-inhibit") {
		t.Errorf("outside GNOME argv %v must be systemd-inhibit + tail", argv)
	}

	lookPath = fakeLookPath("tail")
	if _, err := buildArgv(42, false); !errors.Is(err, ErrNoInhibitor) {
		t.Errorf("no inhibitor: err = %v", err)
	}
}

// TestStartRelease runs the real chain against this test process and checks
// that Release tears the whole process group down.
func TestStartRelease(t *testing.T) {
	p, err := Start(context.Background(), os.Getpid(), false, slog.Default())
	if errors.Is(err, ErrNoInhibitor) {
		t.Skip("no inhibitor installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
		t.Skipf("inhibitor exited at once (no session bus / logind?): %v", p.Err())
	case <-time.After(300 * time.Millisecond):
	}
	if err := p.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Release returned before the chain exited")
	}
	// The head is reaped; the rest of the group got the same SIGTERM and
	// may need a moment to finish dying.
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(-p.cmd.Process.Pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("process group still alive a second after Release")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}
