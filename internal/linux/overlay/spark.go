//go:build linux

package overlay

import (
	"math"
	"math/rand/v2"
	"strconv"
)

// The spark style: a terracotta star of twelve uneven rays, lighter at the
// hub and wrapped in a softly pulsing glow, that slowly turns while its rays
// breathe in a wave running round it. Under it a whimsical verb
// ("Pondering…") types itself in behind a caret, is swept by a shimmer and
// fades out for the next every few seconds, and a dim line counts the time locked and a made-up number
// of tokens. The whole group drifts slowly over the screen so nothing
// burns in. Linux only, no macOS twin. Cosmetic only; the shield stays
// opaque.
//
// Motion is in reference frames at animRefFPS, like dvd and matrix; the
// clock on the stats line counts real seconds.

const (
	sparkRays      = 12
	sparkTurn      = 2 * math.Pi / (40 * animRefFPS) // radians per frame: a turn every 40 s
	sparkBreath    = 3 * animRefFPS                  // frames per breath of one ray
	sparkBreathAmp = 0.10                            // a ray grows and shrinks by this fraction
	sparkWaves     = 2                               // breath waves running round the star
	sparkRayWidth  = 0.11                            // a ray's half width at the hub, of the radius
	sparkRaySteps  = 12                              // outline steps along one side of a ray
	sparkRayPoints = 2 * (sparkRaySteps + 1)         // outline points of one ray

	sparkVerbFrames    = 6 * animRefFPS // a verb stays this long
	sparkTypeFrames    = 2              // frames per rune while a verb types in
	sparkFadeFrames    = animRefFPS / 2 // fading out before the next verb
	sparkShimmerFrames = 2 * animRefFPS // one shimmer sweep across the verb
	sparkShimmerReach  = 3.0            // runes either side of the shimmer it lifts

	sparkDriftX = 240 * animRefFPS // frames per drift sweep across
	sparkDriftY = 170 * animRefFPS // and up and down, so the path takes long to repeat

	sparkTokenRate = 1.4 // mean made-up tokens per frame, about 42 a second

	sparkCoreReach  = 0.6            // the hub's light fades to terracotta by here, of the radius
	sparkGlowReach  = 1.9            // the glow fades out by here, of the radius
	sparkGlowAlpha  = 0.22           // the glow's opacity at the centre
	sparkGlowPulse  = 0.2            // and how much of it pulses
	sparkGlowFrames = 4 * animRefFPS // frames per pulse
)

// The rays' lengths, of the radius, and their tilts off an even spacing, in
// radians: a little uneven, like a drawn star.
var (
	sparkRayLen  = [sparkRays]float64{1.0, 0.80, 0.93, 0.76, 0.98, 0.84, 0.90, 0.78, 0.96, 0.82, 0.88, 0.79}
	sparkRayTilt = [sparkRays]float64{0, 0.05, -0.04, 0.03, -0.02, 0.04, -0.05, 0.02, 0.03, -0.03, 0.05, -0.02}
)

// sparkVerbs are shown one at a time under the star, each with an ellipsis.
var sparkVerbs = []string{
	"Pondering", "Noodling", "Percolating", "Mulling", "Musing", "Ruminating",
	"Cogitating", "Brewing", "Simmering", "Marinating", "Stewing", "Conjuring",
	"Crafting", "Forging", "Hatching", "Ideating", "Synthesizing", "Reticulating",
	"Moseying", "Puttering", "Schlepping", "Finagling", "Vibing", "Combobulating",
}

var (
	sparkColor = dvdRGB{0.851, 0.467, 0.341} // terracotta, #D97757
	sparkCore  = dvdRGB{0.98, 0.72, 0.60}    // the star's hub
	sparkGlint = dvdRGB{0.99, 0.86, 0.78}    // the shimmer's peak
	sparkDim   = dvdRGB{0.45, 0.45, 0.45}    // the stats line
)

// sparkView is the per-window state, in widget pixels with y down.
type sparkView struct {
	seeded           bool
	boundsW, boundsH float64
	rng              uint64 // xorshift64* state, never 0

	frames  float64 // reference frames since the first tick
	secs    float64 // real seconds since the first tick, for the clock
	verb    int     // index into sparkVerbs
	verbAge float64 // frames the verb has been shown
	tokens  float64

	phaseX, phaseY float64 // drift phases, random per display
}

// seed picks the first verb and where on its path the drift starts.
func (v *sparkView) seed(seed uint64) {
	v.rng = goldSeed(seed)
	v.verb = int(goldRand(&v.rng) % uint64(len(sparkVerbs)))
	v.phaseX = 2 * math.Pi * goldUniform(&v.rng)
	v.phaseY = 2 * math.Pi * goldUniform(&v.rng)
	v.seeded = true
}

// tick advances by dt seconds for a widget of the given size: the first
// tick with a size seeds the view, a resize only changes the bounds.
func (v *sparkView) tick(w, h, dt float64) {
	if w <= 0 || h <= 0 {
		return
	}
	v.boundsW, v.boundsH = w, h
	if !v.seeded {
		v.seed(rand.Uint64())
		return
	}
	v.secs += max(dt, 0)
	v.step(animFrames(dt))
}

// step advances by frames reference frames: the star turns and breathes
// through frames, the verb changes to a different one every
// sparkVerbFrames and the tokens count up.
func (v *sparkView) step(frames float64) {
	v.frames += frames
	v.verbAge += frames
	for v.verbAge >= sparkVerbFrames {
		v.verbAge -= sparkVerbFrames
		skip := 1 + int(goldRand(&v.rng)%uint64(len(sparkVerbs)-1))
		v.verb = (v.verb + skip) % len(sparkVerbs)
	}
	v.tokens += frames * sparkTokenRate * 2 * goldUniform(&v.rng)
}

// radius is the star's: about an eighth of the shorter side, in whole pixels.
func (v *sparkView) radius() float64 { return math.Round(min(v.boundsW, v.boundsH) * 0.12) }

// verbSize and statSize are the font sizes of the two lines.
func (v *sparkView) verbSize() float64 { return max(14, math.Round(v.radius()*0.32)) }
func (v *sparkView) statSize() float64 { return max(11, math.Round(v.radius()*0.19)) }

// rays appends to out the outline of every ray of the star centred at
// (cx, cy), sparkRayPoints x, y pairs a ray, all wound the same way so
// they fill as one shape. A ray runs from the hub to a rounded tip,
// widest at the hub.
func (v *sparkView) rays(cx, cy, r float64, out []float64) []float64 {
	for i := range sparkRays {
		ang := -math.Pi/2 + float64(i)*2*math.Pi/sparkRays + sparkRayTilt[i] + v.frames*sparkTurn
		wave := 2*math.Pi*v.frames/sparkBreath - float64(i)*2*math.Pi*sparkWaves/sparkRays
		length := r * sparkRayLen[i] * (1 + sparkBreathAmp*math.Sin(wave))
		ca, sa := math.Cos(ang), math.Sin(ang)
		half := func(t float64) float64 {
			return r * sparkRayWidth * (1 - 0.5*t) * math.Sqrt(max(0, 1-t*t*t*t))
		}
		for k := 0; k <= sparkRaySteps; k++ {
			t := float64(k) / sparkRaySteps
			d, hw := length*t, half(t)
			out = append(out, cx+ca*d-sa*hw, cy+sa*d+ca*hw)
		}
		for k := sparkRaySteps; k >= 0; k-- {
			t := float64(k) / sparkRaySteps
			d, hw := length*t, half(t)
			out = append(out, cx+ca*d+sa*hw, cy+sa*d-ca*hw)
		}
	}
	return out
}

// verbText is the verb on screen with its ellipsis.
func (v *sparkView) verbText() string { return sparkVerbs[v.verb] + "…" }

// typed is how many of a verb's n runes are on screen: a new verb types
// in from none, one rune every sparkTypeFrames.
func (v *sparkView) typed(n int) int { return min(n, int(v.verbAge/sparkTypeFrames)) }

// verbFade is how bright the verb is, 0..1: full while it types in and
// stays, fading out before the next.
func (v *sparkView) verbFade() float64 {
	return max(0, min(1, (sparkVerbFrames-v.verbAge)/sparkFadeFrames))
}

// glow is the halo's opacity at its centre, pulsing gently.
func (v *sparkView) glow() float64 {
	return sparkGlowAlpha * (1 + sparkGlowPulse*math.Sin(2*math.Pi*v.frames/sparkGlowFrames))
}

// runeColor is the colour of rune i of a verb of n runes: terracotta,
// lifted toward the glint near the shimmer, which sweeps from before the
// first rune to past the last, and dimmed by the fade.
func (v *sparkView) runeColor(i, n int) dvdRGB {
	sweep := float64(n) + 2*sparkShimmerReach
	pos := math.Mod(v.frames, sparkShimmerFrames)/sparkShimmerFrames*sweep - sparkShimmerReach
	lift := max(0, 1-math.Abs(float64(i)-pos)/sparkShimmerReach)
	fade := v.verbFade()
	mix := func(a, b float64) float64 { return (a + (b-a)*lift) * fade }
	return dvdRGB{mix(sparkColor.r, sparkGlint.r), mix(sparkColor.g, sparkGlint.g), mix(sparkColor.b, sparkGlint.b)}
}

// stats is the line under the verb: the time locked and the tokens.
func (v *sparkView) stats() string {
	return "(" + sparkElapsed(v.secs) + " · ↓ " + sparkTokens(v.tokens) + " tokens)"
}

// sparkStatsWidest is at least as wide as the stats line gets in a day.
const sparkStatsWidest = "(00h 00m 00s · ↓ 000.0k tokens)"

// sparkElapsed formats seconds as 42s, 1m 12s or 1h 3m 5s.
func sparkElapsed(secs float64) string {
	n := int(max(secs, 0))
	h, m, s := n/3600, n/60%60, n%60
	switch {
	case h > 0:
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
	case m > 0:
		return strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
	}
	return strconv.Itoa(s) + "s"
}

// sparkTokens formats a count as 842, 3.4k, 123k or 1.2M, rounding down.
func sparkTokens(tokens float64) string {
	n := int(max(tokens, 0))
	tenths := func(div int) string {
		return strconv.FormatFloat(float64(n/(div/10))/10, 'f', 1, 64)
	}
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 100_000:
		return tenths(1000) + "k"
	case n < 1_000_000:
		return strconv.Itoa(n/1000) + "k"
	}
	return tenths(1_000_000) + "M"
}

// group places the star and the two lines under it, centred on one x,
// for a text block at most wide across with lines verbH and statH tall:
// the star's centre and the tops of the lines. The group drifts on a slow
// Lissajous path that keeps all of it, the star's glow included, a margin
// inside the bounds; wide is the widest the text gets, so a verb change
// does not move the group.
func (v *sparkView) group(wide, verbH, statH float64) (x, starY, verbY, statY float64) {
	r := v.radius()
	glow := r * sparkGlowReach
	gap := r * 0.23
	w := max(2*glow, wide)
	h := glow + r + gap + verbH + statH
	margin := r * 0.1
	ax := max(0, (v.boundsW-w)/2-margin)
	ay := max(0, (v.boundsH-h)/2-margin)
	x = v.boundsW/2 + ax*math.Sin(2*math.Pi*v.frames/sparkDriftX+v.phaseX)
	top := (v.boundsH-h)/2 + ay*math.Sin(2*math.Pi*v.frames/sparkDriftY+v.phaseY)
	starY = top + glow
	verbY = starY + r + gap
	statY = verbY + verbH
	return x, starY, verbY, statY
}
