// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>

// g_wake_token holds the opaque observer token returned by
// `[NSNotificationCenter addObserverForName:NSWorkspaceDidWakeNotification
//                                  object:nil queue:[NSOperationQueue mainQueue]
//                              usingBlock:^{...}]`.
// `id` (Objective-C object) is automatically retained by ARC when assigned
// to a static variable, so the block lives as long as the subscription does.
static id g_wake_token = nil;

// g_session_token holds the observer token for
// `NSWorkspaceSessionDidBecomeActiveNotification` (fast user switching back
// to our session). Both DidWake and SessionDidBecomeActive trigger the same
// re-arm path because either may invalidate the tap (the design notes: sleep
// disables CGEventTap unconditionally; fast-user-switch may silent-disable
// it depending on the other user's SecureEventInput state).
static id g_session_token = nil;

// g_observed_tap caches the CFMachPortRef pointer so the observer block
// can call `CGEventTapEnable(g_observed_tap, true)` without an extra
// indirection through Go. The pointer is owned by the install path
// (`tap_darwin.m` g_tap) — the wake module borrows it for the lifetime of
// the observer subscription. NULL-check inside the block guards against
// out-of-order teardown where the tap is released before the observer.
static CFMachPortRef g_observed_tap = NULL;

// wake_observer_install subscribes to NSWorkspace wake + session-active
// notifications via `[[NSWorkspace sharedWorkspace] notificationCenter]`
// (NOT the global Foundation notification center; NSWorkspace
// notifications are posted ONLY through NSWorkspace's own center).
//
// Both subscriptions deliver on `[NSOperationQueue mainQueue]` so the
// callback block runs on the main thread (the design notes threading note).
//
// Returns:
//   0 — both observers registered, tap pointer cached.
//   1 — tap is NULL (caller bug; nothing registered).
//   2 — already installed (caller must Remove first; nothing changed —
//       defensive guard against accidental double-install which would
//       leak the previous tokens since this static-globals contract
//       allows exactly one subscription at a time).
int wake_observer_install(CFMachPortRef tap) {
    if (tap == NULL) {
        return 1;
    }
    if (g_wake_token != nil || g_session_token != nil) {
        return 2;
    }

    g_observed_tap = tap;
    NSNotificationCenter *nc = [[NSWorkspace sharedWorkspace] notificationCenter];

    g_wake_token = [nc addObserverForName:NSWorkspaceDidWakeNotification
                                   object:nil
                                    queue:[NSOperationQueue mainQueue]
                               usingBlock:^(NSNotification *n) {
        (void)n;
        if (g_observed_tap != NULL) {
            CGEventTapEnable(g_observed_tap, true);
        }
    }];

    g_session_token = [nc addObserverForName:NSWorkspaceSessionDidBecomeActiveNotification
                                      object:nil
                                       queue:[NSOperationQueue mainQueue]
                                  usingBlock:^(NSNotification *n) {
        (void)n;
        if (g_observed_tap != NULL) {
            CGEventTapEnable(g_observed_tap, true);
        }
    }];

    return 0;
}

// wake_observer_remove unsubscribes both observers and zeroes the globals.
// Idempotent — safe to call when no observer has been installed (all guards
// short-circuit on nil/NULL).
//
// MUST be called from the main thread because `[NSWorkspace sharedWorkspace]
// notificationCenter]` is documented main-thread-only. The Go side
// (`Releaser.Release`) routes through `DispatchMain` to guarantee this.
//
// Under ARC, assigning `nil` to the static `id` drops the strong reference
// to the block-capturing observer object, allowing it to be deallocated.
void wake_observer_remove(void) {
    NSNotificationCenter *nc = [[NSWorkspace sharedWorkspace] notificationCenter];
    if (g_wake_token != nil) {
        [nc removeObserver:g_wake_token];
        g_wake_token = nil;
    }
    if (g_session_token != nil) {
        [nc removeObserver:g_session_token];
        g_session_token = nil;
    }
    // Guard against stale tap-pointer use after Releaser.Release teardown
    // chain unwinds the tap before the observer (D-08 step 5 ordering).
    g_observed_tap = NULL;
}
