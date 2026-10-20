//go:build darwin

package cocoa

/*
#include <stdlib.h>

extern int gold_cap_for_test(void);
extern void gold_sim_for_test(unsigned long long seed, double price, int framesPerCandle, int frames,
                              int visible, double pad,
                              double* outO, double* outH, double* outL, double* outC, int maxOut,
                              int* outCount, unsigned long long* outSeq,
                              double* outCur, double* outLo, double* outHi);
extern double gold_nice_step_for_test(double range, int lines);
extern void gold_format_price_for_test(double p, char* buf, int n);
extern int gold_rain_cap_for_test(void);
extern void* gold_rain_new_for_test(unsigned long long seed, double w, double h, double unit);
extern void gold_rain_step_for_test(void* r);
extern int gold_rain_read_for_test(void* r, double* outX, double* outY, double* outVY, double* outSize,
                                   double* outDepth, int* outVariant, int* outFrame, unsigned* outSpawns, int maxOut);
extern void gold_rain_free_for_test(void* r);
extern int gold_coin_art_for_test(int frame, char* buf, int n, char* palette, int pn);
*/
import "C"

import "unsafe"

// goldCandle mirrors the C GoldCandle: one candle's open, high, low and close.
type goldCandle struct {
	o, h, l, c float64
}

// goldSim is what goldSimForTest reports about a model run: the committed
// candles oldest first, the forming candle, the count of candles ever
// committed, and the axis range fitted to the most recent candles.
type goldSim struct {
	candles []goldCandle
	cur     goldCandle
	seq     uint64
	lo, hi  float64
}

// goldCap is the C ring capacity (GOLD_CAP): the most committed candles the
// model keeps.
func goldCap() int {
	return int(C.gold_cap_for_test())
}

// goldSimForTest runs the SAME pure gold_step GoldView runs every frame, from a
// fresh state seeded with seed and starting at price, for frames frames of
// framesPerCandle frames per candle. visible and pad feed gold_fit_range.
// Test-only helper: cgo cannot reach the static C model from a _test.go file,
// so this thin wrapper lives in the production file alongside dvdStepForTest.
func goldSimForTest(seed uint64, price float64, framesPerCandle, frames, visible int, pad float64) goldSim {
	capN := goldCap()
	o := make([]C.double, capN)
	h := make([]C.double, capN)
	l := make([]C.double, capN)
	c := make([]C.double, capN)
	var cur [4]C.double
	var count C.int
	var seq C.ulonglong
	var lo, hi C.double
	C.gold_sim_for_test(
		C.ulonglong(seed), C.double(price), C.int(framesPerCandle), C.int(frames),
		C.int(visible), C.double(pad),
		&o[0], &h[0], &l[0], &c[0], C.int(capN),
		&count, &seq, &cur[0], &lo, &hi,
	)
	out := goldSim{
		candles: make([]goldCandle, int(count)),
		cur:     goldCandle{float64(cur[0]), float64(cur[1]), float64(cur[2]), float64(cur[3])},
		seq:     uint64(seq),
		lo:      float64(lo),
		hi:      float64(hi),
	}
	for i := range out.candles {
		out.candles[i] = goldCandle{float64(o[i]), float64(h[i]), float64(l[i]), float64(c[i])}
	}
	return out
}

// goldNiceStepForTest is the grid step gold_nice_step picks for a price range
// split into about lines lines.
func goldNiceStepForTest(rangeV float64, lines int) float64 {
	return float64(C.gold_nice_step_for_test(C.double(rangeV), C.int(lines)))
}

// goldFormatPriceForTest is the axis/header label gold_format_price writes for p.
func goldFormatPriceForTest(p float64) string {
	const n = 64
	buf := (*C.char)(C.malloc(n))
	defer C.free(unsafe.Pointer(buf))
	C.gold_format_price_for_test(C.double(p), buf, n)
	return C.GoString(buf)
}

// goldDrop mirrors one C GoldDrop as the tests see it: size is the sprite's
// side in points, frame the sprite frame it shows now.
type goldDrop struct {
	x, y, vy, size, depth float64
	variant, frame        int
	spawns                uint
}

// goldRain is a handle on a C GoldRain, the falling coins behind the chart, stepped one frame at a time by the tests. Test-only, like
// goldSimForTest; free must be called.
type goldRain struct {
	p unsafe.Pointer
}

// newGoldRainForTest fills a w x h view, at layout scale unit, from seed.
func newGoldRainForTest(seed uint64, w, h, unit float64) *goldRain {
	return &goldRain{p: C.gold_rain_new_for_test(C.ulonglong(seed), C.double(w), C.double(h), C.double(unit))}
}

// step advances the rain by one frame, as GoldView does on every timer tick.
func (r *goldRain) step() {
	C.gold_rain_step_for_test(r.p)
}

// drops reports every drop the rain holds.
func (r *goldRain) drops() []goldDrop {
	capN := int(C.gold_rain_cap_for_test())
	x := make([]C.double, capN)
	y := make([]C.double, capN)
	vy := make([]C.double, capN)
	size := make([]C.double, capN)
	depth := make([]C.double, capN)
	variant := make([]C.int, capN)
	frame := make([]C.int, capN)
	spawns := make([]C.uint, capN)
	n := int(C.gold_rain_read_for_test(r.p, &x[0], &y[0], &vy[0], &size[0], &depth[0],
		&variant[0], &frame[0], &spawns[0], C.int(capN)))
	out := make([]goldDrop, n)
	for i := range out {
		out[i] = goldDrop{
			x: float64(x[i]), y: float64(y[i]), vy: float64(vy[i]),
			size: float64(size[i]), depth: float64(depth[i]),
			variant: int(variant[i]), frame: int(frame[i]), spawns: uint(spawns[i]),
		}
	}
	return out
}

func (r *goldRain) free() {
	C.gold_rain_free_for_test(r.p)
	r.p = nil
}

// goldRainCap is the C drop capacity (GOLD_RAIN_CAP).
func goldRainCap() int {
	return int(C.gold_rain_cap_for_test())
}

// goldCoinArt returns every frame of the compiled-in coin sprite, each as
// GOLD_COIN_PX^2 palette characters with row 0 at the top, and the palette
// characters ('.' is transparent and not among them).
func goldCoinArt() (frames []string, palette string) {
	const n = 1024
	buf := (*C.char)(C.malloc(n))
	defer C.free(unsafe.Pointer(buf))
	pal := (*C.char)(C.malloc(n))
	defer C.free(unsafe.Pointer(pal))
	count := int(C.gold_coin_art_for_test(-1, nil, 0, pal, n))
	for f := 0; f < count; f++ {
		C.gold_coin_art_for_test(C.int(f), buf, n, nil, 0)
		frames = append(frames, C.GoString(buf))
	}
	return frames, C.GoString(pal)
}
