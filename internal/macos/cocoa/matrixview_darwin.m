// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // available via -framework QuartzCore (CVDisplayLink, CALayer)
#import "matrixview_darwin.h"      // shared @interface MatrixView (also imported by window_darwin.m)

// MatrixView renders the classic green "digital rain" as the contentView of a
// shield overlay window when config `overlay_style: matrix` is selected. It is
// a pure cosmetic content swap layered ON TOP of the existing opaque-black
// shield NSWindow (window_darwin.m): the window keeps setOpaque:YES + black
// backgroundColor, and this view's backing layer is ALSO opaque black, so the
// desktop can never bleed through (defense in depth — T-gh8-03).
//
// Animation cadence (T-gh8-02): the redraw is FPS-CAPPED, never free-running.
// We drive it with an NSTimer at ~30 FPS scheduled in NSRunLoopCommonModes on
// the MAIN run loop. The plan explicitly accepts this path (vs CVDisplayLink);
// it was chosen deliberately because the timer fires on the main thread, so
// every state mutation + drawRect: is main-thread-safe by construction with no
// cross-thread marshalling — which keeps the leak-free teardown contract simple
// and correct. (A CVDisplayLink callback fires on a background thread and every
// AppKit touch would have to hop to the main queue; more moving parts, more
// ways to leak a still-scheduled link.)
//
// Lifecycle — the leak-free contract (CRITICAL must_have):
//   * START the timer in viewDidMoveToWindow when self.window != nil.
//   * STOP + RELEASE the timer in BOTH viewWillMoveToWindow:nil AND dealloc
//     (whichever fires first), guarded against double-invalidate. After
//     cocoa_close_overlay_window -> [w close], ARC drops the contentView, these
//     run, and nothing stays scheduled (no leaked NSTimer, no pegged core).
//
// SCOPE (YAGNI): style switching ONLY. No color/speed/density config knobs in
// v1 — classic defaults are hardcoded (green #00FF41, ~30 FPS, katakana+digits,
// monospaced cell). The package links Cocoa + QuartzCore + CoreGraphics and
// compiles with -fobjc-arc (see window_darwin.go cgo CFLAGS/LDFLAGS).

// --- Hardcoded classic-rain constants (v1, no config knobs) ---
static const CGFloat kMatrixFontSize   = 16.0;   // monospaced glyph point size
static const CGFloat kMatrixCellWidth  = 14.0;   // horizontal advance per column
static const CGFloat kMatrixCellHeight = 18.0;   // vertical advance per row
static const NSTimeInterval kMatrixFrameInterval = 1.0 / 30.0; // ~30 FPS cap
static const NSInteger kMatrixTrailLen = 18;     // glyphs drawn per column trail

// Glyph alphabet: half-width katakana (U+FF66..U+FF9D) + ASCII digits 0-9.
static const unichar kKatakanaFirst = 0xFF66;
static const unichar kKatakanaLast  = 0xFF9D;

@implementation MatrixView {
    NSTimer      *_timer;       // ~30 FPS driver; nil when stopped
    NSInteger     _columns;     // floor(bounds.width / cellWidth)
    NSInteger     _rows;        // floor(bounds.height / cellHeight)
    CGFloat      *_headY;       // per-column head position (in rows, float)
    CGFloat      *_speed;       // per-column fall speed (rows per tick)
    NSFont       *_font;        // cached monospaced font
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        // Layer-backed, opaque black base so the view fully covers (no
        // transparency) — sits on top of the window's black backgroundColor.
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor];

        _font = [NSFont monospacedSystemFontOfSize:kMatrixFontSize
                                            weight:NSFontWeightRegular];
        if (_font == nil) {
            _font = [NSFont fontWithName:@"Menlo" size:kMatrixFontSize];
        }
        [self rebuildColumns];
    }
    return self;
}

// This view draws its own opaque content every frame; declaring it opaque lets
// AppKit skip compositing whatever is behind it (it is the full-screen base).
- (BOOL)isOpaque { return YES; }

// Flipped so y grows downward — matches the "rain falls down" mental model and
// lets head positions advance with +speed.
- (BOOL)isFlipped { return YES; }

// --- Column model: (re)allocated on init and on every size change ---

- (void)rebuildColumns {
    NSRect b = [self bounds];
    NSInteger cols = (NSInteger)floor(b.size.width / kMatrixCellWidth);
    NSInteger rows = (NSInteger)floor(b.size.height / kMatrixCellHeight);
    if (cols < 1) cols = 1;
    if (rows < 1) rows = 1;

    free(_headY);
    free(_speed);
    _headY = (CGFloat *)calloc((size_t)cols, sizeof(CGFloat));
    _speed = (CGFloat *)calloc((size_t)cols, sizeof(CGFloat));
    _columns = cols;
    _rows = rows;

    for (NSInteger c = 0; c < cols; c++) {
        // Stagger initial heads above the top so columns don't all start in sync.
        _headY[c] = -(CGFloat)(arc4random_uniform((uint32_t)(rows + 1)));
        _speed[c] = 0.4 + (CGFloat)arc4random_uniform(80) / 100.0; // 0.4..1.2 rows/tick
    }
}

- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self rebuildColumns];
}

- (void)resizeSubviewsWithOldSize:(NSSize)oldSize {
    [super resizeSubviewsWithOldSize:oldSize];
    [self rebuildColumns];
}

// --- Random glyph from the katakana+digits alphabet ---

- (NSString *)randomGlyph {
    // ~25% digits, ~75% katakana — classic mix.
    if (arc4random_uniform(4) == 0) {
        unichar d = (unichar)('0' + arc4random_uniform(10));
        return [NSString stringWithCharacters:&d length:1];
    }
    unichar span = (unichar)(kKatakanaLast - kKatakanaFirst + 1);
    unichar g = (unichar)(kKatakanaFirst + arc4random_uniform(span));
    return [NSString stringWithCharacters:&g length:1];
}

// --- Per-frame state advance, marshalled by the main-thread timer ---

- (void)step:(NSTimer *)t {
    (void)t;
    for (NSInteger c = 0; c < _columns; c++) {
        _headY[c] += _speed[c];
        // Respawn a column once its trail has fully passed the bottom edge.
        if (_headY[c] - (CGFloat)kMatrixTrailLen > (CGFloat)_rows) {
            _headY[c] = -(CGFloat)(arc4random_uniform((uint32_t)(_rows / 2 + 1)));
            _speed[c] = 0.4 + (CGFloat)arc4random_uniform(80) / 100.0;
        }
    }
    [self setNeedsDisplay:YES];
}

// --- Drawing: opaque black base + per-column fading green trail ---

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    NSRect b = [self bounds];

    // Opaque black base (covers everything; no desktop bleed-through).
    [[NSColor blackColor] setFill];
    NSRectFill(b);

    NSColor *leadColor  = [NSColor colorWithSRGBRed:0.85 green:1.00 blue:0.85 alpha:1.0]; // near-white green
    // Trail base green #00FF41.
    const CGFloat trailR = 0.0, trailG = 1.0, trailB = 0x41 / 255.0;

    for (NSInteger c = 0; c < _columns; c++) {
        CGFloat headRow = _headY[c];
        CGFloat x = (CGFloat)c * kMatrixCellWidth;

        for (NSInteger k = 0; k < kMatrixTrailLen; k++) {
            CGFloat row = headRow - (CGFloat)k;
            if (row < 0 || row >= (CGFloat)_rows) {
                continue; // off-screen segment of the trail
            }
            CGFloat y = row * kMatrixCellHeight;

            NSColor *color;
            if (k == 0) {
                color = leadColor; // brightest leading glyph
            } else {
                // Linear fade from full green to nearly dark down the trail.
                CGFloat fade = 1.0 - ((CGFloat)k / (CGFloat)kMatrixTrailLen);
                color = [NSColor colorWithSRGBRed:trailR * fade
                                            green:trailG * fade
                                             blue:trailB * fade
                                            alpha:1.0];
            }

            NSDictionary *attrs = @{
                NSFontAttributeName: _font,
                NSForegroundColorAttributeName: color,
            };
            [[self randomGlyph] drawAtPoint:NSMakePoint(x, y) withAttributes:attrs];
        }
    }
}

// --- Lifecycle: start/stop the FPS-capped timer with window attachment ---

- (void)startTimer {
    if (_timer != nil) return; // already running (guard against double-start)
    _timer = [NSTimer timerWithTimeInterval:kMatrixFrameInterval
                                     target:self
                                   selector:@selector(step:)
                                   userInfo:nil
                                    repeats:YES];
    // Common modes so the rain keeps animating during tracking/modal loops.
    [[NSRunLoop mainRunLoop] addTimer:_timer forMode:NSRunLoopCommonModes];
}

- (void)stopTimer {
    if (_timer != nil) {
        [_timer invalidate]; // removes from the run loop + drops the retain
        _timer = nil;        // guard against double-invalidate
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
    // Detaching from a window (newWindow == nil) -> stop now so nothing stays
    // scheduled. This fires on [w close] before dealloc.
    if (newWindow == nil) {
        [self stopTimer];
    }
}

- (void)dealloc {
    // Defense in depth: if the view is torn down without a window-detach
    // notification, stop here too. Double-stop is a guarded no-op.
    [self stopTimer];
    free(_headY);
    free(_speed);
    _headY = NULL;
    _speed = NULL;
    // ARC handles _font / _timer object refs.
}

@end
