//go:build darwin

package eventtap

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// pollInterval is the cadence at which the poller goroutine reads the
// `matched` atomic.Bool. 10ms balances two competing constraints:
//
//   - Latency budget (Phase 4 success criterion #2): hotkey press → Exiting
//     within 50ms. The poller adds at most one pollInterval (10ms worst case)
//     between the C callback setting `matched=true` and the Go-side
//     `sink <- struct{}{}` send; the remaining 40ms covers supervisor fan-in,
//     ctx-watcher, cocoa.RunApp wake, and main goroutine LIFO cleanup.
//   - CPU overhead: 100 polls/s on an idle process is roughly 0.01% CPU on
//     M-series silicon (one atomic.Load + one tick = ~50ns combined).
//
// Tied to the design notes kept as an untyped const so unit-test fixtures in
// tap_test.go can reference it for tighter timeout assertions than the
// 50ms phase-level success criterion (they typically use 5×pollInterval =
// 50ms timeouts to absorb scheduler jitter).
const pollInterval = 10 * time.Millisecond

// pollMatched is the fan-out goroutine body that watches the `matched`
// atomic.Bool (flipped to true by the C callback via the //export
// `eventtap_matched` helper) and forwards a single struct{} signal to `sink`
// on each rising edge.
//
// Lifecycle:
//
//   - `stop` channel close → goroutine returns cleanly. Release() closes
//     this channel after CGEventTapEnable(false) so the callback can no
//     longer set the flag.
//   - Each ticker fire reads `flag.CompareAndSwap(true, false)` — atomic
//     read-and-reset. The reset is critical: without it, a single C-callback
//     match would result in the goroutine sending to `sink` on every
//     subsequent tick until Release. With CAS, each match results in
//     exactly one sink send; the supervisor's exit trigger semantics (read
//     once, then ignored) match this exactly.
// - `sink <- struct{}{}` is non-blocking (select-default per). If
//     `sink` is full (capacity 1; supervisor already pending exit), the
//     send is dropped silently — the goal has already been accomplished
//     and queueing duplicate signals adds no value.
//
// Function is pure Go (no cgo) so unit tests can exercise it directly with
// a non-package-global `*atomic.Bool` and a synchronous sink channel
// without standing up CGEventTap. The package-global `matched` is only
// referenced by the Install path; tests construct their own bool.
//
// Logging: a successful send is logged at INFO level so production runs
// have one observable event between "hotkey pressed" and "Exiting". Per
// the C callback itself never logs — but the poller is Go-side
// (post-tap) and is the canonical place to record the match. The lookup of
// `log == nil` falls back to slog.Default() so production cannot accidentally
// silence this.
func pollMatched(stop <-chan struct{}, flag *atomic.Bool, sink chan<- struct{}, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Atomic read-and-reset. If the flag was true, we are the
			// caller that "consumed" the match — only we send to `sink`.
			// Concurrent callers (there should be none — Install spawns
			// exactly one poller per tap) would see the CAS fail and skip
			// the send.
			if !flag.CompareAndSwap(true, false) {
				continue
			}
			// Non-blocking send per. Supervisor's ExitTrigger has
			// cap=1 and is already read-side sync.Once-guarded
			// (Phase 1 — supervisor.go). A failed send means the
			// supervisor has already received an exit signal from some
			// other source (Ctrl-C, screen watchdog, etc.) and queueing
			// a duplicate signal adds no value.
			select {
			case sink <- struct{}{}:
				log.Info("eventtap: matched hotkey, signalling exit")
			default:
				// Sink full — supervisor already pending exit. Drop the
				// signal silently; logging here would be misleading
				// (suggests a problem when in fact we're racing with
				// another exit path that already won).
			}
		}
	}
}
