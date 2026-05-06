//go:build darwin

package permissions_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestCheckPlatform_Validation covers the + contract
// implemented by permissions.CheckPlatform(arch, ver). Table-driven per
// global Go test conventions; sentinel matching via errors.Is to mirror the
// cmd/dndmode/main.go dispatch path (exit code 2).
//
// arch-first ordering matters: on a x86_64/macOS 12 box the user must see
// "non-arm64" first (more fundamental — recompiling won't help), not the
// macOS-version error which would suggest a system upgrade.
func TestCheckPlatform_Validation(t *testing.T) {
	tests := []struct {
		name          string
		arch          string
		ver           permissions.OSVersion
		wantErr       error  // sentinel to match via errors.Is; nil means no error.
		wantMsgSubstr string // optional substring in err.Error(); skipped if "".
	}{
		{
			name:    "arm64 + macOS 14 Sonoma minimum",
			arch:    "arm64",
			ver:     permissions.OSVersion{Major: 14},
			wantErr: nil,
		},
		{
			name:    "arm64 + macOS 15 Sequoia point release",
			arch:    "arm64",
			ver:     permissions.OSVersion{Major: 15, Minor: 1, Patch: 0},
			wantErr: nil,
		},
		{
			name:          "amd64 host on macOS 14 → ErrNonArm64",
			arch:          "amd64",
			ver:           permissions.OSVersion{Major: 14},
			wantErr:       permissions.ErrNonArm64,
			wantMsgSubstr: "amd64",
		},
		{
			name:          "arm64 host on macOS 13 Ventura → ErrMacOSBelow14",
			arch:          "arm64",
			ver:           permissions.OSVersion{Major: 13, Minor: 5},
			wantErr:       permissions.ErrMacOSBelow14,
			wantMsgSubstr: "13.5.0",
		},
		{
			name:          "x86_64 + macOS 12 → ErrNonArm64 wins over ErrMacOSBelow14",
			arch:          "x86_64",
			ver:           permissions.OSVersion{Major: 12},
			wantErr:       permissions.ErrNonArm64,
			wantMsgSubstr: "x86_64",
		},
		{
			name:    "arm64 + macOS 14.0.0 explicit zeros",
			arch:    "arm64",
			ver:     permissions.OSVersion{Major: 14, Minor: 0, Patch: 0},
			wantErr: nil,
		},
		{
			name:          "empty arch string → ErrNonArm64",
			arch:          "",
			ver:           permissions.OSVersion{Major: 14},
			wantErr:       permissions.ErrNonArm64,
			wantMsgSubstr: "got",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := permissions.CheckPlatform(tt.arch, tt.ver)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("CheckPlatform(%q, %v) = %v, want nil", tt.arch, tt.ver, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckPlatform(%q, %v) = nil, want error matching %v", tt.arch, tt.ver, tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("errors.Is(%v, %v) = false, want true", err, tt.wantErr)
			}
			if tt.wantMsgSubstr != "" && !strings.Contains(err.Error(), tt.wantMsgSubstr) {
				t.Errorf("err.Error() = %q, want substring %q", err.Error(), tt.wantMsgSubstr)
			}
		})
	}
}
