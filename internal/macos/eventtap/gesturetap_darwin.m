//go:build darwin
// +build darwin

#import <CoreFoundation/CoreFoundation.h>
#import <CoreGraphics/CoreGraphics.h>

// Session-level trackpad-gesture suppression tap — the SECOND tap of the
// package, complementing the HID-level tap in tap_darwin.m.
//
// WHY A SECOND TAP: the primary tap sits at kCGHIDEventTap and blocks every
// keyboard / mouse / scroll / media event — but trackpad multitouch gestures
// never appear there as suppressible CGEvents. WindowServer synthesizes them
// from the raw multitouch stream and delivers them to interested processes
// (most importantly the Dock, which owns Mission Control / App Exposé /
// Spaces / Launchpad activation) as session-level events of two PRIVATE CGS
// types:
//
//   kCGSEventGesture     = 29 — gesture envelope (pinch, rotate, …;
//                               IOHIDEventType in private field 110)
//   kCGSEventDockControl = 30 — dock swipes (3/4-finger Mission Control,
//                               App Exposé, Space switching; field 110 == 23
//                               = kIOHIDEventTypeDockSwipe)
//
// A kCGSessionEventTap masked on these two types sees the events BEFORE the
// Dock does; returning NULL from a kCGEventTapOptionDefault callback
// suppresses them. Verified in production by joshuarli/iss (intercepts and
// re-posts dock swipes exactly this way); the type numbering has been stable
// since macOS 10.11.
//
// Anti-pattern 7 ("never kCGSessionEventTap") does NOT apply here: that rule
// exists because system KEYBOARD shortcuts (Cmd+Tab, Spotlight, …) are
// consumed before a session tap — keyboard blocking therefore lives on the
// HID tap and stays there. Dock gestures are the mirror image: they exist
// ONLY at session level, so this is the one place they can be suppressed.
// Without this tap a 3/4-finger swipe opens Mission Control over the shield
// (window thumbnails + Spaces bar leak — the T-04-leak scenario).

enum {
    kCGSEventGesture     = 29,
    kCGSEventDockControl = 30,
};

// Per-process gesture-tap state; same single-install contract as the main
// tap statics in tap_darwin.m (exactly one gesture tap per dndmode process).
// g_gesture_runloop is the main tap's WORKER run loop (passed into
// gesturetap_install_c) — both tap sources are serviced by the same locked
// OS thread, so the gesture tap needs no thread of its own. Not volatile for
// the same reason g_tap isn't: the disable-first Release ordering stops
// callbacks before teardown, and cross-thread re-enable callers are gated by
// the volatile g_observed_tap guard that precedes them (see
// gesturetap_reenable).
static CFMachPortRef      g_gesture_tap     = NULL;
static CFRunLoopSourceRef g_gesture_source  = NULL;
static CFRunLoopRef       g_gesture_runloop = NULL;

// gesturetap_callback: self-heal on the two disable meta-events (same policy
// as eventtap_callback), otherwise swallow EVERYTHING. No hotkey matching —
// the unlock hotkey is a keyboard event and lives on the HID tap. Same
// nosplit-style constraints as the main callback: no Go calls, no
// allocation, no logging (silent-on-input security stance).
static CGEventRef gesturetap_callback(CGEventTapProxy proxy,
                                      CGEventType type,
                                      CGEventRef event,
                                      void *userInfo) {
    (void)proxy;
    (void)userInfo;

    if (type == kCGEventTapDisabledByTimeout ||
        type == kCGEventTapDisabledByUserInput) {
        if (g_gesture_tap != NULL) {
            CGEventTapEnable(g_gesture_tap, true);
        }
        return event;
    }

    // Unconditional suppression: any gesture / dock-control event that
    // reaches the Dock or an application while dndmode is active is an
    // input leak. No per-subtype filtering on purpose — stricter and
    // simpler than selective re-posting.
    return NULL;
}

// gesturetap_install_c creates + enables the session-level gesture tap and
// attaches its source to `loop` — the main tap's worker run loop.
// CFRunLoopAddSource on a foreign RUNNING loop is legal (CFRunLoopRef is one
// of Core Foundation's documented thread-safe types); CFRunLoopWakeUp forces
// the blocked loop out of mach_msg so it notices the new source immediately.
//
// Return codes (disjoint from eventtap_install_c's 1/2 so a wrapped
// ErrTapInstallFailed rc is unambiguous in diagnostics):
//   3 — CGEventTapCreate returned NULL (same triggers as the main tap:
//       Accessibility revoked, SecureEventInput active, mach-port exhaustion)
//   4 — CFMachPortCreateRunLoopSource returned NULL
//   5 — loop is NULL (caller bug; nothing acquired)
int gesturetap_install_c(CFRunLoopRef loop) {
    if (loop == NULL) {
        return 5;
    }

    CGEventMask mask =
        CGEventMaskBit(kCGSEventGesture) |
        CGEventMaskBit(kCGSEventDockControl);

    g_gesture_tap = CGEventTapCreate(
        kCGSessionEventTap,          // gestures are session-synthesized — see header comment
        kCGHeadInsertEventTap,       // ahead of the Dock's consumption
        kCGEventTapOptionDefault,    // suppression-capable (NOT ListenOnly)
        mask,
        gesturetap_callback,
        NULL);
    if (g_gesture_tap == NULL) {
        return 3;
    }

    g_gesture_source = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, g_gesture_tap, 0);
    if (g_gesture_source == NULL) {
        CFRelease(g_gesture_tap);
        g_gesture_tap = NULL;
        return 4;
    }

    CFRunLoopAddSource(loop, g_gesture_source, kCFRunLoopCommonModes);
    CFRunLoopWakeUp(loop);
    g_gesture_runloop = loop;

    // CGEventTapCreate returns a DISABLED tap — enable explicitly, same
    // pitfall as the main tap (silent no-callback failure otherwise).
    CGEventTapEnable(g_gesture_tap, true);
    return 0;
}

// gesturetap_disable_c mirrors the main tap's Release Step 1 disable:
// gesture suppression stops immediately (trackpad gestures recover together
// with the keyboard) even if the CF teardown below were to fail. Idempotent,
// nil-safe.
void gesturetap_disable_c(void) {
    if (g_gesture_tap != NULL) {
        CGEventTapEnable(g_gesture_tap, false);
    }
}

// gesturetap_uninstall_c releases source + tap. ORDERING CONTRACT: MUST run
// BEFORE eventtap_uninstall_c — that call ends with CFRunLoopStop on the
// SHARED worker run loop; once the worker goroutine returns from
// CFRunLoopRun its locked OS thread is reaped and g_gesture_runloop
// dangles. Release() in tap_darwin.go pins this order (gestureUninstallFn
// before uninstallFn); TestReleaser_Release_DisableBeforeUninstall guards
// the full four-step sequence.
//
// Idempotent: nil-checks on every path — safe to call with nothing
// installed or after a prior call.
void gesturetap_uninstall_c(void) {
    if (g_gesture_runloop != NULL && g_gesture_source != NULL) {
        CFRunLoopRemoveSource(g_gesture_runloop, g_gesture_source, kCFRunLoopCommonModes);
    }
    if (g_gesture_source != NULL) {
        CFRelease(g_gesture_source);
        g_gesture_source = NULL;
    }
    if (g_gesture_tap != NULL) {
        CGEventTapEnable(g_gesture_tap, false);
        CFRelease(g_gesture_tap);
        g_gesture_tap = NULL;
    }
    g_gesture_runloop = NULL;
}

// gesturetap_reenable is the self-heal hook shared with the watchdog probe
// (watchdog_darwin.m) and the NSWorkspace wake / session-active observers
// (wake_darwin.m): wherever the main tap gets re-enabled after sleep or a
// silent disable, the gesture tap is re-enabled on the same cadence.
// Nil-safe no-op when the gesture tap is not installed or already torn
// down. Every caller runs its volatile g_observed_tap NULL-guard FIRST, so
// a Release in progress (NULL written at Step 1) also suppresses late
// re-enables of this tap. The gesture tap has no failure counter of its
// own: the shared silent-disable failure mode (ad-hoc re-sign TCC race)
// kills both taps together, and the main tap's counter is the exit signal.
void gesturetap_reenable(void) {
    if (g_gesture_tap != NULL) {
        CGEventTapEnable(g_gesture_tap, true);
    }
}
