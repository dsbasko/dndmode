// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // available via -framework QuartzCore (CVDisplayLink, CALayer)
#import "matrixview_darwin.h"      // shared @interface MatrixView (also imported by window_darwin.m)

// MatrixView renders a cyberpunk "digital rain" as the contentView of a shield
// overlay window when config `overlay_style: matrix` is selected. It is a pure
// cosmetic content swap layered ON TOP of the existing opaque shield NSWindow
// (window_darwin.m): the window keeps setOpaque:YES, and this view's backing
// layer is ALSO opaque, so the desktop can never bleed through (T-gh8-03).
//
// Animation cadence (T-gh8-02): the redraw is FPS-CAPPED, never free-running —
// an NSTimer at ~30 FPS in NSRunLoopCommonModes on the MAIN run loop, so every
// state mutation + drawRect: is main-thread-safe with no cross-thread marshalling.
//
// Lifecycle — the leak-free contract (CRITICAL must_have):
//   * START the timer in viewDidMoveToWindow when self.window != nil.
//   * STOP + RELEASE the timer in BOTH viewWillMoveToWindow:nil AND dealloc
//     (whichever fires first), guarded against double-invalidate.
//
// Look (hardcoded, no config knobs — v1 scope is style switching only): neon
// green "digital rain" over a pure black (#000000) base. Columns are VARIED for
// depth — each picks one of several glyph sizes (kMatrixSizes), its own opacity
// (per-column alpha) AND one of several MATRIX-GREEN shades (kMatrixPalette —
// green only). Every glyph is a NEON TUBE: a wide green bloom (blurred NSShadow)
// under a bright green-white core, with occasional brighter "data flare"
// flickers. Two shadowed passes per glyph — the glow is the expensive part; if
// it ever costs too much it can be restricted to leading glyphs.

// --- Hardcoded constants (v1, no config knobs) ---
static const NSTimeInterval kMatrixFrameInterval = 1.0 / 30.0; // ~30 FPS cap
static const NSInteger kMatrixTrailLen = 16;     // glyphs drawn per column trail

// Per-column glyph sizes (points) — bigger than a flat single size and varied,
// so different columns read as visibly different sizes / depths.
static const CGFloat kMatrixSizes[] = { 24.0, 34.0, 46.0 };
static const NSInteger kMatrixSizeCount = (NSInteger)(sizeof(kMatrixSizes) / sizeof(kMatrixSizes[0]));

// Vertical + horizontal advance as multiples of a column's point size.
static const CGFloat kMatrixCellHFactor = 1.18;
static const CGFloat kMatrixCellWFactor = 0.92;

// All-green Matrix palette — different SHADES of green only (no other hues).
// Each column is assigned one shade uniformly, so the field has green variety
// (classic / lime / emerald / soft) without leaving the Matrix green family.
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
    NSTimer           *_timer;      // ~30 FPS driver; nil when stopped
    NSInteger          _columns;    // active column count (variable-width pack)
    CGFloat           *_x;          // per-column x position (points)
    CGFloat           *_headPx;     // per-column head Y position (points, flipped)
    CGFloat           *_speedPx;    // per-column fall speed (points/tick)
    CGFloat           *_alpha;      // per-column opacity multiplier (0.35..1.0)
    NSInteger         *_bucket;     // per-column size bucket index into kMatrixSizes
    NSInteger         *_hue;        // per-column neon hue index into kMatrixPalette
    NSArray<NSFont *> *_fonts;      // one cached monospaced font per size bucket
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        // Layer-backed, opaque base so the view fully covers (no transparency).
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor];

        [self buildFonts];     // one font per size bucket (built once)
        [self rebuildColumns]; // per-column geometry (rebuilt on resize)
    }
    return self;
}

// Build one cached monospaced font per size bucket (built once, not per resize).
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

// This view draws its own opaque content every frame; declaring it opaque lets
// AppKit skip compositing whatever is behind it (it is the full-screen base).
- (BOOL)isOpaque { return YES; }

// Flipped so y grows downward — matches the "rain falls down" model.
- (BOOL)isFlipped { return YES; }

// --- Column model: variable-width pack, (re)allocated on init + size change ---

- (void)rebuildColumns {
    NSRect b = [self bounds];

    free(_x); free(_headPx); free(_speedPx); free(_alpha); free(_bucket); free(_hue);
    _x = _headPx = _speedPx = _alpha = NULL;
    _bucket = _hue = NULL;
    _columns = 0;

    CGFloat minCellW = kMatrixSizes[0] * kMatrixCellWFactor;
    NSInteger cap = (NSInteger)floor(b.size.width / minCellW) + 2;
    if (cap < 1) cap = 1;

    _x       = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _headPx  = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _speedPx = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _alpha   = (CGFloat *)calloc((size_t)cap, sizeof(CGFloat));
    _bucket  = (NSInteger *)calloc((size_t)cap, sizeof(NSInteger));
    _hue     = (NSInteger *)calloc((size_t)cap, sizeof(NSInteger));

    NSInteger c = 0;
    CGFloat x = 0.0;
    while (x < b.size.width && c < cap) {
        NSInteger bk = (NSInteger)arc4random_uniform((uint32_t)kMatrixSizeCount);
        CGFloat size  = kMatrixSizes[bk];
        CGFloat cellW = size * kMatrixCellWFactor;
        CGFloat cellH = size * kMatrixCellHFactor;

        // All-green field: pick one of the green shades uniformly.
        NSInteger hue = (NSInteger)arc4random_uniform((uint32_t)kMatrixPaletteCount);

        _bucket[c]  = bk;
        _hue[c]     = hue;
        _x[c]       = x;
        _alpha[c]   = 0.35 + (CGFloat)arc4random_uniform(66) / 100.0; // 0.35..1.0
        _headPx[c]  = -(CGFloat)arc4random_uniform((uint32_t)(b.size.height + 1.0));
        _speedPx[c] = cellH * (0.25 + (CGFloat)arc4random_uniform(46) / 100.0);

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
    NSRect b = [self bounds];
    for (NSInteger c = 0; c < _columns; c++) {
        CGFloat cellH = kMatrixSizes[_bucket[c]] * kMatrixCellHFactor;
        _headPx[c] += _speedPx[c];
        if (_headPx[c] - (CGFloat)kMatrixTrailLen * cellH > b.size.height) {
            _headPx[c]  = -(CGFloat)arc4random_uniform((uint32_t)(b.size.height / 2.0 + 1.0));
            _speedPx[c] = cellH * (0.25 + (CGFloat)arc4random_uniform(46) / 100.0);
        }
    }
    [self setNeedsDisplay:YES];
}

// --- Helper: draw one glyph as a glowing layer (blurred shadow + fill) ---

- (void)drawGlyph:(NSString *)glyph
              at:(NSPoint)p
            font:(NSFont *)font
            fill:(NSColor *)fill
       glowColor:(NSColor *)glowColor
        glowBlur:(CGFloat)glowBlur {
    NSShadow *glow = [[NSShadow alloc] init];
    glow.shadowOffset     = NSZeroSize;
    glow.shadowBlurRadius = glowBlur;
    glow.shadowColor      = glowColor;
    [glyph drawAtPoint:p withAttributes:@{
        NSFontAttributeName: font,
        NSForegroundColorAttributeName: fill,
        NSShadowAttributeName: glow,
    }];
}

// --- Drawing: pure black base + per-column green neon-tube rain ---

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    NSRect b = [self bounds];

    // Pure black base (#000000), fully opaque — no desktop bleed-through.
    [[NSColor blackColor] setFill];
    NSRectFill(b);

    for (NSInteger c = 0; c < _columns; c++) {
        NSFont   *font  = _fonts[(NSUInteger)_bucket[c]];
        CGFloat   size  = kMatrixSizes[_bucket[c]];
        CGFloat   cellH = size * kMatrixCellHFactor;
        CGFloat   colA  = _alpha[c];
        CGFloat   x     = _x[c];
        CGFloat   headPx = _headPx[c];
        MatrixRGB hue   = kMatrixPalette[_hue[c]];

        for (NSInteger k = 0; k < kMatrixTrailLen; k++) {
            CGFloat y = headPx - (CGFloat)k * cellH;
            if (y < -cellH || y > b.size.height) {
                continue; // off-screen segment of the trail
            }

            CGFloat fade = (k == 0) ? 1.0 : 1.0 - ((CGFloat)k / (CGFloat)kMatrixTrailLen);
            CGFloat a    = colA * fade;
            NSPoint p    = NSMakePoint(x, y);
            NSString *glyph = [self randomGlyph];

            // Rare white "data flare" — cyberpunk glitch flicker.
            BOOL flare = (arc4random_uniform(100) < 2);

            // Pass 1 — wide saturated neon bloom (skip the faintest tail: invisible + costly).
            if (fade > 0.12) {
                NSColor *bloom = [NSColor colorWithSRGBRed:hue.r green:hue.g blue:hue.b
                                                     alpha:a * 0.9];
                CGFloat bloomBlur = size * (k == 0 ? 1.20 : 0.45 + 0.45 * fade);
                [self drawGlyph:glyph at:p font:font fill:bloom
                      glowColor:bloom glowBlur:bloomBlur];
            }

            // Pass 2 — white-hot core (lead/flare = white; trail = hue pushed toward white).
            NSColor *core;
            if (k == 0 || flare) {
                // Bright green-white head (stays in the green family, not pure white).
                core = [NSColor colorWithSRGBRed:0.80 green:1.00 blue:0.80
                                          alpha:fmin(1.0, a + 0.25)];
            } else {
                core = [NSColor colorWithSRGBRed:fmin(1.0, hue.r + 0.45)
                                          green:fmin(1.0, hue.g + 0.20)
                                           blue:fmin(1.0, hue.b + 0.45)
                                          alpha:a];
            }
            NSColor *coreGlow = [NSColor colorWithSRGBRed:hue.r green:hue.g blue:hue.b alpha:a];
            [self drawGlyph:glyph at:p font:font fill:core
                  glowColor:coreGlow glowBlur:size * 0.22];
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
    if (newWindow == nil) {
        [self stopTimer]; // fires on [w close] before dealloc
    }
}

- (void)dealloc {
    [self stopTimer];
    free(_x);
    free(_headPx);
    free(_speedPx);
    free(_alpha);
    free(_bucket);
    free(_hue);
    _x = _headPx = _speedPx = _alpha = NULL;
    _bucket = _hue = NULL;
    // ARC handles _fonts / _timer object refs.
}

@end
