//go:build linux

package overlay

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
)

// The gold style: a candlestick chart of a synthetic gold price (XAU/USD)
// that keeps climbing, the newest candle forming at the right edge while the
// chart slides left, with pixel-art gold coins raining down behind it. A
// port of the macOS GoldView (internal/macos/cocoa/goldview_darwin.m): the
// same price and rain models (same xorshift64* generator, drift, noise,
// spawn rules), sprite, palette, layout and colours. Cosmetic only; the
// shield stays opaque.
//
// The models work in macOS reference frames (30 FPS). The price advances in
// whole frames, so its walk is exactly the macOS one; the leftover fraction
// only smooths the scroll. The rain and the axis easing scale by the
// fraction directly.

const (
	goldCap             = 640    // committed candles kept; enough for a 3000-px-wide chart at the minimum spacing
	goldFramesPerCandle = 240    // 8 s per candle at 30 FPS
	goldHistory         = 600    // candles generated up front: the chart is full on the first frame
	goldDriftPerCandle  = 2.0e-4 // log-return drift per candle (~+10%/h)
	goldSigmaPerCandle  = 8.0e-4 // log-return spread per candle
	goldNoiseAR         = 0.94   // per-frame noise persistence (smooths the path)

	goldTargetCandles = 130.0 // candles across the chart, before the spacing clamp
	goldMinSpacing    = 5.0   // pixels per candle
	goldMaxSpacing    = 16.0
	goldBodyFrac      = 0.64 // candle body width as a fraction of the spacing
	goldRangePad      = 0.12 // axis headroom above and below the visible candles
	goldRangeEase     = 0.06 // per-frame step of the axis toward its target
	goldGridLines     = 8    // horizontal grid lines the step is chosen for
	goldMAPeriod      = 20   // moving-average length (candles)
	goldVGridEvery    = 24   // a vertical grid line every N candles
)

var (
	goldUp   = dvdRGB{0.16, 0.80, 0.52} // rising candle
	goldDown = dvdRGB{0.94, 0.33, 0.31} // falling candle
	goldMA   = dvdRGB{0.96, 0.76, 0.28} // moving average, ticker
)

// --- price model ---

// goldCandle is one candle's open, high, low and close.
type goldCandle struct {
	o, h, l, c float64
}

// goldState is the price model: committed candles in a ring plus the forming
// one. Go twin of the macOS GoldState.
type goldState struct {
	ring            [goldCap]goldCandle // the oldest is overwritten first
	count           int                 // committed candles held (<= goldCap)
	head            int                 // ring slot the next committed candle goes to
	cur             goldCandle          // the forming candle
	frame           int                 // frames elapsed in cur
	framesPerCandle int
	seq             uint64  // candles ever committed (anchors the scrolling vertical grid)
	noise           float64 // AR(1) per-frame log-return noise
	drift, scale    float64 // per-frame drift and noise innovation
	base            float64 // reference price for the header's change figure
	rng             uint64  // xorshift64* state, never 0
}

// goldRand is xorshift64*: small, seedable and the same sequence on every
// run, so tests can pin the models to a seed.
func goldRand(state *uint64) uint64 {
	x := *state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	*state = x
	return x * 0x2545F4914F6CDD1D
}

// goldUniform is uniform in the open interval (0,1), so log never sees 0.
func goldUniform(state *uint64) float64 {
	return (float64(goldRand(state)>>11) + 0.5) * (1.0 / 9007199254740992.0)
}

// goldSeed replaces the zero seed xorshift cannot leave.
func goldSeed(seed uint64) uint64 {
	if seed == 0 {
		return 0x9E3779B97F4A7C15
	}
	return seed
}

// gauss is a standard normal via Box-Muller.
func (s *goldState) gauss() float64 {
	u1 := goldUniform(&s.rng)
	u2 := goldUniform(&s.rng)
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// init resets the model to a flat candle at price.
func (s *goldState) init(seed uint64, price float64, framesPerCandle int) {
	*s = goldState{rng: goldSeed(seed), framesPerCandle: max(framesPerCandle, 1)}
	f := float64(s.framesPerCandle)
	s.drift = goldDriftPerCandle / f
	// The sum of F AR(1) terms with innovation b has a spread of about
	// sqrt(F) * b / (1 - a), so this b gives one candle goldSigmaPerCandle.
	s.scale = goldSigmaPerCandle * (1 - goldNoiseAR) / math.Sqrt(f)
	s.cur = goldCandle{price, price, price, price}
	s.base = price
}

// step advances one frame; after framesPerCandle frames the forming candle
// is committed and the next one opens at its close.
func (s *goldState) step() {
	s.noise = goldNoiseAR*s.noise + s.scale*s.gauss()
	p := s.cur.c * math.Exp(s.drift+s.noise)
	s.cur.c = p
	s.cur.h = max(s.cur.h, p)
	s.cur.l = min(s.cur.l, p)
	s.frame++
	if s.frame >= s.framesPerCandle {
		s.ring[s.head] = s.cur
		s.head = (s.head + 1) % goldCap
		s.count = min(s.count+1, goldCap)
		s.seq++
		s.cur = goldCandle{p, p, p, p}
		s.frame = 0
	}
}

// at returns the committed candle age candles back (0 = most recent); age
// must be in [0, count).
func (s *goldState) at(age int) goldCandle {
	i := (s.head - 1 - age) % goldCap
	if i < 0 {
		i += goldCap
	}
	return s.ring[i]
}

// fitRange is the price range the axis eases toward: the low/high of the
// forming candle and the visible most recent committed ones, padded by pad
// of the span on each side. A flat span is widened so the axis never
// collapses.
func (s *goldState) fitRange(visible int, pad float64) (lo, hi float64) {
	l, h := s.cur.l, s.cur.h
	for a := range min(visible, s.count) {
		c := s.at(a)
		l = min(l, c.l)
		h = max(h, c.h)
	}
	span := h - l
	if minSpan := h * 1e-4; span < minSpan {
		l -= (minSpan - span) / 2
		h += (minSpan - span) / 2
		span = minSpan
	}
	return l - span*pad, h + span*pad
}

// goldNiceStep picks the grid step: the 1/2/5 x 10^k value at or above
// range/lines, so labels are round prices.
func goldNiceStep(rangeV float64, lines int) float64 {
	if !(rangeV > 0) || lines < 1 {
		return 1
	}
	raw := rangeV / float64(lines)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	var nice float64
	switch f := raw / mag; {
	case f <= 1:
		nice = 1
	case f <= 2:
		nice = 2
	case f <= 5:
		nice = 5
	default:
		nice = 10
	}
	return nice * mag
}

// goldFormatPrice writes p with two decimals and comma-grouped thousands
// ("4,182.37").
func goldFormatPrice(p float64) string {
	raw := fmt.Sprintf("%.2f", p)
	var b strings.Builder
	if raw[0] == '-' {
		b.WriteByte('-')
		raw = raw[1:]
	}
	digits := strings.IndexByte(raw, '.')
	if digits < 0 {
		digits = len(raw)
	}
	for i := range digits {
		if i > 0 && (digits-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(raw[i])
	}
	b.WriteString(raw[digits:])
	return b.String()
}

// --- pixel-art coin sprite ---

// One 12x12 coin, copied from the macOS kGoldCoinArt (a test keeps them
// identical): an 8-frame spin about the vertical axis, five alternative
// faces so the shower is not one coin repeated, and two frames of a glint.
// Each frame is 12 rows of 12 palette characters, '.' transparent.
const (
	goldCoinPx     = 12
	goldCoinSpin   = 8  // frames 0..7: the spin cycle, frame 0 the plain face
	goldCoinFaces  = 5  // frames 8..12: alternative faces that stand in for frame 0
	goldCoinFrames = 15 // plus frames 13, 14: the glint
	goldCoinGlint  = goldCoinSpin + goldCoinFaces
)

const goldCoinPalette = "odglhs"

var goldCoinColors = []dvdRGB{
	{0.439, 0.263, 0.075}, // o outline
	{0.596, 0.349, 0.078}, // d dark
	{0.871, 0.639, 0.161}, // g mid
	{0.965, 0.808, 0.349}, // l light
	{1.000, 0.945, 0.651}, // h highlight
	{1.000, 1.000, 0.902}, // s sparkle
}

var goldCoinArt = [goldCoinFrames]string{
	// spin0
	"....oooo...." +
		"..oohhlloo.." +
		".ollgggggdo." +
		".olggddgggo." +
		"ollgdlgdggdo" +
		"olggdlgdggdo" +
		"olggdggdggdo" +
		"ogggdggdggdo" +
		".ogggddgggo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// spin1
	".....oo....." +
		"....ohlo...." +
		"...olgdlo..." +
		"...olgdlo..." +
		"..olggddgo.." +
		"..olglddgo.." +
		"..olggddgo.." +
		"..olggddgo.." +
		"...oggddo..." +
		"...oggddo..." +
		"....oddo...." +
		".....oo.....",
	// spin2
	".....oo....." +
		"....ohlo...." +
		"....oldo...." +
		"....oldo...." +
		"...olgdgo..." +
		"...olgdgo..." +
		"...oggddo..." +
		"...oggddo..." +
		"....oddo...." +
		"....oddo...." +
		"....oddo...." +
		".....oo.....",
	// spin3
	".....oo....." +
		".....ho....." +
		".....lo....." +
		".....lo....." +
		".....lo....." +
		".....go....." +
		".....go....." +
		".....go....." +
		".....do....." +
		".....do....." +
		".....do....." +
		".....oo.....",
	// spin4
	"....oooo...." +
		"..oohhlloo.." +
		".ollgggggdo." +
		".olgggggggo." +
		"ollggddgggdo" +
		"olggdlgdggdo" +
		"olggdggdggdo" +
		"oggggddgggdo" +
		".oggggggggo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// spin5
	".....oo....." +
		".....oh....." +
		".....ol....." +
		".....ol....." +
		".....ol....." +
		".....og....." +
		".....og....." +
		".....og....." +
		".....od....." +
		".....od....." +
		".....od....." +
		".....oo.....",
	// spin6
	".....oo....." +
		"....olho...." +
		"....odlo...." +
		"....odlo...." +
		"...ogdglo..." +
		"...ogdglo..." +
		"...oddggo..." +
		"...oddggo..." +
		"....oddo...." +
		"....oddo...." +
		"....oddo...." +
		".....oo.....",
	// spin7
	".....oo....." +
		"....olho...." +
		"...oldglo..." +
		"...oldglo..." +
		"..ogddgglo.." +
		"..ogddlglo.." +
		"..ogddgglo.." +
		"..ogddgglo.." +
		"...oddggo..." +
		"...oddggo..." +
		"....oddo...." +
		".....oo.....",
	// face0
	"....oooo...." +
		"..oohhlloo.." +
		".ollgdgggdo." +
		".olgddddggo." +
		"olldgdggggdo" +
		"olggdddgggdo" +
		"olgggdgdggdo" +
		"oggddddgggdo" +
		".ogggdggggo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// face1
	"....oooo...." +
		"..oohhlloo.." +
		".ollgggggdo." +
		".olggddgggo." +
		"ollggddgggdo" +
		"olgddddddgdo" +
		"olggddddggdo" +
		"ogggddddggdo" +
		".ogddggddgo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// face2
	"....oooo...." +
		"..oohhlloo.." +
		".ollgggggdo." +
		".oldgddgdgo." +
		"olldgddgdgdo" +
		"olgddddddgdo" +
		"olggddddggdo" +
		"ogggllllggdo" +
		".oggddddggo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// face3
	"....oooo...." +
		"..oohhlloo.." +
		".ollgggggdo." +
		".olgddddggo." +
		"ollgllllggdo" +
		"olgddddddgdo" +
		"olgllllllgdo" +
		"ogggddddggdo" +
		".oggllllggo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// face4
	"....oooo...." +
		"..oohhlloo.." +
		".ollgggggdo." +
		".olggddgggo." +
		"olldgddgdgdo" +
		"olggddddggdo" +
		"olggddddggdo" +
		"oggdgddgdgdo" +
		".ogggddgggo." +
		".oggggggddo." +
		"..ooddddoo.." +
		"....oooo....",
	// sparkle0
	"............" +
		"............" +
		".....h......" +
		".....s......" +
		"...hsssh...." +
		".....s......" +
		".....h......" +
		"............" +
		"............" +
		"............" +
		"............" +
		"............",
	// sparkle1
	"............" +
		".....h......" +
		".....s......" +
		".....s......" +
		"..hsssssh..." +
		".....s......" +
		".....s......" +
		".....h......" +
		"............" +
		"............" +
		"............" +
		"............",
}

// Coins are shaded by depth: far layers are darker. On the black base that
// reads as distance, and unlike alpha it keeps far coins from showing
// through near ones.
const (
	goldShades    = 4
	goldShadeFar  = 0.38 // brightness of the farthest layer
	goldShadeNear = 0.90 // and of the nearest
)

func goldShadeLevel(shade int) float64 {
	return goldShadeFar + (goldShadeNear-goldShadeFar)*float64(shade)/(goldShades-1)
}

// goldSprites bakes every frame at every shade into premultiplied ARGB32
// pixels (cairo's native layout), goldCoinPx^2 per sprite, row 0 at the
// top. Sprite f*goldShades+shade is frame f at that shade.
func goldSprites() []uint32 {
	out := make([]uint32, 0, goldCoinFrames*goldShades*goldCoinPx*goldCoinPx)
	for f, art := range goldCoinArt {
		for sh := range goldShades {
			level := goldShadeLevel(sh)
			if f >= goldCoinGlint {
				// The glint is light, not metal: a step brighter than its coin.
				level = min(1, level+0.1)
			}
			for i := range len(art) {
				p := strings.IndexByte(goldCoinPalette, art[i])
				if p < 0 {
					out = append(out, 0)
					continue
				}
				c := goldCoinColors[p]
				r := uint32(math.Round(255 * c.r * level))
				g := uint32(math.Round(255 * c.g * level))
				b := uint32(math.Round(255 * c.b * level))
				out = append(out, 0xFF<<24|r<<16|g<<8|b)
			}
		}
	}
	return out
}

// --- falling-gold model ---

// Pixel coins fall behind the chart: a fixed population sized to the
// screen area, each with a depth in [0,1) bucketed into goldShades layers.
// Nearer layers are brighter and fall faster, and the near half is drawn at
// twice the pixel size. A coin that leaves the bottom (or drifts off a
// side) respawns above the top edge, out of sight. Coordinates are y up, as
// on macOS, so falling is y decreasing; the renderer flips them.
const (
	goldRainCap         = 900
	goldRainAreaPerDrop = 2400.0 // px^2 of screen per coin
	goldRainMinDrops    = 12
	goldRainFallFar     = 8.0 // seconds to cross the screen, farthest coin
	goldRainFallNear    = 3.5 // and nearest
	goldRainFPS         = 30.0
	goldRainGlintShare  = 0.2 // coins that glint when they face the screen
)

// goldDrop is one coin.
type goldDrop struct {
	x, y           float64 // center
	vy, vmax       float64 // fall speed and terminal speed, px per frame
	vx             float64 // sideways drift, px per frame
	depth          float64 // 0 far .. 1 near
	spin, spinRate float64 // spin phase and its step, radians
	scale          int     // pixels per art pixel
	variant        int     // 0 = plain face, 1..goldCoinFaces = an alternative face
	glint          bool    // glints when facing the screen
	spawns         uint    // respawns since init
}

func (d *goldDrop) shade() int { return min(int(d.depth*goldShades), goldShades-1) }

// size is the sprite's side in pixels.
func (d *goldDrop) size() float64 { return float64(goldCoinPx * d.scale) }

// reach is how far past an edge the center has to be before nothing of the
// coin shows.
func (d *goldDrop) reach() float64 { return 0.6 * d.size() }

// frame is the sprite frame the coin shows now: the spin phase picks one of
// the goldCoinSpin frames, and the face frame is swapped for its variant's.
func (d *goldDrop) frame() int {
	turns := d.spin / (2 * math.Pi)
	f := int(math.Floor((turns-math.Floor(turns))*goldCoinSpin)) % goldCoinSpin
	if f == 0 && d.variant > 0 {
		return goldCoinSpin + d.variant - 1
	}
	return f
}

// goldRain is the falling coins of one window.
type goldRain struct {
	d     []goldDrop
	pixel int     // pixels per art pixel for the far half (the near half doubles it)
	w, h  float64 // view size
	rng   uint64  // xorshift64* state, never 0
}

func (r *goldRain) spawn(d *goldDrop) {
	d.depth = goldUniform(&r.rng)
	d.scale = r.pixel
	if d.shade() >= goldShades/2 {
		d.scale *= 2
	}
	d.variant = int(goldUniform(&r.rng) * (goldCoinFaces + 1))
	d.glint = goldUniform(&r.rng) < goldRainGlintShare
	fall := goldRainFallFar + (goldRainFallNear-goldRainFallFar)*d.depth
	d.vmax = r.h / (fall * goldRainFPS)
	d.vy = d.vmax * 0.15
	d.vx = (2*goldUniform(&r.rng) - 1) * 0.2
	d.x = goldUniform(&r.rng) * r.w
	d.y = r.h + d.reach() + goldUniform(&r.rng)*r.h*0.3
	d.spin = goldUniform(&r.rng) * 2 * math.Pi
	dir := 1.0
	if goldUniform(&r.rng) < 0.5 {
		dir = -1
	}
	d.spinRate = dir * (0.12 + 0.18*goldUniform(&r.rng))
	d.spawns++
}

// init fills a w x h view with coins already mid-fall, spread over the
// whole height at their terminal speed, so the shower is under way on the
// first frame. unit is the layout scale (h/900): a big display gets 2-px
// art pixels.
func (r *goldRain) init(seed uint64, w, h, unit float64) {
	r.rng = goldSeed(seed)
	r.w, r.h = max(w, 1), max(h, 1)
	r.pixel = 1
	if unit >= 1.75 {
		r.pixel = 2
	}
	n := int(math.Round(r.w * r.h / (goldRainAreaPerDrop * float64(r.pixel*r.pixel))))
	n = min(max(n, goldRainMinDrops), goldRainCap)
	if cap(r.d) < n {
		r.d = make([]goldDrop, n, goldRainCap)
	}
	r.d = r.d[:n]
	for i := range r.d {
		d := &r.d[i]
		*d = goldDrop{}
		r.spawn(d)
		reach := d.reach()
		d.y = goldUniform(&r.rng)*(r.h+2*reach) - reach
		d.vy = d.vmax
		d.spawns = 0
	}
}

// step advances every coin by frames reference frames (1 = one macOS frame).
func (r *goldRain) step(frames float64) {
	for i := range r.d {
		d := &r.d[i]
		d.vy = min(d.vy+d.vmax/goldRainFPS*frames, d.vmax) // terminal speed in about a second
		d.y -= d.vy * frames
		d.x += d.vx * frames
		d.spin += d.spinRate * frames
		if reach := d.reach(); d.y < -reach || d.x < -reach || d.x > r.w+reach {
			r.spawn(d)
		}
	}
}

// --- view ---

// goldView is the per-window animation state: the price, the rain and the
// chart layout, in widget pixels with y down.
type goldView struct {
	st       goldState
	rain     goldRain
	rainSeed uint64 // reused when a resize refills the rain
	seeded   bool

	boundsW, boundsH float64
	acc              float64 // reference frames not yet stepped into the price
	lo, hi           float64 // displayed price range, easing toward fitRange

	unit                           float64 // layout scale, h/900 clamped to 0.8..2
	chartX, chartY, chartW, chartH float64 // plot area; chartY is its top
	spacing                        float64 // pixels per candle
	visible                        int     // committed candles that can be on screen at once
}

// layout sizes the chart for the widget and refills the rain. It scales
// with the height so the chart reads the same on a laptop and on a large
// display.
func (v *goldView) layout(w, h float64) {
	v.boundsW, v.boundsH = w, h
	k := min(2, max(0.8, h/900))
	v.unit = k
	padLeft := max(24, w*0.04)
	axisW := 104 * k
	header := 120 * k
	padBottom := max(24, h*0.06)
	v.chartW = max(1, w-padLeft-axisW)
	v.chartH = max(1, h-padBottom-header)
	v.chartX = padLeft
	v.chartY = h - padBottom - v.chartH
	v.spacing = min(goldMaxSpacing, max(goldMinSpacing, v.chartW/goldTargetCandles))
	v.visible = min(int(math.Ceil(v.chartW/v.spacing))+2, goldCap-goldMAPeriod)
	v.rain.init(v.rainSeed, w, h, k)
}

// seed generates the price history, so the chart is full on the first
// frame, with the change figure measured from the left edge of the first
// screen.
func (v *goldView) seed(priceSeed uint64, price float64) {
	v.st.init(priceSeed, price, goldFramesPerCandle)
	for range goldHistory * goldFramesPerCandle {
		v.st.step()
	}
	if age := min(v.visible-1, v.st.count-1); age >= 0 {
		v.st.base = v.st.at(age).o
	}
	v.seeded = true
}

// fit moves the displayed range toward the visible candles by frames
// reference frames of easing, or jumps there when snap is set. The current
// price always stays on the axis, even if the eased range lags a fast move.
func (v *goldView) fit(snap bool, frames float64) {
	tlo, thi := v.st.fitRange(v.visible, goldRangePad)
	if snap {
		v.lo, v.hi = tlo, thi
		return
	}
	e := 1 - math.Pow(1-goldRangeEase, frames)
	v.lo += (tlo - v.lo) * e
	v.hi += (thi - v.hi) * e
	v.hi = max(v.hi, v.st.cur.h)
	v.lo = min(v.lo, v.st.cur.l)
}

// tick advances by dt seconds for a widget of the given size. The first
// tick with a size seeds the chart; a resize re-lays it out and keeps the
// price.
func (v *goldView) tick(w, h, dt float64) {
	if w <= 0 || h <= 0 {
		return
	}
	if !v.seeded {
		v.rainSeed = rand.Uint64()
		v.layout(w, h)
		v.seed(rand.Uint64(), 3900+float64(rand.IntN(40000))/100)
		v.fit(true, 0)
		return
	}
	if w != v.boundsW || h != v.boundsH {
		v.layout(w, h)
		v.fit(true, 0)
		return
	}
	frames := animFrames(dt)
	v.acc += frames
	for v.acc >= 1 {
		v.st.step()
		v.acc--
	}
	v.rain.step(frames)
	v.fit(false, frames)
}

// progress is how far the forming candle has filled, 0..1; the chart slides
// left by that much of a spacing, which makes the scroll continuous.
func (v *goldView) progress() float64 {
	return (float64(v.st.frame) + v.acc) / float64(v.st.framesPerCandle)
}

// yForPrice maps a price onto the chart, higher prices nearer the top.
func (v *goldView) yForPrice(p float64) float64 {
	return v.chartY + v.chartH - (p-v.lo)/(v.hi-v.lo)*v.chartH
}

// xForAge is a candle's center: age -1 is the forming one, 0 the most
// recent committed.
func (v *goldView) xForAge(age int, progress float64) float64 {
	return v.chartX + v.chartW - v.spacing*(1.5+float64(age)+progress)
}
