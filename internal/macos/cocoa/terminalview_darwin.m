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

// Width of the centered code column (the rest of the shield stays black). AppKit
// draws in points, so this is 1200 points; on a 2x Retina panel that is 2400
// device pixels. If the screen is narrower than this the column is clamped to the
// full width. Kept as one constant so the target width is trivial to change.
static const CGFloat kTermContentWidth = 1200.0;

// Typing speed for the bottom line, fixed at 160 WPM. Using the standard 5
// characters-per-word convention, 160 wpm = 800 chars/min = 13.3 chars/s; at the
// 30 FPS cap that is ~13.3/30 chars per frame. Kept as literal arithmetic (not
// derived from kTermFrameInterval) so it stays a compile-time constant in C.
static const CGFloat kTermTypeCharsPerFrame = 160.0 * 5.0 / 60.0 / 30.0; // 160 WPM

// PAUSE hold after a line finishes typing, in frames: a base plus per-line jitter
// so the cadence between lines is not perfectly uniform. (base .. base+jitter)
static const NSInteger kTermPauseFramesBase   = 10; // ~0.33 s
static const NSInteger kTermPauseFramesJitter = 12; // + up to ~0.40 s

// Synthetic Go source corpus: the scrolling snippets plus their block table live
// in a dedicated header so this view stays focused on rendering. See that header
// for the full rationale (anonymity, ASCII-only, block structure). It defines
// TermBlock, kTermBlocks[] and kTermBlockCount, consumed by the shuffle-bag below.
#include "terminalcorpus_darwin.h"

// Caret blink cadence: ~0.5 s on / 0.5 s off at 30 FPS.
static const NSInteger kTermCaretBlinkFrames = 15;

// Token classes for light syntax highlighting. The order MUST stay in sync with
// kTermPalette below AND the termTokClass constants in terminalview_darwin.go
// (the Go tokenizer unit test asserts against these integer values).
typedef enum {
    TermClassIdent = 0, // identifiers / anything word-like that is not a keyword
    TermClassKeyword,   // Go keywords (kTermKeywords)
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

// Per-language keyword sets: an identifier run matching one of these is promoted
// from TermClassIdent to TermClassKeyword. Which set a view uses is chosen by the
// --style terminal:<lang> language (see TermLangSpec below).
static const char *const kGoKeywords[] = {
    "func", "return", "if", "else", "for", "range", "var", "const", "type",
    "struct", "import", "package", "switch", "case", "default", "break",
    "continue", "go", "defer", "chan", "map", "interface", "select", "nil",
    "true", "false", "int",
};
static const char *const kPyKeywords[] = {
    "def", "class", "return", "if", "elif", "else", "for", "while", "break",
    "continue", "pass", "import", "from", "as", "with", "try", "except",
    "finally", "raise", "lambda", "yield", "None", "True", "False", "and",
    "or", "not", "in", "is", "global", "nonlocal", "async", "await", "del",
    "assert", "self", "int", "str", "float", "bool", "list", "dict",
};
static const char *const kTsKeywords[] = {
    "function", "return", "if", "else", "for", "while", "do", "break",
    "continue", "const", "let", "var", "class", "interface", "type", "enum",
    "extends", "implements", "public", "private", "protected", "readonly",
    "static", "new", "this", "void", "number", "string", "boolean", "null",
    "undefined", "true", "false", "async", "await", "of", "in", "typeof",
    "instanceof", "switch", "case", "default", "try", "catch", "finally",
    "throw", "export", "import", "as",
};
static const char *const kRsKeywords[] = {
    "fn", "let", "mut", "const", "static", "struct", "enum", "impl", "trait",
    "pub", "use", "mod", "if", "else", "match", "for", "while", "loop",
    "return", "break", "continue", "ref", "move", "as", "where", "type",
    "self", "Self", "Some", "None", "Ok", "Err", "true", "false", "i32",
    "u32", "i64", "u64", "usize", "isize", "f32", "f64", "bool", "char",
    "str", "String", "Vec", "Option", "Result", "Box", "dyn", "async",
    "await", "unsafe", "crate", "super",
};
// YoptaScript (terminal:yc) — the one NON-ASCII language: its keywords are
// Cyrillic UTF-8 literals. Nothing in the tokenizer needs to know that: keyword
// comparison is a byte-exact strncmp against the ident run, and an ident run
// swallows every byte >= 0x80 (see term_is_ident_start), so a Cyrillic word is
// one run and matches its literal byte for byte. Spelling matters though — the
// dialect writes both `клёво` and `клево`, and only the exact bytes listed here
// light up, so the corpus and this table must agree on the `ё`/`е` variant.
// Syntax words come first, then the punctuation-equivalents (`жЫ` = {,
// `есть` = }, `сука` = =, `нах` = ;), then the type names.
//
// The language's import/export words (`спиздить`, `предъява`, `братва`) are
// deliberately ABSENT: the corpus may not contain a line that reads like a file
// boundary (see the anonymity note in terminalcorpus_darwin.h), so they would be
// dead entries pointing at text that is not allowed to exist.
static const char *const kYcKeywords[] = {
    "йопта", "куку", "гыы", "участковый", "ясенХуй", "вилкойвглаз",
    "иливжопураз", "го", "потрещим", "крч", "отвечаю", "харэ", "двигай",
    "естьчо", "лещ", "пахану", "хапнуть", "гоп", "тюряжка", "пнх",
    "гыйбать", "тырыпыры", "клево", "батя", "ебнуть",
    "ассо", "сидетьНахуй", "чезажижан", "шкура",
    "чоунастут", "сашаГрей", "яга", "попонятия", "мой", "ебанное",
    "подкрыша", "логопед", "хзйопт",
    "трулио", "чотко", "нетрулио", "нечотко", "нуллио", "порожняк",
    "неибу", "нихуя",
    "жЫ", "есть", "сука", "внатуре", "нах", "нахуй",
    "эквалио", "ровно", "типа", "конкретно", "блябуду",
    "поцик", "поц", "пизже", "хуевей", "ичо", "иличо", "чобля",
    "плюсуюНа", "слилсяНа",
    "пацан", "хуйня", "плавник", "колонна", "двойные", "эээ", "семка",
    "Помойка", "Кент", "Гопец", "СловоПацана",
};

// TermSyntax parameterizes the tokenizer per language: which keyword set to
// promote idents against, whether '#' (Python) or '//' (Go/TS/Rust) starts a line
// comment, and whether single-quoted runs are strings (Python/TS) or just
// punctuation (Go/Rust, where ' is a rune/char/lifetime sigil).
typedef struct {
    const char *const *keywords;
    NSInteger          keywordCount;
    BOOL               hashComment;       // YES: '#' line comment; NO: '//'
    BOOL               singleQuoteString; // YES: '...' is a string literal
} TermSyntax;

#define TERM_KWCOUNT(a) ((NSInteger)(sizeof(a) / sizeof((a)[0])))

// One tokenized run of a source line: bytes [start, start+length) of the line,
// occupying display columns [col, col+cols) of the monospaced grid, painted in
// the color of `cls`. Segments are produced once, when a line enters the visible
// buffer.
//
// BYTES AND COLUMNS ARE NOT THE SAME NUMBER. They coincide for the four ASCII
// languages, but `terminal:yc` (YoptaScript) is Cyrillic UTF-8, where one glyph
// is two bytes. `start`/`length` address the C string (NSString construction,
// strncmp); `col`/`cols` address the fixed monospaced grid (glyph x-position,
// typing progress). Mixing them up would place every Cyrillic line at double its
// x-offset AND clip the typing head mid-codepoint, which yields nil NSStrings and
// blanked-out segments — so pick the pair by what the number is FOR, never by
// which one happens to be at hand.
typedef struct {
    NSInteger start;  // byte offset into the line text
    NSInteger length; // run length in bytes
    NSInteger col;    // display column of the run's first glyph (0-based)
    NSInteger cols;   // run width in glyphs (== length for ASCII)
    TermClass cls;    // color class -> kTermPalette
} TermSeg;

// --- Lightweight UTF-8 tokenizer: source line -> colored segments -----------

// term_is_utf8_cont reports whether b is a UTF-8 continuation byte (10xxxxxx) —
// i.e. the middle of a glyph rather than the start of a new one. Counting the
// bytes that are NOT continuations counts glyphs, which is the whole of the
// byte -> column conversion below (the corpus has no combining marks, and
// nothing outside the BMP, so one codepoint is one monospaced cell).
static BOOL term_is_utf8_cont(unsigned char b) { return (b & 0xC0) == 0x80; }

// term_cols_between counts display columns (glyphs) in text[from, to).
static NSInteger term_cols_between(const char *text, NSInteger from, NSInteger to) {
    NSInteger n = 0;
    for (NSInteger k = from; k < to; k++) {
        if (!term_is_utf8_cont((unsigned char)text[k])) { n++; }
    }
    return n;
}

// term_bytes_for_cols returns the byte length of the first `cols` glyphs of
// text[start, start+maxLen) — the inverse of term_cols_between, used to clip the
// typing line on a GLYPH boundary. Splitting a multi-byte glyph would hand
// +[NSString initWithBytes:...NSUTF8StringEncoding] an invalid sequence, which
// returns nil, which drops the whole segment for that frame: the line would
// flicker instead of typing.
static NSInteger term_bytes_for_cols(const char *text, NSInteger start,
                                     NSInteger maxLen, NSInteger cols) {
    NSInteger k = 0, seen = 0;
    while (k < maxLen) {
        if (!term_is_utf8_cont((unsigned char)text[start + k])) {
            if (seen >= cols) { break; }
            seen++;
        }
        k++;
    }
    return k;
}

// Any byte >= 0x80 counts as an identifier byte, so a Cyrillic word (terminal:yc)
// tokenizes as ONE ident run and reaches term_is_keyword whole. Harmless for the
// four ASCII corpora, which contain no such byte.
static BOOL term_is_ident_start(char c) {
    unsigned char b = (unsigned char)c;
    if (b >= 0x80) { return YES; }
    return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_';
}
static BOOL term_is_digit(char c) { return c >= '0' && c <= '9'; }
static BOOL term_is_ident_char(char c) {
    return term_is_ident_start(c) || term_is_digit(c);
}

// term_is_keyword reports whether the run text[start .. start+len) equals one of
// the language's keywords (syn->keywords) — exact length match, so no null
// terminator at the run boundary is needed (the run is a substring of a longer C
// string).
static BOOL term_is_keyword(const char *text, NSInteger start, NSInteger len,
                            const TermSyntax *syn) {
    for (NSInteger i = 0; i < syn->keywordCount; i++) {
        const char *kw = syn->keywords[i];
        if ((NSInteger)strlen(kw) == len &&
            strncmp(text + start, kw, (size_t)len) == 0) {
            return YES;
        }
    }
    return NO;
}

// term_tokenize splits a source line into colored segments. Called ONCE per line
// as it enters the visible buffer (never per frame). Returns a malloc'd TermSeg
// array (caller owns; free when the line scrolls off) and writes the segment
// count to *outCount. len<=0 -> (NULL, 0). It allocates `len` segments up front:
// every branch consumes >=1 byte and emits <=1 segment, so count <= len.
//
// Rules (per syn): a line comment ('#' or '//') -> the rest of the line is
// comment; a `"` (and, if syn->singleQuoteString, a `'`) -> up to the matching
// quote is string (backslash escapes skipped); a leading digit -> number; an
// ident run is promoted to keyword if in syn->keywords, else ident; any other run
// (operators, brackets, whitespace) -> punct.
//
// Every emit runs through TERM_EMIT, which is what keeps the column cursor and
// the byte cursor from drifting apart: the two are advanced in the same place, by
// the same run, so a segment's col is always the glyph count of everything before
// it. Emitting a segment by hand would silently desynchronize the grid.
static BOOL term_is_line_comment(const char *text, NSInteger i, NSInteger len,
                                 const TermSyntax *syn) {
    if (syn->hashComment) { return text[i] == '#'; }
    return text[i] == '/' && i + 1 < len && text[i + 1] == '/';
}

static TermSeg *term_tokenize(const char *text, NSInteger len, NSInteger *outCount,
                              const TermSyntax *syn) {
    if (text == NULL || len <= 0) {
        if (outCount) { *outCount = 0; }
        return NULL;
    }
    TermSeg *segs = (TermSeg *)calloc((size_t)len, sizeof(TermSeg));
    NSInteger count = 0;
    NSInteger i = 0;
    NSInteger col = 0; // display column of text[i] — advanced only by TERM_EMIT

// Emit text[i, end) as one segment of class `k`, then advance BOTH cursors.
#define TERM_EMIT(end, k)                                                     \
    do {                                                                      \
        NSInteger term_w_ = term_cols_between(text, i, (end));                \
        segs[count++] = (TermSeg){ i, (end) - i, col, term_w_, (k) };         \
        col += term_w_;                                                       \
        i = (end);                                                            \
    } while (0)

    while (i < len) {
        char c = text[i];

        if (term_is_line_comment(text, i, len, syn)) {
            TERM_EMIT(len, TermClassComment);
            break; // comment runs to the end of the line
        }
        if (c == '"' || (syn->singleQuoteString && c == '\'')) {
            char q = c;
            NSInteger j = i + 1;
            while (j < len && text[j] != q) {
                if (text[j] == '\\' && j + 1 < len) { j++; } // skip an escaped char
                j++;
            }
            if (j < len) { j++; } // include the closing quote
            TERM_EMIT(j, TermClassString);
            continue;
        }
        if (term_is_digit(c)) {
            NSInteger j = i + 1;
            while (j < len && (term_is_ident_char(text[j]) || text[j] == '.')) { j++; }
            TERM_EMIT(j, TermClassNumber);
            continue;
        }
        if (term_is_ident_start(c)) {
            NSInteger j = i + 1;
            while (j < len && term_is_ident_char(text[j])) { j++; }
            TermClass cls = term_is_keyword(text, i, j - i, syn) ? TermClassKeyword
                                                                 : TermClassIdent;
            TERM_EMIT(j, cls);
            continue;
        }
        // Operators / brackets / whitespace: coalesce until the next token start.
        NSInteger j = i + 1;
        while (j < len) {
            char d = text[j];
            if (term_is_ident_start(d) || term_is_digit(d) || d == '"') { break; }
            if (syn->singleQuoteString && d == '\'') { break; }
            if (term_is_line_comment(text, j, len, syn)) { break; }
            j++;
        }
        TERM_EMIT(j, TermClassPunct);
    }

#undef TERM_EMIT

    if (outCount) { *outCount = count; }
    return segs;
}

// --- Language selection: corpus table + syntax per --style terminal:<lang> -----

// TermLang identifies the language a TerminalView renders (from the
// terminal:<lang> flag suffix). The default (bare `terminal`) is Go.
typedef enum {
    TermLangGo = 0,
    TermLangPython,
    TermLangTypeScript,
    TermLangRust,
    TermLangYopta,
} TermLang;

// TermLangSpec bundles everything language-specific: the corpus block table (from
// terminalcorpus_darwin.h) and the tokenizer syntax. A view resolves this once in
// its initializer and uses it for the whole session.
typedef struct {
    const TermBlock *blocks;
    NSInteger        blockCount;
    TermSyntax       syntax;
} TermLangSpec;

// term_lang_from_string maps the (already config-validated) language string to the
// enum; NULL / empty / unknown -> Go, matching config.NormalizeTerminalLanguage.
static TermLang term_lang_from_string(const char *s) {
    if (s == NULL) { return TermLangGo; }
    if (strcmp(s, "python") == 0)     { return TermLangPython; }
    if (strcmp(s, "typescript") == 0) { return TermLangTypeScript; }
    if (strcmp(s, "rust") == 0)       { return TermLangRust; }
    if (strcmp(s, "yc") == 0)         { return TermLangYopta; }
    return TermLangGo;
}

// term_lang_spec returns the corpus table + tokenizer syntax for a language.
// Python: '#' comments, single- and double-quoted strings. TS: '//' comments,
// both quote styles. Go/Rust: '//' comments, only double-quoted strings ('
// is a rune/char/lifetime sigil, left as punctuation). Yopta transpiles to JS and
// keeps its operators and literals verbatim, so it takes the TypeScript syntax —
// only the keyword table and the corpus are Cyrillic.
static TermLangSpec term_lang_spec(TermLang lang) {
    switch (lang) {
        case TermLangPython:
            return (TermLangSpec){ kPyBlocks, kPyBlocksCount,
                { kPyKeywords, TERM_KWCOUNT(kPyKeywords), YES, YES } };
        case TermLangTypeScript:
            return (TermLangSpec){ kTsBlocks, kTsBlocksCount,
                { kTsKeywords, TERM_KWCOUNT(kTsKeywords), NO, YES } };
        case TermLangRust:
            return (TermLangSpec){ kRsBlocks, kRsBlocksCount,
                { kRsKeywords, TERM_KWCOUNT(kRsKeywords), NO, NO } };
        case TermLangYopta:
            return (TermLangSpec){ kYcBlocks, kYcBlocksCount,
                { kYcKeywords, TERM_KWCOUNT(kYcKeywords), NO, YES } };
        case TermLangGo:
        default:
            return (TermLangSpec){ kGoBlocks, kGoBlocksCount,
                { kGoKeywords, TERM_KWCOUNT(kGoKeywords), NO, NO } };
    }
}

// One visible line: a RAW pointer into the static corpus (no ownership) plus its
// cached byte length and its tokenized colored segments (heap-owned; freed when
// the line scrolls off the top or the buffer is rebuilt / deallocated).
typedef struct {
    const char *text;     // -> a static block line; storage never freed
    NSInteger   length;   // strlen(text) in BYTES, cached on load
    NSInteger   cols;     // width in GLYPHS — what the typing head counts down
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
    TermLangSpec _spec;       // language corpus table + tokenizer syntax (init-set)
    NSTimer   *_timer;        // ~30 FPS driver; nil when stopped
    NSFont    *_font;         // cached monospaced font (built once in initWithFrame:)
    CGFloat    _cellW;        // monospaced advance width (points), from _font
    CGFloat    _cellH;        // row-to-row height (points)
    CGFloat    _xOffset;      // left margin so the code column is centered (points)
    CGFloat    _contentW;     // width of the centered code column (points)
    TermLine  *_lines;        // visible-line ring, top->bottom; sized to view height
    NSInteger  _lineCount;    // number of visible lines (rows) that fit the height
    NSInteger  _blockIdx;     // current snippet block index into _spec.blocks
    NSInteger  _lineInBlock;  // next line index within the current block
    NSInteger *_bag;          // shuffle-bag: a permutation of block indices
    NSInteger  _bagLen;       // _spec.blockCount, cached (allocated size of _bag)
    NSInteger  _bagPos;       // next slot to draw from _bag (== _bagLen -> reshuffle)
    CGFloat    _visibleChars; // chars revealed on the bottom line (fractional)
    TermPhase  _phase;        // TYPING / PAUSE / SCROLL
    NSInteger  _pauseFrames;  // remaining PAUSE frames (counts down)
    NSInteger  _blink;        // caret blink phase counter (~0.5 s on/off at 30 FPS)
    NSArray<NSDictionary *> *_attrs;  // per-TermClass text attributes (font + color)
    NSDictionary *_caretAttrs;        // caret glyph attributes (font + caret color)
    NSString  *_caretGlyph;           // cached block-cursor glyph (kTermCaretCodepoint)
}

// Plain initWithFrame: defaults to Go, so any caller that does not specify a
// language (or an accidental NSView init path) still renders correctly.
- (instancetype)initWithFrame:(NSRect)frameRect {
    return [self initWithFrame:frameRect language:NULL];
}

- (instancetype)initWithFrame:(NSRect)frameRect language:(const char *)language {
    self = [super initWithFrame:frameRect];
    if (self) {
        _spec = term_lang_spec(term_lang_from_string(language)); // before buildBag
        self.wantsLayer = YES;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor]; // opaque #000000 backing
        [self buildFont];
        [self buildAttributes];
        [self buildBag];
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
// and the block-cursor glyph (built from a unichar rather than pasted in, so the
// codepoint is stated outright instead of riding on the file's encoding — the
// only non-ASCII in this file is kYcKeywords, where the bytes ARE the data).
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

// A fresh PAUSE hold (frames) for the line that just finished typing.
- (NSInteger)nextPauseFrames {
    return kTermPauseFramesBase +
           (NSInteger)arc4random_uniform((uint32_t)kTermPauseFramesJitter + 1);
}

// buildBag allocates the shuffle-bag (one slot per block) and arms it so the first
// advanceBlock reshuffles. _blockIdx = -1 marks "playback not started" so the first
// rebuildBuffer seeds from the bag. Called once from initWithFrame:, freed in dealloc.
- (void)buildBag {
    _bagLen = _spec.blockCount;
    _bag    = (NSInteger *)calloc((size_t)(_bagLen > 0 ? _bagLen : 1),
                                  sizeof(NSInteger));
    _bagPos = _bagLen; // force a reshuffle on the first advanceBlock
    _blockIdx = -1;    // not started yet -> rebuildBuffer seeds from the bag
}

// reshuffleBag lays a fresh random permutation of all block indices into _bag
// (Fisher-Yates). To avoid replaying the just-shown block right after a refill, if
// the new first block equals the last one played it is swapped to the end. Every
// block therefore appears exactly once per bag: no block repeats until the whole
// corpus has played, which is the "no repeat within N" guarantee (N == full-corpus
// playtime, sized to exceed 2 hours).
- (void)reshuffleBag {
    for (NSInteger i = 0; i < _bagLen; i++) { _bag[i] = i; }
    for (NSInteger i = _bagLen - 1; i > 0; i--) {
        NSInteger j = (NSInteger)arc4random_uniform((uint32_t)(i + 1));
        NSInteger t = _bag[i]; _bag[i] = _bag[j]; _bag[j] = t;
    }
    if (_bagLen > 1 && _bag[0] == _blockIdx) {
        NSInteger t = _bag[0]; _bag[0] = _bag[_bagLen - 1]; _bag[_bagLen - 1] = t;
    }
}

// advanceBlock moves playback to the next block in the shuffle-bag, reshuffling
// when the bag is exhausted. Resets the within-block line cursor to 0.
- (void)advanceBlock {
    if (_bag == NULL || _bagLen <= 0) { return; }
    if (_bagPos >= _bagLen) {
        [self reshuffleBag];
        _bagPos = 0;
    }
    _blockIdx    = _bag[_bagPos++];
    _lineInBlock = 0;
}

// nextLine returns the next source line to load, walking the current block in
// order; when the block is exhausted it advances to the next block in the bag. This
// keeps each on-screen stretch coherent while guaranteeing every block plays once
// before any repeats.
- (const char *)nextLine {
    const TermBlock *blk = &_spec.blocks[_blockIdx];
    const char *text = blk->lines[_lineInBlock];
    if (++_lineInBlock >= blk->count) {
        [self advanceBlock];
    }
    return text;
}

// loadLine fills a visible-line slot with a source line: the text pointer aims
// into a static block array (no ownership, never freed) and the line is tokenized
// ONCE, here, into heap-owned colored segments. Centralizing load here covers
// BOTH entry paths (the initial rebuildBuffer fill and the SCROLL branch of
// step:) so no line is ever drawn untokenized.
//
// It does NOT free any prior line->segs: the only caller that reuses a non-fresh
// slot is scrollUp, which frees the departing top line's segments before the
// shift, after which the bottom slot merely aliases the line below it (whose
// segments stay owned by that lower slot) — freeing here would double-free.
- (void)loadLine:(TermLine *)line text:(const char *)text {
    line->text     = text;
    line->length   = (NSInteger)strlen(text);
    line->cols     = term_cols_between(text, 0, line->length);
    line->segs     = term_tokenize(text, line->length, &line->segCount, &_spec.syntax);
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

    // Center a fixed-width code column: clamp the column to the screen when the
    // screen is narrower, then split the leftover space into equal L/R margins.
    // drawRect: offsets every glyph by _xOffset and clips to [_xOffset, +_contentW].
    _contentW = kTermContentWidth < b.size.width ? kTermContentWidth : b.size.width;
    _xOffset  = floor((b.size.width - _contentW) / 2.0);
    if (_xOffset < 0.0) { _xOffset = 0.0; }

    NSInteger rows = (NSInteger)floor(b.size.height / _cellH);
    if (rows < 1) { rows = 1; }
    _lineCount = rows;

    _lines = (TermLine *)calloc((size_t)_lineCount, sizeof(TermLine));

    // Fill the screen immediately so startup is not a blank pane; the bottom line
    // then types itself out from empty. On the first build (_blockIdx < 0) seed from
    // the shuffle-bag; a later rebuild (resize) keeps its place in the current bag.
    if (_blockIdx < 0) {
        [self advanceBlock];
    }
    for (NSInteger i = 0; i < _lineCount; i++) {
        [self loadLine:&_lines[i] text:[self nextLine]];
    }

    _phase        = TermPhaseTyping;
    _visibleChars = 0.0;
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
// shifts up, and the next source line drops into the freed bottom slot and starts
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
    [self loadLine:&_lines[_lineCount - 1] text:[self nextLine]];
    _visibleChars = 0.0;
}

// --- Per-frame advance: type the bottom line, pause, then jump-scroll up ---

- (void)step:(NSTimer *)t {
    (void)t;
    _blink++; // caret blink phase (rendered by drawRect:)

    if (_lineCount > 0) {
        switch (_phase) {
            case TermPhaseTyping: {
                // GLYPHS, not bytes: on a Cyrillic (yc) line the byte length is
                // ~2x the width, so typing to it would stall the caret for a
                // second line's worth of frames past the end of the text.
                NSInteger bottomLen = _lines[_lineCount - 1].cols;
                _visibleChars += kTermTypeCharsPerFrame;
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
// (typing) line the caller passes maxChars = revealed GLYPH count so the segment
// is clipped to what has been "typed"; pass maxChars < 0 to draw it in full.
// x = _xOffset + col*cellW relies on the monospaced advance == _cellW, so
// consecutive segments align into a fixed grid within the centered code column;
// overlong lines run past the column's right edge and are clipped there by the
// drawRect: clip region (no soft-wrap in v1).
//
// The reveal window is measured in COLUMNS and only then converted back to bytes
// (term_bytes_for_cols) — clipping a Cyrillic line at a byte count would both
// misplace the cut and hand NSString a half glyph.
- (void)drawSegment:(TermSeg)seg
               text:(const char *)text
                row:(NSInteger)row
           maxChars:(NSInteger)maxChars {
    NSInteger drawLen = seg.length;
    if (maxChars >= 0) {
        if (seg.col >= maxChars) { return; }               // not yet revealed
        if (seg.col + seg.cols > maxChars) {               // partially revealed
            drawLen = term_bytes_for_cols(text, seg.start, seg.length,
                                          maxChars - seg.col);
        }
    }
    if (drawLen <= 0) { return; }

    NSString *s = [[NSString alloc] initWithBytes:(text + seg.start)
                                           length:(NSUInteger)drawLen
                                         encoding:NSUTF8StringEncoding];
    if (s == nil) { return; }

    CGFloat x = _xOffset + (CGFloat)seg.col * _cellW;
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

    // Confine all glyphs to the centered code column: anything wider than the
    // column is clipped at its right edge (no soft-wrap in v1).
    [NSGraphicsContext saveGraphicsState];
    NSRectClip(NSMakeRect(_xOffset, 0.0, _contentW, NSHeight([self bounds])));

    for (NSInteger row = 0; row < _lineCount; row++) {
        TermLine line = _lines[row];
        NSInteger maxChars = (row == bottom) ? visibleCount : -1; // clip the typing line
        for (NSInteger s = 0; s < line.segCount; s++) {
            [self drawSegment:line.segs[s] text:line.text row:row maxChars:maxChars];
        }
    }

    // Blinking block caret at the bottom line's typing head (~0.5 s on/off).
    if ((_blink / kTermCaretBlinkFrames) % 2 == 0) {
        CGFloat x = _xOffset + (CGFloat)visibleCount * _cellW;
        CGFloat y = (CGFloat)bottom * _cellH;
        [_caretGlyph drawAtPoint:NSMakePoint(x, y) withAttributes:_caretAttrs];
    }

    [NSGraphicsContext restoreGraphicsState];
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
    free(_bag);
    _bag = NULL;
    // ARC handles _font / _attrs / _caretAttrs / _caretGlyph / _timer object refs.
}

@end

// --- Test-only shim: expose the pure tokenizer to Go unit tests -------------
//
// term_tokenize is the ONE piece of pure, testable logic in this view (a source
// string -> {start,len,col,cols,class} segments, where off-by-one boundary bugs
// live). cgo cannot call a static C function or an ObjC method directly from a
// _test.go file (Go toolchain limitation), so this extern shim wraps it: it
// tokenizes `line` with `lang`'s syntax (NULL/unknown => Go, same as the view),
// writes up to `maxSegs` segments into the caller-provided out arrays (any of
// which may be NULL), and returns the segment count (or -1 if maxSegs was too
// small to hold them). Mirrors the cocoa_first_attached_display_id test-shim
// pattern in window_darwin.m. Its Go wrappers are in terminalview_darwin.go.
//
// It exposes byte offsets AND columns because the two diverge exactly where the
// UTF-8 bugs are — a Cyrillic (yc) line where col != start is the case the Go
// test pins.
int terminal_tokenize_for_test(const char *line, const char *lang, int maxSegs,
                               int *outStart, int *outLen,
                               int *outCol, int *outCols, int *outClass) {
    if (line == NULL) { return 0; }
    NSInteger len   = (NSInteger)strlen(line);
    NSInteger count = 0;
    TermSyntax syn  = term_lang_spec(term_lang_from_string(lang)).syntax;
    TermSeg  *segs  = term_tokenize(line, len, &count, &syn);
    if (count > (NSInteger)maxSegs) {
        free(segs);
        return -1;
    }
    for (NSInteger i = 0; i < count; i++) {
        if (outStart) { outStart[i] = (int)segs[i].start; }
        if (outLen)   { outLen[i]   = (int)segs[i].length; }
        if (outCol)   { outCol[i]   = (int)segs[i].col; }
        if (outCols)  { outCols[i]  = (int)segs[i].cols; }
        if (outClass) { outClass[i] = (int)segs[i].cls; }
    }
    free(segs);
    return (int)count;
}

// Test-only shim for term_bytes_for_cols — the glyph-boundary clip drawSegment:
// applies to the typing line. It is the last place a half-codepoint can reach
// NSString (which answers nil, blanking the segment for that frame), and unlike
// the tokenizer it produces no visible artifact in the segment table, so it gets
// its own pin. Returns the byte length of the first `cols` glyphs of `line`.
int terminal_reveal_bytes_for_test(const char *line, int cols) {
    if (line == NULL) { return 0; }
    NSInteger len = (NSInteger)strlen(line);
    return (int)term_bytes_for_cols(line, 0, len, (NSInteger)cols);
}
