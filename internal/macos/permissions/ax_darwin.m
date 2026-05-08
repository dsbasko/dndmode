// +build darwin

#import <ApplicationServices/ApplicationServices.h>
#import <CoreFoundation/CoreFoundation.h>

// permissions_ax_is_trusted returns 1 if our process is in the Accessibility
// TCC list, 0 otherwise. Silent variant: no UI side effect.
//
// Backed by AXIsProcessTrusted() from ApplicationServices. Thread-safe;
// callable from any pthread. No CoreFoundation lifecycle to manage here
// (the API takes no parameters).
//
// (silent variant). Wired to Go via permissions.IsAccessibilityTrusted.
int permissions_ax_is_trusted(void) {
    return AXIsProcessTrusted() ? 1 : 0;
}

// permissions_ax_prompt returns 1 if our process is in the Accessibility
// TCC list, 0 otherwise — same return semantics as permissions_ax_is_trusted
// but with the side effect of triggering the system prompt dialog
// ("X would like to control this computer using accessibility features")
// when the process is not yet trusted.
//
// Implementation: AXIsProcessTrustedWithOptions takes a CFDictionary with
// kAXTrustedCheckOptionPrompt -> kCFBooleanTrue to enable the prompt. We
// allocate the dictionary via CFDictionaryCreate, pass it in, then release
// it (CFDictionary lifecycle: create-side owns release per Core Foundation
// memory rules; the system documents that AXIsProcessTrustedWithOptions does
// NOT retain the dictionary beyond the call).
//
// macOS dedupes the prompt by (cdhash, user) — re-invocation within the
// same process (or between processes sharing a cdhash) does NOT re-prompt.
// contract: caller invokes exactly once per process at polling entry.
//
// (prompt variant). Wired to Go via permissions.PromptAccessibility.
int permissions_ax_prompt(void) {
    const void *keys[] = { kAXTrustedCheckOptionPrompt };
    const void *vals[] = { kCFBooleanTrue };
    CFDictionaryRef opts = CFDictionaryCreate(
        NULL,
        keys, vals, 1,
        &kCFTypeDictionaryKeyCallBacks,
        &kCFTypeDictionaryValueCallBacks);
    int res = AXIsProcessTrustedWithOptions(opts) ? 1 : 0;
    CFRelease(opts);
    return res;
}
