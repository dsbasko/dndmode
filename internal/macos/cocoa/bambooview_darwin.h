// +build darwin

#import <Cocoa/Cocoa.h>

// BambooView is the night-walk contentView used by the shield overlay window
// when config `overlay_style: bamboo` is selected. The camera walks to the right
// through a dim bamboo forest at night, lit only by the moon: several parallax
// layers of pixel-art culms slide left, nearer ones bigger and faster, with mist
// between the layers, leaves drifting down, a few fireflies and a panda
// wandering far off.
// The full @implementation (forest and particle models, sprite art, layout,
// lifecycle, threading rationale) lives in bambooview_darwin.m.
//
// Like MatrixView / TerminalView / DVDView / GoldView, this is a purely cosmetic
// content swap on top of the opaque shield NSWindow (window_darwin.m): the window
// keeps setOpaque:YES, this view's backing layer is opaque black, so the desktop
// can never bleed through (T-gh8-03). One BambooView is installed per display,
// each seeded on its own, so every screen walks through its own forest.
//
// This header exists because cgo compiles each .m file as a SEPARATE clang
// translation unit, so window_darwin.m cannot see BambooView's @interface from
// bambooview_darwin.m via a bare `@class` forward decl — sending alloc /
// initWithFrame: / setAutoresizingMask: to a forward-declared class is a hard
// error in modern clang. Both .m files #import this header to share the real
// @interface.
@interface BambooView : NSView
@end
