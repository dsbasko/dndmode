//go:build darwin

package cocoa

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

// TestTerminalView_Tokenize_Classification exercises the pure ASCII tokenizer
// backing TerminalView's syntax highlighting (term_tokenize, reached through the
// terminal_tokenize_for_test cgo shim). It is the one unit-testable piece of the
// terminal overlay style — drawRect: output is owned by the WindowServer and can
// only be validated in the manual visual run. The cases pin down the boundary
// behavior (segment start/length/class) where off-by-one bugs would hide:
// keyword vs. ident, strings, comments, numbers, and punctuation coalescing.
func TestTerminalView_Tokenize_Classification(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []termSegment
	}{
		{
			name: "empty line has no segments",
			line: "",
			want: nil,
		},
		{
			name: "keyword then ident split on whitespace",
			line: "func main",
			want: []termSegment{
				{start: 0, length: 4, cls: termClassKeyword}, // func
				{start: 4, length: 1, cls: termClassPunct},   // space
				{start: 5, length: 4, cls: termClassIdent},   // main (not a keyword)
			},
		},
		{
			name: "keyword lookalike stays ident",
			line: "returned",
			want: []termSegment{
				{start: 0, length: 8, cls: termClassIdent},
			},
		},
		{
			name: "assignment with number literal",
			line: "x := 42",
			want: []termSegment{
				{start: 0, length: 1, cls: termClassIdent},  // x
				{start: 1, length: 4, cls: termClassPunct},  // " := "
				{start: 5, length: 2, cls: termClassNumber}, // 42
			},
		},
		{
			name: "number keeps dots and hex tail",
			line: "1.0 0x2588",
			want: []termSegment{
				{start: 0, length: 3, cls: termClassNumber}, // 1.0
				{start: 3, length: 1, cls: termClassPunct},  // space
				{start: 4, length: 6, cls: termClassNumber}, // 0x2588
			},
		},
		{
			name: "double-quoted string is one segment",
			line: `s := "abc"`,
			want: []termSegment{
				{start: 0, length: 1, cls: termClassIdent},  // s
				{start: 1, length: 4, cls: termClassPunct},  // " := "
				{start: 5, length: 5, cls: termClassString}, // "abc"
			},
		},
		{
			name: "string with dot inside stays string",
			line: `"cache.warm"`,
			want: []termSegment{
				{start: 0, length: 12, cls: termClassString},
			},
		},
		{
			name: "empty string literal is one segment",
			line: `""`,
			want: []termSegment{
				{start: 0, length: 2, cls: termClassString},
			},
		},
		{
			name: "escaped quote stays inside the string",
			line: `"a\"b"`,
			want: []termSegment{
				{start: 0, length: 6, cls: termClassString}, // escaped \" is not the terminator
			},
		},
		{
			name: "unterminated string runs to end of line",
			line: `"abc`,
			want: []termSegment{
				{start: 0, length: 4, cls: termClassString}, // no closing quote → to EOL
			},
		},
		{
			name: "line comment swallows the rest",
			line: "x // note",
			want: []termSegment{
				{start: 0, length: 1, cls: termClassIdent},   // x
				{start: 1, length: 1, cls: termClassPunct},   // space (stops before //)
				{start: 2, length: 7, cls: termClassComment}, // // note
			},
		},
		{
			name: "comment at column zero is one comment segment",
			line: "// foo",
			want: []termSegment{
				{start: 0, length: 6, cls: termClassComment},
			},
		},
		{
			name: "single slash is punctuation not a comment",
			line: "1 / 2",
			want: []termSegment{
				{start: 0, length: 1, cls: termClassNumber}, // 1
				{start: 1, length: 3, cls: termClassPunct},  // " / "
				{start: 4, length: 1, cls: termClassNumber}, // 2
			},
		},
		{
			name: "keyword at end of line",
			line: "return nil",
			want: []termSegment{
				{start: 0, length: 6, cls: termClassKeyword}, // return
				{start: 6, length: 1, cls: termClassPunct},   // space
				{start: 7, length: 3, cls: termClassKeyword}, // nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenizeLineForTest(tt.line)
			if !equalSegments(got, tt.want) {
				t.Fatalf("tokenize(%q):\n got  %s\n want %s",
					tt.line, formatSegments(got), formatSegments(tt.want))
			}
		})
	}
}

// TestTerminalView_Tokenize_Coverage confirms every produced segment tiles the
// input exactly: segments are contiguous, non-overlapping, start at 0, and cover
// the whole line — a trailing line comment is no exception, since term_tokenize
// emits it as a single segment spanning to end-of-line. This guards the invariant
// drawRect: relies on — that x = start*cellW lays segments into one gap-free grid.
func TestTerminalView_Tokenize_Coverage(t *testing.T) {
	lines := []string{
		"package workerpool",
		"    jobs    []*queuedJob",
		"    const name = \"cache.warm\"",
		"    return d * 2 // exponential, capped",
		"static const unsigned kPollFlags =",
		"for i := 0; i < len(shards); i++ {",
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			segs := tokenizeLineForTest(line)
			pos := 0
			for i, s := range segs {
				if s.start != pos {
					t.Fatalf("segment %d starts at %d, want contiguous at %d (%s)",
						i, s.start, pos, formatSegments(segs))
				}
				if s.length <= 0 {
					t.Fatalf("segment %d has non-positive length %d (%s)",
						i, s.length, formatSegments(segs))
				}
				pos += s.length
			}
			if pos != len(line) {
				t.Fatalf("segments cover %d chars, want the full %d (%s)",
					pos, len(line), formatSegments(segs))
			}
		})
	}
}

func equalSegments(a, b []termSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func formatSegments(segs []termSegment) string {
	if len(segs) == 0 {
		return "[]"
	}
	names := map[termTokClass]string{
		termClassIdent:   "ident",
		termClassKeyword: "keyword",
		termClassString:  "string",
		termClassComment: "comment",
		termClassNumber:  "number",
		termClassPunct:   "punct",
	}
	out := "["
	for i, s := range segs {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("{%s %d:%d}", names[s.cls], s.start, s.start+s.length)
	}
	return out + "]"
}

// TestTerminalView_Tokenize_Yopta_Keywords exercises the `ys` (YoptaScript)
// corpus's own keyword table through the same tokenizer. Two things are pinned
// that no ASCII case can reach: a Cyrillic word arrives at term_is_keyword as ONE
// ident run (term_is_ident_start swallows every byte >= 0x80), and it is matched
// byte-exactly, so a keyword that differs only by `ё` vs `е` does NOT light up —
// which is why the corpus and kYsKeywords have to agree on the spelling.
func TestTerminalView_Tokenize_Yopta_Keywords(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []termTokClass
	}{
		{
			name: "declaration keyword then ident",
			line: "гыы длина",
			want: []termTokClass{termClassKeyword, termClassPunct, termClassIdent},
		},
		{
			name: "brace and terminator words are keywords",
			line: "жЫ есть нах",
			want: []termTokClass{
				termClassKeyword, termClassPunct, // жЫ
				termClassKeyword, termClassPunct, // есть
				termClassKeyword, // нах
			},
		},
		{
			name: "wrong yo-spelling stays an ident",
			line: "клёво",
			want: []termTokClass{termClassIdent},
		},
		{
			name: "keyword lookalike stays ident",
			line: "естьчоТам",
			want: []termTokClass{termClassIdent},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenizeLineColsForTest(tt.line, "ys")
			if len(got) != len(tt.want) {
				t.Fatalf("tokenize(%q) produced %d segments, want %d: %v",
					tt.line, len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].cls != tt.want[i] {
					t.Errorf("segment %d of %q = class %d, want %d",
						i, tt.line, got[i].cls, tt.want[i])
				}
			}
		})
	}
}

// TestTerminalView_Tokenize_ColumnsTrackGlyphs is the UTF-8 pin that the ASCII
// cases structurally cannot be: it asserts that col/cols count GLYPHS while
// start/length count BYTES, and that the two tile the line independently.
//
// This is the invariant drawRect: rests on. It places each segment at
// x = col*cellW and clips the typing head at a column count; if col were ever
// filled from the byte offset, every Cyrillic line would render at double its
// x-position and the typing clip would cut mid-codepoint (nil NSString, blanked
// segment). The bug is invisible in every ASCII test, because there col == start
// by construction.
func TestTerminalView_Tokenize_ColumnsTrackGlyphs(t *testing.T) {
	lines := []string{
		"гыы длина сука 0 нах",
		"    отвечаю помойка.писькомер нах",
		`ясенХуй базар сука "чотко" нах`,
		"// Пузырек: гоняем помойку по росту.",
		"вилкойвглаз (a хуевей 10) жЫ харэ нах есть",
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			segs := tokenizeLineColsForTest(line, "ys")
			if len(segs) == 0 {
				t.Fatalf("tokenize(%q) produced no segments", line)
			}
			wantCols := len([]rune(line))
			if wantCols == len(line) {
				t.Fatalf("line %q is pure ASCII — it cannot pin the byte/glyph split", line)
			}

			bytePos, colPos := 0, 0
			for i, s := range segs {
				if s.start != bytePos {
					t.Fatalf("segment %d starts at byte %d, want contiguous at %d", i, s.start, bytePos)
				}
				if s.col != colPos {
					t.Fatalf("segment %d starts at column %d, want contiguous at %d", i, s.col, colPos)
				}
				if s.cols != len([]rune(line[s.start:s.start+s.length])) {
					t.Fatalf("segment %d spans %d columns, want %d glyphs",
						i, s.cols, len([]rune(line[s.start:s.start+s.length])))
				}
				bytePos += s.length
				colPos += s.cols
			}
			if bytePos != len(line) {
				t.Errorf("segments cover %d bytes, want the full %d", bytePos, len(line))
			}
			if colPos != wantCols {
				t.Errorf("segments cover %d columns, want the full %d", colPos, wantCols)
			}
			if colPos == bytePos {
				t.Errorf("columns (%d) equal bytes (%d) on a Cyrillic line — the two cursors are not being tracked separately",
					colPos, bytePos)
			}
		})
	}
}

// TestTerminalView_RevealBytes_NeverSplitsAGlyph walks the typing head across a
// Cyrillic line one column at a time and asserts the revealed byte prefix is
// always a whole number of glyphs.
//
// This is the failure the byte/column split exists to prevent: drawSegment: hands
// the prefix to +[NSString initWithBytes:...NSUTF8StringEncoding], which returns
// nil for a truncated codepoint, and a nil string silently draws nothing — so a
// half-glyph cut does not look like a half glyph, it looks like the line blinking
// out for one frame at every second character.
func TestTerminalView_RevealBytes_NeverSplitsAGlyph(t *testing.T) {
	lines := []string{
		"гыы длина сука 0 нах",
		"    отвечаю помойка.писькомер нах",
		"вилкойвглаз (a хуевей 10) жЫ харэ нах есть",
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			runes := []rune(line)
			for cols := 0; cols <= len(runes); cols++ {
				got := revealBytesForTest(line, cols)
				want := len(string(runes[:cols]))
				if got != want {
					t.Fatalf("reveal(%d cols) = %d bytes, want %d", cols, got, want)
				}
				if !utf8.ValidString(line[:got]) {
					t.Fatalf("reveal(%d cols) cut mid-codepoint: %q", cols, line[:got])
				}
			}
			// Asking for more columns than the line has must clamp, not overrun.
			if got := revealBytesForTest(line, len(runes)+10); got != len(line) {
				t.Errorf("reveal past end = %d bytes, want the full %d", got, len(line))
			}
		})
	}
}
