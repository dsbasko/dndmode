//go:build linux

package overlay

import (
	"math"
	"testing"
)

// The sharks style has no macOS twin. The helpers are pinned to values of
// the TypeScript original (math.ts run under node); the world and the cells
// are pinned by the properties the original relies on: the population stays
// steady, the sharks keep their bodies and the screen, and every cell lands
// inside the widget with a glyph the atlas holds.

const (
	sharksTestW = 1920.0
	sharksTestH = 1080.0
)

func sharksTestWorld(seed uint64) (*sharksWorld, *sharksField) {
	w := newSharksWorld(seed)
	f := &sharksField{}
	f.resize(sharksTestW, sharksTestH)
	w.resize(sharksTestW, sharksTestH, f.cw, f.ch)
	return w, f
}

func TestSharksHash3_MatchesTypeScript(t *testing.T) {
	for _, tc := range []struct {
		x, y, z int
		want    float64
	}{
		{0, 0, 0, 0},
		{1, 2, 3, 0.6394013063982129},
		{-5, 17, 23, 0.859059470705688},
		{52, -3, 1, 0.2062806577887386},
		{4294967295, 9, 7, 0.05124837812036276},
		{123456, 654321, 11, 0.4870524082798511},
	} {
		if got := sharksHash3(tc.x, tc.y, tc.z); got != tc.want {
			t.Errorf("hash3(%d, %d, %d) = %v, want %v", tc.x, tc.y, tc.z, got, tc.want)
		}
	}
}

func TestSharksCurrent_MatchesTypeScript(t *testing.T) {
	for _, tc := range []struct{ x, y, t, u, v float64 }{
		{0, 0, 0, 8.947368421052632, -22.024291497975707},
		{100, 200, 3, -5.887707159315388, -6.563158701031913},
		{1500, 800, 42.5, -7.314560378309458, 13.40460991725991},
		{-30, 50, 1000, -12.006568971276092, 0.29802203636342534},
	} {
		u, v := sharksCurrent(tc.x, tc.y, tc.t)
		if math.Abs(u-tc.u) > 1e-9 || math.Abs(v-tc.v) > 1e-9 {
			t.Errorf("current(%v, %v, %v) = (%v, %v), want (%v, %v)", tc.x, tc.y, tc.t, u, v, tc.u, tc.v)
		}
	}
}

func TestSharksCurrent_IsDivergenceFree(t *testing.T) {
	const h = 1e-3
	for _, p := range [][3]float64{{10, 20, 0}, {700, 300, 12}, {1800, 1000, 99}} {
		x, y, tt := p[0], p[1], p[2]
		u1, _ := sharksCurrent(x+h, y, tt)
		u0, _ := sharksCurrent(x-h, y, tt)
		_, v1 := sharksCurrent(x, y+h, tt)
		_, v0 := sharksCurrent(x, y-h, tt)
		if div := (u1-u0)/(2*h) + (v1-v0)/(2*h); math.Abs(div) > 1e-6 {
			t.Errorf("divergence at %v = %v", p, div)
		}
	}
}

func TestSharksAngleDiff_MatchesTypeScript(t *testing.T) {
	for _, tc := range []struct{ a, b, want float64 }{
		{3, -3, 0.28318530717958623},
		{-3, 3, -0.28318530717958623},
		{0.5, 7, 0.21681469282041377},
	} {
		if got := sharksAngleDiff(tc.a, tc.b); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("angleDiff(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSharksWorld_SeedsFourDifferentSharksSmallFirst(t *testing.T) {
	for seed := range uint64(20) {
		w, _ := sharksTestWorld(seed)
		if len(w.sharks) != len(sharksSizes) {
			t.Fatalf("seed %d: %d sharks, want %d", seed, len(w.sharks), len(sharksSizes))
		}
		seen := map[int]bool{}
		for i, sh := range w.sharks {
			if sh.kind < 0 || sh.kind >= sharkKinds || seen[sh.kind] {
				t.Fatalf("seed %d: kinds %v are not four different species", seed, w.kinds)
			}
			seen[sh.kind] = true
			if i > 0 && sh.length < w.sharks[i-1].length {
				t.Errorf("seed %d: shark %d (%.0f) drawn after a bigger one (%.0f)", seed, i, sh.length, w.sharks[i-1].length)
			}
		}
		// The biggest is 0.44 of the short side, between 170 and 430 pixels.
		if got, want := w.sharks[len(w.sharks)-1].length, min(430, 0.44*sharksTestH); math.Abs(got-want) > 1e-9 {
			t.Errorf("seed %d: biggest shark %.1f, want %.1f", seed, got, want)
		}
	}
}

func TestSharksWorld_KindsCoverEverySpecies(t *testing.T) {
	seen := map[int]bool{}
	for seed := range uint64(40) {
		for _, k := range newSharksWorld(seed).kinds {
			seen[k] = true
		}
	}
	if len(seen) != sharkKinds {
		t.Errorf("40 seeds picked %d of %d species", len(seen), sharkKinds)
	}
}

func TestSharksWorld_FishCountFollowsTheArea(t *testing.T) {
	for _, tc := range []struct {
		w, h float64
		want int
	}{
		{1920, 1080, 340}, // capped
		{800, 600, 109},
		{400, 300, 85}, // floor
	} {
		w := newSharksWorld(1)
		f := &sharksField{}
		f.resize(tc.w, tc.h)
		w.resize(tc.w, tc.h, f.cw, f.ch)
		if len(w.fish) != tc.want {
			t.Errorf("%vx%v: %d fish, want %d", tc.w, tc.h, len(w.fish), tc.want)
		}
	}
}

// TestSharksWorld_StaysAliveAndOnScreen runs three simulated minutes: the
// sharks eat, eaten fish come back, the fish stay near the screen (a new
// school starts just past an edge, within a perception radius) and every
// shark keeps its body and comes back whenever it dips off the edge.
func TestSharksWorld_StaysAliveAndOnScreen(t *testing.T) {
	w, _ := sharksTestWorld(7)
	total := len(w.fish)
	eaten := 0
	seg := make([]float64, len(w.sharks))
	for i, sh := range w.sharks {
		seg[i] = sh.length / (sharksNodes - 1)
	}
	m := 20 + w.perception
	for step := range 3 * 60 * sharksFPS {
		w.step(sharksStep)
		eaten += len(w.events)
		w.events = w.events[:0]
		if len(w.fish) != total {
			t.Fatalf("step %d: %d fish, want %d", step, len(w.fish), total)
		}
		for i := range w.fish {
			f := &w.fish[i]
			if f.x < -m || f.x > sharksTestW+m || f.y < -m || f.y > sharksTestH+m {
				t.Fatalf("step %d: fish %d at (%.0f, %.0f), off the screen", step, i, f.x, f.y)
			}
			if math.IsNaN(f.vx) || math.IsNaN(f.vy) {
				t.Fatalf("step %d: fish %d has no velocity", step, i)
			}
		}
		for i := range w.sharks {
			sh := &w.sharks[i]
			for k := 1; k < sharksNodes; k++ {
				d := math.Hypot(sh.nodes[k*2]-sh.nodes[(k-1)*2], sh.nodes[k*2+1]-sh.nodes[(k-1)*2+1])
				if math.Abs(d-seg[i]) > 1e-6 {
					t.Fatalf("step %d: shark %d joint %d is %.3f long, want %.3f", step, i, k, d, seg[i])
				}
			}
			// The snout turns back once it leaves the frame, so it never gets
			// further out than its own length.
			x, y, L := sh.nodes[0], sh.nodes[1], sh.length
			if x < -L || x > sharksTestW+L || y < -L || y > sharksTestH+L {
				t.Fatalf("step %d: shark %d snout at (%.0f, %.0f), lost off screen", step, i, x, y)
			}
		}
	}
	if eaten == 0 {
		t.Error("the sharks ate nothing in three minutes")
	}
	alive := 0
	for i := range w.fish {
		if w.fish[i].alive {
			alive++
		}
	}
	if alive < total*3/4 {
		t.Errorf("%d of %d fish alive after %d eaten: they do not come back", alive, total, eaten)
	}
}

func TestSharksShapes_AreSlimAndCloseAtTheEnds(t *testing.T) {
	for k := range sharksShapes {
		shape := &sharksShapes[k]
		for i := 0; i <= 1000; i++ {
			s := float64(i) / 1000
			hw := shape.halfWidth(s)
			if hw < -1e-12 || hw > 0.12 || math.IsNaN(hw) {
				t.Fatalf("kind %d: half-width %v at s=%.3f", k, hw, s)
			}
		}
		if shape.head == nil && shape.halfWidth(0) != 0 {
			t.Errorf("kind %d: snout is %v wide, want a point", k, shape.halfWidth(0))
		}
		if hw := shape.halfWidth(1); hw > 0.01 {
			t.Errorf("kind %d: tail is %v wide", k, hw)
		}
		if (shape.head == nil) != (shape.eye != nil) {
			t.Errorf("kind %d: eyes belong on the body or on a cephalofoil, not both or neither", k)
		}
	}
}

func TestSharksField_CellSizeFollowsTheWidth(t *testing.T) {
	for _, tc := range []struct {
		w            float64
		cw, ch, font float64
		size         int
	}{
		{w: 1920, cw: 13, ch: 19, font: 14, size: 5},
		{w: 1366, cw: 11, ch: 16, font: 12, size: 3},
		{w: 1240, cw: 10, ch: 15, font: 11, size: 2}, // 14.5 rounds up, like Math.round
		{w: 640, cw: 8, ch: 12, font: 9, size: 0},
		{w: 3840, cw: 13, ch: 19, font: 14, size: 5},
	} {
		f := &sharksField{}
		f.resize(tc.w, 800)
		_, _, font := sharksCellSize(f.size)
		if f.cw != tc.cw || f.ch != tc.ch || font != tc.font || f.size != tc.size {
			t.Errorf("width %v: cell %vx%v font %v size %d, want %vx%v font %v size %d",
				tc.w, f.cw, f.ch, font, f.size, tc.cw, tc.ch, tc.font, tc.size)
		}
		if want := int(math.Ceil(tc.w / tc.cw)); f.cols != want {
			t.Errorf("width %v: %d columns, want %d", tc.w, f.cols, want)
		}
	}
}

func TestSharksGlyphs_CoverEveryDrawnGlyph(t *testing.T) {
	all := sharksSkinRamp + sharksFinGlyphs + sharksRidgeGlyph + sharksEdgeHeavy + sharksEdgeLight + sharksMarkGlyphs + "o-*"
	for i := 0; i < len(all); i++ {
		g := all[i]
		if idx := sharksGlyph[g]; sharksGlyphs[idx] != string(g) {
			t.Errorf("glyph %q is not in the atlas", g)
		}
	}
	if sharksGlyphs[sharksGlyphDot] != "·" {
		t.Errorf("glyph %d is %q, want the heat dot", sharksGlyphDot, sharksGlyphs[sharksGlyphDot])
	}
}

// TestSharksField_OpsStayInsideTheWidget renders 20 seconds: every op is a box
// or an atlas glyph inside the widget, with colours in range, the sharks are
// drawn and so are the fish, and no fish shares a cell with a shark.
func TestSharksField_OpsStayInsideTheWidget(t *testing.T) {
	w, f := sharksTestWorld(3)
	sawShark, sawFish, sawWake := false, false, false
	for step := range 20 * sharksFPS {
		w.step(sharksStep)
		f.render(w, sharksStep)
		if len(f.ops)%sharksOpStride != 0 {
			t.Fatalf("step %d: %d op values, not whole ops", step, len(f.ops))
		}
		rects := map[[2]float64]bool{}
		for o := 0; o < len(f.ops); o += sharksOpStride {
			op := f.ops[o : o+sharksOpStride]
			g, x, y := int(op[0]), op[1], op[2]
			if x < 0 || y < 0 || x >= sharksTestW+f.cw || y >= sharksTestH+f.ch {
				t.Fatalf("step %d: op at (%v, %v) is off the widget", step, x, y)
			}
			for _, c := range op[5:] {
				if c < 0 || c > 1 || math.IsNaN(c) {
					t.Fatalf("step %d: op %v has a colour out of range", step, op)
				}
			}
			switch {
			case g == sharksOpRect:
				if op[3] <= 0 || op[4] <= 0 {
					t.Fatalf("step %d: empty box %v", step, op)
				}
				if op[3] == f.cw-1 {
					sawShark = true
					rects[[2]float64{x - 1, y - 1}] = true
				} else {
					sawWake = true
				}
			case g >= 0 && g < len(sharksGlyphs):
				if g == int(sharksGlyph['-']) {
					sawFish = true
					if rects[[2]float64{x, y}] {
						t.Fatalf("step %d: a fish drawn on a shark at (%v, %v)", step, x, y)
					}
				}
			default:
				t.Fatalf("step %d: glyph %d is not in the atlas", step, g)
			}
		}
	}
	if !sawShark || !sawFish || !sawWake {
		t.Errorf("shark blocks %v, fish %v, wake %v: want all drawn", sawShark, sawFish, sawWake)
	}
}

// TestSharksView_StepsAtSixtyAFrame: the world steps once per frame of a 60
// or 59.94 Hz display, about sixty times a second at 144 Hz, and a stall is
// not caught up beyond sharksMaxSteps. A frame without a step is still.
func TestSharksView_StepsAtSixtyAFrame(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dt     float64
		frames int
		want   float64 // steps per frame
	}{
		{"60 Hz", 1.0 / 60, 600, 1},
		{"59.94 Hz", 1.0 / 59.94, 600, 1},
		{"144 Hz", 1.0 / 144, 1440, 60.0 / 144},
	} {
		v := &sharksView{}
		v.seed(1)
		v.tick(sharksTestW, sharksTestH, 0)
		start := v.world.time
		stills := 0
		for range tc.frames {
			v.tick(sharksTestW, sharksTestH, tc.dt)
			if !v.changed {
				stills++
			}
			v.changed = false
		}
		steps := (v.world.time - start) / sharksStep
		if want := tc.want * float64(tc.frames); math.Abs(steps-want) > 1.5 {
			t.Errorf("%s: %.1f steps in %d frames, want %.1f", tc.name, steps, tc.frames, want)
		}
		if tc.want == 1 && stills != 0 {
			t.Errorf("%s: %d frames without a step", tc.name, stills)
		}
		if tc.want < 1 && stills == 0 {
			t.Errorf("%s: every frame stepped, want some still", tc.name)
		}
	}

	v := &sharksView{}
	v.seed(1)
	v.tick(sharksTestW, sharksTestH, 0)
	start := v.world.time
	v.tick(sharksTestW, sharksTestH, 5) // a suspend
	if steps := math.Round((v.world.time - start) / sharksStep); steps != sharksMaxSteps {
		t.Errorf("a stall took %v steps, want %d", steps, sharksMaxSteps)
	}
	if v.acc != 0 {
		t.Errorf("a stall left %v steps owed", v.acc)
	}
}

func TestSharksView_ResizeLaysOutAgain(t *testing.T) {
	v := &sharksView{}
	v.seed(1)
	v.tick(sharksTestW, sharksTestH, 0)
	for range 30 {
		v.tick(sharksTestW, sharksTestH, 1.0/60)
	}
	v.tick(1280, 720, 1.0/60)
	if v.boundsW != 1280 || v.boundsH != 720 || v.field.cw != 10 {
		t.Fatalf("after a resize: bounds %vx%v cell %v", v.boundsW, v.boundsH, v.field.cw)
	}
	for i := range v.world.fish {
		f := &v.world.fish[i]
		if f.x < 0 || f.x > 1280 || f.y < 0 || f.y > 720 {
			t.Fatalf("fish %d at (%.0f, %.0f) is outside the new size", i, f.x, f.y)
		}
	}
	if !v.changed || len(v.field.ops) == 0 {
		t.Error("a resize did not draw the new layout")
	}
}
