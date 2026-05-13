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
