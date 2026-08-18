//go:build darwin

package eventtap

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// captureIdleTimeout and captureTotalTimeout are the two wall-clock ceilings
// a single capture pass runs under. BOTH are per-pass: CaptureConfirmed runs
// collectSteps twice over one tap, and each pass gets its own fresh budget —
// a user who types slowly on the confirmation pass must not inherit whatever
// the first pass spent.
//
// They exist because the CGEventTap callback that feeds this loop suppresses
// input at kCGHIDEventTap, Ctrl-C included. Under raw mode ISIG is off as
// well, so a keystroke never reaches the signal machinery at all. A capture
// that hangs with no ceiling therefore leaves the machine deaf: no way to
// interrupt, no way to type, nothing on screen explaining why. Escape is the
// deliberate exit and these are the involuntary ones — the ceiling that
// covers "walked away mid-capture" (idle) and the one that covers "something
// is wrong with the tap and keystrokes trickle in forever" (total).
//
// Neither value is a security parameter: they bound how long the process can
// hold the keyboard hostage, not how hard the secret is to guess.
const (
	captureIdleTimeout  = 10 * time.Second
	captureTotalTimeout = 60 * time.Second
)

// keyCodeReturn and keyCodeEscape are the two kVK_* virtual keycodes that
// terminate a capture pass. They are spelled as constants here rather than
// looked up through hotkey because hotkey's keycode table is unexported and
// exporting it to read two entries would widen that package's API for the
// sake of this one call site.
//
// The duplication is pinned rather than trusted: TestCaptureKeyCodes_
// MatchHotkeyTable parses "return" and "escape" through hotkey.ParseStep and
// fails if either constant drifts from the table the rest of the project
// matches against. A silent drift here is not a cosmetic bug — a Return that
// no longer terminates would make the capture unstoppable except by timeout,
// and an Escape that no longer cancels would take the only voluntary exit
// away while the tap holds the keyboard.
const (
	keyCodeReturn uint16 = 0x24 // kVK_Return
	keyCodeEscape uint16 = 0x35 // kVK_Escape
)

// Capture failure sentinels. Every one of them is a bare static string: none
// carries a keycode, a step count, an elapsed time or any other number, and
// none may ever grow one. These messages are printed by the --set-password
// branch after the user has physically typed the secret, so anything
// interpolated into them lands in the terminal scrollback, in tmux capture
// buffers and on any screen share that happens to be running — which is the
// exact leak the whole feature exists to close. TestCollectSteps_
// ErrorsNeverEchoKeycodes pins the property by rejecting any digit at all in
// the rendered message.
var (
	// ErrCaptureCancelled means the user pressed Escape (unmodified). It is
	// the voluntary exit from a capture pass and carries no partial input:
	// whatever had been typed before Escape is wiped and never returned.
	ErrCaptureCancelled = errors.New("eventtap: unlock sequence capture cancelled")

	// ErrCaptureTooLong means the user kept typing past hotkey.MaxSteps
	// without pressing Return. It is an error rather than a silent truncation
	// on purpose: hashing a prefix of what was typed would lock the user out
	// with a secret they never entered, and the mode gives no feedback that
	// would let them discover the truncation.
	ErrCaptureTooLong = errors.New("eventtap: unlock sequence has too many steps")

	// ErrCaptureLostEvents means the keystroke ring wrapped far enough that
	// records aged out before this loop read them. Same reasoning as
	// ErrCaptureTooLong, one layer lower: a sequence with a hole in it is not
	// the sequence the user typed, and storing its hash is a lockout.
	ErrCaptureLostEvents = errors.New("eventtap: keystroke ring lagged during capture; the sequence would be incomplete")

	// ErrCaptureTimedOut means the pass hit captureIdleTimeout or
	// captureTotalTimeout. The two are deliberately NOT distinguished in the
	// error: which ceiling fired is a statement about how long the user was
	// typing, and the timing of the secret is as much a secret as its length.
	ErrCaptureTimedOut = errors.New("eventtap: unlock sequence capture timed out")
)

// collectSteps reads one capture pass out of the C-side keystroke ring and
// returns the sequence the user typed, the sequence number just past the
// record it stopped on, and an error.
//
// It is the capture-side twin of pollSequence: same 10ms cadence, same
// seq()/snapshot() accessors, same clampFrom-based lag detection, same
// "decide nothing in C" discipline. The difference is what it does with the
// records — pollSequence slides a window over them looking for a configured
// secret, collectSteps consumes them one at a time and accumulates a new one.
//
// Pure by construction: every input that is not a plain value arrives as a
// function parameter (seq, snapshot, now), so the whole thing runs in unit
// tests with no CGEventTap, no Accessibility grant and no keyboard.
//
// # baseSeq is a parameter, not an in-loop seq() read
//
// pollSequence takes its baseline the same way and poller.go explains why at
// length; the argument transfers verbatim. The caller samples the counter at
// the one moment the ring is provably quiescent — right after
// eventtap_install_c has zeroed it and BEFORE the tap source is attached to
// a run loop — because that is the only place with the ordering knowledge to
// sample it safely. Seeding from seq() in here instead would sample a live
// counter: everything typed between the tap going live and this loop being
// scheduled would be classified as predating the pass and dropped, and
// clampFrom would not report it as a loss because from its point of view
// nothing aged out. The result is a silently truncated secret — the worst
// possible outcome for a function whose output gets hashed and stored.
//
// # endSeq is load-bearing, not a convenience
//
// The second return value is the sequence number one past the record the
// pass stopped on, i.e. immediately after the terminating Return. It is what
// makes two passes over a SINGLE tap possible at all.
//
// A tap has exactly one install-time baseline. Handing that same baseline to
// the confirmation pass would have it re-scan the first pass's records, walk
// straight into the first pass's Return, and return an identical sequence
// without ever waiting for a keystroke. The comparison the second pass exists
// to perform would then be a tautology: it would confirm a typo as eagerly as
// it confirms a correct entry, the plaintext is deleted by then, and the
// length is not stored, so there is nothing left to recover from. Reading
// seq() between passes instead is wrong for the mirror-image reason — it
// discards everything typed between the first Return and that read.
//
// endSeq is defined on the success path only. The cancel, timeout and
// lost-events paths return 0 and their callers have nothing to continue from.
//
// # Terminators
//
// Return (unmodified) ends the pass and is NOT part of the sequence. Escape
// (unmodified) cancels it. Both comparisons happen AFTER masking with
// matcher.UserIntentionalMask, which is the same fail-safe direction the
// matcher uses: macOS raises system bits (CapsLock, NumPad, SecondaryFn on
// the whole function-key group, NX_NONCOALSESCEDMASK) that the user cannot
// withhold, so requiring them absent would make Return unable to terminate
// on some keyboards. With modifiers actually held — cmd+return, shift+escape
// — the mask leaves the bit standing and the press is an ordinary step, so
// both keys remain usable inside a secret.
//
// # Logging
//
// Nothing about the pass is logged: not its completion, not its length, not
// a single keystroke. This is stricter than pollSequence, which logs the one
// final match at INFO, and the extra strictness is specific to being called
// twice. slog's handlers timestamp every record, --debug is permitted on the
// --set-password branch, and two completion lines subtract to the exact
// duration of the confirmation pass. Timing is secret for the same reason
// length is.
//
// The single exception is the ring-lag fact at DEBUG, which says the
// mechanism fell behind and nothing about what was typed.
//
// log == nil falls back to slog.Default(); now == nil falls back to
// time.Now, so production callers pass neither.
func collectSteps(
	ctx context.Context,
	baseSeq uint64,
	seq func() uint64,
	snapshot func([]matcher.KeyEvent) uint64,
	now func() time.Time,
	log *slog.Logger,
) ([]hotkey.Spec, uint64, error) {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}

	// Zeroed on every exit path for the same reason the poller zeroes its
	// copy: this buffer holds a byte-identical mirror of the ring, which on
	// this code path IS the secret being captured. matcher.KeyEvent is
	// pointer-free, so nothing would make the GC scrub it before the span is
	// recycled.
	ring := make([]matcher.KeyEvent, ringCap)
	defer clear(ring)

	steps := make([]hotkey.Spec, 0, hotkey.MaxSteps)

	// fail wipes the partial sequence before surfacing an error. Every
	// non-success exit goes through it: a half-typed secret is still a
	// secret, and the callers of collectSteps discard the slice anyway, so
	// leaving it populated would only extend its lifetime in the heap.
	fail := func(err error) ([]hotkey.Spec, uint64, error) {
		clear(steps)
		return nil, 0, err
	}

	lastSeq := baseSeq
	start := now()
	lastPress := start

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-ticker.C:
			t := now()
			// Ceilings are evaluated before the ring is read, so a pass can
			// never consume input past its budget. A Return that lands in the
			// same tick the budget expires is refused; bounding the time the
			// keyboard stays captured is worth more than that millisecond of
			// generosity, and the user simply runs the command again.
			if t.Sub(start) >= captureTotalTimeout || t.Sub(lastPress) >= captureIdleTimeout {
				return fail(ErrCaptureTimedOut)
			}

			cur := seq()
			if cur == lastSeq {
				// No keystroke since the last tick — the common case while
				// the user reads the prompt. Skip the ring memcpy entirely.
				continue
			}

			// Re-read the counter as part of the snapshot: the memcpy is not
			// atomic with the load above, and the value returned alongside
			// the copy is the one that describes it.
			cur = snapshot(ring)

			// Window length 1: this loop consumes records individually rather
			// than assembling a tail, so the only thing that must still be
			// intact is the single oldest record it is about to read. That is
			// exactly what clampFrom(_, _, 1) bounds — the oldest readable
			// record is `cur-ringCap+1`, one slot inside the ring, never
			// `cur-ringCap`, whose slot the writer may already be reusing.
			from, dropped := clampFrom(lastSeq, cur, 1)
			if dropped {
				log.Debug("eventtap: keystroke ring lagged; older events aged out unread")
				return fail(ErrCaptureLostEvents)
			}

			for s := from; s < cur; s++ {
				rec := ring[s&ringMask]
				mods := rec.Modifiers & matcher.UserIntentionalMask
				switch {
				case mods == 0 && rec.KeyCode == keyCodeReturn:
					// One past the Return: the caller hands this to the next
					// pass as its baseline, so it must exclude the Return
					// itself or that pass would terminate on it immediately.
					return steps, s + 1, nil
				case mods == 0 && rec.KeyCode == keyCodeEscape:
					return fail(ErrCaptureCancelled)
				case len(steps) == hotkey.MaxSteps:
					// The step after the last admissible one. Checked AFTER
					// the terminators so a full-length secret can still be
					// completed with Return — only a genuine 33rd step gets
					// here.
					return fail(ErrCaptureTooLong)
				}
				steps = append(steps, hotkey.Spec{Modifiers: mods, KeyCode: rec.KeyCode})
			}

			lastPress = t
			lastSeq = cur
		}
	}
}
