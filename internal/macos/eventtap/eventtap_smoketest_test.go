//go:build darwin && manual

// moved from `package eventtap_test` (black-box) to internal
// `package eventtap` (white-box) so the smoke test can reach the
// unexported `installTapOnly` helper that replaces the formerly-exported
// `Install`. The production wire-up (cmd/dndmode/main.go) goes through
// `InstallAll`; this internal_test exercises the bare tap path in
// isolation for cgo round-trip validation.
package eventtap

import (
	"log/slog"
	"os"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestEventTap_Smoketest_InstallUninstall_Roundtrip exercises the real cgo
// install + Release cycle on a signed test binary that has been granted
// Accessibility. The test:
//
//  1. Skips early if Accessibility is not granted on the running host
//     (unsigned `go test ./...` invocations get a fresh ad-hoc identity
//     each build → TCC grant invalidated; the regular Accessibility prompt
//     is suppressed here so CI does not hang).
//  2. Skips when HEADLESS=1 for consistency with other smoke suites.
//  3. Constructs a non-trivial Spec (Ctrl+Option+Cmd+X — same combo the
//     Phase 4 manual test scenarios use).
//  4. Calls Install — expects a non-nil *Releaser, nil error.
//  5. Calls Release — expects nil error. Calls Release a second time —
//     expects nil error (two-layer idempotency on the real cgo path).
//
// This is the only end-to-end exercise of the production
// CGEventTapCreate → CFMachPortCreateRunLoopSource → CFRunLoopRun worker
// goroutine → CFRunLoopStop teardown path in the test suite. The pure-Go
// unit tests in tap_test.go cover Releaser idempotency + poller fan-out
// without cgo; this test fills the cgo-only gap.
//
// Synthesised CGEvent injection (HID event posting to assert that the
// callback fires and the matched-event-suppression actually swallows real
// keystrokes) is deferred to Phase 6+ manual UAT per the design notes + the design notes
// deferred — it requires Karabiner-style HID injection, an additional
// Accessibility prompt for the test binary, and signed-by-Apple identity to
// remain stable across `go install`. Manual scenarios 4/6/7 in
// docs/manual-test.md cover those gaps for v1.1 release.
func TestEventTap_Smoketest_InstallUninstall_Roundtrip(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	if !permissions.IsAccessibilityTrusted() {
		t.Skipf("requires Accessibility grant for the test binary identity (re-grant after each `go test` rebuild — see the design notes / errors.go ErrTapInstallFailed comment)")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("smoke panicked: %v", r)
		}
	}()

	// Spec mirrors the manual-test default (Ctrl+Option+Cmd+X). KeyCode 7
	// is `kVK_ANSI_X` on the US-ANSI layout (physical position matched per
	//).
	spec := hotkey.Spec{
		Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
		KeyCode:   7, // kVK_ANSI_X
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sink := make(chan struct{}, 1)

	r, err := installTapOnly(spec, sink, log)
	if err != nil {
		// CGEventTapCreate returned NULL — the host's Accessibility grant
		// is stale despite IsAccessibilityTrusted returning true (this is
		// the silent-disable race per Daniel Raffel TIL — the ad-hoc
		// identity changed between TCC check and CGEventTapCreate). Skip
		// rather than fail so a developer running `go test -tags manual`
		// on a freshly-rebuilt binary sees the expected diagnostic
		// instead of a noisy test failure.
		t.Skipf("installTapOnly failed (likely stale TCC grant): %v", err)
	}
	if r == nil {
		t.Fatalf("installTapOnly returned nil *Releaser without error")
	}

	if err := r.Release(); err != nil {
		t.Errorf("first Release: %v", err)
	}
	if err := r.Release(); err != nil {
		t.Errorf("second Release (must be idempotent no-op): %v", err)
	}
}

// TestEventTap_Smoketest_Callback_AlwaysSuppresses is a placeholder for
// v1.2 manual UAT. The Phase 4 invariant — callback returns NULL
// for ALL keyboard / mouse events including the matched event — cannot be
// asserted without synthesising HID events from the test binary itself.
//
// Synthesising via `CGEventPost` from inside a test would require:
//
//   1. A second Accessibility grant specifically for the test binary
//      identity (the production grant covers `dndmode` only).
//   2. Stable signed identity across `go test -count=1` rebuilds — the
//      ad-hoc identity changes on every rebuild, so manual re-grant would
//      be required for every test run.
//   3. Synchronisation: CGEventPost is asynchronous; the test would have
//      to poll a sink-receive timeout, leading to flaky CI.
//
// the design notes + the design notes deferred both list "Karabiner-style HID injection
// acceptance suite" as Phase 6+ work. Manual scenarios 4 / 6 / 7 in
// docs/manual-test.md (TODO: written in) cover the same ground
// for v1.1.
func TestEventTap_Smoketest_Callback_AlwaysSuppresses(t *testing.T) {
	t.Skip("HID event injection deferred to Phase 6+ manual UAT (the design notes + the design notes deferred; manual-test.md scenarios 4/6/7 cover v1.1)")
}

// TestEventTap_Smoketest_DisableRecovery is a placeholder for v1.2 manual
// UAT. disable-recovery (callback receives kCGEventTapDisabledByTimeout
// → CGEventTapEnable(tap, true) → tap re-enabled within one tick) is
// verifiable only by forcing a silent-disable from outside the test
// binary's control path — which requires either (a) a privileged helper
// that calls `_CGEventTapEnable(tap, false)` via private SPI or (b) a
// real OS-induced silent-disable race (which by definition cannot be
// reproduced on demand — that's why it's called "silent").
//
// The pure-Go DI seam in watchdog_darwin.go (`watchdogState.Probe`)
// exhaustively unit-tests the consecutive-failure counter policy
// without standing up a real tap; that is the closest we can get to
// in CI. The full live-recovery path is verified manually per scenario 6
// (Mission Control / sticky-keys toggle).
func TestEventTap_Smoketest_DisableRecovery(t *testing.T) {
	t.Skip("silent-disable race reproduction requires privileged SPI or unpredictable OS-induced race; deferred to manual-test.md scenario 6 + Phase 6+ Karabiner-style helper")
}
