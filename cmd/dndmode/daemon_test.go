//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These exercise the launcher half of --watch with /bin/sh standing in for
// the re-executed dndmode: the ready protocol on fd 3, exit-code mirroring,
// signal deaths, and the Ctrl-C forwarding. The dndmode half
// (becomeWatchDaemon) needs a registered Carbon hotkey and is covered by the
// acceptance tests that run the real binary.

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func shell(t *testing.T, ctx context.Context, script string, out *os.File, grace time.Duration) launchResult {
	t.Helper()
	if out == nil {
		out = devNull(t)
	}
	res := launchDetached(ctx, "/bin/sh", []string{"-c", script}, os.Environ(), out, out, grace)
	if res.pid > 0 {
		t.Cleanup(func() { _ = syscall.Kill(res.pid, syscall.SIGKILL) })
	}
	return res
}

func TestLaunchDetached_ReadyLeavesChildRunningInItsOwnSession(t *testing.T) {
	t.Parallel()
	res := shell(t, t.Context(), `printf 'ready\n' >&3; exec 3>&-; exec sleep 30`, nil, 2*time.Second)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if !res.ready {
		t.Fatalf("not ready: %+v", res)
	}
	// Still alive — the launcher must not have waited for it.
	if err := syscall.Kill(res.pid, 0); err != nil {
		t.Fatalf("child gone after ready: %v", err)
	}
	// Own session: closing our terminal must not SIGHUP it.
	sid, err := unix.Getsid(res.pid)
	if err != nil {
		t.Fatalf("getsid: %v", err)
	}
	if sid != res.pid {
		t.Errorf("child session = %d, want its own pid %d", sid, res.pid)
	}
	self, _ := unix.Getsid(0)
	if sid == self {
		t.Errorf("child shares the launcher's session %d", self)
	}
}

// TestLaunchDetached_InheritsStdio: everything the child prints before it is
// ready lands on the launcher's descriptors, not in a buffer the launcher
// would have to relay.
func TestLaunchDetached_InheritsStdio(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	res := shell(t, t.Context(), `echo hello-from-child; echo err-from-child >&2; printf 'ready\n' >&3`, f, 2*time.Second)
	if res.err != nil || !res.ready {
		t.Fatalf("launch = %+v", res)
	}
	// The echoes precede the token, but the file write and our read race
	// only through the page cache; poll briefly rather than assume.
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "hello-from-child") && strings.Contains(string(b), "err-from-child") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child output did not reach the inherited file; got %q", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLaunchDetached_MirrorsExitCode(t *testing.T) {
	t.Parallel()
	res := shell(t, t.Context(), `exit 7`, nil, 2*time.Second)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if res.ready || res.interrupted {
		t.Fatalf("result = %+v, want a plain failure", res)
	}
	if res.exitCode != 7 {
		t.Errorf("exitCode = %d, want 7", res.exitCode)
	}
}

func TestLaunchDetached_SignalDeathIsNegative(t *testing.T) {
	t.Parallel()
	res := shell(t, t.Context(), `kill -9 $$`, nil, 2*time.Second)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if res.ready || res.exitCode != -1 {
		t.Errorf("result = %+v, want exitCode -1", res)
	}
	if res.waitErr == nil {
		t.Error("waitErr = nil for a child killed by a signal")
	}
}

// TestLaunchDetached_TokenBeatsALingeringHolder: the launcher returns on the
// token even while a grandchild that inherited the pipe keeps it open —
// otherwise a `shortcuts` or `osascript` the daemon ran before it was ready
// could hold the terminal hostage.
func TestLaunchDetached_TokenBeatsALingeringHolder(t *testing.T) {
	t.Parallel()
	// The background sleep inherits fd 3 and holds it for 30 s; the shell
	// then reports ready and exits.
	res := shell(t, t.Context(), `sleep 30 & printf 'ready\n' >&3; exit 0`, nil, 2*time.Second)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if !res.ready {
		t.Fatalf("token was not honoured while a holder lingered: %+v", res)
	}
}

// TestLaunchDetached_ClosedPipeWithoutTokenIsNotReady pins the half of the
// protocol that matters most: EOF alone means nothing. A child that closes
// fd 3 and exits 0 has not become a daemon.
func TestLaunchDetached_ClosedPipeWithoutTokenIsNotReady(t *testing.T) {
	t.Parallel()
	res := shell(t, t.Context(), `exec 3>&-; exit 0`, nil, 2*time.Second)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if res.ready {
		t.Fatal("ready reported without the token")
	}
	if res.exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", res.exitCode)
	}
}

// TestLaunchDetached_InterruptForwardsSIGTERM: Ctrl-C at the launcher must
// reach the child, which is in another session and would otherwise keep
// starting up in the background.
func TestLaunchDetached_InterruptForwardsSIGTERM(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	// The trap runs between iterations; a single long sleep would defer it
	// until that sleep ended.
	res := shell(t, ctx, `trap 'exit 3' TERM; while :; do sleep 0.05; done`, nil, 5*time.Second)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if !res.interrupted {
		t.Fatalf("not interrupted: %+v", res)
	}
	if res.exitCode != 3 {
		t.Errorf("exitCode = %d, want 3 (the child's TERM handler)", res.exitCode)
	}
	if err := syscall.Kill(res.pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("child still alive after interrupt: kill(0) = %v", err)
	}
}

// TestLaunchDetached_GraceKillsAStuckChild: a child that neither reports nor
// exits after closing the pipe is killed after the grace period rather than
// blocking the terminal forever.
func TestLaunchDetached_GraceKillsAStuckChild(t *testing.T) {
	t.Parallel()
	res := shell(t, t.Context(), `exec 3>&-; trap '' TERM; exec sleep 30`, nil, 300*time.Millisecond)
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if res.ready {
		t.Fatal("ready without the token")
	}
	if res.waitErr == nil || !strings.Contains(res.waitErr.Error(), "killed") {
		t.Errorf("waitErr = %v, want the grace kill", res.waitErr)
	}
}

func TestLaunchDetached_BadExecutable(t *testing.T) {
	t.Parallel()
	res := launchDetached(t.Context(), filepath.Join(t.TempDir(), "missing"), nil, os.Environ(), devNull(t), devNull(t), time.Second)
	if res.err == nil {
		t.Fatal("launching a missing executable did not fail")
	}
}

func TestCheckReadyFD(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if err := checkReadyFD(int(w.Fd())); err != nil {
		t.Errorf("pipe write end rejected: %v", err)
	}
	if err := checkReadyFD(int(r.Fd())); err != nil {
		t.Errorf("pipe read end rejected: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := checkReadyFD(int(f.Fd())); err == nil || !strings.Contains(err.Error(), "not a pipe") {
		t.Errorf("regular file accepted: err = %v", err)
	}

	// A descriptor that is certainly closed.
	spare, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(spare)
	if err := checkReadyFD(spare); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Errorf("closed fd accepted: err = %v", err)
	}
}

func TestDaemonReadyPipe_NotADaemonWithoutTheVariable(t *testing.T) {
	// Not parallel: t.Setenv.
	t.Setenv(watchDaemonEnv, "")
	pipe, isDaemon, err := daemonReadyPipe()
	if err != nil || isDaemon || pipe != nil {
		t.Errorf("daemonReadyPipe() = %v, %v, %v; want nil, false, nil", pipe, isDaemon, err)
	}
}

// TestRedirectFDs: after the redirect, writes through the ORIGINAL descriptor
// number land in the target file — which is what keeps os.Stdout usable in
// the background process.
func TestRedirectFDs(t *testing.T) {
	t.Parallel()
	target, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	if err := redirectFDs(target, int(w.Fd())); err != nil {
		t.Fatalf("redirectFDs: %v", err)
	}
	if _, err := w.WriteString("redirected\n"); err != nil {
		t.Fatalf("write through redirected fd: %v", err)
	}
	b, err := os.ReadFile(target.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "redirected\n" {
		t.Errorf("target holds %q, want the write that went through the old fd number", b)
	}
}

func TestWatchingBanner_NamesKillNotCtrlC(t *testing.T) {
	t.Parallel()
	got := watchingBanner("Ctrl+Option+Cmd+D")
	if !strings.Contains(got, "Ctrl+Option+Cmd+D") || !strings.Contains(got, "--kill") {
		t.Errorf("banner = %q", got)
	}
	if strings.Contains(got, "Ctrl-C") {
		t.Errorf("banner still tells a terminal-less process about Ctrl-C: %q", got)
	}
}
