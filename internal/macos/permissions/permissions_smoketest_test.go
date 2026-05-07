//go:build darwin

// Package permissions_test hosts cgo smoke tests for the AX / IM /
// SecureEventInput probes. All tests are HEADLESS-gated (SKIP under
// HEADLESS=1) because they exercise live TCC / Carbon framework calls
// which require a GUI session and (for AX) may briefly flash a system
// prompt dialog on first invocation.
//
// Return values are NOT asserted — they depend on the dev machine's
// current TCC state and on whether a sudo / password prompt is open at
// test time. The smoke test only protects against:
//
//   - cgo linker errors (undefined Framework symbol),
//   - cgo runtime panics inside the .m bridge,
//   - signature drift between Go and Objective-C sides.
//
// Substantive testing of pure-Go logic (e.g. IMAccess.IsGranted) lives in
// the per-bridge *_test.go files; cgo non-panic checks live here.
//
// Per: smoke layer = real cgo non-panic on dev machines; SKIP in CI
// via HEADLESS=1. Per: PromptAccessibility() is safe to call in tests
// because macOS dedupes the prompt dialog by cdhash+user — re-calls never
// re-prompt.
package permissions_test

import (
	"os"
	"testing"

	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestSmoke_AX_IsTrusted_NonPanic verifies that the silent AX trust check
// does not panic. Return value is env-dependent (depends on whether the
// test binary's cdhash is in the user's Accessibility list at TCC level).
func TestSmoke_AX_IsTrusted_NonPanic(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("IsAccessibilityTrusted panicked: %v", r)
		}
	}()
	_ = permissions.IsAccessibilityTrusted()
}

// TestSmoke_AX_Prompt_NonPanic verifies that the prompt variant does not
// panic. macOS dedupes the prompt dialog by cdhash+user, so calling this
// during tests is safe — on a machine where the test binary is already
// trusted, no UI flashes; on a fresh binary, exactly one dialog appears
// once per (cdhash, user) tuple and never again. contract.
func TestSmoke_AX_Prompt_NonPanic(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("PromptAccessibility panicked: %v", r)
		}
	}()
	_ = permissions.PromptAccessibility()
}
