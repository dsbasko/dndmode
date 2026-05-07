//go:build darwin

package permissions

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation

#include <stdint.h>

extern int permissions_ax_is_trusted(void);
extern int permissions_ax_prompt(void);
*/
import "C"

// IsAccessibilityTrusted returns true if our process is in the Accessibility
// TCC list (i.e. AXIsProcessTrusted() returns YES).
//
// SAFE from any goroutine; AX TCC reads are thread-safe per Apple's
// "Threading Programming Guide" (ApplicationServices framework is documented
// thread-safe for read-only trust queries).
//
// (silent variant). Used by the polling-loop cycle in
// permissions.WaitForGrants to test whether the user has granted Accessibility
// since the last cycle. Does NOT show any UI — for the one-shot system
// prompt dialog, use PromptAccessibility instead.
func IsAccessibilityTrusted() bool {
	return C.permissions_ax_is_trusted() != 0
}

// PromptAccessibility triggers the system Accessibility prompt dialog if our
// process is not yet trusted. Returns the same trust value as
// IsAccessibilityTrusted (true iff already trusted at call time).
//
// CONTRACT: caller invokes EXACTLY ONCE per process at polling-loop
// entry. macOS dedupes the prompt by (cdhash, user) — re-calling within
// the same process will NOT re-display the dialog; the second call is a
// silent no-op aside from returning the current trust state. Still, prefer
// to call once and use IsAccessibilityTrusted for subsequent polling cycles.
//
// SAFE from any goroutine.
//
// (prompt variant). Implementation routes through
// AXIsProcessTrustedWithOptions({kAXTrustedCheckOptionPrompt: kCFBooleanTrue}).
func PromptAccessibility() bool {
	return C.permissions_ax_prompt() != 0
}
