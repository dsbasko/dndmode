//go:build darwin

package eventtap

import (
	"log/slog"
	"time"

	"github.com/dsbasko/dndmode/internal/matcher"
)

// pollInterval is the cadence at which the poller goroutine probes the
// C-side keystroke counter. 10ms balances two competing constraints:
//
//   - Latency budget (Phase 4 success criterion #2): last key of the unlock
//     code → Exiting within 50ms. The poller adds at most one pollInterval
//     (10ms worst case) between the C callback recording the keystroke and
//     the Go-side `sink <- struct{}{}` send; the remaining 40ms covers
//     supervisor fan-in, ctx-watcher, cocoa.RunApp wake, and main goroutine
//     LIFO cleanup.
//   - CPU overhead: 100 polls/s on an idle process is roughly 0.01% CPU on
//     M-series silicon (one acquire-load through cgo + one tick). Ticks that
//     see no new keystroke — which is all of them on an idle machine — stop
//     at that load and never reach the ring snapshot.
//
// Tied to the design notes kept as an untyped const so unit-test fixtures in
// tap_test.go can reference it for tighter timeout assertions than the
// 50ms phase-level success criterion (they typically use 5×pollInterval =
// 50ms timeouts to absorb scheduler jitter).
const pollInterval = 10 * time.Millisecond

// ringCap is the number of key-press records the C-side keystroke ring
// holds, and ringMask is the index mask derived from it.
//
// ringCap MUST equal DND_RING_CAP in tap_ring.h — the Go snapshot buffer is
// sized from this constant while the memcpy on the C side is sized from
// that macro, so a divergence would either truncate the snapshot or overrun
// the Go buffer. ring_guard_test.go pins the two together (and pins ringCap
// to a power of two, which ringMask-based indexing requires) so the drift
// fails a unit test instead of corrupting memory at runtime.
const (
	ringCap  = 64
	ringMask = ringCap - 1
)

// clampFrom raises the lower bound of the sequence range the poller is
// allowed to read out of a ring snapshot, and reports whether it had to.
//
// A snapshot taken at sequence `to` can only speak for the last ringCap
// records, i.e. sequence numbers [to-ringCap, to). The writer keeps going
// during the memcpy, so the very oldest of those slots may already be
// half-overwritten by the time they are copied. Every window this poller
// assembles is `l` records long and ends at some `end` < to, so pinning the
// oldest readable window start one slot inside the ring — the oldest tail
// may begin at to-ringCap+1, never at to-ringCap — is what keeps a torn
// record out of the matcher.
//
// The arithmetic is deliberately written as "compare, THEN subtract":
//
//	if to <= ringCap-l { no clamp }        // safe: no subtraction happened
//	lo := to - (ringCap - l)               // only reached when to is big enough
//
// Doing it the other way round (`lo := to - (ringCap - l); if from < lo`)
// underflows for every `to` below ringCap-l, because these are uint64s:
// `lo` wraps to ~2^64, `from` is clamped above `to`, and matchAny's loop
// never executes. That is not a rare edge — it is the FIRST ~60 keystrokes
// of every single session, so the unlock code would simply not work until
// the ring had wrapped once. poller_test.go pins this case.
//
// The second return value is the "events were dropped" signal: true means
// the caller fell behind far enough that keystrokes aged out of the ring
// unread. pollSequence logs that fact (and only the fact — never the
// keystrokes) at DEBUG level.
func clampFrom(from, to, l uint64) (uint64, bool) {
	if l > uint64(ringCap) {
		// Unreachable in production: hotkey.MaxSteps*2 <= ringCap is pinned
		// by ring_guard_test.go and config validation rejects longer codes.
		// Handled anyway because the subtraction below would underflow, and
		// "no window fits in the ring" is the honest answer: report
		// everything as dropped and let the range collapse to empty.
		return to, true
	}
	if to <= uint64(ringCap)-l {
		return from, false
	}
	lo := to - (uint64(ringCap) - l)
	if from >= lo {
		return from, false
	}
	return lo, true
}

// matchAny reports whether the configured unlock code appears anywhere in
// the keystroke stream as a tail ending on one of the sequence numbers in
// [from, to), reading the records out of `ring` — a snapshot of the C-side
// keystroke ring, indexed by `seq & ringMask`.
//
// Why EVERY tail in the range and not just the newest one: the poller wakes
// on a 10ms ticker, and a snapshot routinely contains more than one new
// keystroke. If the user finishes the code and immediately presses one more
// key inside the same 10ms window, the newest tail is "code + stray key" —
// no match — and checking only that tail would silently drop the unlock.
// The same applies whenever the poller runs late. Checking every window
// that ends in the new range makes the match independent of how the
// keystrokes happen to be split across ticks, which is the only way a
// sliding-window code can behave deterministically.
//
// `tail` is a caller-owned scratch buffer that MUST be exactly m.Len()
// long; it is overwritten in place on every candidate window. Together with
// the caller-owned `ring` this keeps matchAny allocation-free, per the
// "no allocations in hot path" contract stated in matcher.go.
//
// Pure function — no cgo, no IO. Nothing about the keystrokes it reads is
// logged or returned; the only observable output is the boolean.
func matchAny(ring []matcher.KeyEvent, from, to uint64, tail []matcher.KeyEvent, m *matcher.Sequence) bool {
	l := uint64(m.Len())
	from, _ = clampFrom(from, to, l)

	for end := from; end < to; end++ {
		if end+1 < l {
			// Fewer than l keystrokes have happened in total — no window
			// ending here exists yet. Only possible at session start.
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

// pollSequence is the fan-out goroutine body for the unlock-code mechanism:
// it watches the C-side keystroke ring through the `seq` / `snapshot`
// accessors, matches the configured code in pure Go, and forwards a single
// struct{} signal to `sink` when the code is entered.
//
// Comparison used to happen inside the CGEventTap callback, which forced
// the callback to call back into Go on every match. Here the callback only
// appends to a static ring, and every decision is made on this goroutine.
//
// Lifecycle:
//
//   - `stop` channel close → goroutine returns cleanly. Release() closes it
//     after CGEventTapEnable(false), so no further records can appear.
//   - A successful match sends to `sink` and RETURNS. The unlock code is a
//     one-shot event: once the exit is signalled there is nothing left to
//     watch, and continuing to poll would keep matching the same tail on
//     every subsequent tick.
//   - `sink <- struct{}{}` is non-blocking (select-default). A full sink
//     (capacity 1) means the supervisor is already exiting via some other
//     path; a duplicate signal adds no value.
//
// Cost per tick: `seq()` is a bare acquire-load on the C side, and the
// `cur == lastSeq` early return skips the ring memcpy entirely. On an idle
// machine — which is the whole point of this tool — every tick takes that
// branch, so the steady-state cost is one cgo call per 10ms.
//
// `ring` and `tail` are allocated once, before the loop, and reused on
// every tick: nothing here allocates in the hot path.
//
// Logging discipline (this is a security property, not a style choice —
// see the silent-on-wrong-input stance in the project constraints):
//
//   - The successful match is logged at INFO, without the code.
//   - A ring overrun is logged at DEBUG as a bare fact, without the count
//     of lost keystrokes and without their content. "The poller fell
//     behind" is not evidence that anything in particular was typed.
//   - Nothing else is logged. There is no per-keystroke logging, no
//     progress ("2 of 6 steps"), no failed-match line — any of those would
//     leak the length or the timing of the secret into the log.
//
// `log == nil` falls back to slog.Default() so production cannot
// accidentally silence the match line.
func pollSequence(
	stop <-chan struct{},
	seq func() uint64,
	snapshot func([]matcher.KeyEvent) uint64,
	m *matcher.Sequence,
	sink chan<- struct{},
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	l := uint64(m.Len())
	ring := make([]matcher.KeyEvent, ringCap)
	tail := make([]matcher.KeyEvent, m.Len())

	// Zero both buffers on the way out, on BOTH exit paths (stop and match).
	// eventtap_wipe_ring clears the C ring so the just-typed unlock code
	// stops being resident after teardown, but `ring` holds a byte-identical
	// 64-record copy of it and `tail` holds the matched window itself —
	// leaving them behind would make that wipe half a measure. matcher.KeyEvent
	// is pointer-free, so the GC has no reason to zero these spans before
	// recycling them: the code would stay readable in the heap for an
	// unbounded time. On the match path this runs immediately after the sink
	// send, which is the tick whose tail IS the secret.
	defer clear(ring)
	defer clear(tail)

	// Start from whatever the counter already reads rather than from 0:
	// keystrokes that predate this goroutine are not part of this session's
	// input. Install zeroes the counter, so in production this is 0 anyway;
	// the read matters only if a tap is ever re-armed over a live ring.
	lastSeq := seq()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cur := seq()
			if cur == lastSeq {
				// No keystroke since the last tick — the overwhelmingly
				// common case. Skip the snapshot memcpy entirely.
				continue
			}

			// Re-read the counter as part of the snapshot: the memcpy is
			// not atomic with the load above, and the value returned
			// alongside the copy is the one that describes it.
			cur = snapshot(ring)

			if _, dropped := clampFrom(lastSeq, cur, l); dropped {
				log.Debug("eventtap: keystroke ring lagged; older events aged out unread")
			}

			if matchAny(ring, lastSeq, cur, tail, m) {
				select {
				case sink <- struct{}{}:
					log.Info("eventtap: unlock code matched, signalling exit")
				default:
					// Sink full — the supervisor is already exiting via
					// another path (Ctrl-C, watchdog, …). Dropping the
					// duplicate is correct and logging it would suggest a
					// problem where there is none.
				}
				return
			}

			lastSeq = cur
		}
	}
}
