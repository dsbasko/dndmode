// +build darwin

#import <Cocoa/Cocoa.h>

// TerminalView is the animated scrolling-source contentView used by the shield
// overlay window when config `overlay_style: terminal` is selected. It renders a
// stream of pseudo-code lines that type themselves out behind a blinking caret
// and jump-scroll up as new lines arrive, with light syntax highlighting. The
// full @implementation (and its design/lifecycle/threading rationale) lives in
// terminalview_darwin.m.
//
// Like MatrixView, this is a purely cosmetic content swap on top of the opaque
// shield NSWindow (window_darwin.m): the window keeps setOpaque:YES, this view's
// backing layer is opaque black, so the desktop can never bleed through
// (T-gh8-03).
//
// This header exists because cgo compiles each .m file as a SEPARATE clang
// translation unit, so window_darwin.m cannot see TerminalView's @interface from
// terminalview_darwin.m via a bare `@class` forward decl — sending alloc /
// initWithFrame: / setAutoresizingMask: to a forward-declared class is a hard
// error in modern clang. Both .m files #import this header to share the real
// @interface.
@interface TerminalView : NSView
// initWithFrame:language: selects the source language rendered (from the
// --style terminal:<lang> suffix): "go" (default / NULL), "python", "typescript"
// or "rust". Each picks its own compiled-in corpus + syntax highlighting. Plain
// initWithFrame: defaults to Go.
- (instancetype)initWithFrame:(NSRect)frameRect language:(const char *)language;
@end
