// +build darwin

#import <Cocoa/Cocoa.h>

// DVDView is the bouncing "DVD VIDEO" logo contentView used by the shield overlay
// window when config `overlay_style: dvd` is selected. A stylized DVD-VIDEO logo
// drifts diagonally across the screen, bounces off every edge, changes color from
// a neon palette on each bounce, and briefly flashes white when it lands exactly
// in a corner (the "holy grail" of old DVD-player screensavers). The full
// @implementation (geometry, physics, lifecycle, threading rationale) lives in
// dvdview_darwin.m.
//
// Like MatrixView / TerminalView, this is a purely cosmetic content swap on top of
// the opaque shield NSWindow (window_darwin.m): the window keeps setOpaque:YES,
// this view's backing layer is opaque black, so the desktop can never bleed
// through (T-gh8-03). One DVDView is installed per display, so each screen has its
// own independent logo bouncing on its own path.
//
// This header exists because cgo compiles each .m file as a SEPARATE clang
// translation unit, so window_darwin.m cannot see DVDView's @interface from
// dvdview_darwin.m via a bare `@class` forward decl — sending alloc /
// initWithFrame: / setAutoresizingMask: to a forward-declared class is a hard
// error in modern clang. Both .m files #import this header to share the real
// @interface.
@interface DVDView : NSView
@end
