// pm_darwin.c — pure C bridge to IOKit IOPMAssertion + IOPMCopyAssertionsByProcess.
//
// No `+build darwin` comment needed: this file is compiled only when cgo
// includes it via the `import "C"` directive in pm_darwin.go, which is
// itself guarded by `//go:build darwin`.
//
// The file deliberately avoids the Obj-C runtime (no `@autoreleasepool`,
// no NSString) — keeping it pure C lets the compiler skip the Obj-C
// codegen pass and minimizes the LDFLAGS surface to just IOKit +
// CoreFoundation.
//
// Frameworks linked: see #cgo LDFLAGS in pm_darwin.go.

#include <IOKit/pwr_mgt/IOPMLib.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdint.h>
#include <stdlib.h>

// powerassert_acquire creates an IOPMAssertion whose type is selected at
// runtime from allow_display_sleep (POW-01 + POW-02, inverted polarity):
//
//   - allow_display_sleep == 0 (default): kIOPMAssertPreventUserIdleDisplaySleep
//     — the display is kept awake (and the system stays awake as a
//     consequence, since a forced-on display also blocks system idle-sleep).
//     This is the new default so the external monitor does NOT turn off.
//   - allow_display_sleep != 0: kIOPMAssertPreventUserIdleSystemSleep —
//     legacy behavior; only system idle-sleep is blocked, the display is
//     allowed to idle-off.
//
// Both kIOPMAssert* identifiers are CFStringRef constants exported by
// IOPMLib.h — they are NOT owned by us, so no CFRelease is performed on
// assert_type.
//
// On success: returns kIOReturnSuccess (0) and writes the new
// assertion ID through *out_id.
// On failure: returns the IOReturn error from
// IOPMAssertionCreateWithName (e.g. kIOReturnNoMemory if the CFString
// allocation fails).
IOReturn powerassert_acquire(const char *name, int allow_display_sleep, IOPMAssertionID *out_id) {
    CFStringRef cf_name = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
    if (cf_name == NULL) {
        return kIOReturnNoMemory;
    }
    // POW-01 + POW-02: default (allow_display_sleep == 0) keeps the display
    // awake via PreventUserIdleDisplaySleep; allow_display_sleep != 0 selects
    // the legacy PreventUserIdleSystemSleep (display may idle-sleep).
    CFStringRef assert_type = allow_display_sleep
        ? kIOPMAssertPreventUserIdleSystemSleep
        : kIOPMAssertPreventUserIdleDisplaySleep;
    IOReturn rc = IOPMAssertionCreateWithName(
        assert_type,
        kIOPMAssertionLevelOn,
        cf_name,
        out_id);
    CFRelease(cf_name);
    return rc;
}

// powerassert_release releases an IOPMAssertion previously created via
// powerassert_acquire. Thin pass-through; idempotency is enforced by
// the Go-side Assertion struct (atomic.Bool + sync.Once).
IOReturn powerassert_release(IOPMAssertionID id) {
    return IOPMAssertionRelease(id);
}

// pa_dict_has_match checks whether a single per-assertion CFDictionary
// matches the requested name (+ optionally type). Helper used by both
// powerassert_count_by_name and powerassert_enumerate_matching to avoid
// duplicating the kIOPMAssertionNameKey + kIOPMAssertionTypeKey
// extraction logic.
//
// Contract: the name comparison is ALWAYS enforced (name is the unique
// discriminator). The type comparison is enforced ONLY when cf_want_type
// is non-NULL AND has length > 0; an empty/NULL want_type means "match any
// type". This matters because the assertion type is now runtime-selected
// (PreventUserIdleDisplaySleep by default vs PreventUserIdleSystemSleep for
// allow_display_sleep:true), so orphan cleanup must match by name alone and
// accept either type. countOwnByName (smoke) keeps passing a concrete type,
// so its non-empty path is unchanged.
static int pa_dict_has_match(
    CFDictionaryRef d,
    CFStringRef cf_want_name,
    CFStringRef cf_want_type)
{
    CFStringRef name = (CFStringRef)CFDictionaryGetValue(d, kIOPMAssertionNameKey);
    CFStringRef type = (CFStringRef)CFDictionaryGetValue(d, kIOPMAssertionTypeKey);
    if (name == NULL || type == NULL) return 0;
    if (CFStringCompare(name, cf_want_name, 0) != kCFCompareEqualTo) return 0;
    int want_type_len = (cf_want_type != NULL) ? (int)CFStringGetLength(cf_want_type) : 0;
    if (want_type_len > 0 && CFStringCompare(type, cf_want_type, 0) != kCFCompareEqualTo) return 0;
    return 1;
}

// pa_extract_assertion_id pulls the per-assertion ID out of the inner
// CFDictionary. Apple does NOT define a CFSTR macro for this key in
// the public IOPMLib.h header (only kIOPMAssertionNameKey / TypeKey /
// LevelKey are exposed).
//
// Empirical observation on macOS 14/15 via CFShow of a live
// IOPMCopyAssertionsByProcess result shows the actual key is
// "AssertionId" (mixed case — capital A, lowercase d). The 03-RESEARCH
// Pattern 8 [ASSUMED] note proposed "AssertID" / "AssertionID" — both
// turn out to be wrong on current macOS. We try "AssertionId" first
// (verified) then fall back to the legacy forms in case a future macOS
// reverts the key name. Returns 0 if no key yields a CFNumber
// convertible to int32 (caller skips that entry).
static int pa_extract_assertion_id(CFDictionaryRef d, uint32_t *out_id) {
    CFNumberRef aid_num = (CFNumberRef)CFDictionaryGetValue(d, CFSTR("AssertionId"));
    if (aid_num == NULL) {
        aid_num = (CFNumberRef)CFDictionaryGetValue(d, CFSTR("AssertID"));
    }
    if (aid_num == NULL) {
        aid_num = (CFNumberRef)CFDictionaryGetValue(d, CFSTR("AssertionID"));
    }
    if (aid_num == NULL) return 0;
    int32_t aid = 0;
    if (!CFNumberGetValue(aid_num, kCFNumberSInt32Type, &aid)) return 0;
    *out_id = (uint32_t)aid;
    return 1;
}

// powerassert_count_by_name returns the number of assertions held by the
// given own_pid whose name+type match want_name+want_type.
//
// Used by the smoke test (D-14) to verify the production code path:
// pre-acquire count == 0; post-acquire count == 1; post-release count == 0.
//
// Semantics: this function INCLUDES own_pid (the caller wants to count
// assertions IT holds, not orphans from other PIDs). For orphan
// enumeration use powerassert_enumerate_matching.
//
// Returns 0 on any IOKit error or empty result (D-12: "treat NULL as
// success, zero orphans"). The Go side cannot distinguish "0 matches" from
// "IOKit transient failure"; that distinction is irrelevant for the smoke
// roundtrip use case.
int powerassert_count_by_name(const char *want_name, const char *want_type, int own_pid) {
    CFDictionaryRef root = NULL;
    IOReturn rc = IOPMCopyAssertionsByProcess(&root);
    if (rc != kIOReturnSuccess || root == NULL) {
        if (root != NULL) CFRelease(root);
        return 0;
    }

    CFStringRef cf_want_name = CFStringCreateWithCString(NULL, want_name, kCFStringEncodingUTF8);
    CFStringRef cf_want_type = CFStringCreateWithCString(NULL, want_type, kCFStringEncodingUTF8);
    if (cf_want_name == NULL || cf_want_type == NULL) {
        if (cf_want_name != NULL) CFRelease(cf_want_name);
        if (cf_want_type != NULL) CFRelease(cf_want_type);
        CFRelease(root);
        return 0;
    }

    int count = 0;
    CFIndex n_pids = CFDictionaryGetCount(root);
    if (n_pids > 0) {
        const void **keys = (const void **)malloc((size_t)n_pids * sizeof(void *));
        const void **vals = (const void **)malloc((size_t)n_pids * sizeof(void *));
        if (keys != NULL && vals != NULL) {
            CFDictionaryGetKeysAndValues(root, keys, vals);

            for (CFIndex i = 0; i < n_pids; i++) {
                CFNumberRef pid_num = (CFNumberRef)keys[i];
                int pid = 0;
                if (!CFNumberGetValue(pid_num, kCFNumberIntType, &pid)) continue;
                if (pid != own_pid) continue;  // own-PID counter

                CFArrayRef arr = (CFArrayRef)vals[i];
                if (arr == NULL) continue;
                CFIndex n = CFArrayGetCount(arr);
                for (CFIndex j = 0; j < n; j++) {
                    CFDictionaryRef d = (CFDictionaryRef)CFArrayGetValueAtIndex(arr, j);
                    if (d == NULL) continue;
                    if (pa_dict_has_match(d, cf_want_name, cf_want_type)) {
                        count++;
                    }
                }
            }
        }
        if (keys != NULL) free(keys);
        if (vals != NULL) free(vals);
    }

    CFRelease(cf_want_name);
    CFRelease(cf_want_type);
    CFRelease(root);
    return count;
}

// powerassert_enumerate_matching scans IOPMCopyAssertionsByProcess for
// assertions whose name+type match want_name+want_type AND whose owner
// PID is NOT own_pid. Writes up to `cap` (pid, assertion_id) tuples
// into the caller-allocated parallel arrays out_pids[] and out_ids[].
//
// Returns the number written (clipped at cap). 0 means no orphans
// (D-12 "treat NULL as success, zero orphans"). Negative return values
// are NOT produced by this function — the Go side still handles n < 0
// defensively in case future evolution adds error paths.
//
// Used by plan 03-05 orphan cleanup (production path) and by the POW-04
// smoke test in this plan (subprocess-fork orphan release roundtrip).
int powerassert_enumerate_matching(
    const char *want_name, const char *want_type,
    int own_pid,
    int *out_pids, uint32_t *out_ids, int cap)
{
    if (cap <= 0 || out_pids == NULL || out_ids == NULL) return 0;

    CFDictionaryRef root = NULL;
    IOReturn rc = IOPMCopyAssertionsByProcess(&root);
    if (rc != kIOReturnSuccess || root == NULL) {
        if (root != NULL) CFRelease(root);
        return 0;
    }

    CFStringRef cf_want_name = CFStringCreateWithCString(NULL, want_name, kCFStringEncodingUTF8);
    CFStringRef cf_want_type = CFStringCreateWithCString(NULL, want_type, kCFStringEncodingUTF8);
    if (cf_want_name == NULL || cf_want_type == NULL) {
        if (cf_want_name != NULL) CFRelease(cf_want_name);
        if (cf_want_type != NULL) CFRelease(cf_want_type);
        CFRelease(root);
        return 0;
    }

    int written = 0;
    CFIndex n_pids = CFDictionaryGetCount(root);
    if (n_pids > 0) {
        const void **keys = (const void **)malloc((size_t)n_pids * sizeof(void *));
        const void **vals = (const void **)malloc((size_t)n_pids * sizeof(void *));
        if (keys != NULL && vals != NULL) {
            CFDictionaryGetKeysAndValues(root, keys, vals);

            for (CFIndex i = 0; i < n_pids && written < cap; i++) {
                CFNumberRef pid_num = (CFNumberRef)keys[i];
                int pid = 0;
                if (!CFNumberGetValue(pid_num, kCFNumberIntType, &pid)) continue;
                if (pid == own_pid) continue;  // exclude ourselves (orphan scan)

                CFArrayRef arr = (CFArrayRef)vals[i];
                if (arr == NULL) continue;
                CFIndex n = CFArrayGetCount(arr);
                for (CFIndex j = 0; j < n && written < cap; j++) {
                    CFDictionaryRef d = (CFDictionaryRef)CFArrayGetValueAtIndex(arr, j);
                    if (d == NULL) continue;
                    if (!pa_dict_has_match(d, cf_want_name, cf_want_type)) continue;

                    uint32_t aid = 0;
                    if (!pa_extract_assertion_id(d, &aid)) continue;

                    out_pids[written] = pid;
                    out_ids[written]  = aid;
                    written++;
                }
            }
        }
        if (keys != NULL) free(keys);
        if (vals != NULL) free(vals);
    }

    CFRelease(cf_want_name);
    CFRelease(cf_want_type);
    CFRelease(root);
    return written;
}
