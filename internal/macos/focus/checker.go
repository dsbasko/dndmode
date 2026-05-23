//go:build darwin

package focus

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// CheckShortcuts implements: verifies both `dndmode-on` and
// `dndmode-off` exist in the user's Shortcuts library before any IOPM
// resource is acquired. the design notes: check BOTH upfront — never let
// the user enter Active state only to discover dndmode-off is missing
// on exit. the design notes: invoked at PreFlight Step 9.5 (after
// permissions.WaitForGrants, before powerassert.CleanupOrphans) so a
// missing-shortcut failure exits cleanly without holding IOKit
// resources.
//
// Returns:
//   - nil — both shortcuts present (whitespace-filtered set membership).
//   - fmt.Errorf("%w: need <sorted-names>", ErrShortcutsMissing, ...) —
// one or both required names absent. main.go dispatches
//     via errors.Is(err, ErrShortcutsMissing) and maps to exit code 6
// (exitFocusSetup) per the design notes, printing the missing names
//     alongside the first-run bootstrap instruction on stderr. Names
//     are sorted (sort.Strings) so the wording is stable across runs —
// 's acceptance test can match the literal substring.
//   - fmt.Errorf("list shortcuts: %w", err) — runner.List failed
//     (subprocess error). Treated as exitPlatformErr by main.go, NOT
//     as ErrShortcutsMissing. The underlying error is unwrappable via
//     errors.Is for diagnostics.
//
// The function is stateless and goroutine-safe; ctx is propagated to
// runner.List for cancellation (SIGINT during PreFlight → ctx cancel →
// shortcuts subprocess dies → this returns the wrapped context error).
func CheckShortcuts(ctx context.Context, runner ShortcutsRunner) error {
	names, err := runner.List(ctx)
	if err != nil {
		return fmt.Errorf("list shortcuts: %w", err)
	}
	have := make(map[string]struct{}, len(names))
	for _, n := range names {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			have[trimmed] = struct{}{}
		}
	}
	var missing []string
	for _, want := range []string{"dndmode-on", "dndmode-off"} {
		if _, ok := have[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: need %s", ErrShortcutsMissing, strings.Join(missing, ", "))
	}
	return nil
}
