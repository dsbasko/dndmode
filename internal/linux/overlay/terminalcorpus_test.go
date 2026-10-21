//go:build linux

package overlay

import (
	"testing"
	"unicode/utf8"
)

// TestTermCorpus_EveryLineTokenizes reads every line of every language from
// the shared C corpus and checks that it is valid UTF-8 and that the
// language's tokenizer covers it with contiguous segments, the property the
// renderer's colouring relies on.
func TestTermCorpus_EveryLineTokenizes(t *testing.T) {
	for lang, idx := range termLangs {
		c := cCorpus{lang: cInt(idx)}
		blocks := c.blockCount()
		if blocks == 0 {
			t.Errorf("%s: empty corpus", lang)
			continue
		}
		lines := 0
		for b := range blocks {
			for i := range c.blockLen(b) {
				line := c.line(b, i)
				lines++
				if !utf8.ValidString(line) {
					t.Fatalf("%s block %d line %d: invalid UTF-8", lang, b, i)
				}
				pos := 0
				for _, s := range termSyntaxes[idx].tokenize(line) {
					if s.start != pos || s.length <= 0 {
						t.Fatalf("%s block %d line %d: gap at byte %d: %q", lang, b, i, pos, line)
					}
					pos += s.length
				}
				if pos != len(line) {
					t.Fatalf("%s block %d line %d: covered %d of %d bytes", lang, b, i, pos, len(line))
				}
			}
		}
		t.Logf("%-10s %3d blocks %5d lines", lang, blocks, lines)
	}
}
