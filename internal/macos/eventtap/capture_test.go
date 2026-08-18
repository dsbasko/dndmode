//go:build darwin

// White-box tests (package eventtap, not eventtap_test) for the capture
// loop that --set-password uses to read a new unlock sequence out of the
// keystroke ring. collectSteps is unexported and has no exported wrapper
// until CaptureConfirmed lands, so an external test package could not reach
// it.
//
// Everything here is pure Go. The C-side ring is replaced by the same
// fakeRing the poller tests use, the clock is injected, and the terminators
// are ordinary records — so the whole loop runs with no CGEventTap, no
// Accessibility grant, no signed binary and no keyboard.

package eventtap

import (
	"context"
	"errors"
	"slices"
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

// captureTimeout bounds every collectSteps call in this file that is not
// itself testing a timeout. The loop ticks at pollInterval, so a working
// implementation answers within one or two ticks; anything approaching this
// bound means it blocked, and a bounded context turns that into a named
// failure instead of a hung `go test`.
const captureTimeout = 40 * pollInterval

// steppingClock advances a fixed amount on every read. Injected as
// collectSteps' `now`, it makes the wall-clock ceilings reachable in
// milliseconds of real time and — because the loop reads the clock exactly
// once per tick — makes WHICH tick trips them deterministic rather than
// dependent on scheduler jitter.
type steppingClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func newSteppingClock(step time.Duration) *steppingClock {
	return &steppingClock{t: time.Unix(0, 0), step: step}
}

func (c *steppingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

// typingRing appends one keystroke every time the counter is probed, i.e.
// once per tick. It models a user who never stops typing, which is the only
// way to keep the idle ceiling permanently reset and leave the TOTAL ceiling
// as the sole thing that can end the pass.
type typingRing struct {
	f  *fakeRing
	ev matcher.KeyEvent
}

func (r *typingRing) seq() uint64 {
	r.f.push(r.ev)
	return r.f.seq()
}

func (r *typingRing) snapshot(buf []matcher.KeyEvent) uint64 { return r.f.snapshot(buf) }

// mustSpecs parses the same string grammar the config uses, so a test's
// "typed" line and its "expected steps" line are written identically and
// cannot silently diverge.
func mustSpecs(t *testing.T, code string) []hotkey.Spec {
	t.Helper()
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatalf("ParseSequence(%q): %v", code, err)
	}
	return steps
}

// assertSpecs compares two step slices field by field. It reports positions
// and lengths only — never the keycodes themselves — so a failing test in CI
// output does not become the leak the feature exists to close.
func assertSpecs(t *testing.T, got, want []hotkey.Spec) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("collected %d steps, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d differs from the expected sequence", i)
		}
	}
}

// oneEvent returns the KeyEvent a user pressing `code` would produce.
func oneEvent(t *testing.T, code string) matcher.KeyEvent {
	t.Helper()
	evs := mustEvents(t, code)
	if len(evs) != 1 {
		t.Fatalf("oneEvent(%q): got %d events, want 1", code, len(evs))
	}
	return evs[0]
}

// repeatEvent builds n copies of a single keystroke. Used for the
// hotkey.MaxSteps boundary, which cannot be expressed through ParseSequence:
// the parser rejects anything longer than MaxSteps, and the 33rd press is
// precisely what the ceiling test needs to feed the loop.
func repeatEvent(ev matcher.KeyEvent, n int) []matcher.KeyEvent {
	evs := make([]matcher.KeyEvent, n)
	for i := range evs {
		evs[i] = ev
	}
	return evs
}

// ---------------------------------------------------------------------------
// keycode constants
// ---------------------------------------------------------------------------

// TestCaptureKeyCodes_MatchHotkeyTable pins the two terminator constants
// against hotkey's keycode table. capture.go spells them literally because
// that table is unexported; this test is what keeps the copy honest.
//
// Drift here is not cosmetic. A Return that no longer terminates makes the
// capture unstoppable except by timeout while the tap holds every key on the
// machine, and an Escape that no longer cancels removes the only voluntary
// way out of that state.
func TestCaptureKeyCodes_MatchHotkeyTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want uint16
	}{
		{name: "return", want: keyCodeReturn},
		{name: "escape", want: keyCodeEscape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec, err := hotkey.ParseStep(tt.name)
			if err != nil {
				t.Fatalf("ParseStep(%q): %v", tt.name, err)
			}
			if spec.KeyCode != tt.want {
				t.Errorf("capture constant for %q is 0x%02X, hotkey table says 0x%02X",
					tt.name, tt.want, spec.KeyCode)
			}
			if spec.Modifiers != 0 {
				t.Errorf("ParseStep(%q) carries modifiers; the terminator comparison assumes a bare key", tt.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// terminators
// ---------------------------------------------------------------------------

// TestCollectSteps_ReturnTerminates is the happy path: everything typed
// before Return comes back, Return itself does not.
func TestCollectSteps_ReturnTerminates(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o r d return")...)

	steps, endSeq, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
	if err != nil {
		t.Fatalf("collectSteps: unexpected error: %v", err)
	}
	assertSpecs(t, steps, mustSpecs(t, "s w o r d"))
	if endSeq != 6 {
		t.Errorf("endSeq = %d, want 6 (one past the Return at sequence 5)", endSeq)
	}
}

// TestCollectSteps_EscapeCancels covers the voluntary exit. Nothing typed
// before Escape may survive: the caller has no use for a partial secret and
// returning one would only lengthen its life in the heap.
func TestCollectSteps_EscapeCancels(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o escape")...)

	steps, endSeq, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
	if !errors.Is(err, ErrCaptureCancelled) {
		t.Fatalf("collectSteps err = %v, want ErrCaptureCancelled", err)
	}
	if steps != nil {
		t.Errorf("cancelled capture returned %d steps; the partial sequence must not escape", len(steps))
	}
	if endSeq != 0 {
		t.Errorf("endSeq = %d on the cancel path, want 0 (undefined, so it must not look resumable)", endSeq)
	}
}

// TestCollectSteps_ModifiedTerminatorsAreSteps pins the "mask, then compare"
// direction. Return and Escape are terminators only when pressed bare; held
// with a real modifier they are ordinary keys and must be usable inside a
// secret like any other.
func TestCollectSteps_ModifiedTerminatorsAreSteps(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	f := &fakeRing{}
	f.push(mustEvents(t, "cmd+return shift+escape s return")...)

	steps, _, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
	if err != nil {
		t.Fatalf("collectSteps: unexpected error: %v", err)
	}
	assertSpecs(t, steps, mustSpecs(t, "cmd+return shift+escape s"))
}

// TestCollectSteps_SystemBitsDoNotBlockTerminators is the lockout guard from
// the other side. macOS raises bits the user cannot withhold — CapsLock,
// NumPad, SecondaryFn on the whole function-key group, NX_NONCOALSESCEDMASK
// — and Return carries them just like any other key. If the terminator test
// demanded a raw zero instead of a zero after masking, a user with CapsLock
// on could not finish a capture at all while the tap held their keyboard.
func TestCollectSteps_SystemBitsDoNotBlockTerminators(t *testing.T) {
	t.Parallel()

	const systemBits = hotkey.ModFn | 0x10000 | 0x200000 | 0x400000 | 0x100

	tests := []struct {
		name    string
		keyCode uint16
		check   func(t *testing.T, steps []hotkey.Spec, err error)
	}{
		{
			name:    "return",
			keyCode: keyCodeReturn,
			check: func(t *testing.T, steps []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("decorated Return failed to terminate: %v", err)
				}
				assertSpecs(t, steps, mustSpecs(t, "s"))
			},
		},
		{
			name:    "escape",
			keyCode: keyCodeEscape,
			check: func(t *testing.T, steps []hotkey.Spec, err error) {
				if !errors.Is(err, ErrCaptureCancelled) {
					t.Fatalf("decorated Escape failed to cancel: err = %v", err)
				}
				if steps != nil {
					t.Errorf("cancelled capture returned %d steps", len(steps))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
			defer cancel()

			f := &fakeRing{}
			f.push(oneEvent(t, "s"))
			f.push(matcher.KeyEvent{Modifiers: systemBits, KeyCode: tt.keyCode})

			steps, _, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
			tt.check(t, steps, err)
		})
	}
}

// ---------------------------------------------------------------------------
// baseline and endSeq
// ---------------------------------------------------------------------------

// TestCollectSteps_BaseSeqBehindCounter_KeepsPrefix is the pin against silent
// truncation of the secret. Three keystrokes are already in the ring when the
// call starts, so seq() is ahead of the baseline the caller passed. An
// implementation that seeded its cursor from seq() instead of from baseSeq
// would classify those three as predating the pass, return only the last two
// steps, and report no error at all — the user would type five steps and a
// two-step secret would be hashed and stored.
func TestCollectSteps_BaseSeqBehindCounter_KeepsPrefix(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o")...)

	type result struct {
		steps []hotkey.Spec
		err   error
	}
	done := make(chan result, 1)
	go func() {
		steps, _, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
		done <- result{steps: steps, err: err}
	}()

	f.push(mustEvents(t, "r d return")...)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("collectSteps: unexpected error: %v", got.err)
		}
		assertSpecs(t, got.steps, mustSpecs(t, "s w o r d"))
	case <-time.After(captureTimeout):
		t.Fatal("collectSteps did not return; the prefix already in the ring was never consumed")
	}
}

// TestCollectSteps_EndSeqPointsPastReturn covers both halves of the
// two-pass contract that endSeq exists to make possible.
//
// A tap has exactly one install-time baseline, so the confirmation pass has
// to start from the number the first pass reports. If endSeq were wrong — the
// Return's own sequence number, or the baseline again — the second pass would
// re-scan the first pass's records, hit its Return and return an identical
// sequence without waiting for a single keystroke. The comparison would then
// confirm a typo exactly as eagerly as a correct entry, and by that point the
// plaintext is gone and the length is not stored.
func TestCollectSteps_EndSeqPointsPastReturn(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o r d return")...)

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), captureTimeout)
	defer cancelFirst()

	first, endSeq, err := collectSteps(firstCtx, 0, f.seq, f.snapshot, nil, discardLogger())
	if err != nil {
		t.Fatalf("first pass: unexpected error: %v", err)
	}
	if endSeq != uint64(len(first))+1 {
		t.Fatalf("endSeq = %d, want %d (one past the terminating Return)", endSeq, len(first)+1)
	}

	// Half one: with no new input, a pass resumed from endSeq must WAIT.
	// Seeing anything here would mean it replayed the first pass.
	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), captureTimeout)
	defer cancelBlocked()

	replay, _, err := collectSteps(blockedCtx, endSeq, f.seq, f.snapshot, nil, discardLogger())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second pass over unchanged ring returned err = %v, want context.DeadlineExceeded — "+
			"it must not see the first pass's records", err)
	}
	if replay != nil {
		t.Errorf("second pass returned %d steps without any new keystroke", len(replay))
	}

	// Half two: fresh input resumed from endSeq yields ONLY the new steps.
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), captureTimeout)
	defer cancelSecond()

	f.push(mustEvents(t, "a b return")...)

	second, secondEnd, err := collectSteps(secondCtx, endSeq, f.seq, f.snapshot, nil, discardLogger())
	if err != nil {
		t.Fatalf("second pass: unexpected error: %v", err)
	}
	assertSpecs(t, second, mustSpecs(t, "a b"))
	if secondEnd != 9 {
		t.Errorf("second endSeq = %d, want 9 (one past the second Return at sequence 8)", secondEnd)
	}
}

// ---------------------------------------------------------------------------
// ceilings
// ---------------------------------------------------------------------------

// TestCollectSteps_TooLong pins both sides of the hotkey.MaxSteps boundary:
// a full-length secret still completes, and the step after it is refused
// rather than dropped.
//
// Silent truncation is the failure this rules out. A capture that quietly
// kept the first MaxSteps presses would hash a prefix of what the user typed
// and store it — and since nothing about the capture is echoed, the first
// evidence would be a shield that will not come down.
func TestCollectSteps_TooLong(t *testing.T) {
	t.Parallel()

	a := oneEvent(t, "a")
	ret := oneEvent(t, "return")

	tests := []struct {
		name    string
		presses int
		check   func(t *testing.T, steps []hotkey.Spec, err error)
	}{
		{
			name:    "exactly MaxSteps completes",
			presses: hotkey.MaxSteps,
			check: func(t *testing.T, steps []hotkey.Spec, err error) {
				if err != nil {
					t.Fatalf("a full-length sequence must be accepted: %v", err)
				}
				if len(steps) != hotkey.MaxSteps {
					t.Errorf("collected %d steps, want %d", len(steps), hotkey.MaxSteps)
				}
			},
		},
		{
			name:    "one past MaxSteps is refused",
			presses: hotkey.MaxSteps + 1,
			check: func(t *testing.T, steps []hotkey.Spec, err error) {
				if !errors.Is(err, ErrCaptureTooLong) {
					t.Fatalf("collectSteps err = %v, want ErrCaptureTooLong", err)
				}
				if steps != nil {
					t.Errorf("over-long capture returned %d steps; a truncated secret must not escape", len(steps))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
			defer cancel()

			f := &fakeRing{}
			f.push(repeatEvent(a, tt.presses)...)
			f.push(ret)

			steps, _, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
			tt.check(t, steps, err)
		})
	}
}

// TestCollectSteps_LostEvents covers the ring falling behind. The loop is
// handed a baseline far enough back that records aged out unread, and it must
// refuse the pass rather than return the survivors: a sequence with a hole in
// it is not what the user typed, and hashing it locks them out with a secret
// they never entered.
func TestCollectSteps_LostEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	a := oneEvent(t, "a")
	f := &fakeRing{}
	// Comfortably more than the ring holds, so clampFrom is certain to
	// report a drop for a baseline of 0.
	f.push(repeatEvent(a, 2*ringCap)...)
	f.push(oneEvent(t, "return"))

	steps, endSeq, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
	if !errors.Is(err, ErrCaptureLostEvents) {
		t.Fatalf("collectSteps err = %v, want ErrCaptureLostEvents", err)
	}
	if steps != nil {
		t.Errorf("lagged capture returned %d steps; an incomplete sequence must not escape", len(steps))
	}
	if endSeq != 0 {
		t.Errorf("endSeq = %d on the lost-events path, want 0", endSeq)
	}
}

// TestCollectSteps_IdleTimeout covers the ceiling that catches a user who
// walked away mid-capture. Without it the tap keeps every key on the machine
// suppressed — Ctrl-C included — with nothing on screen and no way out.
//
// The clock advances 2s per read and the loop reads it once per tick, so the
// idle ceiling is crossed on a known tick a few tens of milliseconds in.
func TestCollectSteps_IdleTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*captureTimeout)
	defer cancel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o")...)

	clock := newSteppingClock(captureIdleTimeout / 5)

	steps, _, err := collectSteps(ctx, 0, f.seq, f.snapshot, clock.now, discardLogger())
	if !errors.Is(err, ErrCaptureTimedOut) {
		t.Fatalf("collectSteps err = %v, want ErrCaptureTimedOut", err)
	}
	if steps != nil {
		t.Errorf("timed-out capture returned %d steps; the partial sequence must not escape", len(steps))
	}
}

// TestCollectSteps_TotalTimeout covers the other ceiling. A keystroke arrives
// on every single tick, so the idle timer is reset each time and can never
// fire — only the total cap can end this pass. Without that cap the loop
// would run forever and this test would hang, which is exactly the production
// failure it stands in for.
//
// The clock advances captureTotalTimeout/10 per read, so the cap is reached
// after ten ticks and ten keystrokes — well inside hotkey.MaxSteps, so the
// length ceiling cannot claim the pass first.
func TestCollectSteps_TotalTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*captureTimeout)
	defer cancel()

	r := &typingRing{f: &fakeRing{}, ev: oneEvent(t, "a")}
	clock := newSteppingClock(captureTotalTimeout / 10)

	steps, _, err := collectSteps(ctx, 0, r.seq, r.snapshot, clock.now, discardLogger())
	if !errors.Is(err, ErrCaptureTimedOut) {
		t.Fatalf("collectSteps err = %v, want ErrCaptureTimedOut", err)
	}
	if steps != nil {
		t.Errorf("timed-out capture returned %d steps", len(steps))
	}
}

// TestCollectSteps_ContextCancelled pins the third involuntary exit. The
// --set-password branch installs its own SIGINT/SIGTERM/SIGHUP handler, and
// cancelling the context is how that handler unwinds the capture instead of
// killing the process with the terminal still in raw mode.
func TestCollectSteps_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o")...)

	steps, endSeq, err := collectSteps(ctx, 0, f.seq, f.snapshot, nil, discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("collectSteps err = %v, want context.Canceled", err)
	}
	if steps != nil {
		t.Errorf("cancelled capture returned %d steps", len(steps))
	}
	if endSeq != 0 {
		t.Errorf("endSeq = %d on the cancel path, want 0", endSeq)
	}
}

// ---------------------------------------------------------------------------
// leak discipline
// ---------------------------------------------------------------------------

// TestCollectSteps_ErrorsNeverEchoKeycodes is the scrollback pin. These
// messages are printed by --set-password immediately after the user has
// physically typed the secret, so anything interpolated into them lands in
// the terminal history, in tmux capture buffers and on any screen share
// running at the time.
//
// The assertion is deliberately blunt — no digit may appear anywhere in the
// rendered message. A keycode, a step count and an elapsed second are all
// digits, and a blanket rule cannot be satisfied by a formulation that
// happens to encode the same fact differently. It runs over errors produced
// by real capture runs, not just over the sentinels, so a future `%w` wrapper
// that adds context is caught too.
func TestCollectSteps_ErrorsNeverEchoKeycodes(t *testing.T) {
	t.Parallel()

	a := oneEvent(t, "a")

	tests := []struct {
		name  string
		build func(t *testing.T) error
	}{
		{
			name: "cancelled",
			build: func(t *testing.T) error {
				f := &fakeRing{}
				f.push(mustEvents(t, "s w o escape")...)
				return runCapture(t, f.seq, f.snapshot, nil)
			},
		},
		{
			name: "too long",
			build: func(t *testing.T) error {
				f := &fakeRing{}
				f.push(repeatEvent(a, hotkey.MaxSteps+1)...)
				return runCapture(t, f.seq, f.snapshot, nil)
			},
		},
		{
			name: "lost events",
			build: func(t *testing.T) error {
				f := &fakeRing{}
				f.push(repeatEvent(a, 2*ringCap)...)
				return runCapture(t, f.seq, f.snapshot, nil)
			},
		},
		{
			name: "timed out",
			build: func(t *testing.T) error {
				f := &fakeRing{}
				f.push(mustEvents(t, "s w o")...)
				clock := newSteppingClock(captureIdleTimeout / 5)
				return runCapture(t, f.seq, f.snapshot, clock.now)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.build(t)
			if err == nil {
				t.Fatal("expected a capture failure, got nil")
			}
			if i := strings.IndexAny(err.Error(), "0123456789"); i >= 0 {
				t.Errorf("capture error message contains a digit at offset %d; "+
					"no keycode, step count or duration may reach the terminal", i)
			}
		})
	}
}

// runCapture drives one collectSteps pass to failure under a bounded context
// and returns the error. Used by the leak test, which cares only about what
// the message says.
func runCapture(
	t *testing.T,
	seq func() uint64,
	snapshot func([]matcher.KeyEvent) uint64,
	now func() time.Time,
) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 100*captureTimeout)
	defer cancel()

	_, _, err := collectSteps(ctx, 0, seq, snapshot, now, discardLogger())
	return err
}

// ---------------------------------------------------------------------------
// two-pass confirmation
// ---------------------------------------------------------------------------

// promptRecorder captures the pass numbers confirmPasses announced and, on
// the pass it is armed for, feeds the ring the entry that pass is supposed to
// wait for. Arming happens INSIDE the callback rather than before the call,
// which is the point: a second pass seeded from the first pass's baseline
// would already have returned by the time this runs, so any test that passes
// with the input supplied here is a test that proves the hand-off works.
type promptRecorder struct {
	mu     sync.Mutex
	passes []int
	onPass map[int]func()
}

func (p *promptRecorder) prompt(pass int) {
	p.mu.Lock()
	p.passes = append(p.passes, pass)
	fn := p.onPass[pass]
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (p *promptRecorder) seen() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.passes...)
}

// TestConfirmPasses_AgreeingEntries is the success path, and it is written so
// that it can only pass if the second pass genuinely waited: the ring holds
// nothing but the first entry until prompt(2) fires.
func TestConfirmPasses_AgreeingEntries(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o r d return")...)

	rec := &promptRecorder{onPass: map[int]func(){
		2: func() { f.push(mustEvents(t, "s w o r d return")...) },
	}}

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	got, err := confirmPasses(ctx, 0, f.seq, f.snapshot, nil, rec.prompt, discardLogger())
	if err != nil {
		t.Fatalf("confirmPasses: unexpected error: %v", err)
	}
	assertSpecs(t, got, mustSpecs(t, "s w o r d"))

	if want := []int{1, 2}; !slices.Equal(rec.seen(), want) {
		t.Errorf("prompt called with %v, want %v — both prompts must be printed, in order, while the tap is live", rec.seen(), want)
	}
}

// TestConfirmPasses_Mismatch is the typo the second pass exists to catch.
// Nothing is returned: the caller must not be able to hash either entry,
// because there is no way to know which of the two the user meant.
func TestConfirmPasses_Mismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		first  string
		second string
	}{
		{name: "different step", first: "s w o r d return", second: "s w o r f return"},
		{name: "second is a prefix", first: "s w o r d return", second: "s w o return"},
		{name: "second is longer", first: "s w o return", second: "s w o r d return"},
		{name: "same steps, different modifiers", first: "a b return", second: "a shift+b return"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeRing{}
			f.push(mustEvents(t, tc.first)...)

			rec := &promptRecorder{onPass: map[int]func(){
				2: func() { f.push(mustEvents(t, tc.second)...) },
			}}

			ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
			defer cancel()

			got, err := confirmPasses(ctx, 0, f.seq, f.snapshot, nil, rec.prompt, discardLogger())
			if !errors.Is(err, ErrCaptureMismatch) {
				t.Fatalf("err = %v, want ErrCaptureMismatch", err)
			}
			if got != nil {
				t.Errorf("confirmPasses returned %d steps alongside a mismatch; neither entry may survive", len(got))
			}
		})
	}
}

// TestConfirmPasses_SecondPassWaitsForFreshInput is the tautology guard in
// pure-Go form. With the ring left exactly as the first pass finished it and
// no further input ever arriving, confirmPasses must block until the context
// expires rather than re-reading the first pass's records and "confirming"
// them against themselves.
//
// A regression that seeds the second pass from the install-time baseline —
// or from anything else at or before the first Return — turns this test's
// deadline into a successful capture, which is exactly the failure that
// would hash a typo and lock the user out.
func TestConfirmPasses_SecondPassWaitsForFreshInput(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o r d return")...)

	rec := &promptRecorder{}

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	got, err := confirmPasses(ctx, 0, f.seq, f.snapshot, nil, rec.prompt, discardLogger())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded — the second pass must wait for real input", err)
	}
	if got != nil {
		t.Errorf("confirmPasses returned %d steps without a second entry ever being typed", len(got))
	}
	if want := []int{1, 2}; !slices.Equal(rec.seen(), want) {
		t.Errorf("prompt called with %v, want %v", rec.seen(), want)
	}
}

// TestConfirmPasses_FirstPassFailureStopsThere: a cancelled or timed-out
// first pass must surface its own error and never announce a second pass.
// Prompting for a confirmation of something that was never captured would be
// a plain lie to the user, and it would hold the keyboard for another full
// budget while doing it.
func TestConfirmPasses_FirstPassFailureStopsThere(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o escape")...)

	rec := &promptRecorder{}

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	got, err := confirmPasses(ctx, 0, f.seq, f.snapshot, nil, rec.prompt, discardLogger())
	if !errors.Is(err, ErrCaptureCancelled) {
		t.Fatalf("err = %v, want ErrCaptureCancelled", err)
	}
	if got != nil {
		t.Errorf("confirmPasses returned %d steps after a cancelled first pass", len(got))
	}
	if want := []int{1}; !slices.Equal(rec.seen(), want) {
		t.Errorf("prompt called with %v, want %v — no second prompt after a failed first pass", rec.seen(), want)
	}
}

// TestConfirmPasses_SecondPassFailurePropagates: an error in the confirmation
// pass is the user's error, surfaced verbatim. It is not downgraded to a
// mismatch, and nothing captured so far comes back with it.
func TestConfirmPasses_SecondPassFailurePropagates(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "s w o r d return")...)

	rec := &promptRecorder{onPass: map[int]func(){
		2: func() { f.push(mustEvents(t, "s w escape")...) },
	}}

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	got, err := confirmPasses(ctx, 0, f.seq, f.snapshot, nil, rec.prompt, discardLogger())
	if !errors.Is(err, ErrCaptureCancelled) {
		t.Fatalf("err = %v, want ErrCaptureCancelled", err)
	}
	if got != nil {
		t.Errorf("confirmPasses returned %d steps after a cancelled second pass", len(got))
	}
}

// TestConfirmPasses_NilPromptIsAllowed keeps the callback optional so tests
// and any non-interactive caller need not supply one. It must not panic and
// must not change the outcome.
func TestConfirmPasses_NilPromptIsAllowed(t *testing.T) {
	t.Parallel()

	f := &fakeRing{}
	f.push(mustEvents(t, "a b return")...)
	f.push(mustEvents(t, "a b return")...)

	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	got, err := confirmPasses(ctx, 0, f.seq, f.snapshot, nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("confirmPasses: unexpected error: %v", err)
	}
	assertSpecs(t, got, mustSpecs(t, "a b"))
}

// TestErrCaptureMismatch_NeverEchoesTheSecret extends the sentinel leak pin
// to the one error the two-pass layer adds. Saying which step differed, or
// that one entry was longer, would describe the secret to a terminal the
// user cannot yet type into — and unlike the collectSteps sentinels, this
// one is raised at the moment BOTH entries are in hand, so it is the message
// with the most to give away.
func TestErrCaptureMismatch_NeverEchoesTheSecret(t *testing.T) {
	t.Parallel()

	if i := strings.IndexAny(ErrCaptureMismatch.Error(), "0123456789"); i >= 0 {
		t.Errorf("ErrCaptureMismatch contains a digit at offset %d; no step count or position may reach the terminal", i)
	}
}
