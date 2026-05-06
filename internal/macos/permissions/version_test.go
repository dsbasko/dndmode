//go:build darwin

package permissions_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestOSVersion_String_Formatting verifies the OSVersion.String() formatting
// contract used by user-facing stderr messages (wording in
// cmd/dndmode/main.go: "requires macOS 14 (Sonoma) or newer, got <major>.<minor>.").
//
// Table-driven per global Go test conventions. The
// fields under test are exported so the table cases construct them directly;
// no test deps needed for a pure-formatter test.
func TestOSVersion_String_Formatting(t *testing.T) {
	tests := []struct {
		name string
		ver  permissions.OSVersion
		want string
	}{
		{
			name: "typical sonoma point release",
			ver:  permissions.OSVersion{Major: 14, Minor: 5, Patch: 2},
			want: "14.5.2",
		},
		{
			name: "zero value formats to 0.0.0",
			ver:  permissions.OSVersion{},
			want: "0.0.0",
		},
		{
			name: "sequoia GA (no patch yet)",
			ver:  permissions.OSVersion{Major: 15, Minor: 0, Patch: 0},
			want: "15.0.0",
		},
		{
			name: "future big sur cousin (sanity, large numbers)",
			ver:  permissions.OSVersion{Major: 26, Minor: 11, Patch: 7},
			want: "26.11.7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ver.String(); got != tt.want {
				t.Errorf("OSVersion.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSentinelErrors_Wrapping verifies that ErrNonArm64 and ErrMacOSBelow14
// satisfy the errors.Is contract when wrapped via fmt.Errorf("%w: ...").
// cmd/dndmode/main.go relies on this to dispatch exit code 2 regardless of
// the wrapped suffix (→ exitPlatformErr).
func TestSentinelErrors_Wrapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		sentinel error
	}{
		{
			name:     "ErrNonArm64 wrapped with arch suffix",
			err:      fmt.Errorf("%w: got amd64", permissions.ErrNonArm64),
			sentinel: permissions.ErrNonArm64,
		},
		{
			name:     "ErrMacOSBelow14 wrapped with version suffix",
			err:      fmt.Errorf("%w: got 13.5.0", permissions.ErrMacOSBelow14),
			sentinel: permissions.ErrMacOSBelow14,
		},
		{
			name:     "ErrNonArm64 self-match (unwrapped)",
			err:      permissions.ErrNonArm64,
			sentinel: permissions.ErrNonArm64,
		},
		{
			name:     "ErrMacOSBelow14 self-match (unwrapped)",
			err:      permissions.ErrMacOSBelow14,
			sentinel: permissions.ErrMacOSBelow14,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.sentinel)
			}
		})
	}
}

// TestSentinelErrors_DistinctIdentity guards against accidental aliasing
// (e.g. ErrNonArm64 = ErrMacOSBelow14 typo) — wrapping one must not match the
// other under errors.Is.
func TestSentinelErrors_DistinctIdentity(t *testing.T) {
	if errors.Is(permissions.ErrNonArm64, permissions.ErrMacOSBelow14) {
		t.Errorf("ErrNonArm64 must not match ErrMacOSBelow14 under errors.Is")
	}
	if errors.Is(permissions.ErrMacOSBelow14, permissions.ErrNonArm64) {
		t.Errorf("ErrMacOSBelow14 must not match ErrNonArm64 under errors.Is")
	}
}
