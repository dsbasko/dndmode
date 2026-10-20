//go:build darwin

package cocoa

import (
	"math"
	"strings"
	"testing"
)

// The tests below exercise the pure price model behind the gold overlay style
// (gold_step and its helpers, reached through cgo shims). It is the one
// unit-testable piece of the style — drawRect: output is owned by the
// WindowServer and can only be validated in the manual visual run. They pin what
// makes the chart read as a chart: well-formed candles, each opening where the
// last one closed, a steady climb, a ring that wraps without losing continuity,
// and round, readable axis labels.
//
// Candles span 240 frames, as in the view (kGoldFramesPerCandle): the noise is
// smoothed over ~16 frames, so a much shorter candle would have a smaller spread
// than the real one and the climb tests would pass too easily.
const (
	goldTestFrames = 240
	goldTestPrice  = 4000.0
)

func goldRun(seed uint64, candles int) goldSim {
	return goldSimForTest(seed, goldTestPrice, goldTestFrames, candles*goldTestFrames, 100, 0.12)
}

func TestGoldView_CandlesAreWellFormed(t *testing.T) {
	sim := goldRun(42, 500)
	if len(sim.candles) != 500 {
		t.Fatalf("committed candles = %d, want 500", len(sim.candles))
	}
	for i, c := range append(sim.candles, sim.cur) {
		if c.l > math.Min(c.o, c.c) || c.h < math.Max(c.o, c.c) {
			t.Fatalf("candle %d = %+v: low/high do not bracket open and close", i, c)
		}
		if c.l <= 0 {
			t.Fatalf("candle %d = %+v: non-positive price", i, c)
		}
		// One candle's range stays a small fraction of the price: the walk
		// glides, it does not spike.
		if (c.h-c.l)/c.o > 0.01 {
			t.Errorf("candle %d = %+v: range %.3f%% of the open, want under 1%%", i, c, 100*(c.h-c.l)/c.o)
		}
	}
}

func TestGoldView_EachCandleOpensAtPreviousClose(t *testing.T) {
	sim := goldRun(7, 300)
	for i := 1; i < len(sim.candles); i++ {
		if sim.candles[i].o != sim.candles[i-1].c {
			t.Fatalf("candle %d opens at %v, previous closed at %v", i, sim.candles[i].o, sim.candles[i-1].c)
		}
	}
	last := sim.candles[len(sim.candles)-1]
	if sim.cur.o != last.c {
		t.Errorf("forming candle opens at %v, last committed closed at %v", sim.cur.o, last.c)
	}
	if sim.candles[0].o != goldTestPrice {
		t.Errorf("first candle opens at %v, want the start price %v", sim.candles[0].o, goldTestPrice)
	}
}

func TestGoldView_SameSeedSameChart(t *testing.T) {
	a := goldRun(1234, 200)
	b := goldRun(1234, 200)
	for i := range a.candles {
		if a.candles[i] != b.candles[i] {
			t.Fatalf("candle %d differs between runs with the same seed: %+v vs %+v", i, a.candles[i], b.candles[i])
		}
	}
	c := goldRun(1235, 200)
	if c.candles[len(c.candles)-1] == a.candles[len(a.candles)-1] {
		t.Errorf("different seeds produced the same last candle %+v", a.candles[len(a.candles)-1])
	}
}

func TestGoldView_PriceClimbs(t *testing.T) {
	// 400 candles of drift is ~8% against ~1.6% of noise, so every seed must end
	// well above where it started, and most candles must close up.
	for seed := uint64(1); seed <= 20; seed++ {
		sim := goldRun(seed, 400)
		first, last := sim.candles[0], sim.candles[len(sim.candles)-1]
		if gain := last.c/first.o - 1; gain < 0.03 {
			t.Errorf("seed %d: price went from %.2f to %.2f (%+.2f%%), want a clear climb", seed, first.o, last.c, 100*gain)
		}
		up := 0
		for _, c := range sim.candles {
			if c.c >= c.o {
				up++
			}
		}
		if up*2 <= len(sim.candles) {
			t.Errorf("seed %d: %d of %d candles closed up, want a majority", seed, up, len(sim.candles))
		}
	}
}

func TestGoldView_RingWrapsKeepingContinuity(t *testing.T) {
	capN := goldCap()
	sim := goldRun(99, capN+75)
	if len(sim.candles) != capN {
		t.Fatalf("held candles = %d, want the ring capacity %d", len(sim.candles), capN)
	}
	if sim.seq != uint64(capN+75) {
		t.Errorf("seq = %d, want %d candles ever committed", sim.seq, capN+75)
	}
	for i := 1; i < len(sim.candles); i++ {
		if sim.candles[i].o != sim.candles[i-1].c {
			t.Fatalf("after wrap, candle %d opens at %v, previous closed at %v", i, sim.candles[i].o, sim.candles[i-1].c)
		}
	}
	if sim.candles[0].o == goldTestPrice {
		t.Errorf("oldest held candle still opens at the start price; the ring did not drop the oldest candles")
	}
}

func TestGoldView_FitRangeCoversVisibleCandles(t *testing.T) {
	const visible = 100
	sim := goldRun(5, 300)
	lo, hi := sim.cur.l, sim.cur.h
	for _, c := range sim.candles[len(sim.candles)-visible:] {
		lo = math.Min(lo, c.l)
		hi = math.Max(hi, c.h)
	}
	if sim.lo >= lo || sim.hi <= hi {
		t.Errorf("fitted range [%v, %v] does not leave headroom around the visible [%v, %v]", sim.lo, sim.hi, lo, hi)
	}
	pad := (hi - lo) * 0.12
	if math.Abs(sim.lo-(lo-pad)) > 1e-9 || math.Abs(sim.hi-(hi+pad)) > 1e-9 {
		t.Errorf("fitted range [%v, %v], want [%v, %v] (12%% padding)", sim.lo, sim.hi, lo-pad, hi+pad)
	}
}

func TestGoldView_FitRangeNeverCollapses(t *testing.T) {
	// No frames at all: every candle is flat at the start price, and the axis
	// must still have height.
	sim := goldSimForTest(1, goldTestPrice, goldTestFrames, 0, 100, 0.12)
	if !(sim.hi > sim.lo) {
		t.Fatalf("flat chart fitted to [%v, %v], want a non-empty range", sim.lo, sim.hi)
	}
	if sim.lo >= goldTestPrice || sim.hi <= goldTestPrice {
		t.Errorf("flat chart range [%v, %v] does not contain the price %v", sim.lo, sim.hi, goldTestPrice)
	}
}

func TestGoldView_NiceStep(t *testing.T) {
	tests := []struct {
		rangeV float64
		lines  int
		want   float64
	}{
		{rangeV: 60, lines: 6, want: 10},
		{rangeV: 61, lines: 6, want: 20},
		{rangeV: 100, lines: 6, want: 20},
		{rangeV: 250, lines: 6, want: 50},
		{rangeV: 400, lines: 6, want: 100},
		{rangeV: 3, lines: 6, want: 0.5},
		{rangeV: 0, lines: 6, want: 1},
		{rangeV: 10, lines: 0, want: 1},
	}
	for _, tt := range tests {
		if got := goldNiceStepForTest(tt.rangeV, tt.lines); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("nice step for range %v / %d lines = %v, want %v", tt.rangeV, tt.lines, got, tt.want)
		}
	}
}

func TestGoldView_FormatPrice(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{in: 4182.374, want: "4,182.37"},
		{in: 999.995, want: "1,000.00"},
		{in: 12.5, want: "12.50"},
		{in: 123456.7, want: "123,456.70"},
		{in: 1234567.891, want: "1,234,567.89"},
		{in: 0, want: "0.00"},
		{in: -4182.37, want: "-4,182.37"},
	}
	for _, tt := range tests {
		if got := goldFormatPriceForTest(tt.in); got != tt.want {
			t.Errorf("format(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The falling coins behind the chart have their own pure model
// (gold_rain_step), stepped here frame by frame on a laptop-sized view.
const (
	goldRainW    = 1512.0
	goldRainH    = 982.0
	goldRainUnit = 982.0 / 900.0
)

// goldReach mirrors gold_drop_reach: how far past an edge a coin's center has to
// be before nothing of it shows.
func goldReach(d goldDrop) float64 { return d.size * 0.6 }

func TestGoldRain_FillsScreenFromFirstFrame(t *testing.T) {
	r := newGoldRainForTest(3, goldRainW, goldRainH, goldRainUnit)
	defer r.free()
	drops := r.drops()
	if len(drops) < 300 || len(drops) > goldRainCap() {
		t.Fatalf("coins on a %vx%v view = %d, want 300..%d", goldRainW, goldRainH, len(drops), goldRainCap())
	}
	var thirds [3]int
	for _, d := range drops {
		if d.y >= 0 && d.y < goldRainH {
			thirds[int(3*d.y/goldRainH)]++
		}
	}
	for i, n := range thirds {
		if n == 0 {
			t.Errorf("no drop in third %d of the screen on the first frame (%v): the shower must already be under way", i, thirds)
		}
	}
}

func TestGoldRain_CountScalesWithScreen(t *testing.T) {
	count := func(w, h float64) int {
		r := newGoldRainForTest(1, w, h, h/900.0)
		defer r.free()
		return len(r.drops())
	}
	small, laptop, large := count(800, 500), count(goldRainW, goldRainH), count(5120, 2880)
	if !(small < laptop && laptop <= large) {
		t.Errorf("drops: 800x500 = %d, laptop = %d, 5K = %d; want more drops on a bigger screen", small, laptop, large)
	}
	if large > goldRainCap() {
		t.Errorf("5K view holds %d drops, over the capacity %d", large, goldRainCap())
	}
	if tiny := count(0, 0); tiny < 1 {
		t.Errorf("an empty view holds %d drops, want the minimum population", tiny)
	}
}

func TestGoldRain_DropsOnlyFallAndAllRespawn(t *testing.T) {
	r := newGoldRainForTest(11, goldRainW, goldRainH, goldRainUnit)
	defer r.free()
	prev := r.drops()
	// The farthest drop crosses the screen in 9 s; 20 s of frames recycles every
	// slot at least once.
	for f := 0; f < 600; f++ {
		r.step()
		cur := r.drops()
		for i, d := range cur {
			if d.x < -goldReach(d) || d.x > goldRainW+goldReach(d) {
				t.Fatalf("frame %d, drop %d: x = %v is past the side of a %v-wide view", f, i, d.x, goldRainW)
			}
			if d.spawns == prev[i].spawns {
				if !(d.y < prev[i].y) {
					t.Fatalf("frame %d, drop %d: y went from %v to %v; a drop only falls", f, i, prev[i].y, d.y)
				}
				continue
			}
			if d.y-goldReach(d) <= goldRainH {
				t.Fatalf("frame %d, drop %d respawned at y = %v (size %v): it must start out of sight above %v", f, i, d.y, d.size, goldRainH)
			}
		}
		prev = cur
	}
	for i, d := range prev {
		if d.spawns == 0 {
			t.Errorf("drop %d never left the screen in 20 s (y = %v, vy = %v)", i, d.y, d.vy)
		}
	}
}

func TestGoldRain_NearCoinsAreBiggerAndFaster(t *testing.T) {
	r := newGoldRainForTest(21, goldRainW, goldRainH, goldRainUnit)
	defer r.free()
	var nearV, farV, nearN, farN float64
	for f := 0; f < 900; f++ {
		r.step()
		for _, d := range r.drops() {
			switch {
			case d.depth > 0.75:
				nearV += d.vy
				nearN++
			case d.depth < 0.25:
				farV += d.vy
				farN++
			}
		}
	}
	if nearN == 0 || farN == 0 {
		t.Fatalf("no near (%v) or no far (%v) samples", nearN, farN)
	}
	if nearV/nearN < 1.5*farV/farN {
		t.Errorf("near coins fall at %.2f pt/frame on average, far ones at %.2f; want near clearly faster", nearV/nearN, farV/farN)
	}
	// The far half is drawn at 1 pt per art pixel and the near half at 2, so a
	// pixel never lands between device pixels.
	for _, d := range r.drops() {
		want := 12.0
		if d.depth >= 0.5 {
			want = 24.0
		}
		if d.size != want {
			t.Errorf("coin at depth %.2f is %v pt, want %v", d.depth, d.size, want)
		}
	}
	big := newGoldRainForTest(21, 2560, 1800, 2.0)
	defer big.free()
	for _, d := range big.drops() {
		if d.size != 24 && d.size != 48 {
			t.Fatalf("on a 1800-pt-high view a coin is %v pt, want 24 or 48 (2-pt art pixels)", d.size)
		}
	}
}

func TestGoldRain_CoinsSpinThroughEveryFrameAndFace(t *testing.T) {
	frames, _ := goldCoinArt()
	r := newGoldRainForTest(9, goldRainW, goldRainH, goldRainUnit)
	defer r.free()
	seen := make(map[int]bool)
	variants := make(map[int]bool)
	for f := 0; f < 300; f++ {
		r.step()
		for _, d := range r.drops() {
			seen[d.frame] = true
			variants[d.variant] = true
			// The glint frames are overlays, never a coin's own frame.
			if d.frame < 0 || d.frame >= len(frames)-2 {
				t.Fatalf("coin shows frame %d, want a spin or face frame (0..%d)", d.frame, len(frames)-3)
			}
			if d.frame == 0 && d.variant != 0 {
				t.Fatalf("a variant-%d coin shows the plain face", d.variant)
			}
		}
	}
	for f := 0; f < len(frames)-2; f++ {
		if !seen[f] {
			t.Errorf("no coin ever showed frame %d in 10 s", f)
		}
	}
	if len(variants) != 6 {
		t.Errorf("coin variants seen: %v, want the plain face and all 5 alternatives", variants)
	}
}

func TestGoldCoinArt_IsWellFormed(t *testing.T) {
	const side = 12
	frames, palette := goldCoinArt()
	if len(frames) != 15 {
		t.Fatalf("sprite has %d frames, want 8 spin + 5 faces + 2 glint", len(frames))
	}
	if strings.Contains(palette, ".") || len(palette) == 0 {
		t.Fatalf("palette %q: want colors only, '.' is transparent", palette)
	}
	for f, art := range frames {
		if len(art) != side*side {
			t.Fatalf("frame %d has %d pixels, want %d", f, len(art), side*side)
		}
		minX, maxX, minY, maxY := side, -1, side, -1
		for i, ch := range art {
			if ch == '.' {
				continue
			}
			if !strings.ContainsRune(palette, ch) {
				t.Fatalf("frame %d pixel %d is %q, not in the palette %q", f, i, ch, palette)
			}
			x, y := i%side, i/side
			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
		}
		if maxX < 0 {
			t.Fatalf("frame %d is empty", f)
		}
		// A coin spins in place: every coin frame is centered in the cell, or the
		// coin would wobble as it turns. The glint (the last two frames) sits on
		// the coin's lit upper-left on purpose.
		if f >= len(frames)-2 {
			continue
		}
		if c := float64(minX+maxX) / 2; math.Abs(c-5.5) > 1 {
			t.Errorf("frame %d spans columns %d..%d, centered at %v, want about 5.5", f, minX, maxX, c)
		}
		if c := float64(minY+maxY) / 2; math.Abs(c-5.5) > 1 {
			t.Errorf("frame %d spans rows %d..%d, centered at %v, want about 5.5", f, minY, maxY, c)
		}
	}
}

func TestGoldRain_SameSeedSameRain(t *testing.T) {
	run := func(seed uint64) []goldDrop {
		r := newGoldRainForTest(seed, goldRainW, goldRainH, goldRainUnit)
		defer r.free()
		for f := 0; f < 500; f++ {
			r.step()
		}
		return r.drops()
	}
	a, b, c := run(77), run(77), run(78)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("drop %d differs between runs with the same seed: %+v vs %+v", i, a[i], b[i])
		}
	}
	if a[0] == c[0] {
		t.Errorf("different seeds produced the same drop %+v", a[0])
	}
}
