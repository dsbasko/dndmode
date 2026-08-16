package eventtap

import "errors"

// ErrTapInstallFailed is returned by Install when
// `CGEventTapCreate(kCGHIDEventTap, kCGHeadInsertEventTap,
// kCGEventTapOptionDefault, ...)` returns NULL. The three known triggers are:
//
//  1. Accessibility permission missing or revoked between
//     `permissions.IsTrusted()` (Step 9-10 in main.go) and `Install` (Step 16).
// The Phase 3 binary identity caveat (eclecticlight.co
//     "Apple Silicon signed code requirement") applies here — every `go
//     install` produces a new ad-hoc identity which silently invalidates the
//     prior TCC grant.
//  2. SecureEventInput is active (some other process — Terminal in secure
//     mode, password fields focused, sudo prompt — holds the global lock
//     that suppresses HID-level taps).
//  3. Kernel out of mach ports (very rare; reported by Daniel Raffel TIL
//     2026-02-19 in long-running dev environments).
//
// install path wraps the raw cgo return-code into this sentinel
// via `fmt.Errorf("%w: ...", ErrTapInstallFailed, rc, hint)` so that
// `cmd/dndmode/main.go` can `errors.Is(err, eventtap.ErrTapInstallFailed)`
// and print a user-facing remediation message (re-grant Accessibility,
// check Activity Monitor for `secured` flag) before exiting.
//
// ships the bare sentinel so downstream tests + main.go can already
// reference it without waiting for the install implementation to land.
var ErrTapInstallFailed = errors.New("eventtap: CGEventTapCreate returned NULL (missing Accessibility, SecureEventInput, or kernel out of mach ports)")

// ErrEmptyUnlockCode is returned by InstallAll / installTapOnly when the
// `steps` slice is empty. It is a package-boundary guard, not the primary
// validation: config.ValidateUnlockCode already rejects a zero-step code with
// a user-facing message, and main.go never gets past Step 5b with one.
//
// The guard exists because the failure mode is silent and total rather than
// noisy. A zero-step matcher.Sequence has Len() == 0, so MatchTail is handed a
// zero-length tail, finds no step that disagrees, and reports a match — the
// first keypress of the session (any key) would tear the overlay down. An
// empty code must therefore fail the install loudly instead of producing a
// tap that unlocks itself.
//
// This became reachable only when the install signature moved from a single
// hotkey.Spec (always a value) to a []hotkey.Spec (nil-able); it is checked
// before CGEventTapCreate so no mach port is created on this path.
var ErrEmptyUnlockCode = errors.New("eventtap: unlock code has no steps")

// ErrTeardownUnclean is returned by InstallAll / installTapOnly — and by
// StartWatchdog, which re-checks the latch so the exported constructor is
// fenced off in its own right and not merely by its one current caller —
// when an EARLIER teardown in this process could not prove a C-side callback
// idle and the C-side statics are therefore not safe to reuse. Two drains
// can leave that state behind:
//
//   - the tap's own drain handshake, timing out on a path where the worker
//     run loop was alive (`eventtap_callback` may still be running);
//   - the watchdog's cancel-handler drain in `StartWatchdog`'s stop closure,
//     timing out with a GCD probe handler still in flight.
//
// See the `teardownUnclean` latch in tap_darwin.go for the full argument;
// the short form for the first is that installing again would memset the
// shared keystroke ring under a callback that may still be appending to it
// with plain stores, and would publish a `g_tap` that makes that callback's
// own teardown re-check read as "still current". For the second it is that a
// stale watchdog handler re-reads `g_gesture_tap` and writes a session-less
// threshold latch, so a second session would inherit a gesture port that
// handler never retained and a threshold trip that is not its own.
//
// Not reachable in production: `InstallAll` runs once per process and the
// only paths that can set the latch already end in an exit. It exists so the
// unreachable case is an explicit, testable error rather than a silent data
// race, and it is checked before CGEventTapCreate so no mach port is created
// on this path.
var ErrTeardownUnclean = errors.New("eventtap: previous teardown could not prove the tap callback idle; tap is not reinstallable in this process")

// Watchdog signalling contract (note):
//
// The watchdog has observed `CGEventTapIsEnabled == false` in 5 consecutive
// 5-second probe cycles (5 × 5s = 25s wall-clock). On threshold hit
// the watchdog emits a stderr log line "eventtap watchdog:
// tap dead after 5 re-enable failures, exiting to restore input" and
// sends a bare `struct{}` through the `sink` channel of `InstallAll`. The
// supervisor cannot distinguish this signal from a matched-hotkey send.
//
// To preserve the abnormal-platform-stop exit code, the watchdog
// also flips the package-internal `watchdogTripped atomic.Bool` to true
// BEFORE the sink send. `cmd/dndmode/main.go` reads
// `eventtap.WatchdogTrippedSinceLastStart()` AFTER `sup.Wait()` returns
// to dispatch between exit code 4 (`exitSecureInputConflict`, reused for
// the watchdog category) and exit code 0 (`exitOK`).
//
// NOT a panic; the idempotent `Releaser.Release` path runs to completion
// before `os.Exit`. Before the fix the watchdog path silently
// collapsed to exit 0, masking the silent-disable failure from operators
// the LiveChecker.
//
// history: a typed `var ErrWatchdogExitThreshold = errors.New(...)`
// sentinel was previously exported here so the watchdog could forward it
// through the sink channel as a typed signal. The fix chose option (b)
// — `atomic.Bool` latch + bare struct{} channel — making the sentinel
// dead exported code (no callers, no `errors.Is` reachability, still part
// of the public API surface). It was removed in; this docstring
// is its only remaining trace. If a future refactor switches to typed
// `ExitReason` channels (option (a) of 's original two suggested
// fixes), the sentinel can come back with a real caller.
