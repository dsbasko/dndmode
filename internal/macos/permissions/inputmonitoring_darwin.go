//go:build darwin

package permissions

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework IOKit

#include <stdint.h>

extern int permissions_im_check_listen(void);
*/
import "C"

// IMAccess models the IOHIDAccessType enum returned by
// IOHIDCheckAccess(kIOHIDRequestTypeListenEvent). The numeric values are
// pinned to IOKit's underlying kIOHIDAccessType* constants:
//
//	kIOHIDAccessTypeGranted = 0
//	kIOHIDAccessTypeDenied  = 1
//	kIOHIDAccessTypeUnknown = 2
//
// Pure-Go consumers can switch-case on Denied vs Unknown if a future
// polling-UX change needs to surface the difference (e.g. "permission
// refused" vs "permission state not yet decided by the user").
type IMAccess int

const (
	// IMAccessGranted == kIOHIDAccessTypeGranted == 0. The only value that
	// IMAccess.IsGranted() reports truthy.
	IMAccessGranted IMAccess = 0
	// IMAccessDenied == kIOHIDAccessTypeDenied == 1.
	IMAccessDenied IMAccess = 1
	// IMAccessUnknown == kIOHIDAccessTypeUnknown == 2.
	IMAccessUnknown IMAccess = 2
)

// IsGranted reports true iff the access state is IMAccessGranted. Both
// Denied and Unknown — and any out-of-range defensive value — read false.
func (a IMAccess) IsGranted() bool {
	return a == IMAccessGranted
}

// CheckInputMonitoring returns the IOHIDCheckAccess result for the
// "listen" request type (`kIOHIDRequestTypeListenEvent`). The call is
// SILENT: it does NOT trigger the TCC prompt — per / the only TCC
// entry-point in dndmode is the Accessibility prompt (CGEventTap suppression
// requires AX, not IM; IM is sufficient for listen-only). For IM the
// production flow opens the Settings deep-link instead.
//
// NEVER replace this with IOHIDRequestAccess — that is the explicit
// antipattern: it would trigger an IM prompt that we cannot
// dismiss programmatically, blocking polling, and IM is not even required
// for our CGEventTap-default flow.
//
//. SAFE from any goroutine.
func CheckInputMonitoring() IMAccess {
	return IMAccess(C.permissions_im_check_listen())
}
