//go:build linux

package overlay

import "math"

// The sharks style's cell renderer, ported from renderer.ts of ascii-sharks
// (see sharks.go for the licence). The world is drawn as a grid of ASCII
// cells over a faint grid on black:
//  1. a wake field that diffuses where things swim, lighting cells cyan;
//  2. fish as dashes, one per cell, glinting on hard turns;
//  3. sharks rasterised cell by cell with supersampled edges: grey blocks
//     shaded by the light, dense glyphs, and shimmering accent colours.
//
// The original's ambient dots are all switched off, so only the wake's dots
// are kept, and the seabed shadow that dimmed them goes with them. Every
// cell becomes ops that C draws in one call: a filled rectangle or a glyph
// from the atlas, both with a colour and alpha.

// The cell grid: the cell width follows the widget width, the height is
// 1.45 of it, the glyphs are 0.74 of the height.
const (
	sharksCellMin = 8
	sharksCellMax = 13
	sharksCellN   = sharksCellMax - sharksCellMin + 1 // atlas sizes

	sharksGridAlpha = 0.055 // the grid lines, white at this alpha

	sharksAlphaLevels = 6 // brightness levels of the wake's dots
)

// sharksCellSize is the cell width, height and glyph size in pixels of
// atlas size i.
func sharksCellSize(i int) (cw, ch, font float64) {
	cw = float64(sharksCellMin + i)
	ch = math.Round(cw * 1.45)
	return cw, ch, math.Round(ch * 0.74)
}

// sharksGlyphs are the atlas glyphs: every one the renderer draws.
var sharksGlyphs = []string{":", ";", "t", "%", "S", "X", "8", "#", "@", ".", "'", ",", "`", "o", "-", "*", "·"}

// sharksGlyph is the atlas index of an ASCII glyph of sharksGlyphs.
var sharksGlyph = func() (t [128]int8) {
	for i, g := range sharksGlyphs {
		if len(g) == 1 {
			t[g[0]] = int8(i)
		}
	}
	return t
}()

const sharksGlyphDot = 16 // "·", the faded heat mark

// Sparse glyphs on bright skin, dense glyphs in shadow, so the texture
// reinforces the shading instead of fighting it.
const (
	sharksSkinRamp   = ":;t%SX8#@"
	sharksFinGlyphs  = "%;tS:"
	sharksRidgeGlyph = "@8%"
	sharksEdgeHeavy  = ":;"
	sharksEdgeLight  = ".',`"
	sharksMarkGlyphs = "#%@"
)

// Cell classes of a shark raster.
const (
	sharkFlagBody = iota + 1
	sharkFlagEdge
	sharkFlagFin
	sharkFlagRidge
	sharkFlagEye
	sharkFlagMark // dark skin markings: tiger bars, zebra-shark spots
)

var (
	sharksFishAlphas = [...]float64{0.28, 0.42, 0.58, 0.74, 0.9}
	sharksFishRGB    = dvdRGB{230.0 / 255, 238.0 / 255, 1}
	sharksWakeRGB    = dvdRGB{110.0 / 255, 230.0 / 255, 1}
	sharksEyeRGB     = dvdRGB{0x9a / 255.0, 0xa0 / 255.0, 0xad / 255.0}
	sharksAccents    = [...]dvdRGB{
		{0xe0 / 255.0, 0x89 / 255.0, 0x2c / 255.0},
		{0xe0 / 255.0, 0x3a / 255.0, 0x32 / 255.0},
		{0xb4 / 255.0, 0x4a / 255.0, 0xe0 / 255.0},
		{0x33 / 255.0, 0xc8 / 255.0, 0xcc / 255.0},
		{0x3c / 255.0, 0xb5 / 255.0, 0x4a / 255.0},
		{0x31 / 255.0, 0x50 / 255.0, 0xe8 / 255.0},
		{0xd9 / 255.0, 0xc1 / 255.0, 0x4a / 255.0},
	}
)

// Sub-cell sample offsets for anti-aliased silhouettes.
var sharksSubsamples = [...]float64{0.25, 0.3, 0.75, 0.3, 0.25, 0.72, 0.75, 0.72}

// sharksHead is a hammerhead's cephalofoil: half-span, how far its tips
// sweep back, its depth and the notch in the middle of the front edge, all
// fractions of the length.
type sharksHead struct{ span, sweep, depth, notch float64 }

// sharksShape is one species' body outline and fin layout. All values are
// fractions of the length; s runs from the snout (0) to the tail (1).
type sharksShape struct {
	halfWidth    func(s float64) float64
	pectoral     [4]float64 // leading root, trailing root, tip along the body, reach
	falcate      bool       // notched (sickle) trailing edge on the pectorals
	pelvic       [4]float64 // leading root, trailing root, tip, reach
	caudal       float64    // forked tail fin reach, 0 for a ribbon tail
	dorsal       [2]float64
	secondDorsal [2]float64
	head         *sharksHead
	eye          *[2]float64 // nil when the eyes sit on the cephalofoil tips
	gills        [2]float64
	tint         [3]float64 // skin colour multipliers
}

// sharksClassicHalfWidth is the classic top-down half-width at s.
func sharksClassicHalfWidth(s float64) float64 {
	switch {
	case s < 0.13:
		return 0.09 * math.Pow(math.Sin(s/0.13*math.Pi/2), 0.95)
	case s < 0.3:
		return 0.09 + 0.016*sharksSmoothstep(0.13, 0.3, s)
	case s < 0.84:
		return 0.019 + 0.087*math.Pow(1-(s-0.3)/0.54, 1.25)
	}
	k := (s - 0.84) / 0.16
	return 0.019*(1-k) + 0.012*math.Sin(k*math.Pi)
}

var sharksShapes = [sharkKinds]sharksShape{
	sharkClassic: {
		halfWidth:    sharksClassicHalfWidth,
		pectoral:     [4]float64{0.15, 0.43, 0.55, 0.26},
		falcate:      true,
		pelvic:       [4]float64{0.57, 0.65, 0.69, 0.085},
		caudal:       0.085,
		dorsal:       [2]float64{0.33, 0.47},
		secondDorsal: [2]float64{0.71, 0.76},
		eye:          &[2]float64{0.1, 0.14},
		gills:        [2]float64{0.19, 0.29},
		tint:         [3]float64{0.92, 0.97, 1.05},
	},
	// Slim neck under a wide, swept-back cephalofoil, tall first dorsal,
	// long upper tail lobe. Bronze-olive grey.
	sharkHammerhead: {
		halfWidth: func(s float64) float64 {
			if s < 0.06 {
				return 0.045
			}
			if s < 0.3 {
				return 0.045 + 0.061*sharksSmoothstep(0.06, 0.3, s)
			}
			return sharksClassicHalfWidth(s) * 0.92
		},
		head:         &sharksHead{span: 0.17, sweep: 0.035, depth: 0.09},
		pectoral:     [4]float64{0.17, 0.4, 0.5, 0.22},
		falcate:      true,
		pelvic:       [4]float64{0.56, 0.63, 0.67, 0.075},
		caudal:       0.11,
		dorsal:       [2]float64{0.28, 0.44},
		secondDorsal: [2]float64{0.7, 0.74},
		gills:        [2]float64{0.16, 0.26},
		tint:         [3]float64{0.96, 0.98, 0.9},
	},
	// Blunt, square snout on a heavy body; barred back. Warm grey-brown.
	sharkTiger: {
		halfWidth: func(s float64) float64 {
			switch {
			case s < 0.13:
				return 0.1 * math.Pow(math.Sin(s/0.13*math.Pi/2), 0.45)
			case s < 0.3:
				return 0.1 + 0.014*sharksSmoothstep(0.13, 0.3, s)
			case s < 0.84:
				return 0.019 + 0.095*math.Pow(1-(s-0.3)/0.54, 1.2)
			}
			k := (s - 0.84) / 0.16
			return 0.019*(1-k) + 0.012*math.Sin(k*math.Pi)
		},
		pectoral:     [4]float64{0.15, 0.42, 0.53, 0.25},
		falcate:      true,
		pelvic:       [4]float64{0.57, 0.65, 0.69, 0.085},
		caudal:       0.095,
		dorsal:       [2]float64{0.33, 0.46},
		secondDorsal: [2]float64{0.71, 0.75},
		eye:          &[2]float64{0.08, 0.12},
		gills:        [2]float64{0.19, 0.28},
		tint:         [3]float64{1.02, 0.95, 0.84},
	},
	// Short rounded head, body done by half-length, then a long ribbon of a
	// tail fin. Broad rounded pectorals, spotted sandy skin.
	sharkZebra: {
		halfWidth: func(s float64) float64 {
			switch {
			case s < 0.1:
				return 0.075 * math.Pow(math.Sin(s/0.1*math.Pi/2), 0.7)
			case s < 0.28:
				return 0.075 + 0.025*sharksSmoothstep(0.1, 0.28, s)
			case s < 0.55:
				return 0.022 + 0.078*math.Pow(1-(s-0.28)/0.27, 1.1)
			}
			k := (s - 0.55) / 0.45
			return 0.022*(1-k*k) + 0.012*math.Sin(k*math.Pi)
		},
		pectoral:     [4]float64{0.12, 0.3, 0.36, 0.19},
		pelvic:       [4]float64{0.42, 0.48, 0.51, 0.065},
		dorsal:       [2]float64{0.36, 0.44},
		secondDorsal: [2]float64{0.49, 0.53},
		eye:          &[2]float64{0.06, 0.09},
		gills:        [2]float64{0.13, 0.2},
		tint:         [3]float64{1.08, 0.97, 0.74},
	},
	// Great hammerhead: a broader, nearly straight-edged cephalofoil with a
	// notch at the centre, a towering sickle first dorsal and slate skin.
	sharkGreatHammer: {
		halfWidth: func(s float64) float64 {
			if s < 0.05 {
				return 0.04
			}
			if s < 0.3 {
				return 0.04 + 0.07*sharksSmoothstep(0.05, 0.3, s)
			}
			return sharksClassicHalfWidth(s) * 0.95
		},
		head:         &sharksHead{span: 0.21, sweep: 0.006, depth: 0.085, notch: 0.014},
		pectoral:     [4]float64{0.16, 0.38, 0.49, 0.24},
		falcate:      true,
		pelvic:       [4]float64{0.55, 0.62, 0.67, 0.085},
		caudal:       0.13,
		dorsal:       [2]float64{0.24, 0.45},
		secondDorsal: [2]float64{0.68, 0.74},
		gills:        [2]float64{0.15, 0.25},
		tint:         [3]float64{0.86, 0.93, 1.02},
	},
	// Shortfin mako: a long conical snout, a torpedo body that narrows hard
	// to the tail stalk and a tall crescent tail. Metallic indigo back.
	sharkMako: {
		halfWidth: func(s float64) float64 {
			switch {
			case s < 0.17:
				return 0.088 * math.Pow(math.Sin(s/0.17*math.Pi/2), 1.5)
			case s < 0.32:
				return 0.088 + 0.012*sharksSmoothstep(0.17, 0.32, s)
			case s < 0.86:
				return 0.014 + 0.086*math.Pow(1-(s-0.32)/0.54, 1.45)
			}
			k := (s - 0.86) / 0.14
			return 0.014*(1-k) + 0.01*math.Sin(k*math.Pi)
		},
		pectoral:     [4]float64{0.18, 0.4, 0.5, 0.21},
		falcate:      true,
		pelvic:       [4]float64{0.58, 0.64, 0.68, 0.06},
		caudal:       0.13,
		dorsal:       [2]float64{0.34, 0.45},
		secondDorsal: [2]float64{0.76, 0.79},
		eye:          &[2]float64{0.11, 0.15},
		gills:        [2]float64{0.2, 0.3},
		tint:         [3]float64{0.78, 0.9, 1.2},
	},
	// Thresher: a compact body over the front half, then the upper tail lobe
	// as a whip as long as the shark itself. Bronze-violet skin.
	sharkThresher: {
		halfWidth: func(s float64) float64 {
			switch {
			case s < 0.08:
				return 0.07 * math.Pow(math.Sin(s/0.08*math.Pi/2), 0.75)
			case s < 0.2:
				return 0.07 + 0.02*sharksSmoothstep(0.08, 0.2, s)
			case s < 0.48:
				return 0.012 + 0.078*math.Pow(1-(s-0.2)/0.28, 1.3)
			}
			// The whip: thin, with a slight sickle swell before it tapers out.
			k := (s - 0.48) / 0.52
			return 0.012*(1-k) + 0.006*math.Sin(k*math.Pi*0.9) + 0.002
		},
		pectoral:     [4]float64{0.12, 0.27, 0.36, 0.2},
		falcate:      true,
		pelvic:       [4]float64{0.37, 0.41, 0.44, 0.045},
		dorsal:       [2]float64{0.24, 0.32},
		secondDorsal: [2]float64{0.45, 0.47},
		eye:          &[2]float64{0.05, 0.1},
		gills:        [2]float64{0.11, 0.18},
		tint:         [3]float64{1.0, 0.9, 1.08},
	},
	// Nurse shark: a blunt, broad head, a heavy flattened body with both
	// dorsals set far back, rounded paddle pectorals and a long low tail.
	sharkNurse: {
		halfWidth: func(s float64) float64 {
			switch {
			case s < 0.08:
				return 0.085 * math.Pow(math.Sin(s/0.08*math.Pi/2), 0.4)
			case s < 0.3:
				return 0.085 + 0.02*sharksSmoothstep(0.08, 0.3, s)
			case s < 0.72:
				return 0.022 + 0.083*math.Pow(1-(s-0.3)/0.42, 1.1)
			}
			k := (s - 0.72) / 0.28
			return 0.022*(1-k*k) + 0.014*math.Sin(k*math.Pi)
		},
		pectoral:     [4]float64{0.16, 0.34, 0.4, 0.17},
		pelvic:       [4]float64{0.45, 0.52, 0.55, 0.075},
		dorsal:       [2]float64{0.5, 0.58},
		secondDorsal: [2]float64{0.63, 0.69},
		eye:          &[2]float64{0.05, 0.075},
		gills:        [2]float64{0.14, 0.22},
		tint:         [3]float64{1.1, 0.96, 0.76},
	},
}

func sharksInTriangle(px, py, ax, ay, bx, by, cx, cy float64) bool {
	d1 := (px-bx)*(ay-by) - (ax-bx)*(py-by)
	d2 := (px-cx)*(by-cy) - (bx-cx)*(py-cy)
	d3 := (px-ax)*(cy-ay) - (cx-ax)*(py-ay)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !neg || !pos
}

// Op layout: sharksOpStride values per op. glyph -1 fills the box
// (x, y, w, h); glyph >= 0 draws that atlas glyph in the cell whose
// top-left is (x, y), w and h unused. Colours are 0..1.
const (
	sharksOpStride = 9
	sharksOpRect   = -1
)

// sharksField is the renderer's state for one widget: the cell grid, the
// wake and heat fields, the shark raster and the ops of the last frame.
type sharksField struct {
	cw, ch     float64
	size       int // atlas size index
	cols, rows int

	energy, energyTmp, heat []float64

	sharkFlag  []uint8
	sharkShade []float64
	sharkKey   []uint32
	sharkDepth []float64
	sharkKind  []uint8

	// Cells holding a fish this frame (frame stamp); dots skip them.
	fishStamp   []uint32
	frame       uint32
	fishBuckets [len(sharksFishAlphas)][]int

	spine      [sharksNodes * 2]float64
	arc        [sharksNodes]float64
	arcTotal   float64
	shape      *sharksShape
	kind       int
	sharkID    int
	finTipL    float64
	finTipR    float64
	hitFlag    uint8
	hitShade   float64
	hitS, hitV float64
	hitDist    float64

	ops []float64
}

// resize lays the grid out for a widget of w x h.
func (f *sharksField) resize(w, h float64) {
	cw := int(sharksClamp(math.Round(w/124), sharksCellMin, sharksCellMax))
	f.size = cw - sharksCellMin
	f.cw, f.ch, _ = sharksCellSize(f.size)
	cols, rows := int(math.Ceil(w/f.cw)), int(math.Ceil(h/f.ch))
	if cols == f.cols && rows == f.rows {
		return
	}
	f.cols, f.rows = cols, rows
	n := cols * rows
	f.energy = make([]float64, n)
	f.energyTmp = make([]float64, n)
	f.heat = make([]float64, n)
	f.sharkFlag = make([]uint8, n)
	f.sharkShade = make([]float64, n)
	f.sharkKey = make([]uint32, n)
	f.sharkDepth = make([]float64, n)
	f.sharkKind = make([]uint8, n)
	f.fishStamp = make([]uint32, n)
}

// render advances the fields by dt seconds and rebuilds the ops for the
// world as it is now.
func (f *sharksField) render(w *sharksWorld, dt float64) {
	if f.cols == 0 {
		return
	}
	t := w.time
	f.consumeEvents(w)
	f.rasterizeSharks(w.sharks, t)
	f.depositWake(w)
	f.diffuse(dt)

	f.ops = f.ops[:0]
	f.markFish(w)
	f.drawDots()
	f.drawHeat()
	f.drawFish()
	f.drawSharks(t)
}

func (f *sharksField) rect(x, y, w, h float64, c dvdRGB, a float64) {
	f.ops = append(f.ops, sharksOpRect, x, y, w, h, c.r, c.g, c.b, a)
}

func (f *sharksField) glyph(g int, x, y float64, c dvdRGB, a float64) {
	f.ops = append(f.ops, float64(g), x, y, 0, 0, c.r, c.g, c.b, a)
}

// --- simulation → field ---

func (f *sharksField) cellIndex(x, y float64) int {
	c := int(math.Floor(x / f.cw))
	r := int(math.Floor(y / f.ch))
	if c < 0 || r < 0 || c >= f.cols || r >= f.rows {
		return -1
	}
	return r*f.cols + c
}

func (f *sharksField) stamp(field []float64, x, y float64, radius int, amount float64) {
	c0 := int(math.Floor(x / f.cw))
	r0 := int(math.Floor(y / f.ch))
	for r := r0 - radius; r <= r0+radius; r++ {
		for c := c0 - radius; c <= c0+radius; c++ {
			if c < 0 || r < 0 || c >= f.cols || r >= f.rows {
				continue
			}
			d := math.Hypot(float64(c-c0), float64(r-r0)*1.4)
			if d > float64(radius)+0.5 {
				continue
			}
			i := r*f.cols + c
			field[i] = min(1.6, field[i]+amount*(1-d/float64(radius+1)))
		}
	}
}

// consumeEvents flashes every fish eaten since the last frame.
func (f *sharksField) consumeEvents(w *sharksWorld) {
	for _, e := range w.events {
		f.stamp(f.heat, e.x, e.y, 2, 1)
		f.stamp(f.energy, e.x, e.y, 3, 0.9)
	}
	w.events = w.events[:0]
}

func (f *sharksField) depositWake(w *sharksWorld) {
	e := f.energy
	for i := range w.fish {
		fi := &w.fish[i]
		if !fi.alive {
			continue
		}
		if c := f.cellIndex(fi.x, fi.y); c >= 0 {
			e[c] = min(1.6, e[c]+0.012+fi.alarm*0.03)
		}
	}
	for s := range w.sharks {
		sh := &w.sharks[s]
		// The tail sweeps the most water.
		for k := sharksNodes - 6; k < sharksNodes; k++ {
			if c := f.cellIndex(sh.nodes[k*2], sh.nodes[k*2+1]); c >= 0 {
				e[c] = min(1.6, e[c]+0.05+sh.speed*0.0012)
			}
		}
	}
}

// diffuse blurs and decays the wake so it spreads out like ripples and
// fades, and fades the heat.
func (f *sharksField) diffuse(dt float64) {
	cols, rows, e, tmp := f.cols, f.rows, f.energy, f.energyTmp
	decay := math.Exp(-dt * 1.5)
	for r := range rows {
		for c := range cols {
			i := r*cols + c
			l, rt, u, d := e[i], e[i], e[i], e[i]
			if c > 0 {
				l = e[i-1]
			}
			if c < cols-1 {
				rt = e[i+1]
			}
			if r > 0 {
				u = e[i-cols]
			}
			if r < rows-1 {
				d = e[i+cols]
			}
			tmp[i] = (e[i]*4 + l + rt + u + d) * 0.125 * decay
		}
	}
	f.energy, f.energyTmp = tmp, e
	heatDecay := math.Exp(-dt * 2.2)
	for i := range f.heat {
		f.heat[i] *= heatDecay
	}
}

// --- sharks ---

// sampleShark classifies one point against the current shark (spine and
// arc already built). Body coordinates: s along the spine (0 snout → 1
// tail), v across it. It writes the hit fields and reports whether the
// point is on the shark.
func (f *sharksField) sampleShark(px, py, L float64) bool {
	spine, arc := &f.spine, &f.arc
	best := math.Inf(1)
	var bestS, bestNx, bestNy float64
	bestSign := 1.0
	for k := range sharksNodes - 1 {
		ax, ay := spine[k*2], spine[k*2+1]
		sx, sy := spine[(k+1)*2]-ax, spine[(k+1)*2+1]-ay
		len2 := sx*sx + sy*sy
		if len2 == 0 {
			len2 = 1
		}
		u := sharksClamp(((px-ax)*sx+(py-ay)*sy)/len2, 0, 1)
		qx, qy := ax+sx*u, ay+sy*u
		if d2 := (px-qx)*(px-qx) + (py-qy)*(py-qy); d2 < best {
			best = d2
			l := math.Sqrt(len2)
			bestS = (arc[k] + u*l) / f.arcTotal
			bestSign = 1
			if sx*(py-ay)-sy*(px-ax) < 0 {
				bestSign = -1
			}
			bestNx = -sy / l * bestSign
			bestNy = sx / l * bestSign
		}
	}

	a := math.Sqrt(best)
	s := bestS
	u := s * L
	shape := f.shape
	w := shape.halfWidth(s) * L
	f.hitDist = a
	f.hitS = s
	f.hitV = a * bestSign

	if head := shape.head; head != nil && u < L*(head.sweep+head.depth+head.notch) {
		// Cephalofoil: a wide wing across the snout, its tips swept back
		// toward the eyes, and on a great hammerhead a shallow notch in the
		// middle of the front edge.
		r := a / (L * head.span)
		front := L*head.sweep*r*r + L*head.notch*max(0, 1-r/0.14)
		// Full depth almost to the tips, so the wing reads as a solid bar.
		depth := L * head.depth * (1 - 0.2*r*r*r*r)
		if r <= 1 && u >= front && u <= front+depth {
			if r > 0.84 {
				f.hitFlag, f.hitShade = sharkFlagEye, 0
			} else {
				f.hitFlag, f.hitShade = sharkFlagBody, 0.62-0.2*r
				if u < front+L*0.012 {
					f.hitShade += 0.1
				}
			}
			return true
		}
	}

	if a <= w {
		across := a / w
		if w == 0 {
			across = 0 // a is 0 too: the point sits on the spine
		}
		// Rounded back lit from the top-left; flanks fall off into shadow.
		facing := (bestNx*-0.6 + bestNy*-0.8) * across
		shade := 0.58 - across*across*0.42 + facing*0.26
		flag := uint8(sharkFlagBody)
		switch {
		case s > shape.dorsal[0] && s < shape.dorsal[1] && across < 0.3:
			// First dorsal fin, catching the light.
			flag, shade = sharkFlagRidge, 1
		case s > shape.secondDorsal[0] && s < shape.secondDorsal[1] && across < 0.4:
			shade = max(shade, 0.82)
		case shape.eye != nil && s > shape.eye[0] && s < shape.eye[1] && across > 0.6:
			flag, shade = sharkFlagEye, 0
		case s > shape.gills[0] && s < shape.gills[1] && across > 0.58:
			// Five gill slits.
			if int(math.Floor((s-shape.gills[0])/(shape.gills[1]-shape.gills[0])*5))%2 == 0 {
				shade *= 0.45
			}
		case s < 0.05:
			// Pale snout tip.
			shade = max(shade, 0.7)
		case f.kind == sharkZebra && s > 0.08 && s < 0.55 && (across < 0.1 || math.Abs(across-0.6) < 0.08):
			// Zebra sharks carry pale ridges down the back.
			shade = max(shade, 0.8)
		case f.marked(s, a*bestSign, L, across):
			flag = sharkFlagMark
		}
		f.hitFlag, f.hitShade = flag, shade
		return true
	}

	tip := f.finTipL
	if bestSign > 0 {
		tip = f.finTipR
	}
	p0, p1, p2 := shape.pectoral[0], shape.pectoral[1], shape.pectoral[2]
	if sharksInTriangle(u, a, L*p0, 0, L*p1, 0, L*p2, tip) &&
		// Falcate (sickle) fins get a concave trailing edge.
		!(shape.falcate && sharksInTriangle(u, a, L*p1, 0, L*(p1+(p2-p1)*0.33), tip*0.55, L*p2, tip)) {
		f.hitFlag = sharkFlagFin
		if f.marked(s, a*bestSign, L, 0) {
			f.hitFlag = sharkFlagMark
		}
		f.hitShade = 0.38 + 0.22*(1-sharksClamp(a/tip, 0, 1))
		return true
	}
	v0, v1, v2, vr := shape.pelvic[0], shape.pelvic[1], shape.pelvic[2], shape.pelvic[3]
	if sharksInTriangle(u, a, L*v0, 0, L*v1, 0, L*v2, L*vr) ||
		// Forked caudal fin.
		shape.caudal > 0 && sharksInTriangle(u, a, L*0.86, 0, L*0.95, 0, L, L*shape.caudal) {
		f.hitFlag, f.hitShade = sharkFlagFin, 0.36
		return true
	}
	return false
}

// marked reports whether a body point (s along, v across) falls on a dark
// marking.
func (f *sharksField) marked(s, v, L, across float64) bool {
	switch f.kind {
	case sharkTiger:
		// Wavy vertical bars over the back half, fading out toward the head.
		if s < 0.2 || s > 0.9 || across > 0.85 {
			return false
		}
		wobble := 0.35 * math.Sin(v/L*25+s*5)
		bar := math.Sin(s*math.Pi*2*9 + wobble)
		return bar > 0.65-0.5*sharksSmoothstep(0.2, 0.42, s)+0.4*sharksSmoothstep(0.8, 0.9, s)
	case sharkZebra:
		// Leopard spots on a jittered grid, so they ride along with the skin.
		if s < 0.07 {
			return false
		}
		gu := s * L / (L * 0.06)
		gv := v / (L * 0.045)
		cu, cv := math.Floor(gu), math.Floor(gv)
		jx := sharksHash3(int(cu), int(cv), f.sharkID)*0.5 + 0.25
		jy := sharksHash3(int(cu), int(cv), f.sharkID+11)*0.5 + 0.25
		return (gu-cu-jx)*(gu-cu-jx)+(gv-cv-jy)*(gv-cv-jy) < 0.08
	}
	return false
}

// rasterizeSharks builds a smooth, undulating render spine from each sim
// spine, then supersamples every cell in the shark's bounding box against
// the shape. Fully covered cells become solid skin, partial cells edge
// glyphs.
func (f *sharksField) rasterizeSharks(sharks []sharksShark, t float64) {
	cw, ch, cols, rows := f.cw, f.ch, f.cols, f.rows
	clear(f.sharkFlag)
	spine, arc := &f.spine, &f.arc

	for s := range sharks {
		sh := &sharks[s]
		L := sh.length
		n := &sh.nodes
		swim := sharksClamp(sh.speed/60, 0.4, 1.4)
		f.shape = &sharksShapes[sh.kind]
		f.kind = sh.kind
		f.sharkID = sh.id

		// Travelling body wave, growing toward the tail.
		for k := range sharksNodes {
			sk := float64(k) / (sharksNodes - 1)
			a, b := max(0, k-1), min(sharksNodes-1, k+1)
			tx, ty := n[a*2]-n[b*2], n[a*2+1]-n[b*2+1]
			tl := math.Hypot(tx, ty)
			if tl == 0 {
				tl = 1
			}
			tx /= tl
			ty /= tl
			amp := L * 0.05 * (0.1 + 0.9*math.Pow(sk, 1.8)) * swim
			off := amp * math.Sin(sh.phase-sk*5.2)
			spine[k*2] = n[k*2] - ty*off
			spine[k*2+1] = n[k*2+1] + tx*off
		}
		arc[0] = 0
		for k := 1; k < sharksNodes; k++ {
			arc[k] = arc[k-1] + math.Hypot(spine[k*2]-spine[(k-1)*2], spine[k*2+1]-spine[(k-1)*2+1])
		}
		f.arcTotal = arc[sharksNodes-1]
		if f.arcTotal == 0 {
			f.arcTotal = 1
		}

		// Bank into turns: the inside fin foreshortens, the outside one spreads.
		bank := sharksClamp(sh.turn*0.45, -0.35, 0.35)
		flex := 1 + 0.06*math.Sin(sh.phase*0.5)
		reachFin := f.shape.pectoral[3]
		f.finTipR = L * reachFin * flex * (1 - bank)
		f.finTipL = L * reachFin * flex * (1 + bank)

		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		for k := range sharksNodes {
			minX, maxX = min(minX, spine[k*2]), max(maxX, spine[k*2])
			minY, maxY = min(minY, spine[k*2+1]), max(maxY, spine[k*2+1])
		}
		reach := L * 0.36
		c0 := max(0, int(math.Floor((minX-reach)/cw)))
		c1 := min(cols-1, int(math.Floor((maxX+reach)/cw)))
		r0 := max(0, int(math.Floor((minY-reach)/ch)))
		r1 := min(rows-1, int(math.Floor((maxY+reach)/ch)))
		if c0 > c1 || r0 > r1 {
			continue
		}
		skipBeyond := reach + math.Hypot(cw, ch)

		for r := r0; r <= r1; r++ {
			for c := c0; c <= c1; c++ {
				x, y := float64(c)*cw, float64(r)*ch
				centerHit := f.sampleShark(x+cw/2, y+ch/2, L)
				if !centerHit && f.hitDist > skipBeyond {
					continue
				}
				flag, shade, hs, hv := f.hitFlag, f.hitShade, f.hitS, f.hitV

				hits := 0
				if centerHit {
					hits = 1
				}
				for k := 0; k < len(sharksSubsamples); k += 2 {
					if f.sampleShark(x+sharksSubsamples[k]*cw, y+sharksSubsamples[k+1]*ch, L) {
						hits++
					}
				}
				if hits == 0 {
					continue
				}
				coverage := float64(hits) / 5

				i := r*cols + c
				// Body-anchored key so each glyph rides along with the skin.
				f.sharkKey[i] = uint32(sharksHash3(int(math.Round(hs*52)), int(math.Round(hv/(cw*0.9))), sh.id) * 4294967295)
				f.sharkDepth[i] = sh.depth
				f.sharkKind[i] = uint8(sh.kind)

				if centerHit && coverage >= 0.6 {
					f.sharkFlag[i] = flag
					flicker := (sharksHash3(c, r, int(math.Floor(t*6))) - 0.5) * 0.06
					f.sharkShade[i] = sharksClamp(shade+flicker, 0, 1)
				} else {
					f.sharkFlag[i] = sharkFlagEdge
					f.sharkShade[i] = coverage
				}
			}
		}
	}
}

func (f *sharksField) drawSharks(t float64) {
	cw, ch, cols := f.cw, f.ch, f.cols
	for i, flag := range f.sharkFlag {
		if flag == 0 {
			continue
		}
		x := float64(i%cols) * cw
		y := float64(i/cols) * ch
		key := f.sharkKey[i]
		shade := f.sharkShade[i]
		dim := 0.7 + 0.3*f.sharkDepth[i]
		tint := &sharksShapes[f.sharkKind[i]].tint
		accent := sharksAccents[key%uint32(len(sharksAccents))]

		// Colours re-roll on a staggered clock so the hide shimmers.
		tick := int(math.Floor(t*2.4 + float64(key%997)/97))
		roll := sharksHash3(int(key), tick, 7)

		if flag == sharkFlagEdge {
			// Soft, anti-aliased silhouette: no block, just a light glyph.
			set := sharksEdgeLight
			if shade >= 0.5 {
				set = sharksEdgeHeavy
			}
			c := accent
			if roll >= 0.07 {
				g := (70 + shade*80) * dim
				c = sharksTinted(g, tint, 6)
			}
			f.glyph(int(sharksGlyph[set[(key>>8)%uint32(len(set))]]), x, y, c, 1)
			continue
		}

		var level float64
		var glyph byte
		switch flag {
		case sharkFlagRidge:
			level = 226
			glyph = sharksRidgeGlyph[(key>>8)%uint32(len(sharksRidgeGlyph))]
		case sharkFlagEye:
			level = 12
			glyph = 'o'
		case sharkFlagMark:
			level = 24 + shade*22
			glyph = sharksMarkGlyphs[(key>>8)%uint32(len(sharksMarkGlyphs))]
		case sharkFlagFin:
			level = 40 + shade*120
			glyph = sharksFinGlyphs[(key>>8)%uint32(len(sharksFinGlyphs))]
		default:
			level = 46 + shade*160
			jitter := int((key>>4)%3) - 1
			glyph = sharksSkinRamp[sharksClampInt(int(math.Floor((1-shade)*8.99))+jitter, 0, len(sharksSkinRamp)-1)]
		}
		// Tinted per species; the classic tint is a cool blue-grey, like a
		// back seen through deep water.
		f.rect(x+1, y+1, cw-1, ch-1, sharksTinted(level*dim, tint, 4), 1)

		var c dvdRGB
		switch {
		case flag == sharkFlagEye:
			c = sharksEyeRGB
		case flag == sharkFlagMark:
			c = sharksTinted(70, tint, 0)
		case roll < 0.09:
			c = accent
		case roll < 0.13:
			c = dvdRGB{1, 1, 1}
		case shade > 0.55:
			c = dvdRGB{0x0e / 255.0, 0x0f / 255.0, 0x13 / 255.0}
		default:
			c = dvdRGB{0x1b / 255.0, 0x1d / 255.0, 0x24 / 255.0}
		}
		f.glyph(int(sharksGlyph[glyph]), x, y, c, 1)
	}
}

// sharksTinted is grey level (0..255) times a species tint, the blue
// lifted by blue.
func sharksTinted(level float64, tint *[3]float64, blue float64) dvdRGB {
	return dvdRGB{
		min(255, math.Round(level*tint[0])) / 255,
		min(255, math.Round(level*tint[1])) / 255,
		min(255, math.Round(level*tint[2]+blue)) / 255,
	}
}

// --- fish, dots, heat ---

// markFish snaps each fish to a cell and buckets it by brightness: far fish
// are dim, near fish brighter, and turning or panicking fish glint. Nearer
// fish claim a cell first when two land in the same one.
func (f *sharksField) markFish(w *sharksWorld) {
	f.frame++
	for b := range f.fishBuckets {
		f.fishBuckets[b] = f.fishBuckets[b][:0]
	}
	for pass := range 2 {
		for k := range w.fish {
			fi := &w.fish[k]
			near := fi.depth > 0.78
			if !fi.alive || near != (pass == 0) {
				continue
			}
			i := f.cellIndex(fi.x, fi.y)
			if i < 0 || f.fishStamp[i] == f.frame || f.sharkFlag[i] != 0 {
				continue
			}
			f.fishStamp[i] = f.frame
			glint := sharksClamp(math.Abs(fi.turn)/3.5, 0, 1)
			v := 0.3 + 0.4*(fi.depth-0.55)/0.45 + 0.3*max(glint, fi.alarm)
			b := sharksClampInt(int(math.Floor(v*float64(len(sharksFishAlphas)))), 0, len(sharksFishAlphas)-1)
			f.fishBuckets[b] = append(f.fishBuckets[b], i)
		}
	}
}

func (f *sharksField) drawFish() {
	dash := int(sharksGlyph['-'])
	for b, bucket := range f.fishBuckets {
		for _, i := range bucket {
			f.glyph(dash, float64(i%f.cols)*f.cw, float64(i/f.cols)*f.ch, sharksFishRGB, sharksFishAlphas[b])
		}
	}
}

// drawHeat draws the flash where a fish was eaten: a bright star fading to
// a dim red dot.
func (f *sharksField) drawHeat() {
	star := int(sharksGlyph['*'])
	for i, h := range f.heat {
		if h < 0.08 {
			continue
		}
		x, y := float64(i%f.cols)*f.cw, float64(i/f.cols)*f.ch
		g := sharksGlyphDot
		if h > 0.6 {
			g = star
		}
		if h > 0.5 {
			f.glyph(g, x, y, dvdRGB{1, 120.0 / 255, 60.0 / 255}, min(1, h))
		} else {
			f.glyph(g, x, y, dvdRGB{220.0 / 255, 40.0 / 255, 40.0 / 255}, min(1, h*1.6))
		}
	}
}

// drawDots lights a cyan dot low-right in every cell where the wake is
// strong, brighter the stronger it is, except under sharks and fish.
func (f *sharksField) drawDots() {
	size := max(2, math.Round(f.cw*0.19)) + 1
	for i, e := range f.energy {
		if e <= 0.45 || f.sharkFlag[i] != 0 || f.fishStamp[i] == f.frame {
			continue
		}
		level := sharksClampInt(int(math.Floor((e-0.45)*5)), 0, sharksAlphaLevels-1)
		col, row := float64(i%f.cols), float64(i/f.cols)
		f.rect(math.Round(col*f.cw+f.cw*0.66-size/2), math.Round(row*f.ch+f.ch*0.64-size/2), size, size,
			sharksWakeRGB, float64(level+1)/sharksAlphaLevels)
	}
}
