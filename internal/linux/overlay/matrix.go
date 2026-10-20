//go:build linux

package overlay

import (
	"math"
	"math/rand/v2"
)

// The matrix style: green "digital rain" over the black shield. A port of
// the macOS MatrixView (internal/macos/cocoa/matrixview_darwin.m) with the
// same model: a fixed grid of cells per column, each holding a STABLE glyph
// written once by the head as it passes; the head slides down at a
// per-column speed and the trail is a brightness gradient over those
// stationary glyphs. Columns differ in glyph size, shade and opacity for
// depth. Cosmetic only; the shield stays opaque.

// matrixSizes are the per-column glyph sizes (pixels), varied for depth.
var matrixSizes = []float64{24, 34, 46}

const (
	matrixCellHFactor = 1.18 // vertical advance, as a multiple of the size
	matrixCellWFactor = 0.92 // horizontal advance
)

// matrixPalette holds shades of green only.
var matrixPalette = []dvdRGB{
	{0.00, 1.00, 0.25}, // classic matrix green
	{0.45, 1.00, 0.20}, // lime
	{0.00, 0.90, 0.45}, // emerald
	{0.25, 0.85, 0.30}, // soft mid green
}

// matrixGlyphs is the alphabet: digits 0-9, then half-width katakana
// U+FF66..U+FF9D. Cells store indexes into it.
var matrixGlyphs = func() []string {
	g := make([]string, 0, 10+0xFF9D-0xFF66+1)
	for d := '0'; d <= '9'; d++ {
		g = append(g, string(d))
	}
	for r := rune(0xFF66); r <= 0xFF9D; r++ {
		g = append(g, string(r))
	}
	return g
}()

// matrixColumn is one falling streak.
type matrixColumn struct {
	x      float64
	bucket int     // index into matrixSizes
	hue    int     // index into matrixPalette
	alpha  float64 // opacity multiplier, 0.45..1
	head   float64 // head position in cells (fractional; negative = above the top)
	speed  float64 // cells per frame, 0.12..0.40
	trail  float64 // trail length in cells, 8..20
	rows   int
	glyphs []int // stable glyph per cell
}

// matrixRain is the per-window animation.
type matrixRain struct {
	boundsW, boundsH float64
	cols             []matrixColumn
}

// randomGlyph returns a glyph index: ~25% digits, ~75% katakana.
func randomGlyph() int {
	if rand.IntN(4) == 0 {
		return rand.IntN(10)
	}
	return 10 + rand.IntN(len(matrixGlyphs)-10)
}

// respawn reseeds a column's motion and refills its cells. The head starts
// above the top by a random gap so columns enter staggered.
func (c *matrixColumn) respawn() {
	c.trail = 8 + float64(rand.IntN(13))
	c.speed = 0.12 + float64(rand.IntN(29))/100
	c.head = -float64(rand.IntN(c.rows + int(c.trail) + 1))
	for r := range c.glyphs {
		c.glyphs[r] = randomGlyph()
	}
}

// resize packs columns of random sizes across the width.
func (m *matrixRain) resize(boundsW, boundsH float64) {
	m.boundsW, m.boundsH = boundsW, boundsH
	m.cols = m.cols[:0]
	for x := 0.0; x < boundsW; {
		bucket := rand.IntN(len(matrixSizes))
		size := matrixSizes[bucket]
		rows := int(math.Floor(boundsH/(size*matrixCellHFactor))) + 1
		c := matrixColumn{
			x:      x,
			bucket: bucket,
			hue:    rand.IntN(len(matrixPalette)),
			alpha:  0.45 + float64(rand.IntN(56))/100,
			rows:   rows,
			glyphs: make([]int, rows),
		}
		c.respawn()
		m.cols = append(m.cols, c)
		x += size * matrixCellWFactor
	}
}

// step slides every head; the head writes a fresh glyph into each cell it
// enters, and a column respawns once its whole trail has left the bottom.
func (m *matrixRain) step() {
	for i := range m.cols {
		c := &m.cols[i]
		prev := int(math.Floor(c.head))
		c.head += c.speed
		if c.head-c.trail > float64(c.rows) {
			c.respawn()
			continue
		}
		cur := int(math.Floor(c.head))
		for r := prev + 1; r <= cur; r++ {
			if r >= 0 && r < c.rows {
				c.glyphs[r] = randomGlyph()
			}
		}
	}
}

// tick advances one frame for a widget of the given size.
func (m *matrixRain) tick(boundsW, boundsH float64) {
	if boundsW <= 0 || boundsH <= 0 {
		return
	}
	if boundsW != m.boundsW || boundsH != m.boundsH {
		m.resize(boundsW, boundsH)
		return
	}
	m.step()
}

// matrixCell is one glyph to draw.
type matrixCell struct {
	x, y    float64
	bucket  int
	glyph   int
	r, g, b float64
}

// lit calls fn for every visible cell, brightest at the head and fading
// smoothly up the trail; the two cells nearest the head are whitened.
func (m *matrixRain) lit(fn func(matrixCell)) {
	for i := range m.cols {
		c := &m.cols[i]
		cellH := matrixSizes[c.bucket] * matrixCellHFactor
		hue := matrixPalette[c.hue]
		lo := max(int(math.Floor(c.head-c.trail)), 0)
		hi := min(int(math.Floor(c.head)), c.rows-1)
		for r := lo; r <= hi; r++ {
			d := c.head - float64(r)
			if d < 0 || d >= c.trail {
				continue
			}
			bright := (1 - d/c.trail) * c.alpha
			w := 0.0
			if d < 1.5 {
				w = (1.5 - d) / 1.5 * 0.85
			}
			fn(matrixCell{
				x:      c.x,
				y:      float64(r) * cellH,
				bucket: c.bucket,
				glyph:  c.glyphs[r],
				r:      math.Min(1, hue.r+w) * bright,
				g:      math.Min(1, hue.g+w*0.3) * bright,
				b:      math.Min(1, hue.b+w) * bright,
			})
		}
	}
}
