//go:build darwin

package permissions_test

import (
	"os"
	"testing"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine for cgo-into-Foundation calls

	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestSmoke_Platform_VersionRoundtrip validates the cgo smoke layer
// for the platform.go + version_darwin.go pair:
//
//   - permissions.CurrentOSVersion() makes a real cgo call into
//     NSProcessInfo.operatingSystemVersion without panicking.
//   - The returned OSVersion has a Major component >= 10 (sanity baseline:
//     macOS 9 / Classic is long gone; we won't run on it).
//   - permissions.CurrentArch() returns a non-empty string (which is
//     runtime.GOARCH; should be "arm64" on the project's target platform,
//     but the smoke layer only enforces non-empty so the test stays useful
//     on alternative arches if the package is ever cross-built).
//
// Skipped on HEADLESS=1 so CI runners without a windowing session still
// pass; on a developer machine this is one of the first signals that the
// Foundation framework link was wired correctly. defer-recover catches the
// cgo-side abort case explicitly.
func TestSmoke_Platform_VersionRoundtrip(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires real macOS session; HEADLESS=1")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CurrentOSVersion / CurrentArch panicked: %v", r)
		}
	}()

	ver := permissions.CurrentOSVersion()
	if ver.Major < 10 {
		t.Errorf("CurrentOSVersion().Major = %d, want >= 10 (sanity baseline)", ver.Major)
	}

	arch := permissions.CurrentArch()
	if arch == "" {
		t.Errorf("CurrentArch() = %q, want non-empty (runtime.GOARCH)", arch)
	}
}
