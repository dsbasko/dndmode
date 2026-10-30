//go:build linux

package overlay

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
)

// The sharks style: an ASCII ocean. Schools of tiny fish drawn as dashes
// swim, mill into bait balls and bolt in a wave of alarm when a shark comes
// near, and four sharks of random species, drawn cell by cell in shaded
// glyphs, patrol and hunt them. Eaten fish come back as new schools from the
// edges. Linux only, no macOS twin. Cosmetic only; the shield stays opaque.
//
// Ported from ascii-sharks (https://github.com/stackedlol/ascii-sharks):
// the simulation (world.ts, species.ts, math.ts) is here, the cell renderer
// (renderer.ts) in sharks_draw.go. The pointer and click reactions are left
// out, since the shield never reacts to input, and Math.random is a
// per-display xorshift.
//
//	MIT License, Copyright (c) 2026 drewdi
//
//	Permission is hereby granted, free of charge, to any person obtaining a
//	copy of this software and associated documentation files (the
//	"Software"), to deal in the Software without restriction, including
//	without limitation the rights to use, copy, modify, merge, publish,
//	distribute, sublicense, and/or sell copies of the Software, and to permit
//	persons to whom the Software is furnished to do so, subject to the
//	following conditions:
//
//	The above copyright notice and this permission notice shall be included
//	in all copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS
//	OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
//	MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN
//	NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
//	DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR
//	OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE
//	USE OR OTHER DEALINGS IN THE SOFTWARE.
//
// The world steps at a fixed 60 per second through an accumulator, like
// dino, and is redrawn after each display frame that stepped it.

const (
	sharksFPS      = 60 // world steps per second
	sharksStep     = 1.0 / sharksFPS
	sharksMaxSteps = 4   // steps per display frame at most; a longer stall is dropped
	sharksSnap     = 0.1 // a frame this close to a whole number of steps is that many

	sharksNodes = 20 // spine nodes per shark, snout first

	sharksFishSeparationK = 420.0
	sharksFishAlignK      = 1.4
	sharksFishCohesionK   = 1.0
	sharksFishFleeK       = 1100.0
	sharksBlindCos        = -0.6 // ~127° either side of the heading is visible
)

// The shark species. The order is the renderer's shape table.
const (
	sharkClassic = iota
	sharkHammerhead
	sharkTiger
	sharkZebra
	sharkGreatHammer
	sharkMako
	sharkThresher
	sharkNurse
	sharkKinds
)

// sharksSizes are the four sharks' lengths, relative to the biggest; the
// smaller ones read as farther away.
var sharksSizes = [...]float64{1, 0.7, 0.84, 0.77}

// sharksSpecies is one kind of fish. Different behaviour mixes give
// different collective shapes (Couzin et al. 2002): strong alignment gives
// polarised schools and ribbons, low alignment gives swarms, and a
// tangential pull gives a mill.
type sharksSpecies struct {
	cruise, maxSpeed, minSpeed float64
	weight                     float64 // relative odds of a new school being this species
	schoolMin, schoolMax       int
	spacingX, spacingY         float64 // personal space in grid cells, so every fish gets its own glyph
	align, cohesion, jitter    float64
	mill                       float64 // tangential pull around the local centre: >0 makes a bait ball
	verticalDrag               float64 // damps vertical speed; real fish mostly cruise level
}

var sharksFishSpecies = [...]sharksSpecies{
	// sardine
	{cruise: 36, maxSpeed: 64, minSpeed: 18, weight: 3, schoolMin: 18, schoolMax: 28, spacingX: 1.8, spacingY: 1.1,
		align: 1.6, cohesion: 1.6, jitter: 40, mill: 70, verticalDrag: 0.5},
	// anchovy
	{cruise: 46, maxSpeed: 78, minSpeed: 22, weight: 3, schoolMin: 20, schoolMax: 32, spacingX: 1.5, spacingY: 1,
		align: 3.2, cohesion: 0.45, jitter: 30, mill: 0, verticalDrag: 1.4},
	// silverside
	{cruise: 40, maxSpeed: 70, minSpeed: 20, weight: 3, schoolMin: 12, schoolMax: 20, spacingX: 2, spacingY: 1.1,
		align: 2.4, cohesion: 1.1, jitter: 35, mill: 0, verticalDrag: 1.6},
	// mackerel
	{cruise: 32, maxSpeed: 60, minSpeed: 16, weight: 2, schoolMin: 5, schoolMax: 9, spacingX: 2.6, spacingY: 1.3,
		align: 2, cohesion: 0.9, jitter: 25, mill: 0, verticalDrag: 2},
	// goby
	{cruise: 26, maxSpeed: 56, minSpeed: 10, weight: 1, schoolMin: 8, schoolMax: 12, spacingX: 1.4, spacingY: 1.1,
		align: 0.35, cohesion: 1.4, jitter: 110, mill: 0, verticalDrag: 0.3},
}

// sharksFish is one fish, in widget pixels with y down.
type sharksFish struct {
	x, y, vx, vy float64
	species      int
	alarm        float64 // 0 calm → 1 panicking; spreads to neighbours
	alive        bool
	respawnAt    float64
	phase        float64 // tail-beat phase
	turn         float64 // smoothed angular velocity (rad/s); hard turns glint
	depth        float64 // 0.55 far → 1 near
	millDir      float64 // ±1, the rotation shared by a school when it mills
}

// sharksShark is one shark: a follow-the-leader spine steered by wander,
// pursuit and separation.
type sharksShark struct {
	id          int
	kind        int
	length      float64
	nodes       [sharksNodes * 2]float64 // spine positions, snout first: x0, y0, x1, y1, ...
	heading     float64
	speed       float64
	phase       float64 // tail-beat phase; drives the body wave in the renderer
	turn        float64 // smoothed angular velocity (rad/s); the shark banks into turns
	hunger      float64
	wander      float64
	orbitDir    float64 // ±1
	orbitRadius float64
	target      int // index into fish, or -1
	retargetAt  float64
	depth       float64 // 0 far / dim → 1 near / bright
}

// sharksEvent is a fish eaten at (x, y); the renderer flashes it.
type sharksEvent struct{ x, y float64 }

// sharksWorld is the simulation of one display.
type sharksWorld struct {
	width, height float64
	time          float64
	fish          []sharksFish
	sharks        []sharksShark
	events        []sharksEvent
	kinds         [len(sharksSizes)]int
	rng           uint64 // xorshift64* state, never 0

	perception  float64 // fish neighbourhood radius; the spatial hash bucket matches it
	sepX, sepY  [len(sharksFishSpecies)]float64
	foreignSep  float64
	bucketCols  int
	bucketRows  int
	bucketHead  []int32
	bucketCount []uint16
	bucketNext  []int32
	lastSpawn   float64
}

// newSharksWorld picks four different shark species from seed.
func newSharksWorld(seed uint64) *sharksWorld {
	w := &sharksWorld{rng: goldSeed(seed), perception: 60, foreignSep: 40}
	kinds := make([]int, sharkKinds)
	for i := range kinds {
		kinds[i] = i
	}
	for i := len(kinds) - 1; i > 0; i-- {
		j := int(goldRand(&w.rng) % uint64(i+1))
		kinds[i], kinds[j] = kinds[j], kinds[i]
	}
	copy(w.kinds[:], kinds)
	return w
}

func (w *sharksWorld) random() float64 { return goldUniform(&w.rng) }

func (w *sharksWorld) rand(lo, hi float64) float64 { return lo + w.random()*(hi-lo) }

func (w *sharksWorld) randInt(lo, hi int) int {
	return int(math.Floor(w.rand(float64(lo), float64(hi+1))))
}

func (w *sharksWorld) randSign() float64 {
	if w.random() < 0.5 {
		return 1
	}
	return -1
}

// pickSpecies draws a fish species by weight.
func (w *sharksWorld) pickSpecies() int {
	total := 0.0
	for _, sp := range sharksFishSpecies {
		total += sp.weight
	}
	r := w.random() * total
	for i, sp := range sharksFishSpecies {
		r -= sp.weight
		if r <= 0 {
			return i
		}
	}
	return 0
}

// resize sets the world size and the grid cell size, seeding the world on
// the first call. Spacing is in grid cells so every fish lands in its own.
func (w *sharksWorld) resize(width, height, cellW, cellH float64) {
	first := w.width == 0
	w.width, w.height = width, height
	for i, sp := range sharksFishSpecies {
		w.sepX[i] = sp.spacingX * cellW
		w.sepY[i] = sp.spacingY * cellH
	}
	w.perception = max(cellH*3, cellW*5.5)
	w.foreignSep = cellW * 3
	if first || len(w.fish) == 0 {
		w.seed()
		return
	}
	for i := range w.fish {
		f := &w.fish[i]
		f.x = sharksClamp(f.x, 0, width)
		f.y = sharksClamp(f.y, 0, height)
	}
}

// seed puts the sharks on orbits round the centre and scatters the schools.
func (w *sharksWorld) seed() {
	W, H := w.width, w.height
	minSide := min(W, H)
	baseLength := sharksClamp(minSide*0.44, 170, 430)
	n := len(sharksSizes)

	w.sharks = w.sharks[:0]
	for i := range n {
		size := sharksSizes[i]
		length := baseLength * size
		orbitRadius := minSide * w.rand(0.28, 0.4)
		a := float64(i)/float64(n)*2*math.Pi + w.rand(-0.4, 0.4)
		orbitDir := 1.0
		if i%2 == 1 {
			orbitDir = -1
		}
		x := W/2 + math.Cos(a)*orbitRadius*(W/minSide)*0.9
		y := H/2 + math.Sin(a)*orbitRadius
		heading := a + orbitDir*math.Pi/2
		sh := sharksShark{
			id: i + 1, kind: w.kinds[i], length: length, heading: heading, speed: 30,
			phase: w.rand(0, 2*math.Pi), hunger: w.rand(0.25, 0.75),
			orbitDir: orbitDir, orbitRadius: orbitRadius, target: -1, depth: size,
		}
		seg := length / (sharksNodes - 1)
		for k := range sharksNodes {
			sh.nodes[k*2] = x - math.Cos(heading)*seg*float64(k)
			sh.nodes[k*2+1] = y - math.Sin(heading)*seg*float64(k)
		}
		w.sharks = append(w.sharks, sh)
	}
	// Smaller (farther) sharks draw first so the big one swims over them.
	slices.SortStableFunc(w.sharks, func(a, b sharksShark) int { return cmp.Compare(a.length, b.length) })

	total := int(sharksClamp(math.Round(W*H/4400), 85, 340))
	w.fish = w.fish[:0]
	for len(w.fish) < total {
		species := w.pickSpecies()
		sp := &sharksFishSpecies[species]
		size := min(w.randInt(sp.schoolMin, sp.schoolMax), total-len(w.fish))
		millDir := w.randSign()
		cx := w.rand(W*0.06, W*0.94)
		cy := w.rand(H*0.08, H*0.92)
		// Mostly level headings: fish cruise sideways far more than up or down.
		dir := w.rand(-0.5, 0.5)
		if w.random() >= 0.5 {
			dir += math.Pi
		}
		spread := math.Sqrt(float64(size))
		for range size {
			w.fish = append(w.fish, sharksFish{
				x:       cx + w.rand(-1, 1)*w.sepX[species]*spread,
				y:       cy + w.rand(-1, 1)*w.sepY[species]*spread*0.6,
				vx:      math.Cos(dir) * sp.cruise,
				vy:      math.Sin(dir) * sp.cruise,
				species: species,
				alive:   true,
				phase:   w.rand(0, 2*math.Pi),
				depth:   w.rand(0.55, 1),
				millDir: millDir,
			})
		}
	}
	w.bucketNext = make([]int32, len(w.fish))
}

// step advances the world by dt seconds.
func (w *sharksWorld) step(dt float64) {
	w.time += dt
	w.rebuildHash()
	w.stepFish(dt)
	w.stepSharks(dt)
	w.respawn()
}

func (w *sharksWorld) rebuildHash() {
	const pad = 2
	size := w.perception
	w.bucketCols = int(math.Ceil(w.width/size)) + pad*2
	w.bucketRows = int(math.Ceil(w.height/size)) + pad*2
	n := w.bucketCols * w.bucketRows
	if len(w.bucketHead) != n {
		w.bucketHead = make([]int32, n)
		w.bucketCount = make([]uint16, n)
	}
	for i := range w.bucketHead {
		w.bucketHead[i] = -1
		w.bucketCount[i] = 0
	}
	if len(w.bucketNext) != len(w.fish) {
		w.bucketNext = make([]int32, len(w.fish))
	}
	for i := range w.fish {
		f := &w.fish[i]
		if !f.alive {
			continue
		}
		b := w.bucketOf(f.x, f.y)
		w.bucketNext[i] = w.bucketHead[b]
		w.bucketHead[b] = int32(i)
		w.bucketCount[b]++
	}
}

func (w *sharksWorld) bucketOf(x, y float64) int {
	size := w.perception
	c := sharksClampInt(int(math.Floor(x/size))+2, 0, w.bucketCols-1)
	r := sharksClampInt(int(math.Floor(y/size))+2, 0, w.bucketRows-1)
	return r*w.bucketCols + c
}

// stepFish moves the fish: Reynolds boids (separation, alignment, cohesion)
// in Couzin-style zones with a blind spot behind each fish, fleeing sharks
// with the fountain effect, and an alarm that spreads through the school.
func (w *sharksWorld) stepFish(dt float64) {
	W, H := w.width, w.height
	R := w.perception
	R2 := R * R
	cols, rows := w.bucketCols, w.bucketRows
	calmDecay := math.Exp(-dt * 0.7)
	contagion := min(1, dt*6)

	for i := range w.fish {
		f := &w.fish[i]
		if !f.alive {
			continue
		}
		sp := &sharksFishSpecies[f.species]
		// Alarmed fish want more room: the school flashes apart around a
		// shark and closes back up once the threat fades.
		spread := 1 + 1.8*f.alarm
		sepX := w.sepX[f.species] * spread
		sepY := w.sepY[f.species] * spread

		speed := math.Hypot(f.vx, f.vy)
		if speed == 0 {
			speed = 1
		}
		hx, hy := f.vx/speed, f.vy/speed

		var ax, ay, sx, sy, avx, avy, cx, cy, neighbourAlarm float64
		n := 0
		bc := sharksClampInt(int(math.Floor(f.x/R))+2, 0, cols-1)
		br := sharksClampInt(int(math.Floor(f.y/R))+2, 0, rows-1)
		for r := max(0, br-1); r <= min(rows-1, br+1); r++ {
			for c := max(0, bc-1); c <= min(cols-1, bc+1); c++ {
				for j := w.bucketHead[r*cols+c]; j != -1; j = w.bucketNext[j] {
					if int(j) == i {
						continue
					}
					o := &w.fish[j]
					dx, dy := o.x-f.x, o.y-f.y
					d2 := dx*dx + dy*dy
					if d2 >= R2 || d2 <= 1e-6 {
						continue
					}
					d := math.Sqrt(d2)
					// Blind zone directly behind the fish.
					if (dx*hx+dy*hy)/d <= sharksBlindCos {
						continue
					}
					// Elliptical personal space: wide along x, tight along y.
					qx, qy := dx/sepX, dy/sepY
					if q2 := qx*qx + qy*qy; q2 < 1 {
						q := math.Sqrt(q2)
						if q == 0 {
							q = 1e-3
						}
						k := (1 - q) / q
						sx -= qx * k
						sy -= qy * k
					}
					if o.species == f.species {
						avx += o.vx
						avy += o.vy
						cx += dx
						cy += dy
						n++
					} else if d < w.foreignSep {
						// Different species keep their own schools.
						k := (1 - d/w.foreignSep) / d * 0.6
						sx -= dx * k
						sy -= dy * k
					}
					neighbourAlarm = max(neighbourAlarm, o.alarm)
				}
			}
		}

		ax += sx * sharksFishSeparationK
		ay += sy * sharksFishSeparationK
		if n > 0 {
			fn := float64(n)
			alignK := sharksFishAlignK * sp.align
			cohesionK := sharksFishCohesionK * sp.cohesion * (1 - 0.8*f.alarm)
			ax += (avx/fn-f.vx)*alignK + cx/fn*cohesionK
			ay += (avy/fn-f.vy)*alignK + cy/fn*cohesionK
			if sp.mill > 0 {
				// Circle the local centre: the school spins into a bait ball.
				cl := math.Hypot(cx, cy)
				if cl == 0 {
					cl = 1
				}
				ax += -cy / cl * f.millDir * sp.mill * (1 - f.alarm)
				ay += cx / cl * f.millDir * sp.mill * (1 - f.alarm)
			}
		}

		// Predators. Distance is measured to the nearest point along the
		// front half of the shark, so fish also give the flanks a wide berth.
		fear := 0.0
		for s := range w.sharks {
			sh := &w.sharks[s]
			fearR := sh.length*0.6 + 60
			best := math.Inf(1)
			var bx, by float64
			for k := 0; k <= 12; k += 3 {
				nx, ny := sh.nodes[k*2], sh.nodes[k*2+1]
				if d2 := (f.x-nx)*(f.x-nx) + (f.y-ny)*(f.y-ny); d2 < best {
					best, bx, by = d2, nx, ny
				}
			}
			d := math.Sqrt(best)
			if d >= fearR || d < 1e-3 {
				continue
			}
			wt := 1 - d/fearR
			wt *= wt
			awayX, awayY := (f.x-bx)/d, (f.y-by)/d
			// Fountain effect: break to whichever side of the shark's path you're on.
			shx, shy := math.Cos(sh.heading), math.Sin(sh.heading)
			side := 1.0
			if shx*(f.y-sh.nodes[1])-shy*(f.x-sh.nodes[0]) < 0 {
				side = -1
			}
			ax += (awayX*0.7 - shy*side*0.9) * sharksFishFleeK * wt
			ay += (awayY*0.7 + shx*side*0.9) * sharksFishFleeK * wt
			fear = max(fear, math.Sqrt(wt))
		}

		// Alarm: direct threat sets it, neighbours pass it along (slightly
		// weaker each hop), and it fades on its own.
		alarm := max(f.alarm*calmDecay, fear)
		if relayed := neighbourAlarm * 0.86; relayed > alarm {
			alarm += (relayed - alarm) * contagion
		}
		f.alarm = alarm

		// Soft walls.
		const m = 36.0
		if f.x < m {
			ax += (m - f.x) * 7
		} else if f.x > W-m {
			ax -= (f.x - (W - m)) * 7
		}
		if f.y < m {
			ay += (m - f.y) * 7
		} else if f.y > H-m {
			ay -= (f.y - (H - m)) * 7
		}

		// Ambient current and a little individual restlessness.
		curX, curY := sharksCurrent(f.x, f.y, w.time)
		ax += curX*0.5 + (w.random()-0.5)*sp.jitter
		ay += curY*0.5 + (w.random()-0.5)*sp.jitter
		// Swim level, except when bolting from danger.
		ay -= f.vy * sp.verticalDrag * (1 - 0.7*f.alarm)

		// Relax toward a preferred speed that rises with panic.
		preferred := sp.cruise * (1 + 1.3*f.alarm)
		ax += (preferred - speed) * hx * 1.6
		ay += (preferred - speed) * hy * 1.6

		before := math.Atan2(f.vy, f.vx)
		f.vx += ax * dt
		f.vy += ay * dt
		maxSpeed := sp.maxSpeed * (1 + 1.8*f.alarm)
		if s := math.Hypot(f.vx, f.vy); s > maxSpeed {
			f.vx *= maxSpeed / s
			f.vy *= maxSpeed / s
		} else if s < sp.minSpeed && s > 1e-3 {
			f.vx *= sp.minSpeed / s
			f.vy *= sp.minSpeed / s
		}
		f.turn += (sharksAngleDiff(before, math.Atan2(f.vy, f.vx))/dt - f.turn) * min(1, dt*8)
		f.phase += dt * (7 + math.Hypot(f.vx, f.vy)*0.12)
		f.x = sharksClamp(f.x+f.vx*dt, -60, W+60)
		f.y = sharksClamp(f.y+f.vy*dt, -60, H+60)
	}
}

// stepSharks moves the sharks: wander, pursuit of prey or a lazy patrol
// orbit, room for each other, a clear zone in the middle and a turn back
// whenever a snout leaves the frame. Hunger builds over time and drops
// with each fish eaten.
func (w *sharksWorld) stepSharks(dt float64) {
	W, H := w.width, w.height
	minSide := min(W, H)
	cxW, cyW := W/2, H/2

	for s := range w.sharks {
		sh := &w.sharks[s]
		L := sh.length
		nodes := &sh.nodes
		hx, hy := nodes[0], nodes[1]

		sh.hunger = min(1, sh.hunger+dt*0.03)

		// Momentum.
		dx, dy := math.Cos(sh.heading), math.Sin(sh.heading)

		// Wander: a slowly drifting offset on a circle ahead of the snout.
		sh.wander = sharksClamp(sh.wander+(w.random()-0.5)*dt*3.2, -1.1, 1.1)
		dx += math.Cos(sh.heading+sh.wander) * 0.45
		dy += math.Sin(sh.heading+sh.wander) * 0.45

		// Hunt.
		if w.time >= sh.retargetAt {
			sh.target = w.findPrey(sh)
			sh.retargetAt = w.time + 0.45
		}
		hunting := false
		closing := 0.0
		if sh.target >= 0 && w.fish[sh.target].alive && sh.hunger > 0.3 {
			t := &w.fish[sh.target]
			// Pursuit: aim at where the fish is about to be.
			tx := t.x + t.vx*0.35 - hx
			ty := t.y + t.vy*0.35 - hy
			d := math.Hypot(tx, ty)
			if d == 0 {
				d = 1
			}
			wt := 1.4 + 2.6*sh.hunger
			dx += tx / d * wt
			dy += ty / d * wt
			hunting = true
			closing = sharksClamp(1-d/(L*0.9), 0, 1)
		} else {
			// Patrol: a lazy circuit round the centre.
			rx := (hx - cxW) / (W / minSide)
			ry := hy - cyW
			r := math.Hypot(rx, ry)
			if r == 0 {
				r = 1
			}
			radius := sh.orbitRadius * (1 + 0.15*math.Sin(w.time*0.05+float64(sh.id)))
			radial := sharksClamp((radius-r)/radius, -1, 1)
			dx += (-ry/r*sh.orbitDir + rx/r*radial*0.9) * 0.55
			dy += (rx/r*sh.orbitDir + ry/r*radial*0.9) * 0.55
		}

		// Give other sharks room.
		for o := range w.sharks {
			if o == s {
				continue
			}
			other := &w.sharks[o]
			for k := 0; k < sharksNodes; k += 4 {
				ox, oy := hx-other.nodes[k*2], hy-other.nodes[k*2+1]
				d := math.Hypot(ox, oy)
				if rng := (L + other.length) * 0.45; d < rng && d > 1e-3 {
					wt := (1 - d/rng) * 3
					dx += ox / d * wt
					dy += oy / d * wt
				}
			}
		}

		// Keep the middle mostly clear: a soft elliptical no-swim zone.
		ex := (hx - cxW) / (W * 0.3)
		ey := (hy - cyW) / (H * 0.16)
		if e2 := ex*ex + ey*ey; e2 < 1 {
			e := math.Sqrt(e2)
			if e == 0 {
				e = 1e-3
			}
			wt := 1 - e
			if !hunting {
				wt *= 2.5
			}
			dx += ex / e * wt
			dy += ey / e * wt
		}

		// Turn back once the snout leaves the frame, so bodies dip off-screen
		// now and then but the sharks never wander away for good.
		if over := max(-hx, hx-W, -hy, hy-H); over > -L*0.05 {
			wt := sharksClamp((over+L*0.05)/(L*0.35), 0, 1) * 3.5
			toX, toY := cxW-hx, cyW-hy
			d := math.Hypot(toX, toY)
			if d == 0 {
				d = 1
			}
			dx += toX / d * wt
			dy += toY / d * wt
		}

		desired := math.Atan2(dy, dx)
		// Sharks can't pivot on the spot: the turning circle grows with speed.
		agility := 0.8
		if hunting {
			agility = 1.7 + closing
		}
		agility = min(agility, sh.speed/(L*0.22))
		maxTurn := agility * dt
		turned := sharksClamp(sharksAngleDiff(sh.heading, desired), -maxTurn, maxTurn)
		sh.heading += turned
		sh.turn += (turned/dt - sh.turn) * min(1, dt*3)

		cruise := 30 + L*0.04
		targetSpeed := cruise
		if hunting {
			targetSpeed = cruise * (1.45 + closing*1.5)
		}
		sh.speed += (targetSpeed - sh.speed) * min(1, dt*1.1)
		sh.phase += dt * (2 + sh.speed/24)

		nodes[0] += math.Cos(sh.heading) * sh.speed * dt
		nodes[1] += math.Sin(sh.heading) * sh.speed * dt

		// Follow-the-leader spine, with a stiffness limit per joint so the
		// body bends into a C at most and never folds back on itself.
		seg := L / (sharksNodes - 1)
		const maxBend = 0.16
		prevAngle := sh.heading + math.Pi
		for k := 1; k < sharksNodes; k++ {
			px, py := nodes[(k-1)*2], nodes[(k-1)*2+1]
			angle := math.Atan2(nodes[k*2+1]-py, nodes[k*2]-px)
			angle = prevAngle + sharksClamp(sharksAngleDiff(prevAngle, angle), -maxBend, maxBend)
			nodes[k*2] = px + math.Cos(angle)*seg
			nodes[k*2+1] = py + math.Sin(angle)*seg
			prevAngle = angle
		}

		// Feed.
		if sh.hunger > 0.15 {
			mouthR := L*0.05 + 8
			mx, my := nodes[0], nodes[1]
			for i := range w.fish {
				f := &w.fish[i]
				if !f.alive {
					continue
				}
				if (f.x-mx)*(f.x-mx)+(f.y-my)*(f.y-my) < mouthR*mouthR {
					f.alive = false
					f.respawnAt = w.time + w.rand(3, 8)
					sh.hunger = max(0, sh.hunger-0.16)
					w.events = append(w.events, sharksEvent{f.x, f.y})
				}
			}
		}
	}
}

// findPrey picks prey in the shark's forward cone, preferring close fish in
// sparse buckets (stragglers) over fish buried in a dense school, since
// dense schools confuse predators. It returns a fish index or -1.
func (w *sharksWorld) findPrey(sh *sharksShark) int {
	rng := sh.length*0.9 + 160
	hx, hy := sh.nodes[0], sh.nodes[1]
	fx, fy := math.Cos(sh.heading), math.Sin(sh.heading)
	best := -1
	bestScore := math.Inf(1)
	currentScore := math.Inf(1)

	for i := range w.fish {
		f := &w.fish[i]
		if !f.alive {
			continue
		}
		dx, dy := f.x-hx, f.y-hy
		d := math.Hypot(dx, dy)
		if d > rng || d < 1e-3 {
			continue
		}
		cos := (dx*fx + dy*fy) / d
		if cos < 0.2 {
			continue
		}
		crowd := float64(w.bucketCount[w.bucketOf(f.x, f.y)])
		score := d * (1 + (1-cos)*0.8) * (1 + crowd*0.07)
		if score < bestScore {
			bestScore, best = score, i
		}
		if i == sh.target {
			currentScore = score
		}
	}
	// Stick with the current target unless something is clearly better.
	if sh.target >= 0 && currentScore < bestScore*1.3 {
		return sh.target
	}
	return best
}

// respawn brings eaten fish back as a new school swimming in from a random
// edge, which keeps the population steady.
func (w *sharksWorld) respawn() {
	W, H := w.width, w.height
	due := 0
	for i := range w.fish {
		if f := &w.fish[i]; !f.alive && f.respawnAt <= w.time {
			due++
		}
	}
	if due == 0 || due < 6 && w.time-w.lastSpawn < 9 {
		return
	}
	w.lastSpawn = w.time

	side := w.randInt(0, 3)
	along := w.rand(0.15, 0.85)
	x, y := along*W, along*H
	switch side {
	case 0:
		x = -20
	case 1:
		x = W + 20
	case 2:
		y = -20
	case 3:
		y = H + 20
	}
	inward := math.Atan2(H/2-y, W/2-x) + w.rand(-0.5, 0.5)
	species := w.pickSpecies()
	sp := &sharksFishSpecies[species]
	millDir := w.randSign()
	left := sp.schoolMax
	for i := range w.fish {
		f := &w.fish[i]
		if f.alive || f.respawnAt > w.time {
			continue
		}
		if left == 0 {
			break
		}
		left--
		f.alive = true
		f.species = species
		f.alarm = 0
		f.turn = 0
		f.millDir = millDir
		f.x = x + w.rand(-1, 1)*w.perception
		f.y = y + w.rand(-1, 1)*w.perception
		f.vx = math.Cos(inward) * sp.cruise * 1.4
		f.vy = math.Sin(inward) * sp.cruise * 1.4
	}
}

// --- view ---

// sharksView is the per-window state: the world, stepped at sharksFPS from
// the frame clock, and the cells drawn after the last step.
type sharksView struct {
	world   *sharksWorld
	field   sharksField
	seeded  bool
	acc     float64 // world steps not yet taken
	changed bool    // the cells differ from the ones last drawn

	boundsW, boundsH float64
}

// tick advances by dt seconds for a widget of the given size: the first
// tick with a size seeds the world, a resize lays it out again, and the
// world steps once each sixtieth of a second. A display frame close to a
// whole number of steps counts as exactly that, as in dino; after a stall
// at most sharksMaxSteps are taken and the rest dropped.
func (v *sharksView) tick(w, h, dt float64) {
	if w <= 0 || h <= 0 {
		return
	}
	if !v.seeded {
		v.seed(rand.Uint64())
	}
	if w != v.boundsW || h != v.boundsH {
		v.layout(w, h)
		return
	}
	frames := min(max(dt, 0), animMaxDt) * sharksFPS
	if n := math.Round(frames); n >= 1 && math.Abs(frames-n) < sharksSnap {
		frames = n
	}
	v.acc += frames
	steps := 0
	for v.acc >= 1 && steps < sharksMaxSteps {
		v.world.step(sharksStep)
		v.acc--
		steps++
	}
	if steps == sharksMaxSteps {
		v.acc = 0
	}
	if steps > 0 {
		v.field.render(v.world, float64(steps)*sharksStep)
		v.changed = true
	}
}

// seed makes the world; it is laid out on the first tick with a size.
func (v *sharksView) seed(seed uint64) {
	v.world = newSharksWorld(seed)
	v.seeded = true
}

// layout sizes the grid and the world for the widget and draws the first
// cells.
func (v *sharksView) layout(w, h float64) {
	v.boundsW, v.boundsH = w, h
	v.field.resize(w, h)
	v.world.resize(w, h, v.field.cw, v.field.ch)
	v.field.render(v.world, sharksStep)
	v.acc = 0
	v.changed = true
}

// --- helpers ---

func sharksClamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

func sharksClampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

func sharksSmoothstep(e0, e1, x float64) float64 {
	t := sharksClamp((x-e0)/(e1-e0), 0, 1)
	return t * t * (3 - 2*t)
}

// sharksAngleDiff is the shortest signed angle from a to b, in (-π, π].
func sharksAngleDiff(a, b float64) float64 {
	d := math.Mod(b-a, 2*math.Pi)
	if d > math.Pi {
		d -= 2 * math.Pi
	}
	if d < -math.Pi {
		d += 2 * math.Pi
	}
	return d
}

// sharksHash3 is a cheap integer hash to [0, 1), deterministic so textures
// stay stable. The arguments wrap to 32 bits like JavaScript's x | 0.
func sharksHash3(x, y, z int) float64 {
	h := uint32(int32(x))*374761393 ^ uint32(int32(y))*668265263 ^ uint32(int32(z))*1442695041
	h = (h ^ h>>13) * 1274126177
	h ^= h >> 16
	return float64(h) / 4294967296
}

const (
	sharksK1           = 1.0 / 260
	sharksK2           = 1.0 / 310
	sharksK3           = 1.0 / 190
	sharksCurrentScale = 3400
)

// sharksCurrent is a divergence-free ocean current: the curl of a slowly
// drifting stream function ψ(x, y, t). Having no sources or sinks, it moves
// the schools around without bunching them up in one corner.
func sharksCurrent(x, y, t float64) (u, v float64) {
	a := x*sharksK1 + t*0.07
	b := y*sharksK2 - t*0.05
	c := (x+y)*sharksK3 + t*0.11
	// u = ∂ψ/∂y, v = -∂ψ/∂x with ψ = sin(a)cos(b) + 0.5 sin(c)
	u = (-sharksK2*math.Sin(a)*math.Sin(b) + 0.5*sharksK3*math.Cos(c)) * sharksCurrentScale
	v = (-sharksK1*math.Cos(a)*math.Cos(b) - 0.5*sharksK3*math.Cos(c)) * sharksCurrentScale
	return u, v
}
