//go:build darwin

package focus_test

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/dsbasko/dndmode/internal/macos/focus"
	"github.com/dsbasko/dndmode/internal/macos/focus/mocks"
)

// Compile-time guarantee: MockShortcutsRunner satisfies the
// ShortcutsRunner interface. Mismatch (signature drift after refactor)
// surfaces here at build time, before any downstream plan that imports
// the mock breaks.
var _ focus.ShortcutsRunner = (*mocks.MockShortcutsRunner)(nil)

// TestShortcutsRunner_List_MockReturnsConfiguredNames verifies the
// MockShortcutsRunner contract: when EXPECT().List(ctx) is wired with
// a slice, the mock returns the slice and a nil error. Validates that
// can inject the mock wherever focus.ShortcutsRunner
// is expected (DI seam contract).
//
// Validation map ID: 5-01-01.
func TestShortcutsRunner_List_MockReturnsConfiguredNames(t *testing.T) {
	deps := newTestDeps(t)
	want := []string{"dndmode-on", "dndmode-off", "other-shortcut"}
	deps.mockRunner.EXPECT().List(gomock.Any()).Return(want, nil)

	got, err := deps.mockRunner.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestShortcutsRunner_Run_MockPropagatesError verifies the
// MockShortcutsRunner contract for the Run path: when EXPECT().Run
// returns an error, the mock propagates it unchanged. 's
// Releaser uses this exact contract to warn+continue on Deactivate
// failure.
//
// Validation map ID: 5-01-01.
func TestShortcutsRunner_Run_MockPropagatesError(t *testing.T) {
	deps := newTestDeps(t)
	wantErr := errors.New("shortcuts run: synthetic failure")
	deps.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(wantErr)

	err := deps.mockRunner.Run(context.Background(), "dndmode-off")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestNewExecRunner_ReturnsNonNilRunner is a compile-time + run-time
// sanity check that the production constructor returns a non-nil
// ShortcutsRunner. We do NOT invoke the underlying CLI here — that
// lives in focus_smoketest_test.go, HEADLESS-gated.
func TestNewExecRunner_ReturnsNonNilRunner(t *testing.T) {
	runner := focus.NewExecRunner()
	if runner == nil {
		t.Fatal("NewExecRunner returned nil; want non-nil ShortcutsRunner")
	}
}
