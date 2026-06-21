//go:build darwin

// Internal test (package audiomute) so it can construct execVolumeRunner with
// an overridden runCmd seam — the deliberate deviation from the focus clone
// documented in runner.go. The gomock-generated MockVolumeRunner is for
// CONSUMERS of VolumeRunner (Releaser, RecoverFromCrash), not for exercising
// ExecRunner's own stdout-parsing branches, which is what this file covers.

package audiomute

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// runnerTestDeps groups the runner-under-test with a scripted runCmd seam and
// captures what the seam was invoked with, so cases can assert both the parsed
// result and the exact osascript command line.
type runnerTestDeps struct {
	runner  execVolumeRunner
	gotName string
	gotArgs []string
	calls   int
}

// newRunnerTestDeps wires execVolumeRunner.runCmd to return the supplied
// (out, err) and record the invocation.
func newRunnerTestDeps(t *testing.T, out []byte, err error) *runnerTestDeps {
	t.Helper()
	d := &runnerTestDeps{}
	d.runner = execVolumeRunner{
		runCmd: func(_ context.Context, name string, args ...string) ([]byte, error) {
			d.calls++
			d.gotName = name
			d.gotArgs = args
			return out, err
		},
	}
	return d
}

func TestExecVolumeRunner_GetMuted_Scenarios(t *testing.T) {
	syntheticErr := errors.New("synthetic exec failure")

	tests := []struct {
		name      string
		out       []byte
		runErr    error
		wantMuted bool
		wantErr   bool
	}{
		{name: "true stdout => muted", out: []byte("true\n"), wantMuted: true},
		{name: "false stdout => not muted", out: []byte("false\n"), wantMuted: false},
		{name: "leading/trailing space trimmed", out: []byte("  true  "), wantMuted: true},
		{name: "garbage stdout => error", out: []byte("missing value"), wantErr: true},
		{name: "empty stdout => error", out: []byte(""), wantErr: true},
		{name: "exec error => error", out: nil, runErr: syntheticErr, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newRunnerTestDeps(t, tt.out, tt.runErr)

			got, err := deps.runner.GetMuted(context.Background())

			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetMuted: expected error, got nil (muted=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetMuted: unexpected error: %v", err)
			}
			if got != tt.wantMuted {
				t.Errorf("GetMuted: muted = %v, want %v", got, tt.wantMuted)
			}
			if deps.calls != 1 {
				t.Errorf("runCmd calls = %d, want 1", deps.calls)
			}
			if deps.gotName != osascriptPath {
				t.Errorf("invoked %q, want %q", deps.gotName, osascriptPath)
			}
			wantArgs := []string{"-e", "output muted of (get volume settings)"}
			if !equalArgs(deps.gotArgs, wantArgs) {
				t.Errorf("args = %v, want %v", deps.gotArgs, wantArgs)
			}
		})
	}
}

func TestExecVolumeRunner_SetMuted_Scenarios(t *testing.T) {
	syntheticErr := errors.New("synthetic exec failure")

	tests := []struct {
		name     string
		muted    bool
		runErr   error
		wantErr  bool
		wantArgs []string
	}{
		{name: "mute success", muted: true, wantArgs: []string{"-e", "set volume output muted true"}},
		{name: "unmute success", muted: false, wantArgs: []string{"-e", "set volume output muted false"}},
		{name: "exec error propagates", muted: false, runErr: syntheticErr, wantErr: true, wantArgs: []string{"-e", "set volume output muted false"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newRunnerTestDeps(t, nil, tt.runErr)

			err := deps.runner.SetMuted(context.Background(), tt.muted)

			if tt.wantErr {
				if err == nil {
					t.Fatal("SetMuted: expected error, got nil")
				}
			} else if err != nil {
				t.Fatalf("SetMuted: unexpected error: %v", err)
			}
			if deps.calls != 1 {
				t.Errorf("runCmd calls = %d, want 1", deps.calls)
			}
			if deps.gotName != osascriptPath {
				t.Errorf("invoked %q, want %q", deps.gotName, osascriptPath)
			}
			if !equalArgs(deps.gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", deps.gotArgs, tt.wantArgs)
			}
		})
	}
}

// TestStderrText verifies the *exec.ExitError stderr recovery used in error
// messages. A non-ExitError yields "" (no subprocess stderr to surface).
func TestStderrText(t *testing.T) {
	if got := stderrText(errors.New("plain error")); got != "" {
		t.Errorf("stderrText(plain) = %q, want empty", got)
	}
	exitErr := &exec.ExitError{Stderr: []byte("  boom  ")}
	if got := stderrText(exitErr); got != "boom" {
		t.Errorf("stderrText(exitErr) = %q, want %q", got, "boom")
	}
}

func TestNewExecRunner_ReturnsNonNilRunner(t *testing.T) {
	if NewExecRunner() == nil {
		t.Fatal("NewExecRunner returned nil; want non-nil VolumeRunner")
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
