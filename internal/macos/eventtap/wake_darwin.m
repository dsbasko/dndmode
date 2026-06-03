// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>

// g_wake_token holds the opaque observer token returned by
// `[NSNotificationCenter addObserverForName:NSWorkspaceDidWakeNotification
//                                  object:nil queue:nil usingBlock:^{...}]`.
// Wave 1 04-04 implements the subscription and stores the token here so
// `wake_observer_remove` can call `[NSNotificationCenter removeObserver:]`
// symmetrically. `id` (Objective-C object) is automatically retained by ARC
// when assigned to a static variable.
static id g_wake_token = nil;

// g_session_token holds the observer token for
// `NSWorkspaceSessionDidBecomeActiveNotification` (fast user switching back
// to our session). Both DidWake and SessionDidBecomeActive trigger the same
// re-arm path because either may invalidate the tap (the design notes: sleep
// disables CGEventTap unconditionally; fast-user-switch only disables if
// SecureEventInput was held by the other user).
static id g_session_token = nil;

// g_observed_tap caches the CFMachPortRef pointer so the observer block
// can call `CGEventTapEnable(g_observed_tap, true)` without an extra
// indirection through Go. The pointer is owned by the install path
// (`tap_darwin.m` g_tap) — the wake module borrows it for the lifetime of
// the observer subscription. NULL-check in the block guards against
// out-of-order teardown where the tap is released before the observer.
static CFMachPortRef g_observed_tap = NULL;

// wake_observer_install subscribes to NSWorkspace wake + session-active
// notifications and stores the tokens. Wave 1 04-04 implements:
//
//   1. Cache `tap` into `g_observed_tap`.
//   2. `g_wake_token = [[NSWorkspace sharedWorkspace] addObserverForName:
//        NSWorkspaceDidWakeNotification object:nil queue:nil
//        usingBlock:^(NSNotification * _Nonnull n) { if (g_observed_tap)
//        CGEventTapEnable(g_observed_tap, true); }]`.
//   3. Same for `NSWorkspaceSessionDidBecomeActiveNotification` into
//      `g_session_token`.
//
// Returns 0 on success. Failure modes are essentially "addObserverForName
// crashes the process via AppKit assertion" — there is no recoverable error
// path here in practice.
//
// Wave 0 is a no-op.
int wake_observer_install(CFMachPortRef tap) {
    (void)tap;
    return 0;
}

// wake_observer_remove unsubscribes both observers and zeroes the globals.
// Wave 1 04-04 implements:
//
//   1. `if (g_wake_token) [[[NSWorkspace sharedWorkspace] notificationCenter]
//      removeObserver:g_wake_token]; g_wake_token = nil;`.
//   2. Same for `g_session_token`.
//   3. `g_observed_tap = NULL`.
//
// MUST be called from main thread because `[NSWorkspace sharedWorkspace]
// notificationCenter]` is documented main-thread-only. The Go side
// (`Releaser.Release`) routes through `DispatchMain` to guarantee this.
//
// Wave 0 is a no-op.
void wake_observer_remove(void) {
}
