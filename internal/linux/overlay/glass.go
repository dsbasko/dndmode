//go:build linux

package overlay

import "math"

// The glass style: the desktop as it was when the shield went up, blurred
// and faintly darkened, a port of the macOS one. Like on macOS it is one
// still taken before the windows are shown, not a live backdrop: GNOME
// gives clients no blur-behind, and a still cannot vanish. The shield
// window stays opaque, it just paints that picture.
//
// The capture (xdg-desktop-portal Screenshot) and the painting are C in
// overlay_linux.c; this file is the pure half: where each monitor lies in
// the shot, how small the blur may work, and the blur.

const (
	// glassTint is the black laid over the blurred shot (kGlassTintAlpha).
	glassTint = 0.15
	// glassWorkSigma is the blur the working image must keep per pixel:
	// wider blurs are computed on a shot shrunk by up to glassMaxShrink and
	// scaled back up, which a blurred picture survives unchanged.
	glassWorkSigma = 4.0
	glassMaxShrink = 4
)

// glassRect is a monitor or the whole desktop in logical pixels.
type glassRect struct{ x, y, w, h int }

// glassBounds is the smallest rectangle holding every monitor: the area a
// portal screenshot covers.
func glassBounds(mons []glassRect) glassRect {
	if len(mons) == 0 {
		return glassRect{}
	}
	x0, y0 := mons[0].x, mons[0].y
	x1, y1 := x0+mons[0].w, y0+mons[0].h
	for _, m := range mons[1:] {
		x0, y0 = min(x0, m.x), min(y0, m.y)
		x1, y1 = max(x1, m.x+m.w), max(y1, m.y+m.h)
	}
	return glassRect{x0, y0, x1 - x0, y1 - y0}
}

// glassCrop maps monitor mon into the pixels of a shot of imgW x imgH that
// covers bounds. ok is false when the monitor is not wholly inside the shot
// (one plugged in after it was taken) or either side is empty.
func glassCrop(mon, bounds glassRect, imgW, imgH int) (x, y, w, h float64, ok bool) {
	if mon.w <= 0 || mon.h <= 0 || bounds.w <= 0 || bounds.h <= 0 || imgW <= 0 || imgH <= 0 {
		return 0, 0, 0, 0, false
	}
	if mon.x < bounds.x || mon.y < bounds.y || mon.x+mon.w > bounds.x+bounds.w || mon.y+mon.h > bounds.y+bounds.h {
		return 0, 0, 0, 0, false
	}
	sx := float64(imgW) / float64(bounds.w)
	sy := float64(imgH) / float64(bounds.h)
	return float64(mon.x-bounds.x) * sx, float64(mon.y-bounds.y) * sy, float64(mon.w) * sx, float64(mon.h) * sy, true
}

// glassWork is the size to blur a w x h monitor at and the sigma to blur it
// with there: the monitor's logical size, shrunk while sigma stays at least
// glassWorkSigma per working pixel.
func glassWork(w, h int, sigma float64) (ww, wh int, ws float64) {
	f := min(max(int(sigma/glassWorkSigma), 1), glassMaxShrink)
	return max(w/f, 1), max(h/f, 1), sigma / float64(f)
}

// glassBoxes are the widths of three box blurs whose sum approximates a
// Gaussian of sigma (the usual "boxes for Gauss": the largest odd width
// below the ideal one, the next odd one above for the rest).
func glassBoxes(sigma float64) [3]int {
	const n = 3
	ideal := math.Sqrt(12*sigma*sigma/n + 1)
	wl := int(ideal)
	if wl%2 == 0 {
		wl--
	}
	wu := wl + 2
	m := int(math.Round((12*sigma*sigma - n*float64(wl*wl) - 4*n*float64(wl) - 3*n) / float64(-4*wl-4)))
	var b [3]int
	for i := range b {
		if i < m {
			b[i] = wl
		} else {
			b[i] = wu
		}
	}
	return b
}

// glassBlur blurs a w x h ARGB32 image in place (4 bytes per pixel, rows
// stride bytes apart; padding is left alone) by three box passes along
// each axis. Edges repeat the border pixel, so they do not darken.
func glassBlur(px []byte, w, h, stride int, sigma float64) {
	if sigma <= 0 || w <= 0 || h <= 0 {
		return
	}
	tmp := make([]byte, len(px))
	for _, d := range glassBoxes(sigma) {
		r := d / 2
		if r == 0 {
			continue
		}
		for y := range h {
			glassBoxLine(tmp, px, y*stride, 4, w, r)
		}
		for x := range w {
			glassBoxLine(px, tmp, x*4, stride, h, r)
		}
	}
}

// glassBoxLine writes into dst the mean of the 2r+1 pixels of src around
// each of the n pixels of one line, which starts at base and steps step
// bytes per pixel.
func glassBoxLine(dst, src []byte, base, step, n, r int) {
	d := 2*r + 1
	clamp := func(i int) int { return base + min(max(i, 0), n-1)*step }
	for c := range 4 {
		sum := 0
		for i := -r; i <= r; i++ {
			sum += int(src[clamp(i)+c])
		}
		for i := range n {
			dst[base+i*step+c] = byte((sum + d/2) / d)
			sum += int(src[clamp(i+r+1)+c]) - int(src[clamp(i-r)+c])
		}
	}
}
