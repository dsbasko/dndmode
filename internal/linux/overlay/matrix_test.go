//go:build linux

package overlay

import "testing"

func TestMatrixGlyphs_Alphabet(t *testing.T) {
	if len(matrixGlyphs) != 10+56 {
		t.Fatalf("alphabet has %d glyphs, want 66", len(matrixGlyphs))
	}
	if matrixGlyphs[0] != "0" || matrixGlyphs[9] != "9" || matrixGlyphs[10] != "ｦ" || matrixGlyphs[65] != "ﾝ" {
		t.Errorf("unexpected alphabet ends: %q %q %q %q", matrixGlyphs[0], matrixGlyphs[9], matrixGlyphs[10], matrixGlyphs[65])
	}
}

// TestMatrixRain_ResizeCoversWidth: columns are packed edge to edge, every
// column's cells fill the height, and every glyph is a valid index.
func TestMatrixRain_ResizeCoversWidth(t *testing.T) {
	m := &matrixRain{}
	m.tick(0, 0, frame)
	if len(m.cols) != 0 {
		t.Fatal("columns built before the widget had a size")
	}
	m.tick(1920, 1080, frame)
	if len(m.cols) == 0 {
		t.Fatal("no columns")
	}
	x := 0.0
	for i, c := range m.cols {
		if c.x != x {
			t.Fatalf("column %d at x=%g, want %g (no gaps)", i, c.x, x)
		}
		size := matrixSizes[c.bucket]
		x += size * matrixCellWFactor
		if float64(c.rows)*size*matrixCellHFactor < 1080 {
			t.Errorf("column %d: %d rows do not reach the bottom", i, c.rows)
		}
		if c.alpha < 0.45 || c.alpha > 1 || c.speed < 0.12 || c.speed > 0.40 || c.trail < 8 || c.trail > 20 {
			t.Errorf("column %d out of range: %+v", i, c)
		}
		if c.head > 0 {
			t.Errorf("column %d starts inside the screen (head %g)", i, c.head)
		}
		for _, g := range c.glyphs {
			if g < 0 || g >= len(matrixGlyphs) {
				t.Fatalf("glyph index %d out of range", g)
			}
		}
	}
	if x < 1920 {
		t.Errorf("columns end at %g, short of the width", x)
	}
}

// TestMatrixRain_StepMovesHeadAndRespawns: the head advances by its speed,
// the cells it enters are rewritten, and a column whose trail has left the
// bottom starts again above the top.
func TestMatrixRain_StepMovesHeadAndRespawns(t *testing.T) {
	m := &matrixRain{}
	m.tick(800, 600, frame)
	c := &m.cols[0]
	c.head, c.speed, c.trail = 2.9, 0.4, 10
	for r := range c.glyphs {
		c.glyphs[r] = -1 // sentinel: not yet written
	}
	m.step(1)
	if c.head != 3.3 {
		t.Errorf("head = %g, want 3.3", c.head)
	}
	if c.glyphs[3] < 0 {
		t.Error("the cell the head entered was not written")
	}
	if c.glyphs[4] >= 0 || c.glyphs[2] >= 0 {
		t.Error("cells the head did not enter were rewritten")
	}

	c.head = float64(c.rows) + c.trail - 0.1
	m.step(1)
	if c.head > 0 {
		t.Errorf("column did not respawn above the top: head %g", c.head)
	}
}

// TestMatrixRain_LitFadesFromHead: the head cell is the brightest and
// whitest, brightness falls monotonically up the trail, nothing is drawn
// outside the column's rows.
func TestMatrixRain_LitFadesFromHead(t *testing.T) {
	m := &matrixRain{}
	m.tick(800, 600, frame)
	m.cols = m.cols[:1]
	c := &m.cols[0]
	c.head, c.trail, c.alpha, c.hue = 10.2, 8, 1, 0

	var cells []matrixCell
	m.lit(func(cell matrixCell) { cells = append(cells, cell) })
	if len(cells) != 8 {
		t.Fatalf("lit %d cells, want 8 (rows 3..10)", len(cells))
	}
	cellH := matrixSizes[c.bucket] * matrixCellHFactor
	head := cells[len(cells)-1]
	if head.y != 10*cellH || head.r == 0 {
		t.Errorf("head cell = %+v, want row 10 and whitened (red > 0 on pure green)", head)
	}
	for i := 1; i < len(cells); i++ {
		if cells[i].g <= cells[i-1].g {
			t.Errorf("green does not rise towards the head at cell %d", i)
		}
	}

	c.head = 2 // trail partly above the top: only rows 0..2 drawn
	cells = cells[:0]
	m.lit(func(cell matrixCell) { cells = append(cells, cell) })
	if len(cells) != 3 || cells[0].y != 0 {
		t.Errorf("partial streak: %d cells, first y %g", len(cells), cells[0].y)
	}
}
