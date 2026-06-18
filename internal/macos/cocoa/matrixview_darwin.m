// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // available via -framework QuartzCore (CALayer)
#import "matrixview_darwin.h"      // shared @interface MatrixView (also imported by window_darwin.m)

// MatrixView renders a smooth green "digital rain" as the contentView of a
// shield overlay window when config `overlay_style: matrix` is selected. It is
// a pure cosmetic content swap on top of the opaque shield NSWindow
// (window_darwin.m): the window keeps setOpaque:YES, this view's backing layer
// is opaque black, so the desktop can never bleed through (T-gh8-03).
//
// Animation model (rebuilt for smoothness — no churn, no flares):
//   * FIXED GRID. Each column is a fixed grid of cells; every cell holds a
//     STABLE glyph. Glyphs are NOT re-rolled every frame (that earlier churn is
//     exactly what read as "twitchy"). A cell's glyph is written ONCE, by the
//     head as it passes (authentic — the leading edge "types" new characters),
//     and then stays put until the column wraps and refills.
//   * CONTINUOUS HEAD. Each column's head slides down at a per-column speed (in
//     cells/frame, fractional). A cell's brightness is a smooth function of its
//     distance to the head, so the bright leading glyph + fading trail glide as
//     a brightness GRADIENT over stationary glyphs — no per-frame redraw jitter.
//   * NO random bright "flare" flickers (removed — that was the random
//     single-symbol popping the user saw).
//
// Cadence (T-gh8-02): FPS-CAPPED ~30 via NSTimer in NSRunLoopCommonModes on the
// MAIN run loop, so all state + drawRect: is main-thread-safe. Drawing is FLAT
// (one drawAtPoint per lit cell, no NSShadow/blur) — cheap enough for all
// displays at 30 FPS.
//
// Lifecycle — leak-free contract (CRITICAL must_have): start the timer in
// viewDidMoveToWindow (window != nil); stop+release it in viewWillMoveToWindow:nil
// AND dealloc (guarded against double-invalidate).
//
// Look (hardcoded, no config knobs — v1): green only. Columns are VARIED for
// depth — per-column glyph SIZE (kMatrixSizes), per-column OPACITY and per-column
// SHADE (kMatrixPalette). Leading glyph is green-white; the trail fades the
// column's green down to black over a per-column trail length.

static const NSTimeInterval kMatrixFrameInterval = 1.0 / 30.0; // ~30 FPS cap

// Per-column glyph sizes (points) — varied so columns read as different depths.
static const CGFloat kMatrixSizes[] = { 24.0, 34.0, 46.0 };
static const NSInteger kMatrixSizeCount = (NSInteger)(sizeof(kMatrixSizes) / sizeof(kMatrixSizes[0]));

// Vertical + horizontal advance as multiples of a column's point size.
static const CGFloat kMatrixCellHFactor = 1.18;
static const CGFloat kMatrixCellWFactor = 0.92;

// All-green palette — different SHADES of green only (no other hues).
typedef struct { CGFloat r, g, b; } MatrixRGB;
static const MatrixRGB kMatrixPalette[] = {
    { 0.00, 1.00, 0.25 },  // classic matrix green (#00FF41-ish)
    { 0.45, 1.00, 0.20 },  // lime / yellow-green
    { 0.00, 0.90, 0.45 },  // emerald / teal-green
    { 0.25, 0.85, 0.30 },  // soft mid green
};
static const NSInteger kMatrixPaletteCount = (NSInteger)(sizeof(kMatrixPalette) / sizeof(kMatrixPalette[0]));

// Glyph alphabet: half-width katakana (U+FF66..U+FF9D) + ASCII digits 0-9.
static const unichar kKatakanaFirst = 0xFF66;
static const unichar kKatakanaLast  = 0xFF9D;

@implementation MatrixView {
    NSTimer           *_timer;     // ~30 FPS driver; nil when stopped
    NSInteger          _columns;   // active column count (variable-width pack)
    NSInteger          _maxRows;   // row capacity (smallest cell) — _glyphs stride
    CGFloat           *_x;         // per-column x position (points)
    NSInteger         *_bucket;    // per-column size bucket -> kMatrixSizes / _fonts
    NSInteger         *_hue;       // per-column shade -> kMatrixPalette
    CGFloat           *_alpha;     // per-column opacity multiplier
    CGFloat           *_head;      // per-column head position (cells, fractional)
    CGFloat           *_speed;     // per-column fall speed (cells/frame)
    CGFloat           *_trail;     // per-column trail length (cells)
    NSInteger         *_rows;      // per-column cell count (fits the height)
    unichar           *_glyphs;    // stable cell glyphs, indexed [c*_maxRows + r]
    NSArray<NSFont *> *_fonts;     // one cached monospaced font per size bucket
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor];
        [self buildFonts];
        [self rebuildColumns];
    }
    return self;
}

- (void)buildFonts {
    NSMutableArray<NSFont *> *fs = [NSMutableArray arrayWithCapacity:(NSUInteger)kMatrixSizeCount];
    for (NSInteger i = 0; i < kMatrixSizeCount; i++) {
        NSFont *f = [NSFont monospacedSystemFontOfSize:kMatrixSizes[i]
                                                weight:NSFontWeightMedium];
        if (f == nil) {
            f = [NSFont fontWithName:@"Menlo" size:kMatrixSizes[i]];
        }
        [fs addObject:f];
    }
    _fonts = [fs copy];
}

- (BOOL)isOpaque  { return YES; }
- (BOOL)isFlipped { return YES; } // y grows downward -> rain falls with +head

// One random glyph codepoint (~25% digits, ~75% katakana).
- (unichar)randomUnichar {
    if (arc4random_uniform(4) == 0) {
        return (unichar)('0' + arc4random_uniform(10));
    }
    unichar span = (unichar)(kKatakanaLast - kKatakanaFirst + 1);
    return (unichar)(kKatakanaFirst + arc4random_uniform(span));
}

// (Re)seed one column's motion + fill all its cells with fresh stable glyphs.
- (void)respawnColumn:(NSInteger)c rows:(NSInteger)rows {
    _trail[c] = 8.0 + (CGFloat)arc4random_uniform(13);          // 8..20 cells
    _speed[c] = 0.12 + (CGFloat)arc4random_uniform(29) / 100.0; // 0.12..0.40 cells/frame
    // Start above the top by a random gap so columns enter staggered, not in sync.
    _head[c]  = -(CGFloat)arc4random_uniform((uint32_t)(rows + (NSInteger)_trail[c] + 1));
    for (NSInteger r = 0; r < rows; r++) {
        _glyphs[(size_t)c * (size_t)_maxRows + (size_t)r] = [self randomUnichar];
    }
}

// --- Column model: variable-width pack, (re)allocated on init + size change ---

- (void)rebuildColumns {
    NSRect b = [self bounds];

    free(_x); free(_bucket); free(_hue); free(_alpha);
    free(_head); free(_speed); free(_trail); free(_rows); free(_glyphs);
    _x = _alpha = _head = _speed = _trail = NULL;
    _bucket = _hue = _rows = NULL;
    _glyphs = NULL;
    _columns = 0;

    CGFloat minCellH = kMatrixSizes[0] * kMatrixCellHFactor;
    CGFloat minCellW = kMatrixSizes[0] * kMatrixCellWFactor;
    _maxRows = (NSInteger)floor(b.size.height / minCellH) + 2;
    if (_maxRows < 1) _maxRows = 1;
    NSInteger cap = (NSInteger)floor(b.size.width / minCellW) + 2;
    if (cap < 1) cap = 1;

    _x      = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _bucket = (NSInteger *)calloc((size_t)cap, sizeof(NSInteger));
    _hue    = (NSInteger *)calloc((size_t)cap, sizeof(NSInteger));
    _alpha  = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _head   = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _speed  = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _trail  = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _rows   = (NSInteger *)calloc((size_t)cap, sizeof(NSInteger));
    _glyphs = (unichar *)calloc((size_t)cap * (size_t)_maxRows, sizeof(unichar));

    NSInteger c = 0;
    CGFloat x = 0.0;
    while (x < b.size.width && c < cap) {
        NSInteger bk = (NSInteger)arc4random_uniform((uint32_t)kMatrixSizeCount);
        CGFloat size  = kMatrixSizes[bk];
        CGFloat cellW = size * kMatrixCellWFactor;
        CGFloat cellH = size * kMatrixCellHFactor;
        NSInteger rows = (NSInteger)floor(b.size.height / cellH) + 1;
        if (rows > _maxRows) rows = _maxRows;

        _bucket[c] = bk;
        _hue[c]    = (NSInteger)arc4random_uniform((uint32_t)kMatrixPaletteCount);
        _x[c]      = x;
        _alpha[c]  = 0.45 + (CGFloat)arc4random_uniform(56) / 100.0; // 0.45..1.0
        _rows[c]   = rows;
        [self respawnColumn:c rows:rows];

        x += cellW;
        c++;
    }
    _columns = c;
}

- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self rebuildColumns];
}

- (void)resizeSubviewsWithOldSize:(NSSize)oldSize {
    [super resizeSubviewsWithOldSize:oldSize];
    [self rebuildColumns];
}

// --- Per-frame advance: slide each head; the head "types" fresh glyphs as it ---
// --- crosses into new cells; columns wrap when their trail clears the bottom. --

- (void)step:(NSTimer *)t {
    (void)t;
    for (NSInteger c = 0; c < _columns; c++) {
        NSInteger prev = (NSInteger)floor(_head[c]);
        _head[c] += _speed[c];

        if (_head[c] - _trail[c] > (CGFloat)_rows[c]) {
            [self respawnColumn:c rows:_rows[c]]; // whole streak passed the bottom
            continue;
        }
        // Write a fresh stable glyph into every cell the head just entered.
        NSInteger cur = (NSInteger)floor(_head[c]);
        for (NSInteger r = prev + 1; r <= cur; r++) {
            if (r >= 0 && r < _rows[c]) {
                _glyphs[(size_t)c * (size_t)_maxRows + (size_t)r] = [self randomUnichar];
            }
        }
    }
    [self setNeedsDisplay:YES];
}

// --- Drawing: black base + per-column streak as a smooth brightness gradient ---

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    NSRect b = [self bounds];

    [[NSColor blackColor] setFill]; // pure #000000, fully opaque
    NSRectFill(b);

    for (NSInteger c = 0; c < _columns; c++) {
        NSFont   *font  = _fonts[(NSUInteger)_bucket[c]];
        CGFloat   cellH = kMatrixSizes[_bucket[c]] * kMatrixCellHFactor;
        CGFloat   colA  = _alpha[c];
        CGFloat   x     = _x[c];
        CGFloat   head  = _head[c];
        CGFloat   trail = _trail[c];
        NSInteger rows  = _rows[c];
        MatrixRGB hue   = kMatrixPalette[_hue[c]];

        // Only the lit window [head-trail .. head] needs drawing.
        NSInteger lo = (NSInteger)floor(head - trail);
        NSInteger hi = (NSInteger)floor(head);
        if (lo < 0) lo = 0;
        if (hi >= rows) hi = rows - 1;

        for (NSInteger r = lo; r <= hi; r++) {
            CGFloat d = head - (CGFloat)r;   // 0 at head .. grows up the trail
            if (d < 0.0 || d >= trail) {
                continue;
            }
            CGFloat bright = (1.0 - d / trail) * colA;        // smooth fade head->tail
            CGFloat w = (d < 1.5) ? (1.5 - d) / 1.5 * 0.85 : 0.0; // whiten near the head

            CGFloat rr = fmin(1.0, hue.r + w)       * bright;
            CGFloat gg = fmin(1.0, hue.g + w * 0.3) * bright;
            CGFloat bb = fmin(1.0, hue.b + w)       * bright;

            unichar u = _glyphs[(size_t)c * (size_t)_maxRows + (size_t)r];
            NSString *glyph = [NSString stringWithCharacters:&u length:1];
            [glyph drawAtPoint:NSMakePoint(x, (CGFloat)r * cellH) withAttributes:@{
                NSFontAttributeName: font,
                NSForegroundColorAttributeName: [NSColor colorWithSRGBRed:rr green:gg blue:bb alpha:1.0],
            }];
        }
    }
}

// --- Lifecycle: start/stop the FPS-capped timer with window attachment ---

- (void)startTimer {
    if (_timer != nil) return;
    _timer = [NSTimer timerWithTimeInterval:kMatrixFrameInterval
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
    free(_x);
    free(_bucket);
    free(_hue);
    free(_alpha);
    free(_head);
    free(_speed);
    free(_trail);
    free(_rows);
    free(_glyphs);
    _x = _alpha = _head = _speed = _trail = NULL;
    _bucket = _hue = _rows = NULL;
    _glyphs = NULL;
    // ARC handles _fonts / _timer object refs.
}

@end
