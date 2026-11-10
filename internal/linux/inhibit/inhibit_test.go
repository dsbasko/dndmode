//go:build linux

package inhibit

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"sync/atomic"
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

// fakeChains launches sh -c script for each call, in order; the last script
// repeats. calls counts launches.
type fakeChains struct {
	scripts []string
	calls   atomic.Int32
}

func (f *fakeChains) start() (*Process, error) {
	n := int(f.calls.Add(1)) - 1
	s := f.scripts[min(n, len(f.scripts)-1)]
	return launch(context.Background(), []string{"sh", "-c", s}, slog.New(slog.DiscardHandler))
}

func shortKeepTimings(t *testing.T) {
	t.Helper()
	settle, lo, hi := keepSettle, keepRetryMin, keepRetryMax
	keepSettle, keepRetryMin, keepRetryMax = 50*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { keepSettle, keepRetryMin, keepRetryMax = settle, lo, hi })
}

func TestKeep_ChainDyingAtOnceIsAnError(t *testing.T) {
	shortKeepTimings(t)
	f := &fakeChains{scripts: []string{"exit 1"}}
	if _, err := keep(context.Background(), f.start, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("keep returned no error for a chain that exited at once")
	}
}

func TestKeep_RestartsADeadChain(t *testing.T) {
	shortKeepTimings(t)
	// The first chain outlives the settle wait and then dies; the second
	// holds until Release.
	f := &fakeChains{scripts: []string{"sleep 0.1", "exec sleep 30"}}
	k, err := keep(context.Background(), f.start, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		k.mu.Lock()
		cur := k.cur
		k.mu.Unlock()
		n := f.calls.Load()
		if n >= 2 && cur != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chain not restarted: %d launches", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	k.mu.Lock()
	cur := k.cur
	k.mu.Unlock()
	if err := k.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cur.Done():
	default:
		t.Fatal("Release left the restarted chain running")
	}
	if err := k.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

func TestKeep_ChainDyingAtOnceAfterRestartBacksOff(t *testing.T) {
	settle, lo, hi := keepSettle, keepRetryMin, keepRetryMax
	keepSettle, keepRetryMin, keepRetryMax = 30*time.Millisecond, 10*time.Millisecond, time.Second
	t.Cleanup(func() { keepSettle, keepRetryMin, keepRetryMax = settle, lo, hi })

	// Every restart starts fine and exits at once. Without backoff that is
	// a relaunch every ~10ms; doubling from 10ms gives a handful in 400ms.
	f := &fakeChains{scripts: []string{"sleep 0.05", "exit 1"}}
	k, err := keep(context.Background(), f.start, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := k.Release(); err != nil {
		t.Fatal(err)
	}
	if n := f.calls.Load(); n > 8 {
		t.Errorf("%d launches in 400ms: restarts of a chain that dies at once do not back off", n)
	}
}
