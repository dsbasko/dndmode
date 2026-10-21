//go:build linux

package overlay

/*
#include <stdlib.h>
#include "overlay_linux.h"
*/
import "C"

import (
	"fmt"
	"log/slog"
	"math"
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
	case "gold":
		loadGoldSprites()
		return &goldAnim{}
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

// --- gold ---

var goldSpritesOnce sync.Once

// loadGoldSprites bakes the coin sprites into C-side images once per process.
func loadGoldSprites() {
	goldSpritesOnce.Do(func() {
		px := goldSprites()
		C.overlay_gold_sprites((*C.uint32_t)(unsafe.Pointer(&px[0])), C.int(goldCoinFrames*goldShades), C.int(goldCoinPx))
	})
}

// goldFonts are the chart's fonts at one layout scale.
type goldFonts struct {
	unit                                   float64
	axis, tag, ticker, name, price, change unsafe.Pointer
}

// goldAnim draws a goldView: the rain first, then the grid, candles, moving
// average and live price over it, then the axis tag and the header.
type goldAnim struct {
	goldView
	fonts   goldFonts
	xys     []C.double // coin batch: top-left and side
	sprites []C.int    // coin batch: sprite index
	pts     []C.double // moving-average polyline
}

// Pango weights.
const (
	goldRegular  = 400
	goldMedium   = 500
	goldSemibold = 600
)

// ensureFonts (re)creates the fonts when the layout scale changes.
func (a *goldAnim) ensureFonts() {
	k := a.unit
	if a.fonts.axis != nil && a.fonts.unit == k {
		return
	}
	a.freeFonts()
	newFont := func(weight int, size float64) unsafe.Pointer {
		family := C.CString("Sans")
		defer C.free(unsafe.Pointer(family))
		return C.overlay_font_new(family, C.int(weight), C.double(size*k))
	}
	a.fonts = goldFonts{
		unit:   k,
		axis:   newFont(goldRegular, 12),
		tag:    newFont(goldSemibold, 12),
		ticker: newFont(goldSemibold, 20),
		name:   newFont(goldRegular, 13),
		price:  newFont(goldSemibold, 34),
		change: newFont(goldMedium, 17),
	}
}

func (a *goldAnim) freeFonts() {
	f := &a.fonts
	for _, p := range []unsafe.Pointer{f.axis, f.tag, f.ticker, f.name, f.price, f.change} {
		C.overlay_font_free(p)
	}
	*f = goldFonts{}
}

// free releases the fonts; called when the window goes away.
func (a *goldAnim) free() { a.freeFonts() }

func goldTextSize(font unsafe.Pointer, s string) (w, h float64) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	var cw, ch C.double
	C.overlay_text_size(font, cs, &cw, &ch)
	return float64(cw), float64(ch)
}

func goldText(cr *C.cairo_t, font unsafe.Pointer, s string, x, y float64, c dvdRGB, halo float64) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	C.overlay_text(cr, font, cs, C.double(x), C.double(y), C.double(c.r), C.double(c.g), C.double(c.b), C.double(halo))
}

func goldLine(cr *C.cairo_t, x0, y0, x1, y1, width float64, c dvdRGB, alpha, dash float64) {
	C.overlay_line(cr, C.double(x0), C.double(y0), C.double(x1), C.double(y1), C.double(width),
		C.double(c.r), C.double(c.g), C.double(c.b), C.double(alpha), C.double(dash))
}

var goldWhite = dvdRGB{1, 1, 1}

// drawRain draws far layers first, so nearer coins pass in front.
// Positions snap to whole pixels and sprites scale by whole numbers.
func (a *goldAnim) drawRain(cr *C.cairo_t) {
	a.xys, a.sprites = a.xys[:0], a.sprites[:0]
	add := func(x, y, side float64, sprite int) {
		a.xys = append(a.xys, C.double(x), C.double(y), C.double(side))
		a.sprites = append(a.sprites, C.int(sprite))
	}
	h := a.rain.h
	for sh := range goldShades {
		for i := range a.rain.d {
			d := &a.rain.d[i]
			if d.shade() != sh {
				continue
			}
			side := d.size()
			x := math.Floor(d.x - side/2)
			y := h - (math.Floor(d.y-side/2) + side) // the model is y up
			f := d.frame()
			add(x, y, side, f*goldShades+sh)
			if d.glint && (f == 0 || f >= goldCoinSpin) {
				add(x, y, side, (goldCoinGlint+int(d.spawns&1))*goldShades+sh)
			}
		}
	}
	if n := len(a.sprites); n > 0 {
		C.overlay_gold_coins(cr, &a.xys[0], &a.sprites[0], C.int(n))
	}
}

func (a *goldAnim) drawCandle(cr *C.cairo_t, c goldCandle, x float64) {
	rgb := goldDown
	if c.c >= c.o {
		rgb = goldUp
	}
	goldLine(cr, x, a.yForPrice(c.l), x, a.yForPrice(c.h), max(1, a.spacing*0.1), rgb, 1, 0)
	top := a.yForPrice(max(c.o, c.c))
	bot := a.yForPrice(min(c.o, c.c))
	if bot-top < 1 {
		mid := (top + bot) / 2
		top, bot = mid-0.5, mid+0.5
	}
	bw := max(1, a.spacing*goldBodyFrac)
	C.overlay_fill_rect(cr, C.double(x-bw/2), C.double(top), C.double(bw), C.double(bot-top),
		C.double(rgb.r), C.double(rgb.g), C.double(rgb.b))
}

func (a *goldAnim) draw(cr *C.cairo_t) {
	if !a.seeded {
		return
	}
	a.ensureFonts()
	// The falling gold goes first, so the chart, axis and header draw over it.
	a.drawRain(cr)
	if !(a.hi > a.lo) {
		return
	}
	s := &a.st
	f := &a.fonts
	k := a.unit
	progress := a.progress()
	n := min(a.visible, s.count)
	top, bottom, right := a.chartY, a.chartY+a.chartH, a.chartX+a.chartW

	// Live-price tag geometry first: the axis labels it would cover are skipped.
	live := goldDown
	if s.cur.c >= s.cur.o {
		live = goldUp
	}
	yLive := a.yForPrice(s.cur.c)
	livePrice := goldFormatPrice(s.cur.c)
	tw, th := goldTextSize(f.tag, livePrice)
	tagH := th + 6
	tagY := min(bottom-tagH/2, max(top+tagH/2, yLive))

	// Horizontal grid on round prices, labeled on the axis to the right.
	step := goldNiceStep(a.hi-a.lo, goldGridLines)
	first := math.Ceil(a.lo/step) * step
	for p := first; p <= a.hi; p += step {
		y := math.Floor(a.yForPrice(p)) + 0.5
		goldLine(cr, a.chartX, y, right, y, 1, goldWhite, 0.07, 0)
		if math.Abs(y-tagY) < tagH {
			continue
		}
		label := goldFormatPrice(p)
		_, lh := goldTextSize(f.axis, label)
		goldText(cr, f.axis, label, right+12, y-lh/2, dvdRGB{0.42, 0.42, 0.42}, 0)
	}

	C.overlay_clip(cr, C.double(a.chartX), C.double(top), C.double(a.chartW), C.double(a.chartH))

	// Vertical grid, pinned to candles so it scrolls with them.
	for age := range n {
		if (s.seq-1-uint64(age))%goldVGridEvery != 0 {
			continue
		}
		x := a.xForAge(age, progress)
		goldLine(cr, x, top, x, bottom, 1, goldWhite, 0.05, 0)
	}

	for age := n - 1; age >= 0; age-- {
		a.drawCandle(cr, s.at(age), a.xForAge(age, progress))
	}
	a.drawCandle(cr, s.cur, a.xForAge(-1, progress))

	// Moving average of closes, oldest visible candle to the forming one,
	// kept as a running sum.
	if m := min(s.count-(goldMAPeriod-1), n); m > 0 {
		sum := 0.0
		for age := m - 1; age < m-1+goldMAPeriod; age++ {
			sum += s.at(age).c
		}
		a.pts = a.pts[:0]
		for age := m - 1; age >= 0; age-- {
			a.pts = append(a.pts, C.double(a.xForAge(age, progress)), C.double(a.yForPrice(sum/goldMAPeriod)))
			if age > 0 {
				sum += s.at(age-1).c - s.at(age+goldMAPeriod-1).c
			}
		}
		// The forming candle's average swaps the oldest close for the live one.
		sum += s.cur.c - s.at(goldMAPeriod-1).c
		a.pts = append(a.pts, C.double(a.xForAge(-1, progress)), C.double(a.yForPrice(sum/goldMAPeriod)))
		C.overlay_polyline(cr, &a.pts[0], C.int(len(a.pts)/2), 1.6,
			C.double(goldMA.r), C.double(goldMA.g), C.double(goldMA.b), 0.85)
	}

	// Dashed line at the live price.
	goldLine(cr, a.chartX, yLive, right, yLive, 1, live, 0.55, 4)

	C.overlay_unclip(cr)

	// Live-price tag on the axis.
	tagX, tagTop := right+4, tagY-tagH/2
	C.overlay_round_rect(cr, C.double(tagX), C.double(tagTop), C.double(tw+16), C.double(tagH), 3,
		C.double(live.r), C.double(live.g), C.double(live.b))
	goldText(cr, f.tag, livePrice, tagX+8, tagTop+3, dvdRGB{}, 0)

	// Header: ticker and name, then the live price and its change since the
	// session's reference price.
	halo := 8 * k
	headTop := top * 0.22
	const ticker = "XAU/USD"
	kw, kh := goldTextSize(f.ticker, ticker)
	goldText(cr, f.ticker, ticker, a.chartX, headTop, goldMA, halo)
	const name = "Gold Spot · US Dollar"
	_, nh := goldTextSize(f.name, name)
	goldText(cr, f.name, name, a.chartX+kw+14, headTop+(kh-nh)/2, dvdRGB{0.45, 0.45, 0.45}, halo)

	change := 0.0
	if s.base > 0 {
		change = (s.cur.c/s.base - 1) * 100
	}
	trend, arrow := goldUp, "▲"
	if change < 0 {
		trend, arrow = goldDown, "▼"
	}
	pw, ph := goldTextSize(f.price, livePrice)
	priceTop := headTop + kh + 2
	goldText(cr, f.price, livePrice, a.chartX, priceTop, dvdRGB{0.94, 0.94, 0.94}, halo)
	chg := fmt.Sprintf("%s %+.2f%%", arrow, change)
	_, ch := goldTextSize(f.change, chg)
	goldText(cr, f.change, chg, a.chartX+pw+16, priceTop+(ph-ch)*0.7, trend, halo)
}
