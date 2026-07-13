//go:build darwin

package cocoa

/*
#include <stdlib.h>

extern int terminal_tokenize_for_test(const char* line, int maxSegs, int* outStart, int* outLen, int* outClass);
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

// termSegment is one tokenized run: [start, start+length) of the source line,
// painted by cls. Mirrors the C TermSeg struct.
type termSegment struct {
	start  int
	length int
	cls    termTokClass
}

// tokenizeLineForTest tokenizes a single source line via the
// terminal_tokenize_for_test C shim — the same pure tokenizer TerminalView uses
// to color scrolling source. Test-only helper: cgo cannot reach the static C
// term_tokenize / ObjC methods from a _test.go file, so this thin wrapper lives
// in the production file alongside the other cgo wrappers (see
// firstAttachedDisplayIDForTest in window_darwin.go). Returns nil if the line
// produced more segments than the fixed buffer holds (never happens for corpus
// lines; guards against a runaway shim contract).
func tokenizeLineForTest(line string) []termSegment {
	cLine := C.CString(line)
	defer C.free(unsafe.Pointer(cLine))

	const maxSegs = 256
	starts := make([]C.int, maxSegs)
	lens := make([]C.int, maxSegs)
	classes := make([]C.int, maxSegs)

	n := int(C.terminal_tokenize_for_test(cLine, C.int(maxSegs),
		&starts[0], &lens[0], &classes[0]))
	if n < 0 {
		return nil
	}
	segs := make([]termSegment, n)
	for i := 0; i < n; i++ {
		segs[i] = termSegment{
			start:  int(starts[i]),
			length: int(lens[i]),
			cls:    termTokClass(classes[i]),
		}
	}
	return segs
}
