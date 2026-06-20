//go:build darwin

package caffeinate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// quietLogger discards output — tests assert behavior, not log lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeStub creates an executable stub at a temp path and points binPath at it
// for the duration of the test. body is the shell script body. The stub lets
// the Start/Release lifecycle run hermetically without depending on the real
// /usr/bin/caffeinate (covered separately by the HEADLESS-gated smoketest).
//
// `exec`-ing the long-running command is important: a non-exec'd
// `sh -c 'sleep N'` may not forward SIGTERM to the sleeper, so Release would
// time out. exec replaces the shell so our child IS the sleeper.
func writeStub(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "caffeinate-stub")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	old := binPath
	binPath = script
	t.Cleanup(func() { binPath = old })
}

// TestBuildArgs pins the flag contract: -i/-s/-w always present, -d gated by
// allow_display_sleep, -w carries the watched PID. Order matters because -w
// consumes the next token as its pid argument.
func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name              string
		pid               int
		allowDisplaySleep bool
		want              []string
	}{
		{
			name:              "default keeps display awake (-d present)",
			pid:               4321,
			allowDisplaySleep: false,
			want:              []string{"-d", "-i", "-s", "-w", "4321"},
		},
		{
			name:              "allow_display_sleep drops -d",
			pid:               4321,
			allowDisplaySleep: true,
			want:              []string{"-i", "-s", "-w", "4321"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildArgs(tt.pid, tt.allowDisplaySleep)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildArgs(%d, %v) = %v, want %v", tt.pid, tt.allowDisplaySleep, got, tt.want)
			}
		})
	}
}

// TestProcess_StartRelease_Lifecycle covers the happy path: Start spawns a live
// child, Release SIGTERMs it, Done closes, and a second Release is a no-op.
func TestProcess_StartRelease_Lifecycle(t *testing.T) {
	writeStub(t, "#!/bin/sh\nexec sleep 30\n")

	p, err := Start(context.Background(), os.Getpid(), false, quietLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if p.Name() != "caffeinate" {
		t.Errorf("Name() = %q, want %q", p.Name(), "caffeinate")
	}

	// Child must still be running (not reaped) immediately after Start.
	select {
	case <-p.Done():
		t.Fatalf("child exited immediately; expected it to be running (Err=%v)", p.Err())
	default:
	}

	if err := p.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done() not closed after Release")
	}

	// Idempotent: a second Release on an already-stopped child returns nil.
	if err := p.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

// TestProcess_Release_AfterSelfExit verifies Release is a clean no-op when the
// child already exited on its own (Release's contract is "the assertion is
// gone", which a dead child satisfies).
func TestProcess_Release_AfterSelfExit(t *testing.T) {
	writeStub(t, "#!/bin/sh\nexit 0\n")

	p, err := Start(context.Background(), os.Getpid(), false, quietLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the child to exit on its own.
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("self-exiting stub never finished")
	}

	if err := p.Release(); err != nil {
		t.Errorf("Release after self-exit: %v", err)
	}
}

// TestProcess_CtxCancel_KillsChild verifies the ctx binding safety net: a
// cancelled context tears the child down even without an explicit Release.
func TestProcess_CtxCancel_KillsChild(t *testing.T) {
	writeStub(t, "#!/bin/sh\nexec sleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, os.Getpid(), true, quietLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child not reaped within 5s after ctx cancel")
	}
}

// TestStart_BadBinary_Errors verifies Start surfaces a wrapped error when the
// binary cannot be exec'd.
func TestStart_BadBinary_Errors(t *testing.T) {
	old := binPath
	binPath = filepath.Join(t.TempDir(), "definitely-not-here")
	t.Cleanup(func() { binPath = old })

	if _, err := Start(context.Background(), 1, false, quietLogger()); err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
}

// TestStart_CtxAlreadyCancelled_ReturnsContextCanceled pins the behavior that
// main.go's none path relies on: when ctx is already cancelled, exec's Start
// returns context.Canceled WITHOUT launching the child, and Start's %w wrap
// keeps errors.Is(err, context.Canceled) true so the caller can map it to a
// clean exit 0 instead of a false "binary missing" error.
func TestStart_CtxAlreadyCancelled_ReturnsContextCanceled(t *testing.T) {
	writeStub(t, "#!/bin/sh\nexec sleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Start

	_, err := Start(ctx, os.Getpid(), false, quietLogger())
	if err == nil {
		t.Fatal("expected error for cancelled ctx, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrapped context.Canceled", err)
	}
}

// TestProcess_Release_SIGKILLEscalation exercises the SIGTERM→grace→SIGKILL
// fallback: a stub that ignores SIGTERM forces Release past the (lowered) grace
// window into the Kill() branch. Without this, that branch (and its final
// <-p.done) never runs because every other stub exits promptly on SIGTERM.
func TestProcess_Release_SIGKILLEscalation(t *testing.T) {
	// `trap '' TERM` ignores SIGTERM; the loop keeps the shell alive so only
	// SIGKILL can stop it.
	writeStub(t, "#!/bin/sh\ntrap '' TERM\nwhile :; do sleep 1; done\n")

	oldGrace := releaseGrace
	releaseGrace = 150 * time.Millisecond
	t.Cleanup(func() { releaseGrace = oldGrace })

	p, err := Start(context.Background(), os.Getpid(), false, quietLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()
	if err := p.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Release took %v; SIGKILL escalation should bound it well under 5s", elapsed)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done() not closed after SIGKILL escalation")
	}
}
