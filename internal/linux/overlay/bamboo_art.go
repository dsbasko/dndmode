//go:build linux

package overlay

// The bamboo sprites, copied verbatim from the @@BAMBOO_ART_BEGIN@@ block
// of internal/macos/cocoa/bambooview_darwin.m; change the art there first.
// TestBambooArt_MatchesMacOS keeps the two identical. One shared palette,
// '.' transparent; each frame is h rows of w palette characters, row 0 at
// the top.

const bambooPalette = "odmlhnbkstucefgpqxr"

var bambooColors = []dvdRGB{
	{0.027, 0.055, 0.071}, // o night outline
	{0.090, 0.149, 0.173}, // d bamboo shadow
	{0.165, 0.231, 0.239}, // m bamboo body
	{0.251, 0.325, 0.310}, // l moonlit bamboo rim
	{0.384, 0.443, 0.431}, // h rare moonlit glint
	{0.192, 0.263, 0.259}, // n bamboo node
	{0.078, 0.118, 0.137}, // b soil
	{0.024, 0.043, 0.059}, // k deep soil
	{0.082, 0.118, 0.149}, // s stone shadow
	{0.157, 0.208, 0.239}, // t stone body
	{0.243, 0.298, 0.325}, // u stone moonlit edge
	{0.663, 0.710, 0.749}, // c moon silver
	{0.463, 0.533, 0.576}, // e moon shade
	{0.455, 0.518, 0.357}, // f dim firefly core
	{0.133, 0.184, 0.129}, // g faint firefly halo
	{0.471, 0.522, 0.549}, // p moonlit white fur
	{0.255, 0.294, 0.325}, // q white fur in shadow
	{0.067, 0.106, 0.133}, // x black fur
	{0.149, 0.192, 0.227}, // r moonlit black fur rim
}

// bambooEmissive are the characters that are light rather than matter
// (moon, firefly): depth fog dims them less than the forest.
const bambooEmissive = "cefg"

// bambooArtFrame is one sprite frame.
type bambooArtFrame struct {
	name string
	w, h int
	px   string
}

var bambooArt = [bambooArtCount]bambooArtFrame{
	{"stalk0", 12, 24,
		"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...ohmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"..onnnnnno.." +
			"...onnnno..." +
			"...olmmdo..."},
	{"stalk1", 12, 24,
		"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...ohmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo.lm" +
			"...olmmdo.d." +
			"..onnnnnno.." +
			"...onnnno..." +
			"...olmmdo..."},
	{"stalk2", 12, 24,
		"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...ohmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"lm.olmmdo..." +
			".d.olmmdo..." +
			"..onnnnnno.." +
			"...onnnno..." +
			"...olmmdo..."},
	{"stalk3", 12, 24,
		"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmddo..." +
			"...olmddo..." +
			"...olmddo..." +
			"...olmddo..." +
			"...olmmdo..." +
			"...olmddo..." +
			"...olmddo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...ohmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"..onnnnnno.." +
			"...onnnno..." +
			"...olmmdo..."},
	{"top0", 12, 24,
		"............" +
			"............" +
			".....lo....." +
			".....lo....." +
			".....lo....." +
			".....lo....." +
			".....lo....." +
			"....oldo...." +
			"....olmo...." +
			"....olmo...." +
			"....olmo...." +
			"...olmmdo..." +
			"...ohmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"..onnnnnno.." +
			"...onnnno..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..."},
	{"top1", 12, 24,
		"............" +
			"............" +
			".....lo....." +
			".l...lo...l." +
			".lm..lo..lm." +
			"..lm.lo.lm.." +
			"...lmlo.m..." +
			"....oldo...." +
			"....olmoml.." +
			"....olmo.ml." +
			"....olmo..d." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...ohmmdo..." +
			"...olmmdo..." +
			"..onnnnnno.." +
			"...onnnno..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..." +
			"...olmmdo..."},
	{"leaf0", 24, 16,
		"........................" +
			"........................" +
			"........................" +
			"........................" +
			"mmmm...................." +
			"....mmmmm..............." +
			"....d....mmmmm.........." +
			"....lm....d...mmmlhl...." +
			".....lm....lm.....mmll.." +
			".....lm.....lm.......ml." +
			"......lm.....lm.......ml" +
			"......lm......lm.......d" +
			".......m.......lm......." +
			".......d........m......." +
			"........d.......d......." +
			".................d......"},
	{"leaf1", 24, 16,
		"........................" +
			"........................" +
			"........................" +
			"mmmmm..................." +
			"..d..mmmmmm............." +
			"...lm..d...mmmmm........" +
			"....lm..lm..d...mmmlll.." +
			"....lm...lm..lm..d..mml." +
			".....m....lm..lm..lm...d" +
			".....d.....m..lm...lm..." +
			".....d......d..lm...lm.." +
			".............d.lm....m.." +
			"................m.....d." +
			"................d......." +
			"................d......." +
			"........................"},
	{"leaf2", 24, 16,
		"........................" +
			"........................" +
			"mmm....................." +
			"...mmm.................." +
			"...d..mmm..............." +
			"....lm...mmm............" +
			"....lm...d..mmm........." +
			".....lm...lm...mmmll...." +
			".....lm...lm......mml..." +
			".....lm....lm.......ml.." +
			"......m....lm........lm." +
			"......d.....lm.......lm." +
			"......d.....m.........m." +
			"............d.........d." +
			"............d.........d." +
			"........................"},
	{"leaf3", 24, 16,
		"........................" +
			"........................" +
			"........................" +
			"........................" +
			"mmmmmm.................." +
			"..d...mmmmm............." +
			"...lm..d...mmmmmm......." +
			"....lm.lm...d....mmmml.." +
			".....m..lm..lm...lm..ml." +
			".....d..lm...lm...lm..md" +
			".........m....lm...lm..." +
			".........d....lm....lm.." +
			"...............m.....m.." +
			"................d.....d." +
			"........................" +
			"........................"},
	{"leaf4", 24, 16,
		"........................" +
			"........................" +
			"mm......................" +
			"..mm...................." +
			"...dmmm................." +
			"...lm..mm..............." +
			"...lm..d.mmm............" +
			"....lm.lm...mmm........." +
			"....lm..lm..d..mm......." +
			"....m...lm..lm...mmlll.." +
			"....d....lm..lm.....mml." +
			".........m....lm......ml" +
			".........d.....m.......d" +
			".........d......d......." +
			"........................" +
			"........................"},
	{"leaf5", 24, 16,
		"........................" +
			"........................" +
			"........................" +
			"........................" +
			"..mmmmmmmm.............." +
			"mm.d..d...mmmm.........." +
			"...lm..lm.d...mmmm......" +
			"....lm..lmlm..d...mmm..." +
			"....m....m.lm.lm..d..l.." +
			".....d....dlm..lm.lm.ml." +
			"............m...m..lm.ml" +
			"............d....d..lm.d" +
			"............d.......m..." +
			".....................d.." +
			"........................" +
			"........................"},
	{"ground0", 32, 12,
		"................................" +
			".........l......................" +
			"..........m..............l......" +
			"..l.......dm............m......." +
			"...m...d...d..........dm...d...." +
			"...dm..md.bk...b...d...d...md..." +
			"kkbbkkbbkbkkbbkkkbbkkbbkkkbbkkbk" +
			"kkbbkkkbkkbkkbbkkkbbkkbkbbkkbbkk" +
			"kkkbbbkkkkbbkkkkbbbkkkkbbkkkkbkk" +
			"kkkkkkbkkkkkkbbkkkkkkbkkkkkkkkkk" +
			"kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk" +
			"kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"},
	{"ground1", 32, 12,
		"................................" +
			"......................l........." +
			".....l...............m.........." +
			"......m.........l...dm.........." +
			"..d...dm....d....m...d.....l...." +
			"..md...d..bkmd...dm.bk....dm...." +
			"kkbkkkbbkkbbkkkbbkkbkkbbkkbbkkbk" +
			"kkbkkbbkkbkbbkkkbbkkbbkkkkbbkkkk" +
			"kkbbkkkkbbbkkkkkkbkkkkkbbkkkkbkk" +
			"kkkkkkbbkkkkkkbkkkkkkbbkkkkkkkkk" +
			"kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk" +
			"kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"},
	{"grass0", 12, 8,
		"............" +
			"..l........." +
			"...m....l..." +
			"...lm..m...." +
			".l..m.dm..l." +
			"..m.dmmd.m.." +
			"...mdmmdm..." +
			"....dmmdo..."},
	{"grass1", 12, 8,
		".....l......" +
			".....lm....." +
			".l....m...l." +
			"..lm..m..lm." +
			"...lm.mdmm.." +
			"l...m.mdm..." +
			".m...mmd...." +
			"..ddmmmd...."},
	{"grass2", 12, 8,
		"............" +
			"............" +
			"...l....l..." +
			"....m..lm..." +
			".l..lm.m...." +
			"..lm.mdm..l." +
			"....mmdmdd.." +
			".....mmd...."},
	{"fall0", 7, 7,
		"......." +
			"......." +
			"......." +
			".llllm." +
			"..mmmmd" +
			"......." +
			"......."},
	{"fall1", 7, 7,
		"......." +
			"..l...." +
			"..lm..." +
			"...lm.." +
			"....lm." +
			".....d." +
			"......."},
	{"fall2", 7, 7,
		"...l..." +
			"...l..." +
			"...m..." +
			"...m..." +
			"...d..." +
			"...d..." +
			"......."},
	{"fall3", 7, 7,
		"......." +
			".....l." +
			"....lm." +
			"...lm.." +
			"..lm..." +
			"..d...." +
			"......."},
	{"moon", 24, 24,
		".........cccccc........." +
			"......cccccccccccc......" +
			".....ccccccccccccce....." +
			"....ccccccccccccccce...." +
			"...ccccccccccccccccce..." +
			"..ccccccccccccccccccce.." +
			".ccccccccccccccccccccce." +
			".ccccceeeccccccccccccce." +
			".cccceeeeccccccccccccce." +
			"ccccceeeccccccccccccccce" +
			"ccccccecccccccccccccccce" +
			"ccccccccccccccceeeccccce" +
			"cccccccccccccceeeeccccce" +
			"cccccccccccccceeecccccce" +
			"ecccccceccccccceccccccce" +
			".ccccceecccccccccccccce." +
			".ccccceeeccccccccccccce." +
			".ecccccecccccccccccccce." +
			"..ecccccccccccccccccce.." +
			"...ecccccccccccccccce..." +
			"....ecccccccccccccce...." +
			".....ecccccccccccce....." +
			"......eeecccccceee......" +
			".........eeeeee........."},
	{"firefly0", 5, 5,
		"....." +
			"..g.." +
			".gfg." +
			"..g.." +
			"....."},
	{"firefly1", 5, 5,
		"....." +
			"....." +
			"..f.." +
			"....." +
			"....."},
	{"pwalk0", 30, 18,
		".............................." +
			".............................." +
			"....................oo....o..." +
			".........ooooooo...orrooooro.." +
			"......ooopppppppoooxxxppprxxo." +
			".....oppppppppppprqoqppppqxxo." +
			"....opppppppppppprxxqpppxpqo.." +
			"...oppppppppppppxrxqppprxxqo.." +
			"...oppppppppppppxrxqqppxxxpo.." +
			"..oqpppppppppppprxxxqppxxxxxo." +
			"..opqqqppppppppprrxxqqqqpqxqo." +
			"..oqqrxxxqqqqqqqrrxxxqqqqxqo.." +
			"...oorxxqqqqqqqqxxrxxxqqqqo..." +
			"...oxrxxqqqqqqqqqxrxxxxooo...." +
			"...oxrxxoooxooooooxrxxo......." +
			"..oxrxxo..orrxxo.oxrxxxo......" +
			"..oxrrrxo..oooo...oxrrrxo....." +
			"...ooooo...........ooooo......"},
	{"pwalk1", 30, 18,
		".............................." +
			".............................." +
			"....................oo....o..." +
			".........ooooooo...orrooooro.." +
			"......ooopppppppoooxxxppprxxo." +
			".....oppppppppppprqoqppppqxxo." +
			"....opppppppppppprxxqpppxpqo.." +
			"...oppppppppppppxrxqppprxxqo.." +
			"...oppppppppppppxrxqqppxxxpo.." +
			"..oqpppppppppppprxxxqppxxxxxo." +
			"..opqqqppppppppprrxxqqqqpqxqo." +
			"..oqqrxxxqqqqqqqrrxxxqqqqxqo.." +
			"...ooxrxxqqqqqqqxxrxxxqqqqo..." +
			"....oxrxxqqqqqqqqxrxxxxooo...." +
			"....oxrxxoxooooooxrxxoxooo...." +
			"....oxrxxorrxxo.oxrxxorrxxo..." +
			"....oxrrrxoooo..oxrrrxoooo...." +
			".....ooooo.......ooooo........"},
	{"pwalk2", 30, 18,
		".............................." +
			".............................." +
			"....................oo....o..." +
			".........ooooooo...orrooooro.." +
			"......ooopppppppoooxxxppprxxo." +
			".....oppppppppppprqoqppppqxxo." +
			"....opppppppppppprxxqpppxpqo.." +
			"...oppppppppppppxrxqppprxxqo.." +
			"...oppppppppppppxrxqqppxxxpo.." +
			"..oqpppppppppppprxxxqppxxxxxo." +
			"..opqqqppppppppprrxxqqqqpqxqo." +
			"..oqqrxxxqqqqqqqrrxxxqqqqxqo.." +
			"...ooorxxqqqqqqqxxrxxxqqqqo..." +
			".....oxrxxqqqqqqqxrxxxxooo...." +
			"......oxrxxoooooxrxxooxxo....." +
			"......oxrrrxo.oxrrrxooxxooo..." +
			".......orrxxo..ooooo..orrxxo.." +
			"........oooo...........oooo..."},
	{"pwalk3", 30, 18,
		".............................." +
			".............................." +
			"....................oo....o..." +
			".........ooooooo...orrooooro.." +
			"......ooopppppppoooxxxppprxxo." +
			".....oppppppppppprqoqppppqxxo." +
			"....opppppppppppprxxqpppxpqo.." +
			"...oppppppppppppxrxqppprxxqo.." +
			"...oppppppppppppxrxqqppxxxpo.." +
			"..oqpppppppppppprxxxqppxxxxxo." +
			"..opqqqppppppppprrxxqqqqpqxqo." +
			"..oqqrxxxqqqqqqqrrxxxqqqqxqo.." +
			"...ooxrxxqqqqqqqxxrxxxqqqqo..." +
			"....oxrxxqqqqqqqqxrxxxxooo...." +
			"....oxrxxoxxoooooxrxxxxo......" +
			"....oxrrrxxxoo..oxrrrxxxoo...." +
			".....ooooorrxxo..ooooorrxxo..." +
			"..........oooo........oooo...."},
	{"pstand", 30, 18,
		".............................." +
			".............................." +
			"...................ooo....o..." +
			".........ooooooo..orrxooooro.." +
			"......ooopppppppoooxxpppprxxo." +
			".....oppppppppppprqxqppppqxxo." +
			"....opppppppppppprxxpxppppqo.." +
			"...oppppppppppppxrxqrxxpprxo.." +
			"...oppppppppppppxrxpxxxppxxqo." +
			"..oqpppppppppppprxxqxxxppxxqo." +
			"..opqqqppppppppprrxqqpqxxxqqo." +
			"..oqqrxxxqqqqqqqrrxxqqqqxqqo.." +
			"...ooxrxxqqqqqqqxxrxxqqqqqo..." +
			"....oxrxxqqqqqqqqxrxxoxxoo...." +
			"....oxrxxoxxoooooxrxxooxxo...." +
			"....oxrxxooxxo..oxrxxooxxoo..." +
			"....oxrrrxorrxo.oxrrrxoorrxo.." +
			".....ooooo.ooo...ooooo..ooo..."},
	{"psit0", 30, 18,
		".............................." +
			"..............ooo....o........" +
			".............orrxooooro......." +
			".............oxxpppprxxo......" +
			".............oqppppppxxo......" +
			".............oqpppprxqo......." +
			"............oqpppppxxxqoooo..." +
			"............orqqppxxxqqxxmlo.." +
			"..........ooxrqqqpppxqqqqlmo.." +
			".........oppxxrxqqqqqxxqmlo..." +
			".......ooppppxxrxqqqqrxxxo...." +
			"......oqqpppppxxrrxxxxxolo...." +
			".....oqrpppppppxxxrrrrrnmo...." +
			"....orrxxxxpppppqqxxxxxxo....." +
			"....orxxxxxppppqqrxxqoomo....." +
			"...oxrxxxxxxqqqqqrxxxooo......" +
			"....oxrxxxxxqqqqxxrrxxxo......" +
			".....oooooooooooooooooo......."},
	{"psit1", 30, 18,
		".............................." +
			"..............ooo....o........" +
			".............orrxooooro......." +
			".............oxxpppprxxo......" +
			".............oqppppppxxo......" +
			".............oqpppprxqo......." +
			"............oqpppppxxxqooo...." +
			"............orqqppxxxqqxmlo..." +
			"..........ooxrqqqpppxqqqlmo..." +
			".........oppxxrxqqqqqxxxlo...." +
			".......ooppppxxrxqqqqrqxo....." +
			"......oqqpppppxxrrxxxxxlo....." +
			".....oqrpppppppxxxrrrrnmo....." +
			"....orrxxxxpppppqqxxxxxo......" +
			"....orxxxxxppppqqrxxqomo......" +
			"...oxrxxxxxxqqqqqrxxxoo......." +
			"....oxrxxxxxqqqqxxrrxxxo......" +
			".....oooooooooooooooooo......."},
}
