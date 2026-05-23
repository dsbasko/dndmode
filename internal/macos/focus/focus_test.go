//go:build darwin

package focus_test

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/dsbasko/dndmode/internal/macos/focus"
)

// TestActivate_CallsRunOn verifies happy path: Activate invokes
// runner.Run with the exact name "dndmode-on" and returns nil on
// success.
//
// Validation map ID: 5-03-01.
func TestActivate_CallsRunOn(t *testing.T) {
	td := newTestDeps(t)
	td.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-on").Return(nil)

	if err := focus.Activate(context.Background(), td.mockRunner); err != nil {
		t.Fatalf("Activate returned %v; want nil", err)
	}
}

// TestActivate_BestEffort_PropagatesError verifies that Activate
// surfaces the underlying runner error verbatim. "Best-effort" semantics
// belong to the CALLER (main.go logs warn but does not block startup);
// the function itself is just a thin wrapper that preserves the error
// for diagnostics.
//
// Validation map ID: 5-03-02.
func TestActivate_BestEffort_PropagatesError(t *testing.T) {
	td := newTestDeps(t)
	sentinel := errors.New("shortcuts run dndmode-on: synthetic failure")
	td.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-on").Return(sentinel)

	err := focus.Activate(context.Background(), td.mockRunner)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Activate err = %v; want errors.Is(err, sentinel)", err)
	}
}

// TestDeactivate_CallsRunOff verifies the symmetric happy path for
// Deactivate invokes runner.Run with the exact name
// "dndmode-off" and returns nil.
func TestDeactivate_CallsRunOff(t *testing.T) {
	td := newTestDeps(t)
	td.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(nil)

	if err := focus.Deactivate(context.Background(), td.mockRunner); err != nil {
		t.Fatalf("Deactivate returned %v; want nil", err)
	}
}

// TestDeactivate_PropagatesError verifies that Deactivate propagates
// the underlying runner error unchanged. The Releaser layer (
// releaser.go) is what swallows the error per Deactivate itself
// stays neutral so recovery.go can dispatch on it.
func TestDeactivate_PropagatesError(t *testing.T) {
	td := newTestDeps(t)
	sentinel := errors.New("shortcuts run dndmode-off: synthetic failure")
	td.mockRunner.EXPECT().Run(gomock.Any(), "dndmode-off").Return(sentinel)

	err := focus.Deactivate(context.Background(), td.mockRunner)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Deactivate err = %v; want errors.Is(err, sentinel)", err)
	}
}
