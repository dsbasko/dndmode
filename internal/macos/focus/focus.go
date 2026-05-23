//go:build darwin

package focus

import (
	"context"
)

// Activate invokes `shortcuts run dndmode-on` to enable Focus / DND.
//. the design notes: best-effort — main.go (Step 13.7)
// calls this AFTER powerassert.Acquire + runtime.Manager.Write succeed,
// logs slog.Warn on failure, and continues startup. The Focus icon
// in the menu bar is the user's visual confirmation; failure is not
// fatal because the dndmode core value (awake-lock + overlay) is
// already active by this point.
//
// The function is stateless; it does NOT itself log or wrap the
// runner.Run error. Callers attach context (operation name, slog
// fields) at their call site.
//
// MUST be called AFTER powerassert.Acquire + runtime.Manager.Write so
// runtime.json's assertion_id reflects the running assertion.
func Activate(ctx context.Context, runner ShortcutsRunner) error {
	return runner.Run(ctx, "dndmode-on")
}

// Deactivate invokes `shortcuts run dndmode-off`. Called both from
// (a) the *Releaser path on normal exit (LIFO step 4), and
// (b) RecoverFromCrash when runtime.json from a previous
// run indicates the prior dndmode died holding a Focus assertion.
//
// Returns the underlying runner.Run error verbatim. The Releaser layer
// swallows + warns (best-effort, see releaser.go); the recovery
// layer logs warn+continue (same policy). Keeping Deactivate neutral
// lets both call sites apply their own dispatch policy.
func Deactivate(ctx context.Context, runner ShortcutsRunner) error {
	return runner.Run(ctx, "dndmode-off")
}
