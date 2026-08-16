// Package eventtap wraps `CGEventTapCreate` via cgo, providing full input
// blocking for Phase 4 of dndmode with TWO taps:
//
//   - the PRIMARY tap at `kCGHIDEventTap` (tap_darwin.m) blocks every
//     keyboard / mouse / scroll / media event and records key presses into
//     the keystroke ring the unlock code is matched against;
//   - the GESTURE tap at `kCGSessionEventTap` (gesturetap_darwin.m)
//     suppresses trackpad multitouch gestures — Mission Control / App
//     Exposé / Spaces dock-swipes, pinches — which WindowServer synthesizes
//     PAST the HID tap point and delivers to the Dock as session-level CGS
//     events (private types 29/30). The HID tap can never see those, so
//     without the second tap a 3/4-finger swipe opens Mission Control over
//     the shield. Both tap sources are serviced by the same worker run
//     loop / locked OS thread.
//
// The package is the single entry point to CGEventTap, the watchdog keeping
// both taps alive (`dispatch_source_t` 5s timer; the main tap's health
// counter is the exit signal, the gesture tap is re-enabled on the same
// probe), and the NSWorkspace wake observer that re-arms both taps after
// system sleep / fast user switch.
//
// Public API:
//
//	func InstallAll(steps []hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error)
//
// `InstallAll` is THE production entry point. It creates the tap, starts
// the watchdog (GCD `dispatch_source_t` 5s timer), registers the wake
// observer (NSWorkspace DidWake + SessionDidBecomeActive), and spawns the
// polling goroutine that watches the keystroke ring for `steps` — the
// unlock code, resolved by `config.ResolveUnlockCode` from either
// `unlock_code` or the deprecated `hotkey` key, so a legacy single
// combination is simply a code of length 1. The returned `*Releaser`
// satisfies `state.Releaser` (`Release() error` + `Name() string`) and is
// pushed onto the `RestoreState` LIFO chain in `cmd/dndmode/main.go`
// Step 17, replacing the Phase 3 `mock-tap` placeholder.
//
// # Unlock mechanism: ring in C, matching in Go
//
// The comparison does NOT happen in the CGEventTap callback. The pipeline is:
//
//  1. `eventtap_callback` (tap_darwin.m) drops autorepeat, masks the event
//     flags with USER_INTENTIONAL_MASK, appends one `dnd_keyrec_t`
//     (`{flags, keycode}`, tap_ring.h) to a static ring of DND_RING_CAP=64
//     records at `seq & (DND_RING_CAP-1)`, and release-stores the
//     incremented press counter. Then it returns NULL like every other
//     event — the unlock keystroke is swallowed too, so it never leaks
//     into the app underneath.
//  2. The poller goroutine (poller.go) ticks every `pollInterval` (10ms)
//     and probes `seq()` — a bare acquire-load through cgo. Unchanged
//     counter (the steady state on an idle machine) means the tick ends
//     there, before the ring memcpy.
//  3. On a changed counter it calls `snapshot()`, which memcpys the whole
//     ring into a reused Go buffer and returns the press count that copy
//     describes, then runs `matchAny` — pure Go, allocation-free — over
//     every window ending in the newly-arrived range, comparing each
//     against `matcher.Sequence.MatchTail`.
//  4. A match sends one `struct{}` to `sink` (non-blocking) and the poller
//     returns: the unlock is a one-shot event.
//
// Matching is on the TAIL of the keystroke stream, so no "start typing"
// signal is needed and nothing is ever echoed back — the shield does not
// react to input at all, which is the silent-on-wrong-input stance the
// project constraints demand. `clampFrom` keeps windows whose oldest
// record may have been overwritten mid-memcpy out of the matcher; a lagging
// poller is reported at DEBUG as a bare fact, never with a count or a
// keystroke.
//
// Why this shape: the callback is the most safety-critical code in the
// tree (see the nosplit invariant below), so the design deliberately makes
// it SMALLER than the pre-sequence version rather than larger. Everything
// that can be a decision moved to an ordinary Go goroutine, where it is
// unit-testable without a keyboard: `poller_test.go` covers `matchAny` /
// `clampFrom` (including ring wrap-around and the pre-first-wrap boundary),
// `ring_guard_test.go` pins `ringCap` to DND_RING_CAP, and
// `nosplit_gold_test.go` pins the invariant on the .m source itself.
//
// history: this package previously also exported a bare `Install`
// helper that wired ONLY the tap + poller — the returned Releaser had nil
// watchdogStop + nil wakeStop, silently bypassing both subsystems with no
// compile-time or runtime warning. The bare entry point was unexported
// (renamed to `installTapOnly`) to remove the foot-gun; the only caller
// is the manual smoke test (`eventtap_smoketest_test.go`) living inside
// this package. Earlier still, the callback compared the event against an
// expected (flags, keycode) pair held in C globals and signalled a match
// through the //export Go helper `eventtap_matched`; both the globals and
// that export are gone.
//
// # Threading invariants (CRITICAL)
//
//   - `InstallAll` and `Releaser.Release` MUST be called from the main
//     goroutine (the one locked to OS thread #0 by
//     `internal/runtimepin/init()`). The C side touches
//     `CFRunLoopGetMain()` and AppKit notification center, both of which
//     are main-thread-only APIs.
//   - The cgo callback `eventtap_callback` (`tap_darwin.m`) fires on a
//     worker thread owned by the CGEventTap CFRunLoop. It MUST NOT allocate
//     Go memory, block on a channel send, or call into Go AT ALL. It makes
//     ZERO Go calls: its whole body is a field read, a mask, a write into
//     the static ring, and an atomic store. That is the strongest form of
//     the invariant — an //export symbol invoked from this thread could
//     trigger a stack split on a thread the Go runtime does not own.
//     `nosplit_gold_test.go` enforces it mechanically on the source:
//     `TestTapSource_DoesNotImportCgoExportHeader` asserts the file never
//     imports `_cgo_export.h` (so a Go call there cannot even compile), and
//     `TestEventTapCallback_CallsNoGoExports` asserts the callback body
//     names none of the package's `//export` symbols.
//   - The poller goroutine that watches the ring and forwards to `sink` is
//     a separate goroutine. It is NOT pinned to an OS thread, while the
//     worker goroutine that drives the CGEventTap CFRunLoop DOES use
//     `runtime.LockOSThread()`. The distinction is thread AFFINITY, not
//     "Go-only code": the poller does make cgo calls (`eventtap_seq`,
//     `eventtap_snapshot`), but neither touches anything thread-affine —
//     no CFRunLoop, no AppKit, no thread-local state. They are an atomic
//     load and a memcpy over a static array, correct from whichever thread
//     the Go scheduler happens to hand the goroutine. The run-loop worker,
//     by contrast, must stay on ONE thread because `CFRunLoopRun` is bound
//     to the thread that started it. The poller uses `time.Ticker(10ms)`
//     and a non-blocking `select { case sink <- struct{}{}: default: }` so
//     the post-cancel send is safe even when the supervisor stopped
//     reading.
//   - `eventtap_snapshot` copies the ring WITHOUT synchronising against the
//     callback, which is a deliberate, documented allowance (see its
//     comment in tap_darwin.m): the single-writer / naturally-aligned
//     layout makes a torn record benign, and `clampFrom` excludes the one
//     window that could contain it. This holds only while the callback is
//     the SOLE writer — which is why `eventtap_wipe_ring` (the Release-path
//     clear of the ring and its counter) may be called ONLY after both taps
//     are disabled, and never from the poller.
//   - The watchdog timer runs on a GCD high-priority dispatch queue
//     via `dispatch_source_t` (`DISPATCH_SOURCE_TYPE_TIMER`). It calls into
//     Go via `//export eventtap_watchdog_failed` — the package's only
//     `//export`, and it is NOT reachable from the tap callback — after
//     `watchdogState` has accumulated 5 consecutive
//     `CGEventTapIsEnabled == false` probes (5 × 5s = 25s wall-clock).
//     Healthy probes reset the counter.
//   - The wake observer (`wake_darwin.m`) attaches to NSWorkspace
//     notifications `NSWorkspaceDidWakeNotification` +
//     `NSWorkspaceSessionDidBecomeActiveNotification` and calls
//     `CGEventTapEnable(tap, true)` from the AppKit notification thread.
//     Re-enable is idempotent — calling it on an already-enabled tap is a
//     no-op per Apple's documentation.
//
// # Synthetic NSEventTypeApplicationDefined subtype reservation
//
// Phase 4 reserves subtype `0xDF1` in the canonical subtype table maintained
// in `internal/macos/cocoa/doc.go`. ** does not yet post this synthetic
// event** — will use it from the poller goroutine to wake
// `[NSApp run]` after an unlock-code match (parallel to Phase 2's `0xDED`
// stop path), because the active CGEventTap swallows all real input events
// and the run loop would otherwise stay starved (Phase 2). The
// actual shutdown wake-up in Phase 4 is delivered via the Phase 2 `0xDED`
// path through `supervisor.ExitTrigger()` → `cocoa.RunApp` ctx-watcher; the
// `0xDF1` slot stays reserved for future Phase 4+ extensions.
//
// # Sources
//
// - the design notes
// - the design notes (Patterns 1-7,
// callback skeleton, dispatch_source_t lifecycle, wake
//     observer pattern)
// - the design notes (validation
// matrix per /)
//   - internal/matcher/matcher.go                            (Sequence /
//     MatchTail — the pure-Go model this package feeds)
//   - internal/macos/eventtap/tap_ring.h                     (ring layout +
//     capacity, the single source of truth for DND_RING_CAP)
//   - internal/macos/cocoa/doc.go                            (canonical
//     subtype reservation table)
//   - internal/macos/powerassert/assertion.go                (two-layer
//     Releaser idempotency reference)
package eventtap
