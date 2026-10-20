//go:build linux

package overlay

/*
#include <stdlib.h>
#include "overlay_linux.h"
*/
import "C"

import (
	"log/slog"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// animation is the Go side of an animated shield window. C owns the widget
// and a ~30 FPS timer; each tick calls tick with the widget size, each
// repaint calls draw on an already black-filled cairo context. UI thread
// only.
type animation interface {
	tick(w, h float64)
	draw(cr *C.cairo_t)
}

// newAnimation returns the animation for style, or nil for plain black
// (including when an animated style's assets could not be prepared).
func newAnimation(style string, log *slog.Logger) animation {
	switch style {
	case "dvd":
		if loadDVDMask(log) {
			return &dvdAnim{dvdView{logoAspect: dvdLogoAspect}}
		}
	case "matrix":
		if loadMatrixAtlas() {
			return &matrixAnim{}
		}
	}
	return nil
}

//export goAnimTick
func goAnimTick(h C.uintptr_t, w, ht C.double) {
	cgo.Handle(h).Value().(animation).tick(float64(w), float64(ht))
}

//export goAnimDraw
func goAnimDraw(h C.uintptr_t, cr *C.cairo_t) {
	cgo.Handle(h).Value().(animation).draw(cr)
}

//export goAnimFree
func goAnimFree(h C.uintptr_t) { cgo.Handle(h).Delete() }

// --- dvd ---

var (
	dvdMaskOnce   sync.Once
	dvdMaskOK     bool
	dvdLogoAspect float64
)

// loadDVDMask hands the decoded logo to C once per process.
func loadDVDMask(log *slog.Logger) bool {
	dvdMaskOnce.Do(func() {
		alpha, w, h, err := dvdLogoAlpha()
		if err != nil {
			log.Warn("dvd logo unusable, drawing a plain black shield", slog.Any("err", err))
			return
		}
		C.overlay_set_dvd_mask((*C.uchar)(unsafe.Pointer(&alpha[0])), C.int(w), C.int(h))
		dvdLogoAspect = float64(w) / float64(h)
		dvdMaskOK = true
	})
	return dvdMaskOK
}

type dvdAnim struct{ dvdView }

func (a *dvdAnim) draw(cr *C.cairo_t) {
	st := &a.st
	c := st.color()
	C.overlay_dvd_draw(cr, C.double(st.x), C.double(st.y), C.double(st.w), C.double(st.h),
		C.double(c.r), C.double(c.g), C.double(c.b))
}

// --- matrix ---

var matrixAtlasOnce sync.Once

// loadMatrixAtlas renders every glyph at every size into the C-side atlas
// once per process.
func loadMatrixAtlas() bool {
	matrixAtlasOnce.Do(func() {
		sizes := make([]C.double, len(matrixSizes))
		for i, s := range matrixSizes {
			sizes[i] = C.double(s)
		}
		glyphs := C.malloc(C.size_t(len(matrixGlyphs)) * C.size_t(unsafe.Sizeof(uintptr(0))))
		defer C.free(glyphs)
		list := unsafe.Slice((**C.char)(glyphs), len(matrixGlyphs))
		for i, g := range matrixGlyphs {
			list[i] = C.CString(g)
		}
		C.overlay_matrix_atlas(&sizes[0], C.int(len(sizes)), (**C.char)(glyphs), C.int(len(matrixGlyphs)))
		for _, p := range list {
			C.free(unsafe.Pointer(p))
		}
	})
	return true
}

type matrixAnim struct{ matrixRain }

func (a *matrixAnim) draw(cr *C.cairo_t) {
	a.lit(func(c matrixCell) {
		C.overlay_matrix_glyph(cr, C.int(c.bucket), C.int(c.glyph), C.double(c.x), C.double(c.y),
			C.double(c.r), C.double(c.g), C.double(c.b))
	})
}
