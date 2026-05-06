//go:build darwin

// Package permissions wraps the TCC-gated and Foundation-level platform
// probes that cmd/dndmode/main.go runs during PreFlight (Phase 3, steps
// 7..9). It owns the policy of "should we even bother starting dndmode on
// this machine" — arm64 + macOS 14+ static/dynamic platform check, the AX
// (Accessibility) + IM (Input Monitoring) TCC databases, and the
// Carbon-backed SecureEventInput probe. The polling-loop that waits for AX
// and IM grants lives here as well.
//
// Power-management assertions (IOPMAssertion / orphan cleanup) intentionally
// live in a separate package — internal/macos/powerassert — per:
// different frameworks (IOKit + PowerManagement vs ApplicationServices +
// Foundation + Carbon), different LDFLAGS, different cgo units, different
// failure modes. Co-locating them would produce one heavy cgo binary blob
// that links everything-and-the-kitchen-sink even on cold-path code paths.
//
// # Threading invariants
//
//   - All checker functions (IsAccessibilityTrusted, CheckInputMonitoring,
//     IsSecureEventInputActive, CurrentOSVersion, CurrentArch) are stateless
//     and thread-safe — they wrap stateless Apple APIs and may be called
//     from any goroutine.
//   - PromptAccessibility (the AXIsProcessTrustedWithOptions one-shot that
//     opens the system "please grant me Accessibility" dialog) is intended
//     to be called exactly once per process, at the entry of WaitForGrants
//. macOS dedups the dialog by binary identity anyway, but
//     repeated calls bring the Settings window back to the foreground and
//     steal focus from the terminal.
//   - WaitForGrants is ctx-cancellable; SIGINT/SIGTERM propagate through
// signal.NotifyContext and unwind the polling-loop with the
//     ctx.Err() returned by ctx.Done(). The caller maps to exit code 3
// (exitPermissionDenied).
//   - CheckPlatform is a pure-Go orchestrator that takes arch + version as
//     parameters; cgo lives in CurrentOSVersion only. Tests pass synthetic
//     OSVersion values to exercise all branches without leaving Go.
//
// # Sources
//
// - the design notes (Patterns 1-4)
// - the design notes
// - the design notes (#15 TCC cdhash,
//     #16 AXIsProcessTrustedWithOptions prompt, #17 AX vs IM distinct DBs)
package permissions
