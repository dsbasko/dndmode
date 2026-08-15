//go:build darwin

package eventtap

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The nosplit invariant documented on `eventtap_callback` in tap_darwin.m —
// the callback body makes ZERO calls into Go — is the one property in this
// package that no compiler, vet pass, or runtime check enforces. A cgo call
// re-introduced into a CGEventTap callback does not fail to build and does
// not fail a normal test run: it corrupts under load, on a worker thread the
// Go runtime does not own, which is precisely the failure the author already
// hit once under `-race`.
//
// The tests below are a gold-test on the SOURCE TEXT of tap_darwin.m. They
// are deliberately textual: the property being pinned is "this translation
// unit does not call Go", and the only artefact that expresses it is the C
// source. A semantic Go test cannot see inside a C function body — the
// pre-existing TestUserIntentionalMask_MatchesMatcherPackage says as much
// about its own C half ("enforced by code-review") — so text is the closest
// the test suite can get to what the C compiler sees.
//
// Two layers, cheapest first:
//
//  1. TestTapSource_DoesNotImportCgoExportHeader — the header is the ONLY
//     way a symbol from the Go side becomes callable in this file, so its
//     absence makes the whole class of regressions unrepresentable. This is
//     the layer that carries most of the value for a handful of lines.
//  2. TestEventTapCallback_CallsNoGoExports — defence in depth against a
//     future file that imports the header for some other function (the way
//     watchdog_darwin.m legitimately does) and then reaches for it from the
//     callback too.
//
// Both read the same file the build reads. A rename of tap_darwin.m fails
// them loudly rather than silently unpinning the invariant.

// tapSourcePath is the Objective-C translation unit that holds the tap
// callback. Relative to the package directory, which is `go test`'s working
// directory (same convention as ringHeaderPath in ring_guard_test.go).
const tapSourcePath = "tap_darwin.m"

var (
	// cgoExportImportRe matches a real preprocessor directive pulling in the
	// cgo-generated export header, in either #import or #include form and
	// with either quoting. It is anchored at `^[ \t]*#` so the prose in
	// tap_darwin.m's own docblock — which NAMES the header in order to
	// explain why it is absent — cannot trip it: a `//` comment line can
	// never start with `#`.
	cgoExportImportRe = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*(?:import|include)[ \t]*["<]_cgo_export\.h[">]`)

	// callbackSignatureRe locates the callback definition. The capture ends
	// at the `{` that opens the body so brace-matching can start from a
	// known offset rather than from a guess.
	callbackSignatureRe = regexp.MustCompile(`(?s)static\s+CGEventRef\s+eventtap_callback\s*\([^)]*\)\s*\{`)

	// goExportDirectiveRe harvests the //export names declared anywhere in
	// this package's Go files. Those names — and only those — are what
	// _cgo_export.h makes callable from C, so they are the exact symbol set
	// the callback must not touch.
	goExportDirectiveRe = regexp.MustCompile(`(?m)^//export\s+(\w+)`)

	// blockCommentRe / lineCommentRe strip comments before the body is
	// searched for calls. Without this, a future comment that mentions a
	// retired export by name ("used to call eventtap_matched()") would fail
	// the test for documenting history accurately.
	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRe  = regexp.MustCompile(`(?m)//.*$`)
)

// readTapSource returns the contents of tap_darwin.m, failing the test if it
// cannot be read. A rename that leaves the nosplit invariant unpinned is
// itself the regression these tests exist to catch, so a missing file is a
// hard failure and not a skip.
func readTapSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(tapSourcePath)
	if err != nil {
		t.Fatalf("read %s: %v (this file holds the CGEventTap callback; the "+
			"nosplit gold tests cannot run without it — if it was renamed, "+
			"update tapSourcePath here too)", tapSourcePath, err)
	}
	return string(b)
}

// stripCComments removes block and line comments so a search for call sites
// sees code only. Order matters: block comments go first, because a `//`
// inside a `/* … */` must not terminate the block early.
//
// This is not a full C lexer — a `/*` or `//` inside a string literal would
// be mishandled. tap_darwin.m's callback body contains no string literals at
// all (it cannot: no logging is permitted there, which is the silent-fail
// property), so the simple form is sufficient and its limitation is bounded
// by a rule the same file already enforces.
func stripCComments(src string) string {
	return lineCommentRe.ReplaceAllString(blockCommentRe.ReplaceAllString(src, " "), "")
}

// TestTapSource_DoesNotImportCgoExportHeader is the primary gold test:
// tap_darwin.m must not include the cgo-generated export header at all.
//
// Without that header no Go-exported symbol is even declared in this
// translation unit, so a call to one does not compile — the invariant stops
// being a convention and becomes a build error. Adding the #import back is
// the first step of every regression this file guards against, and it is the
// step that is easy to make casually while chasing something else.
//
// watchdog_darwin.m DOES import the header and that is correct: its
// //export call fires from a GCD timer handler, an ordinary thread the Go
// runtime is free to grow a stack on, never from a tap callback.
func TestTapSource_DoesNotImportCgoExportHeader(t *testing.T) {
	src := readTapSource(t)

	if loc := cgoExportImportRe.FindStringIndex(src); loc != nil {
		line := 1 + strings.Count(src[:loc[0]], "\n")
		t.Fatalf("%s:%d imports _cgo_export.h.\n\n"+
			"The CGEventTap callback in this file runs on a worker thread owned "+
			"by the tap's CFRunLoop, and its documented nosplit invariant is that "+
			"it calls into Go ZERO times: no Go locks, no Go allocation, no "+
			"logging, no dispatch_async. Importing the export header is what makes "+
			"a Go call expressible here at all, so its absence is the enforcement.\n\n"+
			"If a NON-callback function in this file needs to call into Go, move "+
			"that function to its own .m file (watchdog_darwin.m is the precedent — "+
			"it imports the header legitimately because its //export fires from a "+
			"GCD handler, not from a tap callback).",
			tapSourcePath, line)
	}
}

// TestEventTapCallback_CallsNoGoExports is the second layer: even if some
// future edit legitimately brings _cgo_export.h into this file, the callback
// body itself must still call none of the symbols it declares.
//
// The body is located by brace-matching from the signature rather than by
// regexp over the whole function, because the body contains nested braces
// (three `if` blocks) that no non-recursive pattern can bound correctly.
//
// The Go-export set is harvested from this package's own //export directives
// instead of being hardcoded, so a newly added export is covered the day it
// is written rather than the day someone remembers to extend this list.
func TestEventTapCallback_CallsNoGoExports(t *testing.T) {
	// extractCallbackBody already returns comment-free text, so a comment
	// recording history accurately ("used to call eventtap_matched()") cannot
	// fail this test.
	code := extractCallbackBody(t, readTapSource(t))

	exports := goExportNames(t)
	if len(exports) == 0 {
		t.Fatalf("no //export directives found in the package's Go files: this "+
			"test would pass vacuously. Either the harvest regexp %q drifted from "+
			"the directive syntax, or the package genuinely stopped exporting to C "+
			"— in the latter case delete this test rather than leaving it green "+
			"for the wrong reason.", goExportDirectiveRe)
	}

	for _, name := range exports {
		callRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
		if callRe.MatchString(code) {
			t.Errorf("eventtap_callback calls the Go export %s().\n\n"+
				"That body runs on the CGEventTap worker thread and MUST NOT call "+
				"into Go at all — not a Go lock, not an allocation, not a logging "+
				"call, and not a bare //export helper. A Go call there needs the Go "+
				"runtime to be able to grow the thread's stack, which is exactly the "+
				"shape that failed under `-race` before the matching moved to the "+
				"poller goroutine.\n\n"+
				"The supported way to get data out of the callback is the keystroke "+
				"ring: append to the static g_ring with a RELEASE store on g_seq, and "+
				"let pollSequence (poller.go) consume it through eventtap_snapshot.",
				name)
		}
	}
}

// extractCallbackBody returns the text between the braces of
// `eventtap_callback`, brace-matched from the opening `{`.
//
// Comments are stripped from the SEARCH copy before matching braces so a `{`
// or `}` written inside a comment cannot unbalance the count; offsets stay
// valid because stripCComments replaces block comments with a single space
// and line comments with nothing, and the copy is what gets returned.
func extractCallbackBody(t *testing.T, src string) string {
	t.Helper()

	clean := stripCComments(src)
	loc := callbackSignatureRe.FindStringIndex(clean)
	if loc == nil {
		t.Fatalf("could not locate `static CGEventRef eventtap_callback(...) {` in %s. "+
			"If the callback was renamed or its signature changed, update "+
			"callbackSignatureRe — a silently unmatched pattern would turn this "+
			"gold test into a no-op.", tapSourcePath)
	}

	depth := 0
	start := loc[1] // one past the opening brace
	for i := loc[1] - 1; i < len(clean); i++ {
		switch clean[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return clean[start:i]
			}
		}
	}
	t.Fatalf("unbalanced braces after the eventtap_callback signature in %s: "+
		"the body could not be delimited, so nothing was checked", tapSourcePath)
	return ""
}

// goExportNames returns every symbol this package exports to C via an
// //export directive — the exact contents of the generated _cgo_export.h.
func goExportNames(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range goExportDirectiveRe.FindAllStringSubmatch(string(b), -1) {
			names = append(names, m[1])
		}
	}
	return names
}
