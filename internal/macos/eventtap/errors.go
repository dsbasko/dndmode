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

// ErrWatchdogExitThreshold signals that the watchdog has observed
// `CGEventTapIsEnabled == false` in 5 consecutive 5-second probe cycles
// (5 × 5s = 25s wall-clock; D-09). Once the threshold is hit, the watchdog
// (Wave 1 04-03) sends this sentinel via the `sink` channel of `Install`
// and emits a stderr log line "eventtap watchdog: tap dead after 5 re-enable
// failures, exiting to restore input" (D-10).
//
// `main.go` reads this from the supervisor's `ExitTrigger()` path indirectly:
// the watchdog forwards the signal through the same channel that `matched`
// events use, and the supervisor cleanly unwinds the LIFO Cleanup chain.
// AFTER `sup.Wait()` returns, main.go reads the package-level
// `WatchdogTripped atomic.Bool` (CR-01 fix) to distinguish the watchdog
// path from a matched-hotkey path: on true → exit code 4
// (`exitSecureInputConflict` — the abnormal-platform-stop slot reused per
// D-10), on false → exit code 0 (`exitOK`). NOT a panic; the idempotent
// `Releaser.Release` path runs to completion before `os.Exit`. Before
// CR-01 the watchdog path silently collapsed to exit 0, masking the
// silent-disable failure from operators + LIFE-12 LiveChecker.
//
// Wave 0 ships the bare sentinel so the watchdog tests in this plan
// (TestWatchdog_Threshold_Triggers_AfterFiveConsecutiveFailures et al.)
// and Wave 1 04-03 implementation can both reference it without coordination.
var ErrWatchdogExitThreshold = errors.New("eventtap: tap dead after 5 re-enable failures")
