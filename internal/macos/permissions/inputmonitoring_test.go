//go:build darwin

// Pure-Go unit tests for the IMAccess typed enum exposed by
// inputmonitoring_darwin.go. The tests do NOT invoke cgo — they only assert
// constant values and method semantics. The cgo-backed CheckInputMonitoring
// is covered by the smoke test (permissions_smoketest_test.go).
//
// Naming follows Go Testing Conventions:
// Test<Entity>_<Method>_<Scenario>.
package permissions_test

import (
	"testing"

	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestIMAccess_IsGranted_Semantics covers the truth table of IsGranted:
// only IMAccessGranted (0) is truthy; everything else — including the
// defensive out-of-range value — is falsy.
func TestIMAccess_IsGranted_Semantics(t *testing.T) {
	tests := []struct {
		name string
		in   permissions.IMAccess
		want bool
	}{
		{name: "Granted is truthy", in: permissions.IMAccessGranted, want: true},
		{name: "Denied is falsy", in: permissions.IMAccessDenied, want: false},
		{name: "Unknown is falsy", in: permissions.IMAccessUnknown, want: false},
		{name: "out-of-range defensive falsy", in: permissions.IMAccess(99), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.IsGranted(); got != tt.want {
				t.Errorf("IMAccess(%d).IsGranted() = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestIMAccess_Constants_MatchIOKit asserts that our IMAccess constants
// numerically match the underlying kIOHIDAccessType* enum values from
// IOKit/hid/IOHIDLib.h:
//
//	kIOHIDAccessTypeGranted = 0
//	kIOHIDAccessTypeDenied  = 1
//	kIOHIDAccessTypeUnknown = 2
//
// If Apple ever renumbers these (extremely unlikely — it would break every
// CGEventTap-using app), this test fails with a clear message and the cgo
// cast in CheckInputMonitoring needs revisiting.
func TestIMAccess_Constants_MatchIOKit(t *testing.T) {
	tests := []struct {
		name string
		got  int
		want int
	}{
		{name: "IMAccessGranted == kIOHIDAccessTypeGranted (0)", got: int(permissions.IMAccessGranted), want: 0},
		{name: "IMAccessDenied == kIOHIDAccessTypeDenied (1)", got: int(permissions.IMAccessDenied), want: 1},
		{name: "IMAccessUnknown == kIOHIDAccessTypeUnknown (2)", got: int(permissions.IMAccessUnknown), want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %d, want %d (IOKit kIOHIDAccessType enum drift?)", tt.got, tt.want)
			}
		})
	}
}
