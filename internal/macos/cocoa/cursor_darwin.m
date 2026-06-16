// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CGDirectDisplay.h>  // CGDisplayHideCursor/ShowCursor + CGMainDisplayID live here

// cocoa_hide_cursor hides the system mouse cursor for the duration of the
// active overlay. This is the in-process cosmetic cursor hide: while the
// black shield NSWindows are up the WindowServer keeps drawing the arrow on
// top of them, and because the Phase 4 CGEventTap suppresses mouse-moved
// events the user otherwise sees a frozen-but-visible arrow on pure black.
//
// Decided mechanism (brainstorm, FINALIZED): CoreGraphics display-level hide
// via CGDisplayHideCursor(CGMainDisplayID()), NOT [NSCursor hide]. AppKit
// auto-reshows the cursor on app-switch, and the shield windows are
// setIgnoresMouseEvents:YES so cursor-rects never fire — neither path is
// reliable here. CGDisplayHideCursor operates at the WindowServer-connection
// level, which dndmode holds live via its shield NSWindows.
//
// No crash-safety cleanup is needed: WindowServer auto-restores the cursor
// when the process dies (same kernel auto-cleanup rationale as the
// IOPMAssertion power assertion). The matching cocoa_show_cursor below is the
// graceful-teardown path.
void cocoa_hide_cursor(void) {
    CGDisplayHideCursor(CGMainDisplayID());
}

// cocoa_show_cursor restores the system mouse cursor on overlay teardown,
// balancing a prior cocoa_hide_cursor. Safe to call even if the cursor is
// already visible (CGDisplayShowCursor is reference-counted by the
// WindowServer; the Controller's cursorHidden guard ensures we only call this
// once per hide).
void cocoa_show_cursor(void) {
    CGDisplayShowCursor(CGMainDisplayID());
}
