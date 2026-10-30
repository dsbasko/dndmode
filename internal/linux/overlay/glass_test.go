//go:build linux

package overlay

import (
	"math"
	"testing"
)

// The glass style's pure half: where each monitor sits in the desktop shot,
// how small the blur may work, and the blur itself. The capture and the
// drawing are C and are covered by the manual smoke test.

func TestGlassBounds_CoversEveryMonitor(t *testing.T) {
	got := glassBounds([]glassRect{{0, 0, 1920, 1080}, {1920, -200, 2560, 1440}})
	want := glassRect{0, -200, 4480, 1440}
	if got != want {
		t.Fatalf("bounds = %+v, want %+v", got, want)
	}
	if got := glassBounds(nil); got != (glassRect{}) {
		t.Fatalf("bounds of no monitors = %+v, want zero", got)
	}
}

func TestGlassCrop_MapsMonitorIntoShot(t *testing.T) {
	left := glassRect{0, 0, 1920, 1080}
	right := glassRect{1920, 0, 1920, 1080}
	both := glassBounds([]glassRect{left, right})
	cases := []struct {
		name        string
		mon, bounds glassRect
		imgW, imgH  int
		want        [4]float64
		wantOK      bool
	}{
		{"single monitor, same size", left, left, 1920, 1080, [4]float64{0, 0, 1920, 1080}, true},
		{"single monitor, scale 2", left, left, 3840, 2160, [4]float64{0, 0, 3840, 2160}, true},
		{"right of two", right, both, 3840, 1080, [4]float64{1920, 0, 1920, 1080}, true},
		{"right of two, scale 2", right, both, 7680, 2160, [4]float64{3840, 0, 3840, 2160}, true},
		{"monitor outside the shot", glassRect{3840, 0, 1920, 1080}, both, 3840, 1080, [4]float64{}, false},
		{"monitor partly outside", glassRect{1000, 0, 1920, 1080}, left, 1920, 1080, [4]float64{}, false},
		{"empty shot", left, left, 0, 0, [4]float64{}, false},
		{"empty monitor", glassRect{0, 0, 0, 1080}, left, 1920, 1080, [4]float64{}, false},
	}
	for _, c := range cases {
		x, y, w, h, ok := glassCrop(c.mon, c.bounds, c.imgW, c.imgH)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && [4]float64{x, y, w, h} != c.want {
			t.Errorf("%s: crop = %v, want %v", c.name, [4]float64{x, y, w, h}, c.want)
		}
	}
}

func TestGlassWork_ShrinksForWideBlurs(t *testing.T) {
	cases := []struct {
		sigma        float64
		wantW, wantH int
		wantSigma    float64
	}{
		{0, 1920, 1080, 0},
		{3, 1920, 1080, 3},
		{8, 960, 540, 4},
		{16, 480, 270, 4},
		{100, 480, 270, 25},
	}
	for _, c := range cases {
		w, h, s := glassWork(1920, 1080, c.sigma)
		if w != c.wantW || h != c.wantH || s != c.wantSigma {
			t.Errorf("sigma %g: work = %dx%d sigma %g, want %dx%d sigma %g", c.sigma, w, h, s, c.wantW, c.wantH, c.wantSigma)
		}
	}
	if w, h, _ := glassWork(3, 2, 16); w != 1 || h != 1 {
		t.Errorf("tiny monitor: work = %dx%d, want at least 1x1", w, h)
	}
}

func TestGlassBoxes_ApproximateTheGaussian(t *testing.T) {
	if got := glassBoxes(16); got != [3]int{31, 31, 33} {
		t.Errorf("boxes(16) = %v, want [31 31 33]", got)
	}
	if got := glassBoxes(0); got != [3]int{1, 1, 1} {
		t.Errorf("boxes(0) = %v, want [1 1 1]", got)
	}
	for _, s := range []float64{0.5, 2, 4, 7.3, 25} {
		b := glassBoxes(s)
		var v float64
		for _, d := range b {
			if d%2 == 0 {
				t.Errorf("boxes(%g) = %v: width %d is even", s, b, d)
			}
			v += float64(d*d-1) / 12
		}
		if got := math.Sqrt(v); math.Abs(got-s) > 0.6 {
			t.Errorf("boxes(%g) = %v give sigma %.2f", s, b, got)
		}
	}
}

// glassTestImage is a w x h ARGB32 buffer with a 2-pixel row padding, so
// the blur has to honour the stride.
func glassTestImage(w, h int, fill byte) (px []byte, stride int) {
	stride = w*4 + 8
	px = make([]byte, stride*h)
	for y := range h {
		for x := range w * 4 {
			px[y*stride+x] = fill
		}
	}
	return px, stride
}

func TestGlassBlur_KeepsAFlatImage(t *testing.T) {
	px, stride := glassTestImage(40, 30, 200)
	glassBlur(px, 40, 30, stride, 6)
	for y := range 30 {
		for x := range 40 * 4 {
			if v := px[y*stride+x]; v != 200 {
				t.Fatalf("pixel byte (%d, %d) = %d, want 200 (edges must not darken)", x, y, v)
			}
		}
		for x := 40 * 4; x < stride; x++ {
			if px[y*stride+x] != 0 {
				t.Fatalf("row %d padding byte %d was touched", y, x)
			}
		}
	}
}

func TestGlassBlur_SpreadsAPointEvenly(t *testing.T) {
	const w, h = 41, 41
	px, stride := glassTestImage(w, h, 0)
	at := func(x, y, c int) int { return int(px[y*stride+x*4+c]) }
	for c := range 4 {
		px[20*stride+20*4+c] = 255
	}
	before := px[20*stride+20*4]
	glassBlur(px, w, h, stride, 3)
	if at(20, 20, 0) >= int(before) {
		t.Fatalf("centre = %d, want below %d after blur", at(20, 20, 0), before)
	}
	for _, d := range []int{1, 3, 6} {
		ring := []int{at(20+d, 20, 1), at(20-d, 20, 1), at(20, 20+d, 1), at(20, 20-d, 1)}
		// Left and right match exactly; across axes the per-pass rounding
		// may differ by a step or two.
		if ring[0] != ring[1] || ring[2] != ring[3] || abs(ring[0]-ring[2]) > 2 {
			t.Fatalf("distance %d: %v, want the same value in every direction", d, ring)
		}
		if d > 1 && ring[0] > at(20+d-2, 20, 1) {
			t.Fatalf("distance %d: %d brighter than nearer pixel %d", d, ring[0], at(20+d-2, 20, 1))
		}
	}
	if at(0, 0, 2) != 0 {
		t.Fatalf("far corner = %d, want untouched by a sigma-3 blur", at(0, 0, 2))
	}
}

func TestGlassBlur_ZeroSigmaIsANoOp(t *testing.T) {
	px, stride := glassTestImage(8, 8, 0)
	px[3*stride+3*4] = 99
	glassBlur(px, 8, 8, stride, 0)
	if px[3*stride+3*4] != 99 {
		t.Fatalf("pixel changed with sigma 0")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
