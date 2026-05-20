//go:build darwin

package permissions

import (
	"bytes"
	"strings"
	"testing"
)

func TestStatusWriter_NewFromBuffer_UsesPipeWriter(t *testing.T) {
	buf := &bytes.Buffer{}
	sw := NewStatusWriter(buf)
	if _, ok := sw.(*pipeWriter); !ok {
		t.Errorf("NewStatusWriter(*bytes.Buffer) = %T, want *pipeWriter (non-TTY fallback)", sw)
	}
}

func TestPipeWriter_Update_FirstCallEmitsStartupLine(t *testing.T) {
	buf := &bytes.Buffer{}
	pw := &pipeWriter{w: buf}
	pw.Update(false, false)

	got := buf.String()
	const want = "dndmode: waiting for grants — ax: missing, im: missing"
	if !strings.Contains(got, want) {
		t.Errorf("Update wrote %q, want substring %q", got, want)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("Update wrote %q, want trailing newline", got)
	}
}

func TestPipeWriter_Update_SubsequentCallsSilent(t *testing.T) {
	buf := &bytes.Buffer{}
	pw := &pipeWriter{w: buf}
	pw.Update(false, false)
	pre := buf.Len()

	pw.Update(true, false)
	pw.Update(false, true)
	pw.Update(true, true)

	if got, want := buf.Len(), pre; got != want {
		t.Errorf("buffer len after 3 silent Updates = %d, want unchanged %d", got, want)
	}
}

func TestPipeWriter_Final_EmitsGrantsReceivedWithNewline(t *testing.T) {
	buf := &bytes.Buffer{}
	pw := &pipeWriter{w: buf}
	pw.Final()

	got := buf.String()
	if !strings.Contains(got, "dndmode: grants received.") {
		t.Errorf("Final wrote %q, want substring %q", got, "dndmode: grants received.")
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("Final wrote %q, want trailing newline", got)
	}
}

func TestTTYWriter_Update_StatesRenderWithGlyphsAndCarriageReturn(t *testing.T) {
	cases := []struct {
		name     string
		ax, im   bool
		expected string
	}{
		{"both_missing", false, false, "ax: ✗ im: ✗"},
		{"ax_granted_im_missing", true, false, "ax: ✓ im: ✗"},
		{"ax_missing_im_granted", false, true, "ax: ✗ im: ✓"},
		{"both_granted", true, true, "ax: ✓ im: ✓"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			tw := &ttyWriter{w: buf}
			tw.Update(tt.ax, tt.im)

			got := buf.String()
			if !strings.HasPrefix(got, "\r") {
				t.Errorf("Update wrote %q, want \\r prefix", got)
			}
			if !strings.Contains(got, tt.expected) {
				t.Errorf("Update wrote %q, want substring %q", got, tt.expected)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("Update wrote %q, want no \\n (in-place overwrite)", got)
			}
			if !strings.Contains(got, "dndmode: waiting — ") {
				t.Errorf("Update wrote %q, want prefix 'dndmode: waiting — '", got)
			}
		})
	}
}

func TestTTYWriter_Final_EmitsGrantsReceivedWithCarriageThenNewline(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := &ttyWriter{w: buf}
	tw.Final()

	got := buf.String()
	if !strings.HasPrefix(got, "\r") {
		t.Errorf("Final wrote %q, want \\r prefix", got)
	}
	if !strings.Contains(got, "dndmode: grants received.") {
		t.Errorf("Final wrote %q, want substring %q", got, "dndmode: grants received.")
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("Final wrote %q, want trailing newline", got)
	}
}

func TestGlyph_Mapping(t *testing.T) {
	if got, want := glyph(true), "✓"; got != want {
		t.Errorf("glyph(true) = %q, want %q", got, want)
	}
	if got, want := glyph(false), "✗"; got != want {
		t.Errorf("glyph(false) = %q, want %q", got, want)
	}
}

// TestPipeWriter_Update_DynamicStateRendering verifies that the one-shot
// startup line emitted by pipeWriter reflects the (ax, im) state passed to
// the FIRST Update call (fix). The pre-fix implementation hardcoded
// "ax: missing, im: missing" regardless of the parameters — a the UI spec
// "Polling entry banner (non-TTY)" violation when WaitForGrants enters
// with one permission already granted (e.g. AX granted, IM missing).
//
// The (true, true) row encodes a subtle invariant: when both grants are
// already present at entry, WaitForGrants invokes Update once then Final
// — so the pipeWriter MUST stay silent on Update (no "waiting" line at
// all) because there is nothing to wait for. Final() then emits the
// canonical "dndmode: grants received." line.
func TestPipeWriter_Update_DynamicStateRendering(t *testing.T) {
	cases := []struct {
		name    string
		ax, im  bool
		want    string // empty string means "buffer must be empty"
		wantNum int    // how many bytes should be written (for empty: 0)
	}{
		{
			name: "both_missing",
			ax:   false, im: false,
			want: "dndmode: waiting for grants — ax: missing, im: missing\n",
		},
		{
			name: "ax_granted_im_missing",
			ax:   true, im: false,
			want: "dndmode: waiting for grants — ax: granted, im: missing\n",
		},
		{
			name: "ax_missing_im_granted",
			ax:   false, im: true,
			want: "dndmode: waiting for grants — ax: missing, im: granted\n",
		},
		{
			name: "both_granted_silent",
			ax:   true, im: true,
			want: "", // No waiting-line — Final() fires immediately in WaitForGrants.
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			pw := &pipeWriter{w: buf}
			pw.Update(tt.ax, tt.im)

			got := buf.String()
			if got != tt.want {
				t.Errorf("Update(%v, %v) wrote %q, want %q", tt.ax, tt.im, got, tt.want)
			}
			if !pw.started {
				t.Errorf("Update(%v, %v) left started=false, want true (one-shot consumed)", tt.ax, tt.im)
			}
		})
	}
}

// TestPipeWriter_Update_SubsequentCallsSilent_AfterDynamicStart verifies
// that the one-shot startup gate engages regardless of the initial state.
// Pre-fix the gate was correct, but the test only covered the (false,
// false) entry. With dynamic rendering we cross-check that starting from
// (false, true) — a likely real-world state — still locks subsequent
// updates to silence.
func TestPipeWriter_Update_SubsequentCallsSilent_AfterDynamicStart(t *testing.T) {
	buf := &bytes.Buffer{}
	pw := &pipeWriter{w: buf}
	pw.Update(false, true)
	pre := buf.Len()

	pw.Update(true, true)
	pw.Update(false, false)
	pw.Update(true, false)

	if got, want := buf.Len(), pre; got != want {
		t.Errorf("buffer len after 3 silent Updates = %d, want unchanged %d", got, want)
	}
}

// TestTTYWriter_EntryBanner_PrintsWaitingForGrants verifies fix:
// ttyWriter.EntryBanner emits the "dndmode: waiting for grants…\n" banner
// ONCE before the first \r-cycle Update. Regression guards: no \r prefix
// (this is a finalized banner, not an in-place repaint), \n suffix so
// subsequent Update lines start on a clean column.
func TestTTYWriter_EntryBanner_PrintsWaitingForGrants(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := &ttyWriter{w: buf}
	tw.EntryBanner()

	got := buf.String()
	const want = "dndmode: waiting for grants…\n"
	if got != want {
		t.Errorf("EntryBanner wrote %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "\r") {
		t.Errorf("EntryBanner wrote %q, want NO \\r prefix (finalized banner, not in-place repaint)", got)
	}
}

// TestPipeWriter_EntryBanner_NoOp verifies fix: pipeWriter.EntryBanner
// is a no-op — the pipeWriter's startup line is emitted by Update (which
// already contains the state), and EntryBanner MUST NOT consume the
// one-shot slot. The follow-up Update(false, false) call must still emit
// the dynamic startup line.
func TestPipeWriter_EntryBanner_NoOp(t *testing.T) {
	buf := &bytes.Buffer{}
	pw := &pipeWriter{w: buf}
	pw.EntryBanner()

	if got := buf.Len(); got != 0 {
		t.Errorf("EntryBanner wrote %d bytes, want 0 (no-op for pipe)", got)
	}
	if pw.started {
		t.Errorf("EntryBanner flipped started=true, want false (one-shot slot must be preserved for Update)")
	}

	pw.Update(false, false)
	got := buf.String()
	const want = "dndmode: waiting for grants — ax: missing, im: missing\n"
	if got != want {
		t.Errorf("Update after EntryBanner wrote %q, want %q (EntryBanner must not consume Update slot)", got, want)
	}
}
