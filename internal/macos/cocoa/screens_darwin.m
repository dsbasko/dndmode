// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CGDisplayConfiguration.h>
#import <stdint.h>
#import <stddef.h>
#import "_cgo_export.h"  // generated header for //export-ed Go functions

// displayReconfigCallback fires on a PRIVATE / WindowServer thread
// (NOT main). It MUST NOT call into AppKit directly. The only safe action
// is to dispatch_async onto the main queue, which will invoke our Go //export
// callback from the main thread (unified path).
//
// We pass NULL userInfo to CGDisplayRegisterReconfigurationCallback because
// our Go callback already has access to the active controller via
// atomic.Pointer (see screens_darwin.go).
static void displayReconfigCallback(CGDirectDisplayID display,
                                    CGDisplayChangeSummaryFlags flags,
                                    void *userInfo) {
    dispatch_async(dispatch_get_main_queue(), ^{
        goCocoaOnScreensChanged();
    });
}

// screenObserverToken holds the opaque token returned by addObserverForName
// so cocoa_screens_unregister_observers can call removeObserver: on it.
// Static because there is exactly one active observer at any time (
// idempotent Init via sync.Once on the Go side guarantees this).
static id screenObserverToken = nil;

// Test-only instrumentation counters for dual-observer verification.
// Incremented on each successful register / decremented (well — set to
// register_count for unregister) call. Exposed to Go via
// cocoa_test_get_screen_register_count + cocoa_test_get_cg_register_count
// so app_test.go can assert that cocoa.Init() registered BOTH observers
// exactly once (and that controller.Release symmetrically unregistered).
//
// These counters are intentionally never reset between registrations within
// the same process — callers (tests) snapshot before+after and assert the
// delta. They are NOT thread-safe in the strict sense, but all
// register/unregister calls happen on the main thread per.
static int cocoa_test_screen_register_count = 0;
static int cocoa_test_cg_register_count = 0;

int cocoa_test_get_screen_register_count(void) {
    return cocoa_test_screen_register_count;
}

int cocoa_test_get_cg_register_count(void) {
    return cocoa_test_cg_register_count;
}

// cocoa_screens_register_observers registers BOTH:
//   1. CGDisplay reconfiguration callback (low-level; fires on private thread).
//   2. NSApplicationDidChangeScreenParameters notification (AppKit; main queue).
// Both routes converge to goCocoaOnScreensChanged via dispatch_async on the
// main queue. Returns 0 on success, the CGError code on failure (caller can
// log; we do not retry — Init is single-shot per process).
//
// Why both: NSApplicationDidChangeScreenParameters can miss
// fullscreen reconfigs; CGDisplay callback can fire before [NSScreen screens]
// is updated. Dual subscription with main-queue dedup at the Go side
// (debouncer) covers the union of all hot-plug scenarios.
int cocoa_screens_register_observers(void) {
    CGError err = CGDisplayRegisterReconfigurationCallback(
        displayReconfigCallback, NULL);
    if (err != kCGErrorSuccess) {
        return (int)err;
    }
    cocoa_test_cg_register_count++;
    screenObserverToken = [[NSNotificationCenter defaultCenter]
        addObserverForName:NSApplicationDidChangeScreenParametersNotification
                    object:nil
                     queue:[NSOperationQueue mainQueue]
                usingBlock:^(NSNotification *n) {
        // Already on main (queue:[NSOperationQueue mainQueue]) — but route
        // through dispatch_async for uniform-path semantics. The cost
        // is one cheap dispatch hop; the benefit is single Go entry point.
        dispatch_async(dispatch_get_main_queue(), ^{
            goCocoaOnScreensChanged();
        });
    }];
    cocoa_test_screen_register_count++;
    return 0;
}

// cocoa_screens_unregister_observers symmetrically tears down both observers.
// Must be called from controller.Release via DispatchMain (which on the
// main goroutine inlines fast-path) before closing windows, to prevent a
// late reconfig event from triggering reconcile on half-released state
// (the design notes).
int cocoa_screens_unregister_observers(void) {
    CGError err = CGDisplayRemoveReconfigurationCallback(
        displayReconfigCallback, NULL);
    if (screenObserverToken) {
        [[NSNotificationCenter defaultCenter] removeObserver:screenObserverToken];
        screenObserverToken = nil;
    }
    return (int)err;
}

// cocoa_enumerate_screens returns the count of NSScreen instances and writes
// up to maxIDs CGDirectDisplayIDs (extracted from
// NSScreen.deviceDescription[NSScreenNumber]) into outIDs. mandates
// identity by displayID, not NSScreen pointer.
//
// MUST be called from main thread ([NSScreen screens] returns autoreleased
// NSArray; ARC + main-thread invariant keeps it safe).
size_t cocoa_enumerate_screens(uint32_t* outIDs, size_t maxIDs) {
    NSArray<NSScreen*> *screens = [NSScreen screens];
    NSUInteger n = [screens count];
    if (n == 0) {
        return 0;
    }
    NSUInteger limit = (n < maxIDs) ? n : maxIDs;
    for (NSUInteger i = 0; i < limit; i++) {
        NSScreen *s = [screens objectAtIndex:i];
        NSNumber *idNum = [[s deviceDescription]
            objectForKey:@"NSScreenNumber"];
        outIDs[i] = idNum ? [idNum unsignedIntValue] : 0;
    }
    return n;
}
