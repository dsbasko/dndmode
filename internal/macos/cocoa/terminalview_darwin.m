// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // available via -framework QuartzCore (CALayer)
#import "terminalview_darwin.h"    // shared @interface TerminalView (also imported by window_darwin.m)

// TerminalView renders a scrolling stream of pseudo-source-code as the
// contentView of a shield overlay window when config `overlay_style: terminal`
// is selected. It is a pure cosmetic content swap on top of the opaque shield
// NSWindow (window_darwin.m): the window keeps setOpaque:YES, this view's
// backing layer is opaque black, so the desktop can never bleed through
// (T-gh8-03) — byte-for-byte the same blocking guarantees as `black`/`matrix`.
//
// Animation model (added incrementally across the plan tasks):
//   * A ring of VISIBLE LINES is drawn top -> bottom. The bottom line TYPES
//     itself out one character at a time behind a blinking caret; when it
//     finishes it pauses, then the buffer JUMP-SCROLLS up by one line and the
//     next corpus line begins typing at the bottom (classic terminal cadence).
//   * Each line is TOKENIZED ONCE when it enters the buffer (not per frame) into
//     colored segments for light syntax highlighting; drawRect: just paints the
//     pre-colored segments.
//
// Ambient, never reactive (security stance "silent on wrong input"): the
// animation is driven solely by its own timer and ignores ALL input. No
// NSResponder input handling is added — a reactive flash/shake would leak the
// fact that keystrokes are being intercepted.
//
// Cadence (T-gh8-02): FPS-CAPPED ~30 via NSTimer in NSRunLoopCommonModes on the
// MAIN run loop, so all state + drawRect: is main-thread-safe. Drawing is FLAT
// (one drawAtPoint per segment, no NSShadow/blur) — cheap enough for all
// displays at 30 FPS.
//
// Lifecycle — leak-free contract (CRITICAL must_have): start the timer in
// viewDidMoveToWindow (window != nil); stop+release it in viewWillMoveToWindow:nil
// AND dealloc (guarded against double-invalidate).

static const NSTimeInterval kTermFrameInterval = 1.0 / 30.0; // ~30 FPS cap

// Base monospaced font size (points). Cell metrics derive from this.
static const CGFloat kTermFontSize = 16.0;

@implementation TerminalView {
    NSTimer *_timer;  // ~30 FPS driver; nil when stopped
    NSFont  *_font;   // cached monospaced font (built once in initWithFrame:)
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor]; // opaque #000000 backing
        [self buildFont];
    }
    return self;
}

- (void)buildFont {
    NSFont *f = [NSFont monospacedSystemFontOfSize:kTermFontSize
                                            weight:NSFontWeightRegular];
    if (f == nil) {
        f = [NSFont fontWithName:@"Menlo" size:kTermFontSize];
    }
    _font = f;
}

- (BOOL)isOpaque  { return YES; }
- (BOOL)isFlipped { return YES; } // y grows downward -> rows advance top to bottom

// --- Drawing: placeholder opaque-black fill (full source render lands in Task 4) ---

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    [[NSColor blackColor] setFill]; // pure #000000, fully opaque
    NSRectFill([self bounds]);
}

// --- Per-frame advance: no-op placeholder (typing/scroll state lands in Task 3) ---

- (void)step:(NSTimer *)t {
    (void)t;
    [self setNeedsDisplay:YES];
}

// --- Lifecycle: start/stop the FPS-capped timer with window attachment ---

- (void)startTimer {
    if (_timer != nil) return;
    _timer = [NSTimer timerWithTimeInterval:kTermFrameInterval
                                     target:self
                                   selector:@selector(step:)
                                   userInfo:nil
                                    repeats:YES];
    [[NSRunLoop mainRunLoop] addTimer:_timer forMode:NSRunLoopCommonModes];
}

- (void)stopTimer {
    if (_timer != nil) {
        [_timer invalidate];
        _timer = nil;
    }
}

- (void)viewDidMoveToWindow {
    [super viewDidMoveToWindow];
    if (self.window != nil) {
        [self startTimer];
    } else {
        [self stopTimer];
    }
}

- (void)viewWillMoveToWindow:(NSWindow *)newWindow {
    [super viewWillMoveToWindow:newWindow];
    if (newWindow == nil) {
        [self stopTimer]; // fires on [w close] before dealloc
    }
}

- (void)dealloc {
    [self stopTimer];
    // ARC handles _font / _timer object refs. (Malloc'd buffers land in Task 3.)
}

@end
