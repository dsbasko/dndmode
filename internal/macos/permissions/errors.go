//go:build darwin

package permissions

import "errors"

// ErrNonArm64 is returned by CheckPlatform when the host architecture is not
// arm64. cmd/dndmode/main.go uses errors.Is(err, permissions.ErrNonArm64) to
// dispatch exit code 2 (exitPlatformErr, covering).
//
// Wrapped via fmt.Errorf("%w: got %s", ErrNonArm64, arch) so the user-facing
// stderr message in main.go can reconstruct the offending GOARCH from the
// error chain (see the design notes "Specific Ideas" — stderr wording
// "dndmode: requires macOS on Apple Silicon (arm64), got darwin/<arch>.").
var ErrNonArm64 = errors.New("permissions: non-arm64 host")

// ErrMacOSBelow14 is returned by CheckPlatform when
// NSProcessInfo.operatingSystemVersion.majorVersion is below 14 (Sonoma).
// cmd/dndmode/main.go uses errors.Is(err, permissions.ErrMacOSBelow14) to
// dispatch exit code 2 (exitPlatformErr, covering).
//
// Wrapped via fmt.Errorf("%w: got %s", ErrMacOSBelow14, ver) so the
// user-facing stderr message in main.go can render the actual installed
// version (see the design notes "Specific Ideas" — stderr wording
// "dndmode: requires macOS 14 (Sonoma) or newer, got <major>.<minor>.").
var ErrMacOSBelow14 = errors.New("permissions: macOS below 14.0")
