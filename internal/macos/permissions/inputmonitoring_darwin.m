// +build darwin

#import <IOKit/hidsystem/IOHIDLib.h>

// permissions_im_check_listen returns the raw IOHIDAccessType value of
// IOHIDCheckAccess(kIOHIDRequestTypeListenEvent) cast to int:
//
//   kIOHIDAccessTypeGranted = 0
//   kIOHIDAccessTypeDenied  = 1
//   kIOHIDAccessTypeUnknown = 2
//
// The Go side wraps the int in permissions.IMAccess and exposes
// IsGranted() for the common case. No CoreFoundation lifecycle, no
// Objective-C objects — pure scalar bridge.
//
//. SILENT probe: does NOT trigger the TCC prompt dialog. To
// drive a user to grant IM, open the Settings deep-link
// `x-apple.systempreferences:com.apple.preference.security?Privacy_ListenEvent`
// from Go via os/exec (see future permissions.deeplink in).
//
// MUST NOT be replaced with the request-access variant — antipattern
//: it would trigger a TCC prompt we cannot programmatically
// dismiss and IM grants are obtained via the Settings deep-link path
// instead.
int permissions_im_check_listen(void) {
    return (int)IOHIDCheckAccess(kIOHIDRequestTypeListenEvent);
}
