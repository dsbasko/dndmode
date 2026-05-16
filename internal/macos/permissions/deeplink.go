//go:build darwin

package permissions

import "os/exec"

//go:generate mockgen -source=deeplink.go -destination=mocks/deeplink.go -package=mocks

// URLAccessibility is the x-apple.systempreferences deep-link URL that opens
// System Settings → Privacy & Security → Accessibility pane. macOS treats
// this URL scheme as stable across macOS 13/14/15 (verified per
// Phase 3 the design notes); changing it is a regression magnet, so the
// constant is asserted verbatim from a unit test.
//
// contract: WaitForGrants opens this URL exactly ONCE per process
// (only if AX is missing at polling-loop entry). Subsequent polling
// cycles do NOT re-open it — bringing the Settings window back to the
// foreground steals focus from the terminal and is jarring UX.
const URLAccessibility = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"

// URLInputMonitor is the x-apple.systempreferences deep-link URL that opens
// System Settings → Privacy & Security → Input Monitoring pane.
// uses IOHIDCheckAccess (silent — no TCC prompt), so the Input Monitoring
// grant has to be reached via the Settings UI rather than a system prompt
// dialog — this URL is the entry point.
//
// contract: WaitForGrants opens this URL exactly ONCE per process
// (only if IM is missing at polling-loop entry).
const URLInputMonitor = "x-apple.systempreferences:com.apple.preference.security?Privacy_ListenEvent"

// DeepLinker abstracts opening the System Settings panes that the user
// needs to visit in order to grant Accessibility / Input Monitoring. The
// production implementation (NewDeepLinker) routes through
// os/exec.Command("open", url).Start() — fire-and-forget, never blocking
// the caller. Tests inject a fake to assert call counts and to simulate
// "open" command failures (e.g. /usr/bin/open missing in a stripped chroot).
//
// Methods MUST be safe to call from any goroutine.
type DeepLinker interface {
	// OpenAX opens the System Settings → Privacy & Security → Accessibility
	// pane. Returns nil if the subprocess was started successfully (NOT if
	// the user actually granted permission — that is observed via
	// IsAccessibilityTrusted on subsequent polling cycles). Errors are
	// non-fatal at the call site: WaitForGrants logs warn and continues
	// polling.
	OpenAX() error

	// OpenIM opens the System Settings → Privacy & Security → Input
	// Monitoring pane. Same semantics as OpenAX.
	OpenIM() error
}

// execRunner is an unexported DI seam letting unit tests intercept the
// os/exec.Command(name, args...).Start() call without forking a real
// subprocess. Production wiring uses osExecRunner; tests use fakeExecRunner.
type execRunner interface {
	Start(name string, args ...string) error
}

// osExecRunner is the production execRunner: it wraps
// os/exec.Command(name, args...).Start() — fire-and-forget. Returns nil if
// the subprocess was successfully launched, or an error if /usr/bin/open is
// missing or argv is malformed.
//
// CRITICAL: must call .Start() (NOT .Run()). .Run() blocks until the
// Settings window is closed — the polling-loop would never enter its first
// cycle. The whole point of the deep-link is to nudge the user without
// halting our own process.
type osExecRunner struct{}

func (osExecRunner) Start(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// defaultLinker is the production DeepLinker. The execRunner field is the
// injection point that lets tests assert what we would have invoked.
type defaultLinker struct {
	r execRunner
}

// NewDeepLinker returns the production DeepLinker backed by os/exec.
// cmd/dndmode/main.go calls this once during PreFlight and hands the
// result to permissions.WaitForGrants.
func NewDeepLinker() DeepLinker {
	return &defaultLinker{r: osExecRunner{}}
}

// newDeepLinkerWithRunner is the test-only constructor: it wires a custom
// execRunner into the linker so unit tests can observe the (name, args...)
// tuples the linker would have passed to /usr/bin/open. Unexported to
// keep the testing surface clearly delimited.
func newDeepLinkerWithRunner(r execRunner) DeepLinker {
	return &defaultLinker{r: r}
}

func (l *defaultLinker) OpenAX() error {
	return l.r.Start("open", URLAccessibility)
}

func (l *defaultLinker) OpenIM() error {
	return l.r.Start("open", URLInputMonitor)
}
