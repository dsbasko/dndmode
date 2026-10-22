//go:build linux

package overlay

import (
	"math"
	"math/rand/v2"
)

// The terminal style: fake source code that types itself out at the bottom
// behind a blinking caret and jump-scrolls up, line by line, in a centred
// column. A port of the macOS TerminalView (terminalview_darwin.m): same
// corpus (shared, see terminalcorpus_linux.c), tokenizer, palette, font
// size, column width, typing speed, pauses and caret blink. Cosmetic only;
// it never reacts to input.

const (
	termFontSize          = 16.0
	termLineHeightFactor  = 1.35
	termContentWidth      = 1200.0                    // centred code column, clamped to the monitor
	termTypeCharsPerFrame = 160.0 * 5.0 / 60.0 / 30.0 // 160 WPM per reference frame
	termPauseFramesBase   = 10                        // hold after a line, ~0.33 s ...
	termPauseFramesJitter = 12                        // ... plus up to ~0.40 s
	termCaretBlinkFrames  = 15                        // ~0.5 s on, 0.5 s off
)

// Typing state of the bottom line.
const (
	termPhaseTyping = iota
	termPhasePause
	termPhaseScroll
)

// termCorpus is the read-only corpus of one language: blocks of lines.
type termCorpus interface {
	blockCount() int
	blockLen(block int) int
	line(block, i int) string
}

// termLine is one visible line, tokenized once when it enters the screen.
// id identifies it to the renderer's layout cache.
type termLine struct {
	id   uint64
	text string
	cols int
	segs []termColSegment
}

// terminalView is the per-window animation state.
type terminalView struct {
	corpus termCorpus
	syntax *termSyntax

	boundsW, boundsH float64
	lines            []termLine // top to bottom; the last one is typing
	nextID           uint64

	bag         []int // shuffle bag of block indexes
	bagPos      int
	blockIdx    int // -1 until playback starts
	lineInBlock int

	visible float64 // glyphs revealed on the bottom line
	phase   int
	pause   float64 // reference frames left in the pause
	blink   float64 // reference frames since start, for the caret
}

// newTerminalView starts a view on corpus with syntax.
func newTerminalView(corpus termCorpus, syntax *termSyntax) *terminalView {
	return &terminalView{corpus: corpus, syntax: syntax, blockIdx: -1}
}

// cellH is the row-to-row advance.
func termCellH() float64 { return math.Ceil(termFontSize * termLineHeightFactor) }

// reshuffle lays a fresh permutation of every block into the bag, never
// starting with the block that just played: no block repeats until the
// whole corpus has (over two hours at this typing speed).
func (v *terminalView) reshuffle() {
	n := v.corpus.blockCount()
	if cap(v.bag) < n {
		v.bag = make([]int, n)
	}
	v.bag = v.bag[:n]
	for i := range v.bag {
		v.bag[i] = i
	}
	rand.Shuffle(n, func(i, j int) { v.bag[i], v.bag[j] = v.bag[j], v.bag[i] })
	if n > 1 && v.bag[0] == v.blockIdx {
		v.bag[0], v.bag[n-1] = v.bag[n-1], v.bag[0]
	}
	v.bagPos = 0
}

// advanceBlock moves to the next block in the bag.
func (v *terminalView) advanceBlock() {
	if v.corpus.blockCount() == 0 {
		return
	}
	if v.bagPos >= len(v.bag) {
		v.reshuffle()
	}
	v.blockIdx = v.bag[v.bagPos]
	v.bagPos++
	v.lineInBlock = 0
}

// nextLine returns the next corpus line, walking blocks in order.
func (v *terminalView) nextLine() termLine {
	if v.blockIdx < 0 {
		v.advanceBlock()
	}
	if v.blockIdx < 0 {
		return termLine{}
	}
	text := v.corpus.line(v.blockIdx, v.lineInBlock)
	v.lineInBlock++
	if v.lineInBlock >= v.corpus.blockLen(v.blockIdx) {
		v.advanceBlock()
	}
	v.nextID++
	return termLine{
		id:   v.nextID,
		text: text,
		cols: termColsBetween(text, 0, len(text)),
		segs: v.syntax.tokenize(text),
	}
}

// resize fills the screen with lines at once (no blank start); the bottom
// one then types from empty. Playback keeps its place in the bag.
func (v *terminalView) resize(boundsW, boundsH float64) {
	v.boundsW, v.boundsH = boundsW, boundsH
	rows := max(int(math.Floor(boundsH/termCellH())), 1)
	v.lines = v.lines[:0]
	for range rows {
		v.lines = append(v.lines, v.nextLine())
	}
	v.phase, v.visible, v.pause, v.blink = termPhaseTyping, 0, 0, 0
}

// step advances by frames reference frames: type, pause, then scroll.
func (v *terminalView) step(frames float64) {
	v.blink += frames
	if len(v.lines) == 0 {
		return
	}
	switch v.phase {
	case termPhaseTyping:
		bottom := float64(v.lines[len(v.lines)-1].cols)
		v.visible += termTypeCharsPerFrame * frames
		if v.visible >= bottom {
			v.visible = bottom
			v.phase = termPhasePause
			v.pause = float64(termPauseFramesBase + rand.IntN(termPauseFramesJitter+1))
		}
	case termPhasePause:
		v.pause -= frames
		if v.pause <= 0 {
			v.phase = termPhaseScroll
		}
	case termPhaseScroll:
		copy(v.lines, v.lines[1:])
		v.lines[len(v.lines)-1] = v.nextLine()
		v.visible = 0
		v.phase = termPhaseTyping
	}
}

// tick advances by dt seconds for a widget of the given size.
func (v *terminalView) tick(boundsW, boundsH, dt float64) {
	if boundsW <= 0 || boundsH <= 0 {
		return
	}
	if boundsW != v.boundsW || boundsH != v.boundsH {
		v.resize(boundsW, boundsH)
		return
	}
	v.step(animFrames(dt))
}

// typed is the number of whole glyphs revealed on the bottom line.
func (v *terminalView) typed() int { return int(math.Floor(v.visible)) }

// caretOn reports the blink phase of the caret.
func (v *terminalView) caretOn() bool {
	return int(v.blink/termCaretBlinkFrames)%2 == 0
}

// column returns the left edge and width of the centred code column.
func (v *terminalView) column() (x, w float64) {
	w = min(termContentWidth, v.boundsW)
	return max(math.Floor((v.boundsW-w)/2), 0), w
}
