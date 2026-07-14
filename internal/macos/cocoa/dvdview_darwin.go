//go:build darwin

package cocoa

/*
extern void dvd_step_for_test(double x, double y, double vx, double vy,
                              double w, double h, int colorIdx, int paletteN, double flash,
                              double boundsW, double boundsH,
                              double* outX, double* outY, double* outVX, double* outVY,
                              int* outColorIdx, double* outFlash);
*/
import "C"

// dvdPhys mirrors the mutable fields of the C DVDState struct that dvd_step reads
// and writes: position, velocity, current palette index, and the corner-flash
// intensity. The immutable inputs (logo size, palette count, bounds) are passed
// alongside it into dvdStepForTest.
type dvdPhys struct {
	x, y     float64
	vx, vy   float64
	colorIdx int
	flash    float64
}

// dvdStepForTest advances one DVD-physics frame via the dvd_step_for_test C shim —
// the SAME pure dvd_step DVDView runs every frame. Test-only helper: cgo cannot
// reach the static C dvd_step from a _test.go file, so this thin wrapper lives in
// the production file alongside tokenizeLineForTest. w/h are the logo size,
// paletteN the palette length (for the modulo), boundsW/boundsH the view size.
func dvdStepForTest(in dvdPhys, w, h float64, paletteN int, boundsW, boundsH float64) dvdPhys {
	var outX, outY, outVX, outVY, outFlash C.double
	var outColorIdx C.int
	C.dvd_step_for_test(
		C.double(in.x), C.double(in.y), C.double(in.vx), C.double(in.vy),
		C.double(w), C.double(h), C.int(in.colorIdx), C.int(paletteN), C.double(in.flash),
		C.double(boundsW), C.double(boundsH),
		&outX, &outY, &outVX, &outVY, &outColorIdx, &outFlash,
	)
	return dvdPhys{
		x:        float64(outX),
		y:        float64(outY),
		vx:       float64(outVX),
		vy:       float64(outVY),
		colorIdx: int(outColorIdx),
		flash:    float64(outFlash),
	}
}
