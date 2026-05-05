//go:build darwin

package permissions

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Foundation

#include <stdint.h>

extern void permissions_os_version(long *major, long *minor, long *patch);
*/
import "C"

import (
	"fmt"
	"runtime"
)

// OSVersion is the parsed NSOperatingSystemVersion triple as returned by
// [[NSProcessInfo processInfo] operatingSystemVersion]. The fields are
// exported so tests can construct synthetic versions and pass them to
// CheckPlatform without invoking cgo.
type OSVersion struct {
	Major int
	Minor int
	Patch int
}

// String formats the version as "MAJOR.MINOR.PATCH" — the form used in the
// user-facing stderr message rendered by cmd/dndmode/main.go on a
// non-Sonoma host.
func (v OSVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// CurrentOSVersion reads NSProcessInfo.operatingSystemVersion via Foundation
// cgo and returns the {major, minor, patch} triple.
//
// Thread-safe: the underlying Objective-C call is stateless and the .m
// implementation is wrapped in @autoreleasepool (mitigation).
// Cheap enough to call repeatedly — there is no caching layer.
//
// On a stripped-down sandbox where NSProcessInfo returns zeros, the result
// is the zero OSVersion{} (Major=0). CheckPlatform treats that as
// ErrMacOSBelow14 just like macOS 13 — the user gets the same actionable
// message.
func CurrentOSVersion() OSVersion {
	var maj, mn, pat C.long
	C.permissions_os_version(&maj, &mn, &pat)
	return OSVersion{
		Major: int(maj),
		Minor: int(mn),
		Patch: int(pat),
	}
}

// CurrentArch returns runtime.GOARCH. Exists as a separate function so
// CheckPlatform can be tested with arbitrary arch strings (the production
// callsite in cmd/dndmode/main.go threads CurrentArch() through). This is
// the same DI-via-injection seam pattern used by Phase 2 cocoa controller.
func CurrentArch() string {
	return runtime.GOARCH
}
