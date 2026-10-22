//go:build linux

package overlay

import (
	"math"
	"testing"
)

// The cases are the macOS TestDVDView_Step_* ones (cocoa/dvdview_test.go),
// so both implementations are pinned to the same behaviour: a 100x40 logo
// in a 1000x1000 view, speed 5 per axis, 8 colours.
const (
	dvdTestW       = 100.0
	dvdTestH       = 40.0
	dvdTestBoundsW = 1000.0
	dvdTestBoundsH = 1000.0
)

// frame is one reference frame in seconds.
const frame = 1.0 / animRefFPS

func dvdStep(in dvdState) dvdState {
	in.w, in.h, in.paletteN = dvdTestW, dvdTestH, len(dvdPalette)
	in.step(dvdTestBoundsW, dvdTestBoundsH, 1)
	return in
}

func TestDVD_Step_FreeDriftKeepsVelocity(t *testing.T) {
	got := dvdStep(dvdState{x: 500, y: 500, vx: 5, vy: -5, colorIdx: 3})
	if got.x != 505 || got.y != 495 || got.vx != 5 || got.vy != -5 || got.colorIdx != 3 || got.flash != 0 {
		t.Errorf("free drift = %+v", got)
	}
}

func TestDVD_Step_BounceReflectsAndClamps(t *testing.T) {
	tests := []struct {
		name           string
		in             dvdState
		wantX, wantY   float64
		wantVX, wantVY float64
	}{
		{"left edge", dvdState{x: 2, y: 500, vx: -5, vy: 5}, 0, 505, 5, 5},
		{"right edge", dvdState{x: 897, y: 500, vx: 5, vy: 5}, dvdTestBoundsW - dvdTestW, 505, -5, 5},
		{"low edge", dvdState{x: 500, y: 3, vx: 5, vy: -5}, 505, 0, 5, 5},
		{"high edge", dvdState{x: 500, y: 958, vx: 5, vy: 5}, 505, dvdTestBoundsH - dvdTestH, 5, -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dvdStep(tt.in)
			if got.x != tt.wantX || got.y != tt.wantY || got.vx != tt.wantVX || got.vy != tt.wantVY {
				t.Errorf("got pos (%g,%g) vel (%g,%g); want (%g,%g) (%g,%g)",
					got.x, got.y, got.vx, got.vy, tt.wantX, tt.wantY, tt.wantVX, tt.wantVY)
			}
			if got.x < 0 || got.x+dvdTestW > dvdTestBoundsW || got.y < 0 || got.y+dvdTestH > dvdTestBoundsH {
				t.Errorf("clamped logo out of bounds: %+v", got)
			}
		})
	}
}

func TestDVD_Step_ColorAdvancesOnBounce(t *testing.T) {
	got := dvdStep(dvdState{x: 2, y: 500, vx: -5, vy: 5, colorIdx: 7})
	if got.colorIdx != 0 {
		t.Errorf("colorIdx after bounce = %d, want 0 (wrap from 7)", got.colorIdx)
	}
}

func TestDVD_Step_CornerArmsFlash(t *testing.T) {
	got := dvdStep(dvdState{x: 2, y: 3, vx: -5, vy: -5})
	if got.x != 0 || got.y != 0 || got.vx != 5 || got.vy != 5 || got.flash != 1 {
		t.Errorf("corner = %+v, want (0,0) vel (5,5) flash 1", got)
	}
	if c := got.color(); c != (dvdRGB{1, 1, 1}) {
		t.Errorf("full flash colour = %+v, want white", c)
	}
}

func TestDVD_Step_FlashDecaysWithoutCorner(t *testing.T) {
	if got := dvdStep(dvdState{x: 500, y: 500, vx: 5, vy: 5, flash: 0.1}); got.flash >= 0.1 || got.flash < 0 {
		t.Errorf("flash after decay = %g, want in [0,0.1)", got.flash)
	}
	if got := dvdStep(dvdState{x: 500, y: 500, vx: 5, vy: 5, flash: 0.02}); got.flash != 0 {
		t.Errorf("flash decayed from 0.02 = %g, want 0", got.flash)
	}
}

// TestDVDView_TickSeedsOnResize: the first tick for a size seeds an in-bounds
// logo of the right proportions; later ticks move it.
func TestDVDView_TickSeedsOnResize(t *testing.T) {
	v := &dvdView{logoAspect: 470.0 / 270.0}
	v.tick(0, 0, frame) // not laid out yet: nothing happens
	if v.st.w != 0 {
		t.Fatal("seeded before the widget had a size")
	}
	v.tick(1920, 1080, frame)
	if v.st.w != 1920*dvdLogoWidthFrac || v.st.x < 0 || v.st.x+v.st.w > 1920 || v.st.y < 0 || v.st.y+v.st.h > 1080 {
		t.Fatalf("seed = %+v", v.st)
	}
	x := v.st.x
	v.tick(1920, 1080, frame)
	if v.st.x == x {
		t.Error("logo did not move on the second tick")
	}
	v.tick(640, 480, frame)
	if want := max(dvdMinLogoWidth, 640*dvdLogoWidthFrac); v.st.w != want {
		t.Errorf("resize logo width = %g, want %g", v.st.w, want)
	}
	v.tick(300, 200, frame) // small monitor: the width floor applies
	if v.st.w != dvdMinLogoWidth {
		t.Errorf("small-monitor logo width = %g, want %g", v.st.w, dvdMinLogoWidth)
	}
}

func TestDVDLogoAlpha(t *testing.T) {
	alpha, w, h, err := dvdLogoAlpha()
	if err != nil {
		t.Fatal(err)
	}
	if w != 470 || h != 270 || len(alpha) != w*h {
		t.Fatalf("logo %dx%d, %d bytes", w, h, len(alpha))
	}
	// Corners are background (not painted), and some of the body is.
	if alpha[0] != 0 || alpha[len(alpha)-1] != 0 {
		t.Error("corner pixels are painted")
	}
	painted := 0
	for _, a := range alpha {
		if a == 255 {
			painted++
		}
	}
	if painted < len(alpha)/10 {
		t.Errorf("only %d of %d pixels painted", painted, len(alpha))
	}
}

// TestAnimFrames_RateIndependent: one second covers the same distance at 30,
// 60 and 144 Hz, and a long stall is capped instead of jumping.
func TestAnimFrames_RateIndependent(t *testing.T) {
	for _, hz := range []float64{30, 60, 144} {
		s := dvdState{x: 100, y: 500, vx: dvdSpeed, vy: 0, w: dvdTestW, h: dvdTestH, paletteN: 8}
		for range int(hz) {
			s.step(dvdTestBoundsW, dvdTestBoundsH, animFrames(1/hz))
		}
		if want := 100 + dvdSpeed*animRefFPS; math.Abs(s.x-want) > 1e-6 {
			t.Errorf("%g Hz: x after 1 s = %g, want %g", hz, s.x, want)
		}
	}
	if got := animFrames(5); got != animMaxDt*animRefFPS {
		t.Errorf("5 s stall = %g frames, want capped at %g", got, animMaxDt*animRefFPS)
	}
	if animFrames(-1) != 0 {
		t.Error("negative dt must not move anything")
	}
}
