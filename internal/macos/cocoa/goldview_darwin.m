// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // CALayer (opaque black backing, like MatrixView)
#import "goldview_darwin.h"        // shared @interface GoldView (also imported by window_darwin.m)
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// GoldView renders a candlestick chart of a steadily climbing gold price as the
// contentView of a shield overlay window when config `overlay_style: gold` is
// selected. It is a purely cosmetic content swap on top of the opaque shield
// NSWindow (window_darwin.m): the window keeps setOpaque:YES, this view's backing
// layer is opaque black, so the desktop can never bleed through (T-gh8-03). Every
// blocking guarantee (HID event tap, shield level) is byte-for-byte identical to
// black.
//
// The price is SYNTHETIC. Nothing is fetched (the project makes no network calls)
// and no real quote history is compiled in: a seeded random walk with a positive
// drift produces it on the fly, so each display draws its own chart and no two
// sessions look alike.
//
// Price model — pure + unit-tested. gold_step advances a GoldState by one frame:
// a per-frame log-return made of a small constant drift plus AR(1) noise (each
// frame's noise remembers most of the previous one), so the close glides instead
// of jittering, and reversals inside a candle leave natural wicks. After
// framesPerCandle frames the forming candle is committed to a ring and the next
// one opens at its close. drift and noise scale are derived from framesPerCandle,
// so the per-candle figures below hold for any candle much longer than the
// noise's ~16-frame memory.
//
// The drift is bounded on purpose: the axis is labeled, so a long session must
// still read as a gold price. ~+10% an hour is a clear climb across one screen
// width (~2.5% over the visible candles against ~0.9% of noise) and roughly x3
// over a 12-hour session.
//
// Rendering: the forming candle sits at the right edge and the chart slides left
// continuously as it forms (sub-point offsets, antialiased), so a newly committed
// candle never jumps. The price axis eases toward the range of the visible
// candles every frame. Grid lines land on 1/2/5 x 10^k price steps.
//
// Falling gold — pure + unit-tested too (gold_rain_step). Hundreds of small
// pixel-art coins fall behind everything else, with their own random state, so
// they never touch the price sequence. They are drawn first and shaded darker
// with distance rather than made translucent, so the chart and header always sit
// on top, legible. The sprite frames are baked into CGImages once; a frame then
// costs one image blit per coin (a few ms for ~600 coins on a Retina laptop).
//
// Cadence: FPS-capped ~30 via NSTimer in NSRunLoopCommonModes on the MAIN run
// loop, so all state + drawRect: is main-thread-safe (same contract as DVDView).
//
// Lifecycle — leak-free contract: start the timer in viewDidMoveToWindow
// (window != nil); stop it in viewWillMoveToWindow:nil AND dealloc (guarded
// against double-invalidate); free the price and rain state and release the
// gradients in dealloc.

#define GOLD_CAP 640 // committed candles kept; enough for a 3000-pt-wide chart at the minimum spacing

static const NSTimeInterval kGoldFrameInterval = 1.0 / 30.0; // ~30 FPS cap

static const int    kGoldFramesPerCandle = 240;    // 8 s per candle at 30 FPS
static const int    kGoldHistory         = 600;    // candles generated up front: the chart is full on the first frame
static const double kGoldDriftPerCandle  = 2.0e-4; // log-return drift per candle (~+10%/h)
static const double kGoldSigmaPerCandle  = 8.0e-4; // log-return spread per candle
static const double kGoldNoiseAR         = 0.94;   // per-frame noise persistence (smooths the path)

static const CGFloat kGoldTargetCandles = 130.0; // candles across the chart, before the spacing clamp
static const CGFloat kGoldMinSpacing    = 5.0;   // points per candle
static const CGFloat kGoldMaxSpacing    = 16.0;
static const CGFloat kGoldBodyFrac      = 0.64;  // candle body width as a fraction of the spacing
static const double  kGoldRangePad      = 0.12;  // axis headroom above and below the visible candles
static const double  kGoldRangeEase     = 0.06;  // per-frame step of the axis toward its target
static const int     kGoldGridLines     = 8;     // horizontal grid lines the step is chosen for
static const int     kGoldMAPeriod      = 20;    // moving-average length (candles)
static const int     kGoldVGridEvery    = 24;    // a vertical grid line every N candles

typedef struct { CGFloat r, g, b; } GoldRGB;
static const GoldRGB kGoldUp   = { 0.16, 0.80, 0.52 }; // rising candle
static const GoldRGB kGoldDown = { 0.94, 0.33, 0.31 }; // falling candle
static const GoldRGB kGoldMA   = { 0.96, 0.76, 0.28 }; // moving average, ticker

// --- Pure price model: Cocoa-free + unit-tested ------------------------------

typedef struct { double o, h, l, c; } GoldCandle;

typedef struct {
    GoldCandle ring[GOLD_CAP];   // committed candles; the oldest is overwritten first
    int        count;            // committed candles held (<= GOLD_CAP)
    int        head;             // ring slot the next committed candle goes to
    GoldCandle cur;              // the forming candle
    int        frame;            // frames elapsed in cur
    int        framesPerCandle;
    uint64_t   seq;              // candles ever committed (anchors the scrolling vertical grid)
    double     noise;            // AR(1) per-frame log-return noise
    double     drift, scale;     // per-frame drift and noise innovation, derived from framesPerCandle
    double     base;             // reference price for the header's change figure
    uint64_t   rng;              // xorshift64* state, never 0
} GoldState;

// xorshift64* — small, seedable, and the same sequence on every run, so the Go
// test can pin the model to a seed. The price model and the falling gold below
// each keep their own state, so the rain never shifts the price sequence.
static uint64_t gold_rand(uint64_t *state) {
    uint64_t x = *state;
    x ^= x >> 12;
    x ^= x << 25;
    x ^= x >> 27;
    *state = x;
    return x * 0x2545F4914F6CDD1DULL;
}

// Uniform in the OPEN interval (0,1): 53 random bits plus half a step, so log()
// below never sees 0.
static double gold_uniform(uint64_t *state) {
    return ((double)(gold_rand(state) >> 11) + 0.5) * (1.0 / 9007199254740992.0);
}

// Standard normal via Box-Muller (one value of the pair is enough here).
static double gold_gauss(GoldState *s) {
    double u1 = gold_uniform(&s->rng);
    double u2 = gold_uniform(&s->rng);
    return sqrt(-2.0 * log(u1)) * cos(2.0 * M_PI * u2);
}

static void gold_init(GoldState *s, uint64_t seed, double price, int framesPerCandle) {
    memset(s, 0, sizeof(*s));
    s->rng = seed != 0 ? seed : 0x9E3779B97F4A7C15ULL;
    s->framesPerCandle = framesPerCandle > 0 ? framesPerCandle : 1;
    double f = (double)s->framesPerCandle;
    s->drift = kGoldDriftPerCandle / f;
    // The sum of F AR(1) terms with innovation b has a spread of about
    // sqrt(F) * b / (1 - a), so this b gives one candle kGoldSigmaPerCandle.
    s->scale = kGoldSigmaPerCandle * (1.0 - kGoldNoiseAR) / sqrt(f);
    s->cur.o = s->cur.h = s->cur.l = s->cur.c = price;
    s->base = price;
}

static void gold_step(GoldState *s) {
    s->noise = kGoldNoiseAR * s->noise + s->scale * gold_gauss(s);
    double p = s->cur.c * exp(s->drift + s->noise);
    s->cur.c = p;
    if (p > s->cur.h) s->cur.h = p;
    if (p < s->cur.l) s->cur.l = p;
    if (++s->frame >= s->framesPerCandle) {
        s->ring[s->head] = s->cur;
        s->head = (s->head + 1) % GOLD_CAP;
        if (s->count < GOLD_CAP) s->count++;
        s->seq++;
        s->cur.o = s->cur.h = s->cur.l = p; // the next candle opens at this close
        s->frame = 0;
    }
}

// gold_at returns the committed candle `age` candles back (0 = most recent);
// age must be in [0, count).
static GoldCandle gold_at(const GoldState *s, int age) {
    int i = (s->head - 1 - age) % GOLD_CAP;
    if (i < 0) i += GOLD_CAP;
    return s->ring[i];
}

// gold_fit_range is the price range the axis eases toward: the low/high of the
// forming candle and the `visible` most recent committed ones, padded by `pad`
// of the span on each side. A flat span is widened so the axis never collapses.
static void gold_fit_range(const GoldState *s, int visible, double pad, double *lo, double *hi) {
    double l = s->cur.l, h = s->cur.h;
    int n = visible < s->count ? visible : s->count;
    for (int a = 0; a < n; a++) {
        GoldCandle c = gold_at(s, a);
        if (c.l < l) l = c.l;
        if (c.h > h) h = c.h;
    }
    double span = h - l;
    double minSpan = h * 1e-4;
    if (span < minSpan) {
        l -= (minSpan - span) / 2.0;
        h += (minSpan - span) / 2.0;
        span = minSpan;
    }
    *lo = l - span * pad;
    *hi = h + span * pad;
}

// gold_nice_step picks the grid step: the 1/2/5 x 10^k value at or above
// range/lines, so labels are round prices.
static double gold_nice_step(double range, int lines) {
    if (!(range > 0.0) || lines < 1) return 1.0;
    double raw = range / (double)lines;
    double mag = pow(10.0, floor(log10(raw)));
    double f = raw / mag;
    double nice = (f <= 1.0) ? 1.0 : (f <= 2.0) ? 2.0 : (f <= 5.0) ? 5.0 : 10.0;
    return nice * mag;
}

// gold_format_price writes p with two decimals and comma-grouped thousands
// ("4,182.37") into buf.
static void gold_format_price(double p, char *buf, size_t n) {
    char raw[48];
    char out[128];
    snprintf(raw, sizeof raw, "%.2f", p);
    const char *d = raw;
    size_t o = 0;
    if (*d == '-') out[o++] = *d++;
    const char *dot = strchr(d, '.');
    size_t digits = dot ? (size_t)(dot - d) : strlen(d);
    for (size_t i = 0; i < digits; i++) {
        if (i > 0 && (digits - i) % 3 == 0) out[o++] = ',';
        out[o++] = d[i];
    }
    out[o] = '\0';
    if (dot) strncat(out, dot, sizeof(out) - o - 1);
    snprintf(buf, n, "%s", out);
}

// --- Pixel-art coin sprite --------------------------------------------------
//
// One 12x12 coin, hand-placed pixel by pixel: an 8-frame spin about the vertical
// axis (face, three narrowing turns, edge-on, back, and the turns mirrored), five
// alternative faces so the shower is not one coin repeated, and two frames of a
// glint. Each frame is 12 rows of 12 palette characters, '.' transparent. The art
// is compiled in: the binary carries no image files.

#define GOLD_COIN_PX     12
#define GOLD_COIN_SPIN   8   // frames 0..7: the spin cycle, frame 0 the plain face
#define GOLD_COIN_FACES  5   // frames 8..12: alternative faces that stand in for frame 0
#define GOLD_COIN_FRAMES 15  // plus frames 13, 14: the glint
#define GOLD_COIN_GLINT  (GOLD_COIN_SPIN + GOLD_COIN_FACES)

static const char kGoldCoinPalette[] = "odglhs";
static const GoldRGB kGoldCoinColors[] = {
    { 0.439, 0.263, 0.075 }, // o outline,
    { 0.596, 0.349, 0.078 }, // d dark,
    { 0.871, 0.639, 0.161 }, // g mid,
    { 0.965, 0.808, 0.349 }, // l light,
    { 1.000, 0.945, 0.651 }, // h highlight,
    { 1.000, 1.000, 0.902 }, // s sparkle
};

static const char *const kGoldCoinArt[GOLD_COIN_FRAMES] = {
    // spin0
    "....oooo...."
    "..oohhlloo.."
    ".ollgggggdo."
    ".olggddgggo."
    "ollgdlgdggdo"
    "olggdlgdggdo"
    "olggdggdggdo"
    "ogggdggdggdo"
    ".ogggddgggo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // spin1
    ".....oo....."
    "....ohlo...."
    "...olgdlo..."
    "...olgdlo..."
    "..olggddgo.."
    "..olglddgo.."
    "..olggddgo.."
    "..olggddgo.."
    "...oggddo..."
    "...oggddo..."
    "....oddo...."
    ".....oo.....",
    // spin2
    ".....oo....."
    "....ohlo...."
    "....oldo...."
    "....oldo...."
    "...olgdgo..."
    "...olgdgo..."
    "...oggddo..."
    "...oggddo..."
    "....oddo...."
    "....oddo...."
    "....oddo...."
    ".....oo.....",
    // spin3
    ".....oo....."
    ".....ho....."
    ".....lo....."
    ".....lo....."
    ".....lo....."
    ".....go....."
    ".....go....."
    ".....go....."
    ".....do....."
    ".....do....."
    ".....do....."
    ".....oo.....",
    // spin4
    "....oooo...."
    "..oohhlloo.."
    ".ollgggggdo."
    ".olgggggggo."
    "ollggddgggdo"
    "olggdlgdggdo"
    "olggdggdggdo"
    "oggggddgggdo"
    ".oggggggggo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // spin5
    ".....oo....."
    ".....oh....."
    ".....ol....."
    ".....ol....."
    ".....ol....."
    ".....og....."
    ".....og....."
    ".....og....."
    ".....od....."
    ".....od....."
    ".....od....."
    ".....oo.....",
    // spin6
    ".....oo....."
    "....olho...."
    "....odlo...."
    "....odlo...."
    "...ogdglo..."
    "...ogdglo..."
    "...oddggo..."
    "...oddggo..."
    "....oddo...."
    "....oddo...."
    "....oddo...."
    ".....oo.....",
    // spin7
    ".....oo....."
    "....olho...."
    "...oldglo..."
    "...oldglo..."
    "..ogddgglo.."
    "..ogddlglo.."
    "..ogddgglo.."
    "..ogddgglo.."
    "...oddggo..."
    "...oddggo..."
    "....oddo...."
    ".....oo.....",
    // face0
    "....oooo...."
    "..oohhlloo.."
    ".ollgdgggdo."
    ".olgddddggo."
    "olldgdggggdo"
    "olggdddgggdo"
    "olgggdgdggdo"
    "oggddddgggdo"
    ".ogggdggggo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // face1
    "....oooo...."
    "..oohhlloo.."
    ".ollgggggdo."
    ".olggddgggo."
    "ollggddgggdo"
    "olgddddddgdo"
    "olggddddggdo"
    "ogggddddggdo"
    ".ogddggddgo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // face2
    "....oooo...."
    "..oohhlloo.."
    ".ollgggggdo."
    ".oldgddgdgo."
    "olldgddgdgdo"
    "olgddddddgdo"
    "olggddddggdo"
    "ogggllllggdo"
    ".oggddddggo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // face3
    "....oooo...."
    "..oohhlloo.."
    ".ollgggggdo."
    ".olgddddggo."
    "ollgllllggdo"
    "olgddddddgdo"
    "olgllllllgdo"
    "ogggddddggdo"
    ".oggllllggo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // face4
    "....oooo...."
    "..oohhlloo.."
    ".ollgggggdo."
    ".olggddgggo."
    "olldgddgdgdo"
    "olggddddggdo"
    "olggddddggdo"
    "oggdgddgdgdo"
    ".ogggddgggo."
    ".oggggggddo."
    "..ooddddoo.."
    "....oooo....",
    // sparkle0
    "............"
    "............"
    ".....h......"
    ".....s......"
    "...hsssh...."
    ".....s......"
    ".....h......"
    "............"
    "............"
    "............"
    "............"
    "............",
    // sparkle1
    "............"
    ".....h......"
    ".....s......"
    ".....s......"
    "..hsssssh..."
    ".....s......"
    ".....s......"
    ".....h......"
    "............"
    "............"
    "............"
    "............",
};

// --- Pure falling-gold model: Cocoa-free + unit-tested -----------------------
//
// Pixel coins fall behind the chart: a fixed population sized to the screen
// area, several hundred on a laptop, each with a depth in [0,1). Depth is
// bucketed into GOLD_SHADES layers: nearer layers are brighter and fall faster,
// and the near half is drawn at twice the pixel size, which is what gives the
// shower its parallax. A coin that leaves the bottom (or drifts off a side)
// respawns above the top edge, out of sight, as a new random coin, and
// accelerates to its terminal speed. Coordinates are view points, y up, so
// falling is y decreasing.

#define GOLD_RAIN_CAP 900
#define GOLD_SHADES   4

static const double kGoldRainAreaPerDrop = 2400.0; // pt^2 of screen per coin
static const int    kGoldRainMinDrops    = 12;
static const double kGoldRainFallFar     = 8.0;    // seconds to cross the screen, farthest coin
static const double kGoldRainFallNear    = 3.5;    // and nearest
static const double kGoldRainFPS         = 30.0;   // frames per second the speeds are given for
static const double kGoldRainGlintShare  = 0.2;    // coins that glint when they face the screen

typedef struct {
    double   x, y;       // center
    double   vy, vmax;   // fall speed and terminal speed, pt per frame
    double   vx;         // sideways drift, pt per frame
    double   depth;      // 0 far .. 1 near
    double   spin, spinRate; // spin phase and its step, radians
    int      scale;      // points per art pixel
    int      variant;    // 0 = plain face, 1..GOLD_COIN_FACES = an alternative face
    int      glint;      // glints when facing the screen
    unsigned spawns;     // respawns since init
} GoldDrop;

typedef struct {
    GoldDrop d[GOLD_RAIN_CAP];
    int      count;
    int      pixel;      // points per art pixel for the far half (the near half doubles it)
    double   w, h;       // view size
    uint64_t rng;        // xorshift64* state, never 0
} GoldRain;

static int gold_drop_shade(const GoldDrop *d) {
    int s = (int)(d->depth * GOLD_SHADES);
    return s < GOLD_SHADES ? s : GOLD_SHADES - 1;
}

// Half-extent of a coin: once its center is this far past an edge, nothing of it
// is visible.
static double gold_drop_reach(const GoldDrop *d) {
    return 0.6 * GOLD_COIN_PX * d->scale;
}

// gold_drop_frame is the sprite frame a coin shows now: its spin phase picks one
// of the GOLD_COIN_SPIN frames, and the face frame is swapped for its variant's.
static int gold_drop_frame(const GoldDrop *d) {
    double turns = d->spin / (2.0 * M_PI);
    int f = (int)floor((turns - floor(turns)) * GOLD_COIN_SPIN) % GOLD_COIN_SPIN;
    if (f == 0 && d->variant > 0) return GOLD_COIN_SPIN + d->variant - 1;
    return f;
}

static void gold_rain_spawn(GoldRain *r, GoldDrop *d) {
    d->depth = gold_uniform(&r->rng);
    d->scale = r->pixel * (gold_drop_shade(d) >= GOLD_SHADES / 2 ? 2 : 1);
    d->variant = (int)(gold_uniform(&r->rng) * (GOLD_COIN_FACES + 1));
    d->glint = gold_uniform(&r->rng) < kGoldRainGlintShare;
    double fall = kGoldRainFallFar + (kGoldRainFallNear - kGoldRainFallFar) * d->depth;
    d->vmax = r->h / (fall * kGoldRainFPS);
    d->vy = d->vmax * 0.15;
    d->vx = (2.0 * gold_uniform(&r->rng) - 1.0) * 0.2;
    d->x = gold_uniform(&r->rng) * r->w;
    d->y = r->h + gold_drop_reach(d) + gold_uniform(&r->rng) * r->h * 0.3;
    d->spin = gold_uniform(&r->rng) * 2.0 * M_PI;
    double dir = gold_uniform(&r->rng) < 0.5 ? -1.0 : 1.0;
    d->spinRate = dir * (0.12 + 0.18 * gold_uniform(&r->rng));
    d->spawns++;
}

// gold_rain_init fills a w x h view with coins already mid-fall, spread over the
// whole height at their terminal speed, so the shower is under way on the first
// frame. unit is the layout scale (H/900): a big display gets 2-pt art pixels.
static void gold_rain_init(GoldRain *r, uint64_t seed, double w, double h, double unit) {
    memset(r, 0, sizeof(*r));
    r->rng = seed != 0 ? seed : 0x9E3779B97F4A7C15ULL;
    r->w = w > 1.0 ? w : 1.0;
    r->h = h > 1.0 ? h : 1.0;
    r->pixel = unit >= 1.75 ? 2 : 1;
    int n = (int)lround(r->w * r->h / (kGoldRainAreaPerDrop * r->pixel * r->pixel));
    if (n < kGoldRainMinDrops) n = kGoldRainMinDrops;
    if (n > GOLD_RAIN_CAP) n = GOLD_RAIN_CAP;
    r->count = n;
    for (int i = 0; i < n; i++) {
        GoldDrop *d = &r->d[i];
        gold_rain_spawn(r, d);
        double reach = gold_drop_reach(d);
        d->y = gold_uniform(&r->rng) * (r->h + 2.0 * reach) - reach;
        d->vy = d->vmax;
        d->spawns = 0;
    }
}

static void gold_rain_step(GoldRain *r) {
    for (int i = 0; i < r->count; i++) {
        GoldDrop *d = &r->d[i];
        d->vy += d->vmax / kGoldRainFPS; // terminal speed in about a second
        if (d->vy > d->vmax) d->vy = d->vmax;
        d->y -= d->vy;
        d->x += d->vx;
        d->spin += d->spinRate;
        double reach = gold_drop_reach(d);
        if (d->y < -reach || d->x < -reach || d->x > r->w + reach) gold_rain_spawn(r, d);
    }
}

static NSColor *gold_color(GoldRGB c, CGFloat alpha) {
    return [NSColor colorWithSRGBRed:c.r green:c.g blue:c.b alpha:alpha];
}

// Coins are shaded by depth: far layers are darker. On the black base that
// reads as distance, and unlike alpha it keeps far coins from showing through
// near ones. Every frame is baked once per shade into a CGImage.
static const CGFloat kGoldShadeFar  = 0.38; // brightness of the farthest layer
static const CGFloat kGoldShadeNear = 0.90; // and of the nearest

static CGFloat gold_shade_level(int shade) {
    return kGoldShadeFar + (kGoldShadeNear - kGoldShadeFar) * (CGFloat)shade / (GOLD_SHADES - 1);
}

// gold_coin_image bakes one sprite frame, its colors scaled by `level`, into a
// 12x12 premultiplied RGBA image (row 0 at the top).
static CGImageRef gold_coin_image(int frame, CGFloat level) {
    uint8_t px[GOLD_COIN_PX * GOLD_COIN_PX * 4];
    memset(px, 0, sizeof px);
    const char *art = kGoldCoinArt[frame];
    for (int i = 0; i < GOLD_COIN_PX * GOLD_COIN_PX; i++) {
        const char *p = strchr(kGoldCoinPalette, art[i]);
        if (art[i] == '.' || p == NULL) continue;
        GoldRGB c = kGoldCoinColors[p - kGoldCoinPalette];
        px[4 * i + 0] = (uint8_t)lround(255.0 * c.r * level);
        px[4 * i + 1] = (uint8_t)lround(255.0 * c.g * level);
        px[4 * i + 2] = (uint8_t)lround(255.0 * c.b * level);
        px[4 * i + 3] = 255;
    }
    CFDataRef data = CFDataCreate(NULL, px, sizeof px);
    CGDataProviderRef prov = CGDataProviderCreateWithCFData(data);
    CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
    CGImageRef img = CGImageCreate(GOLD_COIN_PX, GOLD_COIN_PX, 8, 32, GOLD_COIN_PX * 4, cs,
                                   kCGImageAlphaPremultipliedLast | kCGBitmapByteOrder32Big,
                                   prov, NULL, false, kCGRenderingIntentDefault);
    CGColorSpaceRelease(cs);
    CGDataProviderRelease(prov);
    CFRelease(data);
    return img;
}

@implementation GoldView {
    NSTimer   *_timer;     // ~30 FPS driver; nil when stopped
    GoldState *_st;        // price model (heap: the ring is ~20 KB)
    GoldRain  *_rain;      // falling coins behind the chart
    uint64_t   _rainSeed;  // reused when a resize refills the rain
    CGImageRef _coin[GOLD_COIN_FRAMES][GOLD_SHADES]; // every sprite frame, baked per depth shade
    double     _lo, _hi;   // displayed price range, easing toward gold_fit_range
    NSRect     _chart;     // plot area (view points)
    CGFloat    _spacing;   // points per candle
    int        _visible;   // committed candles that can be on screen at once
    NSDictionary *_axisAttrs;   // price labels on the axis
    NSDictionary *_tagAttrs;    // current-price tag (dark text on the candle color)
    NSDictionary *_tickerAttrs; // "XAU/USD"
    NSDictionary *_nameAttrs;   // instrument name next to the ticker
    NSFont       *_priceFont;   // big header price
    NSFont       *_changeFont;  // header change figure
    NSShadow     *_headShadow;  // dark halo that keeps the header legible over falling gold
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor];
        _st = (GoldState *)calloc(1, sizeof(GoldState));
        _rain = (GoldRain *)calloc(1, sizeof(GoldRain));
        _rainSeed = ((uint64_t)arc4random() << 32) | (uint64_t)arc4random();
        for (int f = 0; f < GOLD_COIN_FRAMES; f++) {
            for (int sh = 0; sh < GOLD_SHADES; sh++) {
                // The glint is light, not metal: a step brighter than its coin.
                CGFloat level = gold_shade_level(sh);
                _coin[f][sh] = gold_coin_image(f, f >= GOLD_COIN_GLINT ? fmin(1.0, level + 0.1) : level);
            }
        }
        [self computeLayout];
        if (_st != NULL) {
            uint64_t seed = ((uint64_t)arc4random() << 32) | (uint64_t)arc4random();
            double start = 3900.0 + (double)arc4random_uniform(40000) / 100.0;
            gold_init(_st, seed, start, kGoldFramesPerCandle);
            for (int i = 0; i < kGoldHistory * kGoldFramesPerCandle; i++) {
                gold_step(_st);
            }
            // The header's change is measured from the left edge of the first
            // screen, so it starts modest and keeps climbing for the session.
            int age = _visible - 1;
            if (age >= _st->count) age = _st->count - 1;
            if (age >= 0) _st->base = gold_at(_st, age).o;
            [self fitRange:YES];
        }
    }
    return self;
}

- (BOOL)isOpaque { return YES; }

// Layout scales with the screen height so the chart reads the same on a laptop
// and on a large external display.
- (void)computeLayout {
    NSRect b = [self bounds];
    CGFloat W = b.size.width, H = b.size.height;
    CGFloat k = fmin(2.0, fmax(0.8, H / 900.0));

    CGFloat padLeft = fmax(24.0, W * 0.04);
    CGFloat axisW = 104.0 * k;
    CGFloat header = 120.0 * k;
    CGFloat padBottom = fmax(24.0, H * 0.06);
    _chart = NSMakeRect(padLeft, padBottom,
                        fmax(1.0, W - padLeft - axisW),
                        fmax(1.0, H - padBottom - header));
    _spacing = fmin(kGoldMaxSpacing, fmax(kGoldMinSpacing, _chart.size.width / kGoldTargetCandles));
    _visible = (int)ceil(_chart.size.width / _spacing) + 2;
    if (_visible > GOLD_CAP - kGoldMAPeriod) _visible = GOLD_CAP - kGoldMAPeriod;

    NSFont *axisFont = [NSFont monospacedDigitSystemFontOfSize:12.0 * k weight:NSFontWeightRegular];
    _axisAttrs = @{ NSFontAttributeName : axisFont,
                    NSForegroundColorAttributeName : [NSColor colorWithWhite:0.42 alpha:1.0] };
    _tagAttrs = @{ NSFontAttributeName : [NSFont monospacedDigitSystemFontOfSize:12.0 * k weight:NSFontWeightSemibold],
                   NSForegroundColorAttributeName : [NSColor blackColor] };
    _headShadow = [[NSShadow alloc] init];
    _headShadow.shadowColor = [NSColor colorWithWhite:0.0 alpha:0.9];
    _headShadow.shadowBlurRadius = 8.0 * k;
    _headShadow.shadowOffset = NSZeroSize;
    _tickerAttrs = @{ NSFontAttributeName : [NSFont systemFontOfSize:20.0 * k weight:NSFontWeightSemibold],
                      NSForegroundColorAttributeName : gold_color(kGoldMA, 1.0),
                      NSShadowAttributeName : _headShadow };
    _nameAttrs = @{ NSFontAttributeName : [NSFont systemFontOfSize:13.0 * k weight:NSFontWeightRegular],
                    NSForegroundColorAttributeName : [NSColor colorWithWhite:0.45 alpha:1.0],
                    NSShadowAttributeName : _headShadow };
    _priceFont = [NSFont monospacedDigitSystemFontOfSize:34.0 * k weight:NSFontWeightSemibold];
    _changeFont = [NSFont monospacedDigitSystemFontOfSize:17.0 * k weight:NSFontWeightMedium];

    if (_rain != NULL) gold_rain_init(_rain, _rainSeed, W, H, k);
}

// Moves the displayed range toward the visible candles (or jumps there on
// init/resize). The current price is always kept on the axis, even if the eased
// range lags behind a fast move.
- (void)fitRange:(BOOL)snap {
    double tlo, thi;
    gold_fit_range(_st, _visible, kGoldRangePad, &tlo, &thi);
    if (snap) {
        _lo = tlo;
        _hi = thi;
        return;
    }
    _lo += (tlo - _lo) * kGoldRangeEase;
    _hi += (thi - _hi) * kGoldRangeEase;
    if (_st->cur.h > _hi) _hi = _st->cur.h;
    if (_st->cur.l < _lo) _lo = _st->cur.l;
}

- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self computeLayout];
    if (_st != NULL) [self fitRange:YES];
}

// --- Per-frame advance + draw ------------------------------------------------

- (void)step:(NSTimer *)t {
    (void)t;
    if (_st == NULL) return;
    gold_step(_st);
    if (_rain != NULL) gold_rain_step(_rain);
    [self fitRange:NO];
    [self setNeedsDisplay:YES];
}

- (CGFloat)yForPrice:(double)p {
    return _chart.origin.y + (CGFloat)((p - _lo) / (_hi - _lo)) * _chart.size.height;
}

// Center x of a candle: age -1 is the forming one, 0 the most recent committed.
// Everything shifts left by `progress` of a spacing while the forming candle
// fills, which is what makes the scroll continuous.
- (CGFloat)xForAge:(int)age progress:(CGFloat)progress {
    return NSMaxX(_chart) - _spacing * (1.5 + (CGFloat)age + progress);
}

- (void)drawCandle:(GoldCandle)c atX:(CGFloat)x inContext:(CGContextRef)ctx {
    GoldRGB rgb = (c.c >= c.o) ? kGoldUp : kGoldDown;
    CGContextSetRGBFillColor(ctx, rgb.r, rgb.g, rgb.b, 1.0);
    CGContextSetRGBStrokeColor(ctx, rgb.r, rgb.g, rgb.b, 1.0);

    CGContextSetLineWidth(ctx, fmax(1.0, _spacing * 0.1));
    CGContextMoveToPoint(ctx, x, [self yForPrice:c.l]);
    CGContextAddLineToPoint(ctx, x, [self yForPrice:c.h]);
    CGContextStrokePath(ctx);

    CGFloat top = [self yForPrice:fmax(c.o, c.c)];
    CGFloat bot = [self yForPrice:fmin(c.o, c.c)];
    if (top - bot < 1.0) {
        CGFloat mid = (top + bot) / 2.0;
        top = mid + 0.5;
        bot = mid - 0.5;
    }
    CGFloat bw = fmax(1.0, _spacing * kGoldBodyFrac);
    CGContextFillRect(ctx, CGRectMake(x - bw / 2.0, bot, bw, top - bot));
}

// Far layers first, so nearer coins pass in front. Positions snap to whole
// points and every sprite is scaled by a whole number with no interpolation, so
// the pixels stay crisp.
- (void)drawRainInContext:(CGContextRef)ctx {
    if (_rain == NULL) return;
    CGContextSaveGState(ctx);
    CGContextSetInterpolationQuality(ctx, kCGInterpolationNone);
    CGContextSetShouldAntialias(ctx, false);
    for (int sh = 0; sh < GOLD_SHADES; sh++) {
        for (int i = 0; i < _rain->count; i++) {
            const GoldDrop *d = &_rain->d[i];
            if (gold_drop_shade(d) != sh) continue;
            CGFloat side = GOLD_COIN_PX * d->scale;
            CGRect rect = CGRectMake(floor(d->x - side / 2.0), floor(d->y - side / 2.0), side, side);
            int f = gold_drop_frame(d);
            CGContextDrawImage(ctx, rect, _coin[f][sh]);
            if (d->glint && (f == 0 || f >= GOLD_COIN_SPIN)) {
                CGContextDrawImage(ctx, rect, _coin[GOLD_COIN_GLINT + (int)(d->spawns & 1)][sh]);
            }
        }
    }
    CGContextRestoreGState(ctx);
}

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    NSRect b = [self bounds];
    CGContextRef ctx = [[NSGraphicsContext currentContext] CGContext];

    // Opaque black base — pure #000000, no bleed-through (T-gh8-03).
    CGContextSetGrayFillColor(ctx, 0.0, 1.0);
    CGContextFillRect(ctx, b);

    // The falling gold goes first, so the chart, axis and header all draw over it.
    [self drawRainInContext:ctx];

    if (_st == NULL || !(_hi > _lo)) return;

    const GoldState *s = _st;
    CGFloat progress = (CGFloat)s->frame / (CGFloat)s->framesPerCandle;
    int n = _visible < s->count ? _visible : s->count;
    char buf[64];

    // Live-price tag geometry first: the axis labels it would cover are skipped.
    GoldRGB live = (s->cur.c >= s->cur.o) ? kGoldUp : kGoldDown;
    CGFloat yLive = [self yForPrice:s->cur.c];
    gold_format_price(s->cur.c, buf, sizeof buf);
    NSString *livePrice = [NSString stringWithUTF8String:buf];
    NSSize tsz = [livePrice sizeWithAttributes:_tagAttrs];
    CGFloat tagH = tsz.height + 6.0;
    CGFloat tagY = fmin(NSMaxY(_chart) - tagH / 2.0, fmax(NSMinY(_chart) + tagH / 2.0, yLive));

    // Horizontal grid on round prices, labeled on the axis to the right. Lines
    // and labels go in separate passes: drawing text resets the context's
    // colors, so a stroke after a label would come out in the label's gray.
    double step = gold_nice_step(_hi - _lo, kGoldGridLines);
    double first = ceil(_lo / step) * step;
    CGContextSetLineWidth(ctx, 1.0);
    CGContextSetRGBStrokeColor(ctx, 1.0, 1.0, 1.0, 0.07);
    for (double v = first; v <= _hi; v += step) {
        CGFloat y = floor([self yForPrice:v]) + 0.5;
        CGContextMoveToPoint(ctx, NSMinX(_chart), y);
        CGContextAddLineToPoint(ctx, NSMaxX(_chart), y);
    }
    CGContextStrokePath(ctx);
    for (double v = first; v <= _hi; v += step) {
        CGFloat y = floor([self yForPrice:v]) + 0.5;
        if (fabs(y - tagY) < tagH) continue;
        gold_format_price(v, buf, sizeof buf);
        NSString *label = [NSString stringWithUTF8String:buf];
        NSSize sz = [label sizeWithAttributes:_axisAttrs];
        [label drawAtPoint:NSMakePoint(NSMaxX(_chart) + 12.0, y - sz.height / 2.0) withAttributes:_axisAttrs];
    }

    CGContextSaveGState(ctx);
    CGContextClipToRect(ctx, NSRectToCGRect(_chart));

    // Vertical grid, pinned to candles so it scrolls with them.
    CGContextSetRGBStrokeColor(ctx, 1.0, 1.0, 1.0, 0.05);
    for (int a = 0; a < n; a++) {
        if ((s->seq - 1 - (uint64_t)a) % kGoldVGridEvery != 0) continue;
        CGFloat x = [self xForAge:a progress:progress];
        CGContextMoveToPoint(ctx, x, NSMinY(_chart));
        CGContextAddLineToPoint(ctx, x, NSMaxY(_chart));
        CGContextStrokePath(ctx);
    }

    for (int a = n - 1; a >= 0; a--) {
        [self drawCandle:gold_at(s, a) atX:[self xForAge:a progress:progress] inContext:ctx];
    }
    [self drawCandle:s->cur atX:[self xForAge:-1 progress:progress] inContext:ctx];

    // Moving average of closes, oldest visible candle to the forming one, kept
    // as a running sum.
    int m = s->count - (kGoldMAPeriod - 1);
    if (m > n) m = n;
    if (m > 0) {
        double sum = 0.0;
        for (int a = m - 1; a < m - 1 + kGoldMAPeriod; a++) sum += gold_at(s, a).c;
        CGContextSetRGBStrokeColor(ctx, kGoldMA.r, kGoldMA.g, kGoldMA.b, 0.85);
        CGContextSetLineWidth(ctx, 1.6);
        CGContextSetLineJoin(ctx, kCGLineJoinRound);
        for (int a = m - 1; a >= 0; a--) {
            CGFloat x = [self xForAge:a progress:progress];
            CGFloat y = [self yForPrice:sum / kGoldMAPeriod];
            if (a == m - 1) CGContextMoveToPoint(ctx, x, y);
            else CGContextAddLineToPoint(ctx, x, y);
            if (a > 0) sum += gold_at(s, a - 1).c - gold_at(s, a + kGoldMAPeriod - 1).c;
        }
        // The forming candle's average swaps the oldest close for the live one.
        sum += s->cur.c - gold_at(s, kGoldMAPeriod - 1).c;
        CGContextAddLineToPoint(ctx, [self xForAge:-1 progress:progress], [self yForPrice:sum / kGoldMAPeriod]);
        CGContextStrokePath(ctx);
    }

    // Dashed line at the live price.
    CGFloat dash[2] = { 4.0, 4.0 };
    CGContextSetLineDash(ctx, 0.0, dash, 2);
    CGContextSetLineWidth(ctx, 1.0);
    CGContextSetRGBStrokeColor(ctx, live.r, live.g, live.b, 0.55);
    CGContextMoveToPoint(ctx, NSMinX(_chart), yLive);
    CGContextAddLineToPoint(ctx, NSMaxX(_chart), yLive);
    CGContextStrokePath(ctx);
    CGContextSetLineDash(ctx, 0.0, NULL, 0);

    CGContextRestoreGState(ctx);

    // Live-price tag on the axis.
    NSRect tagRect = NSMakeRect(NSMaxX(_chart) + 4.0, tagY - tagH / 2.0, tsz.width + 16.0, tagH);
    [gold_color(live, 1.0) setFill];
    [[NSBezierPath bezierPathWithRoundedRect:tagRect xRadius:3.0 yRadius:3.0] fill];
    [livePrice drawAtPoint:NSMakePoint(NSMinX(tagRect) + 8.0, NSMinY(tagRect) + 3.0) withAttributes:_tagAttrs];

    // Header: ticker and name, then the live price and its change since the
    // session's reference price.
    CGFloat headTop = NSMaxY(b) - (NSMaxY(b) - NSMaxY(_chart)) * 0.22;
    NSString *ticker = @"XAU/USD";
    NSSize ksz = [ticker sizeWithAttributes:_tickerAttrs];
    CGFloat row1 = headTop - ksz.height;
    [ticker drawAtPoint:NSMakePoint(NSMinX(_chart), row1) withAttributes:_tickerAttrs];
    NSString *name = @"Gold Spot · US Dollar";
    NSSize nsz = [name sizeWithAttributes:_nameAttrs];
    [name drawAtPoint:NSMakePoint(NSMinX(_chart) + ksz.width + 14.0, row1 + (ksz.height - nsz.height) / 2.0)
       withAttributes:_nameAttrs];

    double change = (s->base > 0.0) ? (s->cur.c / s->base - 1.0) * 100.0 : 0.0;
    GoldRGB trend = (change >= 0.0) ? kGoldUp : kGoldDown;
    NSDictionary *priceAttrs = @{ NSFontAttributeName : _priceFont,
                                  NSForegroundColorAttributeName : [NSColor colorWithWhite:0.94 alpha:1.0],
                                  NSShadowAttributeName : _headShadow };
    NSDictionary *changeAttrs = @{ NSFontAttributeName : _changeFont,
                                   NSForegroundColorAttributeName : gold_color(trend, 1.0),
                                   NSShadowAttributeName : _headShadow };
    NSSize psz = [livePrice sizeWithAttributes:priceAttrs];
    CGFloat row2 = row1 - psz.height - 2.0;
    [livePrice drawAtPoint:NSMakePoint(NSMinX(_chart), row2) withAttributes:priceAttrs];
    snprintf(buf, sizeof buf, "%s %+.2f%%", change >= 0.0 ? "▲" : "▼", change);
    NSString *chg = [NSString stringWithUTF8String:buf];
    NSSize csz = [chg sizeWithAttributes:changeAttrs];
    [chg drawAtPoint:NSMakePoint(NSMinX(_chart) + psz.width + 16.0, row2 + (psz.height - csz.height) * 0.3)
      withAttributes:changeAttrs];
}

// --- Lifecycle: start/stop the FPS-capped timer with window attachment -------

- (void)startTimer {
    if (_timer != nil) return;
    _timer = [NSTimer timerWithTimeInterval:kGoldFrameInterval
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
    free(_st);
    _st = NULL;
    free(_rain);
    _rain = NULL;
    for (int f = 0; f < GOLD_COIN_FRAMES; f++) {
        for (int sh = 0; sh < GOLD_SHADES; sh++) CGImageRelease(_coin[f][sh]);
    }
    // ARC handles _timer's object ref.
}

@end

// --- Test-only shims: expose the pure price model to Go unit tests -----------
//
// gold_step and its helpers are the unit-testable piece of this view — drawRect:
// output is owned by the WindowServer. cgo cannot call a static C function from
// a _test.go file, so these extern shims wrap them. Mirrors dvd_step_for_test;
// the Go wrappers are in goldview_darwin.go.

int gold_cap_for_test(void) { return GOLD_CAP; }

// gold_sim_for_test runs a fresh model from (seed, price) for `frames` frames
// with framesPerCandle frames per candle, then copies the committed candles,
// oldest first, into the out arrays (up to maxOut), plus the forming candle, the
// candle counter and the axis range fitted to the `visible` most recent candles.
void gold_sim_for_test(unsigned long long seed, double price, int framesPerCandle, int frames,
                       int visible, double pad,
                       double *outO, double *outH, double *outL, double *outC, int maxOut,
                       int *outCount, unsigned long long *outSeq,
                       double *outCur /* o, h, l, c */, double *outLo, double *outHi) {
    GoldState *s = (GoldState *)calloc(1, sizeof(GoldState));
    if (s == NULL) {
        if (outCount) *outCount = 0;
        return;
    }
    gold_init(s, (uint64_t)seed, price, framesPerCandle);
    for (int i = 0; i < frames; i++) gold_step(s);

    int n = s->count < maxOut ? s->count : maxOut;
    for (int i = 0; i < n; i++) {
        GoldCandle c = gold_at(s, n - 1 - i);
        outO[i] = c.o;
        outH[i] = c.h;
        outL[i] = c.l;
        outC[i] = c.c;
    }
    if (outCount) *outCount = n;
    if (outSeq) *outSeq = (unsigned long long)s->seq;
    if (outCur) {
        outCur[0] = s->cur.o;
        outCur[1] = s->cur.h;
        outCur[2] = s->cur.l;
        outCur[3] = s->cur.c;
    }
    double lo = 0.0, hi = 0.0;
    gold_fit_range(s, visible, pad, &lo, &hi);
    if (outLo) *outLo = lo;
    if (outHi) *outHi = hi;
    free(s);
}

double gold_nice_step_for_test(double range, int lines) { return gold_nice_step(range, lines); }

void gold_format_price_for_test(double p, char *buf, int n) {
    if (n > 0) gold_format_price(p, buf, (size_t)n);
}

// The falling-gold model is stepped from Go one frame at a time, so the tests
// can check every frame (each coin only falls until it respawns), not just the
// end state. The handle is a heap GoldRain; the Go wrapper frees it.

int gold_rain_cap_for_test(void) { return GOLD_RAIN_CAP; }

void *gold_rain_new_for_test(unsigned long long seed, double w, double h, double unit) {
    GoldRain *r = (GoldRain *)calloc(1, sizeof(GoldRain));
    if (r != NULL) gold_rain_init(r, (uint64_t)seed, w, h, unit);
    return r;
}

void gold_rain_step_for_test(void *r) {
    if (r != NULL) gold_rain_step((GoldRain *)r);
}

// gold_rain_read_for_test copies each coin's fields into the out arrays (up to
// maxOut) and returns how many coins the rain holds. outSize is the sprite's
// side in points; outFrame the sprite frame it shows now.
int gold_rain_read_for_test(void *rp, double *outX, double *outY, double *outVY, double *outSize,
                            double *outDepth, int *outVariant, int *outFrame, unsigned *outSpawns, int maxOut) {
    GoldRain *r = (GoldRain *)rp;
    if (r == NULL) return 0;
    int n = r->count < maxOut ? r->count : maxOut;
    for (int i = 0; i < n; i++) {
        const GoldDrop *d = &r->d[i];
        outX[i] = d->x;
        outY[i] = d->y;
        outVY[i] = d->vy;
        outSize[i] = GOLD_COIN_PX * d->scale;
        outDepth[i] = d->depth;
        outVariant[i] = d->variant;
        outFrame[i] = gold_drop_frame(d);
        outSpawns[i] = d->spawns;
    }
    return n;
}

void gold_rain_free_for_test(void *r) { free(r); }

// gold_coin_art_for_test copies sprite frame `frame` (GOLD_COIN_PX^2 palette
// characters, row 0 at the top) into buf and returns the frame count; with a
// frame out of range it only returns the count. palette receives the palette
// characters.
int gold_coin_art_for_test(int frame, char *buf, int n, char *palette, int pn) {
    if (palette != NULL && pn > 0) snprintf(palette, (size_t)pn, "%s", kGoldCoinPalette);
    if (frame >= 0 && frame < GOLD_COIN_FRAMES && buf != NULL && n > 0) {
        snprintf(buf, (size_t)n, "%s", kGoldCoinArt[frame]);
    }
    return GOLD_COIN_FRAMES;
}
