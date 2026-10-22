//go:build linux

package overlay

import (
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The model cases are the macOS TestGoldView_* / TestGoldRain_* /
// TestGoldCoinArt_* ones (cocoa/goldview_test.go), run against the Go twin
// of the C model, so both implementations are pinned to the same behaviour.
// Candles span 240 frames, as in the view.
const (
	goldTestFrames = 240
	goldTestPrice  = 4000.0
)

// goldSim is what a model run reports: the committed candles oldest first,
// the forming candle, the count of candles ever committed and the axis
// range fitted to the most recent candles.
type goldSim struct {
	candles []goldCandle
	cur     goldCandle
	seq     uint64
	lo, hi  float64
}

func goldSimRun(seed uint64, price float64, framesPerCandle, frames, visible int, pad float64) goldSim {
	var s goldState
	s.init(seed, price, framesPerCandle)
	for range frames {
		s.step()
	}
	out := goldSim{candles: make([]goldCandle, s.count), cur: s.cur, seq: s.seq}
	for i := range out.candles {
		out.candles[i] = s.at(s.count - 1 - i)
	}
	out.lo, out.hi = s.fitRange(visible, pad)
	return out
}

func goldRun(seed uint64, candles int) goldSim {
	return goldSimRun(seed, goldTestPrice, goldTestFrames, candles*goldTestFrames, 100, 0.12)
}

func TestGold_CandlesAreWellFormed(t *testing.T) {
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
		if (c.h-c.l)/c.o > 0.01 {
			t.Errorf("candle %d = %+v: range %.3f%% of the open, want under 1%%", i, c, 100*(c.h-c.l)/c.o)
		}
	}
}

func TestGold_EachCandleOpensAtPreviousClose(t *testing.T) {
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

func TestGold_SameSeedSameChart(t *testing.T) {
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

func TestGold_PriceClimbs(t *testing.T) {
	// 400 candles of drift is ~8% against ~1.6% of noise, so every seed must
	// end well above where it started, and most candles must close up.
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

func TestGold_RingWrapsKeepingContinuity(t *testing.T) {
	sim := goldRun(99, goldCap+75)
	if len(sim.candles) != goldCap {
		t.Fatalf("held candles = %d, want the ring capacity %d", len(sim.candles), goldCap)
	}
	if sim.seq != uint64(goldCap+75) {
		t.Errorf("seq = %d, want %d candles ever committed", sim.seq, goldCap+75)
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

func TestGold_FitRangeCoversVisibleCandles(t *testing.T) {
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

func TestGold_FitRangeNeverCollapses(t *testing.T) {
	sim := goldSimRun(1, goldTestPrice, goldTestFrames, 0, 100, 0.12)
	if !(sim.hi > sim.lo) {
		t.Fatalf("flat chart fitted to [%v, %v], want a non-empty range", sim.lo, sim.hi)
	}
	if sim.lo >= goldTestPrice || sim.hi <= goldTestPrice {
		t.Errorf("flat chart range [%v, %v] does not contain the price %v", sim.lo, sim.hi, goldTestPrice)
	}
}

func TestGold_NiceStep(t *testing.T) {
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
		if got := goldNiceStep(tt.rangeV, tt.lines); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("nice step for range %v / %d lines = %v, want %v", tt.rangeV, tt.lines, got, tt.want)
		}
	}
}

func TestGold_FormatPrice(t *testing.T) {
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
		if got := goldFormatPrice(tt.in); got != tt.want {
			t.Errorf("format(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The falling coins are stepped frame by frame on a laptop-sized view.
const (
	goldRainW    = 1512.0
	goldRainH    = 982.0
	goldRainUnit = 982.0 / 900.0
)

func newGoldRain(seed uint64, w, h, unit float64) *goldRain {
	r := &goldRain{}
	r.init(seed, w, h, unit)
	return r
}

// drops copies the coins, so a later step does not change them.
func (r *goldRain) drops() []goldDrop { return append([]goldDrop(nil), r.d...) }

func TestGoldRain_FillsScreenFromFirstFrame(t *testing.T) {
	drops := newGoldRain(3, goldRainW, goldRainH, goldRainUnit).drops()
	if len(drops) < 300 || len(drops) > goldRainCap {
		t.Fatalf("coins on a %vx%v view = %d, want 300..%d", goldRainW, goldRainH, len(drops), goldRainCap)
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
	count := func(w, h float64) int { return len(newGoldRain(1, w, h, h/900.0).d) }
	small, laptop, large := count(800, 500), count(goldRainW, goldRainH), count(5120, 2880)
	if !(small < laptop && laptop <= large) {
		t.Errorf("drops: 800x500 = %d, laptop = %d, 5K = %d; want more drops on a bigger screen", small, laptop, large)
	}
	if large > goldRainCap {
		t.Errorf("5K view holds %d drops, over the capacity %d", large, goldRainCap)
	}
	if tiny := count(0, 0); tiny < 1 {
		t.Errorf("an empty view holds %d drops, want the minimum population", tiny)
	}
}

func TestGoldRain_DropsOnlyFallAndAllRespawn(t *testing.T) {
	r := newGoldRain(11, goldRainW, goldRainH, goldRainUnit)
	prev := r.drops()
	// The farthest drop crosses the screen in 8 s; 20 s of frames recycles
	// every slot at least once.
	for f := 0; f < 600; f++ {
		r.step(1)
		cur := r.drops()
		for i, d := range cur {
			if d.x < -d.reach() || d.x > goldRainW+d.reach() {
				t.Fatalf("frame %d, drop %d: x = %v is past the side of a %v-wide view", f, i, d.x, goldRainW)
			}
			if d.spawns == prev[i].spawns {
				if !(d.y < prev[i].y) {
					t.Fatalf("frame %d, drop %d: y went from %v to %v; a drop only falls", f, i, prev[i].y, d.y)
				}
				continue
			}
			if d.y-d.reach() <= goldRainH {
				t.Fatalf("frame %d, drop %d respawned at y = %v (size %v): it must start out of sight above %v", f, i, d.y, d.size(), goldRainH)
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
	r := newGoldRain(21, goldRainW, goldRainH, goldRainUnit)
	var nearV, farV, nearN, farN float64
	for f := 0; f < 900; f++ {
		r.step(1)
		for _, d := range r.d {
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
		t.Errorf("near coins fall at %.2f px/frame on average, far ones at %.2f; want near clearly faster", nearV/nearN, farV/farN)
	}
	// The far half is drawn at 1 px per art pixel and the near half at 2.
	for _, d := range r.d {
		want := 12.0
		if d.depth >= 0.5 {
			want = 24.0
		}
		if d.size() != want {
			t.Errorf("coin at depth %.2f is %v px, want %v", d.depth, d.size(), want)
		}
	}
	for _, d := range newGoldRain(21, 2560, 1800, 2.0).d {
		if d.size() != 24 && d.size() != 48 {
			t.Fatalf("on a 1800-px-high view a coin is %v px, want 24 or 48 (2-px art pixels)", d.size())
		}
	}
}

func TestGoldRain_CoinsSpinThroughEveryFrameAndFace(t *testing.T) {
	r := newGoldRain(9, goldRainW, goldRainH, goldRainUnit)
	seen := make(map[int]bool)
	variants := make(map[int]bool)
	for f := 0; f < 300; f++ {
		r.step(1)
		for _, d := range r.d {
			fr := d.frame()
			seen[fr] = true
			variants[d.variant] = true
			// The glint frames are overlays, never a coin's own frame.
			if fr < 0 || fr >= goldCoinGlint {
				t.Fatalf("coin shows frame %d, want a spin or face frame (0..%d)", fr, goldCoinGlint-1)
			}
			if fr == 0 && d.variant != 0 {
				t.Fatalf("a variant-%d coin shows the plain face", d.variant)
			}
		}
	}
	for f := range goldCoinGlint {
		if !seen[f] {
			t.Errorf("no coin ever showed frame %d in 10 s", f)
		}
	}
	if len(variants) != 6 {
		t.Errorf("coin variants seen: %v, want the plain face and all 5 alternatives", variants)
	}
}

func TestGoldCoinArt_IsWellFormed(t *testing.T) {
	const side = goldCoinPx
	if strings.Contains(goldCoinPalette, ".") || len(goldCoinColors) != len(goldCoinPalette) {
		t.Fatalf("palette %q with %d colours: want colours only, '.' is transparent", goldCoinPalette, len(goldCoinColors))
	}
	for f, art := range goldCoinArt {
		if len(art) != side*side {
			t.Fatalf("frame %d has %d pixels, want %d", f, len(art), side*side)
		}
		minX, maxX, minY, maxY := side, -1, side, -1
		for i, ch := range art {
			if ch == '.' {
				continue
			}
			if !strings.ContainsRune(goldCoinPalette, ch) {
				t.Fatalf("frame %d pixel %d is %q, not in the palette %q", f, i, ch, goldCoinPalette)
			}
			x, y := i%side, i/side
			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
		}
		if maxX < 0 {
			t.Fatalf("frame %d is empty", f)
		}
		// Every coin frame is centered, or the coin would wobble as it turns.
		// The glint sits on the coin's lit upper-left on purpose.
		if f >= goldCoinGlint {
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

// TestGoldCoinArt_MatchesMacOS keeps the copied sprite identical to the
// macOS kGoldCoinArt.
func TestGoldCoinArt_MatchesMacOS(t *testing.T) {
	src, err := os.ReadFile("../../macos/cocoa/goldview_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "kGoldCoinArt[GOLD_COIN_FRAMES] = {")
	end := strings.Index(s[start:], "};")
	if start < 0 || end < 0 {
		t.Fatal("kGoldCoinArt not found in goldview_darwin.m")
	}
	var frames []string
	var cur strings.Builder
	for _, line := range strings.Split(s[start:start+end], "\n") {
		m := regexp.MustCompile(`^\s*"([^"]*)"(,?)`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cur.WriteString(m[1])
		if m[2] == "," {
			frames = append(frames, cur.String())
			cur.Reset()
		}
	}
	if len(frames) != goldCoinFrames {
		t.Fatalf("parsed %d macOS frames, want %d", len(frames), goldCoinFrames)
	}
	for f := range frames {
		if frames[f] != goldCoinArt[f] {
			t.Errorf("frame %d differs from the macOS sprite", f)
		}
	}
}

func TestGoldSprites_Baked(t *testing.T) {
	px := goldSprites()
	if len(px) != goldCoinFrames*goldShades*goldCoinPx*goldCoinPx {
		t.Fatalf("baked %d pixels", len(px))
	}
	// Frame 0 pixel 4 is outline 'o': opaque, brighter in nearer shades.
	at := func(frame, shade, i int) uint32 { return px[((frame*goldShades+shade)*goldCoinPx*goldCoinPx)+i] }
	if at(0, 0, 0) != 0 {
		t.Errorf("transparent pixel = %#x, want 0", at(0, 0, 0))
	}
	far, near := at(0, 0, 4), at(0, goldShades-1, 4)
	if far>>24 != 0xFF || near>>24 != 0xFF || (near>>16)&0xFF <= (far>>16)&0xFF {
		t.Errorf("outline far %#x near %#x: want opaque, near brighter", far, near)
	}
}

func TestGoldRain_SameSeedSameRain(t *testing.T) {
	run := func(seed uint64) []goldDrop {
		r := newGoldRain(seed, goldRainW, goldRainH, goldRainUnit)
		for range 500 {
			r.step(1)
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

// TestGoldView_TickSeedsFullChart: the first tick with a size fills the
// chart and fits the axis around the live price; nothing happens before
// the widget has a size.
func TestGoldView_TickSeedsFullChart(t *testing.T) {
	v := &goldView{}
	v.tick(0, 0, frame)
	if v.seeded {
		t.Fatal("seeded before the widget had a size")
	}
	v.tick(1920, 1080, frame)
	if !v.seeded || v.st.count < v.visible {
		t.Fatalf("after the first tick: seeded %v, %d candles for %d visible", v.seeded, v.st.count, v.visible)
	}
	if !(v.lo < v.st.cur.l && v.st.cur.h < v.hi) {
		t.Errorf("axis [%v, %v] does not hold the live candle [%v, %v]", v.lo, v.hi, v.st.cur.l, v.st.cur.h)
	}
	if v.chartY <= 0 || v.chartY+v.chartH >= 1080 || v.chartX+v.chartW >= 1920 {
		t.Errorf("chart (%v, %v, %v, %v) leaves no room for the header, axis or bottom pad", v.chartX, v.chartY, v.chartW, v.chartH)
	}
	if len(v.rain.d) == 0 {
		t.Error("no coins after seeding")
	}
}

// TestGoldView_CandleCadenceIsRateIndependent: a candle takes 8 s at any
// refresh rate, and the scroll progress grows smoothly between commits.
func TestGoldView_CandleCadenceIsRateIndependent(t *testing.T) {
	for _, hz := range []float64{30, 60, 144} {
		v := &goldView{}
		v.tick(1920, 1080, 0)
		seq := v.st.seq
		prev := v.progress()
		// One tick over 8 s, so rounding in the summed dt cannot miss the commit.
		ticks := int(hz * goldFramesPerCandle / animRefFPS)
		for i := range ticks + 1 {
			v.tick(1920, 1080, 1/hz)
			if p := v.progress(); i < ticks-2 && !(p > prev) {
				t.Fatalf("%g Hz tick %d: progress %v after %v, want it growing", hz, i, p, prev)
			}
			prev = v.progress()
		}
		if got := v.st.seq - seq; got != 1 {
			t.Errorf("%g Hz: %d candles committed in 8 s, want 1", hz, got)
		}
	}
}

// TestGoldView_ResizeKeepsPrice: a resize re-lays out the chart and the
// rain but keeps the price history.
func TestGoldView_ResizeKeepsPrice(t *testing.T) {
	v := &goldView{}
	v.tick(1920, 1080, 0)
	cur, seq, chartW := v.st.cur, v.st.seq, v.chartW
	v.tick(1280, 720, frame)
	if v.st.cur != cur || v.st.seq != seq {
		t.Error("resize changed the price")
	}
	if v.chartW >= chartW || v.boundsW != 1280 {
		t.Errorf("resize to 1280 kept the chart %v px wide (was %v)", v.chartW, chartW)
	}
	if !(v.lo < v.st.cur.c && v.st.cur.c < v.hi) {
		t.Errorf("axis [%v, %v] lost the live price %v", v.lo, v.hi, v.st.cur.c)
	}
}
