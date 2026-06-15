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
