// +build darwin

#import <Cocoa/Cocoa.h>

// MatrixView is the animated green digital-rain contentView used by the shield
// overlay window when config `overlay_style: matrix` is selected. The full
// @implementation (and its design/lifecycle/threading rationale) lives in
// matrixview_darwin.m.
//
// This header exists because cgo compiles each .m file as a SEPARATE clang
// translation unit, so window_darwin.m cannot see MatrixView's @interface from
// matrixview_darwin.m via a bare `@class` forward decl — sending alloc /
// initWithFrame: / setAutoresizingMask: to a forward-declared class is a hard
// error in modern clang. Both .m files #import this header to share the real
// @interface.
@interface MatrixView : NSView
@end
