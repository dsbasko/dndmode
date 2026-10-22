//go:build linux

package overlay

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
	"math"
	"math/rand/v2"
)

// The dvd style: the "DVD VIDEO" logo bouncing edge to edge over the black
// shield, recoloured on every bounce and flashing white when it lands
// exactly in a corner — the old DVD-player screensaver. A port of the macOS
// DVDView (internal/macos/cocoa/dvdview_darwin.m): same logo, palette,
// speed, size and physics. Cosmetic only; the shield stays opaque.

const (
	dvdLogoWidthFrac = 0.16 // logo width as a fraction of the monitor width
	dvdMinLogoWidth  = 80.0 // floor so small monitors stay legible
	dvdSpeed         = 2.5  // pixels per reference frame (~75 px/s)
	dvdFlashDecay    = 0.08 // corner-flash fade per reference frame (~0.4 s to zero)
)

// dvdRGB is a palette colour, components in 0..1.
type dvdRGB struct{ r, g, b float64 }

// dvdPalette is the neon palette; the logo moves to the next entry on every
// bounce, so the new colour always differs from the previous one.
var dvdPalette = []dvdRGB{
	{1.00, 0.20, 0.20}, // red
	{1.00, 0.55, 0.10}, // orange
	{1.00, 0.95, 0.20}, // yellow
	{0.30, 1.00, 0.35}, // green
	{0.20, 0.95, 1.00}, // cyan
	{0.35, 0.55, 1.00}, // blue
	{0.80, 0.35, 1.00}, // purple
	{1.00, 0.40, 0.85}, // pink
}

// dvdLogoPNG is the DVD-VIDEO mark as an 8-bit grayscale PNG: 0 is the logo
// body, 255 the background and the disc's oval hole. Extracted byte for byte
// from internal/macos/cocoa/dvdlogo_darwin.h.
//
//go:embed dvdlogo.png
var dvdLogoPNG []byte

// dvdLogoAlpha decodes the logo into an alpha mask (255 = paint), row-major,
// w*h bytes. Anti-aliased edges survive as partial coverage.
func dvdLogoAlpha() (alpha []byte, w, h int, err error) {
	img, err := png.Decode(bytes.NewReader(dvdLogoPNG))
	if err != nil {
		return nil, 0, 0, err
	}
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	alpha = make([]byte, w*h)
	gray, ok := img.(*image.Gray)
	for y := range h {
		for x := range w {
			var v uint8
			if ok {
				v = gray.GrayAt(b.Min.X+x, b.Min.Y+y).Y
			} else {
				r, _, _, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				v = uint8(r >> 8)
			}
			alpha[y*w+x] = 255 - v
		}
	}
	return alpha, w, h, nil
}

// dvdState is the motion state of one logo. step is the Go twin of the
// macOS dvd_step and must stay behaviour-identical to it.
type dvdState struct {
	x, y     float64 // logo origin (top-left), pixels
	vx, vy   float64 // velocity, pixels per frame
	w, h     float64 // logo size
	colorIdx int
	paletteN int
	flash    float64 // 0..1, 1 = just hit a corner
}

// step advances by frames reference frames (1 = one macOS frame at 30 FPS;
// fractional at higher refresh rates): move, clamp and reflect off each
// edge, advance the colour on any bounce, arm the flash when both axes
// bounce at once.
func (s *dvdState) step(boundsW, boundsH, frames float64) {
	s.x += s.vx * frames
	s.y += s.vy * frames

	bounceX, bounceY := false, false
	if s.x < 0 {
		s.x = 0
		s.vx = math.Abs(s.vx)
		bounceX = true
	} else if s.x+s.w > boundsW {
		s.x = boundsW - s.w
		s.vx = -math.Abs(s.vx)
		bounceX = true
	}
	if s.y < 0 {
		s.y = 0
		s.vy = math.Abs(s.vy)
		bounceY = true
	} else if s.y+s.h > boundsH {
		s.y = boundsH - s.h
		s.vy = -math.Abs(s.vy)
		bounceY = true
	}

	if (bounceX || bounceY) && s.paletteN > 1 {
		s.colorIdx = (s.colorIdx + 1) % s.paletteN
	}
	if bounceX && bounceY {
		s.flash = 1
	} else if s.flash > 0 {
		s.flash = max(s.flash-dvdFlashDecay*frames, 0)
	}
}

// color is the current palette colour lerped towards white by the flash.
func (s *dvdState) color() dvdRGB {
	c := dvdPalette[s.colorIdx%len(dvdPalette)]
	f := s.flash
	return dvdRGB{c.r + (1-c.r)*f, c.g + (1-c.g)*f, c.b + (1-c.b)*f}
}

// dvdView is the per-window animation: the state plus the bounds it was
// seeded for, so a resize re-seeds like the macOS setFrameSize does.
type dvdView struct {
	st               dvdState
	boundsW, boundsH float64
	logoAspect       float64
}

// resize sizes the logo for the new bounds and seeds a random position,
// diagonal direction and starting colour.
func (v *dvdView) resize(boundsW, boundsH float64) {
	v.boundsW, v.boundsH = boundsW, boundsH
	w := max(dvdMinLogoWidth, boundsW*dvdLogoWidthFrac)
	v.st.w, v.st.h = w, w/v.logoAspect
	freeW, freeH := max(0, boundsW-v.st.w), max(0, boundsH-v.st.h)
	v.st.x = rand.Float64() * freeW
	v.st.y = rand.Float64() * freeH
	v.st.vx, v.st.vy = dvdSpeed, dvdSpeed
	if rand.IntN(2) == 0 {
		v.st.vx = -dvdSpeed
	}
	if rand.IntN(2) == 0 {
		v.st.vy = -dvdSpeed
	}
	v.st.paletteN = len(dvdPalette)
	v.st.colorIdx = rand.IntN(v.st.paletteN)
	v.st.flash = 0
}

// tick advances by dt seconds for a widget of the given size.
func (v *dvdView) tick(boundsW, boundsH, dt float64) {
	if boundsW <= 0 || boundsH <= 0 {
		return
	}
	if boundsW != v.boundsW || boundsH != v.boundsH {
		v.resize(boundsW, boundsH)
		return
	}
	v.st.step(boundsW, boundsH, animFrames(dt))
}

// animRefFPS is the frame rate the macOS speeds are expressed in. Linux
// draws on every display refresh (60 Hz and up) and scales the motion by
// the time that actually passed, so speeds stay identical and only the
// steps get finer.
const animRefFPS = 30

// animMaxDt caps one step, so a stall (suspend, a busy compositor) does
// not make the logo or the rain jump across the screen.
const animMaxDt = 0.1

// animFrames converts elapsed seconds into reference frames.
func animFrames(dt float64) float64 {
	return min(max(dt, 0), animMaxDt) * animRefFPS
}
