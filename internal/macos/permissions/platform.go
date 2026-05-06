//go:build darwin

package permissions

import "fmt"

// CheckPlatform enforces the + contract: dndmode runs only
// on arm64 hosts (Apple Silicon) and only on macOS 14 (Sonoma) or newer.
//
// It is a pure-Go orchestrator — arch and ver are taken as parameters
// rather than read from runtime.GOARCH and NSProcessInfo directly so the
// function is testable on any architecture with hand-rolled inputs. The
// production callsite in cmd/dndmode/main.go threads CurrentArch() and
// CurrentOSVersion() through here.
//
// Ordering note: arch is checked before version. On a x86_64 box running
// macOS 12 the user should see "non-arm64" first — that is the more
// fundamental problem (no amount of system upgrade fixes it; only
// recompiling on Apple Silicon or buying one does). The macOS-version
// branch would otherwise suggest a misleading remediation.
//
// Return values:
//   - nil — arch == "arm64" and ver.Major >= 14.
//   - fmt.Errorf("%w: got <arch>", ErrNonArm64) — non-arm64 host. The
//     wrapping satisfies errors.Is(err, ErrNonArm64) for the main.go
// dispatch path (exit code 2 / exitPlatformErr).
//   - fmt.Errorf("%w: got <ver>", ErrMacOSBelow14) — macOS < 14. The
//     wrapping satisfies errors.Is(err, ErrMacOSBelow14) for the same
//     dispatch (also exit code 2). ver is rendered via OSVersion.String()
//     so the user sees "13.5.0" rather than the struct literal.
//
// Stderr wording (printed by main.go on bail-out, per the design notes
// "Specific Ideas"):
//
//	non-arm64: "dndmode: requires macOS on Apple Silicon (arm64), got darwin/<arch>."
//	old macOS: "dndmode: requires macOS 14 (Sonoma) or newer, got <major>.<minor>."
func CheckPlatform(arch string, ver OSVersion) error {
	if arch != "arm64" {
		return fmt.Errorf("%w: got %s", ErrNonArm64, arch)
	}
	if ver.Major < 14 {
		return fmt.Errorf("%w: got %s", ErrMacOSBelow14, ver)
	}
	return nil
}
