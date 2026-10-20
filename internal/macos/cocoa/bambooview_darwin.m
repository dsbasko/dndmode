// +build darwin

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>  // CALayer (opaque black backing, like MatrixView)
#import <IOSurface/IOSurface.h>    // the frame buffers the layer shows without a copy
#import "bambooview_darwin.h"      // shared @interface BambooView (also imported by window_darwin.m)
#include <math.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// BambooView renders a walk through a bamboo forest at night as the contentView
// of a shield overlay window when config `overlay_style: bamboo` is selected. It
// is a purely cosmetic content swap on top of the opaque shield NSWindow
// (window_darwin.m): the window keeps setOpaque:YES, this view's backing layer is
// opaque black, so the desktop can never bleed through (T-gh8-03). Every blocking
// guarantee (HID event tap, shield level) is byte-for-byte identical to black.
//
// The camera walks to the right, so the forest slides left in BAMBOO_LAYERS
// parallax layers. Nearer layers draw their pixel art bigger (by whole numbers
// only), brighter and less fogged, and slide faster, which is all the depth
// there is. The scene is dim on purpose: the moon is its only light, mist lies
// between the layers, leaves drift down, a few fireflies blink, and far off a
// panda wanders through the culms.
//
// Forest model — pure + unit-tested, and stateless: every culm, ground tile and
// tuft is a hash of (seed, layer, slot), and the scroll is the frame counter
// times the layer speed, so a layer is a pure function of the frame. The forest
// never runs out and never drifts after hours of walking. A culm is rigid: all
// its pieces and leaves are laid out from one x (bamboo_culm), so no part of it
// can fall behind another.
//
// Particles and the panda — pure + unit-tested too: they keep their own state
// and their own random number generator, so they never change the forest. A
// particle that leaves the view respawns outside it, upwind; the panda walks,
// stops and sits to eat on a far layer, wanders off the left edge and some time
// later comes back from the right.
//
// Sprites are text grids compiled into the binary (no image files), baked once
// per layer into CGImages already shaded for that layer's depth and blown up to
// its scale. A culm is drawn as a few tall pre-composed chunks.
//
// Rendering: pixel art gains nothing from Retina, so the frame is painted by hand
// into an IOSurface with one pixel per art pixel of the farthest layer, and the
// surface is the layer's contents (updateLayer); the compositor scales it up
// with nearest-neighbour filtering, with no image copied or converted per frame. Every blit is a 1:1 copy in the frame's own pixel
// format, and the parts that never move (sky, stars, moon, the mist band) are
// painted once per layout. A frame costs a few ms on a Retina laptop, against
// some 60 ms for drawRect: at device resolution.
//
// Cadence: FPS-capped at 15 via NSTimer in NSRunLoopCommonModes on the MAIN run
// loop, so all state + updateLayer is main-thread-safe (same contract as
// DVDView). A slow walk through pixel art needs no more, and the overlay is
// meant to cost as little as it can while the machine is left alone.
//
// Lifecycle — leak-free contract: start the timer in viewDidMoveToWindow
// (window != nil); stop it in viewWillMoveToWindow:nil AND dealloc (guarded
// against double-invalidate); free the model and release the images in dealloc.

#define BAMBOO_FPS 15
static const NSTimeInterval kBambooFrameInterval = 1.0 / BAMBOO_FPS;
static const double kBambooFPS = BAMBOO_FPS; // every speed and period below is per second

typedef struct { CGFloat r, g, b; } BambooRGB;

// --- Pixel-art sprites --------------------------------------------------------
//
// Hand-placed pixel art, one shared palette, '.' transparent. Each frame is H rows
// of W palette characters, row 0 at the top. The culm segments tile vertically
// (each ends in its node ring at the bottom), the ground strips tile
// horizontally, every leaf cluster hangs from a twig at its left edge (the view
// mirrors them for the left side of a culm), and the panda faces right and
// stands on its bottom row (mirrored, it walks left).

enum {
    BAMBOO_ART_STALK   = 0,  // 4 culm segments, 12x24
    BAMBOO_ART_TOP     = 4,  // 2 culm tips, 12x24
    BAMBOO_ART_LEAF    = 6,  // 6 leaf clusters, 24x16
    BAMBOO_ART_GROUND  = 12, // 2 ground strips, 32x12
    BAMBOO_ART_GRASS   = 14, // 3 tufts, 12x8
    BAMBOO_ART_FALL    = 17, // 4 frames of a tumbling leaf, 7x7
    BAMBOO_ART_MOON    = 21, // 24x24
    BAMBOO_ART_FLY     = 22, // 2 firefly glows, 5x5
    BAMBOO_ART_PANDA   = 24, // 4 walk frames, standing, 2 sitting and chewing; 30x18
    BAMBOO_ART_COUNT   = 31,
};

#define BAMBOO_STALKS  4
#define BAMBOO_TOPS    2
#define BAMBOO_LEAVES  6
#define BAMBOO_GROUNDS 2
#define BAMBOO_GRASSES 3
#define BAMBOO_FALLS   4
#define BAMBOO_FLIES   2
#define BAMBOO_PANDAS  7

// The panda's frames, in BAMBOO_ART_PANDA order.
enum { BAMBOO_PANDA_WALK_ART = 0, BAMBOO_PANDA_STAND_ART = 4, BAMBOO_PANDA_SIT_ART = 5 };

#define BAMBOO_CELL_W  12 // culm segment width, art px; the culm is columns 3..8
#define BAMBOO_SEG_H   24 // culm segment height, art px
#define BAMBOO_LEAF_W  24
#define BAMBOO_LEAF_H  16
#define BAMBOO_GROUND_W 32
#define BAMBOO_GROUND_H 12
#define BAMBOO_PANDA_W 30
#define BAMBOO_PANDA_H 18

typedef struct {
    const char *name;
    int         w, h;
    const char *px; // h rows of w palette characters, row 0 at the top
} BambooArt;

// @@BAMBOO_ART_BEGIN@@ generated from the sprite text; do not edit by hand
static const char kBambooPalette[] = "odmlhnbkstucefgpqxr";
static const BambooRGB kBambooColors[] = {
    { 0.027, 0.055, 0.071 }, // o night outline
    { 0.090, 0.149, 0.173 }, // d bamboo shadow
    { 0.165, 0.231, 0.239 }, // m bamboo body
    { 0.251, 0.325, 0.310 }, // l moonlit bamboo rim
    { 0.384, 0.443, 0.431 }, // h rare moonlit glint
    { 0.192, 0.263, 0.259 }, // n bamboo node
    { 0.078, 0.118, 0.137 }, // b soil
    { 0.024, 0.043, 0.059 }, // k deep soil
    { 0.082, 0.118, 0.149 }, // s stone shadow
    { 0.157, 0.208, 0.239 }, // t stone body
    { 0.243, 0.298, 0.325 }, // u stone moonlit edge
    { 0.663, 0.710, 0.749 }, // c moon silver
    { 0.463, 0.533, 0.576 }, // e moon shade
    { 0.455, 0.518, 0.357 }, // f dim firefly core
    { 0.133, 0.184, 0.129 }, // g faint firefly halo
    { 0.471, 0.522, 0.549 }, // p moonlit white fur
    { 0.255, 0.294, 0.325 }, // q white fur in shadow
    { 0.067, 0.106, 0.133 }, // x black fur
    { 0.149, 0.192, 0.227 }, // r moonlit black fur rim
};

static const BambooArt kBambooArt[BAMBOO_ART_COUNT] = {
    { "stalk0", 12, 24,
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...ohmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "..onnnnnno.."
      "...onnnno..."
      "...olmmdo..." },
    { "stalk1", 12, 24,
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...ohmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo.lm"
      "...olmmdo.d."
      "..onnnnnno.."
      "...onnnno..."
      "...olmmdo..." },
    { "stalk2", 12, 24,
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...ohmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "lm.olmmdo..."
      ".d.olmmdo..."
      "..onnnnnno.."
      "...onnnno..."
      "...olmmdo..." },
    { "stalk3", 12, 24,
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmddo..."
      "...olmddo..."
      "...olmddo..."
      "...olmddo..."
      "...olmmdo..."
      "...olmddo..."
      "...olmddo..."
      "...olmmdo..."
      "...olmmdo..."
      "...ohmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "..onnnnnno.."
      "...onnnno..."
      "...olmmdo..." },
    { "top0", 12, 24,
      "............"
      "............"
      ".....lo....."
      ".....lo....."
      ".....lo....."
      ".....lo....."
      ".....lo....."
      "....oldo...."
      "....olmo...."
      "....olmo...."
      "....olmo...."
      "...olmmdo..."
      "...ohmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "..onnnnnno.."
      "...onnnno..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..." },
    { "top1", 12, 24,
      "............"
      "............"
      ".....lo....."
      ".l...lo...l."
      ".lm..lo..lm."
      "..lm.lo.lm.."
      "...lmlo.m..."
      "....oldo...."
      "....olmoml.."
      "....olmo.ml."
      "....olmo..d."
      "...olmmdo..."
      "...olmmdo..."
      "...ohmmdo..."
      "...olmmdo..."
      "..onnnnnno.."
      "...onnnno..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..."
      "...olmmdo..." },
    { "leaf0", 24, 16,
      "........................"
      "........................"
      "........................"
      "........................"
      "mmmm...................."
      "....mmmmm..............."
      "....d....mmmmm.........."
      "....lm....d...mmmlhl...."
      ".....lm....lm.....mmll.."
      ".....lm.....lm.......ml."
      "......lm.....lm.......ml"
      "......lm......lm.......d"
      ".......m.......lm......."
      ".......d........m......."
      "........d.......d......."
      ".................d......" },
    { "leaf1", 24, 16,
      "........................"
      "........................"
      "........................"
      "mmmmm..................."
      "..d..mmmmmm............."
      "...lm..d...mmmmm........"
      "....lm..lm..d...mmmlll.."
      "....lm...lm..lm..d..mml."
      ".....m....lm..lm..lm...d"
      ".....d.....m..lm...lm..."
      ".....d......d..lm...lm.."
      ".............d.lm....m.."
      "................m.....d."
      "................d......."
      "................d......."
      "........................" },
    { "leaf2", 24, 16,
      "........................"
      "........................"
      "mmm....................."
      "...mmm.................."
      "...d..mmm..............."
      "....lm...mmm............"
      "....lm...d..mmm........."
      ".....lm...lm...mmmll...."
      ".....lm...lm......mml..."
      ".....lm....lm.......ml.."
      "......m....lm........lm."
      "......d.....lm.......lm."
      "......d.....m.........m."
      "............d.........d."
      "............d.........d."
      "........................" },
    { "leaf3", 24, 16,
      "........................"
      "........................"
      "........................"
      "........................"
      "mmmmmm.................."
      "..d...mmmmm............."
      "...lm..d...mmmmmm......."
      "....lm.lm...d....mmmml.."
      ".....m..lm..lm...lm..ml."
      ".....d..lm...lm...lm..md"
      ".........m....lm...lm..."
      ".........d....lm....lm.."
      "...............m.....m.."
      "................d.....d."
      "........................"
      "........................" },
    { "leaf4", 24, 16,
      "........................"
      "........................"
      "mm......................"
      "..mm...................."
      "...dmmm................."
      "...lm..mm..............."
      "...lm..d.mmm............"
      "....lm.lm...mmm........."
      "....lm..lm..d..mm......."
      "....m...lm..lm...mmlll.."
      "....d....lm..lm.....mml."
      ".........m....lm......ml"
      ".........d.....m.......d"
      ".........d......d......."
      "........................"
      "........................" },
    { "leaf5", 24, 16,
      "........................"
      "........................"
      "........................"
      "........................"
      "..mmmmmmmm.............."
      "mm.d..d...mmmm.........."
      "...lm..lm.d...mmmm......"
      "....lm..lmlm..d...mmm..."
      "....m....m.lm.lm..d..l.."
      ".....d....dlm..lm.lm.ml."
      "............m...m..lm.ml"
      "............d....d..lm.d"
      "............d.......m..."
      ".....................d.."
      "........................"
      "........................" },
    { "ground0", 32, 12,
      "................................"
      ".........l......................"
      "..........m..............l......"
      "..l.......dm............m......."
      "...m...d...d..........dm...d...."
      "...dm..md.bk...b...d...d...md..."
      "kkbbkkbbkbkkbbkkkbbkkbbkkkbbkkbk"
      "kkbbkkkbkkbkkbbkkkbbkkbkbbkkbbkk"
      "kkkbbbkkkkbbkkkkbbbkkkkbbkkkkbkk"
      "kkkkkkbkkkkkkbbkkkkkkbkkkkkkkkkk"
      "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
      "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk" },
    { "ground1", 32, 12,
      "................................"
      "......................l........."
      ".....l...............m.........."
      "......m.........l...dm.........."
      "..d...dm....d....m...d.....l...."
      "..md...d..bkmd...dm.bk....dm...."
      "kkbkkkbbkkbbkkkbbkkbkkbbkkbbkkbk"
      "kkbkkbbkkbkbbkkkbbkkbbkkkkbbkkkk"
      "kkbbkkkkbbbkkkkkkbkkkkkbbkkkkbkk"
      "kkkkkkbbkkkkkkbkkkkkkbbkkkkkkkkk"
      "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
      "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk" },
    { "grass0", 12, 8,
      "............"
      "..l........."
      "...m....l..."
      "...lm..m...."
      ".l..m.dm..l."
      "..m.dmmd.m.."
      "...mdmmdm..."
      "....dmmdo..." },
    { "grass1", 12, 8,
      ".....l......"
      ".....lm....."
      ".l....m...l."
      "..lm..m..lm."
      "...lm.mdmm.."
      "l...m.mdm..."
      ".m...mmd...."
      "..ddmmmd...." },
    { "grass2", 12, 8,
      "............"
      "............"
      "...l....l..."
      "....m..lm..."
      ".l..lm.m...."
      "..lm.mdm..l."
      "....mmdmdd.."
      ".....mmd...." },
    { "fall0", 7, 7,
      "......."
      "......."
      "......."
      ".llllm."
      "..mmmmd"
      "......."
      "......." },
    { "fall1", 7, 7,
      "......."
      "..l...."
      "..lm..."
      "...lm.."
      "....lm."
      ".....d."
      "......." },
    { "fall2", 7, 7,
      "...l..."
      "...l..."
      "...m..."
      "...m..."
      "...d..."
      "...d..."
      "......." },
    { "fall3", 7, 7,
      "......."
      ".....l."
      "....lm."
      "...lm.."
      "..lm..."
      "..d...."
      "......." },
    { "moon", 24, 24,
      ".........cccccc........."
      "......cccccccccccc......"
      ".....ccccccccccccce....."
      "....ccccccccccccccce...."
      "...ccccccccccccccccce..."
      "..ccccccccccccccccccce.."
      ".ccccccccccccccccccccce."
      ".ccccceeeccccccccccccce."
      ".cccceeeeccccccccccccce."
      "ccccceeeccccccccccccccce"
      "ccccccecccccccccccccccce"
      "ccccccccccccccceeeccccce"
      "cccccccccccccceeeeccccce"
      "cccccccccccccceeecccccce"
      "ecccccceccccccceccccccce"
      ".ccccceecccccccccccccce."
      ".ccccceeeccccccccccccce."
      ".ecccccecccccccccccccce."
      "..ecccccccccccccccccce.."
      "...ecccccccccccccccce..."
      "....ecccccccccccccce...."
      ".....ecccccccccccce....."
      "......eeecccccceee......"
      ".........eeeeee........." },
    { "firefly0", 5, 5,
      "....."
      "..g.."
      ".gfg."
      "..g.."
      "....." },
    { "firefly1", 5, 5,
      "....."
      "....."
      "..f.."
      "....."
      "....." },
    { "pwalk0", 30, 18,
      ".............................."
      ".............................."
      "....................oo....o..."
      ".........ooooooo...orrooooro.."
      "......ooopppppppoooxxxppprxxo."
      ".....oppppppppppprqoqppppqxxo."
      "....opppppppppppprxxqpppxpqo.."
      "...oppppppppppppxrxqppprxxqo.."
      "...oppppppppppppxrxqqppxxxpo.."
      "..oqpppppppppppprxxxqppxxxxxo."
      "..opqqqppppppppprrxxqqqqpqxqo."
      "..oqqrxxxqqqqqqqrrxxxqqqqxqo.."
      "...oorxxqqqqqqqqxxrxxxqqqqo..."
      "...oxrxxqqqqqqqqqxrxxxxooo...."
      "...oxrxxoooxooooooxrxxo......."
      "..oxrxxo..orrxxo.oxrxxxo......"
      "..oxrrrxo..oooo...oxrrrxo....."
      "...ooooo...........ooooo......" },
    { "pwalk1", 30, 18,
      ".............................."
      ".............................."
      "....................oo....o..."
      ".........ooooooo...orrooooro.."
      "......ooopppppppoooxxxppprxxo."
      ".....oppppppppppprqoqppppqxxo."
      "....opppppppppppprxxqpppxpqo.."
      "...oppppppppppppxrxqppprxxqo.."
      "...oppppppppppppxrxqqppxxxpo.."
      "..oqpppppppppppprxxxqppxxxxxo."
      "..opqqqppppppppprrxxqqqqpqxqo."
      "..oqqrxxxqqqqqqqrrxxxqqqqxqo.."
      "...ooxrxxqqqqqqqxxrxxxqqqqo..."
      "....oxrxxqqqqqqqqxrxxxxooo...."
      "....oxrxxoxooooooxrxxoxooo...."
      "....oxrxxorrxxo.oxrxxorrxxo..."
      "....oxrrrxoooo..oxrrrxoooo...."
      ".....ooooo.......ooooo........" },
    { "pwalk2", 30, 18,
      ".............................."
      ".............................."
      "....................oo....o..."
      ".........ooooooo...orrooooro.."
      "......ooopppppppoooxxxppprxxo."
      ".....oppppppppppprqoqppppqxxo."
      "....opppppppppppprxxqpppxpqo.."
      "...oppppppppppppxrxqppprxxqo.."
      "...oppppppppppppxrxqqppxxxpo.."
      "..oqpppppppppppprxxxqppxxxxxo."
      "..opqqqppppppppprrxxqqqqpqxqo."
      "..oqqrxxxqqqqqqqrrxxxqqqqxqo.."
      "...ooorxxqqqqqqqxxrxxxqqqqo..."
      ".....oxrxxqqqqqqqxrxxxxooo...."
      "......oxrxxoooooxrxxooxxo....."
      "......oxrrrxo.oxrrrxooxxooo..."
      ".......orrxxo..ooooo..orrxxo.."
      "........oooo...........oooo..." },
    { "pwalk3", 30, 18,
      ".............................."
      ".............................."
      "....................oo....o..."
      ".........ooooooo...orrooooro.."
      "......ooopppppppoooxxxppprxxo."
      ".....oppppppppppprqoqppppqxxo."
      "....opppppppppppprxxqpppxpqo.."
      "...oppppppppppppxrxqppprxxqo.."
      "...oppppppppppppxrxqqppxxxpo.."
      "..oqpppppppppppprxxxqppxxxxxo."
      "..opqqqppppppppprrxxqqqqpqxqo."
      "..oqqrxxxqqqqqqqrrxxxqqqqxqo.."
      "...ooxrxxqqqqqqqxxrxxxqqqqo..."
      "....oxrxxqqqqqqqqxrxxxxooo...."
      "....oxrxxoxxoooooxrxxxxo......"
      "....oxrrrxxxoo..oxrrrxxxoo...."
      ".....ooooorrxxo..ooooorrxxo..."
      "..........oooo........oooo...." },
    { "pstand", 30, 18,
      ".............................."
      ".............................."
      "...................ooo....o..."
      ".........ooooooo..orrxooooro.."
      "......ooopppppppoooxxpppprxxo."
      ".....oppppppppppprqxqppppqxxo."
      "....opppppppppppprxxpxppppqo.."
      "...oppppppppppppxrxqrxxpprxo.."
      "...oppppppppppppxrxpxxxppxxqo."
      "..oqpppppppppppprxxqxxxppxxqo."
      "..opqqqppppppppprrxqqpqxxxqqo."
      "..oqqrxxxqqqqqqqrrxxqqqqxqqo.."
      "...ooxrxxqqqqqqqxxrxxqqqqqo..."
      "....oxrxxqqqqqqqqxrxxoxxoo...."
      "....oxrxxoxxoooooxrxxooxxo...."
      "....oxrxxooxxo..oxrxxooxxoo..."
      "....oxrrrxorrxo.oxrrrxoorrxo.."
      ".....ooooo.ooo...ooooo..ooo..." },
    { "psit0", 30, 18,
      ".............................."
      "..............ooo....o........"
      ".............orrxooooro......."
      ".............oxxpppprxxo......"
      ".............oqppppppxxo......"
      ".............oqpppprxqo......."
      "............oqpppppxxxqoooo..."
      "............orqqppxxxqqxxmlo.."
      "..........ooxrqqqpppxqqqqlmo.."
      ".........oppxxrxqqqqqxxqmlo..."
      ".......ooppppxxrxqqqqrxxxo...."
      "......oqqpppppxxrrxxxxxolo...."
      ".....oqrpppppppxxxrrrrrnmo...."
      "....orrxxxxpppppqqxxxxxxo....."
      "....orxxxxxppppqqrxxqoomo....."
      "...oxrxxxxxxqqqqqrxxxooo......"
      "....oxrxxxxxqqqqxxrrxxxo......"
      ".....oooooooooooooooooo......." },
    { "psit1", 30, 18,
      ".............................."
      "..............ooo....o........"
      ".............orrxooooro......."
      ".............oxxpppprxxo......"
      ".............oqppppppxxo......"
      ".............oqpppprxqo......."
      "............oqpppppxxxqooo...."
      "............orqqppxxxqqxmlo..."
      "..........ooxrqqqpppxqqqlmo..."
      ".........oppxxrxqqqqqxxxlo...."
      ".......ooppppxxrxqqqqrqxo....."
      "......oqqpppppxxrrxxxxxlo....."
      ".....oqrpppppppxxxrrrrnmo....."
      "....orrxxxxpppppqqxxxxxo......"
      "....orxxxxxppppqqrxxqomo......"
      "...oxrxxxxxxqqqqqrxxxoo......."
      "....oxrxxxxxqqqqxxrrxxxo......"
      ".....oooooooooooooooooo......." },
};

// Characters that are light rather than matter (moon, firefly): depth
// fog dims them less than the forest.
static const char kBambooEmissive[] = "cefg";
// @@BAMBOO_ART_END@@

// --- Pure forest model: Cocoa-free + unit-tested ------------------------------

#define BAMBOO_LAYERS       5
#define BAMBOO_CHUNKS       6   // pre-composed chunk variants per layer
#define BAMBOO_MAX_STALKS   320 // visible culms per layer, ever (a 6K display at the densest layer)
#define BAMBOO_MAX_FALLS    160
#define BAMBOO_MAX_FLIES    48

// One parallax layer, far to near. scale is art px per view point over the
// display's base pixel; speed is view points per second at a base pixel of 1;
// spacing is art px between culm slots; base is the foot line as a fraction of
// the view height; light and fog shade the sprites; chunk is how many segments a
// pre-composed chunk holds (the far layers have more, shorter-looking segments);
// density is the share of slots that hold a culm.
typedef struct {
    int    scale;
    double speed;
    double spacing;
    double base;
    double light, fog;
    int    chunk;
    double density;
} BambooLayer;

static const BambooLayer kBambooLayers[BAMBOO_LAYERS] = {
    //  scale speed spacing  base   light  fog  chunk density
    {   1,     4.0,  19.0,   0.30,  0.55, 0.80,  8,   0.75 },
    {   1,     7.0,  27.0,   0.24,  0.70, 0.58,  8,   0.70 },
    {   2,    12.0,  26.0,   0.16,  0.85, 0.32,  4,   0.70 },
    {   3,    20.0,  27.0,   0.08,  1.00, 0.08,  2,   0.70 },
    {   5,    34.0,  90.0,  -0.04,  0.30, 0.00,  1,   0.35 },
};

#define BAMBOO_GROUND_LAYERS 4 // the foreground layer stands below the view and has no floor

static const double kBambooCappedShare  = 0.40; // culms that end in a tip inside the view
static const double kBambooLeafChance   = 0.50; // chance a node in the leafy part grows a cluster
static const double kBambooLeafyFrom    = 0.30; // leaves grow above this share of the visible culm
static const double kBambooTuftChance   = 0.45; // ground tiles with a tuft
static const double kBambooStepPeriod   = 1.1;  // seconds per walking step (camera bob)
static const double kBambooBob[BAMBOO_LAYERS] = { 0.0, 0.0, 0.5, 1.0, 2.0 }; // bob, view points

// A view bigger than this (pt^2) gets 2-pt art pixels: the forest keeps its look
// on a large display, and the frame (one pixel per art pixel) stays about the
// size of a laptop's.
static const double kBambooBigView = 2.4e6;

static const double kBambooAreaPerFall = 26000.0;  // pt^2 of view per falling leaf
static const double kBambooAreaPerFly  = 120000.0; // pt^2 of view per firefly
static const double kBambooFlyLow  = 0.06; // firefly band, fractions of the view height
static const double kBambooFlyHigh = 0.52;

// The panda lives on one far layer. Its pace is in art px per second on top of
// the scroll; it is slower than the walk, so on screen it always drifts left,
// fast when it walks left, barely when it walks along with the camera. Every
// stay in a state lasts between the two bounds, in seconds.
static const int    kBambooPandaLayer  = 2;
static const double kBambooPandaLight  = 1.0;  // its shading: brighter than its layer, or the mist would swallow it
static const double kBambooPandaFog    = 0.18;
static const double kBambooPandaSink   = 3.0;  // art px its feet stand below the foot line, in the grass
static const double kBambooPandaPace   = 4.0;  // art px per second
static const double kBambooPandaStride = 1.5;  // art px walked per walk frame
static const double kBambooPandaChew   = 0.45; // seconds per chewing frame
static const double kBambooPandaWalk[2]  = { 5.0, 14.0 };
static const double kBambooPandaStand[2] = { 2.0, 5.0 };
static const double kBambooPandaSit[2]   = { 8.0, 20.0 };
static const double kBambooPandaAway[2]  = { 20.0, 60.0 };

typedef struct {
    double   x, y;           // center
    double   vx, vy;         // drift, pt per frame (vx includes the walk)
    double   phase, rate;    // side-to-side swing
    double   swing;          // its amplitude, pt per frame
    double   spin, spinRate; // tumble, radians
    int      layer;          // depth it falls at (sets size and shade)
    unsigned spawns;         // respawns since init
} BambooFall;

typedef struct {
    double   x, y;
    double   vx, vy;         // wander, pt per frame (the walk is added on top)
    double   phase, rate;    // blink
    int      layer;
    unsigned spawns;
} BambooFly;

enum { BAMBOO_PANDA_AWAY, BAMBOO_PANDA_WALK, BAMBOO_PANDA_STAND, BAMBOO_PANDA_SIT };

typedef struct {
    int      state;
    int      dir;     // +1 faces and walks right, -1 left
    double   wx;      // center, in its layer's scrolled coordinates (view x + the layer's offset)
    int      left;    // frames left in this state
    double   walked;  // art px walked since it came, for the gait
    int      held;    // frames in this state so far, for the chewing
    unsigned visits;  // times it has come back since init
} BambooPanda;

typedef struct {
    uint64_t    seed;          // the forest: every culm and tile hashes it
    uint64_t    frame;         // frames walked
    double      w, h;          // view size, points
    int         pixel;         // view points per art px on the farthest layer
    BambooFall  fall[BAMBOO_MAX_FALLS];
    int         falls;
    BambooFly   fly[BAMBOO_MAX_FLIES];
    int         flies;
    BambooPanda panda;
    uint64_t    rng;           // particles' and the panda's xorshift64* state, never 0
} BambooForest;

// A culm as one frame sees it. x is its center line and base its foot, both in
// view points; segs counts its segments foot to tip; capped culms end in a tip
// sprite on top of the last segment, the rest run out of the top of the view.
typedef struct {
    int64_t  slot;
    double   x, base;
    int      segs;
    int      capped;
    uint64_t hash;
} BambooStalk;

// splitmix64 finalizer: decorrelates neighbouring slots completely.
static uint64_t bamboo_mix(uint64_t x) {
    x += 0x9E3779B97F4A7C15ULL;
    x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9ULL;
    x = (x ^ (x >> 27)) * 0x94D049BB133111EBULL;
    return x ^ (x >> 31);
}

static uint64_t bamboo_hash(uint64_t seed, int layer, int64_t slot, int salt) {
    return bamboo_mix(seed ^ bamboo_mix(((uint64_t)layer << 56) ^ ((uint64_t)salt << 40) ^ (uint64_t)slot));
}

// bamboo_frac maps a hash to [0,1).
static double bamboo_frac(uint64_t h) {
    return (double)(h >> 11) * (1.0 / 9007199254740992.0);
}

// xorshift64* for the particles, as in GoldView.
static uint64_t bamboo_rand(uint64_t *state) {
    uint64_t x = *state;
    x ^= x >> 12;
    x ^= x << 25;
    x ^= x >> 27;
    *state = x;
    return x * 0x2545F4914F6CDD1DULL;
}

static double bamboo_uniform(uint64_t *state) {
    return ((double)(bamboo_rand(state) >> 11) + 0.5) * (1.0 / 9007199254740992.0);
}

// Points per art px on a layer.
static double bamboo_px(const BambooForest *f, int layer) {
    return (double)(f->pixel * kBambooLayers[layer].scale);
}

static double bamboo_spacing(const BambooForest *f, int layer) {
    return kBambooLayers[layer].spacing * bamboo_px(f, layer);
}

// How far a layer has scrolled, in view points. It is the frame count times the
// speed, never a running sum, so it is exact after any number of frames.
static double bamboo_offset(const BambooForest *f, int layer) {
    return (double)f->frame * kBambooLayers[layer].speed * f->pixel / kBambooFPS;
}

static double bamboo_speed(const BambooForest *f, int layer) {
    return kBambooLayers[layer].speed * f->pixel / kBambooFPS;
}

// Camera bob of a layer, whole view points: the nearer, the more it bobs.
static double bamboo_bob(const BambooForest *f, int layer) {
    double t = (double)f->frame / kBambooFPS;
    return round(kBambooBob[layer] * f->pixel * sin(2.0 * M_PI * t / kBambooStepPeriod));
}

static double bamboo_base(const BambooForest *f, int layer) {
    return round(f->h * kBambooLayers[layer].base) + bamboo_bob(f, layer);
}

// bamboo_stalks lists the culms of a layer that can show in this frame, left to
// right, and returns how many. A culm reaches out by its leaves, so slots are
// listed a leaf's span past each edge.
static int bamboo_stalks(const BambooForest *f, int layer, BambooStalk *out, int maxOut) {
    const BambooLayer *L = &kBambooLayers[layer];
    double p = bamboo_px(f, layer);
    double sp = bamboo_spacing(f, layer);
    double off = bamboo_offset(f, layer);
    double margin = (BAMBOO_LEAF_W + BAMBOO_CELL_W) * p;
    double base = bamboo_base(f, layer);
    double segH = BAMBOO_SEG_H * p;
    int needed = (int)ceil((f->h - base) / segH) + 1;
    int64_t first = (int64_t)floor((off - margin) / sp) - 1;
    int64_t last = (int64_t)floor((off + f->w + margin) / sp) + 1;
    int n = 0;
    for (int64_t k = first; k <= last && n < maxOut; k++) {
        uint64_t h = bamboo_hash(f->seed, layer, k, 0);
        if (bamboo_frac(h) >= L->density) continue;
        double jitter = (bamboo_frac(bamboo_mix(h ^ 1)) - 0.5) * 0.7;
        double x = ((double)k + 0.5 + jitter) * sp - off;
        if (x < -margin || x > f->w + margin) continue;
        BambooStalk *s = &out[n++];
        s->slot = k;
        s->x = x;
        s->base = base;
        s->hash = h;
        s->capped = layer < BAMBOO_LAYERS - 1 && bamboo_frac(bamboo_mix(h ^ 2)) < kBambooCappedShare;
        s->segs = needed;
        if (s->capped) {
            int segs = (int)lround(needed * (0.35 + 0.5 * bamboo_frac(bamboo_mix(h ^ 3))));
            s->segs = segs < 2 ? 2 : segs;
        }
    }
    return n;
}

// A culm as it is drawn: a stack of pieces, then the leaf clusters on its
// nodes. x, y is a piece's bottom-left corner and h its height, in view points.
enum { BAMBOO_PIECE_CHUNK, BAMBOO_PIECE_SEG, BAMBOO_PIECE_TOP, BAMBOO_PIECE_LEAF };

typedef struct {
    int    kind;
    int    variant; // which chunk, segment, tip or leaf cluster
    int    mirror;  // a leaf cluster that hangs left
    double x, y, h;
} BambooPiece;

#define BAMBOO_MAX_PIECES 160

// bamboo_culm lays out one culm, foot to top: its chunks, single segments for
// what is left of a capped culm, the tip, then its leaves; it returns how many
// pieces there are. Every piece takes its x from the culm's one left edge, so
// the culm is rigid and no part of it can fall behind another.
static int bamboo_culm(const BambooForest *f, int layer, const BambooStalk *s, BambooPiece *out, int maxOut) {
    const BambooLayer *L = &kBambooLayers[layer];
    double p = bamboo_px(f, layer);
    double segH = BAMBOO_SEG_H * p;
    double left = s->x - 6.0 * p;
    double H = f->h;
    int n = 0;

    int seg = 0;
    double y = s->base;
    while (seg + L->chunk <= s->segs && y < H && n < maxOut) {
        int v = (int)(bamboo_mix(s->hash ^ (uint64_t)(seg + 16)) % BAMBOO_CHUNKS);
        out[n++] = (BambooPiece){ BAMBOO_PIECE_CHUNK, v, 0, left, y, segH * L->chunk };
        seg += L->chunk;
        y += segH * L->chunk;
    }
    while (seg < s->segs && y < H && n < maxOut) {
        int v = (int)(bamboo_mix(s->hash ^ (uint64_t)(seg + 16)) % BAMBOO_STALKS);
        out[n++] = (BambooPiece){ BAMBOO_PIECE_SEG, v, 0, left, y, segH };
        seg++;
        y += segH;
    }
    if (s->capped && y < H && n < maxOut) {
        int v = (int)(bamboo_mix(s->hash ^ 5) % BAMBOO_TOPS);
        out[n++] = (BambooPiece){ BAMBOO_PIECE_TOP, v, 0, left, y, segH };
    }

    // Leaves hang from the nodes of the upper part of the culm. A node ring is
    // near the bottom of its segment, level with the twig on row 4 of a
    // cluster, so the cluster hangs below the node.
    double leafy = s->base + (fmin(H, s->base + s->segs * segH) - s->base) * kBambooLeafyFrom;
    for (int k = 1; k < s->segs && n < maxOut; k++) {
        double yn = s->base + k * segH;
        if (yn < leafy) continue;
        if (yn > H + BAMBOO_LEAF_H * p) break;
        uint64_t h = bamboo_mix(s->hash ^ (uint64_t)(k * 977 + 31));
        if (bamboo_frac(h) >= kBambooLeafChance) continue;
        int side = (int)((h >> 16) & 1);
        double lx = side ? left + 2.0 * p - BAMBOO_LEAF_W * p : left + 9.0 * p;
        out[n++] = (BambooPiece){ BAMBOO_PIECE_LEAF, (int)((h >> 8) % BAMBOO_LEAVES), side,
                                  lx, yn - 10.0 * p, BAMBOO_LEAF_H * p };
    }
    return n;
}

// Particles. A falling leaf drifts with the walk (the nearer, the faster) plus
// a little wind, swings side to side and tumbles; it respawns upwind — above the
// top edge or past the right one, out of sight — in proportion to how much of
// the flow enters through each.

static double bamboo_fall_reach(const BambooForest *f, const BambooFall *l) {
    return 7.0 * bamboo_px(f, l->layer);
}

static void bamboo_fall_spawn(BambooForest *f, BambooFall *l) {
    double u = bamboo_uniform(&f->rng);
    l->layer = u < 0.45 ? 2 : u < 0.85 ? 3 : 4;
    double p = bamboo_px(f, l->layer);
    l->vx = -bamboo_speed(f, l->layer) - (1.5 + 4.5 * bamboo_uniform(&f->rng)) * p / kBambooFPS;
    l->vy = -(3.75 + 5.25 * bamboo_uniform(&f->rng)) * p / kBambooFPS;
    l->phase = bamboo_uniform(&f->rng) * 2.0 * M_PI;
    l->rate = 2.0 * M_PI / (kBambooFPS * (1.5 + 2.0 * bamboo_uniform(&f->rng)));
    l->swing = (3.0 + 6.0 * bamboo_uniform(&f->rng)) * p / kBambooFPS;
    l->spin = bamboo_uniform(&f->rng) * 2.0 * M_PI;
    double dir = bamboo_uniform(&f->rng) < 0.5 ? -1.0 : 1.0;
    l->spinRate = dir * 2.0 * M_PI / (kBambooFPS * (0.8 + 1.2 * bamboo_uniform(&f->rng)));
    double reach = bamboo_fall_reach(f, l);
    double top = f->w * fabs(l->vy), right = f->h * fabs(l->vx);
    if (bamboo_uniform(&f->rng) * (top + right) < top) {
        l->x = bamboo_uniform(&f->rng) * f->w;
        l->y = f->h + reach + bamboo_uniform(&f->rng) * f->h * 0.2;
    } else {
        l->x = f->w + reach + bamboo_uniform(&f->rng) * f->w * 0.1;
        l->y = bamboo_uniform(&f->rng) * f->h;
    }
    l->spawns++;
}

static int bamboo_fall_frame(const BambooFall *l) {
    double turns = l->spin / (2.0 * M_PI);
    return (int)floor((turns - floor(turns)) * BAMBOO_FALLS) % BAMBOO_FALLS;
}

// A firefly wanders slowly inside the low band of the view, drifting with the
// walk at its depth, and blinks; one that drifts off the left edge comes back
// past the right one.

static double bamboo_fly_reach(const BambooForest *f, const BambooFly *b) {
    return 5.0 * bamboo_px(f, b->layer);
}

static void bamboo_fly_place(BambooForest *f, BambooFly *b) {
    b->layer = bamboo_uniform(&f->rng) < 0.6 ? 2 : 3;
    b->y = f->h * (kBambooFlyLow + (kBambooFlyHigh - kBambooFlyLow) * bamboo_uniform(&f->rng));
    b->vx = b->vy = 0.0;
    b->phase = bamboo_uniform(&f->rng) * 2.0 * M_PI;
    b->rate = 2.0 * M_PI / (kBambooFPS * (2.5 + 3.5 * bamboo_uniform(&f->rng)));
}

static double bamboo_fly_glow(const BambooFly *b) {
    double s = sin(b->phase);
    return s > 0.0 ? s * s : 0.0;
}

// The panda keeps to its layer's floor: its center is in the layer's scrolled
// coordinates, so it slides with the ground and only its own steps move it
// along. It walks for a while, stops to look around or sits down to eat, then
// walks on, either way; once it has drifted off the left edge it stays away a
// while and comes back past the right one.

static double bamboo_panda_reach(const BambooForest *f) {
    return BAMBOO_PANDA_W / 2.0 * bamboo_px(f, kBambooPandaLayer);
}

// View x of the panda's center.
static double bamboo_panda_x(const BambooForest *f) {
    return f->panda.wx - bamboo_offset(f, kBambooPandaLayer);
}

// The line its feet stand on: down in the ground strip, so the soil hides its
// soles and the grass on the strip its paws, as it does the culms' feet.
static double bamboo_panda_foot(const BambooForest *f) {
    return bamboo_base(f, kBambooPandaLayer) - kBambooPandaSink * bamboo_px(f, kBambooPandaLayer);
}

static int bamboo_panda_frames(BambooForest *f, const double span[2]) {
    return (int)lround(kBambooFPS * (span[0] + (span[1] - span[0]) * bamboo_uniform(&f->rng)));
}

static void bamboo_panda_enter(BambooForest *f, int state) {
    BambooPanda *a = &f->panda;
    a->state = state;
    a->held = 0;
    switch (state) {
    case BAMBOO_PANDA_WALK:  a->left = bamboo_panda_frames(f, kBambooPandaWalk); break;
    case BAMBOO_PANDA_STAND: a->left = bamboo_panda_frames(f, kBambooPandaStand); break;
    case BAMBOO_PANDA_SIT:   a->left = bamboo_panda_frames(f, kBambooPandaSit); break;
    default:                 a->left = bamboo_panda_frames(f, kBambooPandaAway); break;
    }
}

static void bamboo_panda_step(BambooForest *f) {
    BambooPanda *a = &f->panda;
    a->held++;
    if (a->state == BAMBOO_PANDA_AWAY) {
        if (--a->left > 0) return;
        a->dir = bamboo_uniform(&f->rng) < 0.5 ? -1 : 1;
        // An art px past the right edge, so not a sliver of it shows yet.
        a->wx = bamboo_offset(f, kBambooPandaLayer) + f->w + bamboo_panda_reach(f) + bamboo_px(f, kBambooPandaLayer);
        a->walked = 0.0;
        a->visits++;
        bamboo_panda_enter(f, BAMBOO_PANDA_WALK);
        return;
    }
    if (a->state == BAMBOO_PANDA_WALK) {
        double pace = kBambooPandaPace / kBambooFPS;
        a->wx += a->dir * pace * bamboo_px(f, kBambooPandaLayer);
        a->walked += pace;
    }
    if (bamboo_panda_x(f) < -bamboo_panda_reach(f)) {
        bamboo_panda_enter(f, BAMBOO_PANDA_AWAY);
        return;
    }
    if (--a->left > 0) return;
    switch (a->state) {
    case BAMBOO_PANDA_WALK:
        bamboo_panda_enter(f, bamboo_uniform(&f->rng) < 0.55 ? BAMBOO_PANDA_STAND : BAMBOO_PANDA_SIT);
        break;
    case BAMBOO_PANDA_SIT:
        bamboo_panda_enter(f, BAMBOO_PANDA_STAND);
        break;
    default:
        a->dir = bamboo_uniform(&f->rng) < 0.5 ? -1 : 1;
        bamboo_panda_enter(f, BAMBOO_PANDA_WALK);
        break;
    }
}

// bamboo_panda_art is the panda's frame in BAMBOO_ART_PANDA order: the gait
// follows the ground it covers, so its feet never slide.
static int bamboo_panda_art(const BambooPanda *a) {
    switch (a->state) {
    case BAMBOO_PANDA_WALK:
        return BAMBOO_PANDA_WALK_ART + (int)floor(a->walked / kBambooPandaStride) % 4;
    case BAMBOO_PANDA_SIT:
        return BAMBOO_PANDA_SIT_ART + (int)(a->held / lround(kBambooPandaChew * kBambooFPS)) % 2;
    default:
        return BAMBOO_PANDA_STAND_ART;
    }
}

static void bamboo_forest_init(BambooForest *f, uint64_t seed, double w, double h) {
    memset(f, 0, sizeof(*f));
    f->seed = seed;
    f->rng = bamboo_mix(seed) != 0 ? bamboo_mix(seed) : 0x9E3779B97F4A7C15ULL;
    f->w = w > 1.0 ? w : 1.0;
    f->h = h > 1.0 ? h : 1.0;
    f->pixel = f->w * f->h > kBambooBigView ? 2 : 1;
    double area = f->w * f->h / (double)(f->pixel * f->pixel);

    int n = (int)lround(area / kBambooAreaPerFall);
    if (n < 8) n = 8;
    if (n > BAMBOO_MAX_FALLS) n = BAMBOO_MAX_FALLS;
    f->falls = n;
    for (int i = 0; i < n; i++) {
        BambooFall *l = &f->fall[i];
        bamboo_fall_spawn(f, l);
        // Mid-fall over the whole view on the first frame.
        double reach = bamboo_fall_reach(f, l);
        l->x = bamboo_uniform(&f->rng) * (f->w + 2.0 * reach) - reach;
        l->y = bamboo_uniform(&f->rng) * (f->h + 2.0 * reach) - reach;
        l->spawns = 0;
    }

    n = (int)lround(area / kBambooAreaPerFly);
    if (n < 4) n = 4;
    if (n > BAMBOO_MAX_FLIES) n = BAMBOO_MAX_FLIES;
    f->flies = n;
    for (int i = 0; i < n; i++) {
        bamboo_fly_place(f, &f->fly[i]);
        f->fly[i].x = bamboo_uniform(&f->rng) * f->w;
    }

    // The panda is in sight on the first frame, looking around.
    f->panda.dir = bamboo_uniform(&f->rng) < 0.5 ? -1 : 1;
    f->panda.wx = f->w * (0.45 + 0.35 * bamboo_uniform(&f->rng));
    bamboo_panda_enter(f, BAMBOO_PANDA_STAND);
}

static void bamboo_step(BambooForest *f) {
    f->frame++;
    for (int i = 0; i < f->falls; i++) {
        BambooFall *l = &f->fall[i];
        l->phase += l->rate;
        l->spin += l->spinRate;
        l->x += l->vx + l->swing * sin(l->phase);
        l->y += l->vy;
        double reach = bamboo_fall_reach(f, l);
        if (l->y < -reach || l->x < -reach) bamboo_fall_spawn(f, l);
    }
    double lo = f->h * kBambooFlyLow, hi = f->h * kBambooFlyHigh;
    for (int i = 0; i < f->flies; i++) {
        BambooFly *b = &f->fly[i];
        // A random walk with drag, in pt per frame: the drag leaves a third of
        // a speed after a second, the kicks keep it at a few points per second.
        double p = bamboo_px(f, b->layer);
        double drag = pow(0.3, 1.0 / kBambooFPS);
        b->vx = drag * b->vx + (bamboo_uniform(&f->rng) - 0.5) * 0.9 * p / kBambooFPS;
        b->vy = drag * b->vy + (bamboo_uniform(&f->rng) - 0.5) * 0.9 * p / kBambooFPS;
        b->x += b->vx - bamboo_speed(f, b->layer);
        b->y += b->vy;
        if (b->y < lo) { b->y = lo; b->vy = fabs(b->vy); }
        if (b->y > hi) { b->y = hi; b->vy = -fabs(b->vy); }
        b->phase += b->rate;
        if (b->x < -bamboo_fly_reach(f, b)) {
            bamboo_fly_place(f, b);
            b->x = f->w + bamboo_fly_reach(f, b) + bamboo_uniform(&f->rng) * f->w * 0.2;
            b->spawns++;
        }
    }
    bamboo_panda_step(f);
}

// --- Sprite baking --------------------------------------------------------------

// Depth shading: a layer's sprites are dimmed by its light and pulled toward
// the fog color by its fog, so far culms melt into the night instead of turning
// translucent (a far culm never shows through a near one).
static const BambooRGB kBambooFog = { 0.016, 0.036, 0.042 };

static BambooRGB bamboo_shade(char ch, double light, double fog) {
    const char *p = strchr(kBambooPalette, ch);
    BambooRGB c = p != NULL ? kBambooColors[p - kBambooPalette] : (BambooRGB){ 1, 0, 1 };
    if (strchr(kBambooEmissive, ch) != NULL) {
        light = 1.0;
        fog *= 0.5;
    }
    return (BambooRGB){ c.r * light * (1.0 - fog) + kBambooFog.r * fog,
                        c.g * light * (1.0 - fog) + kBambooFog.g * fog,
                        c.b * light * (1.0 - fog) + kBambooFog.b * fog };
}

// bamboo_image bakes a w x h grid of palette characters (row 0 at the top) into
// a premultiplied RGBA image, shaded for one layer and already blown up to
// `scale` frame pixels per art pixel, so drawing it is a 1:1 copy. mirror flips
// it left to right.
static CGImageRef bamboo_image(const char *grid, int w, int h, double light, double fog, int mirror, int scale) {
    int iw = w * scale, ih = h * scale;
    size_t n = (size_t)iw * (size_t)ih * 4;
    uint8_t *px = (uint8_t *)calloc(1, n);
    if (px == NULL) return NULL;
    for (int y = 0; y < ih; y++) {
        for (int x = 0; x < iw; x++) {
            int ax = x / scale, ay = y / scale;
            char ch = grid[ay * w + (mirror ? w - 1 - ax : ax)];
            if (ch == '.' || strchr(kBambooPalette, ch) == NULL) continue;
            BambooRGB c = bamboo_shade(ch, light, fog);
            // B, G, R, A in memory: the frame's own layout, so a blit never
            // converts pixels.
            uint8_t *o = &px[4 * (y * iw + x)];
            o[0] = (uint8_t)lround(255.0 * c.b);
            o[1] = (uint8_t)lround(255.0 * c.g);
            o[2] = (uint8_t)lround(255.0 * c.r);
            o[3] = 255;
        }
    }
    CFDataRef data = CFDataCreate(NULL, px, (CFIndex)n);
    free(px);
    CGDataProviderRef prov = CGDataProviderCreateWithCFData(data);
    CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
    CGImageRef img = CGImageCreate((size_t)iw, (size_t)ih, 8, 32, (size_t)iw * 4, cs,
                                   kCGImageAlphaPremultipliedFirst | kCGBitmapByteOrder32Little,
                                   prov, NULL, false, kCGRenderingIntentDefault);
    CGColorSpaceRelease(cs);
    CGDataProviderRelease(prov);
    CFRelease(data);
    return img;
}

static CGImageRef bamboo_art_image(int art, double light, double fog, int mirror, int scale) {
    const BambooArt *a = &kBambooArt[art];
    return bamboo_image(a->px, a->w, a->h, light, fog, mirror, scale);
}

// Every image one layer draws.
typedef struct {
    CGImageRef chunk[BAMBOO_CHUNKS];        // `chunk` segments stacked, foot first
    CGImageRef seg[BAMBOO_STALKS];
    CGImageRef top[BAMBOO_TOPS];
    CGImageRef leaf[BAMBOO_LEAVES][2];      // [variant][0 hangs right, 1 hangs left]
    CGImageRef ground[BAMBOO_GROUNDS];
    CGImageRef grass[BAMBOO_GRASSES];
    CGImageRef fall[BAMBOO_FALLS];
    CGImageRef fly[BAMBOO_FLIES];
    CGImageRef panda[BAMBOO_PANDAS][2];     // [frame][0 faces right, 1 left]; the panda's layer only
    BambooRGB  soil;                        // fill below the ground strip
} BambooSprites;

// bamboo_chunk_segment picks the segment art of segment s of chunk variant v:
// mostly plain, with branch stubs and aged culm now and then.
static int bamboo_chunk_segment(uint64_t seed, int layer, int v, int s) {
    double u = bamboo_frac(bamboo_hash(seed, layer, (int64_t)(v * 64 + s), 7));
    return BAMBOO_ART_STALK + (u < 0.55 ? 0 : u < 0.7 ? 1 : u < 0.85 ? 2 : 3);
}

static void bamboo_bake_layer(BambooSprites *sp, uint64_t seed, int layer) {
    const BambooLayer *L = &kBambooLayers[layer];
    for (int v = 0; v < BAMBOO_CHUNKS; v++) {
        int rows = BAMBOO_SEG_H * L->chunk;
        char *grid = (char *)malloc((size_t)(BAMBOO_CELL_W * rows));
        if (grid == NULL) continue;
        // Row 0 is the top: the chunk's last segment comes first.
        for (int s = 0; s < L->chunk; s++) {
            const BambooArt *a = &kBambooArt[bamboo_chunk_segment(seed, layer, v, s)];
            memcpy(grid + (size_t)(L->chunk - 1 - s) * BAMBOO_SEG_H * BAMBOO_CELL_W, a->px,
                   (size_t)(BAMBOO_SEG_H * BAMBOO_CELL_W));
        }
        sp->chunk[v] = bamboo_image(grid, BAMBOO_CELL_W, rows, L->light, L->fog, 0, L->scale);
        free(grid);
    }
    for (int i = 0; i < BAMBOO_STALKS; i++) sp->seg[i] = bamboo_art_image(BAMBOO_ART_STALK + i, L->light, L->fog, 0, L->scale);
    for (int i = 0; i < BAMBOO_TOPS; i++) sp->top[i] = bamboo_art_image(BAMBOO_ART_TOP + i, L->light, L->fog, 0, L->scale);
    for (int i = 0; i < BAMBOO_LEAVES; i++) {
        sp->leaf[i][0] = bamboo_art_image(BAMBOO_ART_LEAF + i, L->light, L->fog, 0, L->scale);
        sp->leaf[i][1] = bamboo_art_image(BAMBOO_ART_LEAF + i, L->light, L->fog, 1, L->scale);
    }
    for (int i = 0; i < BAMBOO_GROUNDS; i++) sp->ground[i] = bamboo_art_image(BAMBOO_ART_GROUND + i, L->light, L->fog, 0, L->scale);
    for (int i = 0; i < BAMBOO_GRASSES; i++) sp->grass[i] = bamboo_art_image(BAMBOO_ART_GRASS + i, L->light, L->fog, 0, L->scale);
    for (int i = 0; i < BAMBOO_FALLS; i++) sp->fall[i] = bamboo_art_image(BAMBOO_ART_FALL + i, L->light, L->fog, 0, L->scale);
    for (int i = 0; i < BAMBOO_FLIES; i++) sp->fly[i] = bamboo_art_image(BAMBOO_ART_FLY + i, L->light, L->fog, 0, L->scale);
    for (int i = 0; layer == kBambooPandaLayer && i < BAMBOO_PANDAS; i++) {
        sp->panda[i][0] = bamboo_art_image(BAMBOO_ART_PANDA + i, kBambooPandaLight, kBambooPandaFog, 0, L->scale);
        sp->panda[i][1] = bamboo_art_image(BAMBOO_ART_PANDA + i, kBambooPandaLight, kBambooPandaFog, 1, L->scale);
    }
    // The soil under the strip is the strip's own bottom-left pixel.
    const BambooArt *g = &kBambooArt[BAMBOO_ART_GROUND];
    sp->soil = bamboo_shade(g->px[(g->h - 1) * g->w], L->light, L->fog);
}

static void bamboo_release_layer(BambooSprites *sp) {
    for (int i = 0; i < BAMBOO_CHUNKS; i++) CGImageRelease(sp->chunk[i]);
    for (int i = 0; i < BAMBOO_STALKS; i++) CGImageRelease(sp->seg[i]);
    for (int i = 0; i < BAMBOO_TOPS; i++) CGImageRelease(sp->top[i]);
    for (int i = 0; i < BAMBOO_LEAVES; i++) {
        CGImageRelease(sp->leaf[i][0]);
        CGImageRelease(sp->leaf[i][1]);
    }
    for (int i = 0; i < BAMBOO_GROUNDS; i++) CGImageRelease(sp->ground[i]);
    for (int i = 0; i < BAMBOO_GRASSES; i++) CGImageRelease(sp->grass[i]);
    for (int i = 0; i < BAMBOO_FALLS; i++) CGImageRelease(sp->fall[i]);
    for (int i = 0; i < BAMBOO_FLIES; i++) CGImageRelease(sp->fly[i]);
    for (int i = 0; i < BAMBOO_PANDAS; i++) {
        CGImageRelease(sp->panda[i][0]);
        CGImageRelease(sp->panda[i][1]);
    }
}

// --- View ---------------------------------------------------------------------

#define BAMBOO_STARS 45

static const BambooRGB kBambooSkyLow = { 0.014, 0.030, 0.036 }; // sky at the far floor line
static const BambooRGB kBambooMist   = { 0.070, 0.110, 0.125 };
static const BambooRGB kBambooMoonGlow = { 0.620, 0.700, 0.760 };
static const int kBambooMoonScale = 3; // frame pixels per moon art pixel
static const double kBambooMistBand = 0.22; // mist band height, fraction of the view

@implementation BambooView {
    NSTimer       *_timer;   // ~30 FPS driver; nil when stopped
    BambooForest  *_f;       // the walk (heap: the particle arrays)
    uint64_t       _seed;    // reused when a resize rebuilds the walk
    BambooSprites  _sp[BAMBOO_LAYERS];
    CGImageRef     _moon;
    IOSurfaceRef   _surface[2]; // the frame, twice: one pixel per art pixel of the farthest layer
    CGContextRef   _frame[2];   // a bitmap context on each surface
    int            _cur;        // the surface the next frame is painted into
    double         _unit;    // view points per frame pixel (the forest's base pixel)
    CGImageRef     _back;    // sky, stars and moon: static, painted once per layout
    CGImageRef     _mistBand; // one band of mist, painted once per layout
    BambooStalk   *_stalks;  // scratch: one layer's visible culms
    BambooPiece   *_pieces;  // scratch: one culm's pieces
}

- (instancetype)initWithFrame:(NSRect)frameRect {
    self = [super initWithFrame:frameRect];
    if (self) {
        self.wantsLayer = YES;
        self.layerContentsRedrawPolicy = NSViewLayerContentsRedrawOnSetNeedsDisplay;
        self.layer.backgroundColor = [[NSColor blackColor] CGColor];
        self.layer.opaque = YES;
        // The frame is drawn at art resolution and blown up by the compositor:
        // nearest-neighbour keeps every art pixel a crisp square.
        self.layer.magnificationFilter = kCAFilterNearest;
        self.layer.contentsGravity = kCAGravityResize;
        _f = (BambooForest *)calloc(1, sizeof(BambooForest));
        _stalks = (BambooStalk *)calloc(BAMBOO_MAX_STALKS, sizeof(BambooStalk));
        _pieces = (BambooPiece *)calloc(BAMBOO_MAX_PIECES, sizeof(BambooPiece));
        _seed = ((uint64_t)arc4random() << 32) | (uint64_t)arc4random();
        for (int i = 0; i < BAMBOO_LAYERS; i++) bamboo_bake_layer(&_sp[i], _seed, i);
        _moon = bamboo_art_image(BAMBOO_ART_MOON, 1.0, 0.0, 0, kBambooMoonScale);

        [self computeLayout];
    }
    return self;
}

- (BOOL)isOpaque { return YES; }

// The frame is rendered by hand into a bitmap and handed to the layer, not
// drawn by AppKit (see updateLayer).
- (BOOL)wantsUpdateLayer { return YES; }

// The walk is rebuilt for the view's size, and so is the frame bitmap: one
// pixel per base art pixel, so a Retina laptop paints a quarter of its device
// pixels and the compositor scales the frame up.
- (void)computeLayout {
    NSRect b = [self bounds];
    CGFloat W = b.size.width, H = b.size.height;
    if (_f == NULL) return;
    bamboo_forest_init(_f, _seed, W, H);
    _unit = _f->pixel;

    [self releaseFrames];
    size_t fw = (size_t)ceil(_f->w / _unit), fh = (size_t)ceil(_f->h / _unit);
    CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
    CFPropertyListRef csList = CGColorSpaceCopyPropertyList(cs);
    for (int i = 0; i < 2; i++) {
        _surface[i] = IOSurfaceCreate((__bridge CFDictionaryRef) @{
            (__bridge NSString *)kIOSurfaceWidth : @(fw),
            (__bridge NSString *)kIOSurfaceHeight : @(fh),
            (__bridge NSString *)kIOSurfaceBytesPerElement : @4,
            (__bridge NSString *)kIOSurfacePixelFormat : @((uint32_t)'BGRA'),
        });
        if (_surface[i] == NULL) continue;
        // Tagged like the sprites, so the compositor matches the colors on the
        // GPU instead of the CPU doing it on every frame.
        if (csList != NULL) IOSurfaceSetValue(_surface[i], kIOSurfaceColorSpace, csList);
        _frame[i] = CGBitmapContextCreate(IOSurfaceGetBaseAddress(_surface[i]), fw, fh, 8,
                                          IOSurfaceGetBytesPerRow(_surface[i]), cs,
                                          kCGImageAlphaNoneSkipFirst | kCGBitmapByteOrder32Little);
        if (_frame[i] != NULL) {
            // Everything below is laid out in view points.
            CGContextScaleCTM(_frame[i], 1.0 / _unit, 1.0 / _unit);
            CGContextSetInterpolationQuality(_frame[i], kCGInterpolationNone);
        }
    }
    if (csList != NULL) CFRelease(csList);
    CGColorSpaceRelease(cs);

    [self paintStatics];
}

- (void)releaseFrames {
    for (int i = 0; i < 2; i++) {
        CGContextRelease(_frame[i]);
        _frame[i] = NULL;
        if (_surface[i] != NULL) CFRelease(_surface[i]);
        _surface[i] = NULL;
    }
}

static void bamboo_blit(CGContextRef ctx, CGImageRef img, double x, double y, double unit);

// bamboo_paint renders `draw` into a fresh w x h bitmap in the frame's
// pixel format (view points, `unit` points per pixel) and returns it as an
// image; opaque bitmaps start out black.
static CGImageRef bamboo_paint(double w, double h, double unit, int opaque, void (^draw)(CGContextRef)) {
    size_t pw = (size_t)ceil(w / unit), ph = (size_t)ceil(h / unit);
    if (pw == 0 || ph == 0) return NULL;
    CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
    CGContextRef ctx = CGBitmapContextCreate(NULL, pw, ph, 8, 0, cs,
                                             (opaque ? kCGImageAlphaNoneSkipFirst : kCGImageAlphaPremultipliedFirst) |
                                                 kCGBitmapByteOrder32Little);
    CGColorSpaceRelease(cs);
    if (ctx == NULL) return NULL;
    CGContextScaleCTM(ctx, 1.0 / unit, 1.0 / unit);
    CGContextSetInterpolationQuality(ctx, kCGInterpolationNone);
    if (opaque) {
        CGContextSetGrayFillColor(ctx, 0.0, 1.0);
        CGContextFillRect(ctx, CGRectMake(0, 0, w, h));
    }
    draw(ctx);
    CGImageRef img = CGBitmapContextCreateImage(ctx);
    CGContextRelease(ctx);
    return img;
}

// The parts of the scene that never move are painted once: the night sky
// (black overhead, a faint teal haze down by the far floor), the stars, and the
// moon with its halo; and one band of mist, which every frame lays over the
// foot of the far layers.
- (void)paintStatics {
    CGImageRelease(_back);
    CGImageRelease(_mistBand);
    _back = _mistBand = NULL;
    double W = _f->w, H = _f->h, unit = _unit;
    uint64_t seed = _seed;
    CGImageRef moonImg = _moon;
    CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);

    CGFloat sky[8] = { kBambooSkyLow.r, kBambooSkyLow.g, kBambooSkyLow.b, 1.0, 0.0, 0.0, 0.0, 1.0 };
    CGGradientRef skyG = CGGradientCreateWithColorComponents(cs, sky, NULL, 2);
    CGFloat glow[8] = { kBambooMoonGlow.r, kBambooMoonGlow.g, kBambooMoonGlow.b, 0.06,
                        kBambooMoonGlow.r, kBambooMoonGlow.g, kBambooMoonGlow.b, 0.0 };
    CGGradientRef glowG = CGGradientCreateWithColorComponents(cs, glow, NULL, 2);
    _back = bamboo_paint(W, H, unit, 1, ^(CGContextRef ctx) {
        CGContextDrawLinearGradient(ctx, skyG, CGPointMake(0, H * kBambooLayers[0].base), CGPointMake(0, H * 0.85),
                                    kCGGradientDrawsBeforeStartLocation);
        uint64_t s = bamboo_mix(seed ^ 0x5741525355ULL);
        for (int i = 0; i < BAMBOO_STARS; i++) {
            double x = floor(bamboo_frac(bamboo_mix(s + 3 * i)) * W / unit) * unit;
            double y = floor(H * (0.45 + 0.55 * bamboo_frac(bamboo_mix(s + 3 * i + 1))) / unit) * unit;
            CGContextSetRGBFillColor(ctx, 0.75, 0.80, 0.85, 0.08 + 0.25 * bamboo_frac(bamboo_mix(s + 3 * i + 2)));
            CGContextFillRect(ctx, CGRectMake(x, y, unit, unit));
        }
        const BambooArt *m = &kBambooArt[BAMBOO_ART_MOON];
        double r = m->w * kBambooMoonScale * unit / 2.0;
        CGPoint c = CGPointMake(floor(W * 0.2), floor(H * 0.78));
        CGContextDrawRadialGradient(ctx, glowG, c, 0.0, c, r * 4.4, 0);
        bamboo_blit(ctx, moonImg, c.x - r, c.y - r, unit);
    });

    double band = H * kBambooMistBand;
    CGFloat mist[12] = { kBambooMist.r, kBambooMist.g, kBambooMist.b, 0.0,
                         kBambooMist.r, kBambooMist.g, kBambooMist.b, 0.28,
                         kBambooMist.r, kBambooMist.g, kBambooMist.b, 0.0 };
    CGFloat at[3] = { 0.0, 0.28, 1.0 };
    CGGradientRef mistG = CGGradientCreateWithColorComponents(cs, mist, at, 3);
    _mistBand = bamboo_paint(W, band, unit, 0, ^(CGContextRef ctx) {
        CGContextDrawLinearGradient(ctx, mistG, CGPointMake(0, 0), CGPointMake(0, band), 0);
    });

    CGGradientRelease(skyG);
    CGGradientRelease(glowG);
    CGGradientRelease(mistG);
    CGColorSpaceRelease(cs);
}

- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self computeLayout];
}

// --- Per-frame advance + draw ------------------------------------------------

- (void)step:(NSTimer *)t {
    (void)t;
    if (_f == NULL) return;
    bamboo_step(_f);
    [self setNeedsDisplay:YES];
}

// bamboo_blit copies a pre-scaled sprite with its bottom-left corner at (x, y)
// view points, snapped to a whole frame pixel, so the copy is exactly 1:1.
static void bamboo_blit(CGContextRef ctx, CGImageRef img, double x, double y, double unit) {
    if (img == NULL) return;
    CGContextDrawImage(ctx, CGRectMake(floor(x / unit) * unit, floor(y / unit) * unit,
                                       (double)CGImageGetWidth(img) * unit,
                                       (double)CGImageGetHeight(img) * unit), img);
}

// One culm, as bamboo_culm lays it out.
- (void)drawStalk:(const BambooStalk *)s layer:(int)layer inContext:(CGContextRef)ctx {
    const BambooSprites *sp = &_sp[layer];
    int n = bamboo_culm(_f, layer, s, _pieces, BAMBOO_MAX_PIECES);
    for (int i = 0; i < n; i++) {
        const BambooPiece *pc = &_pieces[i];
        CGImageRef img = NULL;
        switch (pc->kind) {
        case BAMBOO_PIECE_CHUNK: img = sp->chunk[pc->variant]; break;
        case BAMBOO_PIECE_SEG:   img = sp->seg[pc->variant]; break;
        case BAMBOO_PIECE_TOP:   img = sp->top[pc->variant]; break;
        default:                 img = sp->leaf[pc->variant][pc->mirror]; break;
        }
        bamboo_blit(ctx, img, pc->x, pc->y, _unit);
    }
}

// The floor of a layer: soil below, the ground strip tiled along the foot
// line, and tufts on some tiles.
- (void)drawGroundForLayer:(int)layer inContext:(CGContextRef)ctx {
    const BambooSprites *sp = &_sp[layer];
    double p = bamboo_px(_f, layer);
    double base = bamboo_base(_f, layer);
    double off = bamboo_offset(_f, layer);
    double tw = BAMBOO_GROUND_W * p;
    double stripY = base - 7.0 * p;

    CGContextSetRGBFillColor(ctx, sp->soil.r, sp->soil.g, sp->soil.b, 1.0);
    if (stripY > 0.0) CGContextFillRect(ctx, CGRectMake(0.0, 0.0, _f->w, stripY + _unit));

    // Tiles are laid from one snapped origin, so neighbours always meet.
    double origin = floor(off / _unit) * _unit;
    int64_t first = (int64_t)floor(off / tw) - 1;
    int64_t last = (int64_t)floor((off + _f->w) / tw) + 1;
    for (int64_t j = first; j <= last; j++) {
        uint64_t h = bamboo_hash(_f->seed, layer, j, 9);
        bamboo_blit(ctx, sp->ground[h % BAMBOO_GROUNDS], (double)j * tw - origin, stripY, _unit);
    }
    for (int64_t j = first; j <= last; j++) {
        uint64_t h = bamboo_hash(_f->seed, layer, j, 10);
        double x = (double)j * tw - origin;
        if (bamboo_frac(h) < kBambooTuftChance) {
            double tx = x + floor(bamboo_frac(bamboo_mix(h)) * (BAMBOO_GROUND_W - 12)) * p;
            bamboo_blit(ctx, sp->grass[(h >> 20) % BAMBOO_GRASSES], tx, base - 1.0 * p, _unit);
        }
    }
}

- (void)drawFallsForLayer:(int)layer inContext:(CGContextRef)ctx {
    double half = 3.5 * bamboo_px(_f, layer);
    for (int i = 0; i < _f->falls; i++) {
        const BambooFall *l = &_f->fall[i];
        if (l->layer != layer) continue;
        bamboo_blit(ctx, _sp[layer].fall[bamboo_fall_frame(l)], l->x - half, l->y - half, _unit);
    }
}

- (void)drawFliesForLayer:(int)layer inContext:(CGContextRef)ctx {
    double half = 2.5 * bamboo_px(_f, layer);
    for (int i = 0; i < _f->flies; i++) {
        const BambooFly *b = &_f->fly[i];
        if (b->layer != layer) continue;
        double g = bamboo_fly_glow(b);
        if (g < 0.04) continue;
        // Even at its brightest a firefly stays dimmer than the moon.
        CGContextSetAlpha(ctx, (CGFloat)(0.15 + 0.55 * g));
        bamboo_blit(ctx, _sp[layer].fly[g > 0.5 ? 0 : 1], b->x - half, b->y - half, _unit);
    }
    CGContextSetAlpha(ctx, 1.0);
}

- (void)drawPandaInContext:(CGContextRef)ctx {
    const BambooPanda *a = &_f->panda;
    if (a->state == BAMBOO_PANDA_AWAY) return;
    double p = bamboo_px(_f, kBambooPandaLayer);
    CGImageRef img = _sp[kBambooPandaLayer].panda[bamboo_panda_art(a)][a->dir < 0];
    bamboo_blit(ctx, img, bamboo_panda_x(_f) - BAMBOO_PANDA_W / 2.0 * p, bamboo_panda_foot(_f), _unit);
}

// renderFrame paints the whole scene, far to near, into a frame bitmap.
- (void)renderFrameInto:(CGContextRef)ctx {
    double W = _f->w, H = _f->h;

    // Opaque base — the static sky, itself painted over pure #000000 into an
    // opaque bitmap, so nothing can bleed through (T-gh8-03).
    CGContextSetGrayFillColor(ctx, 0.0, 1.0);
    if (_back != NULL) CGContextDrawImage(ctx, CGRectMake(0, 0, W, H), _back);
    else CGContextFillRect(ctx, CGRectMake(0, 0, W, H));

    for (int layer = 0; layer < BAMBOO_LAYERS; layer++) {
        int n = bamboo_stalks(_f, layer, _stalks, BAMBOO_MAX_STALKS);
        for (int i = 0; i < n; i++) [self drawStalk:&_stalks[i] layer:layer inContext:ctx];
        // The panda walks among its layer's culms, behind the grass and the
        // mist of its floor.
        if (layer == kBambooPandaLayer) [self drawPandaInContext:ctx];
        if (layer < BAMBOO_GROUND_LAYERS) [self drawGroundForLayer:layer inContext:ctx];
        if (layer >= 2) [self drawFallsForLayer:layer inContext:ctx];
        if (layer == 2 || layer == 3) [self drawFliesForLayer:layer inContext:ctx];
        // Mist settles over the foot of every layer but the nearest two, so
        // the floor behind it fades into the night.
        if (layer < 3) bamboo_blit(ctx, _mistBand, 0.0, bamboo_base(_f, layer) - H * kBambooMistBand * 0.28, _unit);
    }
}

// AppKit calls this instead of drawRect: (wantsUpdateLayer). The frame is
// painted straight into an IOSurface and the surface handed to the opaque layer
// as its contents: the compositor reads it where it lies, with no image to
// create, copy or convert. The two surfaces take turns, so the one being
// painted is never the one on screen, and a new contents object is what tells
// the layer that the frame changed.
- (void)updateLayer {
    if (_f == NULL || _stalks == NULL || _pieces == NULL) return;
    IOSurfaceRef surface = _surface[_cur];
    CGContextRef ctx = _frame[_cur];
    if (surface == NULL || ctx == NULL) return;
    IOSurfaceLock(surface, 0, NULL);
    [self renderFrameInto:ctx];
    CGContextFlush(ctx);
    IOSurfaceUnlock(surface, 0, NULL);
    self.layer.contents = (__bridge id)surface;
    _cur ^= 1;
}

// --- Lifecycle: start/stop the FPS-capped timer with window attachment -------

- (void)startTimer {
    if (_timer != nil) return;
    _timer = [NSTimer timerWithTimeInterval:kBambooFrameInterval
                                     target:self
                                   selector:@selector(step:)
                                   userInfo:nil
                                    repeats:YES];
    [[NSRunLoop mainRunLoop] addTimer:_timer forMode:NSRunLoopCommonModes];
}

- (void)stopTimer {
    if (_timer != nil) {
        [_timer invalidate];
        _timer = nil;
    }
}

- (void)viewDidMoveToWindow {
    [super viewDidMoveToWindow];
    if (self.window != nil) {
        [self startTimer];
    } else {
        [self stopTimer];
    }
}

- (void)viewWillMoveToWindow:(NSWindow *)newWindow {
    [super viewWillMoveToWindow:newWindow];
    if (newWindow == nil) {
        [self stopTimer]; // fires on [w close] before dealloc
    }
}

- (void)dealloc {
    [self stopTimer];
    free(_f);
    _f = NULL;
    free(_stalks);
    _stalks = NULL;
    free(_pieces);
    _pieces = NULL;
    for (int i = 0; i < BAMBOO_LAYERS; i++) bamboo_release_layer(&_sp[i]);
    CGImageRelease(_moon);
    [self releaseFrames];
    CGImageRelease(_back);
    CGImageRelease(_mistBand);
    // ARC handles _timer's object ref.
}

@end

// --- Test-only shims: expose the pure models to Go unit tests ----------------
//
// The forest, the particles and the sprite art are the unit-testable pieces of
// this view — the frame itself can only be judged by eye. cgo cannot call a
// static C function from a _test.go file, so these extern shims wrap them.
// Mirrors gold_rain_*_for_test; the Go wrappers are in bambooview_darwin.go.

int bamboo_layers_for_test(void) { return BAMBOO_LAYERS; }
int bamboo_fps_for_test(void) { return BAMBOO_FPS; }
int bamboo_max_pieces_for_test(void) { return BAMBOO_MAX_PIECES; }
int bamboo_max_stalks_for_test(void) { return BAMBOO_MAX_STALKS; }
int bamboo_max_particles_for_test(void) { return BAMBOO_MAX_FALLS > BAMBOO_MAX_FLIES ? BAMBOO_MAX_FALLS : BAMBOO_MAX_FLIES; }

void *bamboo_new_for_test(unsigned long long seed, double w, double h) {
    BambooForest *f = (BambooForest *)calloc(1, sizeof(BambooForest));
    if (f != NULL) bamboo_forest_init(f, (uint64_t)seed, w, h);
    return f;
}

void bamboo_step_for_test(void *fp, int frames) {
    if (fp == NULL) return;
    for (int i = 0; i < frames; i++) bamboo_step((BambooForest *)fp);
}

// bamboo_skip_for_test jumps the walk ahead without stepping the particles:
// the forest is a function of the frame alone.
void bamboo_skip_for_test(void *fp, unsigned long long frames) {
    if (fp != NULL) ((BambooForest *)fp)->frame += frames;
}

// bamboo_layer_for_test reports a layer's geometry in this forest: points per
// art px, scroll speed in points per frame, foot line and slot spacing in
// points, and its depth shading.
void bamboo_layer_for_test(void *fp, int layer, double *outPx, double *outSpeed, double *outBase,
                           double *outSpacing, double *outLight, double *outFog) {
    BambooForest *f = (BambooForest *)fp;
    if (f == NULL || layer < 0 || layer >= BAMBOO_LAYERS) return;
    *outPx = bamboo_px(f, layer);
    *outSpeed = bamboo_speed(f, layer);
    *outBase = bamboo_base(f, layer);
    *outSpacing = bamboo_spacing(f, layer);
    *outLight = kBambooLayers[layer].light;
    *outFog = kBambooLayers[layer].fog;
}

// bamboo_stalks_for_test lists a layer's culms in this frame, left to right.
int bamboo_stalks_for_test(void *fp, int layer, long long *outSlot, double *outX, double *outBase,
                           int *outSegs, int *outCapped, int maxOut) {
    BambooForest *f = (BambooForest *)fp;
    if (f == NULL || layer < 0 || layer >= BAMBOO_LAYERS) return 0;
    BambooStalk *st = (BambooStalk *)calloc(BAMBOO_MAX_STALKS, sizeof(BambooStalk));
    if (st == NULL) return 0;
    int n = bamboo_stalks(f, layer, st, BAMBOO_MAX_STALKS);
    if (n > maxOut) n = maxOut;
    for (int i = 0; i < n; i++) {
        outSlot[i] = (long long)st[i].slot;
        outX[i] = st[i].x;
        outBase[i] = st[i].base;
        outSegs[i] = st[i].segs;
        outCapped[i] = st[i].capped;
    }
    free(st);
    return n;
}

// bamboo_culm_for_test lays out culm `index` of a layer's list in this frame,
// as the view draws it: per piece its kind, bottom-left corner and height in
// view points, and whether it is mirrored. It returns the piece count.
int bamboo_culm_for_test(void *fp, int layer, int index, int *outKind, double *outX, double *outY,
                         double *outH, int *outMirror, int maxOut) {
    BambooForest *f = (BambooForest *)fp;
    if (f == NULL || layer < 0 || layer >= BAMBOO_LAYERS) return 0;
    BambooStalk *st = (BambooStalk *)calloc(BAMBOO_MAX_STALKS, sizeof(BambooStalk));
    BambooPiece *pc = (BambooPiece *)calloc(BAMBOO_MAX_PIECES, sizeof(BambooPiece));
    int n = 0;
    if (st != NULL && pc != NULL && index >= 0 && index < bamboo_stalks(f, layer, st, BAMBOO_MAX_STALKS)) {
        n = bamboo_culm(f, layer, &st[index], pc, BAMBOO_MAX_PIECES);
        if (n > maxOut) n = maxOut;
        for (int i = 0; i < n; i++) {
            outKind[i] = pc[i].kind;
            outX[i] = pc[i].x;
            outY[i] = pc[i].y;
            outH[i] = pc[i].h;
            outMirror[i] = pc[i].mirror;
        }
    }
    free(st);
    free(pc);
    return n;
}

// bamboo_panda_for_test reports the panda: its layer, view x of its center,
// the line its feet stand on, how far past an edge its center must be before
// none of it shows, its state, the way it faces, its frame in BAMBOO_ART_PANDA
// order and how many times it has come back.
void bamboo_panda_for_test(void *fp, int *outLayer, double *outX, double *outFoot, double *outReach,
                           int *outState, int *outDir, int *outArt, unsigned *outVisits) {
    BambooForest *f = (BambooForest *)fp;
    if (f == NULL) return;
    *outLayer = kBambooPandaLayer;
    *outX = bamboo_panda_x(f);
    *outFoot = bamboo_panda_foot(f);
    *outReach = bamboo_panda_reach(f);
    *outState = f->panda.state;
    *outDir = f->panda.dir;
    *outArt = bamboo_panda_art(&f->panda);
    *outVisits = f->panda.visits;
}

int bamboo_falls_for_test(void *fp, double *outX, double *outY, double *outReach, int *outLayer,
                          int *outFrame, unsigned *outSpawns, int maxOut) {
    BambooForest *f = (BambooForest *)fp;
    if (f == NULL) return 0;
    int n = f->falls < maxOut ? f->falls : maxOut;
    for (int i = 0; i < n; i++) {
        const BambooFall *l = &f->fall[i];
        outX[i] = l->x;
        outY[i] = l->y;
        outReach[i] = bamboo_fall_reach(f, l);
        outLayer[i] = l->layer;
        outFrame[i] = bamboo_fall_frame(l);
        outSpawns[i] = l->spawns;
    }
    return n;
}

int bamboo_flies_for_test(void *fp, double *outX, double *outY, double *outReach, double *outGlow,
                          unsigned *outSpawns, int maxOut) {
    BambooForest *f = (BambooForest *)fp;
    if (f == NULL) return 0;
    int n = f->flies < maxOut ? f->flies : maxOut;
    for (int i = 0; i < n; i++) {
        const BambooFly *b = &f->fly[i];
        outX[i] = b->x;
        outY[i] = b->y;
        outReach[i] = bamboo_fly_reach(f, b);
        outGlow[i] = bamboo_fly_glow(b);
        outSpawns[i] = b->spawns;
    }
    return n;
}

void bamboo_free_for_test(void *fp) { free(fp); }

// bamboo_art_for_test copies sprite frame `frame` (h rows of w palette
// characters, row 0 at the top) into buf, its name into name and its size into
// w/h, and returns the frame count; with a frame out of range it only returns
// the count. palette receives the palette characters, emissive the ones fog
// dims less.
int bamboo_art_for_test(int frame, char *name, int nn, int *w, int *h, char *buf, int n,
                        char *palette, char *emissive, int pn) {
    if (palette != NULL && pn > 0) snprintf(palette, (size_t)pn, "%s", kBambooPalette);
    if (emissive != NULL && pn > 0) snprintf(emissive, (size_t)pn, "%s", kBambooEmissive);
    if (frame >= 0 && frame < BAMBOO_ART_COUNT) {
        const BambooArt *a = &kBambooArt[frame];
        if (name != NULL && nn > 0) snprintf(name, (size_t)nn, "%s", a->name);
        if (w != NULL) *w = a->w;
        if (h != NULL) *h = a->h;
        if (buf != NULL && n > 0) snprintf(buf, (size_t)n, "%s", a->px);
    }
    return BAMBOO_ART_COUNT;
}
