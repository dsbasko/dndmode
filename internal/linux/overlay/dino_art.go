//go:build linux

package overlay

// The dino sprites: own pixel art after the Chrome offline runner, drawn at
// half the runner's resolution and baked at twice the size, so one art
// pixel is a 2x2 block of frame pixels. One palette, '.' transparent; each
// frame is h rows of w palette characters, row 0 at the top. The night
// theme needs no outline: the black shield shows through the eye and the
// gaps.

const dinoPalette = "ag"

var dinoColors = []dvdRGB{
	{0.675, 0.675, 0.675}, // a dino, obstacles, ground line and score (#ACACAC)
	{0.373, 0.373, 0.373}, // g clouds and pebbles (#5F5F5F)
}

// dinoArtScale is frame pixels per art pixel.
const dinoArtScale = 2

// dinoArtFrame is one sprite frame.
type dinoArtFrame struct {
	name string
	w, h int
	px   string
}

// Frame indices into dinoArt.
const (
	dinoArtRun    = 0                 // 2 frames
	dinoArtJump   = dinoArtRun + 2    // 1 frame
	dinoArtDuck   = dinoArtJump + 1   // 2 frames
	dinoArtCactus = dinoArtDuck + 2   // 3 small cacti
	dinoArtBig    = dinoArtCactus + 3 // 3 large cacti
	dinoArtBird   = dinoArtBig + 3    // 2 frames: wings up, wings down
	dinoArtCloud  = dinoArtBird + 2   // 1 frame
	dinoArtGround = dinoArtCloud + 1  // 3 ground tiles
	dinoArtDigit  = dinoArtGround + 3 // 0..9
	dinoArtCount  = dinoArtDigit + 10
)

// dinoHead is the standing dino above the legs: head, arm, body and tail.
const dinoHead = "...........aaaaaaaaa.." +
	"..........aa.aaaaaaaa." +
	"..........aaaaaaaaaaa." +
	"..........aaaaaaaaaaa." +
	"..........aaaaaaaaaaa." +
	"..........aaaaa......." +
	"..........aaaaaaaa...." +
	"a........aaaaa........" +
	"a.......aaaaaa........" +
	"aa.....aaaaaaaaa......" +
	"aaa...aaaaaaaa.a......" +
	"aaaaaaaaaaaaaa........" +
	"aaaaaaaaaaaaaa........" +
	".aaaaaaaaaaaa........." +
	"..aaaaaaaaaaa........." +
	"...aaaaaaaaa.........." +
	"....aaaaaaaa.........." +
	".....aaaaaa..........." +
	".....aaa.aaa.........."

// dinoDuckHead is the ducking dino above the legs: head stretched forward
// level with the back.
const dinoDuckHead = "....................aaaaaaaa.." +
	"a.....aaaaaaaaa....aa.aaaaaaa." +
	"aa..aaaaaaaaaaaaaaaaaaaaaaaaa." +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaa." +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaa." +
	".aaaaaaaaaaaaaaaaaaaaaa......." +
	"..aaaaaaaaaaaaaaaaaaaaaaaaa..." +
	"...aaaaaaaaaaaaaa.a..........." +
	"....aaaaaaaaaaa..............." +
	".....aaa.aaa.................."

var dinoArt = [dinoArtCount]dinoArtFrame{
	{"run0", 22, 24, dinoHead +
		".....aa...aaaa........" +
		".....a................" +
		".....a................" +
		".....a................" +
		".....aa..............."},
	{"run1", 22, 24, dinoHead +
		"....aaa...aa.........." +
		"...........a.........." +
		"...........a.........." +
		"...........a.........." +
		"...........aa........."},
	{"jump", 22, 24, dinoHead +
		".....aa...aa.........." +
		".....a.....a.........." +
		".....a.....a.........." +
		".....a.....a.........." +
		".....aa....aa........."},
	{"duck0", 30, 13, dinoDuckHead +
		".....aa...aaaa................" +
		".....a........................" +
		".....aa......................."},
	{"duck1", 30, 13, dinoDuckHead +
		"....aaa...aa.................." +
		"...........a.................." +
		"...........aa................."},
	{"cactus0", 9, 18,
		"....a...." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa.a." +
			"a..aaa.a." +
			"a..aaa.a." +
			"a..aaa.a." +
			"a..aaaaa." +
			"a..aaaa.." +
			"aa.aaa..." +
			".aaaaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..."},
	{"cactus1", 9, 18,
		"....a...." +
			"...aaa..." +
			"...aaa..." +
			"a..aaa..." +
			"a..aaa..." +
			"a..aaa..." +
			"aaaaaa..." +
			".aaaaa.a." +
			"...aaa.a." +
			"...aaa.a." +
			"...aaaaa." +
			"...aaaa.." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..."},
	{"cactus2", 9, 18,
		"....a...." +
			"...aaa..." +
			"...aaa.a." +
			"...aaa.a." +
			"...aaa.a." +
			"...aaaaa." +
			"...aaaa.." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..." +
			"...aaa..."},
	{"big0", 13, 25,
		".....aaa....." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			".a..aaaaa...." +
			"aaa.aaaaa..a." +
			"aaa.aaaaa.aaa" +
			"aaa.aaaaa.aaa" +
			"aaa.aaaaa.aaa" +
			"aaa.aaaaa.aaa" +
			"aaa.aaaaa.aaa" +
			"aaaaaaaaa.aaa" +
			".aaaaaaaa.aaa" +
			"....aaaaaaaaa" +
			"....aaaaaaaa." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...."},
	{"big1", 13, 25,
		".....aaa....." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa..a." +
			"....aaaaa.aaa" +
			"....aaaaa.aaa" +
			"....aaaaa.aaa" +
			".a..aaaaa.aaa" +
			"aaa.aaaaaaaaa" +
			"aaa.aaaaaaaa." +
			"aaa.aaaaa...." +
			"aaa.aaaaa...." +
			"aaa.aaaaa...." +
			"aaaaaaaaa...." +
			".aaaaaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...."},
	{"big2", 13, 25,
		".....aaa....." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			".a..aaaaa...." +
			"aaa.aaaaa...." +
			"aaa.aaaaa...." +
			"aaa.aaaaa...." +
			"aaa.aaaaa...." +
			"aaaaaaaaa...." +
			".aaaaaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...." +
			"....aaaaa...."},
	{"bird0", 23, 20,
		".........a............." +
			".........aa............" +
			".........aaa..........." +
			".........aaaa.........." +
			".........aaaaa........." +
			"....a....aaaaaa........" +
			"...aaa...aaaaaaa......." +
			"..aa.aa..aaaaaaaa......" +
			".aaaaaaa.aaaaaaaaa....." +
			"aaaaaaaaaaaaaaaaaaaaa.." +
			"......aaaaaaaaaaaaaaaaa" +
			".......aaaaaaaaaaaaaa.." +
			"........aaaaaaaaaaaaaaa" +
			".........aaaaaa........" +
			"......................." +
			"......................." +
			"......................." +
			"......................." +
			"......................." +
			"......................."},
	{"bird1", 23, 20,
		"......................." +
			"......................." +
			"......................." +
			"......................." +
			"......................." +
			"....a.................." +
			"...aaa................." +
			"..aa.aa................" +
			".aaaaaaa..............." +
			"aaaaaaaaaaaaaaaaaaaaa.." +
			"......aaaaaaaaaaaaaaaaa" +
			".......aaaaaaaaaaaaaa.." +
			"........aaaaaaaaaaaaaaa" +
			".........aaaaaa........" +
			".........aaaaa........." +
			".........aaaa.........." +
			".........aaa..........." +
			".........aa............" +
			".........a............." +
			"......................."},
	{"cloud", 23, 7,
		"..........gggg........." +
			".......ggg....ggg......" +
			"......g..........g....." +
			"...ggg............ggg.." +
			"..g...................g" +
			".g....................g" +
			"ggggggggggggggggggggggg"},
	{"ground0", 50, 5,
		"...........aa.......................a............." +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
			"....g............gg..................g............" +
			"..................................gg.............." +
			".........g....................g..................."},
	{"ground1", 50, 5,
		"......................aaa........................." +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
			"..g...................g...............g..........." +
			"...........gg..................................g.." +
			"......................................g..........."},
	{"ground2", 50, 5,
		"....................................aa............" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
			".......g............................g............." +
			"g.....................gg.........................." +
			"..............g.................................g."},
	{"digit0", 5, 7, ".aaa." + "a...a" + "a..aa" + "a.a.a" + "aa..a" + "a...a" + ".aaa."},
	{"digit1", 5, 7, "..a.." + ".aa.." + "..a.." + "..a.." + "..a.." + "..a.." + ".aaa."},
	{"digit2", 5, 7, ".aaa." + "a...a" + "....a" + "...a." + "..a.." + ".a..." + "aaaaa"},
	{"digit3", 5, 7, "aaaaa" + "...a." + "..a.." + "...a." + "....a" + "a...a" + ".aaa."},
	{"digit4", 5, 7, "...a." + "..aa." + ".a.a." + "a..a." + "aaaaa" + "...a." + "...a."},
	{"digit5", 5, 7, "aaaaa" + "a...." + "aaaa." + "....a" + "....a" + "a...a" + ".aaa."},
	{"digit6", 5, 7, "..aa." + ".a..." + "a...." + "aaaa." + "a...a" + "a...a" + ".aaa."},
	{"digit7", 5, 7, "aaaaa" + "....a" + "...a." + "..a.." + ".a..." + ".a..." + ".a..."},
	{"digit8", 5, 7, ".aaa." + "a...a" + "a...a" + ".aaa." + "a...a" + "a...a" + ".aaa."},
	{"digit9", 5, 7, ".aaa." + "a...a" + "a...a" + ".aaaa" + "....a" + "...a." + ".aa.."},
}
