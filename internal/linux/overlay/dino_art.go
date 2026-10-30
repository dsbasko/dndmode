//go:build linux

package overlay

// The dino sprites: own pixel art after the Chrome offline runner, drawn at
// half the runner's resolution and baked at twice the size, so one art
// pixel is a 2x2 block of frame pixels. One palette, '.' transparent; each
// frame is h rows of w palette characters, row 0 at the top. No outline:
// the background shows through the eye and the gaps.
//
// The palette has a night and a day colour per character, after the Chrome
// runner's two themes, and the shades between them; the run fades from one
// to the other. By day the moon and the stars take the background colour,
// so they fade out with the night.

const dinoPalette = "agmstu"

// dinoShades is the number of baked shades: 0 is night, dinoShades-1 day.
const dinoShades = 9

var dinoNight = []dvdRGB{
	{172.0 / 255, 172.0 / 255, 172.0 / 255}, // a dino, obstacles, ground line and score (#ACACAC)
	{95.0 / 255, 95.0 / 255, 95.0 / 255},    // g clouds and pebbles (#5F5F5F)
	{240.0 / 255, 240.0 / 255, 240.0 / 255}, // m moon (#F0F0F0)
	{218.0 / 255, 218.0 / 255, 218.0 / 255}, // s stars and a shooting star's head (#DADADA)
	{138.0 / 255, 138.0 / 255, 138.0 / 255}, // t a shooting star's tail (#8A8A8A)
	{74.0 / 255, 74.0 / 255, 74.0 / 255},    // u the tail's end (#4A4A4A)
}

var dinoDay = []dvdRGB{
	{83.0 / 255, 83.0 / 255, 83.0 / 255},    // a (#535353)
	{218.0 / 255, 218.0 / 255, 218.0 / 255}, // g (#DADADA)
	dinoDayBack,                             // m gone into the sky
	dinoDayBack,                             // s gone into the sky
	dinoDayBack,                             // t gone into the sky
	dinoDayBack,                             // u gone into the sky
}

var (
	dinoNightBack = dvdRGB{0, 0, 0}                               // the shield's black
	dinoDayBack   = dvdRGB{247.0 / 255, 247.0 / 255, 247.0 / 255} // #F7F7F7
)

// dinoMix is shade steps of the way from night to day.
func dinoMix(night, day dvdRGB, shade int) dvdRGB {
	t := float64(shade) / (dinoShades - 1)
	// Weighted rather than night + (day-night)*t, so both ends are exact.
	return dvdRGB{
		night.r*(1-t) + day.r*t,
		night.g*(1-t) + day.g*t,
		night.b*(1-t) + day.b*t,
	}
}

// dinoShade is palette colour i at a shade.
func dinoShade(i, shade int) dvdRGB { return dinoMix(dinoNight[i], dinoDay[i], shade) }

// dinoBackground is the sky at a shade.
func dinoBackground(shade int) dvdRGB { return dinoMix(dinoNightBack, dinoDayBack, shade) }

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
	dinoArtMoon   = dinoArtCloud + 1  // 4 phases: crescent, half, gibbous, full
	dinoArtStar   = dinoArtMoon + 4   // 3 frames of a twinkle: +, x, dot
	dinoArtMeteor = dinoArtStar + 3   // 3 frames: flying, fading, almost out
	dinoArtGround = dinoArtMeteor + 3 // 3 ground tiles
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
	{"moon0", 10, 10,
		"......m..." +
			".......mm." +
			"........m." +
			"........mm" +
			"........mm" +
			"........mm" +
			"........mm" +
			"........m." +
			".......mm." +
			"......m..."},
	{"moon1", 10, 10,
		".....mm..." +
			".....mmmm." +
			".....mmmm." +
			".....mmmmm" +
			".....mmmmm" +
			".....mmmmm" +
			".....mmmmm" +
			".....mmmm." +
			".....mmmm." +
			".....mm..."},
	{"moon2", 10, 10,
		"....mmm..." +
			"..mmmmmmm." +
			"..mmmmmmm." +
			".mmmmmmmmm" +
			".mmmmmmmmm" +
			".mmmmmmmmm" +
			".mmmmmmmmm" +
			"..mmmmmmm." +
			"..mmmmmmm." +
			"....mmm..."},
	{"moon3", 10, 10,
		"...mmmm..." +
			".mmmmmmmm." +
			".mmmmmmmm." +
			"mmmmmmmmmm" +
			"mmmmmmmmmm" +
			"mmmmmmmmmm" +
			"mmmmmmmmmm" +
			".mmmmmmmm." +
			".mmmmmmmm." +
			"...mmmm..."},
	{"star0", 3, 3, ".s." + "sss" + ".s."},
	{"star1", 3, 3, "s.s" + ".s." + "s.s"},
	{"star2", 3, 3, "..." + ".s." + "..."},
	{"meteor0", 8, 4, "......uu" + "....tt.." + "..ss...." + "ss......"},
	{"meteor1", 8, 4, "........" + "....uu.." + "..tt...." + "ss......"},
	{"meteor2", 8, 4, "........" + "........" + "........" + "uu......"},
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
