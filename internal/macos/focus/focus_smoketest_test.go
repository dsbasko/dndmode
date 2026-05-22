//go:build darwin

// Smoke tests for the focus package. All tests are HEADLESS-gated (SKIP
// under HEADLESS=1) AND additionally LookPath-gated for `shortcuts` —
// CI hosts may lack the Apple Shortcuts CLI even when HEADLESS is not
// set. Mirrors permissions/permissions_smoketest_test.go layout.
//
// Return values are NOT asserted: the dev host's Shortcuts library
// content is unknown and varies per machine. The smoke layer only
// protects against:
//
//   - panics inside execShortcutsRunner (Go panic, runtime error,
//     unparseable subprocess output that would crash strings.Split,
//     etc. — all unlikely but cheap to guard).
//   - signature drift between focus.ShortcutsRunner and the production
//     impl returned by NewExecRunner.
//
// TestShortcuts_RunMissing_ExitCode_Smoke additionally addresses
// the plan it logs the empirical exit code of
// `shortcuts run "<nonexistent>"` via t.Logf and DOES NOT assert it
// (the design notes). The observed value is captured into design notes
// for Phase 6 README documentation.

package focus_test

import (
	"context"
	"encoding/hex"
	"math/rand"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/macos/focus"
)

// randHex returns 8 lowercase hex characters derived from the given
// random source. Used to build a name that is statistically guaranteed
// to NOT exist in the user's Shortcuts library, so the smoke test does
// not depend on a specific library state.
func randHex(rng *rand.Rand) string {
	var buf [4]byte
	_, _ = rng.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// TestSmoke_ShortcutsList_NonPanic verifies the production
// execShortcutsRunner.List does not panic. Return value (the user's
// shortcut names) is NOT asserted.
func TestSmoke_ShortcutsList_NonPanic(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires live /usr/bin/shortcuts; HEADLESS=1")
	}
	if _, err := exec.LookPath("shortcuts"); err != nil {
		t.Skipf("shortcuts CLI not available: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("execShortcutsRunner.List panicked: %v", r)
		}
	}()
	runner := focus.NewExecRunner()
	_, _ = runner.List(context.Background())
}

// TestShortcuts_RunMissing_ExitCode_Smoke addresses the plan Open
// Question: empirically capture the exit code of
// `shortcuts run "<nonexistent>"`. The result is logged via t.Logf and
// NOT asserted — per the design notes, this is informational only. The
// maintainer copies the observed value into the
// Phase 6 README so users know what to expect when they fat-finger a
// shortcut name.
func TestShortcuts_RunMissing_ExitCode_Smoke(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires live /usr/bin/shortcuts; HEADLESS=1")
	}
	if _, err := exec.LookPath("shortcuts"); err != nil {
		t.Skipf("shortcuts CLI not available: %v", err)
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	name := "nonexistent-dndmode-" + randHex(rng)
	cmd := exec.Command("shortcuts", "run", name)
	_ = cmd.Run()
	t.Logf("shortcuts run %q exit code: %d (informational; document in design notes)",
		name, cmd.ProcessState.ExitCode())
}
