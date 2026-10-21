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
// and drives it from the GTK frame clock (once per display refresh); each
// frame calls tick with the widget size and the seconds since the previous
// frame, each repaint calls draw on an already black-filled cairo context.
// UI thread only.
type animation interface {
	tick(w, h, dt float64)
	draw(cr *C.cairo_t)
}

// newAnimation returns the animation for style, or nil for plain black
// (including when an animated style's assets could not be prepared). lang
// is the terminal style's source language.
func newAnimation(style, lang string, log *slog.Logger) animation {
	switch style {
	case "terminal":
		return newTermAnim(lang)
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
func goAnimTick(h C.uintptr_t, w, ht, dt C.double) {
	cgo.Handle(h).Value().(animation).tick(float64(w), float64(ht), float64(dt))
}

//export goAnimDraw
func goAnimDraw(h C.uintptr_t, cr *C.cairo_t) {
	cgo.Handle(h).Value().(animation).draw(cr)
}

//export goAnimFree
func goAnimFree(h C.uintptr_t) {
	handle := cgo.Handle(h)
	if f, ok := handle.Value().(interface{ free() }); ok {
		f.free()
	}
	handle.Delete()
}

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

// --- terminal ---

// cCorpus reads one language of the shared corpus through C.
type cCorpus struct{ lang C.int }

// cInt lets tests, which cannot use cgo, build a cCorpus.
func cInt(i int) C.int { return C.int(i) }

func (c cCorpus) blockCount() int    { return int(C.overlay_term_block_count(c.lang)) }
func (c cCorpus) blockLen(b int) int { return int(C.overlay_term_block_len(c.lang, C.int(b))) }
func (c cCorpus) line(b, i int) string {
	return C.GoString(C.overlay_term_line(c.lang, C.int(b), C.int(i)))
}

var (
	termInitOnce         sync.Once
	termCellW, termLineH float64
)

// termAnim draws a terminalView. Each visible line keeps one Pango layout,
// built when the line enters the screen and freed when it scrolls off; the
// typing line is the same layout clipped to the glyphs typed so far.
type termAnim struct {
	view    *terminalView
	layouts map[uint64]unsafe.Pointer
}

func newTermAnim(lang string) *termAnim {
	termInitOnce.Do(func() {
		var w, h C.double
		C.overlay_term_init(C.double(termFontSize), &w, &h)
		termCellW, termLineH = float64(w), float64(h)
	})
	i := termLangIndex(lang)
	return &termAnim{
		view:    newTerminalView(cCorpus{lang: C.int(i)}, &termSyntaxes[i]),
		layouts: map[uint64]unsafe.Pointer{},
	}
}

func (a *termAnim) tick(w, h, dt float64) {
	a.view.tick(w, h, dt)
	live := make(map[uint64]bool, len(a.view.lines))
	for _, l := range a.view.lines {
		live[l.id] = true
		if _, ok := a.layouts[l.id]; !ok {
			a.layouts[l.id] = termLayout(l)
		}
	}
	for id, p := range a.layouts {
		if !live[id] {
			C.overlay_term_free(p)
			delete(a.layouts, id)
		}
	}
}

// termLayout builds the coloured Pango layout of one line.
func termLayout(l termLine) unsafe.Pointer {
	n := len(l.segs)
	if n == 0 {
		return nil
	}
	text := C.CString(l.text)
	defer C.free(unsafe.Pointer(text))
	starts := make([]C.int, n)
	lens := make([]C.int, n)
	rgb := make([]C.double, 3*n)
	for i, s := range l.segs {
		starts[i], lens[i] = C.int(s.start), C.int(s.length)
		c := termPalette[s.cls]
		rgb[3*i], rgb[3*i+1], rgb[3*i+2] = C.double(c.r), C.double(c.g), C.double(c.b)
	}
	return C.overlay_term_layout(text, &starts[0], &lens[0], &rgb[0], C.int(n))
}

func (a *termAnim) draw(cr *C.cairo_t) {
	v := a.view
	if len(v.lines) == 0 {
		return
	}
	colX, colW := v.column()
	cellH := termCellH()
	bottom := len(v.lines) - 1
	for row, l := range v.lines {
		clipW := colW
		if row == bottom {
			clipW = min(colW, float64(v.typed())*termCellW)
		}
		C.overlay_term_draw(cr, a.layouts[l.id], C.double(colX), C.double(float64(row)*cellH),
			C.double(colX), C.double(clipW))
	}
	if x := colX + float64(v.typed())*termCellW; v.caretOn() && x < colX+colW {
		c := termCaretColor
		C.overlay_fill_rect(cr, C.double(x), C.double(float64(bottom)*cellH), C.double(termCellW), C.double(termLineH),
			C.double(c.r), C.double(c.g), C.double(c.b))
	}
}

// free releases every layout; called when the window goes away.
func (a *termAnim) free() {
	for id, p := range a.layouts {
		C.overlay_term_free(p)
		delete(a.layouts, id)
	}
}
