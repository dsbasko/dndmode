//go:build darwin

package cocoa

import (
	"fmt"
	"testing"
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
			line: `"dndmode.active"`,
			want: []termSegment{
				{start: 0, length: 16, cls: termClassString},
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
		"package dndmode",
		"    windows []*overlayWindow",
		"    const name = \"dndmode.active\"",
		"    return true // stay silent on wrong input",
		"static const NSUInteger kShieldBehavior =",
		"for i := 0; i < len(displays); i++ {",
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
