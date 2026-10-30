//go:build linux

package overlay

import (
	"fmt"
	"testing"
)

// fakeCorpus has blocks of lengths lens; line i of block b reads "b<b> l<i>".
type fakeCorpus struct{ lens []int }

func (f fakeCorpus) blockCount() int      { return len(f.lens) }
func (f fakeCorpus) blockLen(b int) int   { return f.lens[b] }
func (f fakeCorpus) line(b, i int) string { return fmt.Sprintf("b%d l%d", b, i) }
func newFakeView(lens ...int) *terminalView {
	return newTerminalView(fakeCorpus{lens}, &termSyntaxes[0])
}

// TestTerminal_ResizeFillsScreen: the screen starts full, lines are in
// block order, and the bottom one types from empty.
func TestTerminal_ResizeFillsScreen(t *testing.T) {
	v := newFakeView(50)
	v.tick(0, 0, frame)
	if len(v.lines) != 0 {
		t.Fatal("filled before the widget had a size")
	}
	v.tick(1920, 1080, frame)
	if want := int(1080 / termCellH()); len(v.lines) != want {
		t.Fatalf("rows = %d, want %d", len(v.lines), want)
	}
	for i, l := range v.lines {
		if l.text != fmt.Sprintf("b0 l%d", i) {
			t.Fatalf("line %d = %q, want block order", i, l.text)
		}
		if len(l.segs) == 0 {
			t.Fatalf("line %d not tokenized", i)
		}
	}
	if v.typed() != 0 || v.phase != termPhaseTyping {
		t.Errorf("bottom line starts typed=%d phase=%d", v.typed(), v.phase)
	}
	if x, w := v.column(); w != termContentWidth || x != (1920-termContentWidth)/2 {
		t.Errorf("column at %g width %g", x, w)
	}
}

// TestTerminal_TypePauseScroll: the bottom line types at the macOS speed,
// pauses, then the buffer scrolls up by one and the next line types.
func TestTerminal_TypePauseScroll(t *testing.T) {
	v := newFakeView(100)
	v.tick(800, 220, frame)
	bottom := v.lines[len(v.lines)-1]
	top := v.lines[1]

	v.step(1)
	if v.visible != termTypeCharsPerFrame {
		t.Fatalf("typed %g after one frame, want %g", v.visible, termTypeCharsPerFrame)
	}
	for v.phase == termPhaseTyping {
		v.step(1)
	}
	if v.typed() != bottom.cols {
		t.Fatalf("typing stopped at %d of %d glyphs", v.typed(), bottom.cols)
	}
	if v.pause < termPauseFramesBase || v.pause > termPauseFramesBase+termPauseFramesJitter {
		t.Fatalf("pause = %g frames", v.pause)
	}
	for v.phase != termPhaseTyping {
		v.step(1)
	}
	if v.lines[0].id != top.id || v.lines[len(v.lines)-2].id != bottom.id {
		t.Error("buffer did not scroll up by exactly one line")
	}
	if v.visible != 0 {
		t.Error("new bottom line does not start empty")
	}
}

// TestTerminal_ShuffleBag: every block plays once per bag, and a refill
// never starts with the block that just played.
func TestTerminal_ShuffleBag(t *testing.T) {
	v := newFakeView(1, 1, 1, 1, 1)
	for round := range 20 {
		seen := map[int]bool{}
		for range 5 {
			prev := v.blockIdx
			v.advanceBlock()
			if seen[v.blockIdx] {
				t.Fatalf("round %d: block %d repeated inside one bag", round, v.blockIdx)
			}
			if v.bagPos == 1 && v.blockIdx == prev {
				t.Fatalf("round %d: refill started with the block that just played", round)
			}
			seen[v.blockIdx] = true
		}
	}
}

// TestTerminal_CaretBlinks: half a second on, half a second off.
func TestTerminal_CaretBlinks(t *testing.T) {
	v := newFakeView(10)
	if !v.caretOn() {
		t.Error("caret starts off")
	}
	v.blink = termCaretBlinkFrames
	if v.caretOn() {
		t.Error("caret still on after half a period")
	}
	v.blink = 2 * termCaretBlinkFrames
	if !v.caretOn() {
		t.Error("caret off after a full period")
	}
}
