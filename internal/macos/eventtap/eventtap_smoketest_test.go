//go:build darwin && manual

// moved from `package eventtap_test` (black-box) to internal
// `package eventtap` (white-box) so the smoke test can reach the
// unexported `installTapOnly` helper that replaces the formerly-exported
// `Install`. The production wire-up (cmd/dndmode/main.go) goes through
// `InstallAll`; this internal_test exercises the bare tap path in
// isolation for cgo round-trip validation.
package eventtap

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/macos/permissions"
	"github.com/dsbasko/dndmode/internal/matcher"
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

	// The code mirrors the manual-test default (Ctrl+Option+Cmd+X) — a
	// 1-step unlock code, which is what the legacy `hotkey` key resolves to.
	// KeyCode 7 is `kVK_ANSI_X` on the US-ANSI layout (physical position
	// matched per).
	steps := []hotkey.Spec{{
		Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
		KeyCode:   7, // kVK_ANSI_X
	}}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sink := make(chan struct{}, 1)

	// installTapOnly takes the verifier, not the steps: masking and
	// construction moved to config.ResolveUnlockCode when the parameter
	// became a matcher.Verifier. ParseSequence-shaped Specs carry no bit
	// outside UserIntentionalMask, so wrapping them directly is what
	// production does too.
	r, err := installTapOnly(matcher.NewSequence(steps), sink, log)
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

// ---------------------------------------------------------------------------
// Keystroke-ring smoke tests (INTERACTIVE)
// ---------------------------------------------------------------------------
//
// Everything below needs a human at the keyboard. The ring is filled by the
// CGEventTap callback from real HID events, and this package deliberately
// exposes no way to inject a record: a test-only C setter was considered for
// the sequence matcher and rejected for the same reason the old
// `eventtap_test_set_expected` was deleted — a writable ring in the shipped
// binary is a lockout primitive for a process-injected adversary, and a second
// writer would also destroy the single-writer premise the whole correctness
// argument for eventtap_snapshot rests on (see tap_darwin.m).
//
// So the input is a person, and the tests are gated twice: the `manual` build
// tag (which `make test` and `make lint` never pass) plus
// DNDMODE_SMOKE_INTERACTIVE=1. Without the env var they skip, so
// `go test -tags manual ./...` stays non-blocking for anyone running it to
// check that the tagged files still COMPILE — which is what the
// `go vet -tags manual` gate in this plan's task list is for.
//
// ⚠️ WHILE THESE RUN, THE KEYBOARD AND TRACKPAD ARE FULLY BLOCKED. That is the
// product's entire job, and it applies to the test runner too: Ctrl+C is
// swallowed along with everything else. Every wait below is bounded by an
// explicit deadline and the tap is torn down from t.Cleanup, so the longest a
// run can hold the input devices is the sum of those deadlines (~1 minute).
// Run with a matching `-timeout` so a panic inside a wait cannot strand a live
// tap: go test -tags manual -timeout 120s -run Smoketest_Ring ./internal/macos/eventtap
//
// Prompts go to stderr with fmt.Fprintln rather than t.Log because t.Log is
// buffered until the test ends — an instruction the operator can only read
// after the window it describes has closed is worse than none.

// smokeInteractiveEnv gates the tests that require a human to type.
const smokeInteractiveEnv = "DNDMODE_SMOKE_INTERACTIVE"

// requireInteractiveSmoke skips unless a GUI session, an Accessibility grant,
// and explicit interactive opt-in are all present. Same skip conditions as the
// roundtrip smoke test above, plus the env gate.
func requireInteractiveSmoke(t *testing.T) {
	t.Helper()
	if os.Getenv("HEADLESS") != "" {
		t.Skip("interactive smoke test requires a GUI session; HEADLESS=1")
	}
	if os.Getenv(smokeInteractiveEnv) == "" {
		t.Skipf("interactive smoke test needs a human at the keyboard; set %s=1 to run "+
			"(it BLOCKS keyboard and trackpad for the duration)", smokeInteractiveEnv)
	}
	if !permissions.IsAccessibilityTrusted() {
		t.Skipf("requires Accessibility grant for the test binary identity (re-grant after each `go test` rebuild)")
	}
}

// installSmokeTap installs the bare tap for the given unlock code and
// registers teardown. It returns the sink the poller signals on a match.
//
// Release is registered with t.Cleanup rather than defer so it still runs if a
// later helper calls t.Fatalf — leaving a live tap behind would take the
// machine's input with it until the process exits.
func installSmokeTap(t *testing.T, steps []hotkey.Spec) chan struct{} {
	t.Helper()

	sink := make(chan struct{}, 1)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	r, err := installTapOnly(matcher.NewSequence(steps), sink, log)
	if err != nil {
		t.Skipf("installTapOnly failed (likely stale TCC grant): %v", err)
	}
	t.Cleanup(func() {
		if err := r.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	})
	return sink
}

// promptOperator prints an instruction to stderr immediately (unbuffered) and
// gives the operator a moment to read it before the window opens.
func promptOperator(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n>>> "+format+"\n", args...)
	time.Sleep(2 * time.Second)
}

// waitForSeq polls the C-side press counter until it reaches want or the
// deadline expires, and returns the last value it saw. Polling at
// pollInterval matches production's own cadence, so a failure here is a
// failure of the same probe the poller uses.
func waitForSeq(want uint64, timeout time.Duration) uint64 {
	deadline := time.Now().Add(timeout)
	var cur uint64
	for time.Now().Before(deadline) {
		cur = seq()
		if cur >= want {
			return cur
		}
		time.Sleep(pollInterval)
	}
	return cur
}

// ringTail returns the last n records of the ring as the poller would
// assemble them: snapshot the whole buffer, then index the half-open range
// [cur-n, cur) through the mask.
func ringTail(t *testing.T, n int) []matcher.KeyEvent {
	t.Helper()

	buf := make([]matcher.KeyEvent, ringCap)
	cur := newSnapshotFn()(buf)
	if cur < uint64(n) {
		t.Fatalf("ring holds %d records, need the last %d", cur, n)
	}
	tail := make([]matcher.KeyEvent, n)
	for i := range tail {
		tail[i] = buf[(cur-uint64(n)+uint64(i))&ringMask]
	}
	return tail
}

// TestEventTap_Smoketest_RingAccumulatesKeystrokes is the cgo round-trip the
// pure-Go tests cannot reach: a real key press travelling from the HID tap
// through the C callback's ring append, out through eventtap_snapshot, and
// into a matcher.KeyEvent with its fields intact.
//
// It asserts three things at once, because they only hold together:
//
//   - the counter advances by EXACTLY one per press (not zero — the callback
//     records; not more — KeyUp and FlagsChanged are suppressed without being
//     recorded, which is what keeps a 5-key code from needing a 10-record
//     window);
//   - the recorded tail matches what hotkey.ParseSequence produces for the
//     same keys, so keycode and modifier mapping survive the boundary;
//   - order is preserved across the ring index arithmetic.
//
// The unlock code installed here is the default Ctrl+Option+Cmd+X, which the
// typed keys deliberately do not match — the poller must stay silent while the
// ring fills.
func TestEventTap_Smoketest_RingAccumulatesKeystrokes(t *testing.T) {
	requireInteractiveSmoke(t)

	const typed = "q w e r t"
	want, err := hotkey.ParseSequence(typed)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", typed, err)
	}

	unlock, err := hotkey.ParseSequence("Ctrl+Option+Cmd+X")
	if err != nil {
		t.Fatalf("ParseSequence(unlock): %v", err)
	}
	sink := installSmokeTap(t, unlock)

	base := seq()
	promptOperator("Type the five letters %s (no modifiers, one press each). "+
		"Nothing will appear on screen — input is blocked. You have 20s.", typed)

	got := waitForSeq(base+uint64(len(want)), 20*time.Second)
	if got != base+uint64(len(want)) {
		t.Fatalf("press counter went %d -> %d, want exactly +%d "+
			"(a larger delta means extra presses were recorded — retype without "+
			"stray keys; a smaller one means presses were dropped)",
			base, got, len(want))
	}

	tail := ringTail(t, len(want))
	if !matcher.NewSequence(want).MatchTail(tail) {
		t.Errorf("ring tail does not match the typed keys.\n got: %+v\nwant: %+v\n"+
			"(keycodes are PHYSICAL positions — on a non-ANSI layout the letters "+
			"printed on the keys are not the ones this test names)", tail, want)
	}

	select {
	case <-sink:
		t.Errorf("poller signalled a match for keys that are not the unlock code")
	default:
	}
}

// TestEventTap_Smoketest_AutorepeatIsFiltered pins the brute-force guard: a
// held key produces a stream of KeyDown events with kCGKeyboardEventAutorepeat
// set, and the callback drops every one of them after the first.
//
// Without the filter, holding one key would sweep the ring at the system
// repeat rate (~15/s) instead of at typing speed, covering every
// repeated-character code for free. The property is invisible to any test that
// does not hold a real key down, which is why it lives here.
func TestEventTap_Smoketest_AutorepeatIsFiltered(t *testing.T) {
	requireInteractiveSmoke(t)

	unlock, err := hotkey.ParseSequence("Ctrl+Option+Cmd+X")
	if err != nil {
		t.Fatalf("ParseSequence(unlock): %v", err)
	}
	installSmokeTap(t, unlock)

	base := seq()
	promptOperator("HOLD the `a` key down for about 5 seconds, then release it. " +
		"Do not press anything else.")

	// Wait out the hold plus the release, then read the counter once. Waiting
	// for a specific value would defeat the point — the assertion is that the
	// value does NOT grow.
	time.Sleep(8 * time.Second)

	got := seq()
	if delta := got - base; delta != 1 {
		t.Errorf("press counter advanced by %d for one held key, want exactly 1 "+
			"(>1 means the autorepeat filter in eventtap_callback is not dropping "+
			"kCGKeyboardEventAutorepeat events; 0 means the key press never "+
			"reached the ring at all)", delta)
	}
}

// TestEventTap_Smoketest_MultiStepCodeMatches is the end-to-end proof of the
// feature: a multi-step unlock code typed on real hardware reaches the sink.
//
// It exercises the whole chain the unit tests can only exercise in pieces —
// callback → ring → eventtap_seq → eventtap_snapshot → matchAny's sliding
// window → matcher.Sequence.MatchTail → sink — and it is the only test in the
// suite where the window boundaries are set by human typing rhythm rather than
// by a fixture. A code typed across several 10ms poll ticks is the normal
// case, not an edge case: at typing speed every keystroke lands in a different
// tick.
//
// `q w e r` is used rather than a passphrase because it is four adjacent keys
// in one physical row, which keeps the instruction unambiguous on any layout.
func TestEventTap_Smoketest_MultiStepCodeMatches(t *testing.T) {
	requireInteractiveSmoke(t)

	const code = "q w e r"
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", code, err)
	}
	sink := installSmokeTap(t, steps)

	promptOperator("Type %s to unlock. Type a few other letters FIRST if you like — "+
		"the match is on the TAIL of the stream, so leading noise must not break it. "+
		"You have 20s.", code)

	select {
	case <-sink:
		// Matched — the tap is torn down by t.Cleanup.
	case <-time.After(20 * time.Second):
		t.Fatalf("unlock code %q typed but no match reached the sink within 20s", code)
	}
}
