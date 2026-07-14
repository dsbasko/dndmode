//go:build darwin

package cocoa

import "testing"

// TestDVDView_Step_Physics exercises the pure motion core behind the dvd overlay
// style (dvd_step, reached through the dvd_step_for_test cgo shim). It is the one
// unit-testable piece of the style — drawRect: output is owned by the WindowServer
// and can only be validated in the manual visual run. The cases pin the four
// behaviors that make a DVD bounce read right: edge reflection, in-bounds
// clamping, color advance on any bounce, and the corner flash.
//
// Geometry for every case: a 100x40 logo in a 1000x1000 view, speed 5 per axis.
const (
	dvdTestW       = 100.0
	dvdTestH       = 40.0
	dvdTestBoundsW = 1000.0
	dvdTestBoundsH = 1000.0
	dvdTestPalette = 8 // matches kDVDPaletteCount
)

func TestDVDView_Step_FreeDriftKeepsVelocity(t *testing.T) {
	// Well inside the bounds → no bounce: position advances by velocity, velocity
	// and color unchanged, flash stays 0.
	in := dvdPhys{x: 500, y: 500, vx: 5, vy: -5, colorIdx: 3, flash: 0}
	got := dvdStepForTest(in, dvdTestW, dvdTestH, dvdTestPalette, dvdTestBoundsW, dvdTestBoundsH)

	if got.x != 505 || got.y != 495 {
		t.Errorf("free drift position = (%g,%g), want (505,495)", got.x, got.y)
	}
	if got.vx != 5 || got.vy != -5 {
		t.Errorf("free drift velocity = (%g,%g), want (5,-5)", got.vx, got.vy)
	}
	if got.colorIdx != 3 {
		t.Errorf("free drift colorIdx = %d, want 3 (no bounce → no recolor)", got.colorIdx)
	}
	if got.flash != 0 {
		t.Errorf("free drift flash = %g, want 0", got.flash)
	}
}

func TestDVDView_Step_BounceReflectsAndClamps(t *testing.T) {
	tests := []struct {
		name           string
		in             dvdPhys
		wantX, wantY   float64
		wantVX, wantVY float64
	}{
		{
			// Crosses the left edge (x would go to -3) → clamp to 0, vx flips positive.
			name:  "left edge",
			in:    dvdPhys{x: 2, y: 500, vx: -5, vy: 5},
			wantX: 0, wantY: 505, wantVX: 5, wantVY: 5,
		},
		{
			// Crosses the right edge (x+w would exceed boundsW) → clamp so x+w == boundsW.
			name:  "right edge",
			in:    dvdPhys{x: 897, y: 500, vx: 5, vy: 5},
			wantX: dvdTestBoundsW - dvdTestW, wantY: 505, wantVX: -5, wantVY: 5,
		},
		{
			// Crosses the bottom edge (y would go negative) → clamp to 0, vy flips positive.
			name:  "bottom edge",
			in:    dvdPhys{x: 500, y: 3, vx: 5, vy: -5},
			wantX: 505, wantY: 0, wantVX: 5, wantVY: 5,
		},
		{
			// Crosses the top edge → clamp so y+h == boundsH, vy flips negative.
			name:  "top edge",
			in:    dvdPhys{x: 500, y: 958, vx: 5, vy: 5},
			wantX: 505, wantY: dvdTestBoundsH - dvdTestH, wantVX: 5, wantVY: -5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dvdStepForTest(tt.in, dvdTestW, dvdTestH, dvdTestPalette, dvdTestBoundsW, dvdTestBoundsH)
			if got.x != tt.wantX || got.y != tt.wantY {
				t.Errorf("position = (%g,%g), want (%g,%g)", got.x, got.y, tt.wantX, tt.wantY)
			}
			if got.vx != tt.wantVX || got.vy != tt.wantVY {
				t.Errorf("velocity = (%g,%g), want (%g,%g)", got.vx, got.vy, tt.wantVX, tt.wantVY)
			}
			// Clamped position must stay fully inside the bounds.
			if got.x < 0 || got.x+dvdTestW > dvdTestBoundsW || got.y < 0 || got.y+dvdTestH > dvdTestBoundsH {
				t.Errorf("clamped logo out of bounds: (%g,%g) size (%g,%g) in (%g,%g)",
					got.x, got.y, dvdTestW, dvdTestH, dvdTestBoundsW, dvdTestBoundsH)
			}
		})
	}
}

func TestDVDView_Step_ColorAdvancesOnBounce(t *testing.T) {
	// Any single-axis bounce advances the color by exactly 1 (mod paletteN), which
	// is always a different hue.
	in := dvdPhys{x: 2, y: 500, vx: -5, vy: 5, colorIdx: 7} // last index → wraps to 0
	got := dvdStepForTest(in, dvdTestW, dvdTestH, dvdTestPalette, dvdTestBoundsW, dvdTestBoundsH)
	if got.colorIdx != 0 {
		t.Errorf("colorIdx after bounce = %d, want 0 (wrap from 7)", got.colorIdx)
	}
	if got.colorIdx == in.colorIdx {
		t.Errorf("colorIdx unchanged (%d) after bounce; want a different hue", got.colorIdx)
	}
}

func TestDVDView_Step_CornerArmsFlash(t *testing.T) {
	// Both axes cross in the same frame (bottom-left corner) → flash armed to 1.0.
	in := dvdPhys{x: 2, y: 3, vx: -5, vy: -5, colorIdx: 0, flash: 0}
	got := dvdStepForTest(in, dvdTestW, dvdTestH, dvdTestPalette, dvdTestBoundsW, dvdTestBoundsH)
	if got.x != 0 || got.y != 0 {
		t.Errorf("corner position = (%g,%g), want (0,0)", got.x, got.y)
	}
	if got.vx != 5 || got.vy != 5 {
		t.Errorf("corner velocity = (%g,%g), want (5,5)", got.vx, got.vy)
	}
	if got.flash != 1.0 {
		t.Errorf("corner flash = %g, want 1.0", got.flash)
	}
}

func TestDVDView_Step_FlashDecaysWithoutCorner(t *testing.T) {
	// A prior flash fades on a plain (non-corner) frame and never goes negative.
	in := dvdPhys{x: 500, y: 500, vx: 5, vy: 5, flash: 0.1}
	got := dvdStepForTest(in, dvdTestW, dvdTestH, dvdTestPalette, dvdTestBoundsW, dvdTestBoundsH)
	if got.flash >= 0.1 || got.flash < 0 {
		t.Errorf("flash after decay = %g, want in [0,0.1)", got.flash)
	}

	// A second decay step from a small value clamps to exactly 0 (never negative).
	in2 := dvdPhys{x: 500, y: 500, vx: 5, vy: 5, flash: 0.02}
	got2 := dvdStepForTest(in2, dvdTestW, dvdTestH, dvdTestPalette, dvdTestBoundsW, dvdTestBoundsH)
	if got2.flash != 0 {
		t.Errorf("flash decayed from 0.02 = %g, want clamped to 0", got2.flash)
	}
}
