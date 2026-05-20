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
//     EntryBanner()   -> "dndmode: waiting for grants…\n"
//     Update(ax, im) -> "\rdndmode: waiting — ax: {✓|✗} im: {✓|✗}"
//     Final()         -> "\rdndmode: grants received.\n"
//   - Non-TTY mode (pipe / file redirect / bytes.Buffer in tests):
//     EntryBanner()   -> no-op (pipeWriter.Update already carries state)
//     Update(ax, im) -> one-shot startup line on first call reflecting
//     (ax, im) state — "ax: granted, im: missing" etc.; when both
//     grants are already present, NOTHING is written and Final() fires
//     immediately. Subsequent Update calls SILENT (edge events go via
//     slog in WaitForGrants).
//     Final()         -> "dndmode: grants received.\n"
//
// Tests inject a fake to assert call sequence; production callers (main.go
// Step 8) pass os.Stdout to NewStatusWriter.
type StatusWriter interface {
	// EntryBanner is invoked exactly ONCE by WaitForGrants before the
	// initial AX/IM probe. For ttyWriter — emits the finalized banner
	// "dndmode: waiting for grants…\n" so the TTY user sees "we are
	// waiting for grants" before the per-cycle \r-repaint starts. For
	// pipeWriter — no-op (the pipe startup line, emitted by Update on
	// the first cycle, already contains the state and renders the
	// banner redundant).
	//
	// Added per the UI spec "Polling entry banner (TTY)" + fix from
	//. Safe to call from any goroutine; called by
	// WaitForGrants from the same goroutine that drives Update / Final.
	EntryBanner()

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

// EntryBanner emits the once-per-WaitForGrants banner that signals the
// TTY user "we are about to start polling for grants". Printed BEFORE the
// first \r-cycle Update so the in-place repaint does not visually clobber
// the entry message. Uses U+2026 HORIZONTAL ELLIPSIS for the trailing
// glyph — matches the "thinking" / "waiting" convention from the UI spec.
// Trailing \n is required so the subsequent Update's \r-prefix repaints
// the line below the banner, not over it.
func (t *ttyWriter) EntryBanner() {
	fmt.Fprintln(t.w, "dndmode: waiting for grants…")
}

func (t *ttyWriter) Update(ax, im bool) {
	fmt.Fprintf(t.w, "\rdndmode: waiting — ax: %s im: %s", glyph(ax), glyph(im))
}

func (t *ttyWriter) Final() {
	fmt.Fprintln(t.w, "\rdndmode: grants received.")
}

// pipeWriter writes ONE startup line on the first Update — the line
// reflects the (ax, im) state passed by WaitForGrants (post- fix:
// dynamic rendering, no longer hardcoded "missing, missing"). Subsequent
// Update calls are silent — the polling-loop emits per-permission
// grant-edge events to stderr via slog.Info("permission granted", kind=…)
// instead. On Final it writes the canonical "grants received" line
// so the pipe consumer (typically a log file) gets a clear "we're done
// waiting" marker.
//
// Special case (nuance): if the FIRST Update is called with both
// (ax, im) already true — meaning WaitForGrants will invoke Final
// immediately — pipeWriter stays silent on Update. The absence of a
// "waiting" line is itself a correct signal that there was nothing to
// wait for; Final's "grants received." is sufficient. The started flag
// still flips so any later Update remains silent.
type pipeWriter struct {
	w       io.Writer
	started bool
}

// EntryBanner is a no-op for pipeWriter — the startup line emitted by
// the first Update already encodes both the "we are waiting" intent and
// the current (ax, im) state, making a separate banner redundant for
// pipe consumers (log files, journal scrapers). MUST NOT consume the
// one-shot started slot — Update is the sole gate.
func (p *pipeWriter) EntryBanner() {
	// no-op (see comment above and StatusWriter.EntryBanner doc).
}

func (p *pipeWriter) Update(ax, im bool) {
	if p.started {
		// Subsequent cycles produce no stdout — grant-edge events go via
		// slog (handled by WaitForGrants).
		return
	}
	p.started = true
	if ax && im {
		// Both grants already present at entry — WaitForGrants will call
		// Final immediately. Emit nothing here: the absence of a
		// "waiting" line + the canonical "grants received." line from
		// Final fully describe the state.
		return
	}
	fmt.Fprintf(p.w, "dndmode: waiting for grants — ax: %s, im: %s\n", axState(ax), axState(im))
}

func (p *pipeWriter) Final() {
	fmt.Fprintln(p.w, "dndmode: grants received.")
}

// axState maps a bool grant state to the lowercase noun form used in
// pipeWriter's startup line ("ax: granted, im: missing"). The non-TTY
// counterpart of glyph(): pipe consumers (log files, journal scrapers)
// prefer plain words over Unicode glyphs.
func axState(ok bool) string {
	if ok {
		return "granted"
	}
	return "missing"
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
