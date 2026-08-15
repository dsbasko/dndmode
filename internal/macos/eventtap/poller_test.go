//go:build darwin

// White-box tests (package eventtap, not eventtap_test) for the unlock-code
// poller. Both matchAny and pollSequence are unexported and have no exported
// wrapper — an external test package could not reach them, and golangci-lint's
// `unused` checker would delete them outright before Task 6 wires them up.
//
// Everything here is pure Go: the C-side ring is replaced by a fakeRing that
// implements the same seq()/snapshot() contract, so the whole matching path
// is exercised without CGEventTap, Accessibility permissions, or a signed
// binary.

package eventtap

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// mustSequence builds a Sequence from the same string grammar the config
// uses, so the tests exercise real kVK_* keycodes rather than hand-picked
// integers that could drift from what ParseSequence produces.
func mustSequence(t *testing.T, code string) *matcher.Sequence {
	t.Helper()
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", code, err)
	}
	return matcher.NewSequence(steps)
}

// mustEvents turns the same grammar into the KeyEvent stream a user pressing
// those keys would produce, so a test's "typed" line and its "expected code"
// line are written identically and cannot silently diverge.
func mustEvents(t *testing.T, typed string) []matcher.KeyEvent {
	t.Helper()
	steps, err := hotkey.ParseSequence(typed)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", typed, err)
	}
	evs := make([]matcher.KeyEvent, len(steps))
	for i, s := range steps {
		evs[i] = matcher.KeyEvent{Modifiers: s.Modifiers, KeyCode: s.KeyCode}
	}
	return evs
}

// writeRing lays `evs` into a ringCap-sized snapshot buffer exactly the way
// the C callback would: the event with sequence number `start+i` lands in
// slot (start+i)&ringMask, overwriting whatever was there.
func writeRing(ring []matcher.KeyEvent, start uint64, evs []matcher.KeyEvent) {
	for i, ev := range evs {
		ring[(start+uint64(i))&ringMask] = ev
	}
}

// newRing returns a snapshot buffer holding `evs` starting at sequence
// `start`, plus the sequence counter value that snapshot() would return
// alongside it.
func newRing(start uint64, evs []matcher.KeyEvent) ([]matcher.KeyEvent, uint64) {
	ring := make([]matcher.KeyEvent, ringCap)
	writeRing(ring, start, evs)
	return ring, start + uint64(len(evs))
}

// ---------------------------------------------------------------------------
// clampFrom
// ---------------------------------------------------------------------------

// TestClampFrom_NoUnderflowBeforeFirstWrap is the regression test for the
// bug the "compare before subtract" ordering exists to prevent. For every
// `to` at or below ringCap-l the ring has not wrapped yet, nothing can have
// aged out, and `from` must come back untouched. The naive
// `lo := to - (ringCap - l)` ordering wraps to ~2^64 here and would clamp
// `from` above `to`, disabling the unlock code for the first ~60 keystrokes
// of every session.
func TestClampFrom_NoUnderflowBeforeFirstWrap(t *testing.T) {
	t.Parallel()

	const l = 4
	for _, to := range []uint64{0, 1, 5, 32, ringCap - l} {
		got, dropped := clampFrom(0, to, l)
		if got != 0 || dropped {
			t.Errorf("clampFrom(0, %d, %d) = (%d, %v), want (0, false) — "+
				"unsigned underflow would show up as a huge `from` here",
				to, l, got, dropped)
		}
	}
}

// TestClampFrom_ClampsOnlyWhenBehind pins the boundary and both sides of it:
// one past the no-wrap threshold the oldest readable sequence starts moving,
// and a caller that is already inside the window is left alone.
func TestClampFrom_ClampsOnlyWhenBehind(t *testing.T) {
	t.Parallel()

	const l = 4

	tests := []struct {
		name        string
		from, to    uint64
		wantFrom    uint64
		wantDropped bool
	}{
		{name: "exactly at threshold", from: 0, to: ringCap - l, wantFrom: 0, wantDropped: false},
		{name: "one past threshold", from: 0, to: ringCap - l + 1, wantFrom: 1, wantDropped: true},
		{name: "caller already inside window", from: 200, to: 220, wantFrom: 200, wantDropped: false},
		{name: "caller far behind", from: 0, to: 220, wantFrom: 220 - (ringCap - l), wantDropped: true},
		{name: "empty range", from: 10, to: 10, wantFrom: 10, wantDropped: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotFrom, gotDropped := clampFrom(tt.from, tt.to, l)
			if gotFrom != tt.wantFrom || gotDropped != tt.wantDropped {
				t.Errorf("clampFrom(%d, %d, %d) = (%d, %v), want (%d, %v)",
					tt.from, tt.to, l, gotFrom, gotDropped, tt.wantFrom, tt.wantDropped)
			}
		})
	}
}

// TestClampFrom_LongerThanRing covers the defensive branch: a code longer
// than the ring can never be assembled from a snapshot. Config validation
// and ring_guard_test.go make this unreachable in production, but the
// subtraction below it would underflow, so it must not fall through.
func TestClampFrom_LongerThanRing(t *testing.T) {
	t.Parallel()

	gotFrom, gotDropped := clampFrom(0, 10, ringCap+1)
	if gotFrom != 10 || !gotDropped {
		t.Errorf("clampFrom(0, 10, %d) = (%d, %v), want (10, true) — the "+
			"range must collapse to empty, not underflow", ringCap+1, gotFrom, gotDropped)
	}
}

// ---------------------------------------------------------------------------
// matchAny
// ---------------------------------------------------------------------------

// TestMatchAny_TableDriven walks the behaviours the sliding-window match has
// to have. Each case describes a keystroke stream starting at some sequence
// number, laid into a ring snapshot exactly as the C callback would.
func TestMatchAny_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		code  string
		typed string
		// start is the sequence number of the first typed event. Values at
		// or above ringCap exercise index wrapping.
		start uint64
		want  bool
	}{
		{
			name:  "code at end of stream",
			code:  "s w o r d",
			typed: "s w o r d",
			want:  true,
		},
		{
			name:  "garbage before the code",
			code:  "s w o r d",
			typed: "x y z s w o r d",
			want:  true,
		},
		{
			// The reason matchAny checks every tail rather than the newest
			// one. Both the code and the stray key land in the same 10ms
			// snapshot, so the newest tail is "w o r d z" and does not match.
			name:  "stray keypress after the code, same window",
			code:  "s w o r d",
			typed: "s w o r d z",
			want:  true,
		},
		{
			name:  "history shorter than the code",
			code:  "s w o r d",
			typed: "s w",
			want:  false,
		},
		{
			name:  "wrong key in the middle",
			code:  "s w o r d",
			typed: "s w q r d",
			want:  false,
		},
		{
			name:  "code wrapping the ring boundary",
			code:  "s w o r d",
			typed: "s w o r d",
			start: ringCap - 3, // occupies slots 61,62,63,0,1
			want:  true,
		},
		{
			name:  "garbage plus code wrapping the ring boundary",
			code:  "s w o r d",
			typed: "q q q s w o r d",
			start: 3*ringCap - 4,
			want:  true,
		},
		{
			name:  "self-overlapping code, complete",
			code:  "a b a b",
			typed: "a b a b",
			want:  true,
		},
		{
			name:  "self-overlapping code, one short",
			code:  "a b a b",
			typed: "b a b a",
			want:  false,
		},
		{
			// "a b a" + "b" completes the code on the 4th key while the
			// naive prefix matcher would already have consumed "a b a".
			name:  "self-overlapping code completed late",
			code:  "a b a b",
			typed: "a b a a b a b",
			want:  true,
		},
		{
			name:  "modifier steps",
			code:  "ctrl+s w cmd+z",
			typed: "q ctrl+s w cmd+z",
			want:  true,
		},
		{
			name:  "extra modifier on a bare step breaks it",
			code:  "s w o r d",
			typed: "s w o r shift+d",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := mustSequence(t, tt.code)
			ring, to := newRing(tt.start, mustEvents(t, tt.typed))
			tail := make([]matcher.KeyEvent, m.Len())

			if got := matchAny(ring, tt.start, to, tail, m); got != tt.want {
				t.Errorf("matchAny(code=%q, typed=%q, [%d,%d)) = %v, want %v",
					tt.code, tt.typed, tt.start, to, got, tt.want)
			}
		})
	}
}

// TestMatchAny_ExactLengthHistory pins the boundary the `end+1 < l` guard
// controls: the very first possible window is the one ending at sequence
// l-1, and it must be checked, not skipped. This is the "code entered as
// the first thing in the session" case.
func TestMatchAny_ExactLengthHistory(t *testing.T) {
	t.Parallel()

	m := mustSequence(t, "s w o r d")
	ring, to := newRing(0, mustEvents(t, "s w o r d"))
	tail := make([]matcher.KeyEvent, m.Len())

	if to != uint64(m.Len()) {
		t.Fatalf("fixture: to = %d, want %d", to, m.Len())
	}
	if !matchAny(ring, 0, to, tail, m) {
		t.Error("matchAny = false for a code entered as the first keystrokes " +
			"of the session; the `end+1 < l` guard must not skip the first " +
			"complete window")
	}
}

// TestMatchAny_EmptyRange pins that a range with nothing new in it reports
// no match and touches nothing — pollSequence short-circuits before calling
// matchAny in this case, but a direct caller must not get a false positive
// off stale ring contents.
func TestMatchAny_EmptyRange(t *testing.T) {
	t.Parallel()

	m := mustSequence(t, "s w o r d")
	ring, to := newRing(0, mustEvents(t, "s w o r d"))
	tail := make([]matcher.KeyEvent, m.Len())

	if matchAny(ring, to, to, tail, m) {
		t.Errorf("matchAny with from == to == %d = true, want false "+
			"(no new keystrokes to consider)", to)
	}
}

// TestMatchAny_LagDropsAgedOutEvents pins the consequence of the clamp: a
// poller that fell far behind must NOT match a code that has already aged
// out of the ring, because those slots now hold newer (or torn) records. It
// must still match a code that is inside the readable window.
func TestMatchAny_LagDropsAgedOutEvents(t *testing.T) {
	t.Parallel()

	const code = "s w o r d"

	// 200 keystrokes happened while the poller last looked at sequence 0 —
	// well past the point where the ring recycles. Only the newest
	// ringCap-l of them survive.
	const to uint64 = 200

	// buildStream lays `to` keystrokes down in stream order with the code
	// spliced in at `at`, then replays them into a ring the way the C
	// callback would. Replaying in order is what makes an aged-out code
	// actually disappear: later keystrokes recycle its slots.
	buildStream := func(t *testing.T, at uint64) []matcher.KeyEvent {
		t.Helper()
		filler := mustEvents(t, "q")[0]
		codeEvs := mustEvents(t, code)

		stream := make([]matcher.KeyEvent, to)
		for i := range stream {
			stream[i] = filler
		}
		copy(stream[at:], codeEvs)

		ring := make([]matcher.KeyEvent, ringCap)
		for i, ev := range stream {
			ring[uint64(i)&ringMask] = ev
		}
		return ring
	}

	if _, dropped := clampFrom(0, to, 5); !dropped {
		t.Fatalf("fixture: clampFrom(0, %d, 5) did not report a drop", to)
	}

	t.Run("aged out", func(t *testing.T) {
		t.Parallel()

		m := mustSequence(t, code)
		tail := make([]matcher.KeyEvent, m.Len())
		ring := buildStream(t, 100) // recycled long before the snapshot

		if matchAny(ring, 0, to, tail, m) {
			t.Error("matchAny = true for a code that aged out of the ring; " +
				"the clamp must keep the poller from reading recycled slots")
		}
	})

	t.Run("still inside the window", func(t *testing.T) {
		t.Parallel()

		m := mustSequence(t, code)
		tail := make([]matcher.KeyEvent, m.Len())
		ring := buildStream(t, to-5) // the newest five records

		if !matchAny(ring, 0, to, tail, m) {
			t.Error("matchAny = false for a code sitting in the newest records; " +
				"the clamp must not discard live history")
		}
	})
}

// TestMatchAny_DoesNotAllocate pins the "no allocations in hot path"
// contract matcher.go states: with caller-owned ring and tail buffers,
// matchAny must allocate nothing per call.
//
// NOT t.Parallel — testing.AllocsPerRun panics when called from a parallel
// test, and its measurement would be noise anyway if other tests were
// allocating alongside it.
func TestMatchAny_DoesNotAllocate(t *testing.T) {
	m := mustSequence(t, "s w o r d f i s h")
	ring, to := newRing(0, mustEvents(t, "q q q s w o r d f i s h"))
	tail := make([]matcher.KeyEvent, m.Len())

	if got := testing.AllocsPerRun(100, func() {
		matchAny(ring, 0, to, tail, m)
	}); got != 0 {
		t.Errorf("matchAny allocated %v times per run, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// pollSequence
// ---------------------------------------------------------------------------

// fakeRing stands in for the C-side keystroke ring. It keeps the logical
// keystroke stream as a flat slice and renders it into the caller's snapshot
// buffer the same way the real memcpy does, so pollSequence sees exactly the
// contract it will see in production.
//
// Every method is mutex-guarded: pollSequence calls seq/snapshot from its own
// goroutine while the test body pushes keystrokes, and `make test` runs with
// -race.
type fakeRing struct {
	mu        sync.Mutex
	events    []matcher.KeyEvent
	seqCalls  int
	snapCalls int
}

func (f *fakeRing) push(evs ...matcher.KeyEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evs...)
}

func (f *fakeRing) seq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seqCalls++
	return uint64(len(f.events))
}

func (f *fakeRing) snapshot(buf []matcher.KeyEvent) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapCalls++
	for i, ev := range f.events {
		buf[uint64(i)&ringMask] = ev
	}
	return uint64(len(f.events))
}

func (f *fakeRing) snapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapCalls
}

func (f *fakeRing) seqCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seqCalls
}

// waitStarted blocks until the poller has taken its initial sequence
// reading. pollSequence seeds lastSeq from seq() before entering the loop,
// so a push() racing that read would be classified as pre-existing input and
// never matched — the test would then fail for a reason that has nothing to
// do with the code under test.
func (f *fakeRing) waitStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * pollInterval)
	for time.Now().Before(deadline) {
		if f.seqCount() > 0 {
			return
		}
		time.Sleep(pollInterval / 10)
	}
	t.Fatalf("pollSequence did not read the sequence counter within %v of starting",
		20*pollInterval)
}

// discardLogger keeps test output clean while still exercising every
// log.Debug / log.Info call site (a nil logger would too, but through
// slog.Default(), which writes to stderr).
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// startPoller runs pollSequence in a goroutine and returns a stop function
// and a done channel closed when the goroutine returns. Cleanup stops the
// poller and fails the test if the goroutine outlives it — a leaked poller
// holds a ticker and would mask a broken shutdown path.
//
// stop() is idempotent (sync.Once) so a test can assert on shutdown timing
// itself without the cleanup closing an already-closed channel.
func startPoller(t *testing.T, f *fakeRing, m *matcher.Sequence, sink chan struct{}) (stop func(), done chan struct{}) {
	t.Helper()

	stopCh := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(stopCh) }) }

	done = make(chan struct{})
	go func() {
		defer close(done)
		pollSequence(stopCh, f.seq, f.snapshot, m, sink, discardLogger())
	}()

	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(20 * pollInterval):
			t.Errorf("pollSequence goroutine did not exit within %v after close(stop)",
				20*pollInterval)
		}
	})

	f.waitStarted(t)
	return stop, done
}

// TestPollSequence_MatchSendsExactlyOneSignal is the happy path: keystrokes
// appear in the ring, the poller notices on a later tick, sends one signal,
// and returns. "Exactly one" matters because the supervisor's exit trigger
// is read once — a poller that kept matching the same tail every tick would
// spin forever after unlocking.
func TestPollSequence_MatchSendsExactlyOneSignal(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	_, done := startPoller(t, f, m, sink)

	f.push(mustEvents(t, "q s w o r d")...)

	select {
	case <-sink:
	case <-time.After(20 * pollInterval):
		t.Fatalf("sink did not receive within %v after the code was entered", 20*pollInterval)
	}

	select {
	case <-done:
	case <-time.After(20 * pollInterval):
		t.Fatalf("pollSequence did not return after a match within %v", 20*pollInterval)
	}

	// The goroutine has returned, so no further send is possible; assert the
	// channel is empty rather than racing on a second receive.
	select {
	case <-sink:
		t.Error("sink received a second signal; a match must send exactly once")
	default:
	}
}

// TestPollSequence_NoMatch_KeepsPolling pins that non-matching input is
// consumed silently: no signal, no early return, and the goroutine still
// shuts down on stop. This is the silent-on-wrong-input stance expressed as
// a test.
func TestPollSequence_NoMatch_KeepsPolling(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	stop, done := startPoller(t, f, m, sink)

	f.push(mustEvents(t, "q w e r t y")...)
	time.Sleep(5 * pollInterval)

	select {
	case <-sink:
		t.Fatal("sink received a signal for input that is not the unlock code")
	default:
	}

	select {
	case <-done:
		t.Fatal("pollSequence returned without a match")
	default:
	}

	stop()
	select {
	case <-done:
	case <-time.After(20 * pollInterval):
		t.Fatalf("pollSequence did not exit within %v after close(stop)", 20*pollInterval)
	}
}

// TestPollSequence_StopChannel_StopsPolling is the Release() teardown path:
// closing stop must return the goroutine within a bounded time even though
// nothing was ever typed.
func TestPollSequence_StopChannel_StopsPolling(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollSequence(stop, f.seq, f.snapshot, m, sink, discardLogger())
	}()

	time.Sleep(3 * pollInterval)
	close(stop)

	select {
	case <-done:
	case <-time.After(20 * pollInterval):
		t.Fatalf("pollSequence did not exit within %v after close(stop)", 20*pollInterval)
	}

	select {
	case <-sink:
		t.Error("sink received a signal but nothing was ever typed")
	default:
	}
}

// TestPollSequence_FullSinkBuffer_DoesNotBlock pins the non-blocking send. A
// poller blocked on a full sink would never return, and Release() waits on
// it — the locked OS thread and the CGEventTap would leak, leaving the
// overlay up.
func TestPollSequence_FullSinkBuffer_DoesNotBlock(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)
	sink <- struct{}{} // pre-fill: supervisor already exiting via another path

	done := make(chan struct{})
	go func() {
		defer close(done)
		pollSequence(make(chan struct{}), f.seq, f.snapshot, m, sink, discardLogger())
	}()

	f.waitStarted(t)
	f.push(mustEvents(t, "s w o r d")...)

	select {
	case <-done:
	case <-time.After(20 * pollInterval):
		t.Fatalf("pollSequence blocked on a full sink; did not return within %v", 20*pollInterval)
	}

	if got := len(sink); got != 1 {
		t.Errorf("sink len = %d, want 1 (the pre-fill must remain; the "+
			"duplicate must be dropped)", got)
	}
}

// TestPollSequence_NoKeystrokes_SkipsSnapshot pins the early return that
// keeps the idle cost at one atomic load per tick. Ticking for many
// intervals with an unchanged counter must not trigger a single ring memcpy.
func TestPollSequence_NoKeystrokes_SkipsSnapshot(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	startPoller(t, f, m, sink)

	time.Sleep(10 * pollInterval)

	if got := f.snapshotCount(); got != 0 {
		t.Errorf("snapshot called %d times with no keystrokes, want 0 — the "+
			"`cur == lastSeq` early return must skip the memcpy", got)
	}
}

// TestPollSequence_PreExistingKeystrokes_NotReplayed pins the initial
// lastSeq = seq() read: records that predate the goroutine belong to a
// previous tap and must not be matched. Without it, re-arming a tap over a
// live ring would unlock instantly off stale input.
func TestPollSequence_PreExistingKeystrokes_NotReplayed(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o r d")...) // already in the ring before start

	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	startPoller(t, f, m, sink)

	time.Sleep(5 * pollInterval)

	select {
	case <-sink:
		t.Error("sink received a signal for keystrokes that predate the poller")
	default:
	}
}
