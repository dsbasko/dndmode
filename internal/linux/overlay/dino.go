//go:build linux

package overlay

import (
	"math"
	"math/rand/v2"
	"strings"
)

// The dino style: an endless run after the Chrome offline T-Rex game,
// fading from night to day and back every 700 points like the runner,
// starting at night under a moon and two stars. The dino plays itself: it
// jumps cacti and low pterodactyls, ducks under middle ones and runs under
// high ones, never misses, and speeds up to the runner's cap. Linux only,
// no macOS twin. Cosmetic only; the shield stays opaque.
//
// The world is a strip of the Chrome runner's height in frame pixels (y
// down), centred on the screen; the frame is painted at one pixel per frame
// pixel and scaled up with nearest-neighbour filtering, like bamboo. The
// model steps at the runner's 60 FPS with its speed, jump and score
// constants.

// The runner's constants, per step at dinoFPS, in frame pixels.
const (
	dinoFPS        = 60
	dinoStartSpeed = 6.0
	dinoMaxSpeed   = 13.0
	dinoAccel      = 0.001
	dinoJumpV      = 10.0 // a jump's start speed, up
	dinoGravity    = 0.6
	dinoAirSteps   = 35 // steps from take-off to landing, from dinoJumpV and dinoGravity

	dinoScoreCoef = 0.025 // score points per frame pixel run
	dinoScoreMod  = 100000

	dinoFlashSteps  = 15 // a quarter second off or on
	dinoFlashBlinks = 3

	dinoDayEvery   = 700 // points between a turn to day and back to night
	dinoShadeSteps = 7   // steps per shade of the fade: 8 shades in 56 steps
)

// The strip, y down: the runner's canvas height, the ground line the dino
// and the cacti stand on, and the three bird heights (the top of the bird
// sprite). A low bird is jumped, a middle one ducked under, a high one run
// under.
const (
	dinoStripH   = 150
	dinoGroundY  = 140
	dinoX        = 40 // the dino's left edge
	dinoBirdLow  = 102
	dinoBirdMid  = 70
	dinoBirdHigh = 40
)

// Obstacle kinds and the runner's rules for them: the speed a bird needs,
// the speed above which cacti come in groups, and the base gap.
const (
	dinoKindCactus = iota
	dinoKindBig
	dinoKindBird
	dinoKinds

	dinoBirdSpeed    = 8.5
	dinoCactusGroups = 4.0 // small cacti group above this speed
	dinoBigGroups    = 7.0 // large cacti group above this speed
	dinoMaxRepeat    = 2   // the same kind at most twice in a row

	dinoGapMargin = 30.0 // frame pixels of free ground on top of a jump's length
	dinoDuckLead  = 3.0  // steps of run the dino ducks before a middle bird
	dinoSpawnAt   = 50.0 // obstacles are placed this far beyond the right edge
)

// The sky: clouds drift left at a tenth of the run, a runner's 100..400
// frame pixels apart, their tops between the sky levels.
const (
	dinoCloudSpeed  = 0.1
	dinoCloudGapMin = 100.0
	dinoCloudGapMax = 400.0
	dinoSkyTop      = 10
	dinoSkyBottom   = 60
)

// The night sky: the moon and two stars, drifting left at a fixed pace,
// slower than the clouds and whatever the run's speed. By day they take the
// sky's colour (dino_art.go).
const (
	dinoSkyDrift   = 0.25
	dinoMoonY      = 20
	dinoMoonPhases = 4
	dinoMoonFull   = dinoMoonPhases - 1 // the first night's moon
	dinoMoonW      = 10 * dinoArtScale
	dinoStars      = 2
	dinoStarW      = 3 * dinoArtScale
)

// dinoCloud is a cloud's or a star's left edge and top.
type dinoCloud struct {
	x float64
	y int
}

// dinoObstacle is a cactus group or a bird: its left edge, top and size in
// frame pixels, and the free run after it before the next one starts.
type dinoObstacle struct {
	kind int
	n    int    // cacti in the group (1 for a bird)
	art  [3]int // art frame of each cactus
	x    float64
	top  int
	w, h int
	gap  float64
}

// dinoMinGap is the least gap the dino can always land in and take off from
// again: a whole jump at a little above the speed, since the run speeds up
// while the obstacle comes in, and some ground.
func dinoMinGap(speed float64) float64 {
	return dinoAirSteps*min(speed+0.5, dinoMaxSpeed) + dinoGapMargin
}

// --- run model ---

// dinoRun is the run: the dino, the speed and the distance. The dino's
// height is lift above the ground line, y up; everything else is frame
// pixels with y down.
type dinoRun struct {
	w     float64 // frame width
	seed  uint64
	speed float64
	dist  float64 // frame pixels run
	steps int64

	lift, vy float64 // height above the ground and its speed, up
	jumping  bool
	ducking  bool

	flash      int // steps left of the score flash
	flashScore int // the hundred the flash shows

	day       bool // the theme the run fades towards
	shade     int  // 0 night .. dinoShades-1 day
	shadeWait int  // steps until the next shade of a fade

	obs    []dinoObstacle // left to right
	next   float64        // where the next obstacle's left edge goes
	made   int            // obstacles made so far
	last   int            // kind of the last obstacle
	repeat int            // how many times in a row it came
	rng    uint64         // xorshift64* state, never 0

	clouds    []dinoCloud
	cloudNext float64 // where the next cloud's left edge goes
	cloudRng  uint64  // the sky's own generator, so the obstacles do not depend on the width

	moonX     float64
	moonPhase int // 0 crescent .. dinoMoonFull, one more each night
	stars     []dinoCloud
}

func (r *dinoRun) init(seed uint64, w float64) {
	*r = dinoRun{w: w, seed: seed, speed: dinoStartSpeed, rng: goldSeed(seed)}
	r.next = w + dinoSpawnAt
	r.cloudRng = goldSeed(bambooMix(seed))
	for r.cloudNext = r.cloudGap() - dinoCloudW; r.cloudNext < w; {
		r.addCloud()
	}
	r.moonX, r.moonPhase = 0.7*w, dinoMoonFull
	for range dinoStars {
		r.stars = append(r.stars, dinoCloud{goldUniform(&r.cloudRng) * (w - dinoStarW), r.skyLevel()})
	}
}

// skyLevel is a random top in the sky band.
func (r *dinoRun) skyLevel() int {
	return dinoSkyTop + min(int(goldUniform(&r.cloudRng)*(dinoSkyBottom-dinoSkyTop+1)), dinoSkyBottom-dinoSkyTop)
}

// dinoCloudW is a cloud's width in frame pixels.
const dinoCloudW = 23 * dinoArtScale

func (r *dinoRun) cloudGap() float64 {
	return dinoCloudGapMin + goldUniform(&r.cloudRng)*(dinoCloudGapMax-dinoCloudGapMin)
}

// addCloud puts a cloud at cloudNext and moves cloudNext a gap past it.
func (r *dinoRun) addCloud() {
	r.clouds = append(r.clouds, dinoCloud{r.cloudNext, r.skyLevel()})
	r.cloudNext += dinoCloudW + r.cloudGap()
}

func (r *dinoRun) uniform() float64 { return goldUniform(&r.rng) }

// jump takes off from the ground, standing up from a duck; in the air it
// does nothing.
func (r *dinoRun) jump() {
	if r.jumping {
		return
	}
	r.jumping, r.ducking = true, false
	r.vy = dinoJumpV
}

// duck crouches or stands up; only on the ground.
func (r *dinoRun) duck(on bool) {
	if !r.jumping {
		r.ducking = on
	}
}

// pilot plays the next obstacle not yet behind the dino: takes off so the
// top of the jump comes halfway over a cactus or a low bird, ducks just
// before a middle bird and stands up once it is past, runs under a high one.
func (r *dinoRun) pilot() {
	var o *dinoObstacle
	for i := range r.obs {
		if r.obs[i].x+float64(r.obs[i].w) > dinoX {
			o = &r.obs[i]
			break
		}
	}
	switch {
	case o == nil, o.kind == dinoKindBird && o.top == dinoBirdHigh:
		r.duck(false)
	case o.kind == dinoKindBird && o.top == dinoBirdMid:
		crouch := float64(dinoArt[dinoArtDuck].w * dinoArtScale)
		r.duck(o.x-(dinoX+crouch) <= r.speed*dinoDuckLead)
	default:
		r.duck(false)
		stand := float64(dinoArt[dinoArtRun].w * dinoArtScale)
		if d := o.x - (dinoX + stand); d+(stand+float64(o.w))/2 <= r.speed*dinoAirSteps/2 {
			r.jump()
		}
	}
}

// step advances the run by one runner frame.
func (r *dinoRun) step() {
	r.pilot()
	if r.jumping {
		r.lift += r.vy
		r.vy -= dinoGravity
		if r.lift <= 0 {
			r.lift, r.vy, r.jumping = 0, 0, false
		}
	}
	before := r.score()
	r.speed = min(r.speed+dinoAccel, dinoMaxSpeed)
	r.dist += r.speed
	r.steps++
	r.move()

	if r.flash > 0 {
		r.flash--
	}
	if now := r.score(); now/100 != before/100 && now > 0 {
		r.flash = 2 * dinoFlashBlinks * dinoFlashSteps
		r.flashScore = now / 100 * 100
	}
	r.fade(before)
}

// fade moves the shade one step towards the theme every dinoShadeSteps
// steps, and turns the theme every dinoDayEvery points; the wrap past 99999
// counts as a turn, so the cycle keeps going.
func (r *dinoRun) fade(before int) {
	target := 0
	if r.day {
		target = dinoShades - 1
	}
	if r.shade != target {
		if r.shadeWait--; r.shadeWait <= 0 {
			if target > r.shade {
				r.shade++
			} else {
				r.shade--
			}
			r.shadeWait = dinoShadeSteps
		}
	}
	if r.score()/dinoDayEvery != before/dinoDayEvery {
		r.day = !r.day
		r.shadeWait = dinoShadeSteps
		if !r.day {
			r.moonPhase = (r.moonPhase + 1) % dinoMoonPhases
		}
	}
}

// move slides the obstacles left, drops the ones gone past the left edge
// and places a new one beyond the right edge once the gap after the last
// one has come in.
func (r *dinoRun) move() {
	for i := range r.obs {
		r.obs[i].x -= r.speed
	}
	n := 0
	for _, o := range r.obs {
		if o.x+float64(o.w) > 0 {
			r.obs[n] = o
			n++
		}
	}
	r.obs = r.obs[:n]
	r.next -= r.speed
	if r.next < r.w+dinoSpawnAt {
		o := r.spawn(r.next)
		r.obs = append(r.obs, o)
		r.next = o.x + float64(o.w) + o.gap
	}

	drift := r.speed * dinoCloudSpeed
	n = 0
	for _, c := range r.clouds {
		if c.x -= drift; c.x+dinoCloudW > 0 {
			r.clouds[n] = c
			n++
		}
	}
	r.clouds = r.clouds[:n]
	if r.cloudNext -= drift; r.cloudNext < r.w+dinoSpawnAt {
		r.addCloud()
	}

	if r.moonX -= dinoSkyDrift; r.moonX+dinoMoonW < 0 {
		r.moonX = r.w + dinoSpawnAt
	}
	for i := range r.stars {
		s := &r.stars[i]
		if s.x -= dinoSkyDrift; s.x+dinoStarW < 0 {
			s.x, s.y = r.w+dinoSpawnAt, r.skyLevel()
		}
	}
}

// spawn makes the next obstacle at x, after the runner: any kind the speed
// allows, but not three of a kind in a row; cacti grouped once fast enough;
// the gap the runner's (size times speed plus a base, up to half again)
// on top of the least gap the dino needs.
func (r *dinoRun) spawn(x float64) dinoObstacle {
	kinds := dinoKinds - 1
	if r.speed > dinoBirdSpeed {
		kinds = dinoKinds
	}
	kind := min(int(r.uniform()*float64(kinds)), kinds-1)
	if r.made > 0 && kind == r.last && r.repeat >= dinoMaxRepeat {
		kind = (kind + 1 + min(int(r.uniform()*float64(kinds-1)), kinds-2)) % kinds
	}
	if r.made > 0 && kind == r.last {
		r.repeat++
	} else {
		r.last, r.repeat = kind, 1
	}
	r.made++

	o := dinoObstacle{kind: kind, n: 1, x: x}
	base := 120.0
	switch kind {
	case dinoKindBird:
		base = 150
		o.art[0] = dinoArtBird
		a := &dinoArt[dinoArtBird]
		o.w, o.h = a.w*dinoArtScale, a.h*dinoArtScale
		o.top = [3]int{dinoBirdLow, dinoBirdMid, dinoBirdHigh}[min(int(r.uniform()*3), 2)]
	default:
		first, groups := dinoArtCactus, dinoCactusGroups
		if kind == dinoKindBig {
			first, groups = dinoArtBig, dinoBigGroups
		}
		if r.speed > groups {
			o.n = 1 + min(int(r.uniform()*3), 2)
		}
		for i := range o.n {
			o.art[i] = first + min(int(r.uniform()*3), 2)
		}
		a := &dinoArt[first]
		o.w, o.h = o.n*a.w*dinoArtScale, a.h*dinoArtScale
		o.top = dinoGroundY - o.h
	}
	runner := float64(o.w)*r.speed + base*0.6
	o.gap = dinoMinGap(r.speed) + r.uniform()*runner*0.5
	return o
}

// dinoFrame is the dino as drawn: its art frame (legs swap every five
// steps) and top-left corner.
func (r *dinoRun) dinoFrame() (art, x, y int) {
	legs := int(r.steps/5) % 2
	switch {
	case r.jumping:
		art = dinoArtJump
	case r.ducking:
		art = dinoArtDuck + legs
	default:
		art = dinoArtRun + legs
	}
	return art, dinoX, dinoGroundY - int(math.Round(r.lift)) - dinoArt[art].h*dinoArtScale
}

// parts calls fn with each sprite of the obstacle as drawn: the cacti side
// by side, or the bird flapping every ten steps.
func (o *dinoObstacle) parts(steps int64, fn func(art, x, y int)) {
	x := int(math.Round(o.x))
	if o.kind == dinoKindBird {
		fn(dinoArtBird+int(steps/10)%2, x, o.top)
		return
	}
	w := o.w / o.n
	for i := range o.n {
		fn(o.art[i], x+i*w, o.top)
	}
}

// score is the distance in runner points, five digits.
func (r *dinoRun) score() int {
	return int(math.Round(r.dist*dinoScoreCoef)) % dinoScoreMod
}

// shownScore is the number on screen and whether it is lit: during a flash
// the hundred reached, off first, then on, three times.
func (r *dinoRun) shownScore() (int, bool) {
	if r.flash == 0 {
		return r.score(), true
	}
	phase := (2*dinoFlashBlinks*dinoFlashSteps - r.flash) / dinoFlashSteps
	return r.flashScore, phase%2 == 1
}

// --- sprites ---

// dinoBake bakes a w x h grid of palette characters in the colours of a
// shade, blown up to scale frame pixels per art pixel, so drawing it is a
// 1:1 copy.
func dinoBake(grid string, w, h, scale, shade int) bambooImage {
	img := bambooImage{w: w * scale, h: h * scale, px: make([]uint32, w*scale*h*scale)}
	for y := range img.h {
		for x := range img.w {
			i := strings.IndexByte(dinoPalette, grid[(y/scale)*w+x/scale])
			if i < 0 {
				continue
			}
			img.px[y*img.w+x] = bambooARGB(dinoShade(i, shade))
		}
	}
	return img
}

func dinoBakeArt(art, shade int) bambooImage {
	a := &dinoArt[art]
	return dinoBake(a.px, a.w, a.h, dinoArtScale, shade)
}

// --- view ---

// The score sits at the top right of the strip: five digits, 2 frame pixels
// apart.
const (
	dinoDigitStep  = 12
	dinoScoreRight = 10
	dinoScoreTop   = 6
	dinoGroundTile = 50 * dinoArtScale
	dinoSnap       = 0.1 // a frame this close to a whole number of steps is that many

	// Each shade's sprites are one set: the art frames at their dinoArt
	// index, then the back (a 1x1 sky, stretched over the canvas).
	dinoArtBack = dinoArtCount
	dinoSheetN  = dinoArtCount + 1
)

// dinoView is the per-window animation state: the run, stepped at 60 FPS
// from the frame clock, and the sprites baked for it in every shade.
type dinoView struct {
	r      dinoRun
	images []bambooImage // shade s, sprite i at s*dinoSheetN+i
	seed   uint64
	seeded bool

	boundsW, boundsH float64
	acc              float64 // runner frames not yet stepped
	changed          bool    // the frame differs from the one last drawn

	blits []bambooBlit
}

// unit is view pixels per frame pixel: the strip takes about half the
// screen height, in whole pixels.
func (v *dinoView) unit() float64 { return max(2, math.Round(v.boundsH/300)) }

// frameSize is the strip in frame pixels.
func (v *dinoView) frameSize() (int, int) {
	return int(math.Ceil(v.boundsW / v.unit())), dinoStripH
}

// yOffset is where the strip's top goes in the widget, centring it.
func (v *dinoView) yOffset() float64 {
	return math.Round((v.boundsH - dinoStripH*v.unit()) / 2)
}

// layout lays the run out again for the widget size, with the same seed;
// the sprites do not depend on the size and are baked once, every shade of
// the fade ahead of time so it costs nothing while it runs.
func (v *dinoView) layout(w, h float64) {
	v.boundsW, v.boundsH = w, h
	fw, _ := v.frameSize()
	v.r.init(v.seed, float64(fw))
	if !v.seeded {
		v.images = make([]bambooImage, 0, dinoShades*dinoSheetN)
		for shade := range dinoShades {
			for i := range dinoArtCount {
				v.images = append(v.images, dinoBakeArt(i, shade))
			}
			v.images = append(v.images, bambooImage{w: 1, h: 1, px: []uint32{bambooARGB(dinoBackground(shade))}})
		}
		v.seeded = true
	}
	v.acc = 0
	v.changed = true
}

// tick advances by dt seconds for a widget of the given size: the first
// tick with a size seeds the run, a resize lays it out again, and the run
// steps a whole frame each sixtieth of a second. A display frame close to a
// whole number of steps counts as exactly that, so a 59.94 Hz panel or a
// wobbling clock gives one step per frame instead of an odd 0 or 2.
func (v *dinoView) tick(w, h, dt float64) {
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
	frames := min(max(dt, 0), animMaxDt) * dinoFPS
	if n := math.Round(frames); n >= 1 && math.Abs(frames-n) < dinoSnap {
		frames = n
	}
	v.acc += frames
	for v.acc >= 1 {
		v.r.step()
		v.acc--
		v.changed = true
	}
}

// background is the sky of the current shade as baked, so the widget fill
// around the strip lands on the same bytes as the strip's back.
func (v *dinoView) background() dvdRGB {
	p := v.images[v.r.shade*dinoSheetN+dinoArtBack].px[0]
	return dvdRGB{float64(p>>16&0xFF) / 255, float64(p>>8&0xFF) / 255, float64(p&0xFF) / 255}
}

// blit queues art frame art of the current shade with its top-left corner
// at (x, y) frame pixels, unless it misses the frame.
func (v *dinoView) blit(art, x, y int) {
	sprite := v.r.shade*dinoSheetN + art
	im := &v.images[sprite]
	fw, fh := v.frameSize()
	if x+im.w <= 0 || x >= fw || y+im.h <= 0 || y >= fh {
		return
	}
	v.blits = append(v.blits, bambooBlit{sprite, x, y, im.w, im.h, 255})
}

// frame lists the blits of the whole strip, back to front: the back, the
// stars and the moon, the clouds, the ground, the obstacles, the dino and
// the score.
func (v *dinoView) frame() []bambooBlit {
	r := &v.r
	fw, fh := v.frameSize()
	v.blits = append(v.blits[:0], bambooBlit{r.shade*dinoSheetN + dinoArtBack, 0, 0, fw, fh, 255})
	for _, s := range r.stars {
		v.blit(dinoArtStar, int(math.Round(s.x)), s.y)
	}
	v.blit(dinoArtMoon+r.moonPhase, int(math.Round(r.moonX)), dinoMoonY)
	for _, c := range r.clouds {
		v.blit(dinoArtCloud, int(math.Round(c.x)), c.y)
	}
	// The ground is stateless: tile i covers [i, i+1) tiles of the run, its
	// look a hash of the seed and i.
	first := int64(math.Floor(r.dist / dinoGroundTile))
	for i := first; float64(i)*dinoGroundTile-r.dist < float64(fw); i++ {
		look := int(bambooMix(r.seed^uint64(i)) % 3)
		v.blit(dinoArtGround+look, int(math.Round(float64(i)*dinoGroundTile-r.dist)), dinoGroundY-dinoArtScale)
	}
	for i := range r.obs {
		r.obs[i].parts(r.steps, v.blit)
	}
	v.blit(r.dinoFrame())
	if score, lit := r.shownScore(); lit {
		for i := range 5 {
			d := score / int(math.Pow10(4-i)) % 10
			v.blit(dinoArtDigit+d, fw-dinoScoreRight-(5-i)*dinoDigitStep, dinoScoreTop)
		}
	}
	return v.blits
}
