//go:build linux

package overlay

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The model cases are the macOS TestBamboo* ones (cocoa/bambooview_test.go),
// run against the Go twin of the C model, so both implementations are pinned
// to the same behaviour: a forest that fills every layer edge to edge,
// slides left at a steady speed that grows with nearness, never runs out or
// jumps, and sprites that tile without seams.

const (
	bambooW = 1512.0
	bambooH = 982.0
)

// bambooSeconds is how many frames the walk takes for s seconds.
func bambooSeconds(s int) int { return s * bambooFPS }

func newBambooForest(seed uint64, w, h float64) *bambooForest {
	f := &bambooForest{}
	f.init(seed, w, h)
	return f
}

// skip moves the forest n frames on without stepping the particles.
func (f *bambooForest) skip(n uint64) { f.frame += n }

func (f *bambooForest) stepN(n int) {
	for range n {
		f.step()
	}
}

type bambooLayerView struct {
	px, speed, base, spacing, light, fog float64
}

func (f *bambooForest) layer(i int) bambooLayerView {
	L := &bambooLayerTable[i]
	return bambooLayerView{f.px(i), f.speed(i), f.base(i), f.spacing(i), L.light, L.fog}
}

func (f *bambooForest) list(i int) []bambooStalk { return f.stalks(i, nil) }

func (f *bambooForest) culmOf(i, k int) []bambooPiece {
	st := f.list(i)
	return f.culm(i, &st[k], nil)
}

type bambooPandaView struct {
	layer           int
	x, foot, reach  float64
	state, dir, art int
	visits          uint
}

func (f *bambooForest) pandaView() bambooPandaView {
	return bambooPandaView{
		layer: bambooPandaLayer, x: f.pandaX(), foot: f.pandaFoot(), reach: f.pandaReach(),
		state: f.panda.state, dir: f.panda.dir, art: f.panda.art(), visits: f.panda.visits,
	}
}

type bambooParticle struct {
	x, y, reach float64
	frame       int
	glow        float64
	spawns      uint
}

func (f *bambooForest) falls() []bambooParticle {
	out := make([]bambooParticle, len(f.fall))
	for i := range f.fall {
		l := &f.fall[i]
		out[i] = bambooParticle{x: l.x, y: l.y, reach: f.fallReach(l), frame: l.frame(), spawns: l.spawns}
	}
	return out
}

func (f *bambooForest) flies() []bambooParticle {
	out := make([]bambooParticle, len(f.fly))
	for i := range f.fly {
		b := &f.fly[i]
		out[i] = bambooParticle{x: b.x, y: b.y, reach: f.flyReach(b), glow: b.glow(), spawns: b.spawns}
	}
	return out
}

func TestBambooForest_LayersRecede(t *testing.T) {
	f := newBambooForest(1, bambooW, bambooH)
	for i := 1; i < bambooLayers; i++ {
		far, near := f.layer(i-1), f.layer(i)
		if near.px < far.px {
			t.Errorf("layer %d draws %v px per art px, layer %d %v: a nearer layer is never smaller", i, near.px, i-1, far.px)
		}
		if !(near.speed > far.speed) {
			t.Errorf("layer %d slides %v px/frame, layer %d %v: a nearer layer must slide faster", i, near.speed, i-1, far.speed)
		}
		if !(near.base < far.base) {
			t.Errorf("layer %d stands at y = %v, layer %d at %v: a nearer floor is lower on screen", i, near.base, i-1, far.base)
		}
		if near.fog > far.fog {
			t.Errorf("layer %d fog %v, layer %d %v: a nearer layer is never foggier", i, near.fog, i-1, far.fog)
		}
	}
	for i := range bambooLayers {
		if px := f.layer(i).px; px != math.Trunc(px) || px < 1 {
			t.Errorf("layer %d draws %v px per art px, want a whole number", i, px)
		}
	}
}

func TestBambooForest_FillsEveryLayerEdgeToEdge(t *testing.T) {
	f := newBambooForest(5, bambooW, bambooH)
	check := func(when string) {
		// The foreground layer is sparse on purpose; the four behind it must
		// read as a forest from edge to edge.
		for i := range bambooLayers - 1 {
			sp := f.layer(i).spacing
			st := f.list(i)
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
					t.Errorf("%s: layer %d has a %v-px clearing between slots %d and %d (spacing %v)", when, i, gap, st[k-1].slot, st[k].slot, sp)
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
	f := newBambooForest(9, bambooW, bambooH)
	f.skip(uint64(bambooSeconds(3 * 3600)))
	for i := range bambooLayers {
		speed := f.layer(i).speed
		before := make(map[int64]float64)
		for _, s := range f.list(i) {
			before[s.slot] = s.x
		}
		f.skip(1)
		matched := 0
		for _, s := range f.list(i) {
			x, ok := before[s.slot]
			if !ok {
				continue
			}
			matched++
			if d := x - s.x; math.Abs(d-speed) > 1e-6 {
				t.Errorf("layer %d culm %d moved %v px in one frame, want the layer speed %v", i, s.slot, d, speed)
			}
		}
		if matched == 0 {
			t.Errorf("layer %d: no culm survived one frame", i)
		}
	}
}

// Every piece of a culm must stand on the one below at the same x, and every
// leaf cluster must hang from a node of that same culm, in every frame.
func TestBambooForest_CulmIsRigid(t *testing.T) {
	f := newBambooForest(13, bambooW, bambooH)
	for range 40 {
		f.skip(uint64(bambooSeconds(1)) + 3)
		for i := range bambooLayers {
			px := f.layer(i).px
			segH := 24 * px
			for k, s := range f.list(i) {
				pieces := f.culmOf(i, k)
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
		f := newBambooForest(seed, bambooW, bambooH)
		f.skip(5000)
		return f.list(2)
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
	f := newBambooForest(17, bambooW, bambooH)
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
	for fr := range bambooSeconds(150) {
		f.step()
		cur := f.falls()
		for i, l := range cur {
			if l.spawns == prev[i].spawns {
				if !(l.y < prev[i].y) {
					t.Fatalf("frame %d, leaf %d: y went from %v to %v; a leaf only falls", fr, i, prev[i].y, l.y)
				}
				continue
			}
			above := l.y-l.reach > bambooH
			right := l.x-l.reach > bambooW
			if !above && !right {
				t.Fatalf("frame %d, leaf %d respawned at (%v, %v): it must start out of sight above or right of the view", fr, i, l.x, l.y)
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
	f := newBambooForest(23, bambooW, bambooH)
	flies := f.flies()
	if len(flies) < 6 {
		t.Fatalf("fireflies on a laptop = %d, want a handful at least", len(flies))
	}
	lit := make([]bool, len(flies))
	dark := make([]bool, len(flies))
	for fr := range bambooSeconds(240) {
		f.step()
		for i, b := range f.flies() {
			if b.y < 0 || b.y > bambooH*0.6 {
				t.Fatalf("frame %d, firefly %d at y = %v: they keep to the low part of the view", fr, i, b.y)
			}
			if b.glow < 0 || b.glow > 1 {
				t.Fatalf("frame %d, firefly %d glows %v, want 0..1", fr, i, b.glow)
			}
			if b.x < -b.reach || b.x > 1.3*bambooW+b.reach {
				t.Fatalf("frame %d, firefly %d at x = %v, far outside the view", fr, i, b.x)
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

// The panda wanders on one far layer: it stands on that layer's floor,
// slides with it exactly while it stands or sits, walks both ways at one
// pace, always drifts left on screen, and once it is gone it comes back out
// of sight past the right edge.
func TestBambooPanda_WandersOnTheFloorAndComesBack(t *testing.T) {
	f := newBambooForest(29, bambooW, bambooH)
	a := f.pandaView()
	if a.state == bambooPandaAway || a.x-a.reach < 0 || a.x+a.reach > bambooW {
		t.Fatalf("first frame: panda %+v, want it in full view", a)
	}
	L := f.layer(a.layer)
	if !(L.fog > f.layer(bambooLayers-1).fog) || a.layer > bambooLayers/2 {
		t.Errorf("panda on layer %d: it belongs far off in the forest", a.layer)
	}
	states := make(map[int]bool)
	dirs := make(map[int]bool)
	walkArt := make(map[int]bool)
	sitArt := make(map[int]bool)
	pace := -1.0
	visible := 0
	total := bambooSeconds(40 * 60)
	for fr := range total {
		prev, prevBase := a, L.base
		f.step()
		a = f.pandaView()
		L = f.layer(a.layer)
		states[a.state] = true
		if a.state == bambooPandaAway {
			continue
		}
		if a.foot != prev.foot+(L.base-prevBase) || a.foot >= L.base || a.foot <= L.base-7*L.px {
			t.Fatalf("frame %d: panda's feet at y = %v, its floor at %v", fr, a.foot, L.base)
		}
		if a.x+a.reach > 0 && a.x-a.reach < bambooW {
			visible++
		}
		if a.visits != prev.visits {
			if a.x-a.reach < bambooW {
				t.Fatalf("frame %d: panda came back at x = %v, in sight", fr, a.x)
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
				t.Fatalf("frame %d: a walking panda moved %v px, the floor %v", fr, moved, L.speed)
			}
			if pace < 0 {
				pace = own
			} else if math.Abs(own-pace) > 1e-6 {
				t.Fatalf("frame %d: panda walks %v px per frame, before %v", fr, own, pace)
			}
		case bambooPandaStand, bambooPandaSit:
			if a.state == bambooPandaSit {
				sitArt[a.art] = true
			}
			if prev.state != bambooPandaWalk && math.Abs(moved-L.speed) > 1e-6 {
				t.Fatalf("frame %d: a resting panda moved %v px, the floor %v: its feet slid", fr, moved, L.speed)
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
		f := newBambooForest(1, w, h)
		return len(f.fall), len(f.fly)
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

func bambooFrame(t *testing.T, name string) bambooArtFrame {
	t.Helper()
	for _, s := range bambooArt {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no sprite frame %q", name)
	return bambooArtFrame{}
}

func (s bambooArtFrame) at(x, y int) byte { return s.px[y*s.w+x] }

func TestBambooArt_IsWellFormed(t *testing.T) {
	if strings.Contains(bambooPalette, ".") || len(bambooColors) != len(bambooPalette) {
		t.Fatalf("palette %q with %d colours: want colours only, '.' is transparent", bambooPalette, len(bambooColors))
	}
	for _, ch := range bambooEmissive {
		if !strings.ContainsRune(bambooPalette, ch) {
			t.Errorf("emissive %q is not in the palette %q", ch, bambooPalette)
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
		for range w.n {
			s := bambooArt[i]
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
				if ch := s.px[p]; ch != '.' && strings.IndexByte(bambooPalette, ch) < 0 {
					t.Fatalf("%s pixel %d is %q, not in the palette %q", s.name, p, ch, bambooPalette)
				}
			}
		}
	}
	if i != len(bambooArt) {
		t.Errorf("sprite has %d frames, want %d", len(bambooArt), i)
	}
}

// Every segment must be solid across the culm columns 3..8 in every row, and
// the top row of one must meet the bottom row of any other without a step.
func TestBambooArt_CulmSegmentsTile(t *testing.T) {
	for _, s := range bambooArt[bambooArtStalk : bambooArtStalk+bambooStalks] {
		for y := range s.h {
			for x := 3; x <= 8; x++ {
				if s.at(x, y) == '.' {
					t.Fatalf("%s has a hole at column %d, row %d of the culm", s.name, x, y)
				}
			}
		}
		for x := range s.w {
			if solid := s.at(x, 0) != '.'; solid != (x >= 3 && x <= 8) {
				t.Errorf("%s row 0 column %d solid = %v, want the culm exactly in columns 3..8", s.name, x, solid)
			}
		}
	}
	for _, tip := range []string{"top0", "top1"} {
		s := bambooFrame(t, tip)
		for x := 3; x <= 8; x++ {
			if s.at(x, s.h-1) == '.' {
				t.Errorf("%s bottom row has a gap at column %d: it must sit flush on a segment", tip, x)
			}
		}
	}
}

func TestBambooArt_GroundTilesAndLeavesAttach(t *testing.T) {
	for _, name := range []string{"ground0", "ground1"} {
		s := bambooFrame(t, name)
		for y := s.h - 4; y < s.h; y++ {
			for x := range s.w {
				if s.at(x, y) == '.' {
					t.Fatalf("%s has a hole at (%d, %d) in its solid bottom rows", name, x, y)
				}
			}
		}
	}
	for k := range bambooLeaves {
		s := bambooFrame(t, "leaf"+strconv.Itoa(k))
		attached := false
		for y := 1; y <= 6; y++ {
			attached = attached || s.at(0, y) != '.'
		}
		if !attached {
			t.Errorf("leaf%d has no twig at column 0 near the top: it would float beside the culm", k)
		}
	}
}

// TestBambooArt_MatchesMacOS keeps bamboo_art.go identical to the art block
// of the macOS view: palette, colours, emissive set and every frame.
func TestBambooArt_MatchesMacOS(t *testing.T) {
	src, err := os.ReadFile("../../macos/cocoa/bambooview_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start, end := strings.Index(s, "@@BAMBOO_ART_BEGIN@@"), strings.Index(s, "@@BAMBOO_ART_END@@")
	if start < 0 || end < start {
		t.Fatal("art block not found in bambooview_darwin.m")
	}
	block := s[start:end]
	if m := regexp.MustCompile(`kBambooPalette\[\] = "([^"]+)"`).FindStringSubmatch(block); m == nil || m[1] != bambooPalette {
		t.Errorf("palette differs from macOS: %v", m)
	}
	if m := regexp.MustCompile(`kBambooEmissive\[\] = "([^"]+)"`).FindStringSubmatch(block); m == nil || m[1] != bambooEmissive {
		t.Errorf("emissive set differs from macOS: %v", m)
	}
	colors := regexp.MustCompile(`\{ ([0-9.]+), ([0-9.]+), ([0-9.]+) \}, // (\w)`).FindAllStringSubmatch(block, -1)
	if len(colors) != len(bambooColors) {
		t.Fatalf("macOS has %d colours, Go %d", len(colors), len(bambooColors))
	}
	for i, m := range colors {
		r, _ := strconv.ParseFloat(m[1], 64)
		g, _ := strconv.ParseFloat(m[2], 64)
		b, _ := strconv.ParseFloat(m[3], 64)
		if (dvdRGB{r, g, b}) != bambooColors[i] || m[4][0] != bambooPalette[i] {
			t.Errorf("colour %d (%s) differs from macOS", i, m[4])
		}
	}
	head := regexp.MustCompile(`^\s*\{ "(\w+)", (\d+), (\d+),`)
	row := regexp.MustCompile(`^\s*"([^"]*)"`)
	var frames []bambooArtFrame
	for _, line := range strings.Split(block, "\n") {
		if m := head.FindStringSubmatch(line); m != nil {
			w, _ := strconv.Atoi(m[2])
			h, _ := strconv.Atoi(m[3])
			frames = append(frames, bambooArtFrame{name: m[1], w: w, h: h})
			continue
		}
		if m := row.FindStringSubmatch(line); m != nil && len(frames) > 0 {
			frames[len(frames)-1].px += m[1]
		}
	}
	if len(frames) != len(bambooArt) {
		t.Fatalf("parsed %d macOS frames, want %d", len(frames), len(bambooArt))
	}
	for i := range frames {
		if frames[i] != bambooArt[i] {
			t.Errorf("frame %d (%s) differs from the macOS sprite", i, frames[i].name)
		}
	}
}

// --- Linux rendering ---

// TestBambooSheet_Baked: every sprite is baked at its layer's scale, the
// panda only for its layer, and the soil is one opaque pixel.
func TestBambooSheet_Baked(t *testing.T) {
	var s bambooSheet
	s.bakeLayers(7)
	for layer := range bambooLayers {
		L := bambooLayerTable[layer]
		if im := s.images[s.chunk[layer][0]]; im.w != bambooCellW*L.scale || im.h != bambooSegH*L.chunk*L.scale {
			t.Errorf("layer %d chunk is %dx%d", layer, im.w, im.h)
		}
		if im := s.images[s.leaf[layer][2][1]]; im.w != bambooLeafW*L.scale || len(im.px) != im.w*im.h {
			t.Errorf("layer %d mirrored leaf is %dx%d with %d pixels", layer, im.w, im.h, len(im.px))
		}
		if im := s.images[s.soil[layer]]; im.w != 1 || im.h != 1 || im.px[0]>>24 != 0xFF {
			t.Errorf("layer %d soil = %+v", layer, im)
		}
	}
	// A mirrored leaf is the plain one flipped.
	a, b := s.images[s.leaf[3][0][0]], s.images[s.leaf[3][0][1]]
	for y := range a.h {
		for x := range a.w {
			if a.px[y*a.w+x] != b.px[y*b.w+b.w-1-x] {
				t.Fatalf("mirrored leaf differs at (%d, %d)", x, y)
			}
		}
	}
	// Far layers are darker: the same culm pixel dims with depth.
	far, near := s.images[s.seg[0][0]], s.images[s.seg[3][0]]
	if (far.px[4]>>8)&0xFF >= (near.px[3*4]>>8)&0xFF {
		t.Errorf("far culm green %#x is not darker than near %#x", far.px[4], near.px[3*4])
	}
}

func TestBambooStatics(t *testing.T) {
	back := paintBambooBack(3, bambooW, bambooH, 1)
	if back.w != int(bambooW) || back.h != int(bambooH) {
		t.Fatalf("back is %dx%d", back.w, back.h)
	}
	for i, p := range back.px {
		if p>>24 != 0xFF {
			t.Fatalf("back pixel %d = %#x, want opaque", i, p)
		}
	}
	// The moon (silver) is up and to the left; the top-right corner is night.
	cx, cy := int(math.Floor(bambooW*0.2)), back.h-1-int(math.Floor(bambooH*0.78))
	if moon := back.px[cy*back.w+cx]; (moon>>16)&0xFF < 0x80 {
		t.Errorf("moon center = %#x, want silver", moon)
	}
	if corner := back.px[back.w-1]; corner&0xFFFFFF > 0x101010 {
		t.Errorf("top-right corner = %#x, want night", corner)
	}
	mist := paintBambooMist(bambooW, bambooH, 1)
	top, mid, bottom := mist.px[0]>>24, mist.px[int(float64(mist.h)*0.72)*mist.w]>>24, mist.px[(mist.h-1)*mist.w]>>24
	if !(mid > top && mid > bottom) || mid > 0x50 {
		t.Errorf("mist alpha top %d, densest %d, bottom %d: want a faint band, densest low down", top, mid, bottom)
	}
}

// TestBambooView_StepsAt15FPS: at any refresh rate the walk takes 15 steps a
// second, and only those ticks change the frame.
func TestBambooView_StepsAt15FPS(t *testing.T) {
	for _, hz := range []float64{30, 60, 144} {
		v := &bambooView{}
		v.tick(0, 0, 0)
		if v.seeded {
			t.Fatal("seeded before the widget had a size")
		}
		v.tick(bambooW, bambooH, 0)
		if !v.seeded || !v.changed {
			t.Fatalf("%g Hz: first tick did not seed the walk", hz)
		}
		v.changed = false
		changes := 0
		// A tick over a second, so rounding in the summed dt cannot miss a step.
		for range int(hz) + 1 {
			v.tick(bambooW, bambooH, 1/hz)
			if v.changed {
				changes++
				v.changed = false
			}
		}
		if v.f.frame != bambooFPS || changes != bambooFPS {
			t.Errorf("%g Hz: %d steps and %d changed frames in 1 s, want %d", hz, v.f.frame, changes, bambooFPS)
		}
	}
}

// TestBambooView_FrameBlits: the frame starts with the opaque back, draws
// only baked sprites, and every 1:1 blit has its sprite's size.
func TestBambooView_FrameBlits(t *testing.T) {
	v := &bambooView{}
	v.tick(bambooW, bambooH, 0)
	for range 3 * bambooFPS {
		v.tick(bambooW, bambooH, 1.0/bambooFPS)
	}
	blits := v.frame()
	fw, fh := v.frameSize()
	if len(blits) < 100 {
		t.Fatalf("frame has %d blits, want a forest", len(blits))
	}
	if b := blits[0]; b.sprite != v.sheet.back || b.x != 0 || b.y != 0 || b.w != fw || b.h != fh {
		t.Errorf("first blit = %+v, want the back over the whole %dx%d frame", b, fw, fh)
	}
	visible := 0
	for _, b := range blits {
		if b.sprite < 0 || b.sprite >= len(v.sheet.images) || b.alpha <= 0 || b.alpha > 255 {
			t.Fatalf("blit %+v", b)
		}
		im := v.sheet.images[b.sprite]
		if im.w > 1 && (b.w != im.w || b.h != im.h) {
			t.Fatalf("blit %+v stretches a %dx%d sprite", b, im.w, im.h)
		}
		if b.x < fw && b.y < fh && b.x+b.w > 0 && b.y+b.h > 0 {
			visible++
		}
	}
	if visible < len(blits)/2 {
		t.Errorf("%d of %d blits are in the frame", visible, len(blits))
	}
}

// TestBambooView_BigDisplayHalvesTheFrame: a large display gets 2-px art
// pixels, so the frame stays about laptop-sized.
func TestBambooView_BigDisplayHalvesTheFrame(t *testing.T) {
	v := &bambooView{}
	v.tick(2560, 1440, 0)
	if fw, fh := v.frameSize(); v.unit() != 2 || fw != 1280 || fh != 720 {
		t.Errorf("2560x1440: unit %v, frame %dx%d; want 2 and 1280x720", v.unit(), fw, fh)
	}
	if im := v.sheet.images[v.sheet.back]; im.w != 1280 || im.h != 720 {
		t.Errorf("back is %dx%d, want the frame size", im.w, im.h)
	}
}
