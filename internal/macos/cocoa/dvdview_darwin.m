// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // CALayer (opaque black backing, like MatrixView)
#import "dvdview_darwin.h"         // shared @interface DVDView (also imported by window_darwin.m)
#import "dvdlogo_darwin.h"         // compiled-in DVD-VIDEO logo mask (kDVDLogoPNG / dims)

// DVDView renders the real "DVD VIDEO" logo bouncing edge-to-edge as the
// contentView of a shield overlay window when config `overlay_style: dvd` is
// selected — the old-DVD-player screensaver. It is a purely cosmetic content swap
// on top of the opaque shield NSWindow (window_darwin.m): the window keeps
// setOpaque:YES, this view's backing layer is opaque black, so the desktop can
// never bleed through (T-gh8-03). Every blocking guarantee (HID event tap, shield
// level) is byte-for-byte identical to black.
//
// Rendering model — VARIANT A (mask built once, recolored every frame):
//   * The logo is the ACTUAL DVD-VIDEO mark, compiled in as an 8-bit grayscale
//     PNG (dvdlogo_darwin.h) — a faithful 1:1 silhouette, not a hand-drawn
//     approximation. buildMask decodes it ONCE into a CGImage *image mask* where a
//     sample of 0 paints and 255 skips (so the white background AND the disc's
//     oval hole are punched out — the black shield shows through the hole).
//   * The logo is MONOCHROME (one color that changes), so drawRect: just draws
//     that static mask at the current position with the CURRENT palette color as
//     the fill — CGContextDrawImage stencils the fill color through the mask. No
//     per-frame geometry. That is exactly what an old DVD player does: recolor the
//     same silhouette. The mask is resolution-independent to scale (built once at
//     the PNG's native size; drawn scaled into _logoSize).
//
// Physics — pure + unit-tested. The motion (advance, edge bounce, color advance,
// corner detection, flash decay) lives in dvd_step, a Cocoa-free C function that
// steps a DVDState struct. The view holds one DVDState and calls dvd_step each
// frame, then reads it in drawRect:; the SAME function is exercised by the Go unit
// test via the dvd_step_for_test shim (bottom of this file) — drawRect: output is
// owned by the WindowServer and can only be validated in the manual visual run.
//
// Cadence: FPS-capped ~30 via NSTimer in NSRunLoopCommonModes on the MAIN run
// loop, so all state + drawRect: is main-thread-safe (same contract as MatrixView).
//
// Lifecycle — leak-free contract: start the timer in viewDidMoveToWindow
// (window != nil); stop it in viewWillMoveToWindow:nil AND dealloc (guarded
// against double-invalidate); release the logo mask in dealloc.

static const NSTimeInterval kDVDFrameInterval = 1.0 / 30.0; // ~30 FPS cap

static const CGFloat kDVDLogoWidthFrac = 0.16; // logo width as a fraction of screen width
static const CGFloat kDVDMinLogoWidth  = 80.0; // floor so tiny/off screens stay legible
static const CGFloat kDVDSpeed         = 2.5;  // points/frame (~75 pt/s at 30 FPS)
static const CGFloat kDVDFlashDecay    = 0.08; // per-frame flash fade (~0.4s to zero)

// Bright neon palette — the logo cycles to the NEXT entry on every bounce. Kept
// vivid (fully-saturated primaries + secondaries) so each recolor reads clearly.
typedef struct { CGFloat r, g, b; } DVDRGB;
static const DVDRGB kDVDPalette[] = {
    { 1.00, 0.20, 0.20 },  // red
    { 1.00, 0.55, 0.10 },  // orange
    { 1.00, 0.95, 0.20 },  // yellow
    { 0.30, 1.00, 0.35 },  // green
    { 0.20, 0.95, 1.00 },  // cyan
    { 0.35, 0.55, 1.00 },  // blue
    { 0.80, 0.35, 1.00 },  // purple
    { 1.00, 0.40, 0.85 },  // pink
};
static const int kDVDPaletteCount = (int)(sizeof(kDVDPalette) / sizeof(kDVDPalette[0]));

// --- Pure physics: one animation tick, Cocoa-free + unit-tested -------------
//
// DVDState is the full motion state. dvd_step advances it by one frame: move,
// then clamp+reflect off each edge, advancing the color index on any bounce and
// arming the corner flash when BOTH axes reflect in the same frame. Because the
// color index advances by exactly 1 (mod paletteN), the new color is always
// different from the previous one for any paletteN > 1 (the "new != previous"
// guarantee, for free). Kept in this file so dvd_step_for_test can reach it.
typedef struct {
    double x, y;      // logo origin (bottom-left) in view points
    double vx, vy;    // velocity (points/frame); sign = direction
    double w, h;      // logo size (points)
    int    colorIdx;  // index into kDVDPalette
    int    paletteN;  // palette size (kDVDPaletteCount; injected for the test shim)
    double flash;     // 0..1 corner-flash intensity (1 = just hit a corner)
} DVDState;

static void dvd_step(DVDState *s, double boundsW, double boundsH) {
    s->x += s->vx;
    s->y += s->vy;

    int bounceX = 0, bounceY = 0;
    if (s->x < 0.0) {
        s->x = 0.0;
        s->vx = fabs(s->vx);
        bounceX = 1;
    } else if (s->x + s->w > boundsW) {
        s->x = boundsW - s->w;
        s->vx = -fabs(s->vx);
        bounceX = 1;
    }
    if (s->y < 0.0) {
        s->y = 0.0;
        s->vy = fabs(s->vy);
        bounceY = 1;
    } else if (s->y + s->h > boundsH) {
        s->y = boundsH - s->h;
        s->vy = -fabs(s->vy);
        bounceY = 1;
    }

    if ((bounceX || bounceY) && s->paletteN > 1) {
        s->colorIdx = (s->colorIdx + 1) % s->paletteN; // +1 (mod N) => always a new hue
    }
    if (bounceX && bounceY) {
        s->flash = 1.0;                 // exact corner: arm the flash
    } else if (s->flash > 0.0) {
        s->flash -= kDVDFlashDecay;     // otherwise fade any prior flash out
        if (s->flash < 0.0) s->flash = 0.0;
    }
}

// CGDataProvider release callback: frees the mask byte buffer when the CGImage
// mask that owns it is released (in dealloc).
static void dvdReleaseMaskData(void *info, const void *data, size_t size) {
    (void)info;
    (void)size;
    free((void *)data);
}

@implementation DVDView {
    NSTimer   *_timer;      // ~30 FPS driver; nil when stopped
    DVDState   _st;         // motion state (stepped by dvd_step)
    CGSize     _logoSize;   // logo box (points) — derived from bounds width
    CGImageRef _logoMask;   // DVD-VIDEO silhouette as an image mask (0 = paint)
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor];
        [self buildMask];
        [self computeLogoSize];
        [self seedMotion];
    }
    return self;
}

- (BOOL)isOpaque { return YES; }

// --- Build the logo mask ONCE from the compiled-in PNG ----------------------
//
// Decodes kDVDLogoPNG and normalizes it into a clean 8-bit gray buffer (whatever
// format the PNG decoded to, re-rendered to single-channel), then wraps it as a
// CGImage image mask where sample 0 = paint. Size-independent: built once, drawn
// scaled. Orientation: drawing the decoded image into an un-modified bitmap
// context and reading the same buffer back through the CGImage family
// (CGImageMaskCreate) yields it upright — the canonical resize/redraw round-trip,
// no CTM flip. The final draw in a non-flipped view (isFlipped == NO) keeps it
// upright too. (If the logo ever renders upside down, that pairing is the place
// to look.)
- (void)buildMask {
    if (_logoMask) { CGImageRelease(_logoMask); _logoMask = NULL; }

    NSData *png = [NSData dataWithBytesNoCopy:(void *)kDVDLogoPNG
                                       length:kDVDLogoPNGLen
                                 freeWhenDone:NO];
    NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:png];
    CGImageRef decoded = [rep CGImage];
    if (decoded == NULL) { return; }

    size_t w = (size_t)kDVDLogoPNGWidth;
    size_t h = (size_t)kDVDLogoPNGHeight;
    unsigned char *buf = (unsigned char *)malloc(w * h);
    if (buf == NULL) { return; }

    CGColorSpaceRef gray = CGColorSpaceCreateDeviceGray();
    CGContextRef bctx = CGBitmapContextCreate(buf, w, h, 8, w, gray, (CGBitmapInfo)kCGImageAlphaNone);
    CGColorSpaceRelease(gray);
    if (bctx == NULL) { free(buf); return; }

    CGContextDrawImage(bctx, CGRectMake(0.0, 0.0, (CGFloat)w, (CGFloat)h), decoded);
    CGContextRelease(bctx);

    CGDataProviderRef prov =
        CGDataProviderCreateWithData(NULL, buf, w * h, dvdReleaseMaskData);
    if (prov == NULL) { free(buf); return; }
    // Image mask: sample 0 paints the fill color, 255 skips; intermediate values
    // are partial coverage (anti-aliased edges survive).
    _logoMask = CGImageMaskCreate(w, h, 8, 8, w, prov, NULL, false);
    CGDataProviderRelease(prov);
}

// Logo draw size: a fraction of the screen width, height from the mask's native
// aspect ratio (so the logo is never distorted). Recomputed on init + resize.
- (void)computeLogoSize {
    NSRect b = [self bounds];
    CGFloat W = fmax(kDVDMinLogoWidth, b.size.width * kDVDLogoWidthFrac);
    CGFloat aspect = (CGFloat)kDVDLogoPNGWidth / (CGFloat)kDVDLogoPNGHeight;
    CGFloat H = W / aspect;
    _logoSize = CGSizeMake(W, H);
    _st.w = W;
    _st.h = H;
}

// Random start position (inside bounds) + fixed-magnitude diagonal velocity with
// a random sign per axis + random starting color. Called on init and on resize.
- (void)seedMotion {
    NSRect b = [self bounds];
    CGFloat freeW = fmax(0.0, b.size.width  - _logoSize.width);
    CGFloat freeH = fmax(0.0, b.size.height - _logoSize.height);
    _st.x = (freeW > 0.0) ? (double)arc4random_uniform((uint32_t)freeW) : 0.0;
    _st.y = (freeH > 0.0) ? (double)arc4random_uniform((uint32_t)freeH) : 0.0;
    _st.vx = (arc4random_uniform(2) == 0) ? kDVDSpeed : -kDVDSpeed;
    _st.vy = (arc4random_uniform(2) == 0) ? kDVDSpeed : -kDVDSpeed;
    _st.colorIdx = (int)arc4random_uniform((uint32_t)kDVDPaletteCount);
    _st.paletteN = kDVDPaletteCount;
    _st.flash = 0.0;
}

- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self computeLogoSize]; // mask is size-independent; only the draw box changes
    [self seedMotion];
}

// --- Per-frame advance + draw ------------------------------------------------

- (void)step:(NSTimer *)t {
    (void)t;
    NSRect b = [self bounds];
    dvd_step(&_st, b.size.width, b.size.height);
    [self setNeedsDisplay:YES];
}

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    NSRect b = [self bounds];
    CGContextRef ctx = [[NSGraphicsContext currentContext] CGContext];

    // Opaque black base — pure #000000, no bleed-through (T-gh8-03).
    CGContextSetGrayFillColor(ctx, 0.0, 1.0);
    CGContextFillRect(ctx, b);

    if (_logoMask == NULL) { return; }

    // Current palette color, whitened by the corner-flash amount (lerp -> white).
    DVDRGB c = kDVDPalette[_st.colorIdx % kDVDPaletteCount];
    CGFloat f = _st.flash;
    CGFloat rr = c.r + (1.0 - c.r) * f;
    CGFloat gg = c.g + (1.0 - c.g) * f;
    CGFloat bb = c.b + (1.0 - c.b) * f;

    // Stencil the fill color through the logo mask at the current position. The
    // mask's 0-samples (the blue logo body) take the color; 255-samples (white
    // background + disc hole) are skipped, so the black base shows through.
    CGContextSetRGBFillColor(ctx, rr, gg, bb, 1.0);
    CGContextDrawImage(ctx, CGRectMake(_st.x, _st.y, _logoSize.width, _logoSize.height), _logoMask);
}

// --- Lifecycle: start/stop the FPS-capped timer with window attachment -------

- (void)startTimer {
    if (_timer != nil) return;
    _timer = [NSTimer timerWithTimeInterval:kDVDFrameInterval
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
    if (_logoMask) { CGImageRelease(_logoMask); _logoMask = NULL; }
    // ARC handles _timer's object ref.
}

@end

// --- Test-only shim: expose the pure physics to Go unit tests ----------------
//
// dvd_step is the one unit-testable piece of this view (edge reflection, color
// advance, corner-flash) — drawRect: output is owned by the WindowServer. cgo
// cannot call a static C function directly from a _test.go file, so this extern
// shim steps a DVDState built from the scalar args and writes the results back
// through out-pointers. Mirrors terminal_tokenize_for_test. Its Go wrapper is in
// dvdview_darwin.go.
void dvd_step_for_test(double x, double y, double vx, double vy,
                       double w, double h, int colorIdx, int paletteN, double flash,
                       double boundsW, double boundsH,
                       double *outX, double *outY, double *outVX, double *outVY,
                       int *outColorIdx, double *outFlash) {
    DVDState s;
    s.x = x; s.y = y; s.vx = vx; s.vy = vy;
    s.w = w; s.h = h; s.colorIdx = colorIdx; s.paletteN = paletteN; s.flash = flash;
    dvd_step(&s, boundsW, boundsH);
    if (outX)        *outX = s.x;
    if (outY)        *outY = s.y;
    if (outVX)       *outVX = s.vx;
    if (outVY)       *outVY = s.vy;
    if (outColorIdx) *outColorIdx = s.colorIdx;
    if (outFlash)    *outFlash = s.flash;
}
