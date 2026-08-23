//go:build darwin

package cocoa

import "errors"

// An empty [NSScreen screens] used to be a startup error here (ErrNoDisplays,
// mapped by main.go to exit 2). It is not one any more: reconcile treats zero
// displays as a headless start, and callers distinguish it via
// Controller.WindowCount() == 0. The sentinel was deleted rather than left
// exported-but-unreachable — an error value nothing can return is a lie in the
// package's API.

// ErrUnexpectedExit is returned by RunApp when [NSApp run] exits without
// ctx-driven cancellation — for example, an NSException inside AppKit, or
// somebody calling [NSApp terminate:nil] from a delegate. cmd/dndmode/main.go
// reacts by calling stopper.RequestStop("cocoa exit: " + err.Error()) so the
// supervisor unwinds the LIFO Cleanup chain.
var ErrUnexpectedExit = errors.New("cocoa: NSApp.run returned unexpectedly")
