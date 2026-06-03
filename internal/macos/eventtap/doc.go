// Package eventtap wraps `CGEventTapCreate` at `kCGHIDEventTap` level via
// cgo, providing HID-level keyboard/mouse input blocking for Phase 4 of
// dndmode. The package is the single entry point to CGEventTap, the watchdog
// keeping the tap alive (`dispatch_source_t` 5s timer), and the NSWorkspace
// wake observer that re-arms the tap after system sleep / fast user switch.
//
// Public API (fully implements; here ships skeleton + sentinel
// errors + pure-Go watchdog policy):
//
//	func Install(spec hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error)
//
// `Install` creates the tap, registers the wake observer, starts the watchdog,
// and spawns the polling goroutine that reads the atomic `matched` flag (set
// by the //export Go callback `eventtap_matched`) and forwards it to `sink`.
// The returned `*Releaser` satisfies `state.Releaser` (`Release() error` +
// `Name() string`) and is pushed onto the `RestoreState` LIFO chain in
// `cmd/dndmode/main.go` Step 16, replacing the Phase 3 `mock-tap` placeholder.
//
// # Threading invariants (CRITICAL)
//
//   - `Install` and `Releaser.Release` MUST be called from the main goroutine
//     (the one locked to OS thread #0 by `internal/runtimepin/init()`). The C
//     side touches `CFRunLoopGetMain()` and AppKit notification center, both
//     of which are main-thread-only APIs.
//   - The cgo callback `eventtap_callback` (`tap_darwin.m`) fires on a worker
//     thread owned by the CGEventTap CFRunLoop. It MUST NOT allocate Go memory
// or block on a channel send. Per, the callback writes ONLY to the
//     package-level `atomic.Bool matched` via the //export Go helper
// `eventtap_matched` — body is exactly `matched.Store(true)`.
// enforces this via a gold-test on the file's contents.
//   - The poller goroutine that drains `matched` and forwards to `sink` is
//     a separate goroutine pinned to its own OS thread via
//     `runtime.LockOSThread()` (D-02). It uses `time.Ticker(10ms)` and a
//     non-blocking `select { case sink <- struct{}{}: default: }` so the
//     post-cancel send is safe even when the supervisor stopped reading.
// - The watchdog timer runs on a GCD high-priority dispatch queue
//     via `dispatch_source_t` (`DISPATCH_SOURCE_TYPE_TIMER`). It calls into
//     Go via `//export eventtap_watchdog_failed` only after `watchdogState`
//     has accumulated 5 consecutive `CGEventTapIsEnabled == false` probes
// (5 × 5s = 25s wall-clock). Healthy probes reset the counter.
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
// `[NSApp run]` after a hotkey match (parallel to Phase 2's `0xDED` stop
// path), because the active CGEventTap swallows all real input events and
// the run loop would otherwise stay starved (Phase 2). The
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
//   - internal/macos/cocoa/doc.go                            (canonical
//     subtype reservation table)
//   - internal/macos/powerassert/assertion.go                (two-layer
//     Releaser idempotency reference)
package eventtap
