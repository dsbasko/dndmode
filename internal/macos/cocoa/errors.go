//go:build darwin

package cocoa

import "errors"

// ErrNoDisplays is returned by controller.reconcile(coldStart=true) when
// [NSScreen screens] is empty at startup. cmd/dndmode/main.go uses
// errors.Is(err, cocoa.ErrNoDisplays) to detect this case and emit the
// user-facing stderr message before exiting with code 2.
//
// The exact stderr message printed by main.go (the design notes "Specific Ideas"):
//
//	"dndmode: no displays detected (lid closed without external monitor?). "
//	"Open the lid or connect a display, then re-run."
var ErrNoDisplays = errors.New("cocoa: no displays detected")

// ErrUnexpectedExit is returned by RunApp when [NSApp run] exits without
// ctx-driven cancellation — for example, an NSException inside AppKit, or
// somebody calling [NSApp terminate:nil] from a delegate. cmd/dndmode/main.go
// reacts by calling stopper.RequestStop("cocoa exit: " + err.Error()) so the
// supervisor unwinds the LIFO Cleanup chain.
var ErrUnexpectedExit = errors.New("cocoa: NSApp.run returned unexpectedly")
