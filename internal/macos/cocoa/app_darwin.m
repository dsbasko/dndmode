// +build darwin

#import <Cocoa/Cocoa.h>
#import <stdint.h>

// cocoa_init does the once-per-process AppKit setup that MUST happen on
// the main thread before any NSWindow is created.
//
// contract:
//   - [NSApplication sharedApplication] establishes the singleton NSApp.
// - setActivationPolicy:NSApplicationActivationPolicyProhibited
//     hides the dndmode process from Dock, Cmd+Tab, and menu bar — the
//     CLI is the only observable surface (PROJECT.md "silent protection").
//   - We DO NOT explicitly invoke the AppKit launch-finalisation method
//     (the one [NSApp run] calls implicitly on the first iteration of the
// run loop). Per, AppKit's own run loop entry handles it.
//   - Screen observer registration is done by the Go side immediately
//     after this returns, via cocoa_screens_register_observers (separate
//     C function in screens_darwin.m). Two-step setup keeps each .m file
//     focused on a single concern.
void cocoa_init(void) {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyProhibited];
}

// cocoa_run_app blocks on [NSApp run] until a stop event is processed.
// Returns:
//   0 on clean exit (whether ctx-driven or otherwise — Go side
//     differentiates via ctx.Err() check after RunApp returns).
// 1 on NSException caught from [NSApp run] (unexpected exit path).
//
// MUST be called from main thread; AppKit invariant + runtimepin/init().
int cocoa_run_app(void) {
    @try {
        [NSApp run];
        return 0;
    } @catch (NSException *e) {
        // Phase 2: unexpected exit → non-nil error in Go side.
        // We swallow the exception here (logging is the Go side's job)
        // and signal via return code 1.
        return 1;
    }
}

// cocoa_stop_app schedules NSApp.run to return.
//
// Implementation: [NSApp stop:nil] sets an internal flag checked by the run
// loop on the next event. But if no further event arrives — and Phase 4
// CGEventTap will literally swallow most events — the run loop is starved.
// The fix is to post a synthetic NSEvent of type NSEventTypeApplicationDefined
// with our reserved Phase 2 subtype (0xDED, see doc.go subtype reservation
// table). The synthetic event wakes the run loop, which then sees the
// stop flag and returns from [NSApp run].
//
// Thread-safety: BOTH [NSApp stop:] and [NSApp postEvent:atStart:] are
// documented thread-safe by Apple ("Threading Programming Guide" +
// NSApplication.h:352). This function is invoked from the ctx-watcher Go
// goroutine, which runs on an arbitrary OS thread chosen by the Go scheduler.
// Phase 2 the design notes confirms this is safe.
void cocoa_stop_app(int subtype) {
    [NSApp stop:nil];
    NSEvent *evt = [NSEvent
        otherEventWithType:NSEventTypeApplicationDefined
                  location:NSZeroPoint
             modifierFlags:0
                 timestamp:0
              windowNumber:0
                   context:nil
                   subtype:(short)subtype
                     data1:0
                     data2:0];
    [NSApp postEvent:evt atStart:YES];
}
