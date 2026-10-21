//go:build linux

package overlay

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The dino style has no macOS twin, so these cases pin the model on their
// own: well-formed sprites, an autopilot that never touches an obstacle,
// a speed and a score that follow the Chrome runner, and a frame that is
// cheap to repaint.

func TestDinoArt_IsWellFormed(t *testing.T) {
	if strings.Contains(dinoPalette, ".") || len(dinoColors) != len(dinoPalette) {
		t.Fatalf("palette %q with %d colours: want colours only, '.' is transparent", dinoPalette, len(dinoColors))
	}
	want := []struct {
		prefix string
		n      int
		w, h   int
	}{
		{"run", 2, 22, 24}, {"jump", 1, 22, 24}, {"duck", 2, 30, 13},
		{"cactus", 3, 9, 18}, {"big", 3, 13, 25}, {"bird", 2, 23, 20},
		{"cloud", 1, 23, 7}, {"ground", 3, 50, 5}, {"digit", 10, 5, 7},
	}
	i := 0
	for _, w := range want {
		for range w.n {
			s := dinoArt[i]
			i++
			if !strings.HasPrefix(s.name, w.prefix) || s.w != w.w || s.h != w.h {
				t.Fatalf("frame %d is %q %dx%d, want %s* %dx%d", i-1, s.name, s.w, s.h, w.prefix, w.w, w.h)
			}
			if len(s.px) != s.w*s.h {
				t.Fatalf("%s has %d pixels, want %d", s.name, len(s.px), s.w*s.h)
			}
			if strings.Trim(s.px, ".") == "" {
				t.Fatalf("%s is empty", s.name)
			}
			for p := range len(s.px) {
				if ch := s.px[p]; ch != '.' && strings.IndexByte(dinoPalette, ch) < 0 {
					t.Fatalf("%s pixel %d is %q, not in the palette %q", s.name, p, ch, dinoPalette)
				}
			}
		}
	}
	if i != len(dinoArt) {
		t.Errorf("sprite has %d frames, want %d", len(dinoArt), i)
	}
}

// The dino and the cacti stand on their bottom row, so a sprite placed by
// its bottom edge touches the ground line instead of hovering over it.
func TestDinoArt_StandsOnBottomRow(t *testing.T) {
	for _, s := range dinoArt {
		switch {
		case strings.HasPrefix(s.name, "run"), strings.HasPrefix(s.name, "jump"), strings.HasPrefix(s.name, "duck"),
			strings.HasPrefix(s.name, "cactus"), strings.HasPrefix(s.name, "big"):
			if strings.Trim(s.px[(s.h-1)*s.w:], ".") == "" {
				t.Errorf("%s: bottom row is empty", s.name)
			}
		}
	}
}

func TestDinoBake(t *testing.T) {
	grid := "a." + ".g"
	img := dinoBake(grid, 2, 2, 2)
	if img.w != 4 || img.h != 4 || len(img.px) != 16 {
		t.Fatalf("baked %dx%d with %d pixels, want 4x4 with 16", img.w, img.h, len(img.px))
	}
	a := bambooARGB(dinoColors[strings.IndexByte(dinoPalette, 'a')])
	g := bambooARGB(dinoColors[strings.IndexByte(dinoPalette, 'g')])
	want := []uint32{
		a, a, 0, 0,
		a, a, 0, 0,
		0, 0, g, g,
		0, 0, g, g,
	}
	for i, p := range want {
		if img.px[i] != p {
			t.Fatalf("pixel %d = %#x, want %#x", i, img.px[i], p)
		}
	}
}

// dinoTestW is a 1080p frame width: 1920 view pixels at 4 per frame pixel.
const dinoTestW = 480

func newDinoTestRun(seed uint64) *dinoRun {
	r := &dinoRun{}
	r.init(seed, dinoTestW)
	return r
}

// The run speeds up from the runner's start speed to its cap in about two
// minutes and never slows down or overshoots.
func TestDinoRun_SpeedRampsToTheCap(t *testing.T) {
	r := newDinoTestRun(1)
	if r.speed != dinoStartSpeed {
		t.Fatalf("start speed %g, want %g", r.speed, dinoStartSpeed)
	}
	capped := -1
	for i := range 3 * 60 * dinoFPS {
		prev := r.speed
		r.step()
		if r.speed < prev || r.speed > dinoMaxSpeed {
			t.Fatalf("step %d: speed %g after %g, want monotonic and <= %g", i, r.speed, prev, dinoMaxSpeed)
		}
		if capped < 0 && r.speed == dinoMaxSpeed {
			capped = i + 1
		}
	}
	if capped < 100*dinoFPS || capped > 2*60*dinoFPS {
		t.Errorf("speed capped after %d steps (%.0f s), want 100..120 s", capped, float64(capped)/dinoFPS)
	}
}

// A jump is ballistic: it rises by the start speed, peaks at about 88 frame
// pixels and lands 35 steps later. Pressing again in the air does nothing.
func TestDinoRun_JumpIsBallistic(t *testing.T) {
	r := newDinoTestRun(1)
	r.jump()
	peak, steps := 0.0, 0
	for r.jumping {
		r.step()
		steps++
		if steps == 1 && r.lift != dinoJumpV {
			t.Fatalf("first step rose %g, want %g", r.lift, dinoJumpV)
		}
		if steps == 5 {
			v := r.vy
			r.jump()
			if r.vy != v {
				t.Fatal("a jump in the air changed the vertical speed")
			}
		}
		peak = max(peak, r.lift)
		if steps > 100 {
			t.Fatal("never landed")
		}
	}
	if r.lift != 0 || r.vy != 0 {
		t.Errorf("landed at lift %g with vy %g, want 0 and 0", r.lift, r.vy)
	}
	if steps != dinoAirSteps {
		t.Errorf("airtime %d steps, want %d", steps, dinoAirSteps)
	}
	if peak < 88 || peak > 89 {
		t.Errorf("peak %g, want about 88.4", peak)
	}
}

// Ducking only happens on the ground, and a jump stands the dino up.
func TestDinoRun_Duck(t *testing.T) {
	r := newDinoTestRun(1)
	r.duck(true)
	if !r.ducking {
		t.Fatal("did not duck on the ground")
	}
	r.jump()
	if r.ducking || !r.jumping {
		t.Fatalf("jump from a duck: ducking %v jumping %v, want false true", r.ducking, r.jumping)
	}
	r.duck(true)
	if r.ducking {
		t.Error("ducked in the air")
	}
}

// The score is the distance times the runner's coefficient, five digits,
// wrapping past 99999 so a shield left up for hours keeps counting.
func TestDinoRun_Score(t *testing.T) {
	r := newDinoTestRun(1)
	for _, c := range []struct {
		score, want int
	}{{0, 0}, {1, 1}, {99999, 99999}, {100000, 0}, {100123, 123}, {250042, 50042}} {
		r.dist = float64(c.score) / dinoScoreCoef
		if got := r.score(); got != c.want {
			t.Errorf("distance for %d: score %d, want %d", c.score, got, c.want)
		}
	}
	r = newDinoTestRun(1)
	for range 10 * dinoFPS {
		r.step()
	}
	if want := int(math.Round(r.dist * dinoScoreCoef)); r.score() != want || want == 0 {
		t.Errorf("score %d after %g frame pixels, want %d", r.score(), r.dist, want)
	}
}

// Every hundred points the score blinks off and on three times, showing the
// hundred it reached, then counts on.
func TestDinoRun_ScoreFlashesEveryHundred(t *testing.T) {
	r := newDinoTestRun(1)
	r.dist = 99.4 / dinoScoreCoef
	r.speed = 0.5 / dinoScoreCoef / 10 // crosses 100 within a few steps, slowly after
	var seen []bool
	for range 200 {
		r.step()
		v, on := r.shownScore()
		if r.flash > 0 && v != 100 {
			t.Fatalf("flashing %d, want the hundred reached, 100", v)
		}
		if r.flash > 0 {
			seen = append(seen, on)
		}
	}
	if len(seen) != 2*dinoFlashBlinks*dinoFlashSteps {
		t.Fatalf("flash lasted %d steps, want %d", len(seen), 2*dinoFlashBlinks*dinoFlashSteps)
	}
	for i, on := range seen {
		if want := (i/dinoFlashSteps)%2 == 1; on != want {
			t.Fatalf("flash step %d: shown %v, want %v (off first)", i, on, want)
		}
	}
	if v, on := r.shownScore(); !on || v != r.score() {
		t.Errorf("after the flash shows %d (%v), want the score %d", v, on, r.score())
	}
}

// runDino steps a run, calling each with the run after every step that
// added an obstacle.
func runDino(r *dinoRun, steps int, each func(r *dinoRun)) {
	made := r.made
	for range steps {
		r.step()
		if r.made != made {
			made = r.made
			each(r)
		}
	}
}

// Obstacles sit on the ground line (birds at one of three heights), come in
// from beyond the right edge, and leave a gap after them that the dino can
// always land in and take off from again at the speed it meets them.
func TestDinoObstacles_GapsLetTheDinoLand(t *testing.T) {
	for seed := range uint64(5) {
		r := newDinoTestRun(seed + 1)
		runDino(r, 4*60*dinoFPS, func(r *dinoRun) {
			o := r.obs[len(r.obs)-1]
			if o.x < r.w {
				t.Fatalf("seed %d: obstacle %d spawned at x %g, inside the %g wide frame", seed+1, r.made, o.x, r.w)
			}
			switch o.kind {
			case dinoKindBird:
				if o.top != dinoBirdLow && o.top != dinoBirdMid && o.top != dinoBirdHigh {
					t.Fatalf("seed %d: bird at top %d", seed+1, o.top)
				}
			default:
				if o.top+o.h != dinoGroundY {
					t.Fatalf("seed %d: cactus from %d to %d, want its foot on the ground line %d", seed+1, o.top, o.top+o.h, dinoGroundY)
				}
			}
			if len(r.obs) < 2 {
				return
			}
			p := r.obs[len(r.obs)-2]
			if got := o.x - (p.x + float64(p.w)); math.Abs(got-p.gap) > 1e-6 {
				t.Fatalf("seed %d: obstacle %d starts %g after the previous, want its gap %g", seed+1, r.made, got, p.gap)
			}
			// The dino meets p a little later and faster; dinoMinGap's
			// headroom covers that, so a whole jump at today's speed must fit.
			if need := dinoAirSteps*r.speed + dinoGapMargin; p.gap < need {
				t.Fatalf("seed %d: gap %g at speed %g, want at least %g", seed+1, p.gap, r.speed, need)
			}
		})
	}
}

// Pterodactyls only come once the run is faster than the runner's bird
// speed, then at all three heights; cacti come in groups of up to three.
func TestDinoObstacles_Kinds(t *testing.T) {
	heights := map[int]bool{}
	groups := map[int]bool{}
	for seed := range uint64(10) {
		r := newDinoTestRun(seed + 1)
		runDino(r, 4*60*dinoFPS, func(r *dinoRun) {
			o := r.obs[len(r.obs)-1]
			if o.kind == dinoKindBird {
				if r.speed <= dinoBirdSpeed {
					t.Fatalf("seed %d: bird at speed %g, want only above %g", seed+1, r.speed, dinoBirdSpeed)
				}
				heights[o.top] = true
				return
			}
			if o.n < 1 || o.n > 3 {
				t.Fatalf("seed %d: group of %d cacti", seed+1, o.n)
			}
			groups[o.n] = true
		})
		if len(r.obs) > 8 {
			t.Errorf("seed %d: %d obstacles kept, want the ones gone left dropped", seed+1, len(r.obs))
		}
	}
	if len(heights) != 3 || len(groups) != 3 {
		t.Errorf("bird heights %v and group sizes %v, want 3 of each", heights, groups)
	}
}

// The same seed makes the same obstacles; another seed makes others.
func TestDinoObstacles_SameSeedSameRun(t *testing.T) {
	list := func(seed uint64) []dinoObstacle {
		var out []dinoObstacle
		runDino(newDinoTestRun(seed), 2*60*dinoFPS, func(r *dinoRun) { out = append(out, r.obs[len(r.obs)-1]) })
		return out
	}
	a, b, c := list(7), list(7), list(8)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("%d and %d obstacles from one seed", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("obstacle %d differs for one seed: %+v and %+v", i, a[i], b[i])
		}
	}
	same := len(a) == len(c)
	for i := 0; same && i < len(a); i++ {
		same = a[i] == c[i]
	}
	if same {
		t.Error("seeds 7 and 8 made the same obstacles")
	}
}

// dinoOpaque reports whether the art frame has a pixel at (x, y) frame
// pixels from its top-left corner.
func dinoOpaque(art, x, y int) bool {
	a := &dinoArt[art]
	x, y = x/dinoArtScale, y/dinoArtScale
	return x >= 0 && y >= 0 && x < a.w && y < a.h && a.px[y*a.w+x] != '.'
}

// dinoTouches reports whether the dino, as drawn, shares a pixel with any
// obstacle as drawn.
func dinoTouches(r *dinoRun) bool {
	art0, dx, dy := r.dinoFrame()
	da := &dinoArt[art0]
	dw, dh := da.w*dinoArtScale, da.h*dinoArtScale
	hit := false
	for i := range r.obs {
		r.obs[i].parts(r.steps, func(art, x, y int) {
			a := &dinoArt[art]
			w, h := a.w*dinoArtScale, a.h*dinoArtScale
			x0, x1 := max(dx, x), min(dx+dw, x+w)
			y0, y1 := max(dy, y), min(dy+dh, y+h)
			for py := y0; py < y1 && !hit; py++ {
				for px := x0; px < x1 && !hit; px++ {
					hit = dinoOpaque(art, px-x, py-y) && dinoOpaque(art0, px-dx, py-dy)
				}
			}
		})
	}
	return hit
}

// The autopilot never misses: half an hour of runs on ten seeds, checked on
// every step pixel by pixel, never puts the dino on an obstacle.
func TestDinoPilot_NeverTouches(t *testing.T) {
	for seed := range uint64(10) {
		t.Run(strconv.FormatUint(seed+1, 10), func(t *testing.T) {
			t.Parallel()
			r := newDinoTestRun(seed + 1)
			for i := range 30 * 60 * dinoFPS {
				r.step()
				if dinoTouches(r) {
					o := r.obs[0]
					t.Fatalf("step %d at speed %.2f: dino (lift %.1f, ducking %v) touches %+v",
						i, r.speed, r.lift, r.ducking, o)
				}
			}
			if r.made < 100 {
				t.Errorf("only %d obstacles in half an hour", r.made)
			}
		})
	}
}

// The pilot jumps cacti and low birds, ducks under middle birds and runs
// under high ones.
func TestDinoPilot_Decisions(t *testing.T) {
	cases := []struct {
		name       string
		kind, top  int
		jump, duck bool
	}{
		{"cactus", dinoKindCactus, 0, true, false},
		{"big cactus", dinoKindBig, 0, true, false},
		{"low bird", dinoKindBird, dinoBirdLow, true, false},
		{"middle bird", dinoKindBird, dinoBirdMid, false, true},
		{"high bird", dinoKindBird, dinoBirdHigh, false, false},
	}
	for _, c := range cases {
		for _, speed := range []float64{dinoStartSpeed, dinoBirdSpeed + 0.1, dinoMaxSpeed} {
			r := newDinoTestRun(1)
			r.speed = speed
			art := [3]int{dinoArtCactus}
			switch c.kind {
			case dinoKindBig:
				art[0] = dinoArtBig
			case dinoKindBird:
				art[0] = dinoArtBird
			}
			a := &dinoArt[art[0]]
			o := dinoObstacle{kind: c.kind, n: 1, art: art, x: r.w, top: c.top, w: a.w * dinoArtScale, h: a.h * dinoArtScale, gap: 1e9}
			if c.kind != dinoKindBird {
				o.top = dinoGroundY - o.h
			}
			r.obs = []dinoObstacle{o}
			r.next = 1e9
			jumped, ducked := false, false
			for len(r.obs) > 0 {
				r.step()
				jumped = jumped || r.jumping
				ducked = ducked || r.ducking
				if dinoTouches(r) {
					t.Fatalf("%s at speed %g: touched", c.name, speed)
				}
			}
			if jumped != c.jump || ducked != c.duck {
				t.Errorf("%s at speed %g: jumped %v ducked %v, want %v %v", c.name, speed, jumped, ducked, c.jump, c.duck)
			}
			if r.ducking {
				t.Errorf("%s at speed %g: still ducking after it passed", c.name, speed)
			}
		}
	}
}

// The ground slides with the run; clouds drift the same way, slower.
func TestDinoRun_Parallax(t *testing.T) {
	r := newDinoTestRun(3)
	if len(r.clouds) == 0 {
		t.Fatal("no clouds in the sky at the start")
	}
	c0 := r.clouds[0]
	d0 := r.dist
	r.step()
	moved := c0.x - r.clouds[0].x
	if run := r.dist - d0; moved <= 0 || moved >= run {
		t.Errorf("cloud drifted %g while the ground slid %g, want slower but the same way", moved, run)
	}
	for range 60 * dinoFPS {
		r.step()
		for _, c := range r.clouds {
			if c.y < 0 || c.y+dinoArt[dinoArtCloud].h*dinoArtScale > dinoGroundY-2*dinoArt[dinoArtRun].h {
				t.Fatalf("cloud at y %d, want in the sky above the jumps", c.y)
			}
		}
	}
	if len(r.clouds) == 0 || len(r.clouds) > 10 {
		t.Errorf("%d clouds after a minute, want the sky kept but not crowded", len(r.clouds))
	}
}

const (
	dinoTestViewW = 1920.0
	dinoTestViewH = 1080.0
)

func newDinoTestView() *dinoView {
	v := &dinoView{}
	v.tick(dinoTestViewW, dinoTestViewH, 0)
	return v
}

// The strip is the runner's height in frame pixels, a frame pixel is a whole
// number of view pixels (at least two, about the height over 300) and the
// strip sits in the middle of the screen.
func TestDinoView_Layout(t *testing.T) {
	for _, c := range []struct {
		w, h, unit float64
	}{{1920, 1080, 4}, {2560, 1440, 5}, {3840, 2160, 7}, {1366, 768, 3}, {640, 400, 2}} {
		v := &dinoView{}
		v.tick(c.w, c.h, 0)
		fw, fh := v.frameSize()
		if v.unit() != c.unit || fh != dinoStripH || fw != int(math.Ceil(c.w/c.unit)) {
			t.Errorf("%gx%g: unit %g frame %dx%d, want unit %g frame %dx%d", c.w, c.h, v.unit(), fw, fh, c.unit, int(math.Ceil(c.w/c.unit)), dinoStripH)
		}
		if top, bottom := v.yOffset(), c.h-v.yOffset()-dinoStripH*v.unit(); math.Abs(top-bottom) > 1 {
			t.Errorf("%gx%g: strip %g from the top and %g from the bottom", c.w, c.h, top, bottom)
		}
	}
}

// The model steps at the runner's 60 FPS whatever the display rate, one step
// per frame on a 60 Hz display even when its frame times wobble, so the run
// never stutters; a stall is capped.
func TestDinoView_StepsAt60FPS(t *testing.T) {
	for _, hz := range []float64{30, 60, 144} {
		v := newDinoTestView()
		start := v.r.steps
		for range int(hz) {
			v.tick(dinoTestViewW, dinoTestViewH, 1/hz)
		}
		if n := v.r.steps - start; n < dinoFPS-1 || n > dinoFPS+1 {
			t.Errorf("%g Hz: %d steps in 1 s, want %d", hz, n, dinoFPS)
		}
	}
	rng := uint64(9)
	for _, hz := range []float64{59.94, 60, 60.05, 0} {
		v := newDinoTestView()
		for i := range 2000 {
			dt := 1 / hz
			if hz == 0 { // a wobbling 60 Hz clock
				dt = (1 + 0.08*(goldUniform(&rng)-0.5)) / 60
			}
			before := v.r.steps
			v.tick(dinoTestViewW, dinoTestViewH, dt)
			if n := v.r.steps - before; n != 1 {
				t.Fatalf("%g Hz frame %d: %d steps, want exactly 1", hz, i, n)
			}
		}
	}
	v := newDinoTestView()
	before := v.r.steps
	v.tick(dinoTestViewW, dinoTestViewH, 5)
	if n := v.r.steps - before; n != int64(animMaxDt*dinoFPS) {
		t.Errorf("a 5 s stall made %d steps, want %d", n, int(animMaxDt*dinoFPS))
	}
}

// A frame is only new after a step: a tick too short to step leaves it as it
// was, and the first tick and a resize lay the run out again.
func TestDinoView_Changed(t *testing.T) {
	v := &dinoView{}
	v.tick(0, 0, 0)
	if v.seeded {
		t.Fatal("seeded before the widget had a size")
	}
	v.tick(dinoTestViewW, dinoTestViewH, 0)
	if !v.seeded || !v.changed {
		t.Fatal("first tick did not lay the run out")
	}
	v.changed = false
	v.tick(dinoTestViewW, dinoTestViewH, 0.004)
	if v.changed {
		t.Error("a quarter of a step changed the frame")
	}
	v.tick(dinoTestViewW, dinoTestViewH, 1.0/60)
	if !v.changed {
		t.Error("a step did not change the frame")
	}
	v.changed = false
	seed := v.seed
	v.tick(1280, 720, 0)
	if !v.changed || v.seed != seed {
		t.Error("a resize did not lay the same run out again")
	}
	if fw, _ := v.frameSize(); v.r.w != float64(fw) {
		t.Errorf("run is %g wide after a resize, want the frame width %d", v.r.w, fw)
	}
}

// The frame starts with the opaque back over the whole canvas (the canvas
// is never cleared), draws only baked sprites at their size and only where
// the canvas is, and puts five digits at the top right while they are lit.
func TestDinoView_FrameBlits(t *testing.T) {
	v := newDinoTestView()
	for range 3 * 60 * dinoFPS {
		v.tick(dinoTestViewW, dinoTestViewH, 1.0/60)
	}
	fw, fh := v.frameSize()
	check := func(lit bool) {
		t.Helper()
		bs := v.frame()
		if len(bs) == 0 || bs[0] != (bambooBlit{v.back, 0, 0, fw, fh, 255}) {
			t.Fatalf("first blit %+v, want the back over the %dx%d frame", bs[0], fw, fh)
		}
		digits := 0
		for _, b := range bs[1:] {
			if b.sprite < 0 || b.sprite >= dinoArtCount {
				t.Fatalf("blit of sprite %d, not an art frame", b.sprite)
			}
			im := &v.images[b.sprite]
			if b.w != im.w || b.h != im.h || b.alpha != 255 {
				t.Fatalf("blit %+v of a %dx%d sprite, want 1:1 and opaque", b, im.w, im.h)
			}
			if b.x+b.w <= 0 || b.x >= fw || b.y+b.h <= 0 || b.y >= fh {
				t.Fatalf("blit %+v outside the %dx%d frame", b, fw, fh)
			}
			if b.sprite >= dinoArtDigit {
				digits++
				if b.x < fw/2 || b.y > 20 {
					t.Errorf("digit at %d,%d, want top right", b.x, b.y)
				}
			}
		}
		if want := map[bool]int{true: 5, false: 0}[lit]; digits != want {
			t.Errorf("%d digits drawn, want %d (lit %v)", digits, want, lit)
		}
	}
	v.r.flash = 0
	check(true)
	v.r.flash, v.r.flashScore = 2*dinoFlashBlinks*dinoFlashSteps, 100
	check(false)
}

// The sheet holds every art frame at its index, baked 1:1, and the back.
func TestDinoView_Sheet(t *testing.T) {
	v := newDinoTestView()
	if len(v.images) != dinoArtCount+1 || v.back != dinoArtCount {
		t.Fatalf("%d images with the back at %d, want %d and %d", len(v.images), v.back, dinoArtCount+1, dinoArtCount)
	}
	for i, a := range dinoArt {
		if im := v.images[i]; im.w != a.w*dinoArtScale || im.h != a.h*dinoArtScale {
			t.Errorf("%s baked %dx%d, want %dx%d", a.name, im.w, im.h, a.w*dinoArtScale, a.h*dinoArtScale)
		}
	}
	if b := v.images[v.back]; b.w != 1 || b.h != 1 || b.px[0] != 0xFF000000 {
		t.Errorf("back %dx%d %#x, want opaque black 1x1", b.w, b.h, b.px)
	}
}

// dino is a style the shield draws, animated by a dinoAnim; building one
// touches no GTK, so it is safe without a desktop.
func TestDinoStyle_Registered(t *testing.T) {
	if !slices.Contains(Styles, "dino") {
		t.Fatalf("Styles %v lacks dino", Styles)
	}
	if a, ok := newAnimation("dino", "", nil).(*dinoAnim); !ok || a == nil {
		t.Fatalf("newAnimation(dino) = %T, want *dinoAnim", newAnimation("dino", "", nil))
	}
}
