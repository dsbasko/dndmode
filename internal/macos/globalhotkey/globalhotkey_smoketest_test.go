//go:build darwin

// Live cgo smoke tests for the Carbon registration bridge. All are
// HEADLESS-gated (they talk to WindowServer through Carbon).
//
// There is deliberately no interactive env gate here, unlike
// internal/macos/eventtap: the one thing a human could contribute — pressing
// the combination — cannot be observed from `go test` at all, because Carbon
// dispatches hot-key events through a main run loop the test harness never
// spins. See TestSmoke_Register_Delivery.
package globalhotkey

import (
	"errors"
	"os"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine
)

// testCombo is the combination the smoke tests register. It is the shipped
// default for activate_hotkey, which makes these tests double as a check
// that the default is actually registrable.
const testCombo = "Ctrl+Option+Cmd+D"

func requireGUI(t *testing.T) {
	t.Helper()
	if os.Getenv("HEADLESS") != "" {
		t.Skip("Carbon hotkey registration requires a GUI session; HEADLESS=1")
	}
}

func mustParse(t *testing.T, s string) hotkey.Spec {
	t.Helper()
	spec, err := hotkey.Parse(s)
	if err != nil {
		t.Fatalf("hotkey.Parse(%q): %v", s, err)
	}
	return spec
}

// TestSmoke_Register_RoundTrip is the load-bearing one: it answers whether
// Carbon accepts a registration from this process at all, and whether
// Release actually gives the combination back (the second Register would
// fail with ErrComboTaken if it did not).
func TestSmoke_Register_RoundTrip(t *testing.T) {
	requireGUI(t)
	spec := mustParse(t, testCombo)

	ch, reg, err := Register(spec, nil)
	if errors.Is(err, ErrComboTaken) {
		t.Skipf("%s is already registered by another app on this machine", testCombo)
	}
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if ch == nil || reg == nil {
		t.Fatal("Register returned nil channel or nil registration on success")
	}
	if got := reg.Name(); got != "global-hotkey" {
		t.Errorf("Name() = %q, want %q", got, "global-hotkey")
	}

	if err := reg.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Idempotency — the LIFO cleanup stack may call this more than once.
	if err := reg.Release(); err != nil {
		t.Errorf("second Release should be a no-op, got: %v", err)
	}

	// Re-register: proves the first Release really handed the combination
	// back to the system rather than just flipping a Go-side flag.
	ch2, reg2, err := Register(spec, nil)
	if err != nil {
		t.Fatalf("re-Register after Release: %v", err)
	}
	_ = ch2
	if err := reg2.Release(); err != nil {
		t.Errorf("Release of second registration: %v", err)
	}
}

// TestSmoke_Register_SecondRegistrationRefused pins the one-per-process
// invariant. The Carbon handler routes to a single package-level channel, so
// a second live registration would have no way to say which combo fired.
func TestSmoke_Register_SecondRegistrationRefused(t *testing.T) {
	requireGUI(t)
	spec := mustParse(t, testCombo)

	_, reg, err := Register(spec, nil)
	if errors.Is(err, ErrComboTaken) {
		t.Skipf("%s is already registered by another app on this machine", testCombo)
	}
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	t.Cleanup(func() { _ = reg.Release() })

	if _, _, err := Register(spec, nil); !errors.Is(err, ErrAlreadyRegistered) {
		t.Errorf("second Register error = %v, want ErrAlreadyRegistered", err)
	}
}

// TestSmoke_Register_ModifierlessRefusedBeforeCarbon checks that the guard
// runs before anything is installed: a refused combination must leave the
// process with no registration at all, so a subsequent valid Register
// succeeds rather than hitting ErrAlreadyRegistered.
func TestSmoke_Register_ModifierlessRefusedBeforeCarbon(t *testing.T) {
	requireGUI(t)

	bare := mustParse(t, "Ctrl+D")
	bare.Modifiers = 0 // strip to a modifier-less spec the parser would reject

	if _, _, err := Register(bare, nil); !errors.Is(err, ErrNoModifier) {
		t.Fatalf("Register(modifier-less) error = %v, want ErrNoModifier", err)
	}

	spec := mustParse(t, testCombo)
	_, reg, err := Register(spec, nil)
	if errors.Is(err, ErrComboTaken) {
		t.Skipf("%s is already registered by another app on this machine", testCombo)
	}
	if err != nil {
		t.Fatalf("Register after a refused one: %v (refusal leaked state)", err)
	}
	_ = reg.Release()
}

// TestSmoke_Register_Delivery documents why delivery is NOT testable here, and
// always skips.
//
// An earlier version of this test waited on the channel for a keypress and
// failed when none arrived. It could never have passed: Carbon dispatches
// hot-key events through the main run loop of an application, and `go test`
// runs no [NSApp run] — test functions execute on scheduler-chosen goroutines
// while the main thread sits in the test harness. The press was delivered
// nowhere, and the failure looked exactly like a broken feature.
//
// Worse, it also made the SECOND requirement invisible. Delivery additionally
// needs the process to be at least NSApplicationActivationPolicyAccessory;
// under Prohibited (what cocoa.Init sets) registration returns noErr and the
// combination is genuinely claimed, but no press ever arrives. A test that
// cannot pass under any policy cannot distinguish the two.
//
// The real path is covered where it exists: cmd/dndmode calls
// cocoa.SetAtRestPolicyAccessory before registering, and the end-to-end check
// is running `dndmode --watch` and pressing the combination. Registration,
// release and re-registration — everything that does not need a run loop — is
// covered by the tests above, which do run in CI.
func TestSmoke_Register_Delivery(t *testing.T) {
	t.Skip("delivery cannot be exercised under `go test`: Carbon dispatches hot-key " +
		"events through the main [NSApp run] loop, which the test harness does not " +
		"provide, and it additionally requires activation policy >= Accessory. " +
		"Verify end-to-end with `dndmode --watch` instead.")
}
