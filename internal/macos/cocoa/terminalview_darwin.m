// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // available via -framework QuartzCore (CALayer)
#import "terminalview_darwin.h"    // shared @interface TerminalView (also imported by window_darwin.m)
#include <string.h>                // strlen for corpus-line lengths

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

// Line box height as a multiple of the font size (row-to-row advance).
static const CGFloat kTermLineHeightFactor = 1.35;

// Typing speed for the bottom line, in characters per frame. Randomized per line
// within [min, min+span) so each line types at a slightly different cadence,
// reading like a human/agent typing rather than a metronome.
static const CGFloat kTermTypeSpeedMin  = 0.9; // ~27 chars/s at 30 FPS
static const CGFloat kTermTypeSpeedSpan = 1.3; // up to ~66 chars/s

// PAUSE hold after a line finishes typing, in frames: a base plus per-line jitter
// so the cadence between lines is not perfectly uniform. (base .. base+jitter)
static const NSInteger kTermPauseFramesBase   = 10; // ~0.33 s
static const NSInteger kTermPauseFramesJitter = 12; // + up to ~0.40 s

// Synthetic source corpus (fully compiled-in — no real filesystem, git, or system
// data is ever read; see the security note in the plan). ASCII only, so tokenizing
// and monospaced cell widths stay trivial. A Go/C mix flavored with a few
// dndmode-themed lines so the screen reads like a real project at work. Lines are
// kept short enough to fit typical widths; anything wider is bounds-clipped in
// drawRect: (no soft-wrap in v1). Blank "" lines are intentional breathing room.
static const char *kCorpus[] = {
    "package dndmode",
    "",
    "import (",
    "    \"context\"",
    "    \"os\"",
    "    \"os/signal\"",
    "    \"syscall\"",
    ")",
    "",
    "// shieldController keeps every attached display covered.",
    "type shieldController struct {",
    "    windows []*overlayWindow",
    "    tap     *eventTap",
    "    assert  IOPMAssertionID",
    "    style   string",
    "}",
    "",
    "// newController builds the shield for the given overlay style.",
    "func newController(style string) *shieldController {",
    "    return &shieldController{style: style}",
    "}",
    "",
    "func (c *shieldController) Start(ctx context.Context) error {",
    "    if err := c.preventSleep(); err != nil {",
    "        return err",
    "    }",
    "    for _, d := range attachedDisplays() {",
    "        w := createOverlayWindow(d, c.style)",
    "        c.windows = append(c.windows, w)",
    "    }",
    "    return c.tap.Install()",
    "}",
    "",
    "// preventSleep pins an IOPMAssertion for the whole session.",
    "func (c *shieldController) preventSleep() error {",
    "    const name = \"dndmode.active\"",
    "    id, err := pmAssertionCreate(name)",
    "    if err != nil {",
    "        return err",
    "    }",
    "    c.assert = id",
    "    return nil",
    "}",
    "",
    "// onKey swallows every event until the unlock hotkey matches.",
    "func (c *shieldController) onKey(ev *cgEvent) bool {",
    "    if c.hotkey.Matches(ev) {",
    "        c.unlock()",
    "        return false",
    "    }",
    "    return true // stay silent on wrong input",
    "}",
    "",
    "static int shield_level(void) {",
    "    return CGShieldingWindowLevel();",
    "}",
    "",
    "// collection behavior: cover every Space and full-screen app",
    "static const NSUInteger kShieldBehavior =",
    "    NSWindowCollectionBehaviorCanJoinAllSpaces |",
    "    NSWindowCollectionBehaviorStationary |",
    "    NSWindowCollectionBehaviorFullScreenAuxiliary;",
    "",
    "for i := 0; i < len(displays); i++ {",
    "    reconcile(displays[i])",
    "}",
    "",
    "var caret = 0x2588 // full block glyph for the cursor",
    "const frameInterval = 1.0 / 30.0",
    "",
    "func main() { os.Exit(run()) }",
};
static const NSInteger kTermCorpusCount = (NSInteger)(sizeof(kCorpus) / sizeof(kCorpus[0]));

// One visible line: a RAW pointer into the static corpus (no ownership) plus its
// cached byte length. Task 4 extends this record with tokenized colored segments.
typedef struct {
    const char *text;   // -> kCorpus[i]; static storage, never freed
    NSInteger   length; // strlen(text), cached on load
} TermLine;

// Typing state machine for the bottom (active) line.
typedef enum {
    TermPhaseTyping = 0, // revealing characters of the bottom line one by one
    TermPhasePause,      // brief hold after the line finishes typing
    TermPhaseScroll,     // jump-scroll up by one, pull the next corpus line
} TermPhase;

@implementation TerminalView {
    NSTimer   *_timer;        // ~30 FPS driver; nil when stopped
    NSFont    *_font;         // cached monospaced font (built once in initWithFrame:)
    CGFloat    _cellW;        // monospaced advance width (points), from _font
    CGFloat    _cellH;        // row-to-row height (points)
    TermLine  *_lines;        // visible-line ring, top->bottom; sized to view height
    NSInteger  _lineCount;    // number of visible lines (rows) that fit the height
    NSInteger  _cursor;       // next corpus index to pull into the bottom slot
    CGFloat    _visibleChars; // chars revealed on the bottom line (fractional)
    CGFloat    _typeSpeed;    // current bottom line's typing speed (chars/frame)
    TermPhase  _phase;        // TYPING / PAUSE / SCROLL
    NSInteger  _pauseFrames;  // remaining PAUSE frames (counts down)
    NSInteger  _blink;        // caret blink phase counter (drawn in Task 4)
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor]; // opaque #000000 backing
        [self buildFont];
        [self rebuildBuffer];
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

// --- Visible-line buffer: (re)allocated on init + every size change ---

// A fresh typing speed for a newly started bottom line (chars/frame).
- (CGFloat)nextTypeSpeed {
    return kTermTypeSpeedMin +
           (CGFloat)arc4random_uniform(1000) / 1000.0 * kTermTypeSpeedSpan;
}

// A fresh PAUSE hold (frames) for the line that just finished typing.
- (NSInteger)nextPauseFrames {
    return kTermPauseFramesBase +
           (NSInteger)arc4random_uniform((uint32_t)kTermPauseFramesJitter + 1);
}

// loadLine fills a visible-line slot with a RAW corpus line. The text pointer aims
// into the static kCorpus (no ownership, never freed). Centralizing corpus loading
// here means Task 4 attaches tokenized segments in ONE place, covering BOTH entry
// paths (the initial rebuildBuffer fill and the SCROLL branch of step:).
- (void)loadLine:(TermLine *)line fromCorpus:(NSInteger)index {
    const char *text = kCorpus[index];
    line->text   = text;
    line->length = (NSInteger)strlen(text);
}

- (void)rebuildBuffer {
    NSRect b = [self bounds];

    free(_lines);
    _lines = NULL;
    _lineCount = 0;

    // Monospaced cell metrics: width from the font's advance, height from the font
    // size times the line factor. Both feed drawRect: (Task 4) and row sizing here.
    _cellW = [@"M" sizeWithAttributes:@{ NSFontAttributeName: _font }].width;
    if (_cellW < 1.0) { _cellW = kTermFontSize * 0.6; } // defensive floor
    _cellH = ceil(kTermFontSize * kTermLineHeightFactor);

    NSInteger rows = (NSInteger)floor(b.size.height / _cellH);
    if (rows < 1) { rows = 1; }
    _lineCount = rows;

    _lines = (TermLine *)calloc((size_t)_lineCount, sizeof(TermLine));

    // Fill the screen immediately with sequential corpus lines so startup is not a
    // blank pane; the bottom line then types itself out from empty.
    _cursor = 0;
    for (NSInteger i = 0; i < _lineCount; i++) {
        [self loadLine:&_lines[i] fromCorpus:_cursor];
        _cursor = (_cursor + 1) % kTermCorpusCount;
    }

    _phase        = TermPhaseTyping;
    _visibleChars = 0.0;
    _typeSpeed    = [self nextTypeSpeed];
    _pauseFrames  = 0;
    _blink        = 0;
}

- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self rebuildBuffer];
}

- (void)resizeSubviewsWithOldSize:(NSSize)oldSize {
    [super resizeSubviewsWithOldSize:oldSize];
    [self rebuildBuffer];
}

// Jump-scroll the buffer up by one line: the top line falls off, every other line
// shifts up, and the next corpus line drops into the freed bottom slot and starts
// retyping from empty. (Task 4 frees the top line's segments HERE, before the
// shift, to avoid leaking them; the raw-pointer TermLine of Task 3 owns nothing.)
- (void)scrollUp {
    if (_lines == NULL || _lineCount <= 0) { return; }
    for (NSInteger i = 0; i < _lineCount - 1; i++) {
        _lines[i] = _lines[i + 1];
    }
    [self loadLine:&_lines[_lineCount - 1] fromCorpus:_cursor];
    _cursor = (_cursor + 1) % kTermCorpusCount;
    _visibleChars = 0.0;
    _typeSpeed    = [self nextTypeSpeed];
}

// --- Per-frame advance: type the bottom line, pause, then jump-scroll up ---

- (void)step:(NSTimer *)t {
    (void)t;
    _blink++; // caret blink phase (rendered in Task 4)

    if (_lineCount > 0) {
        switch (_phase) {
            case TermPhaseTyping: {
                NSInteger bottomLen = _lines[_lineCount - 1].length;
                _visibleChars += _typeSpeed;
                if (_visibleChars >= (CGFloat)bottomLen) {
                    _visibleChars = (CGFloat)bottomLen; // clamp — never over-type
                    _phase = TermPhasePause;
                    _pauseFrames = [self nextPauseFrames];
                }
                break;
            }
            case TermPhasePause:
                if (--_pauseFrames <= 0) {
                    _phase = TermPhaseScroll;
                }
                break;
            case TermPhaseScroll:
                [self scrollUp];
                _phase = TermPhaseTyping;
                break;
        }
    }

    [self setNeedsDisplay:YES];
}

// --- Drawing: placeholder opaque-black fill (full source render lands in Task 4) ---

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    [[NSColor blackColor] setFill]; // pure #000000, fully opaque
    NSRectFill([self bounds]);
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
    free(_lines);
    _lines = NULL;
    // ARC handles _font / _timer object refs.
}

@end
