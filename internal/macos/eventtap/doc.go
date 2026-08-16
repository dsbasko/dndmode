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
//     there, before the ring memcpy. Its starting point is a baseline
//     PARAMETER, sampled by Install right after the ring was zeroed and
//     before the tap source joins any run loop — the last instant at which
//     the counter is provably quiescent. Sampling it inside the goroutine
//     instead would discard everything typed between the tap going live and
//     the goroutine being scheduled, and a code typed there would be
//     swallowed with no diagnostic.
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
//     `internal/runtimepin/init()`). The reason is the wake subsystem, not
//     the tap: `wake_darwin.m` registers on `NSWorkspace`'s notification
//     center and confines observer delivery to `[NSOperationQueue
//     mainQueue]`, so its lifetime argument — an observer block can never
//     run concurrently with `Release`, because the main thread is *inside*
//     `Release` — only holds while both calls happen on that thread. (The
//     tap's own run loop is NOT the main one: `tap_darwin.m` attaches the
//     source to `CFRunLoopGetCurrent()` on the worker thread.)
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
//     are disabled, and never from the poller. Disabling is necessary but
//     NOT sufficient: `CGEventTapEnable(tap, false)` carries no documented
//     callback-drain guarantee, so an already-dispatched callback keeps
//     running and would both race the memset and repopulate the ring after
//     it. The two wipes that can have a live worker loop therefore reach it
//     differently, because the tap source is attached at one of them and
//     detached at the other:
//     the uninstall-time wipe runs after `CFRunLoopRemoveSource` plus a
//     confirmed `eventtap_drain_worker_callbacks` handshake — with the
//     source off the loop, "no callback is running" also means "none can
//     start", so the drain is conclusive;
//     the Release Step 1 wipe cannot rely on a drain at all (a mach message
//     already queued on the still-attached tap port can produce a callback
//     after any handshake) and instead runs AS a block on the worker loop
//     via `eventtap_wipe_ring_on_worker`. The callback is a run-loop callout
//     on that same thread and a queued block is executed by it, so the two
//     are serialised by the thread and cannot overlap. Both helpers report a
//     handshake timeout to their caller rather than swallowing it, and both
//     answer it the same way: on a timeout NOTHING callback-visible is
//     touched non-atomically. `eventtap_uninstall_c` skips the CFReleases
//     (leaking the mach port instead of freeing one a live callback may be
//     executing against) AND skips its tail wipe (the callback's ring append
//     is a pair of plain stores, so an overlapping memset is a data race,
//     not a benign one); `eventtap_wipe_ring_on_worker` simply does not
//     wipe, with no direct-memset fallback. The one write that still happens
//     on the timeout path is `g_tap = NULL`, and it — like every access to
//     `g_tap` and `g_gesture_tap`, including the callbacks' self-heal reads
//     — goes through `__atomic_*`, which is what makes it well-defined
//     rather than a race. The Go side logs the timeout at WARN on the
//     teardown paths where the worker loop is alive (a loop that services
//     blocks in microseconds failing to service one in 100ms is a fault),
//     and at DEBUG on the install-rollback path where the worker died
//     before `CFRunLoopRun` and a timeout is the expected outcome.
//     It also LATCHES those WARN paths (`teardownUnclean` in tap_darwin.go):
//     the C side keeps no record that a writer may still be outstanding, so
//     a later install would run `eventtap_wipe_ring`'s memset against a
//     callback still appending with plain stores and publish a fresh
//     non-NULL `g_tap` that the same callback's post-enable re-check would
//     read as "my tap is still current". Once set, this process refuses to
//     install a tap again (`ErrTeardownUnclean`) — free in production,
//     where `InstallAll` runs once and every latching path exits anyway.
//   - The watchdog timer runs on a GCD high-priority dispatch queue
//     via `dispatch_source_t` (`DISPATCH_SOURCE_TYPE_TIMER`). It calls into
//     Go via `//export eventtap_watchdog_failed` — the package's only
//     `//export`, and it is NOT reachable from the tap callback — after
//     `watchdogState` has accumulated 5 consecutive
//     `CGEventTapIsEnabled == false` probes (5 × 5s = 25s wall-clock).
//     Healthy probes reset the counter. `watchdog_stop` nils the source
//     before its BOUNDED drain, so a handler can outlive that wait; two
//     structural properties — not the generation check, which is one read at
//     the top and can be descheduled past — keep such a handler from
//     disturbing its own session's teardown. Its failure counter is
//     `__block`-local to that session, so its arithmetic reaches storage
//     nobody reads; and every tap access compares `g_observed_tap` against
//     the session's own retained `tap` by pointer IDENTITY, so a freshly
//     published pointer reads as "not mine" rather than as proof that the
//     stale tap is still current. The session generation (`g_watchdog_gen`)
//     remains as a cheap early-out on top of both. What those C-side guards
//     cannot cover is a LATER session, because two things the handler does
//     re-read process-global state after its last check: `gesturetap_reenable`
//     loads `g_gesture_tap` fresh, and `eventtap_watchdog_failed` flips a
//     latch carrying no session identity. So a timed-out drain latches
//     `teardownUnclean` on the Go side and this process refuses to install
//     again — a second session, and with it both effects, becomes
//     unreachable rather than merely narrow. `StartWatchdog` re-checks that
//     same latch on entry, so the refusal is a property of the watchdog
//     constructor and not merely of the one install path that calls it.
//     Symmetrically, the Go-side `stop` closure JOINS its threshold poller
//     rather than only signalling it, so the package-global latches that the
//     next `StartWatchdog` resets have no in-flight reader.
//   - The wake observer (`wake_darwin.m`) attaches to NSWorkspace
//     notifications `NSWorkspaceDidWakeNotification` +
//     `NSWorkspaceSessionDidBecomeActiveNotification` and calls
//     `CGEventTapEnable(tap, true)` from the AppKit notification thread.
//     Re-enable is idempotent — calling it on an already-enabled tap is a
//     no-op per Apple's documentation.
//   - Both of those subsystems exist to re-enable the taps, which makes
//     their teardown ORDER part of the memory-safety argument rather than a
//     matter of taste: `Releaser.Release` stops them (watchdog first, then
//     the wake observers) BEFORE it CFReleases either tap, and
//     `watchdog_stop` waits for its source's cancel handler so "stopped"
//     means "no handler is running and none can start". The shared
//     `g_observed_tap` NULL write earlier in Release is a guard for
//     handlers that have not started yet; it cannot reach one that already
//     loaded the pointer, which is why the ordering — plus a CFRetain the
//     watchdog holds on the tap for the lifetime of its source — is what
//     actually rules out a use-after-free.
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
