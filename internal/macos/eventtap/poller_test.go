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
	"strings"
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
			tail := make([]matcher.KeyEvent, m.MaxLen())

			if got := matchAny(ring, tt.start, tt.start, to, tail, m); got != tt.want {
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
	tail := make([]matcher.KeyEvent, m.MaxLen())

	if to != uint64(m.Len()) {
		t.Fatalf("fixture: to = %d, want %d", to, m.Len())
	}
	if !matchAny(ring, 0, 0, to, tail, m) {
		t.Error("matchAny = false for a code entered as the first keystrokes " +
			"of the session; the `end+1 < l` guard must not skip the first " +
			"complete window")
	}
}

// TestMatchAny_WindowSpanningBaseline pins the floor the baseline puts
// under the window START, not just under the window end. A poller re-armed
// over a ring that still holds a previous tap's keystrokes is handed `base`
// = the press count at rearm; a window that begins below it splices stale
// records onto fresh ones and must never match, however convincing the
// concatenation looks.
//
// The fixture is the exact failure it prevents: `s w o r` recorded before
// the rearm and a single `d` after it. Every record is genuinely in the
// ring, the five are consecutive, and the concatenation IS the unlock
// code — only the baseline separates them.
//
// Production never reaches this case (Install samples the baseline over a
// freshly zeroed ring, so `base` is 0 and the floor is inert), which is
// precisely why it needs a test: nothing else would notice if the guarantee
// pollSequence documents stopped holding.
func TestMatchAny_WindowSpanningBaseline(t *testing.T) {
	t.Parallel()

	const code = "s w o r d"
	// base is the press count at rearm: the first four presses predate it,
	// the fifth is the only fresh one.
	const base uint64 = 4

	m := mustSequence(t, code)
	tail := make([]matcher.KeyEvent, m.MaxLen())
	ring, to := newRing(0, mustEvents(t, code))

	if matchAny(ring, base, base, to, tail, m) {
		t.Errorf("matchAny(base=%d) = true for a window starting at 0 — four "+
			"of the five records predate the baseline and must not be "+
			"spliced onto the fresh one", base)
	}

	// Control: the same ring over the same range DOES match when the
	// baseline says every record belongs to this session. Without it the
	// assertion above would also pass if matchAny had simply gone deaf.
	if !matchAny(ring, 0, base, to, tail, m) {
		t.Error("matchAny(base=0) = false for the same ring and range — the " +
			"fixture must be a real match that only the baseline suppresses")
	}

	// And the floor suppresses spliced windows, not the poller: a code typed
	// entirely after the baseline still matches.
	fresh, freshTo := newRing(base, mustEvents(t, code))
	if !matchAny(fresh, base, base, freshTo, tail, m) {
		t.Error("matchAny = false for a code typed entirely after the " +
			"baseline; a re-armed poller must still unlock")
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
	tail := make([]matcher.KeyEvent, m.MaxLen())

	if matchAny(ring, 0, to, to, tail, m) {
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
		tail := make([]matcher.KeyEvent, m.MaxLen())
		ring := buildStream(t, 100) // recycled long before the snapshot

		if matchAny(ring, 0, 0, to, tail, m) {
			t.Error("matchAny = true for a code that aged out of the ring; " +
				"the clamp must keep the poller from reading recycled slots")
		}
	})

	t.Run("still inside the window", func(t *testing.T) {
		t.Parallel()

		m := mustSequence(t, code)
		tail := make([]matcher.KeyEvent, m.MaxLen())
		ring := buildStream(t, to-5) // the newest five records

		if !matchAny(ring, 0, 0, to, tail, m) {
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
	tail := make([]matcher.KeyEvent, m.MaxLen())

	if got := testing.AllocsPerRun(100, func() {
		matchAny(ring, 0, 0, to, tail, m)
	}); got != 0 {
		t.Errorf("matchAny allocated %v times per run, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// matchAny — Verifier generalisation
// ---------------------------------------------------------------------------

// digestSalt is a fixed, non-secret salt so the digest fixtures below are
// deterministic. Production salts come from crypto/rand; a constant here
// only has to be SaltLen bytes long.
var digestSalt = []byte("0123456789abcdef")

// mustDigest builds a *matcher.Digest over the same string grammar
// mustSequence uses, so a Digest case and a Sequence case describing the
// same secret are written identically and cannot silently diverge.
//
// This is the verifier `--set-password` produces: it stores a salted hash
// and NOT the length, so it admits every window from 1 to hotkey.MaxSteps
// and lets the hash pick.
func mustDigest(t *testing.T, code string) *matcher.Digest {
	t.Helper()
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", code, err)
	}
	d, err := matcher.NewDigest(digestSalt, matcher.HashSteps(digestSalt, steps))
	if err != nil {
		t.Fatalf("NewDigest(%q): %v", code, err)
	}
	return d
}

// mustEventsMulti concatenates the event streams of several grammar
// fragments. It exists because hotkey.ParseSequence enforces the
// hotkey.MaxSteps ceiling on the WHOLE string, so a fixture that types
// filler in front of a maximum-length code cannot be written as one string
// — the typed stream is longer than any legal unlock code, which is exactly
// the situation the ring clamp has to survive. Empty fragments are skipped.
func mustEventsMulti(t *testing.T, parts ...string) []matcher.KeyEvent {
	t.Helper()
	var out []matcher.KeyEvent
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, mustEvents(t, p)...)
	}
	return out
}

// layStream lays `n` filler keystrokes down in stream ORDER with `evs`
// spliced in at sequence `at`, then replays the whole stream into a ring
// snapshot the way the C callback would. Replaying in order is what makes an
// aged-out window actually disappear: later keystrokes recycle its slots.
func layStream(t *testing.T, n, at uint64, evs []matcher.KeyEvent) []matcher.KeyEvent {
	t.Helper()
	filler := mustEvents(t, "q")[0]

	stream := make([]matcher.KeyEvent, n)
	for i := range stream {
		stream[i] = filler
	}
	copy(stream[at:], evs)

	ring := make([]matcher.KeyEvent, ringCap)
	for i, ev := range stream {
		ring[uint64(i)&ringMask] = ev
	}
	return ring
}

// maxStepsCode is a hotkey.MaxSteps-long unlock code: 26 letters plus six
// digits. It exists to exercise the longest window matchAny will ever
// assemble, which for a *Digest is also the window the ring clamp is
// computed from.
const maxStepsCode = "a b c d e f g h i j k l m n o p q r s t u v w x y z 1 2 3 4 5 6"

// TestMatchAny_Digest_TableDriven is TestMatchAny_TableDriven's sibling for
// the length-agnostic verifier. Every behaviour the sliding window has to
// have must survive not knowing how long the secret is: the hash is what
// rejects a wrong window, and it has to reject 31 wrong lengths per end
// position without ever producing a false positive.
func TestMatchAny_Digest_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		// pre is typed before `typed` and parsed separately, so a fixture
		// may exceed hotkey.MaxSteps in total even though no single fragment
		// does.
		pre   string
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
			// Same reason as for a Sequence: both the code and the stray key
			// land in one 10ms snapshot, so the newest tail is "code + z".
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
			// The case a Digest could plausibly get wrong and a Sequence
			// cannot: "s w" IS a prefix of the secret, and the verifier does
			// not know the secret is five steps long. The step count inside
			// the preimage is what makes the short window hash to something
			// else instead of matching a prefix.
			name:  "prefix of the code is not the code",
			code:  "s w o r d",
			typed: "q s w",
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
		{
			// The longest window the grammar admits, entered as the very
			// first keystrokes of the session: `to` is 32, which is below
			// ringCap, so this is also the case the "compare, THEN subtract"
			// ordering in clampFrom protects.
			name:  "maximum-length code as the whole history",
			code:  maxStepsCode,
			typed: maxStepsCode,
			want:  true,
		},
		{
			// A maximum-length code whose window starts one slot inside the
			// oldest readable record — exactly the boundary clampFrom pins.
			name:  "maximum-length code preceded by filler",
			code:  maxStepsCode,
			pre:   "q q q",
			typed: maxStepsCode,
			want:  true,
		},
		{
			name:  "maximum-length code wrapping the ring boundary",
			code:  maxStepsCode,
			typed: maxStepsCode,
			start: ringCap - 5,
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := mustDigest(t, tt.code)
			ring, to := newRing(tt.start, mustEventsMulti(t, tt.pre, tt.typed))
			tail := make([]matcher.KeyEvent, d.MaxLen())

			if got := matchAny(ring, tt.start, tt.start, to, tail, d); got != tt.want {
				t.Errorf("matchAny(digest(%q), typed=%q %q, [%d,%d)) = %v, want %v",
					tt.code, tt.pre, tt.typed, tt.start, to, got, tt.want)
			}
		})
	}
}

// TestMatchAny_Digest_WindowSpanningBaseline is
// TestMatchAny_WindowSpanningBaseline for the length loop. The baseline
// floor has to be re-checked for EVERY candidate length, not once per end
// position: at a given end a 1-record window can sit entirely above the
// baseline while the 5-record one reaches below it, and the loop must reject
// only the latter.
func TestMatchAny_Digest_WindowSpanningBaseline(t *testing.T) {
	t.Parallel()

	const code = "s w o r d"
	// The first four presses predate the rearm; the fifth is the only fresh
	// one. Their concatenation IS the secret — only the baseline separates
	// them.
	const base uint64 = 4

	d := mustDigest(t, code)
	tail := make([]matcher.KeyEvent, d.MaxLen())
	ring, to := newRing(0, mustEvents(t, code))

	if matchAny(ring, base, base, to, tail, d) {
		t.Errorf("matchAny(base=%d) = true for a window starting at 0 — four "+
			"of the five records belong to a previous tap and must not be "+
			"spliced onto the fresh one", base)
	}

	// Control: the same ring and range DO match when every record belongs to
	// this session, so the assertion above cannot pass by the matcher having
	// gone deaf.
	if !matchAny(ring, 0, base, to, tail, d) {
		t.Error("matchAny(base=0) = false for the same ring and range — the " +
			"fixture must be a real match that only the baseline suppresses")
	}

	// And the floor suppresses spliced windows, not the poller: a code typed
	// entirely after the baseline still matches.
	fresh, freshTo := newRing(base, mustEvents(t, code))
	if !matchAny(fresh, base, base, freshTo, tail, d) {
		t.Error("matchAny = false for a code typed entirely after the " +
			"baseline; a re-armed poller must still unlock")
	}
}

// TestMatchAny_ClampedByMaxLen is the regression test for the bound that
// changed when Verifier arrived: clampFrom is fed the verifier's WORST-CASE
// window length, not the length of the window being assembled.
//
// The fixture puts the same five-record secret in the same ring, in the same
// range, and asks two verifiers about it. The window ends at sequence 150
// with the snapshot taken at 200:
//
//	clamp by 5  → 200 - (64-5)  = 141 ≤ 150 → visible
//	clamp by 32 → 200 - (64-32) = 168 > 150 → refused
//
// So the *Sequence (MaxLen 5) matches and the *Digest (MaxLen 32) must not.
// Had matchAny clamped per candidate length, the Digest would have been told
// the same optimistic 141 while ALSO assembling 32-record windows whose start
// reaches sequence 119 — slots the writer recycled long before the snapshot.
// Matching on recycled records means unlocking on a keystroke stream the user
// never typed.
func TestMatchAny_ClampedByMaxLen(t *testing.T) {
	t.Parallel()

	const code = "s w o r d"
	const to uint64 = 200
	const codeEnd uint64 = 150

	evs := mustEvents(t, code)
	at := codeEnd + 1 - uint64(len(evs))

	seqM := mustSequence(t, code)
	dig := mustDigest(t, code)

	// Fixture sanity: the two clamps must genuinely straddle the window, or
	// the test proves nothing.
	if lo, _ := clampFrom(0, to, uint64(seqM.MaxLen())); lo > codeEnd {
		t.Fatalf("fixture: clamp by MaxLen=%d gives %d, which already excludes "+
			"the window ending at %d", seqM.MaxLen(), lo, codeEnd)
	}
	if lo, _ := clampFrom(0, to, uint64(dig.MaxLen())); lo <= codeEnd {
		t.Fatalf("fixture: clamp by MaxLen=%d gives %d, which does not exclude "+
			"the window ending at %d", dig.MaxLen(), lo, codeEnd)
	}

	ring := layStream(t, to, at, evs)

	t.Run("sequence sees its own narrow window", func(t *testing.T) {
		tail := make([]matcher.KeyEvent, seqM.MaxLen())
		if !matchAny(ring, 0, 0, to, tail, seqM) {
			t.Error("matchAny = false for a *Sequence whose only admissible " +
				"window is inside the clamp; the fixture must be a real match")
		}
	})

	t.Run("digest refuses the possibly-recycled window", func(t *testing.T) {
		tail := make([]matcher.KeyEvent, dig.MaxLen())
		if matchAny(ring, 0, 0, to, tail, dig) {
			t.Errorf("matchAny = true for a *Digest over a window ending at %d, "+
				"which sits below the MaxLen=%d clamp at %d — the clamp must be "+
				"computed from the worst-case window length, not the candidate's",
				codeEnd, dig.MaxLen(), to-(uint64(ringCap)-uint64(dig.MaxLen())))
		}
	})

	t.Run("digest still matches inside the clamp", func(t *testing.T) {
		// Same stream, secret moved into the region every candidate length
		// can reach. Without this the test above would also pass if the
		// Digest path had simply stopped working.
		const freshEnd uint64 = 195
		fresh := layStream(t, to, freshEnd+1-uint64(len(evs)), evs)
		tail := make([]matcher.KeyEvent, dig.MaxLen())
		if !matchAny(fresh, 0, 0, to, tail, dig) {
			t.Errorf("matchAny = false for a *Digest over a window ending at %d, "+
				"which is inside the clamp; the clamp must not discard live history",
				freshEnd)
		}
	})
}

// matchAnyLegacy is the pre-Verifier matchAny, kept verbatim as the oracle
// for TestMatchAny_Sequence_MatchesLegacyAlgorithm. It takes a concrete
// *matcher.Sequence, clamps by that one length, and checks exactly one
// window per end position.
func matchAnyLegacy(ring []matcher.KeyEvent, base, from, to uint64, tail []matcher.KeyEvent, m *matcher.Sequence) bool {
	l := uint64(m.Len())
	from, _ = clampFrom(from, to, l)

	for end := from; end < to; end++ {
		if end+1 < base+l {
			continue
		}
		start := end + 1 - l
		for i := uint64(0); i < l; i++ {
			tail[i] = ring[(start+i)&ringMask]
		}
		if m.MatchTail(tail) {
			return true
		}
	}
	return false
}

// TestMatchAny_Sequence_MatchesLegacyAlgorithm pins that generalising
// matchAny over Verifier did not change what it decides for the plaintext
// unlock code — the path every existing config takes. A *Sequence reports
// MinLen == MaxLen, so the length loop must collapse to the single iteration
// the old code did, the clamp must be fed the same number, and the baseline
// floor must fire on the same windows.
//
// The oracle is the old implementation itself rather than a table of
// expected booleans: a table would only pin the cases someone thought to
// write down, while cross-checking the two implementations over a corpus
// covers wrapping, clamping and baseline interactions no one enumerated.
func TestMatchAny_Sequence_MatchesLegacyAlgorithm(t *testing.T) {
	t.Parallel()

	codes := []string{"s w o r d", "a b a b", "ctrl+s w cmd+z", "q", maxStepsCode}
	// Chunked rather than flat: the last entry is longer than
	// hotkey.MaxSteps overall, which ParseSequence rejects as a unlock code
	// but which the ring must still handle as a keystroke STREAM.
	typings := [][]string{
		{"s w o r d"},
		{"x y z s w o r d"},
		{"s w o r d z"},
		{"s w"},
		{"s w q r d"},
		{"a b a a b a b"},
		{"b a b a"},
		{"q ctrl+s w cmd+z"},
		{"s w o r shift+d"},
		{"q"},
		{maxStepsCode},
		{"q q q", maxStepsCode},
	}
	// 0 and 3 stay inside the first lap; ringCap-3 and 3*ringCap-4 wrap the
	// index; 200 is far enough past ringCap that the clamp engages.
	starts := []uint64{0, 3, ringCap - 3, 3*ringCap - 4, 200}

	for _, code := range codes {
		m := mustSequence(t, code)
		for _, parts := range typings {
			typed := strings.Join(parts, " ")
			evs := mustEventsMulti(t, parts...)
			for _, start := range starts {
				ring, to := newRing(start, evs)
				// Baselines: at the start of the stream (production), a few
				// records in (re-armed poller), and past the whole stream.
				for _, base := range []uint64{0, start, start + 2, to} {
					// Lower bounds: the caller's own baseline, zero (a
					// poller far behind), and `to` (empty range).
					for _, from := range []uint64{start, 0, to} {
						gotTail := make([]matcher.KeyEvent, m.MaxLen())
						wantTail := make([]matcher.KeyEvent, m.Len())

						got := matchAny(ring, base, from, to, gotTail, m)
						want := matchAnyLegacy(ring, base, from, to, wantTail, m)
						if got != want {
							t.Errorf("matchAny(code=%q, typed=%q, start=%d, base=%d, from=%d, to=%d) = %v, "+
								"legacy = %v — the Sequence path must be unchanged",
								code, typed, start, base, from, to, got, want)
						}
					}
				}
			}
		}
	}
}

// TestMatchAny_EmptyVerifier_NeverMatches pins the second layer of the
// ErrEmptyUnlockCode guard. matcher.NewSequence(nil) is a non-nil Verifier
// that reports MaxLen 0, so a `v == nil` check alone would let it through;
// offering it a zero-length window would then hand matcher.Sequence an empty
// tail. That has to read as "no secret is configured", never as "the empty
// tail matched" — the latter is an unlock on the first tick with no input at
// all.
//
// `tail` is deliberately nil: an empty verifier must return before it
// touches the scratch buffer, so a regression here is a panic rather than a
// quiet wrong answer.
func TestMatchAny_EmptyVerifier_NeverMatches(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		steps []hotkey.Spec
	}{
		{name: "nil steps", steps: nil},
		{name: "empty steps", steps: []hotkey.Spec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v := matcher.NewSequence(tc.steps)
			if got := v.MaxLen(); got != 0 {
				t.Fatalf("NewSequence(%v).MaxLen() = %d, want 0 — the "+
					"installInternal guard and the matchAny short-circuit "+
					"both key off this", tc.steps, got)
			}

			ring, to := newRing(0, mustEvents(t, "s w o r d"))
			if matchAny(ring, 0, 0, to, nil, v) {
				t.Error("matchAny = true for an empty verifier over a ring " +
					"full of keystrokes")
			}

			// And over an untouched ring, which is what the very first ticks
			// of a session see.
			if matchAny(make([]matcher.KeyEvent, ringCap), 0, 0, 5, nil, v) {
				t.Error("matchAny = true for an empty verifier over an empty " +
					"ring — the empty tail must not read as a match")
			}
		})
	}
}

// TestMatchAny_Digest_DoesNotAllocate extends the "no allocations in hot
// path" contract to the length loop. A *Digest hashes up to hotkey.MaxSteps
// windows per end position, and every one of those hashes has to build its
// preimage on the stack: an allocation here would be paid on the poller's
// 10ms tick for as long as the mode is up.
//
// NOT t.Parallel — testing.AllocsPerRun panics when called from a parallel
// test, and its measurement would be noise anyway if other tests were
// allocating alongside it.
func TestMatchAny_Digest_DoesNotAllocate(t *testing.T) {
	d := mustDigest(t, "s w o r d f i s h")
	ring, to := newRing(0, mustEvents(t, "q q q s w o r d f i s h"))
	tail := make([]matcher.KeyEvent, d.MaxLen())

	if got := testing.AllocsPerRun(100, func() {
		matchAny(ring, 0, 0, to, tail, d)
	}); got != 0 {
		t.Errorf("matchAny allocated %v times per run with a *Digest, want 0", got)
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

// There is deliberately no waitStarted helper here any more. It used to
// block until the poller had taken its initial seq() reading, because
// pollSequence seeded lastSeq itself and a push() racing that read would be
// misclassified as pre-existing input. The baseline is now a parameter,
// sampled by the caller BEFORE the goroutine starts, so no push can race it
// and there is nothing to wait for. Reintroducing such a helper would be a
// sign the parameter had been turned back into an in-goroutine read — which
// is precisely the production race (a code typed before the poller is
// scheduled, silently swallowed) that moving it out closed.

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
//
// The baseline is sampled here, synchronously, before the goroutine is
// launched — the same discipline Install follows (it reads the counter while
// the tap source is not yet attached to a run loop). That is what makes
// "everything pushed before startPoller is pre-existing, everything pushed
// after is this session's input" a deterministic property of these tests
// rather than a race against the Go scheduler.
func startPoller(t *testing.T, f *fakeRing, v matcher.Verifier, sink chan struct{}) (stop func(), done chan struct{}) {
	t.Helper()

	stopCh := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(stopCh) }) }

	baseSeq := f.seq()

	done = make(chan struct{})
	go func() {
		defer close(done)
		pollSequence(stopCh, baseSeq, f.seq, f.snapshot, v, sink, discardLogger())
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

// TestPollSequence_DigestVerifier_MatchSendsSignal walks the hashed secret
// end to end through the poller, which is the path a config written by
// `--set-password` takes. It covers what the matchAny tests cannot: that
// pollSequence sizes its scratch buffer from MaxLen rather than from the
// secret's real length. A buffer sized from MinLen would be one record long
// and matchAny would index past it on the second candidate length — a panic
// on the poller goroutine, i.e. a machine that stays shielded.
func TestPollSequence_DigestVerifier_MatchSendsSignal(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	d := mustDigest(t, "s w o r d")
	sink := make(chan struct{}, 1)

	_, done := startPoller(t, f, d, sink)

	f.push(mustEvents(t, "q s w o r d")...)

	select {
	case <-sink:
	case <-time.After(20 * pollInterval):
		t.Fatal("no signal within 20 ticks after the hashed unlock code was typed")
	}

	select {
	case <-done:
	case <-time.After(20 * pollInterval):
		t.Fatal("pollSequence did not return after signalling a match")
	}
}

// TestPollSequence_DigestVerifier_WrongCodeKeepsPolling is the negative half:
// a Digest offers hotkey.MaxSteps candidate windows per keystroke, so a near
// miss gives it 32 chances per press to be wrong. Silence is the required
// behaviour — the poller logs nothing on a failed match by design.
func TestPollSequence_DigestVerifier_WrongCodeKeepsPolling(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	d := mustDigest(t, "s w o r d")
	sink := make(chan struct{}, 1)

	_, done := startPoller(t, f, d, sink)

	// A prefix of the secret, a suffix of it, and a one-key substitution —
	// laid so that no five consecutive records spell the code.
	f.push(mustEvents(t, "s w o r w o r d s w q r d")...)

	select {
	case <-sink:
		t.Fatal("signal sent for a keystroke stream that never contained the code")
	case <-done:
		t.Fatal("pollSequence returned without a match")
	case <-time.After(10 * pollInterval):
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
		pollSequence(stop, 0, f.seq, f.snapshot, m, sink, discardLogger())
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
		pollSequence(make(chan struct{}), 0, f.seq, f.snapshot, m, sink, discardLogger())
	}()

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

// TestPollSequence_PreExistingKeystrokes_NotReplayed pins the baseline
// parameter: records below it belong to a previous tap and must not be
// matched. Without it, re-arming a tap over a live ring would unlock
// instantly off stale input.
//
// startPoller samples the baseline synchronously, so the five records pushed
// below are unambiguously beneath it — the assertion no longer depends on
// beating the scheduler.
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

// TestPollSequence_KeystrokesBeforeGoroutineStart_StillMatch is the twin of
// the test above, pinning the same boundary from the other side: records at
// or above the baseline belong to THIS session and must match even when
// every one of them landed before the poller goroutine ran a single
// statement.
//
// This is the shape Install produces. The baseline is sampled while the tap
// source is not yet attached to any run loop; the callback goes live and the
// poller goroutine starts several steps later (gesture-tap install, channel
// and closure setup, the `go` statement, scheduling). Anything typed in that
// gap — up to and including the whole unlock code — sits above the baseline
// and must still unlock. Seeding lastSeq from seq() inside the goroutine
// instead would classify all of it as pre-existing and swallow the code with
// no diagnostic whatsoever, because a failed match is silent by design.
func TestPollSequence_KeystrokesBeforeGoroutineStart_StillMatch(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	// Baseline first, on an empty ring — exactly as Install does — and only
	// then the entire code, all of it before pollSequence starts.
	baseSeq := f.seq()
	f.push(mustEvents(t, "s w o r d")...)

	stopCh := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(stopCh) }) })
	go pollSequence(stopCh, baseSeq, f.seq, f.snapshot, m, sink, discardLogger())

	select {
	case <-sink:
	case <-time.After(20 * pollInterval):
		t.Fatalf("sink did not receive within %v — a code entered between the "+
			"baseline sample and the poller's first tick must still match",
			20*pollInterval)
	}
}

// TestPollSequence_CodeSplitAcrossTicks is the shape production actually
// sees. A human types at ~100-200ms per key against a 10ms tick, so the
// unlock code arrives one keystroke per tick and no single snapshot ever
// contains the whole thing — every other pollSequence test pushes the entire
// stream in one go and therefore exercises only the "it all landed in one
// window" case.
//
// What is under test is the carry-over of `lastSeq`: the window matchAny is
// given must start where the previous tick stopped, not at the newest
// record. If that carry-over broke, a code typed at human speed would never
// match while every existing test stayed green.
func TestPollSequence_CodeSplitAcrossTicks(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	startPoller(t, f, m, sink)

	for _, step := range []string{"s", "w", "o", "r", "d"} {
		f.push(mustEvents(t, step)...)
		// Long enough for the poller to observe this keystroke on its own
		// tick, so the next one lands in a separate snapshot.
		time.Sleep(3 * pollInterval)
	}

	select {
	case <-sink:
	case <-time.After(20 * pollInterval):
		t.Fatalf("sink did not receive within %v for a code typed one key per tick",
			20*pollInterval)
	}
}

// TestPollSequence_StrayKeyBetweenTicks_NoMatch is the negative twin: the
// same one-key-per-tick delivery, but with a wrong key in the middle. It
// pins that the cross-tick carry-over does not become a "collect the right
// keys in any order" filter — the steps must still be consecutive.
func TestPollSequence_StrayKeyBetweenTicks_NoMatch(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	startPoller(t, f, m, sink)

	for _, step := range []string{"s", "w", "o", "z", "r", "d"} {
		f.push(mustEvents(t, step)...)
		time.Sleep(3 * pollInterval)
	}

	select {
	case <-sink:
		t.Error("sink received a signal for a code interrupted by a stray key")
	default:
	}
}

// TestPollSequence_RingOverflow_RecoversAndMatches drives the one branch of
// pollSequence that no other test reaches: more than ringCap keystrokes
// between two ticks, i.e. the poller falling far enough behind that the
// oldest records were overwritten before it could read them.
//
// Two properties matter, and only the second is about the log line:
//
//  1. the aged-out records must NOT produce a match — the slots they
//     occupied now hold newer keystrokes, and a window assembled across that
//     boundary is fiction;
//  2. the poller must keep working afterwards. `lastSeq` jumps forward by
//     more than the ring holds, and a mistake there (clamping to a `from`
//     above `to`, say) would leave the unlock code permanently dead with no
//     symptom other than a machine that will not unlock.
func TestPollSequence_RingOverflow_RecoversAndMatches(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	m := mustSequence(t, "s w o r d")
	sink := make(chan struct{}, 1)

	startPoller(t, f, m, sink)

	// One burst larger than the ring, containing the code near its START so
	// the records are guaranteed to be overwritten by the tail of the burst.
	flood := mustEvents(t, "s w o r d")
	for i := 0; i < ringCap+16; i++ {
		flood = append(flood, mustEvents(t, "z")...)
	}
	f.push(flood...)

	time.Sleep(5 * pollInterval)
	select {
	case <-sink:
		t.Fatal("sink received a signal for keystrokes that aged out of the ring")
	default:
	}

	// The poller survived the overflow: a code typed after it still matches.
	f.push(mustEvents(t, "s w o r d")...)
	select {
	case <-sink:
	case <-time.After(20 * pollInterval):
		t.Fatalf("sink did not receive within %v after a ring overflow — the "+
			"poller must recover, not go deaf", 20*pollInterval)
	}
}

// TestClampFrom_CounterBehindCaller covers `to < from` — defence in depth
// against ANY counter reset observed below the poller's own lastSeq.
// Release orders its teardown so this cannot happen there (the poller is
// drained BEFORE eventtap_wipe_ring zeroes the counter), but clampFrom must
// not depend on that ordering holding: it is the last line of defence if a
// future reorder, a re-install, or a second wipe path ever lets a poller tick
// observe a counter behind it.
//
// The range must collapse to empty. Anything else would hand matchAny a
// window over the freshly-zeroed ring, and a zero record decodes as a bare
// `a` press — an unlock code of `a`-only steps could then match on the
// wipe itself.
func TestClampFrom_CounterBehindCaller(t *testing.T) {
	t.Parallel()

	const l = 4
	for _, to := range []uint64{0, 1, ringCap, ringCap * 4} {
		from, dropped := clampFrom(100+to, to, l)
		if from < to {
			t.Errorf("clampFrom(%d, %d, %d) = (%d, %v), want from >= to so the "+
				"half-open range [from, to) stays empty", 100+to, to, l, from, dropped)
		}
	}
}

// TestMatchAny_ToBeforeFrom is the same reset seen one layer up: matchAny
// must iterate zero times rather than run away over a wrapped uint64 range.
func TestMatchAny_ToBeforeFrom(t *testing.T) {
	t.Parallel()

	m := mustSequence(t, "s w o r d")
	evs := mustEvents(t, "s w o r d")
	ring, cur := newRing(0, evs)
	tail := make([]matcher.KeyEvent, m.MaxLen())

	// The code IS in the ring and would match over [0, cur) — the point is
	// that an inverted range must not consult it at all.
	if !matchAny(ring, 0, 0, cur, tail, m) {
		t.Fatal("precondition failed: the code is not in the ring")
	}
	if matchAny(ring, 0, cur, 0, tail, m) {
		t.Errorf("matchAny(ring, %d, 0, …) = true, want false — a counter "+
			"reset must collapse the range, not replay the whole ring", cur)
	}
}
