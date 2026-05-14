//go:build darwin

package permissions

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// StatusWriter abstracts polling-loop status rendering for the AX / IM
// permission wait. Two production implementations exist behind a single
// factory (NewStatusWriter): ttyWriter (\r-overwrite per cycle, Unicode
// glyphs — used when stdout is a real TTY) and pipeWriter (one-shot startup
// line + silent cycles, edge-events logged via slog by WaitForGrants —
// used when stdout is a pipe or redirected file).
//
// Per the UI spec, the rendering invariants are:
//   - TTY mode:
//     Update(ax, im) -> "\rdndmode: waiting — ax: {✓|✗} im: {✓|✗}"
//     Final()         -> "\rdndmode: grants received.\n"
//   - Non-TTY mode (pipe / file redirect / bytes.Buffer in tests):
//     Update(ax, im) -> one-shot startup line on first call;
//     subsequent calls SILENT (edge events go via slog in WaitForGrants).
//     Final()         -> "dndmode: grants received.\n"
//
// Tests inject a fake to assert call sequence; production callers (main.go
// Step 8) pass os.Stdout to NewStatusWriter.
type StatusWriter interface {
	// Update is invoked once per polling cycle with the current AX / IM
	// grant state. Implementations decide whether to redraw, log an edge,
	// or stay silent; both safe to call from any goroutine, but the actual
	// caller (WaitForGrants) drives it from one goroutine.
	Update(ax, im bool)

	// Final is invoked exactly once after both permissions are granted.
	// Implementations emit the canonical "dndmode: grants received."
	// line followed by a newline so subsequent PreFlight stdout banners
	// start on a clean column.
	Final()
}

// ttyWriter writes \r-prefixed status lines (in-place overwrite). Per
// the UI spec the glyph spacing is "ax: <glyph> im: <glyph>" with a single
// space between the pair — the East Asian Width = Narrow invariant of the
// chosen Unicode glyphs (U+2713 / U+2717) lets the \r-overwrite cleanly
// repaint each cycle without shifting columns.
type ttyWriter struct {
	w io.Writer
}

func (t *ttyWriter) Update(ax, im bool) {
	fmt.Fprintf(t.w, "\rdndmode: waiting — ax: %s im: %s", glyph(ax), glyph(im))
}

func (t *ttyWriter) Final() {
	fmt.Fprintln(t.w, "\rdndmode: grants received.")
}

// pipeWriter writes ONE startup line on the first Update and stays silent on
// subsequent Updates — the polling-loop emits per-permission grant-edge
// events to stderr via slog.Info("permission granted", kind=…) instead
// (D-06). On Final it writes the canonical "grants received" line so the
// pipe consumer (typically a log file) gets a clear "we're done waiting"
// marker.
type pipeWriter struct {
	w       io.Writer
	started bool
}

func (p *pipeWriter) Update(ax, im bool) {
	if p.started {
		// Subsequent cycles produce no stdout — grant-edge events go via
		// slog (handled by WaitForGrants).
		return
	}
	fmt.Fprintln(p.w, "dndmode: waiting for grants — ax: missing, im: missing")
	p.started = true
}

func (p *pipeWriter) Final() {
	fmt.Fprintln(p.w, "dndmode: grants received.")
}

// glyph maps a bool grant state to its single-column Unicode marker per
// the UI spec Typography: U+2713 CHECK MARK for granted, U+2717 BALLOT X
// for missing. Both are East Asian Width = Narrow on macOS Terminal /
// iTerm2 / Alacritty, so they occupy exactly one terminal cell — the
// invariant that lets ttyWriter's \r-overwrite work without column drift.
func glyph(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// NewStatusWriter returns the StatusWriter best suited for w. If w is a
// real TTY-backed *os.File (e.g. os.Stdout when running attached to a
// terminal), the result is a ttyWriter that uses \r-overwrite. Otherwise
// (pipe, redirected stdout, bytes.Buffer in tests, any non-*os.File), the
// result is a pipeWriter that emits one startup line + nothing per cycle.
//
// Detection uses golang.org/x/term.IsTerminal on the underlying file
// descriptor — the canonical, cross-shell-safe check. main.go (
// Step 8) passes os.Stdout.
func NewStatusWriter(w io.Writer) StatusWriter {
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return &ttyWriter{w: w}
	}
	return &pipeWriter{w: w}
}
