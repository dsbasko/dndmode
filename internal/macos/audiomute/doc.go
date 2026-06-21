//go:build darwin

// Package audiomute wraps the macOS AppleScript volume API
// (`/usr/bin/osascript`) for the dndmode session-mute lifecycle. It is a
// structural clone of the sibling focus package: a one-method-per-subprocess
// DI seam (VolumeRunner) plus a best-effort idempotent *Releaser, kept free of
// any import of the state package via the compile-time blank-identifier trick.
//
// # Why audio mute instead of Focus
//
// dndmode v1 enabled macOS Focus / Do Not Disturb on every start. Focus syncs
// across the user's Apple devices through iCloud ("Share Across Devices"), so
// starting dndmode on the Mac silently turned on DND on the user's iPhone.
// There is no API to enable Focus "on this device only". The overlay already
// sits at CGShieldingWindowLevel() — above NotificationCenter banner windows —
// so banners cannot leak visually; the only thing Focus actually contributed
// was silencing notification *sounds*. audiomute replaces that with a local
// system-output mute saved/restored around the session, leaving the iPhone
// untouched. Focus stays available as an opt-in (config `focus: true`).
//
// # No TCC permission required
//
// AppleScript `set volume output muted true|false` and
// `output muted of (get volume settings)` manipulate system output volume
// without any Accessibility or Automation prompt, so audiomute adds no new
// permission to the dndmode pre-flight.
//
// # Threading invariants
//
//   - VolumeRunner methods (GetMuted / SetMuted) are safe to call from any
//     goroutine. The production execVolumeRunner uses exec.CommandContext;
//     cancellation of ctx SIGKILLs the subprocess (Go's os/exec contract).
//   - GetMuted runs once on the main goroutine at lifecycle Step 13.3, before
//     the runtime.json snapshot is built. SetMuted(true) runs at Step 13.7;
//     the *Releaser's SetMuted(false) runs inside the LIFO Cleanup chain. The
//     Releaser's atomic.Bool + sync.Mutex idempotency defends against an
//     overlapping signal-handler Cleanup.
//
// # State.Releaser conformance
//
// *Releaser satisfies state.Releaser (Release() error + Name() string)
// without importing the state package — same compile-time blank-identifier
// interface assignment used in focus/releaser.go and powerassert/assertion.go.
package audiomute
