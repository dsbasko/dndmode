//go:build darwin

package cocoa

/*
#include <stdlib.h>

extern int bamboo_layers_for_test(void);
extern int bamboo_fps_for_test(void);
extern int bamboo_max_pieces_for_test(void);
extern int bamboo_max_stalks_for_test(void);
extern int bamboo_max_particles_for_test(void);
extern void* bamboo_new_for_test(unsigned long long seed, double w, double h);
extern void bamboo_step_for_test(void* f, int frames);
extern void bamboo_skip_for_test(void* f, unsigned long long frames);
extern void bamboo_layer_for_test(void* f, int layer, double* outPx, double* outSpeed, double* outBase,
                                  double* outSpacing, double* outLight, double* outFog);
extern int bamboo_stalks_for_test(void* f, int layer, long long* outSlot, double* outX, double* outBase,
                                  int* outSegs, int* outCapped, int maxOut);
extern int bamboo_culm_for_test(void* f, int layer, int index, int* outKind, double* outX, double* outY,
                                double* outH, int* outMirror, int maxOut);
extern void bamboo_panda_for_test(void* f, int* outLayer, double* outX, double* outFoot, double* outReach,
                                  int* outState, int* outDir, int* outArt, unsigned* outVisits);
extern int bamboo_falls_for_test(void* f, double* outX, double* outY, double* outReach, int* outLayer,
                                 int* outFrame, unsigned* outSpawns, int maxOut);
extern int bamboo_flies_for_test(void* f, double* outX, double* outY, double* outReach, double* outGlow,
                                 unsigned* outSpawns, int maxOut);
extern void bamboo_free_for_test(void* f);
extern int bamboo_art_for_test(int frame, char* name, int nn, int* w, int* h, char* buf, int n,
                               char* palette, char* emissive, int pn);
*/
import "C"

import "unsafe"

// bambooLayer mirrors one parallax layer of a BambooForest as the tests see it:
// px is points per art pixel, speed points per frame, base the foot line and
// spacing the culm slot width, both in points.
type bambooLayer struct {
	px, speed, base, spacing, light, fog float64
}

// bambooStalk is one culm in one frame: x its center line.
type bambooStalk struct {
	slot    int64
	x, base float64
	segs    int
	capped  bool
}

// Kinds of bambooPiece, as in bambooview_darwin.m.
const (
	bambooPieceChunk = iota
	bambooPieceSeg
	bambooPieceTop
	bambooPieceLeaf
)

// bambooPiece is one blit of a culm: x, y its bottom-left corner and h its
// height, in view points.
type bambooPiece struct {
	kind    int
	x, y, h float64
	mirror  bool
}

// States of the panda, as in bambooview_darwin.m.
const (
	bambooPandaAway = iota
	bambooPandaWalk
	bambooPandaStand
	bambooPandaSit
)

// bambooPanda is the panda in one frame: x the view x of its center, foot the
// line its feet stand on, art its frame among the panda frames.
type bambooPanda struct {
	layer           int
	x, foot, reach  float64
	state, dir, art int
	visits          uint
}

// bambooParticle is a falling leaf or a firefly: reach is how far past an edge
// its center must be before nothing of it shows.
type bambooParticle struct {
	x, y, reach float64
	layer       int
	frame       int
	glow        float64
	spawns      uint
}

// bambooForest is a handle on a C BambooForest, the walk behind BambooView,
// stepped by the tests. Test-only, like goldRain; free must be called.
type bambooForest struct {
	p unsafe.Pointer
}

// newBambooForestForTest builds the walk for a w x h view.
func newBambooForestForTest(seed uint64, w, h float64) *bambooForest {
	return &bambooForest{p: C.bamboo_new_for_test(C.ulonglong(seed), C.double(w), C.double(h))}
}

// step advances the walk by n frames, as BambooView does on every timer tick.
func (f *bambooForest) step(n int) {
	C.bamboo_step_for_test(f.p, C.int(n))
}

// skip moves the forest n frames on without stepping the particles.
func (f *bambooForest) skip(n uint64) {
	C.bamboo_skip_for_test(f.p, C.ulonglong(n))
}

func (f *bambooForest) layer(i int) bambooLayer {
	var px, speed, base, spacing, light, fog C.double
	C.bamboo_layer_for_test(f.p, C.int(i), &px, &speed, &base, &spacing, &light, &fog)
	return bambooLayer{float64(px), float64(speed), float64(base), float64(spacing), float64(light), float64(fog)}
}

// stalks lists layer i's culms in the current frame, left to right.
func (f *bambooForest) stalks(i int) []bambooStalk {
	capN := int(C.bamboo_max_stalks_for_test())
	slot := make([]C.longlong, capN)
	x := make([]C.double, capN)
	base := make([]C.double, capN)
	segs := make([]C.int, capN)
	capped := make([]C.int, capN)
	n := int(C.bamboo_stalks_for_test(f.p, C.int(i), &slot[0], &x[0], &base[0], &segs[0], &capped[0], C.int(capN)))
	out := make([]bambooStalk, n)
	for k := range out {
		out[k] = bambooStalk{
			slot: int64(slot[k]), x: float64(x[k]), base: float64(base[k]),
			segs: int(segs[k]), capped: capped[k] != 0,
		}
	}
	return out
}

// culm lays out culm k of layer i's list as the view draws it.
func (f *bambooForest) culm(i, k int) []bambooPiece {
	capN := int(C.bamboo_max_pieces_for_test())
	kind := make([]C.int, capN)
	x := make([]C.double, capN)
	y := make([]C.double, capN)
	h := make([]C.double, capN)
	mirror := make([]C.int, capN)
	n := int(C.bamboo_culm_for_test(f.p, C.int(i), C.int(k), &kind[0], &x[0], &y[0], &h[0], &mirror[0], C.int(capN)))
	out := make([]bambooPiece, n)
	for j := range out {
		out[j] = bambooPiece{kind: int(kind[j]), x: float64(x[j]), y: float64(y[j]), h: float64(h[j]), mirror: mirror[j] != 0}
	}
	return out
}

func (f *bambooForest) panda() bambooPanda {
	var layer, state, dir, art C.int
	var x, foot, reach C.double
	var visits C.uint
	C.bamboo_panda_for_test(f.p, &layer, &x, &foot, &reach, &state, &dir, &art, &visits)
	return bambooPanda{
		layer: int(layer), x: float64(x), foot: float64(foot), reach: float64(reach),
		state: int(state), dir: int(dir), art: int(art), visits: uint(visits),
	}
}

// falls reports every falling leaf.
func (f *bambooForest) falls() []bambooParticle {
	capN := int(C.bamboo_max_particles_for_test())
	x := make([]C.double, capN)
	y := make([]C.double, capN)
	reach := make([]C.double, capN)
	layer := make([]C.int, capN)
	frame := make([]C.int, capN)
	spawns := make([]C.uint, capN)
	n := int(C.bamboo_falls_for_test(f.p, &x[0], &y[0], &reach[0], &layer[0], &frame[0], &spawns[0], C.int(capN)))
	out := make([]bambooParticle, n)
	for k := range out {
		out[k] = bambooParticle{
			x: float64(x[k]), y: float64(y[k]), reach: float64(reach[k]),
			layer: int(layer[k]), frame: int(frame[k]), spawns: uint(spawns[k]),
		}
	}
	return out
}

// flies reports every firefly.
func (f *bambooForest) flies() []bambooParticle {
	capN := int(C.bamboo_max_particles_for_test())
	x := make([]C.double, capN)
	y := make([]C.double, capN)
	reach := make([]C.double, capN)
	glow := make([]C.double, capN)
	spawns := make([]C.uint, capN)
	n := int(C.bamboo_flies_for_test(f.p, &x[0], &y[0], &reach[0], &glow[0], &spawns[0], C.int(capN)))
	out := make([]bambooParticle, n)
	for k := range out {
		out[k] = bambooParticle{
			x: float64(x[k]), y: float64(y[k]), reach: float64(reach[k]),
			glow: float64(glow[k]), spawns: uint(spawns[k]),
		}
	}
	return out
}

func (f *bambooForest) free() {
	C.bamboo_free_for_test(f.p)
	f.p = nil
}

// bambooLayerCount is BAMBOO_LAYERS.
func bambooLayerCount() int {
	return int(C.bamboo_layers_for_test())
}

// bambooFPS is the frame rate the walk is stepped at.
func bambooFPS() int {
	return int(C.bamboo_fps_for_test())
}

// bambooSprite is one compiled-in sprite frame: w x h palette characters, row
// 0 at the top.
type bambooSprite struct {
	name string
	w, h int
	px   string
}

// bambooArt returns every compiled-in sprite frame, the palette characters
// ('.' is transparent and not among them) and the emissive ones.
func bambooArt() (frames []bambooSprite, palette, emissive string) {
	const n = 4096
	buf := (*C.char)(C.malloc(n))
	defer C.free(unsafe.Pointer(buf))
	name := (*C.char)(C.malloc(64))
	defer C.free(unsafe.Pointer(name))
	pal := (*C.char)(C.malloc(64))
	defer C.free(unsafe.Pointer(pal))
	emi := (*C.char)(C.malloc(64))
	defer C.free(unsafe.Pointer(emi))
	count := int(C.bamboo_art_for_test(-1, nil, 0, nil, nil, nil, 0, pal, emi, 64))
	for i := 0; i < count; i++ {
		var w, h C.int
		C.bamboo_art_for_test(C.int(i), name, 64, &w, &h, buf, n, nil, nil, 0)
		frames = append(frames, bambooSprite{name: C.GoString(name), w: int(w), h: int(h), px: C.GoString(buf)})
	}
	return frames, C.GoString(pal), C.GoString(emi)
}
