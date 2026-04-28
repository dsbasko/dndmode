//go:build darwin

package cocoa

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>

extern void cocoa_init(void);
extern int  cocoa_run_app(void);
extern void cocoa_stop_app(int subtype);
*/
import "C"

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

// stopSubtype is the NSEventTypeApplicationDefined.subtype reserved for
// Phase 2's overlay-stop synthetic events (see doc.go subtype reservation
// table). Phase 4 will use 0xDF1 for hotkey events; Phase 5 0xDF5 for focus.
const stopSubtype = 0xDED

// ErrNotInitialized is returned by RunApp if it is called before a successful
// Init(). [NSApp run] without [NSApplication sharedApplication] crashes with
// an AppKit assertion; this sentinel converts that latent footgun into a
// clean Go error so future Phase 3-5 callers fail loudly during development
// rather than at runtime in production (WARNING guard).
//
// Symmetric to ErrUnexpectedExit: both indicate "RunApp cannot proceed";
// caller (cmd/dndmode/main.go) treats either as a fatal stop trigger.
var ErrNotInitialized = errors.New("cocoa: RunApp called before Init")

var (
	initOnce sync.Once
	initErr  error

	// initDone is set true atomically inside initOnce.Do AFTER Init's body
	// runs successfully (i.e. cocoa_init returned and observers registered
	// without error). RunApp checks this before invoking [NSApp run].
	// atomic.Bool is used (rather than a plain bool) so the read in RunApp
	// and the write in Init are race-free even though they are expected to
	// happen in the same main goroutine — defensive against future
	// Phase 4-5 callers who may invoke RunApp from a different goroutine.
	initDone atomic.Bool

	runMu     sync.Mutex
	runCancel context.CancelFunc
)

// Init does sharedApplication + setActivationPolicy:Prohibited + screen
// observer registration. Idempotent (sync.Once); subsequent calls return the
// initial result without re-running the body.
//
// MUST be called from the main goroutine. The main goroutine is locked to
// OS thread #0 by internal/runtimepin/init() (blank-imported first in
// cmd/dndmode/main.go).
//
// Returns nil on success; non-nil error if screen observer registration
// failed (NSApp setup itself does not throw — it always succeeds or panics
// at the AppKit level, which the C side does not catch in init).
func Init(log *slog.Logger) error {
	initOnce.Do(func() {
		C.cocoa_init()
		if rc := registerScreenObservers(); rc != 0 {
			initErr = errors.New("cocoa: failed to register screen observers")
			if log != nil {
				log.Error("cocoa init: register screen observers",
					slog.Int("rc", rc))
			}
			return
		}
		// Mark Init complete only on full success — RunApp's guard reads
		// this to refuse running [NSApp run] without prior sharedApplication.
		initDone.Store(true)
	})
	return initErr
}

// RunApp blocks the calling goroutine on [NSApp run] until either ctx is
// cancelled (clean exit, returns nil) or NSApp.run returns unexpectedly
// (NSException, somebody called [NSApp terminate:nil] from a delegate, etc.;
// returns ErrUnexpectedExit).
//
// Spawned ctx-watcher goroutine listens on ctx.Done() and on cancellation
// posts a synthetic NSEventTypeApplicationDefined event with subtype 0xDED
// + calls [NSApp stop:] to wake the run loop. Both Cocoa calls are
// documented thread-safe (Apple "Threading Programming Guide"; the design notes
//).
//
// Single-shot per process: a second concurrent call panics with
// "cocoa.RunApp: already running". This is a programming error, not a
// runtime condition — the design assumes one Cocoa run loop per process.
//
// MUST be called from the main goroutine; under the hood [NSApp run] is
// a strict main-thread API. main.go handles the contract:
// it is the caller and stays on the main goroutine throughout.
func RunApp(ctx context.Context) error {
	// WARNING guard: refuse to run if Init() never completed (or failed).
	// [NSApp run] without [NSApplication sharedApplication] is undefined
	// behaviour and historically AppKit-asserts the process — converting that
	// to a clean Go error lets the caller log + exit gracefully. Documented
	// for future Phase 3-5 wire-up that may add additional run paths.
	if !initDone.Load() {
		return ErrNotInitialized
	}
	runMu.Lock()
	if runCancel != nil {
		runMu.Unlock()
		panic("cocoa.RunApp: already running")
	}
	ctxWatcher, cancel := context.WithCancel(ctx)
	runCancel = cancel
	runMu.Unlock()

	// ctx-watcher goroutine. Runs on an arbitrary Go-scheduler-chosen OS
	// thread; that's fine because postEvent + stop are thread-safe.
	go func() {
		<-ctxWatcher.Done()
		C.cocoa_stop_app(C.int(stopSubtype))
	}()
	defer cancel()

	rc := C.cocoa_run_app()

	runMu.Lock()
	runCancel = nil
	runMu.Unlock()

	if rc != 0 {
		return ErrUnexpectedExit
	}
	if ctx.Err() != nil {
		return nil // ctx-driven (expected) exit
	}
	// [NSApp run] returned without ctx-cancellation AND without exception —
	// means somebody (delegate, system) called [NSApp stop:] outside our
	// control path. Treat as unexpected.
	return ErrUnexpectedExit
}
