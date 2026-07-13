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
// and monospaced cell widths stay trivial. A Go/C mix around a generic worker-pool
// theme so the screen reads like a real project at work. Deliberately anonymous:
// no line may name this tool or hint at input interception, hotkeys, or unlocking —
// the shield must not tell a bystander that keystrokes are being watched (the
// security stance and README's "leaks no signal" claim both depend on it). Lines
// are kept short enough to fit typical widths; anything wider is bounds-clipped in
// drawRect: (no soft-wrap in v1). Blank "" lines are intentional breathing room.
static const char *kCorpus[] = {
    "package workerpool",
    "",
    "import (",
    "    \"context\"",
    "    \"os\"",
    "    \"os/signal\"",
    "    \"syscall\"",
    ")",
    "",
    "// poolRunner drains the job queue with a bounded worker set.",
    "type poolRunner struct {",
    "    jobs    []*queuedJob",
    "    limiter *rateLimiter",
    "    index   *shardIndex",
    "    name    string",
    "}",
    "",
    "// newRunner builds a pool runner for the given queue name.",
    "func newRunner(name string) *poolRunner {",
    "    return &poolRunner{name: name}",
    "}",
    "",
    "func (p *poolRunner) Start(ctx context.Context) error {",
    "    if err := p.warmCache(); err != nil {",
    "        return err",
    "    }",
    "    for _, j := range pendingJobs() {",
    "        go p.runJob(ctx, j)",
    "        p.jobs = append(p.jobs, j)",
    "    }",
    "    return p.limiter.Start()",
    "}",
    "",
    "// warmCache preloads the shard index before the first job lands.",
    "func (p *poolRunner) warmCache() error {",
    "    const name = \"cache.warm\"",
    "    idx, err := loadIndex(name)",
    "    if err != nil {",
    "        return err",
    "    }",
    "    p.index = idx",
    "    return nil",
    "}",
    "",
    "// nextBackoff doubles the retry delay up to the configured ceiling.",
    "func (p *poolRunner) nextBackoff(d time.Duration) time.Duration {",
    "    if d*2 > p.maxDelay {",
    "        return p.maxDelay",
    "    }",
    "    return d * 2 // exponential, capped",
    "}",
    "",
    "static int ring_mask(void) {",
    "    return RING_CAPACITY - 1;",
    "}",
    "",
    "// poll flags: readable, writable, and error conditions",
    "static const unsigned kPollFlags =",
    "    POLL_READABLE |",
    "    POLL_WRITABLE |",
    "    POLL_ERROR;",
    "",
    "for i := 0; i < len(shards); i++ {",
    "    rebalance(shards[i])",
    "}",
    "",
    "var caret = 0x2588 // full block glyph for the cursor",
    "const frameInterval = 1.0 / 30.0",
    "",
    "func main() { os.Exit(run()) }",
};
static const NSInteger kTermCorpusCount = (NSInteger)(sizeof(kCorpus) / sizeof(kCorpus[0]));

// Caret blink cadence: ~0.5 s on / 0.5 s off at 30 FPS.
static const NSInteger kTermCaretBlinkFrames = 15;

// Token classes for light syntax highlighting. The order MUST stay in sync with
// kTermPalette below AND the termTokClass constants in terminalview_darwin.go
// (the Go tokenizer unit test asserts against these integer values).
typedef enum {
    TermClassIdent = 0, // identifiers / anything word-like that is not a keyword
    TermClassKeyword,   // Go + C keywords (kTermKeywords)
    TermClassString,    // "..." double-quoted literals
    TermClassComment,   // // to end of line
    TermClassNumber,    // leading-digit runs
    TermClassPunct,     // operators, brackets, whitespace
} TermClass;

// Restrained dark-editor palette (sRGB), indexed by TermClass. Hardcoded in v1
// (no config knobs), mirroring matrix; trivially promotable to config later.
typedef struct { CGFloat r, g, b; } TermRGB;
static const TermRGB kTermPalette[] = {
    { 0.80, 0.82, 0.85 }, // ident   — soft off-white
    { 0.65, 0.55, 0.95 }, // keyword — violet
    { 0.45, 0.80, 0.45 }, // string  — green
    { 0.40, 0.42, 0.45 }, // comment — dim gray
    { 0.90, 0.65, 0.35 }, // number  — amber
    { 0.60, 0.62, 0.65 }, // punct   — muted gray
};
// Class count = palette size (single source of truth, mirroring kTermKeywordCount
// below). buildAttributes loops this many times indexing kTermPalette, so deriving
// it here — instead of a hardcoded literal — keeps that loop in-bounds if a class
// or a palette entry is ever added or removed.
static const NSInteger kTermClassCount =
    (NSInteger)(sizeof(kTermPalette) / sizeof(kTermPalette[0]));
static const TermRGB  kTermCaretColor     = { 0.90, 1.00, 0.90 }; // pale-green cursor
static const unichar  kTermCaretCodepoint = 0x2588;              // full block glyph

// Small static Go + C keyword set: an identifier run matching one of these is
// promoted from TermClassIdent to TermClassKeyword.
static const char *kTermKeywords[] = {
    "func", "return", "if", "else", "for", "range", "var", "const", "type",
    "struct", "import", "package", "switch", "case", "default", "break",
    "continue", "go", "defer", "chan", "map", "interface", "nil", "true",
    "false", "int", "char", "void", "static", "sizeof", "while",
};
static const NSInteger kTermKeywordCount =
    (NSInteger)(sizeof(kTermKeywords) / sizeof(kTermKeywords[0]));

// One tokenized run of a source line: [start, start+length) painted in the color
// of `cls`. Segments are produced once, when a line enters the visible buffer.
typedef struct {
    NSInteger start;  // byte offset into the line text (ASCII => char offset)
    NSInteger length; // run length in bytes/chars
    TermClass cls;    // color class -> kTermPalette
} TermSeg;

// --- Lightweight ASCII tokenizer: source line -> colored segments -----------

static BOOL term_is_ident_start(char c) {
    return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_';
}
static BOOL term_is_digit(char c) { return c >= '0' && c <= '9'; }
static BOOL term_is_ident_char(char c) {
    return term_is_ident_start(c) || term_is_digit(c);
}

// term_is_keyword reports whether the run text[start .. start+len) equals one of
// the static Go/C keywords — exact length match, so no null terminator at the
// run boundary is needed (the run is a substring of a longer C string).
static BOOL term_is_keyword(const char *text, NSInteger start, NSInteger len) {
    for (NSInteger i = 0; i < kTermKeywordCount; i++) {
        const char *kw = kTermKeywords[i];
        if ((NSInteger)strlen(kw) == len &&
            strncmp(text + start, kw, (size_t)len) == 0) {
            return YES;
        }
    }
    return NO;
}

// term_tokenize splits an ASCII source line into colored segments. Called ONCE
// per line as it enters the visible buffer (never per frame). Returns a malloc'd
// TermSeg array (caller owns; free when the line scrolls off) and writes the
// segment count to *outCount. len<=0 -> (NULL, 0). It allocates `len` segments up
// front: every branch consumes >=1 char and emits <=1 segment, so count <= len.
//
// Rules (over ASCII): `//` -> the rest of the line is comment; `"` -> up to the
// closing quote is string (backslash escapes skipped); a leading digit -> number;
// an ident run is promoted to keyword if in kTermKeywords, else ident; any other
// run (operators, brackets, whitespace) -> punct.
static TermSeg *term_tokenize(const char *text, NSInteger len, NSInteger *outCount) {
    if (text == NULL || len <= 0) {
        if (outCount) { *outCount = 0; }
        return NULL;
    }
    TermSeg *segs = (TermSeg *)calloc((size_t)len, sizeof(TermSeg));
    NSInteger count = 0;
    NSInteger i = 0;
    while (i < len) {
        char c = text[i];

        if (c == '/' && i + 1 < len && text[i + 1] == '/') {
            segs[count++] = (TermSeg){ i, len - i, TermClassComment };
            break; // comment runs to the end of the line
        }
        if (c == '"') {
            NSInteger j = i + 1;
            while (j < len && text[j] != '"') {
                if (text[j] == '\\' && j + 1 < len) { j++; } // skip an escaped char
                j++;
            }
            if (j < len) { j++; } // include the closing quote
            segs[count++] = (TermSeg){ i, j - i, TermClassString };
            i = j;
            continue;
        }
        if (term_is_digit(c)) {
            NSInteger j = i + 1;
            while (j < len && (term_is_ident_char(text[j]) || text[j] == '.')) { j++; }
            segs[count++] = (TermSeg){ i, j - i, TermClassNumber };
            i = j;
            continue;
        }
        if (term_is_ident_start(c)) {
            NSInteger j = i + 1;
            while (j < len && term_is_ident_char(text[j])) { j++; }
            TermClass cls = term_is_keyword(text, i, j - i) ? TermClassKeyword
                                                            : TermClassIdent;
            segs[count++] = (TermSeg){ i, j - i, cls };
            i = j;
            continue;
        }
        // Operators / brackets / whitespace: coalesce until the next token start.
        NSInteger j = i + 1;
        while (j < len) {
            char d = text[j];
            if (term_is_ident_start(d) || term_is_digit(d) || d == '"') { break; }
            if (d == '/' && j + 1 < len && text[j + 1] == '/') { break; }
            j++;
        }
        segs[count++] = (TermSeg){ i, j - i, TermClassPunct };
        i = j;
    }
    if (outCount) { *outCount = count; }
    return segs;
}

// One visible line: a RAW pointer into the static corpus (no ownership) plus its
// cached byte length and its tokenized colored segments (heap-owned; freed when
// the line scrolls off the top or the buffer is rebuilt / deallocated).
typedef struct {
    const char *text;     // -> kCorpus[i]; static storage, never freed
    NSInteger   length;   // strlen(text), cached on load
    TermSeg    *segs;     // malloc'd tokenized segments (owned) — NULL for blank lines
    NSInteger   segCount; // number of segments in segs
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
    NSInteger  _blink;        // caret blink phase counter (~0.5 s on/off at 30 FPS)
    NSArray<NSDictionary *> *_attrs;  // per-TermClass text attributes (font + color)
    NSDictionary *_caretAttrs;        // caret glyph attributes (font + caret color)
    NSString  *_caretGlyph;           // cached block-cursor glyph (kTermCaretCodepoint)
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor]; // opaque #000000 backing
        [self buildFont];
        [self buildAttributes];
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

// Build the per-TermClass drawing attributes (font + palette color) once, so
// drawRect: paints pre-colored segments without allocating NSColor/NSDictionary
// per frame. Indexed by (NSUInteger)TermClass. Also caches the caret attributes
// and the block-cursor glyph (built from a unichar so the .m source stays ASCII).
- (void)buildAttributes {
    NSMutableArray<NSDictionary *> *a =
        [NSMutableArray arrayWithCapacity:(NSUInteger)kTermClassCount];
    for (NSInteger i = 0; i < kTermClassCount; i++) {
        TermRGB c = kTermPalette[i];
        [a addObject:@{
            NSFontAttributeName: _font,
            NSForegroundColorAttributeName:
                [NSColor colorWithSRGBRed:c.r green:c.g blue:c.b alpha:1.0],
        }];
    }
    _attrs = [a copy];

    _caretAttrs = @{
        NSFontAttributeName: _font,
        NSForegroundColorAttributeName:
            [NSColor colorWithSRGBRed:kTermCaretColor.r
                                green:kTermCaretColor.g
                                 blue:kTermCaretColor.b
                                alpha:1.0],
    };

    unichar u = kTermCaretCodepoint;
    _caretGlyph = [NSString stringWithCharacters:&u length:1];
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

// loadLine fills a visible-line slot with a corpus line: the text pointer aims
// into the static kCorpus (no ownership, never freed) and the line is tokenized
// ONCE, here, into heap-owned colored segments. Centralizing load here covers
// BOTH entry paths (the initial rebuildBuffer fill and the SCROLL branch of
// step:) so no line is ever drawn untokenized.
//
// It does NOT free any prior line->segs: the only caller that reuses a non-fresh
// slot is scrollUp, which frees the departing top line's segments before the
// shift, after which the bottom slot merely aliases the line below it (whose
// segments stay owned by that lower slot) — freeing here would double-free.
- (void)loadLine:(TermLine *)line fromCorpus:(NSInteger)index {
    const char *text = kCorpus[index];
    line->text     = text;
    line->length   = (NSInteger)strlen(text);
    line->segs     = term_tokenize(text, line->length, &line->segCount);
}

- (void)rebuildBuffer {
    NSRect b = [self bounds];

    if (_lines != NULL) {
        for (NSInteger i = 0; i < _lineCount; i++) {
            free(_lines[i].segs); // release each visible line's tokenized segments
        }
    }
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
// retyping from empty. The departing top line's tokenized segments are freed HERE,
// before the shift, so they never leak; loadLine then tokenizes the fresh bottom
// line. (After the shift the bottom slot briefly aliases the line below it, which
// is why loadLine must NOT free the slot's prior segments — see loadLine.)
- (void)scrollUp {
    if (_lines == NULL || _lineCount <= 0) { return; }
    free(_lines[0].segs); // top line leaves the buffer — release its segments
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
    _blink++; // caret blink phase (rendered by drawRect:)

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

// --- Drawing: opaque-black base + pre-colored segments + blinking caret ---

// Draw one tokenized segment of a visible line at row `row`. For the bottom
// (typing) line the caller passes maxChars = revealed char count so the segment
// is clipped to what has been "typed"; pass maxChars < 0 to draw it in full.
// x = start*cellW relies on the monospaced advance == _cellW, so consecutive
// segments align into a fixed grid; overlong lines run past the right edge and
// are bounds-clipped (no soft-wrap in v1).
- (void)drawSegment:(TermSeg)seg
               text:(const char *)text
                row:(NSInteger)row
           maxChars:(NSInteger)maxChars {
    NSInteger drawLen = seg.length;
    if (maxChars >= 0) {
        if (seg.start >= maxChars) { return; }            // not yet revealed
        if (seg.start + drawLen > maxChars) {
            drawLen = maxChars - seg.start;               // partially revealed
        }
    }
    if (drawLen <= 0) { return; }

    NSString *s = [[NSString alloc] initWithBytes:(text + seg.start)
                                           length:(NSUInteger)drawLen
                                         encoding:NSUTF8StringEncoding];
    if (s == nil) { return; }

    CGFloat x = (CGFloat)seg.start * _cellW;
    CGFloat y = (CGFloat)row * _cellH;
    [s drawAtPoint:NSMakePoint(x, y)
        withAttributes:_attrs[(NSUInteger)seg.cls]];
}

- (void)drawRect:(NSRect)dirtyRect {
    (void)dirtyRect;
    [[NSColor blackColor] setFill]; // pure #000000, fully opaque
    NSRectFill([self bounds]);

    if (_lines == NULL || _lineCount <= 0) { return; }

    NSInteger bottom       = _lineCount - 1;
    NSInteger visibleCount = (NSInteger)floor(_visibleChars); // typed chars on bottom

    for (NSInteger row = 0; row < _lineCount; row++) {
        TermLine line = _lines[row];
        NSInteger maxChars = (row == bottom) ? visibleCount : -1; // clip the typing line
        for (NSInteger s = 0; s < line.segCount; s++) {
            [self drawSegment:line.segs[s] text:line.text row:row maxChars:maxChars];
        }
    }

    // Blinking block caret at the bottom line's typing head (~0.5 s on/off).
    if ((_blink / kTermCaretBlinkFrames) % 2 == 0) {
        CGFloat x = (CGFloat)visibleCount * _cellW;
        CGFloat y = (CGFloat)bottom * _cellH;
        [_caretGlyph drawAtPoint:NSMakePoint(x, y) withAttributes:_caretAttrs];
    }
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
    if (_lines != NULL) {
        for (NSInteger i = 0; i < _lineCount; i++) {
            free(_lines[i].segs); // release each visible line's tokenized segments
        }
    }
    free(_lines);
    _lines = NULL;
    // ARC handles _font / _attrs / _caretAttrs / _caretGlyph / _timer object refs.
}

@end

// --- Test-only shim: expose the pure tokenizer to Go unit tests -------------
//
// term_tokenize is the ONE piece of pure, testable logic in this view (a source
// string -> {start,len,class} segments, where off-by-one boundary bugs live).
// cgo cannot call a static C function or an ObjC method directly from a _test.go
// file (Go toolchain limitation), so this extern shim wraps it: it tokenizes
// `line`, writes up to `maxSegs` segments into the caller-provided
// outStart/outLen/outClass arrays, and returns the segment count (or -1 if
// maxSegs was too small to hold them). Mirrors the cocoa_first_attached_display_id
// test-shim pattern in window_darwin.m. Its Go wrapper is in terminalview_darwin.go.
int terminal_tokenize_for_test(const char *line, int maxSegs,
                               int *outStart, int *outLen, int *outClass) {
    if (line == NULL) { return 0; }
    NSInteger len   = (NSInteger)strlen(line);
    NSInteger count = 0;
    TermSeg  *segs  = term_tokenize(line, len, &count);
    if (count > (NSInteger)maxSegs) {
        free(segs);
        return -1;
    }
    for (NSInteger i = 0; i < count; i++) {
        if (outStart) { outStart[i] = (int)segs[i].start; }
        if (outLen)   { outLen[i]   = (int)segs[i].length; }
        if (outClass) { outClass[i] = (int)segs[i].cls; }
    }
    free(segs);
    return (int)count;
}
