//go:build linux

package overlay

import (
	"math"
	"math/rand/v2"
	"strings"
)

// The bamboo style: a walk to the right through a bamboo forest at night,
// lit only by the moon. Parallax layers of pixel-art culms slide left,
// nearer ones bigger, brighter and faster, with mist between the layers,
// leaves drifting down, a few fireflies and a panda wandering far off. A
// port of the macOS BambooView (internal/macos/cocoa/bambooview_darwin.m):
// the same forest, particle and panda models, sprites (bamboo_art.go),
// shading, layout and frame rate. Cosmetic only; the shield stays opaque.
//
// The forest is stateless: every culm, ground tile and tuft is a hash of
// (seed, layer, slot), and the scroll is the frame counter times the layer
// speed, so a layer is a pure function of the frame and never drifts after
// hours. Particles and the panda keep their own state and generator, so they
// never change the forest. Coordinates are view pixels with y up, as on
// macOS; the renderer flips them.
//
// Like on macOS the walk is stepped at 15 FPS: a slow walk through pixel art
// needs no more, and the shield should cost as little as it can. The frame
// is painted at one pixel per art pixel of the farthest layer and scaled up
// with nearest-neighbour filtering.

const bambooFPS = 15

// Sprite frames, in bambooArt order.
const (
	bambooArtStalk  = 0  // 4 culm segments, 12x24
	bambooArtTop    = 4  // 2 culm tips, 12x24
	bambooArtLeaf   = 6  // 6 leaf clusters, 24x16
	bambooArtGround = 12 // 2 ground strips, 32x12
	bambooArtGrass  = 14 // 3 tufts, 12x8
	bambooArtFall   = 17 // 4 frames of a tumbling leaf, 7x7
	bambooArtMoon   = 21 // 24x24
	bambooArtFly    = 22 // 2 firefly glows, 5x5
	bambooArtPanda  = 24 // 4 walk frames, standing, 2 sitting and chewing; 30x18
	bambooArtCount  = 31
)

const (
	bambooStalks  = 4
	bambooTops    = 2
	bambooLeaves  = 6
	bambooGrounds = 2
	bambooGrasses = 3
	bambooFalls   = 4
	bambooFlies   = 2
	bambooPandas  = 7
)

// The panda's frames, in bambooArtPanda order.
const (
	bambooPandaWalkArt  = 0
	bambooPandaStandArt = 4
	bambooPandaSitArt   = 5
)

const (
	bambooCellW   = 12 // culm segment width, art px; the culm is columns 3..8
	bambooSegH    = 24 // culm segment height, art px
	bambooLeafW   = 24
	bambooLeafH   = 16
	bambooGroundW = 32
	bambooPandaW  = 30
)

const (
	bambooLayers    = 5
	bambooChunks    = 6   // pre-composed chunk variants per layer
	bambooMaxStalks = 320 // visible culms per layer, ever (a 6K display at the densest layer)
	bambooMaxFalls  = 160
	bambooMaxFlies  = 48
	bambooMaxPieces = 160
)

// bambooLayer is one parallax layer, far to near. scale is art px per view
// pixel over the display's base pixel; speed is view pixels per second at a
// base pixel of 1; spacing is art px between culm slots; base is the foot
// line as a fraction of the view height; light and fog shade the sprites;
// chunk is how many segments a pre-composed chunk holds; density is the
// share of slots that hold a culm.
type bambooLayer struct {
	scale      int
	speed      float64
	spacing    float64
	base       float64
	light, fog float64
	chunk      int
	density    float64
}

var bambooLayerTable = [bambooLayers]bambooLayer{
	{1, 4.0, 19.0, 0.30, 0.55, 0.80, 8, 0.75},
	{1, 7.0, 27.0, 0.24, 0.70, 0.58, 8, 0.70},
	{2, 12.0, 26.0, 0.16, 0.85, 0.32, 4, 0.70},
	{3, 20.0, 27.0, 0.08, 1.00, 0.08, 2, 0.70},
	{5, 34.0, 90.0, -0.04, 0.30, 0.00, 1, 0.35},
}

const bambooGroundLayers = 4 // the foreground layer stands below the view and has no floor

const (
	bambooCappedShare = 0.40 // culms that end in a tip inside the view
	bambooLeafChance  = 0.50 // chance a node in the leafy part grows a cluster
	bambooLeafyFrom   = 0.30 // leaves grow above this share of the visible culm
	bambooTuftChance  = 0.45 // ground tiles with a tuft
	bambooStepPeriod  = 1.1  // seconds per walking step (camera bob)
)

var bambooBob = [bambooLayers]float64{0, 0, 0.5, 1.0, 2.0} // bob, view pixels

// A view bigger than this (px^2) gets 2-px art pixels: the forest keeps its
// look on a large display, and the frame stays about the size of a laptop's.
const bambooBigView = 2.4e6

const (
	bambooAreaPerFall = 26000.0  // px^2 of view per falling leaf
	bambooAreaPerFly  = 120000.0 // px^2 of view per firefly
	bambooFlyLow      = 0.06     // firefly band, fractions of the view height
	bambooFlyHigh     = 0.52
)

// The panda lives on one far layer. Its pace is in art px per second on top
// of the scroll; it is slower than the walk, so on screen it always drifts
// left. Every stay in a state lasts between the two bounds, in seconds.
const (
	bambooPandaLayer  = 2
	bambooPandaLight  = 1.0 // brighter than its layer, or the mist would swallow it
	bambooPandaFog    = 0.18
	bambooPandaSink   = 3.0  // art px its feet stand below the foot line, in the grass
	bambooPandaPace   = 4.0  // art px per second
	bambooPandaStride = 1.5  // art px walked per walk frame
	bambooPandaChew   = 0.45 // seconds per chewing frame
)

var (
	bambooPandaWalkSpan  = [2]float64{5, 14}
	bambooPandaStandSpan = [2]float64{2, 5}
	bambooPandaSitSpan   = [2]float64{8, 20}
	bambooPandaAwaySpan  = [2]float64{20, 60}
)

// --- hashing and random numbers ---

// bambooMix is the splitmix64 finalizer: decorrelates neighbouring slots.
func bambooMix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func bambooHash(seed uint64, layer int, slot int64, salt int) uint64 {
	return bambooMix(seed ^ bambooMix(uint64(layer)<<56^uint64(salt)<<40^uint64(slot)))
}

// bambooFrac maps a hash to [0,1).
func bambooFrac(h uint64) float64 {
	return float64(h>>11) * (1.0 / 9007199254740992.0)
}

// --- forest model ---

// bambooFall is a falling leaf.
type bambooFall struct {
	x, y           float64 // center
	vx, vy         float64 // drift, px per frame (vx includes the walk)
	phase, rate    float64 // side-to-side swing
	swing          float64 // its amplitude, px per frame
	spin, spinRate float64 // tumble, radians
	layer          int     // depth it falls at (sets size and shade)
	spawns         uint    // respawns since init
}

// bambooFly is a firefly.
type bambooFly struct {
	x, y        float64
	vx, vy      float64 // wander, px per frame (the walk is added on top)
	phase, rate float64 // blink
	layer       int
	spawns      uint
}

// States of the panda.
const (
	bambooPandaAway = iota
	bambooPandaWalk
	bambooPandaStand
	bambooPandaSit
)

type bambooPanda struct {
	state  int
	dir    int     // +1 faces and walks right, -1 left
	wx     float64 // center, in its layer's scrolled coordinates
	left   int     // frames left in this state
	walked float64 // art px walked since it came, for the gait
	held   int     // frames in this state so far, for the chewing
	visits uint    // times it has come back since init
}

// bambooForest is the walk of one window. Go twin of the macOS BambooForest.
type bambooForest struct {
	seed  uint64 // the forest: every culm and tile hashes it
	frame uint64 // frames walked
	w, h  float64
	pixel int // view pixels per art px on the farthest layer
	fall  []bambooFall
	fly   []bambooFly
	panda bambooPanda
	rng   uint64 // particles' and the panda's xorshift64* state, never 0
}

// bambooStalk is a culm as one frame sees it: x its center line and base
// its foot; segs counts its segments foot to tip; capped culms end in a tip.
type bambooStalk struct {
	slot    int64
	x, base float64
	segs    int
	capped  bool
	hash    uint64
}

// Kinds of bambooPiece.
const (
	bambooPieceChunk = iota
	bambooPieceSeg
	bambooPieceTop
	bambooPieceLeaf
)

// bambooPiece is one blit of a culm: x, y its bottom-left corner and h its
// height, in view pixels.
type bambooPiece struct {
	kind    int
	variant int  // which chunk, segment, tip or leaf cluster
	mirror  bool // a leaf cluster that hangs left
	x, y, h float64
}

// px is view pixels per art px on a layer.
func (f *bambooForest) px(layer int) float64 {
	return float64(f.pixel * bambooLayerTable[layer].scale)
}

func (f *bambooForest) spacing(layer int) float64 {
	return bambooLayerTable[layer].spacing * f.px(layer)
}

// offset is how far a layer has scrolled. It is the frame count times the
// speed, never a running sum, so it is exact after any number of frames.
func (f *bambooForest) offset(layer int) float64 {
	return float64(f.frame) * bambooLayerTable[layer].speed * float64(f.pixel) / bambooFPS
}

func (f *bambooForest) speed(layer int) float64 {
	return bambooLayerTable[layer].speed * float64(f.pixel) / bambooFPS
}

// bob is the camera bob of a layer, whole view pixels.
func (f *bambooForest) bob(layer int) float64 {
	t := float64(f.frame) / bambooFPS
	return math.Round(bambooBob[layer] * float64(f.pixel) * math.Sin(2*math.Pi*t/bambooStepPeriod))
}

func (f *bambooForest) base(layer int) float64 {
	return math.Round(f.h*bambooLayerTable[layer].base) + f.bob(layer)
}

// stalks appends the culms of a layer that can show in this frame to out,
// left to right. A culm reaches out by its leaves, so slots are listed a
// leaf's span past each edge.
func (f *bambooForest) stalks(layer int, out []bambooStalk) []bambooStalk {
	L := &bambooLayerTable[layer]
	p := f.px(layer)
	sp := f.spacing(layer)
	off := f.offset(layer)
	margin := (bambooLeafW + bambooCellW) * p
	base := f.base(layer)
	segH := bambooSegH * p
	needed := int(math.Ceil((f.h-base)/segH)) + 1
	first := int64(math.Floor((off-margin)/sp)) - 1
	last := int64(math.Floor((off+f.w+margin)/sp)) + 1
	start := len(out)
	for k := first; k <= last && len(out)-start < bambooMaxStalks; k++ {
		h := bambooHash(f.seed, layer, k, 0)
		if bambooFrac(h) >= L.density {
			continue
		}
		jitter := (bambooFrac(bambooMix(h^1)) - 0.5) * 0.7
		x := (float64(k)+0.5+jitter)*sp - off
		if x < -margin || x > f.w+margin {
			continue
		}
		s := bambooStalk{slot: k, x: x, base: base, hash: h, segs: needed}
		s.capped = layer < bambooLayers-1 && bambooFrac(bambooMix(h^2)) < bambooCappedShare
		if s.capped {
			s.segs = max(int(math.Round(float64(needed)*(0.35+0.5*bambooFrac(bambooMix(h^3))))), 2)
		}
		out = append(out, s)
	}
	return out
}

// culm appends one culm's pieces to out, foot to top: its chunks, single
// segments for what is left of a capped culm, the tip, then its leaves.
// Every piece takes its x from the culm's one left edge, so the culm is
// rigid and no part of it can fall behind another.
func (f *bambooForest) culm(layer int, s *bambooStalk, out []bambooPiece) []bambooPiece {
	L := &bambooLayerTable[layer]
	p := f.px(layer)
	segH := bambooSegH * p
	left := s.x - 6*p
	H := f.h
	start := len(out)
	full := func() bool { return len(out)-start >= bambooMaxPieces }

	seg := 0
	y := s.base
	for seg+L.chunk <= s.segs && y < H && !full() {
		v := int(bambooMix(s.hash^uint64(seg+16)) % bambooChunks)
		out = append(out, bambooPiece{bambooPieceChunk, v, false, left, y, segH * float64(L.chunk)})
		seg += L.chunk
		y += segH * float64(L.chunk)
	}
	for seg < s.segs && y < H && !full() {
		v := int(bambooMix(s.hash^uint64(seg+16)) % bambooStalks)
		out = append(out, bambooPiece{bambooPieceSeg, v, false, left, y, segH})
		seg++
		y += segH
	}
	if s.capped && y < H && !full() {
		v := int(bambooMix(s.hash^5) % bambooTops)
		out = append(out, bambooPiece{bambooPieceTop, v, false, left, y, segH})
	}

	// Leaves hang from the nodes of the upper part of the culm. A node ring
	// is near the bottom of its segment, level with the twig on row 4 of a
	// cluster, so the cluster hangs below the node.
	leafy := s.base + (math.Min(H, s.base+float64(s.segs)*segH)-s.base)*bambooLeafyFrom
	for k := 1; k < s.segs && !full(); k++ {
		yn := s.base + float64(k)*segH
		if yn < leafy {
			continue
		}
		if yn > H+bambooLeafH*p {
			break
		}
		h := bambooMix(s.hash ^ uint64(k*977+31))
		if bambooFrac(h) >= bambooLeafChance {
			continue
		}
		mirror := (h>>16)&1 == 1
		lx := left + 9*p
		if mirror {
			lx = left + 2*p - bambooLeafW*p
		}
		out = append(out, bambooPiece{bambooPieceLeaf, int((h >> 8) % bambooLeaves), mirror, lx, yn - 10*p, bambooLeafH * p})
	}
	return out
}

// Particles. A falling leaf drifts with the walk (the nearer, the faster)
// plus a little wind, swings side to side and tumbles; it respawns upwind,
// above the top edge or past the right one, out of sight, in proportion to
// how much of the flow enters through each.

func (f *bambooForest) fallReach(l *bambooFall) float64 { return 7 * f.px(l.layer) }

func (f *bambooForest) uniform() float64 { return goldUniform(&f.rng) }

func (f *bambooForest) fallSpawn(l *bambooFall) {
	u := f.uniform()
	switch {
	case u < 0.45:
		l.layer = 2
	case u < 0.85:
		l.layer = 3
	default:
		l.layer = 4
	}
	p := f.px(l.layer)
	l.vx = -f.speed(l.layer) - (1.5+4.5*f.uniform())*p/bambooFPS
	l.vy = -(3.75 + 5.25*f.uniform()) * p / bambooFPS
	l.phase = f.uniform() * 2 * math.Pi
	l.rate = 2 * math.Pi / (bambooFPS * (1.5 + 2*f.uniform()))
	l.swing = (3 + 6*f.uniform()) * p / bambooFPS
	l.spin = f.uniform() * 2 * math.Pi
	dir := 1.0
	if f.uniform() < 0.5 {
		dir = -1
	}
	l.spinRate = dir * 2 * math.Pi / (bambooFPS * (0.8 + 1.2*f.uniform()))
	reach := f.fallReach(l)
	top, right := f.w*math.Abs(l.vy), f.h*math.Abs(l.vx)
	if f.uniform()*(top+right) < top {
		l.x = f.uniform() * f.w
		l.y = f.h + reach + f.uniform()*f.h*0.2
	} else {
		l.x = f.w + reach + f.uniform()*f.w*0.1
		l.y = f.uniform() * f.h
	}
	l.spawns++
}

func (l *bambooFall) frame() int {
	turns := l.spin / (2 * math.Pi)
	return int(math.Floor((turns-math.Floor(turns))*bambooFalls)) % bambooFalls
}

// A firefly wanders slowly inside the low band of the view, drifting with
// the walk at its depth, and blinks; one that drifts off the left edge comes
// back past the right one.

func (f *bambooForest) flyReach(b *bambooFly) float64 { return 5 * f.px(b.layer) }

func (f *bambooForest) flyPlace(b *bambooFly) {
	b.layer = 3
	if f.uniform() < 0.6 {
		b.layer = 2
	}
	b.y = f.h * (bambooFlyLow + (bambooFlyHigh-bambooFlyLow)*f.uniform())
	b.vx, b.vy = 0, 0
	b.phase = f.uniform() * 2 * math.Pi
	b.rate = 2 * math.Pi / (bambooFPS * (2.5 + 3.5*f.uniform()))
}

func (b *bambooFly) glow() float64 {
	if s := math.Sin(b.phase); s > 0 {
		return s * s
	}
	return 0
}

// The panda keeps to its layer's floor: its center is in the layer's
// scrolled coordinates, so it slides with the ground and only its own steps
// move it along. It walks for a while, stops to look around or sits down to
// eat, then walks on, either way; once it has drifted off the left edge it
// stays away a while and comes back past the right one.

func (f *bambooForest) pandaReach() float64 { return bambooPandaW / 2.0 * f.px(bambooPandaLayer) }

// pandaX is the view x of the panda's center.
func (f *bambooForest) pandaX() float64 { return f.panda.wx - f.offset(bambooPandaLayer) }

// pandaFoot is the line its feet stand on: down in the ground strip, so the
// soil hides its soles and the grass its paws.
func (f *bambooForest) pandaFoot() float64 {
	return f.base(bambooPandaLayer) - bambooPandaSink*f.px(bambooPandaLayer)
}

func (f *bambooForest) pandaFrames(span [2]float64) int {
	return int(math.Round(bambooFPS * (span[0] + (span[1]-span[0])*f.uniform())))
}

func (f *bambooForest) pandaEnter(state int) {
	a := &f.panda
	a.state = state
	a.held = 0
	switch state {
	case bambooPandaWalk:
		a.left = f.pandaFrames(bambooPandaWalkSpan)
	case bambooPandaStand:
		a.left = f.pandaFrames(bambooPandaStandSpan)
	case bambooPandaSit:
		a.left = f.pandaFrames(bambooPandaSitSpan)
	default:
		a.left = f.pandaFrames(bambooPandaAwaySpan)
	}
}

func (f *bambooForest) pandaStep() {
	a := &f.panda
	a.held++
	if a.state == bambooPandaAway {
		if a.left--; a.left > 0 {
			return
		}
		a.dir = 1
		if f.uniform() < 0.5 {
			a.dir = -1
		}
		// An art px past the right edge, so not a sliver of it shows yet.
		a.wx = f.offset(bambooPandaLayer) + f.w + f.pandaReach() + f.px(bambooPandaLayer)
		a.walked = 0
		a.visits++
		f.pandaEnter(bambooPandaWalk)
		return
	}
	if a.state == bambooPandaWalk {
		pace := bambooPandaPace / bambooFPS
		a.wx += float64(a.dir) * pace * f.px(bambooPandaLayer)
		a.walked += pace
	}
	if f.pandaX() < -f.pandaReach() {
		f.pandaEnter(bambooPandaAway)
		return
	}
	if a.left--; a.left > 0 {
		return
	}
	switch a.state {
	case bambooPandaWalk:
		if f.uniform() < 0.55 {
			f.pandaEnter(bambooPandaStand)
		} else {
			f.pandaEnter(bambooPandaSit)
		}
	case bambooPandaSit:
		f.pandaEnter(bambooPandaStand)
	default:
		a.dir = 1
		if f.uniform() < 0.5 {
			a.dir = -1
		}
		f.pandaEnter(bambooPandaWalk)
	}
}

// art is the panda's frame in bambooArtPanda order: the gait follows the
// ground it covers, so its feet never slide.
func (a *bambooPanda) art() int {
	switch a.state {
	case bambooPandaWalk:
		return bambooPandaWalkArt + int(math.Floor(a.walked/bambooPandaStride))%4
	case bambooPandaSit:
		return bambooPandaSitArt + a.held/int(math.Round(bambooPandaChew*bambooFPS))%2
	default:
		return bambooPandaStandArt
	}
}

// init builds the walk for a w x h view, the shower and the fireflies
// already under way and the panda in sight.
func (f *bambooForest) init(seed uint64, w, h float64) {
	fall, fly := f.fall[:0], f.fly[:0]
	*f = bambooForest{seed: seed, rng: goldSeed(bambooMix(seed)), w: max(w, 1), h: max(h, 1), pixel: 1}
	if f.w*f.h > bambooBigView {
		f.pixel = 2
	}
	area := f.w * f.h / float64(f.pixel*f.pixel)

	n := min(max(int(math.Round(area/bambooAreaPerFall)), 8), bambooMaxFalls)
	f.fall = append(fall, make([]bambooFall, n)...)
	for i := range f.fall {
		l := &f.fall[i]
		f.fallSpawn(l)
		// Mid-fall over the whole view on the first frame.
		reach := f.fallReach(l)
		l.x = f.uniform()*(f.w+2*reach) - reach
		l.y = f.uniform()*(f.h+2*reach) - reach
		l.spawns = 0
	}

	n = min(max(int(math.Round(area/bambooAreaPerFly)), 4), bambooMaxFlies)
	f.fly = append(fly, make([]bambooFly, n)...)
	for i := range f.fly {
		f.flyPlace(&f.fly[i])
		f.fly[i].x = f.uniform() * f.w
	}

	f.panda.dir = 1
	if f.uniform() < 0.5 {
		f.panda.dir = -1
	}
	f.panda.wx = f.w * (0.45 + 0.35*f.uniform())
	f.pandaEnter(bambooPandaStand)
}

// step advances the walk by one frame.
func (f *bambooForest) step() {
	f.frame++
	for i := range f.fall {
		l := &f.fall[i]
		l.phase += l.rate
		l.spin += l.spinRate
		l.x += l.vx + l.swing*math.Sin(l.phase)
		l.y += l.vy
		if reach := f.fallReach(l); l.y < -reach || l.x < -reach {
			f.fallSpawn(l)
		}
	}
	lo, hi := f.h*bambooFlyLow, f.h*bambooFlyHigh
	// A random walk with drag, in px per frame: the drag leaves a third of
	// a speed after a second, the kicks keep it at a few pixels per second.
	drag := math.Pow(0.3, 1.0/bambooFPS)
	for i := range f.fly {
		b := &f.fly[i]
		p := f.px(b.layer)
		b.vx = drag*b.vx + (f.uniform()-0.5)*0.9*p/bambooFPS
		b.vy = drag*b.vy + (f.uniform()-0.5)*0.9*p/bambooFPS
		b.x += b.vx - f.speed(b.layer)
		b.y += b.vy
		if b.y < lo {
			b.y, b.vy = lo, math.Abs(b.vy)
		}
		if b.y > hi {
			b.y, b.vy = hi, -math.Abs(b.vy)
		}
		b.phase += b.rate
		if b.x < -f.flyReach(b) {
			f.flyPlace(b)
			b.x = f.w + f.flyReach(b) + f.uniform()*f.w*0.2
			b.spawns++
		}
	}
	f.pandaStep()
}

// --- sprites ---

// bambooImage is a sprite baked for drawing: premultiplied ARGB32 pixels,
// row 0 at the top, already shaded and blown up to its frame size.
type bambooImage struct {
	w, h int
	px   []uint32
}

// Depth shading: a layer's sprites are dimmed by its light and pulled toward
// the fog colour by its fog, so far culms melt into the night instead of
// turning translucent (a far culm never shows through a near one).
var bambooFogColor = dvdRGB{0.016, 0.036, 0.042}

func bambooShade(ch byte, light, fog float64) dvdRGB {
	c := dvdRGB{1, 0, 1}
	if i := strings.IndexByte(bambooPalette, ch); i >= 0 {
		c = bambooColors[i]
	}
	if strings.IndexByte(bambooEmissive, ch) >= 0 {
		light = 1
		fog *= 0.5
	}
	return dvdRGB{
		c.r*light*(1-fog) + bambooFogColor.r*fog,
		c.g*light*(1-fog) + bambooFogColor.g*fog,
		c.b*light*(1-fog) + bambooFogColor.b*fog,
	}
}

// bambooARGB packs an opaque colour.
func bambooARGB(c dvdRGB) uint32 {
	return 0xFF<<24 | uint32(math.Round(255*c.r))<<16 | uint32(math.Round(255*c.g))<<8 | uint32(math.Round(255*c.b))
}

// bambooBake bakes a w x h grid of palette characters, shaded for one layer
// and blown up to scale frame pixels per art pixel, so drawing it is a 1:1
// copy. mirror flips it left to right.
func bambooBake(grid string, w, h int, light, fog float64, mirror bool, scale int) bambooImage {
	img := bambooImage{w: w * scale, h: h * scale, px: make([]uint32, w*scale*h*scale)}
	for y := range img.h {
		for x := range img.w {
			ax, ay := x/scale, y/scale
			if mirror {
				ax = w - 1 - ax
			}
			ch := grid[ay*w+ax]
			if ch == '.' || strings.IndexByte(bambooPalette, ch) < 0 {
				continue
			}
			img.px[y*img.w+x] = bambooARGB(bambooShade(ch, light, fog))
		}
	}
	return img
}

func bambooBakeArt(art int, light, fog float64, mirror bool, scale int) bambooImage {
	a := &bambooArt[art]
	return bambooBake(a.px, a.w, a.h, light, fog, mirror, scale)
}

// bambooSheet indexes every sprite one window draws in its image list.
type bambooSheet struct {
	images []bambooImage
	chunk  [bambooLayers][bambooChunks]int
	seg    [bambooLayers][bambooStalks]int
	top    [bambooLayers][bambooTops]int
	leaf   [bambooLayers][bambooLeaves][2]int // [0 hangs right, 1 hangs left]
	ground [bambooLayers][bambooGrounds]int
	grass  [bambooLayers][bambooGrasses]int
	fall   [bambooLayers][bambooFalls]int
	fly    [bambooLayers][bambooFlies]int
	panda  [bambooPandas][2]int // [0 faces right, 1 left]; the panda's layer only
	soil   [bambooLayers]int    // 1x1: fill below the ground strip
	back   int                  // sky, stars and moon: static, painted once per layout
	mist   int                  // one band of mist
}

func (s *bambooSheet) add(img bambooImage) int {
	s.images = append(s.images, img)
	return len(s.images) - 1
}

// bambooChunkSegment picks the segment art of segment s of chunk variant v:
// mostly plain, with branch stubs and aged culm now and then.
func bambooChunkSegment(seed uint64, layer, v, s int) int {
	u := bambooFrac(bambooHash(seed, layer, int64(v*64+s), 7))
	switch {
	case u < 0.55:
		return bambooArtStalk
	case u < 0.7:
		return bambooArtStalk + 1
	case u < 0.85:
		return bambooArtStalk + 2
	default:
		return bambooArtStalk + 3
	}
}

// bakeLayers bakes every layer's sprites for a forest seed (the chunk
// variants hash it).
func (s *bambooSheet) bakeLayers(seed uint64) {
	for layer := range bambooLayers {
		L := &bambooLayerTable[layer]
		for v := range bambooChunks {
			// Row 0 is the top: the chunk's last segment comes first.
			var grid strings.Builder
			for k := L.chunk - 1; k >= 0; k-- {
				grid.WriteString(bambooArt[bambooChunkSegment(seed, layer, v, k)].px)
			}
			s.chunk[layer][v] = s.add(bambooBake(grid.String(), bambooCellW, bambooSegH*L.chunk, L.light, L.fog, false, L.scale))
		}
		bake := func(art int, mirror bool) int {
			return s.add(bambooBakeArt(art, L.light, L.fog, mirror, L.scale))
		}
		for i := range bambooStalks {
			s.seg[layer][i] = bake(bambooArtStalk+i, false)
		}
		for i := range bambooTops {
			s.top[layer][i] = bake(bambooArtTop+i, false)
		}
		for i := range bambooLeaves {
			s.leaf[layer][i][0] = bake(bambooArtLeaf+i, false)
			s.leaf[layer][i][1] = bake(bambooArtLeaf+i, true)
		}
		for i := range bambooGrounds {
			s.ground[layer][i] = bake(bambooArtGround+i, false)
		}
		for i := range bambooGrasses {
			s.grass[layer][i] = bake(bambooArtGrass+i, false)
		}
		for i := range bambooFalls {
			s.fall[layer][i] = bake(bambooArtFall+i, false)
		}
		for i := range bambooFlies {
			s.fly[layer][i] = bake(bambooArtFly+i, false)
		}
		if layer == bambooPandaLayer {
			for i := range bambooPandas {
				for m := range 2 {
					s.panda[i][m] = s.add(bambooBakeArt(bambooArtPanda+i, bambooPandaLight, bambooPandaFog, m == 1, L.scale))
				}
			}
		}
		// The soil under the strip is the strip's own bottom-left pixel.
		g := &bambooArt[bambooArtGround]
		soil := bambooShade(g.px[(g.h-1)*g.w], L.light, L.fog)
		s.soil[layer] = s.add(bambooImage{w: 1, h: 1, px: []uint32{bambooARGB(soil)}})
	}
}

// --- statics ---

const bambooStars = 45

var (
	bambooSkyLow   = dvdRGB{0.014, 0.030, 0.036} // sky at the far floor line
	bambooMist     = dvdRGB{0.070, 0.110, 0.125}
	bambooMoonGlow = dvdRGB{0.620, 0.700, 0.760}
)

const (
	bambooMoonScale = 3    // frame pixels per moon art pixel
	bambooMistBand  = 0.22 // mist band height, fraction of the view
)

// bambooCanvas is a pixel buffer for the statics, row 0 at the top, in frame
// pixels; unit is view pixels per frame pixel, for the flip from the model's
// y-up view pixels.
type bambooCanvas struct {
	bambooImage
	unit float64
}

// over composites colour c at alpha a onto pixel (x, y), row 0 at the top.
func (c *bambooCanvas) over(x, y int, col dvdRGB, a float64) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h || a <= 0 {
		return
	}
	p := c.px[y*c.w+x]
	ch := func(shift uint, v float64) uint32 {
		d := float64((p>>shift)&0xFF) / 255
		return uint32(math.Round(255 * (v*a + d*(1-a))))
	}
	c.px[y*c.w+x] = 0xFF<<24 | ch(16, col.r)<<16 | ch(8, col.g)<<8 | ch(0, col.b)
}

// rowY is the view y (up) of the center of frame row r (down).
func (c *bambooCanvas) rowY(r int) float64 { return (float64(c.h-1-r) + 0.5) * c.unit }

// paintBambooBack paints the parts of the scene that never move: the night sky
// (black overhead, a faint teal haze down by the far floor), the stars, and
// the moon with its halo.
func paintBambooBack(seed uint64, w, h, unit float64) bambooImage {
	fw, fh := int(math.Ceil(w/unit)), int(math.Ceil(h/unit))
	c := bambooCanvas{bambooImage{w: fw, h: fh, px: make([]uint32, fw*fh)}, unit}
	black := bambooARGB(dvdRGB{})
	y0, y1 := h*bambooLayerTable[0].base, h*0.85
	for r := range fh {
		t := max((c.rowY(r)-y0)/(y1-y0), 0)
		col := black
		if t <= 1 {
			col = bambooARGB(dvdRGB{bambooSkyLow.r * (1 - t), bambooSkyLow.g * (1 - t), bambooSkyLow.b * (1 - t)})
		}
		for x := range fw {
			c.px[r*fw+x] = col
		}
	}
	s := bambooMix(seed ^ 0x5741525355)
	for i := range uint64(bambooStars) {
		x := math.Floor(bambooFrac(bambooMix(s+3*i)) * w / unit)
		y := math.Floor(h * (0.45 + 0.55*bambooFrac(bambooMix(s+3*i+1))) / unit)
		a := 0.08 + 0.25*bambooFrac(bambooMix(s+3*i+2))
		c.over(int(x), fh-1-int(y), dvdRGB{0.75, 0.80, 0.85}, a)
	}
	m := &bambooArt[bambooArtMoon]
	rad := float64(m.w*bambooMoonScale) * unit / 2
	cx, cy := math.Floor(w*0.2), math.Floor(h*0.78)
	reach := rad * 4.4
	for r := range fh {
		dy := c.rowY(r) - cy
		for x := range fw {
			dx := (float64(x)+0.5)*unit - cx
			if d := math.Hypot(dx, dy); d < reach {
				c.over(x, r, bambooMoonGlow, 0.06*(1-d/reach))
			}
		}
	}
	moon := bambooBakeArt(bambooArtMoon, 1, 0, false, bambooMoonScale)
	left := int(math.Floor((cx - rad) / unit))
	top := fh - int(math.Floor((cy-rad)/unit)) - moon.h
	for y := range moon.h {
		for x := range moon.w {
			if p := moon.px[y*moon.w+x]; p != 0 && left+x >= 0 && left+x < fw && top+y >= 0 && top+y < fh {
				c.px[(top+y)*fw+left+x] = p
			}
		}
	}
	return c.bambooImage
}

// paintBambooMist paints one band of mist, which every frame lays over the
// foot of the far layers: transparent at both edges, densest low down.
func paintBambooMist(w, h, unit float64) bambooImage {
	band := h * bambooMistBand
	fw, fh := int(math.Ceil(w/unit)), int(math.Ceil(band/unit))
	img := bambooImage{w: fw, h: fh, px: make([]uint32, fw*fh)}
	c := bambooCanvas{img, unit}
	for r := range fh {
		t := c.rowY(r) / band
		var a float64
		if t < 0.28 {
			a = 0.28 * t / 0.28
		} else {
			a = 0.28 * (1 - (t-0.28)/0.72)
		}
		a = min(max(a, 0), 1)
		pm := func(v float64) uint32 { return uint32(math.Round(255 * v * a)) }
		p := uint32(math.Round(255*a))<<24 | pm(bambooMist.r)<<16 | pm(bambooMist.g)<<8 | pm(bambooMist.b)
		for x := range fw {
			img.px[r*fw+x] = p
		}
	}
	return img
}

// --- view ---

// bambooBlit is one sprite to draw: its image index, top-left corner and
// size in frame pixels (y down), and its opacity 0..255.
type bambooBlit struct {
	sprite, x, y, w, h, alpha int
}

// bambooView is the per-window animation state: the walk, stepped at 15 FPS
// from the frame clock, and the sprites baked for it.
type bambooView struct {
	f      bambooForest
	sheet  bambooSheet
	seed   uint64
	seeded bool

	boundsW, boundsH float64
	acc              float64 // walk frames not yet stepped
	changed          bool    // the frame differs from the one last drawn

	stalkBuf []bambooStalk
	pieceBuf []bambooPiece
	blits    []bambooBlit
}

// unit is view pixels per frame pixel.
func (v *bambooView) unit() float64 { return float64(v.f.pixel) }

// frameSize is the frame in frame pixels.
func (v *bambooView) frameSize() (int, int) {
	return int(math.Ceil(v.f.w / v.unit())), int(math.Ceil(v.f.h / v.unit()))
}

// layout rebuilds the walk and the statics for the widget size; the layer
// sprites only depend on the seed and are baked once.
func (v *bambooView) layout(w, h float64) {
	v.boundsW, v.boundsH = w, h
	v.f.init(v.seed, w, h)
	if !v.seeded {
		v.sheet.bakeLayers(v.seed)
		v.sheet.back = v.sheet.add(bambooImage{})
		v.sheet.mist = v.sheet.add(bambooImage{})
		v.seeded = true
	}
	v.sheet.images[v.sheet.back] = paintBambooBack(v.seed, v.f.w, v.f.h, v.unit())
	v.sheet.images[v.sheet.mist] = paintBambooMist(v.f.w, v.f.h, v.unit())
	v.acc = 0
	v.changed = true
}

// tick advances by dt seconds for a widget of the given size: the first
// tick with a size seeds the walk, a resize rebuilds it, and the walk steps
// a whole frame each fifteenth of a second.
func (v *bambooView) tick(w, h, dt float64) {
	if w <= 0 || h <= 0 {
		return
	}
	if !v.seeded {
		v.seed = rand.Uint64()
	}
	if !v.seeded || w != v.boundsW || h != v.boundsH {
		v.layout(w, h)
		return
	}
	v.acc += min(max(dt, 0), animMaxDt) * bambooFPS
	for v.acc >= 1 {
		v.f.step()
		v.acc--
		v.changed = true
	}
}

// blit queues image img with its bottom-left corner at (x, y) view pixels,
// snapped to a whole frame pixel so the copy is exactly 1:1.
func (v *bambooView) blit(img int, x, y float64, alpha int) {
	im := &v.sheet.images[img]
	_, fh := v.frameSize()
	u := v.unit()
	col := int(math.Floor(x / u))
	row := fh - int(math.Floor(y/u)) - im.h
	v.blits = append(v.blits, bambooBlit{img, col, row, im.w, im.h, alpha})
}

// frame lists the blits of the whole scene, far to near.
func (v *bambooView) frame() []bambooBlit {
	f := &v.f
	sh := &v.sheet
	fw, fh := v.frameSize()
	v.blits = v.blits[:0]
	// Opaque base: the static sky, itself painted over pure black.
	v.blits = append(v.blits, bambooBlit{sh.back, 0, 0, fw, fh, 255})

	for layer := range bambooLayers {
		v.stalkBuf = f.stalks(layer, v.stalkBuf[:0])
		for i := range v.stalkBuf {
			v.pieceBuf = f.culm(layer, &v.stalkBuf[i], v.pieceBuf[:0])
			for _, pc := range v.pieceBuf {
				var img int
				switch pc.kind {
				case bambooPieceChunk:
					img = sh.chunk[layer][pc.variant]
				case bambooPieceSeg:
					img = sh.seg[layer][pc.variant]
				case bambooPieceTop:
					img = sh.top[layer][pc.variant]
				default:
					m := 0
					if pc.mirror {
						m = 1
					}
					img = sh.leaf[layer][pc.variant][m]
				}
				v.blit(img, pc.x, pc.y, 255)
			}
		}
		// The panda walks among its layer's culms, behind the grass and the
		// mist of its floor.
		if layer == bambooPandaLayer && f.panda.state != bambooPandaAway {
			m := 0
			if f.panda.dir < 0 {
				m = 1
			}
			p := f.px(bambooPandaLayer)
			v.blit(sh.panda[f.panda.art()][m], f.pandaX()-bambooPandaW/2.0*p, f.pandaFoot(), 255)
		}
		if layer < bambooGroundLayers {
			v.ground(layer)
		}
		if layer >= 2 {
			half := 3.5 * f.px(layer)
			for i := range f.fall {
				if l := &f.fall[i]; l.layer == layer {
					v.blit(sh.fall[layer][l.frame()], l.x-half, l.y-half, 255)
				}
			}
		}
		if layer == 2 || layer == 3 {
			half := 2.5 * f.px(layer)
			for i := range f.fly {
				b := &f.fly[i]
				g := b.glow()
				if b.layer != layer || g < 0.04 {
					continue
				}
				// Even at its brightest a firefly stays dimmer than the moon.
				art := 1
				if g > 0.5 {
					art = 0
				}
				v.blit(sh.fly[layer][art], b.x-half, b.y-half, int(math.Round(255*(0.15+0.55*g))))
			}
		}
		// Mist settles over the foot of every layer but the nearest two, so
		// the floor behind it fades into the night.
		if layer < 3 {
			v.blit(sh.mist, 0, f.base(layer)-f.h*bambooMistBand*0.28, 255)
		}
	}
	return v.blits
}

// ground queues the floor of a layer: soil below, the ground strip tiled
// along the foot line, and tufts on some tiles.
func (v *bambooView) ground(layer int) {
	f := &v.f
	sh := &v.sheet
	fw, fh := v.frameSize()
	u := v.unit()
	p := f.px(layer)
	base := f.base(layer)
	off := f.offset(layer)
	tw := bambooGroundW * p
	stripY := base - 7*p

	if stripY > 0 {
		rows := int(math.Ceil((stripY + u) / u))
		v.blits = append(v.blits, bambooBlit{sh.soil[layer], 0, fh - rows, fw, rows, 255})
	}
	// Tiles are laid from one snapped origin, so neighbours always meet.
	origin := math.Floor(off/u) * u
	first := int64(math.Floor(off/tw)) - 1
	last := int64(math.Floor((off+f.w)/tw)) + 1
	for j := first; j <= last; j++ {
		h := bambooHash(f.seed, layer, j, 9)
		v.blit(sh.ground[layer][h%bambooGrounds], float64(j)*tw-origin, stripY, 255)
	}
	for j := first; j <= last; j++ {
		h := bambooHash(f.seed, layer, j, 10)
		if bambooFrac(h) < bambooTuftChance {
			x := float64(j)*tw - origin
			tx := x + math.Floor(bambooFrac(bambooMix(h))*(bambooGroundW-12))*p
			v.blit(sh.grass[layer][(h>>20)%bambooGrasses], tx, base-1*p, 255)
		}
	}
}
