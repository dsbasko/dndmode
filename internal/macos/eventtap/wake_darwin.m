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

// g_observed_tap is DEFINED in watchdog_darwin.m (single binary-wide
// definition; this TU sees it via `extern`). Both the watchdog GCD block
// and the NSWorkspace wake / session-active blocks read this same global
// so Release Step 1 (eventtap_set_observed_tap(NULL)) closes the race
// window for BOTH subsystems in a single atomic write. See the docstring
// above the definition in watchdog_darwin.m for full rationale.
//
// `volatile` is mandatory here too: this file's blocks run on
// `[NSOperationQueue mainQueue]` while the writer runs on the goroutine
// that drives Release (typically also main, but cross-thread invariants
// are defended unconditionally).
extern volatile CFMachPortRef g_observed_tap;

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

    // fix: do NOT seed g_observed_tap here yet. The previous code
    // wrote `g_observed_tap = tap` BEFORE addObserverForName, so if
    // addObserverForName failed (rc=2 "already installed" short-circuit
    // OR a future addition of a fallible second observer) the global
    // would remain seeded with a tap pointer that no observer references.
    // The seed now lives at the BOTTOM of the function — after both
    // observers have non-nil tokens — so failure paths leave the global
    // pristine for the caller's rollback chain to keep its invariants.
    NSNotificationCenter *nc = [[NSWorkspace sharedWorkspace] notificationCenter];

    g_wake_token = [nc addObserverForName:NSWorkspaceDidWakeNotification
                                   object:nil
                                    queue:[NSOperationQueue mainQueue]
                               usingBlock:^(NSNotification *n) {
        (void)n;
        // guard: snapshot g_observed_tap BEFORE
        // any CGEventTapEnable call. Between Release Step 1 (NULL write)
        // and Step 5 (wake_observer_remove), this block may still fire
        // from a pending main-queue dispatch. Snapshot pattern ensures
        // either we no-op safely (snap == NULL) or we hold a local that
        // remained valid throughout — same rationale as the watchdog
        // handler in watchdog_darwin.m.
        CFMachPortRef tap_snap = g_observed_tap;
        if (tap_snap == NULL) {
            return;
        }
        CGEventTapEnable(tap_snap, true);
    }];

    g_session_token = [nc addObserverForName:NSWorkspaceSessionDidBecomeActiveNotification
                                      object:nil
                                       queue:[NSOperationQueue mainQueue]
                                  usingBlock:^(NSNotification *n) {
        (void)n;
        // Same guard as the DidWake block above — identical
        // rationale, identical pattern. Kept duplicated rather than
        // factored into a helper because Objective-C blocks capturing
        // function pointers across notification names obscure stack
        // traces, and a 4-line snapshot pattern is its own documentation.
        CFMachPortRef tap_snap = g_observed_tap;
        if (tap_snap == NULL) {
            return;
        }
        CGEventTapEnable(tap_snap, true);
    }];

    // fix: seed the shared global ONLY after both observer
    // registrations have returned non-nil tokens. For an InstallAll
    // call site this is also re-set explicitly after wake_observer_install
    // returns — idempotent re-write. For a wake-observer-only caller
    // (none in production v1.0, but the API stays callable in isolation
    // for smoke tests / future refactors) this line is the sole writer.
    g_observed_tap = tap;

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
    // Defence-in-depth: Release Step 1 already wrote NULL via
    // eventtap_set_observed_tap (BEFORE Step 5 reaches here), so this
    // assignment is a redundant re-NULL — kept so the wake-observer
    // teardown is self-contained for any caller exercising it in
    // isolation (smoke tests; unit tests).
    g_observed_tap = NULL;
}
