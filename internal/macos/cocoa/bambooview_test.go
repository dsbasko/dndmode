//go:build darwin

package cocoa

import (
	"math"
	"strings"
	"testing"
)

// The tests below exercise the pure models behind the bamboo overlay style (the
// forest, the falling leaves, the fireflies and the panda, reached through cgo
// shims) and the compiled-in sprite art. The rendered frame is owned by the WindowServer and
// can only be validated in the manual visual run. They pin what makes the walk
// read as a walk: a forest that fills every layer edge to edge, slides left at
// a steady speed that grows with nearness, never runs out or jumps, and sprites
// that tile without seams.

const (
	bambooW = 1512.0
	bambooH = 982.0
)

// bambooSeconds is how many frames the walk takes for s seconds.
func bambooSeconds(s int) int { return s * bambooFPS() }

func TestBambooForest_LayersRecede(t *testing.T) {
	f := newBambooForestForTest(1, bambooW, bambooH)
	defer f.free()
	n := bambooLayerCount()
	if n < 4 {
		t.Fatalf("layers = %d, want at least 4 for the parallax to read as depth", n)
	}
	for i := 1; i < n; i++ {
		far, near := f.layer(i-1), f.layer(i)
		if near.px < far.px {
			t.Errorf("layer %d draws %v pt per art px, layer %d %v: a nearer layer is never smaller", i, near.px, i-1, far.px)
		}
		if !(near.speed > far.speed) {
			t.Errorf("layer %d slides %v pt/frame, layer %d %v: a nearer layer must slide faster", i, near.speed, i-1, far.speed)
		}
		if !(near.base < far.base) {
			t.Errorf("layer %d stands at y = %v, layer %d at %v: a nearer floor is lower on screen", i, near.base, i-1, far.base)
		}
		if near.fog > far.fog {
			t.Errorf("layer %d fog %v, layer %d %v: a nearer layer is never foggier", i, near.fog, i-1, far.fog)
		}
	}
	// Whole points per art pixel only, so no pixel lands between device pixels.
	for i := 0; i < n; i++ {
		if px := f.layer(i).px; px != math.Trunc(px) || px < 1 {
			t.Errorf("layer %d draws %v pt per art px, want a whole number", i, px)
		}
	}
}

func TestBambooForest_FillsEveryLayerEdgeToEdge(t *testing.T) {
	f := newBambooForestForTest(5, bambooW, bambooH)
	defer f.free()
	check := func(when string) {
		// The foreground layer is sparse on purpose; the four behind it must
		// read as a forest from edge to edge.
		for i := 0; i < bambooLayerCount()-1; i++ {
			sp := f.layer(i).spacing
			st := f.stalks(i)
			if len(st) == 0 {
				t.Fatalf("%s: layer %d has no culms", when, i)
			}
			if st[0].x > 3*sp {
				t.Errorf("%s: layer %d starts at x = %v, want a culm within %v of the left edge", when, i, st[0].x, 3*sp)
			}
			if st[len(st)-1].x < bambooW-3*sp {
				t.Errorf("%s: layer %d ends at x = %v, want a culm within %v of the right edge", when, i, st[len(st)-1].x, 3*sp)
			}
			for k := 1; k < len(st); k++ {
				if gap := st[k].x - st[k-1].x; gap > 6*sp {
					t.Errorf("%s: layer %d has a %v-pt clearing between slots %d and %d (spacing %v)", when, i, gap, st[k-1].slot, st[k].slot, sp)
				}
				if st[k].slot <= st[k-1].slot {
					t.Fatalf("%s: layer %d lists slot %d after %d, want left to right", when, i, st[k].slot, st[k-1].slot)
				}
			}
			for _, s := range st {
				top := s.base + float64(s.segs)*24*f.layer(i).px
				if !s.capped && top < bambooH {
					t.Errorf("%s: layer %d culm %d tops out at %v, inside a %v-high view, without a tip", when, i, s.slot, top, bambooH)
				}
				if s.segs < 2 {
					t.Errorf("%s: layer %d culm %d has %d segments", when, i, s.slot, s.segs)
				}
			}
		}
	}
	check("first frame")
	f.skip(1000)
	check("after 1000 frames")
	f.skip(uint64(bambooSeconds(12 * 3600)))
	check("after 12 hours")
}

func TestBambooForest_SlidesSteadilyAndNeverJumps(t *testing.T) {
	f := newBambooForestForTest(9, bambooW, bambooH)
	defer f.free()
	f.skip(uint64(bambooSeconds(3 * 3600)))
	for i := 0; i < bambooLayerCount(); i++ {
		speed := f.layer(i).speed
		before := make(map[int64]float64)
		for _, s := range f.stalks(i) {
			before[s.slot] = s.x
		}
		f.skip(1)
		matched := 0
		for _, s := range f.stalks(i) {
			x, ok := before[s.slot]
			if !ok {
				continue
			}
			matched++
			if d := x - s.x; math.Abs(d-speed) > 1e-6 {
				t.Errorf("layer %d culm %d moved %v pt in one frame, want the layer speed %v", i, s.slot, d, speed)
			}
		}
		if matched == 0 {
			t.Errorf("layer %d: no culm survived one frame", i)
		}
	}
}

// A culm is drawn as a stack of blits. Every piece of it must stand on the one
// below at the same x, and every leaf cluster must hang from a node of that same
// culm, in every frame: a piece that is laid out on its own can fall behind the
// rest of the culm and tear it apart.
func TestBambooForest_CulmIsRigid(t *testing.T) {
	f := newBambooForestForTest(13, bambooW, bambooH)
	defer f.free()
	for step := 0; step < 40; step++ {
		f.skip(uint64(bambooSeconds(1)) + 3)
		for i := 0; i < bambooLayerCount(); i++ {
			px := f.layer(i).px
			segH := 24 * px
			for k, s := range f.stalks(i) {
				pieces := f.culm(i, k)
				if len(pieces) == 0 || pieces[0].kind == bambooPieceLeaf {
					t.Fatalf("layer %d culm %d: no culm pieces", i, s.slot)
				}
				left := s.x - 6*px
				y := s.base
				for _, pc := range pieces {
					if pc.kind == bambooPieceLeaf {
						want := left + 9*px
						if pc.mirror {
							want = left + 2*px - 24*px
						}
						if pc.x != want {
							t.Fatalf("layer %d culm %d: leaf at x = %v, want %v next to the culm", i, s.slot, pc.x, want)
						}
						node := math.Round((pc.y + 10*px - s.base) / segH)
						if math.Abs(s.base+node*segH-(pc.y+10*px)) > 1e-6 || node < 1 || int(node) >= s.segs {
							t.Fatalf("layer %d culm %d: leaf twig at y = %v is not on a node of the culm", i, s.slot, pc.y+10*px)
						}
						continue
					}
					if pc.x != left {
						t.Fatalf("layer %d culm %d: a piece at x = %v, the culm at %v: it tore", i, s.slot, pc.x, left)
					}
					if pc.y != y {
						t.Fatalf("layer %d culm %d: a piece starts at y = %v, the one below ends at %v", i, s.slot, pc.y, y)
					}
					y += pc.h
				}
				if top := s.base + float64(s.segs)*segH; y < math.Min(top, bambooH) {
					t.Errorf("layer %d culm %d: pieces end at y = %v, the culm at %v", i, s.slot, y, math.Min(top, bambooH))
				}
			}
		}
	}
}

func TestBambooForest_SameSeedSameForest(t *testing.T) {
	list := func(seed uint64) []bambooStalk {
		f := newBambooForestForTest(seed, bambooW, bambooH)
		defer f.free()
		f.skip(5000)
		return f.stalks(2)
	}
	a, b, c := list(42), list(42), list(43)
	if len(a) != len(b) {
		t.Fatalf("same seed: %d vs %d culms", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("culm %d differs between runs with the same seed: %+v vs %+v", i, a[i], b[i])
		}
	}
	same := len(a) == len(c)
	for i := 0; same && i < len(a); i++ {
		same = a[i].x == c[i].x
	}
	if same {
		t.Errorf("different seeds grew the same forest")
	}
}

func TestBambooFalls_DriftDownAndRespawnUpwind(t *testing.T) {
	f := newBambooForestForTest(17, bambooW, bambooH)
	defer f.free()
	prev := f.falls()
	if len(prev) < 20 {
		t.Fatalf("falling leaves on a laptop = %d, want a few dozen", len(prev))
	}
	var thirds [3]int
	for _, l := range prev {
		if l.y >= 0 && l.y < bambooH {
			thirds[int(3*l.y/bambooH)]++
		}
	}
	for i, n := range thirds {
		if n == 0 {
			t.Errorf("no leaf in third %d of the view on the first frame (%v)", i, thirds)
		}
	}
	for frame := 0; frame < bambooSeconds(150); frame++ {
		f.step(1)
		cur := f.falls()
		for i, l := range cur {
			if l.spawns == prev[i].spawns {
				if !(l.y < prev[i].y) {
					t.Fatalf("frame %d, leaf %d: y went from %v to %v; a leaf only falls", frame, i, prev[i].y, l.y)
				}
				continue
			}
			above := l.y-l.reach > bambooH
			right := l.x-l.reach > bambooW
			if !above && !right {
				t.Fatalf("frame %d, leaf %d respawned at (%v, %v): it must start out of sight above or right of the view", frame, i, l.x, l.y)
			}
		}
		prev = cur
	}
	frames := make(map[int]bool)
	for i, l := range prev {
		if l.spawns == 0 {
			t.Errorf("leaf %d never left the view in 150 s (at %v, %v)", i, l.x, l.y)
		}
		frames[l.frame] = true
	}
	if len(frames) < 2 {
		t.Errorf("leaves show frames %v, want them tumbling", frames)
	}
}

func TestBambooFlies_StayLowBlinkAndReturn(t *testing.T) {
	f := newBambooForestForTest(23, bambooW, bambooH)
	defer f.free()
	flies := f.flies()
	if len(flies) < 6 {
		t.Fatalf("fireflies on a laptop = %d, want a handful at least", len(flies))
	}
	lit := make([]bool, len(flies))
	dark := make([]bool, len(flies))
	for frame := 0; frame < bambooSeconds(240); frame++ {
		f.step(1)
		for i, b := range f.flies() {
			if b.y < 0 || b.y > bambooH*0.6 {
				t.Fatalf("frame %d, firefly %d at y = %v: they keep to the low part of the view", frame, i, b.y)
			}
			if b.glow < 0 || b.glow > 1 {
				t.Fatalf("frame %d, firefly %d glows %v, want 0..1", frame, i, b.glow)
			}
			if b.x < -b.reach || b.x > 1.3*bambooW+b.reach {
				t.Fatalf("frame %d, firefly %d at x = %v, far outside the view", frame, i, b.x)
			}
			lit[i] = lit[i] || b.glow > 0.9
			dark[i] = dark[i] || b.glow < 0.05
		}
	}
	for i, b := range f.flies() {
		if !lit[i] || !dark[i] {
			t.Errorf("firefly %d: lit %v, dark %v in 4 minutes; want it to blink", i, lit[i], dark[i])
		}
		if b.spawns == 0 {
			t.Errorf("firefly %d never drifted off and came back in 4 minutes", i)
		}
	}
}

// The panda wanders on one far layer: it stands on that layer's floor, slides
// with it exactly while it stands or sits (its feet never skate), walks both
// ways at one pace, always drifts left on screen because it is slower than the
// walk, and once it is gone it comes back out of sight past the right edge.
func TestBambooPanda_WandersOnTheFloorAndComesBack(t *testing.T) {
	f := newBambooForestForTest(29, bambooW, bambooH)
	defer f.free()
	a := f.panda()
	if a.state == bambooPandaAway || a.x-a.reach < 0 || a.x+a.reach > bambooW {
		t.Fatalf("first frame: panda %+v, want it in full view", a)
	}
	L := f.layer(a.layer)
	if !(L.fog > f.layer(bambooLayerCount()-1).fog) || a.layer > bambooLayerCount()/2 {
		t.Errorf("panda on layer %d: it belongs far off in the forest", a.layer)
	}
	states := make(map[int]bool)
	dirs := make(map[int]bool)
	walkArt := make(map[int]bool)
	sitArt := make(map[int]bool)
	pace := -1.0
	visible := 0
	total := bambooSeconds(40 * 60)
	for frame := 0; frame < total; frame++ {
		prev, prevBase := a, L.base
		f.step(1)
		a = f.panda()
		L = f.layer(a.layer)
		states[a.state] = true
		if a.state == bambooPandaAway {
			continue
		}
		// Its feet stand in the ground strip (base - 7 px .. base + 5 px), low
		// enough for the grass to hide its paws.
		if a.foot != prev.foot+(L.base-prevBase) || a.foot >= L.base || a.foot <= L.base-7*L.px {
			t.Fatalf("frame %d: panda's feet at y = %v, its floor at %v", frame, a.foot, L.base)
		}
		if a.x+a.reach > 0 && a.x-a.reach < bambooW {
			visible++
		}
		if a.visits != prev.visits {
			if a.x-a.reach < bambooW {
				t.Fatalf("frame %d: panda came back at x = %v, in sight", frame, a.x)
			}
			continue
		}
		moved := prev.x - a.x
		switch a.state {
		case bambooPandaWalk:
			dirs[a.dir] = true
			walkArt[a.art] = true
			if prev.state != bambooPandaWalk {
				continue
			}
			own := math.Abs(moved - L.speed)
			if own < 1e-9 || moved <= 0 {
				t.Fatalf("frame %d: a walking panda moved %v pt, the floor %v", frame, moved, L.speed)
			}
			if pace < 0 {
				pace = own
			} else if math.Abs(own-pace) > 1e-6 {
				t.Fatalf("frame %d: panda walks %v pt per frame, before %v", frame, own, pace)
			}
		case bambooPandaStand, bambooPandaSit:
			if a.state == bambooPandaSit {
				sitArt[a.art] = true
			}
			if prev.state != bambooPandaWalk && math.Abs(moved-L.speed) > 1e-6 {
				t.Fatalf("frame %d: a resting panda moved %v pt, the floor %v: its feet slid", frame, moved, L.speed)
			}
		}
	}
	for _, st := range []int{bambooPandaAway, bambooPandaWalk, bambooPandaStand, bambooPandaSit} {
		if !states[st] {
			t.Errorf("in 40 minutes the panda never was in state %d (%v)", st, states)
		}
	}
	if !dirs[-1] || !dirs[1] {
		t.Errorf("panda walked %v, want both ways", dirs)
	}
	if len(walkArt) != 4 || len(sitArt) != 2 {
		t.Errorf("panda frames seen: walking %v, sitting %v; want the whole gait and the chewing", walkArt, sitArt)
	}
	if a.visits < 3 {
		t.Errorf("panda came back %d times in 40 minutes, want it to keep wandering by", a.visits)
	}
	if share := float64(visible) / float64(total); share < 0.4 || share > 0.95 {
		t.Errorf("panda in sight %.0f%% of the time, want most of it but not all", 100*share)
	}
}

func TestBambooParticles_CountScalesWithScreen(t *testing.T) {
	count := func(w, h float64) (int, int) {
		f := newBambooForestForTest(1, w, h)
		defer f.free()
		return len(f.falls()), len(f.flies())
	}
	sl, sf := count(800, 500)
	ll, lf := count(bambooW, bambooH)
	bl, bf := count(5120, 2880)
	if !(sl < ll && ll <= bl) || !(sf <= lf && lf <= bf) {
		t.Errorf("leaves/fireflies: 800x500 = %d/%d, laptop = %d/%d, 5K = %d/%d; want more on a bigger screen", sl, sf, ll, lf, bl, bf)
	}
	if tl, tf := count(0, 0); tl < 1 || tf < 1 {
		t.Errorf("an empty view holds %d leaves and %d fireflies, want the minimum population", tl, tf)
	}
}

// bambooFrame finds a sprite frame by name.
func bambooFrame(t *testing.T, frames []bambooSprite, name string) bambooSprite {
	t.Helper()
	for _, s := range frames {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no sprite frame %q", name)
	return bambooSprite{}
}

func (s bambooSprite) at(x, y int) byte { return s.px[y*s.w+x] }

func TestBambooArt_IsWellFormed(t *testing.T) {
	frames, palette, emissive := bambooArt()
	if strings.Contains(palette, ".") || len(palette) == 0 {
		t.Fatalf("palette %q: want colors only, '.' is transparent", palette)
	}
	for _, ch := range emissive {
		if !strings.ContainsRune(palette, ch) {
			t.Errorf("emissive %q is not in the palette %q", ch, palette)
		}
	}
	want := []struct {
		prefix string
		n      int
		w, h   int
	}{
		{"stalk", 4, 12, 24}, {"top", 2, 12, 24}, {"leaf", 6, 24, 16}, {"ground", 2, 32, 12},
		{"grass", 3, 12, 8}, {"fall", 4, 7, 7}, {"moon", 1, 24, 24}, {"firefly", 2, 5, 5},
		{"pwalk", 4, 30, 18}, {"pstand", 1, 30, 18}, {"psit", 2, 30, 18},
	}
	i := 0
	for _, w := range want {
		for k := 0; k < w.n; k++ {
			if i >= len(frames) {
				t.Fatalf("sprite has %d frames, want more", len(frames))
			}
			s := frames[i]
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
			for p := 0; p < len(s.px); p++ {
				if ch := s.px[p]; ch != '.' && !strings.ContainsRune(palette, rune(ch)) {
					t.Fatalf("%s pixel %d is %q, not in the palette %q", s.name, p, ch, palette)
				}
			}
		}
	}
	if i != len(frames) {
		t.Errorf("sprite has %d frames, want %d", len(frames), i)
	}
}

// A culm is a stack of segments, so every segment must be solid across the culm
// columns 3..8 in every row, and the top row of one must meet the bottom row of
// any other without a step.
func TestBambooArt_CulmSegmentsTile(t *testing.T) {
	frames, _, _ := bambooArt()
	var segs []bambooSprite
	for _, s := range frames {
		if strings.HasPrefix(s.name, "stalk") {
			segs = append(segs, s)
		}
	}
	for _, s := range segs {
		for y := 0; y < s.h; y++ {
			for x := 3; x <= 8; x++ {
				if s.at(x, y) == '.' {
					t.Fatalf("%s has a hole at column %d, row %d of the culm", s.name, x, y)
				}
			}
		}
		// Nothing of the culm itself outside columns 2..9 except in the node rows
		// and branch stubs, which stand off to the side: row 0 is the plain culm.
		for x := 0; x < s.w; x++ {
			if solid := s.at(x, 0) != '.'; solid != (x >= 3 && x <= 8) {
				t.Errorf("%s row 0 column %d solid = %v, want the culm exactly in columns 3..8", s.name, x, solid)
			}
		}
	}
	for _, tip := range []string{"top0", "top1"} {
		s := bambooFrame(t, frames, tip)
		for x := 3; x <= 8; x++ {
			if s.at(x, s.h-1) == '.' {
				t.Errorf("%s bottom row has a gap at column %d: it must sit flush on a segment", tip, x)
			}
		}
	}
}

func TestBambooArt_GroundTilesAndLeavesAttach(t *testing.T) {
	frames, _, _ := bambooArt()
	for _, name := range []string{"ground0", "ground1"} {
		s := bambooFrame(t, frames, name)
		for y := s.h - 4; y < s.h; y++ {
			for x := 0; x < s.w; x++ {
				if s.at(x, y) == '.' {
					t.Fatalf("%s has a hole at (%d, %d) in its solid bottom rows", name, x, y)
				}
			}
		}
	}
	for k := 0; k < 6; k++ {
		s := bambooFrame(t, frames, "leaf"+string(rune('0'+k)))
		attached := false
		for y := 1; y <= 6; y++ {
			attached = attached || s.at(0, y) != '.'
		}
		if !attached {
			t.Errorf("leaf%d has no twig at column 0 near the top: it would float beside the culm", k)
		}
	}
}
