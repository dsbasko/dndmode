//go:build darwin

package eventtap

import (
	"context"
	"errors"
	"log/slog"
	"slices"
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
// it confirms a correct entry, and by the time anyone noticed, this process
// would have wiped the plaintext and stored no length — nothing it kept could
// say what was actually typed. (Nothing about the DIGEST is claimed here: a
// single salted SHA-256 over a short key sequence is enumerable offline, and
// the README says so. The point is only that dndmode retains no copy.) Reading
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

// CaptureConfirmed reads a new unlock sequence from the keyboard twice over
// ONE CGEventTap and returns it only if both entries agree.
//
// It is the only exported way into capture, and that is a safety property
// rather than a packaging choice: a single-pass capture cannot be confirmed,
// and an unconfirmed sequence is a typo that gets salted, hashed, written to
// the config and never handed back — the plaintext is wiped by then, the
// length is deliberately not stored, and no command recovers either. That is
// a statement about RETENTION and not about cryptography: the stored pair is
// a single salted SHA-256, so a short typo IS enumerable offline by anyone
// holding the file. Which changes nothing for the person at the keyboard,
// who now has a shield answering only to a sequence they never meant to type.
// collectSteps, the single pass, stays unexported so no caller can assemble
// that outcome by accident. Confirmation is by construction here, not by
// discipline at the call site.
//
// # Why the tap is installed once and not per pass
//
// Everything between the two passes — printing the second prompt, comparing
// nothing yet, scheduling the next goroutine step — happens while the tap is
// still live. If the tap came down after the first Return and went back up
// for the second pass, that gap would be an open terminal: the leading steps
// of the secret the user is already typing would go to the shell, echo into
// the scrollback, and sit in the tty input buffer for whatever runs next.
// The window is small and the leak it produces is permanent, so the tap is
// installed once, held across both passes, and removed by a single defer.
//
// This is also why the prompts arrive through a callback instead of being
// printed by the caller: main.go could not print between the passes without
// the passes being separate calls, and separate calls are exactly what would
// reintroduce the gap.
//
// prompt is invoked with 1 before the first pass and 2 before the second,
// on this goroutine, with the tap live. It may be nil (tests). Whatever it
// writes lands on a terminal whose input is currently suppressed, which is
// the intended arrangement: the user can read but cannot type into anything
// else.
//
// # Baselines
//
// The first pass starts from the install-time baseline installForCapture
// returns. The second starts from the endSeq the FIRST pass returned — not
// from the install-time value, and not from a fresh seq() read. Both
// alternatives are wrong in opposite directions and collectSteps' doc
// comment spells out why at length: the install-time value would have the
// second pass re-scan the first pass's records and terminate on the first
// pass's Return without ever waiting for a keystroke, making confirmation a
// tautology; a fresh seq() read would discard everything typed between the
// first Return and that read.
//
// # Safeguards
//
// While this function runs, input for the entire system is suppressed at
// kCGHIDEventTap, Ctrl-C included — it never reaches the process, so the
// signal machinery cannot end the capture. Four things can:
//
//   - Escape (unmodified) — the voluntary exit, ErrCaptureCancelled;
//   - captureIdleTimeout (10s without a keystroke), per pass;
//   - captureTotalTimeout (60s), per pass;
//   - hotkey.MaxSteps + 1 presses without a Return, ErrCaptureTooLong.
//
// ctx is a fifth, for callers that install their own signal handler while
// the tap is up (cmd/dndmode does).
//
// # What it does not say
//
// Nothing here is logged: not a pass boundary, not a step count, not a
// duration. Two timestamped completion lines subtract to the exact length of
// the confirmation pass, and the timing of a secret is as much a secret as
// its length. The only line this function can emit is a Release failure,
// which describes the tap and not the input.
func CaptureConfirmed(ctx context.Context, prompt func(pass int), log *slog.Logger) ([]hotkey.Spec, error) {
	if log == nil {
		log = slog.Default()
	}

	r, baseSeq, err := installForCapture(log)
	if err != nil {
		return nil, err
	}
	// One defer for both passes. Release restores input, and its ring wipe
	// clears the C-side copy of what was just typed — which on this path is
	// the new secret in full, not merely the tail of a session.
	defer func() {
		if relErr := r.Release(); relErr != nil {
			log.Warn("eventtap: releasing the capture tap failed", slog.Any("err", relErr))
		}
	}()

	// newSnapshotFn owns a staging buffer and is not safe for concurrent
	// use. Constructed once here and used by both passes on this goroutine;
	// installForCapture started no poller, so there is no second reader.
	return confirmPasses(ctx, baseSeq, seq, newSnapshotFn(), time.Now, prompt, log)
}

// ErrCaptureMismatch means the two passes produced different sequences.
//
// Like every other capture sentinel it is a bare static string: saying WHERE
// they diverged, or that one was longer, would describe the secret the user
// just typed to a terminal they cannot yet type into.
var ErrCaptureMismatch = errors.New("eventtap: the two entries did not match")

// confirmPasses is CaptureConfirmed with the tap factored out: the two
// passes, the baseline hand-off between them, the comparison and the wiping,
// with every impure input (counter, ring, clock) arriving as a function
// parameter. That split is what makes the two-pass logic testable — a real
// CaptureConfirmed test would need a signed binary, an Accessibility grant
// and a human at the keyboard, and would take the machine's input with it.
func confirmPasses(
	ctx context.Context,
	baseSeq uint64,
	seqFn func() uint64,
	snapshot func([]matcher.KeyEvent) uint64,
	now func() time.Time,
	prompt func(pass int),
	log *slog.Logger,
) ([]hotkey.Spec, error) {
	if prompt != nil {
		prompt(1)
	}
	first, endSeq, err := collectSteps(ctx, baseSeq, seqFn, snapshot, now, log)
	if err != nil {
		return nil, err
	}

	if prompt != nil {
		prompt(2)
	}
	// endSeq, not baseSeq and not seqFn() — see the Baselines section on
	// CaptureConfirmed. Anything typed between the first Return and this
	// point is already in the ring at or past endSeq, so it counts towards
	// the second pass rather than being dropped.
	second, _, err := collectSteps(ctx, endSeq, seqFn, snapshot, now, log)
	if err != nil {
		clear(first)
		return nil, err
	}

	if !slices.Equal(first, second) {
		// Both, immediately. A mismatch means one of them is the secret the
		// user meant and the other is a typo, and there is no way to tell
		// which — so neither may outlive this branch.
		clear(first)
		clear(second)
		return nil, ErrCaptureMismatch
	}

	// The two are equal, so `second` is a redundant copy of the secret.
	// `first` is returned and therefore not wiped here; its owner is the
	// caller, which hashes it and drops it.
	clear(second)
	return first, nil
}
