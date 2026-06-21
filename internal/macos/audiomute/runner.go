//go:build darwin

package audiomute

//go:generate mockgen -source=runner.go -destination=mocks/runner.go -package=mocks -build_constraint=darwin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// osascriptPath is the absolute path to the AppleScript interpreter. Using the
// absolute path (vs. a bare "osascript" resolved through $PATH) keeps the
// invocation deterministic regardless of the launching environment.
const osascriptPath = "/usr/bin/osascript"

// VolumeRunner abstracts /usr/bin/osascript volume control for unit-test
// injection. Production impl (execVolumeRunner) wraps exec.CommandContext;
// CONSUMERS of VolumeRunner (the Releaser, RecoverFromCrash) inject a
// gomock-generated MockVolumeRunner to drive deterministic scenarios.
//
// Mirrors the DI seam pattern from internal/macos/focus/shortcuts.go
// (ShortcutsRunner): one method per distinct subprocess invocation, error
// semantics passed through unchanged.
type VolumeRunner interface {
	// GetMuted reports whether system audio output is currently muted via
	// `osascript -e 'output muted of (get volume settings)'`. AppleScript
	// prints the boolean as the literal "true"/"false" on stdout; anything
	// else (empty, garbage) is surfaced as a wrapped error so the caller can
	// refuse to mute without a recorded prior state.
	GetMuted(ctx context.Context) (bool, error)
	// SetMuted sets system audio output mute via
	// `osascript -e 'set volume output muted true|false'`. Exit 0 = success;
	// non-zero exit is surfaced as a wrapped error including trimmed stderr.
	SetMuted(ctx context.Context, muted bool) error
}

// execVolumeRunner is the production VolumeRunner backed by exec.CommandContext.
//
// DELIBERATE deviation from the focus clone: execVolumeRunner carries an
// unexported runCmd exec seam (defaulting to defaultRunCmd) so GetMuted's
// stdout-parsing branches (true / false / garbage / exec-error) are
// unit-testable without a real osascript binary. The focus package has no
// such seam (execShortcutsRunner is concrete) because its ExecRunner carries
// no stdout-parsing logic worth unit-testing in isolation.
type execVolumeRunner struct {
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// defaultRunCmd is the real exec path. cmd.Output() returns stdout; on a
// non-zero exit the *exec.ExitError it returns carries the child's stderr in
// its Stderr field (populated because we leave cmd.Stderr unset), which
// stderrText recovers for diagnostics.
func defaultRunCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// GetMuted implements VolumeRunner. Trimmed stdout must equal "true" or
// "false"; any other value (including empty) is an error.
func (r execVolumeRunner) GetMuted(ctx context.Context) (bool, error) {
	out, err := r.runCmd(ctx, osascriptPath, "-e", "output muted of (get volume settings)")
	if err != nil {
		return false, fmt.Errorf("osascript get muted: %w (stderr: %s)", err, stderrText(err))
	}
	switch s := strings.TrimSpace(string(out)); s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("osascript get muted: unexpected output %q", s)
	}
}

// SetMuted implements VolumeRunner. The boolean is rendered with %t, yielding
// the AppleScript-valid `set volume output muted true|false`.
func (r execVolumeRunner) SetMuted(ctx context.Context, muted bool) error {
	script := fmt.Sprintf("set volume output muted %t", muted)
	if _, err := r.runCmd(ctx, osascriptPath, "-e", script); err != nil {
		return fmt.Errorf("osascript set muted %t: %w (stderr: %s)", muted, err, stderrText(err))
	}
	return nil
}

// stderrText extracts the trimmed stderr from a subprocess *exec.ExitError
// (cmd.Output() stashes it there when cmd.Stderr is unset). Non-ExitError
// errors (ctx cancellation, binary-not-found) carry no stderr → "".
func stderrText(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
}

// NewExecRunner returns the production VolumeRunner wrapping /usr/bin/osascript.
// Called from cmd/dndmode/main.go Step 13.3/13.7 (mute lifecycle) and Step
// 10.5 (RecoverFromCrash). The implementation is stateless; the same instance
// can be safely shared across call sites.
func NewExecRunner() VolumeRunner {
	return execVolumeRunner{runCmd: defaultRunCmd}
}
