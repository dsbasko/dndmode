//go:build darwin

package cocoa

/*
#include <stdlib.h>

extern int terminal_tokenize_for_test(const char* line, const char* lang, int maxSegs, int* outStart, int* outLen, int* outCol, int* outCols, int* outClass);
extern int terminal_reveal_bytes_for_test(const char* line, int cols);
*/
import "C"

import "unsafe"

// termTokClass mirrors the TermClass enum in terminalview_darwin.m. The integer
// values MUST stay in sync with that enum (both are asserted by the tokenizer
// unit test in terminalview_test.go).
type termTokClass int

const (
	termClassIdent termTokClass = iota
	termClassKeyword
	termClassString
	termClassComment
	termClassNumber
	termClassPunct
)

// termSegment is one tokenized run: bytes [start, start+length) of the source
// line, painted by cls. Mirrors the byte half of the C TermSeg struct; the
// column half is exposed separately by tokenizeLineColsForTest, so the many
// ASCII cases (where the two are equal by construction) stay readable.
type termSegment struct {
	start  int
	length int
	cls    termTokClass
}

// termColSegment is one tokenized run with BOTH coordinate systems: bytes
// [start, start+length) of the source line occupying display columns
// [col, col+cols) of the monospaced grid. They coincide for ASCII and diverge
// for the Cyrillic `ys` corpus, which is the case worth pinning — drawRect:
// positions glyphs by col and clips the typing head by cols, so a col/cols bug
// is invisible in every ASCII test and wrecks every ys line.
type termColSegment struct {
	start  int
	length int
	col    int
	cols   int
	cls    termTokClass
}

// tokenizeLineForTest tokenizes a single source line with the Go syntax via the
// terminal_tokenize_for_test C shim — the same pure tokenizer TerminalView uses
// to color scrolling source. Test-only helper: cgo cannot reach the static C
// term_tokenize / ObjC methods from a _test.go file, so this thin wrapper lives
// in the production file alongside the other cgo wrappers (see
// firstAttachedDisplayIDForTest in window_darwin.go). Returns nil if the line
// produced more segments than the fixed buffer holds (never happens for corpus
// lines; guards against a runaway shim contract).
func tokenizeLineForTest(line string) []termSegment {
	cols := tokenizeLineColsForTest(line, "go")
	if cols == nil {
		return nil
	}
	segs := make([]termSegment, len(cols))
	for i, s := range cols {
		segs[i] = termSegment{start: s.start, length: s.length, cls: s.cls}
	}
	return segs
}

// tokenizeLineColsForTest is tokenizeLineForTest with the language selectable
// and the column coordinates kept. lang matches the --style terminal:<lang>
// suffix ("" / unknown => go), so a test can exercise a corpus's own keyword
// table and quoting rules rather than always reading the line as Go.
func tokenizeLineColsForTest(line, lang string) []termColSegment {
	cLine := C.CString(line)
	defer C.free(unsafe.Pointer(cLine))
	cLang := C.CString(lang)
	defer C.free(unsafe.Pointer(cLang))

	const maxSegs = 256
	starts := make([]C.int, maxSegs)
	lens := make([]C.int, maxSegs)
	cols := make([]C.int, maxSegs)
	widths := make([]C.int, maxSegs)
	classes := make([]C.int, maxSegs)

	n := int(C.terminal_tokenize_for_test(cLine, cLang, C.int(maxSegs),
		&starts[0], &lens[0], &cols[0], &widths[0], &classes[0]))
	if n < 0 {
		return nil
	}
	segs := make([]termColSegment, n)
	for i := 0; i < n; i++ {
		segs[i] = termColSegment{
			start:  int(starts[i]),
			length: int(lens[i]),
			col:    int(cols[i]),
			cols:   int(widths[i]),
			cls:    termTokClass(classes[i]),
		}
	}
	return segs
}

// revealBytesForTest returns how many BYTES of line drawSegment: would paint to
// reveal its first `cols` glyphs — the typing-line clip, via the
// terminal_reveal_bytes_for_test C shim. Test-only, same cgo reason as
// tokenizeLineForTest.
func revealBytesForTest(line string, cols int) int {
	cLine := C.CString(line)
	defer C.free(unsafe.Pointer(cLine))
	return int(C.terminal_reveal_bytes_for_test(cLine, C.int(cols)))
}
