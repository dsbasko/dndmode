//go:build darwin

package focus

//go:generate mockgen -source=shortcuts.go -destination=mocks/shortcuts.go -package=mocks -build_constraint=darwin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ShortcutsRunner abstracts /usr/bin/shortcuts CLI for unit-test injection.
// Production impl (execShortcutsRunner) wraps exec.CommandContext; tests
// inject a gomock-generated MockShortcutsRunner to drive deterministic
// scenarios (both shortcuts present, one missing, both missing, exec failure).
//
// Mirrors the DI seam pattern from internal/macos/powerassert/orphan.go
// (AssertionEnumerator / AssertionReleaser / LiveChecker): one method per
// distinct subprocess invocation, error semantics passed through unchanged.
type ShortcutsRunner interface {
	// List returns the names of all user-created shortcuts via
	// `shortcuts list`. One name per stdout line; whitespace-only lines
	// are filtered. Returns the wrapped exec error on subprocess failure.
	List(ctx context.Context) ([]string, error)
	// Run invokes `shortcuts run <name>`. Exit 0 = success; non-zero exit
	// is surfaced as a wrapped error including trimmed stderr text
	// (Apple's CLI is "exit 1 on any error" — see 05-the design notes "shortcuts
	// run exit code is binary"; the precise code for a missing shortcut
	// is captured by the smoke test TestShortcuts_RunMissing_ExitCode_Smoke
	// per the plan Open, informational only).
	Run(ctx context.Context, name string) error
}

// execShortcutsRunner is the production ShortcutsRunner backed by
// exec.CommandContext. ctx-cancel auto-kills the subprocess (SIGKILL on
// ctx.Done()) — main.go's signal.NotifyContext cancels ctx, so a child
// shortcuts process never outlives its parent dndmode.
type execShortcutsRunner struct{}

// List wraps `shortcuts list`. Stdout is parsed line-by-line; whitespace-only
// lines (including a trailing empty line from the final newline) are skipped.
// The result slice is nil when no shortcuts exist — callers compare with the
// "missing names" set directly, so an empty slice is the natural "all missing"
// signal.
func (execShortcutsRunner) List(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "shortcuts", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("shortcuts list: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// Run wraps `shortcuts run <name>`. Stderr is captured to a bounded
// bytes.Buffer (user shortcut output is typically <1KB; pathological input
// would still be bounded by Go's slice growth). Non-nil exec error is
// returned wrapped with the shortcut name and trimmed stderr, so callers
// can surface a diagnosable message to the user (warn-on-fail).
func (execShortcutsRunner) Run(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "shortcuts", "run", name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("shortcuts run %q: %w (stderr: %s)",
			name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// NewExecRunner returns the production ShortcutsRunner wrapping
// /usr/bin/shortcuts. Called from cmd/dndmode/main.go Step 9.5
// (CheckShortcuts), Step 10.5 (RecoverFromCrash),
// and Step 13.7 (Activate). The implementation is stateless;
// the same instance can be safely shared across all three call sites.
func NewExecRunner() ShortcutsRunner { return execShortcutsRunner{} }
