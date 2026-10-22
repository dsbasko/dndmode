//go:build linux

package overlay

import (
	"math"
	"testing"
	"unicode/utf8"
)

// The spark style has no macOS twin, so these cases pin the model on their
// own: a star that stays inside its radius, turns and breathes under a
// pulsing glow, a verb that changes, types in and fades on time under a
// shimmer, a group that never leaves the
// screen and stats that read like a spinner's.

const (
	sparkTestW = 1920.0
	sparkTestH = 1080.0
)

func sparkTestView(seed uint64) *sparkView {
	v := &sparkView{boundsW: sparkTestW, boundsH: sparkTestH}
	v.seed(seed)
	return v
}

func TestSparkRays_StayInsideTheRadius(t *testing.T) {
	v := sparkTestView(1)
	r := v.radius()
	reach := r * (1 + sparkBreathAmp)
	for range 600 {
		v.step(0.7)
		pts := v.rays(100, 200, r, nil)
		if len(pts) != 2*sparkRays*sparkRayPoints {
			t.Fatalf("%d coordinates, want %d", len(pts), 2*sparkRays*sparkRayPoints)
		}
		for i := 0; i < len(pts); i += 2 {
			if d := math.Hypot(pts[i]-100, pts[i+1]-200); d > reach+1e-9 {
				t.Fatalf("frame %.1f: point %d is %.2f from the centre, past %.2f", v.frames, i/2, d, reach)
			}
		}
	}
}

// sparkTip is where ray i ends: the middle point of its outline.
func sparkTip(pts []float64, i int) (x, y float64) {
	k := 2 * (i*sparkRayPoints + sparkRaySteps)
	return pts[k], pts[k+1]
}

func TestSparkRays_TurnAndBreathe(t *testing.T) {
	v := sparkTestView(1)
	r := v.radius()
	x0, y0 := sparkTip(v.rays(0, 0, r, nil), 0)
	v.step(sparkBreath / 4.0) // a quarter breath: ray 0 at its longest
	x1, y1 := sparkTip(v.rays(0, 0, r, nil), 0)

	turned := math.Atan2(y1, x1) - math.Atan2(y0, x0)
	if want := sparkTurn * sparkBreath / 4.0; math.Abs(turned-want) > 1e-9 {
		t.Errorf("ray 0 turned %.6f rad, want %.6f", turned, want)
	}
	if got, want := math.Hypot(x1, y1), r*sparkRayLen[0]*(1+sparkBreathAmp); math.Abs(got-want) > 1e-9 {
		t.Errorf("ray 0 at the top of its breath is %.3f long, want %.3f", got, want)
	}
	if got := math.Hypot(x0, y0); got != r*sparkRayLen[0] {
		t.Errorf("ray 0 at rest is %.3f long, want %.3f", got, r*sparkRayLen[0])
	}
}

func TestSparkVerb_ChangesEverySixSecondsToAnother(t *testing.T) {
	v := sparkTestView(7)
	prev, changes := v.verb, 0
	for range 10 * sparkVerbFrames {
		v.step(1)
		if v.verb != prev {
			changes++
			prev = v.verb
		}
		if v.verb < 0 || v.verb >= len(sparkVerbs) {
			t.Fatalf("verb index %d out of range", v.verb)
		}
	}
	if changes != 10 {
		t.Errorf("%d verb changes in %d frames, want 10", changes, 10*sparkVerbFrames)
	}
}

func TestSparkVerb_TypesIn(t *testing.T) {
	v := sparkTestView(7)
	const n = 10
	for _, c := range []struct {
		age  float64
		want int
	}{
		{0, 0}, {sparkTypeFrames - 0.01, 0}, {sparkTypeFrames, 1}, {4.5 * sparkTypeFrames, 4},
		{n * sparkTypeFrames, n}, {sparkVerbFrames - 1, n},
	} {
		v.verbAge = c.age
		if got := v.typed(n); got != c.want {
			t.Errorf("typed at age %.2f = %d, want %d", c.age, got, c.want)
		}
	}
	// The longest verb is typed in well before it starts to fade.
	longest := 0
	for _, verb := range sparkVerbs {
		longest = max(longest, utf8.RuneCountInString(verb+"…"))
	}
	if done := float64(longest * sparkTypeFrames); done > sparkVerbFrames/2 {
		t.Errorf("the longest verb takes %.0f of %d frames to type in", done, sparkVerbFrames)
	}
}

func TestSparkVerb_FadesOut(t *testing.T) {
	v := sparkTestView(7)
	cases := []struct {
		age, want float64
	}{
		{0, 1},
		{sparkVerbFrames / 2, 1},
		{sparkVerbFrames - sparkFadeFrames/2.0, 0.5},
	}
	for _, c := range cases {
		v.verbAge = c.age
		if got := v.verbFade(); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("fade at age %.1f = %.3f, want %.3f", c.age, got, c.want)
		}
	}
	v.verbAge = sparkVerbFrames / 2
	c := v.runeColor(-100, 5) // far from the shimmer: plain terracotta
	if c != sparkColor {
		t.Errorf("rune away from the shimmer is %+v, want %+v", c, sparkColor)
	}
}

func TestSparkShimmer_SweepsLeftToRight(t *testing.T) {
	v := sparkTestView(7)
	v.verbAge = sparkVerbFrames / 2
	n := utf8.RuneCountInString(v.verbText())
	// brightest is the most lifted rune, -1 while the shimmer is off the verb.
	brightest := func() int {
		best, bestG := -1, sparkColor.g
		for i := range n {
			if g := v.runeColor(i, n).g; g > bestG {
				best, bestG = i, g
			}
		}
		return best
	}
	v.frames = sparkShimmerFrames * 1000 // the sweep restarts
	last := -1
	for range 39 { // to just before the sweep starts over
		v.frames += sparkShimmerFrames / 40.0
		b := brightest()
		if b < 0 {
			continue
		}
		if b < last {
			t.Fatalf("brightest rune went back from %d to %d", last, b)
		}
		last = b
	}
	if last != n-1 {
		t.Errorf("at the end of the sweep the brightest rune is %d, want the last, %d", last, n-1)
	}
	v.frames = sparkShimmerFrames*1000 + sparkShimmerFrames/2.0
	if c := v.runeColor(n/2, n); c.r <= sparkColor.r || c.g <= sparkColor.g {
		t.Errorf("rune under the shimmer is %+v, not lifted above %+v", c, sparkColor)
	}
}

func TestSparkGlow_PulsesGently(t *testing.T) {
	v := sparkTestView(1)
	lo, hi := math.Inf(1), math.Inf(-1)
	for range 2 * sparkGlowFrames {
		v.step(1)
		g := v.glow()
		lo, hi = min(lo, g), max(hi, g)
	}
	if want := sparkGlowAlpha * (1 - sparkGlowPulse); math.Abs(lo-want) > 1e-3 {
		t.Errorf("glow low = %.4f, want %.4f", lo, want)
	}
	if want := sparkGlowAlpha * (1 + sparkGlowPulse); math.Abs(hi-want) > 1e-3 {
		t.Errorf("glow high = %.4f, want %.4f", hi, want)
	}
}

func TestSparkGroup_StaysOnScreen(t *testing.T) {
	for _, size := range [][2]float64{{1920, 1080}, {1280, 800}, {3840, 2160}, {800, 1280}, {320, 240}} {
		v := &sparkView{boundsW: size[0], boundsH: size[1]}
		v.seed(3)
		r := v.radius()
		wide, verbH, statH := 14*v.verbSize(), 1.3*v.verbSize(), 1.3*v.statSize()
		for range 4000 {
			v.step(sparkDriftY / 1000.0)
			x, starY, verbY, statY := v.group(wide, verbH, statH)
			glow := r * sparkGlowReach
			half := max(glow, wide/2)
			if fits := 2*half <= size[0]; fits && (x-half < 0 || x+half > size[0]) {
				t.Fatalf("%v: group spans x %.1f..%.1f", size, x-half, x+half)
			}
			if top, bottom := starY-glow, statY+statH; bottom-top <= size[1] && (top < 0 || bottom > size[1]) {
				t.Fatalf("%v: group spans y %.1f..%.1f", size, top, bottom)
			}
			if verbY < starY+r || statY != verbY+verbH {
				t.Fatalf("%v: lines at %.1f and %.1f overlap the star or each other", size, verbY, statY)
			}
		}
	}
}

func TestSparkGroup_HoldsStillAcrossAVerbChange(t *testing.T) {
	v := sparkTestView(5)
	v.step(sparkVerbFrames - 0.5)
	verb := v.verb
	x0, y0, _, _ := v.group(600, 50, 30)
	v.step(1)
	if v.verb == verb {
		t.Fatal("the verb did not change")
	}
	x1, y1, _, _ := v.group(600, 50, 30)
	if math.Abs(x1-x0) > 1 || math.Abs(y1-y0) > 1 {
		t.Errorf("the group jumped from (%.1f, %.1f) to (%.1f, %.1f) in one frame", x0, y0, x1, y1)
	}
}

func TestSparkTick_SeedsOnceAndCountsRealTime(t *testing.T) {
	var v sparkView
	v.tick(0, 0, 1)
	if v.seeded {
		t.Fatal("seeded without a size")
	}
	v.tick(sparkTestW, sparkTestH, 1)
	if !v.seeded || v.frames != 0 || v.secs != 0 {
		t.Fatalf("first tick: seeded %v, frames %.1f, secs %.1f; want seeded, nothing elapsed", v.seeded, v.frames, v.secs)
	}
	v.tick(sparkTestW, sparkTestH, 5) // a stall: motion capped, the clock not
	if v.frames != animMaxDt*animRefFPS || v.secs != 5 {
		t.Errorf("after a 5 s stall frames = %.1f, secs = %.1f; want %.1f and 5", v.frames, v.secs, animMaxDt*animRefFPS)
	}
	v.tick(800, 600, 0.5) // a resize keeps the clock
	if v.secs != 5.5 || v.boundsW != 800 {
		t.Errorf("after a resize secs = %.1f, width %.0f; want 5.5 and 800", v.secs, v.boundsW)
	}
}

func TestSparkTokens_CountUpAtTheMeanRate(t *testing.T) {
	v := sparkTestView(11)
	const frames = 30 * 60 * animRefFPS // half an hour
	for range frames {
		v.step(1)
	}
	want := frames * sparkTokenRate
	if math.Abs(v.tokens-want) > want*0.01 {
		t.Errorf("%.0f tokens after %d frames, want about %.0f", v.tokens, frames, want)
	}
}

func TestSparkStats_Format(t *testing.T) {
	for _, c := range []struct {
		secs float64
		want string
	}{
		{0, "0s"}, {42.9, "42s"}, {72, "1m 12s"}, {3600, "1h 0m 0s"}, {3785, "1h 3m 5s"}, {-3, "0s"},
	} {
		if got := sparkElapsed(c.secs); got != c.want {
			t.Errorf("sparkElapsed(%v) = %q, want %q", c.secs, got, c.want)
		}
	}
	for _, c := range []struct {
		tokens float64
		want   string
	}{
		{0, "0"}, {842.7, "842"}, {1000, "1.0k"}, {3456, "3.4k"}, {99_999, "99.9k"},
		{123_456, "123k"}, {1_250_000, "1.2M"},
	} {
		if got := sparkTokens(c.tokens); got != c.want {
			t.Errorf("sparkTokens(%v) = %q, want %q", c.tokens, got, c.want)
		}
	}
	v := sparkView{secs: 72, tokens: 3456}
	if got, want := v.stats(), "(1m 12s · ↓ 3.4k tokens)"; got != want {
		t.Errorf("stats = %q, want %q", got, want)
	}
	if n, w := utf8.RuneCountInString(v.stats()), utf8.RuneCountInString(sparkStatsWidest); n > w {
		t.Errorf("stats %q is longer than sparkStatsWidest", v.stats())
	}
}

func TestSparkVerbs_AreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, verb := range sparkVerbs {
		if verb == "" || seen[verb] {
			t.Errorf("verb %q is empty or listed twice", verb)
		}
		seen[verb] = true
	}
	if len(sparkVerbs) < 2 {
		t.Fatal("a verb change needs at least two verbs")
	}
}
