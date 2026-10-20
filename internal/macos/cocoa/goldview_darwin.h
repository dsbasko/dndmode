// +build darwin

#import <Cocoa/Cocoa.h>

// GoldView is the candlestick-chart contentView used by the shield overlay window
// when config `overlay_style: gold` is selected. A synthetic gold price (XAU/USD)
// climbs steadily as a chart of Japanese candlesticks: the newest candle forms at
// the right edge, its close wandering frame by frame, while the whole chart
// slides smoothly left and the price axis refits itself to the visible range.
// Behind the chart, hundreds of pixel-art gold coins rain down. The full
// @implementation (price and rain models, layout, lifecycle, threading
// rationale) lives in goldview_darwin.m.
//
// Like MatrixView / TerminalView / DVDView, this is a purely cosmetic content
// swap on top of the opaque shield NSWindow (window_darwin.m): the window keeps
// setOpaque:YES, this view's backing layer is opaque black, so the desktop can
// never bleed through (T-gh8-03). One GoldView is installed per display, each
// seeded on its own, so every screen draws its own chart.
//
// This header exists because cgo compiles each .m file as a SEPARATE clang
// translation unit, so window_darwin.m cannot see GoldView's @interface from
// goldview_darwin.m via a bare `@class` forward decl — sending alloc /
// initWithFrame: / setAutoresizingMask: to a forward-declared class is a hard
// error in modern clang. Both .m files #import this header to share the real
// @interface.
@interface GoldView : NSView
@end
