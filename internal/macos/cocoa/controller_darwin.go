//go:build darwin

package cocoa

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// screenEnumerator returns the current list of CGDirectDisplayIDs. The
// production implementation uses cgo to call [NSScreen screens]; tests
// inject a fake to drive reconcile transitions without a GUI session.
type screenEnumerator interface {
	Enumerate() []uint32
}

// windowFactory creates and closes per-display NSWindows. Production impl
// wraps createOverlayWindow / closeOverlayWindow from window_darwin.go;
// tests inject a fake that records calls and can simulate failures.
type windowFactory interface {
	Create(displayID uint32) (unsafe.Pointer, error)
	Close(w unsafe.Pointer)
}

// cgoScreenEnumerator is the production implementation backed by
// cocoa_enumerate_screens (screens_darwin.m).
type cgoScreenEnumerator struct{}

func (cgoScreenEnumerator) Enumerate() []uint32 { return enumerateScreens() }

// cgoWindowFactory is the production implementation backed by
// cocoa_create_overlay_window / cocoa_close_overlay_window (window_darwin.m).
type cgoWindowFactory struct{}

func (cgoWindowFactory) Create(displayID uint32) (unsafe.Pointer, error) {
	return createOverlayWindow(displayID)
}
func (cgoWindowFactory) Close(w unsafe.Pointer) { closeOverlayWindow(w) }

// observerRegistrar manages the screen-reconfig observer lifecycle.
// Production impl calls registerScreenObservers / unregisterScreenObservers;
// tests inject a fake to assert register/unregister call counts.
type observerRegistrar interface {
	Register() int
	Unregister() int
}

type cgoObserverRegistrar struct{}

func (cgoObserverRegistrar) Register() int   { return registerScreenObservers() }
func (cgoObserverRegistrar) Unregister() int { return unregisterScreenObservers() }

// Controller manages the per-display overlay NSWindows for Phase 2.
// Implements state.Releaser (Release() error + Name() string), so it can be
// pushed into the RestoreState LIFO Cleanup chain by cmd/dndmode/main.go.
//
// Threading invariants:
//   - NewController, CreateWindowsForAllScreens, Release, reconcile MUST
//     execute on the main goroutine. NewController/CreateWindowsForAllScreens
//     are called from main.go directly. reconcile is called either directly
//     (cold-start) or from onScreensChanged → DispatchMain (debounce path).
//     Release is called from defer rs.Cleanup() on the main goroutine.
//   - Internal mu: protects the windows map and debouncer pointer.
//   - released (atomic.Bool) + releaseOnce (sync.Once): two-layer idempotency
// per + Phase 1 mock_releaser.go pattern.
type Controller struct {
	log *slog.Logger

	mu          sync.Mutex
	windows     map[uint32]unsafe.Pointer // displayID → boxed NSWindow*
	debouncer   *time.Timer
	debounceWin time.Duration

	released    atomic.Bool
	releaseOnce sync.Once

	// Dependency injection seams for unit tests. Production callers use
	// NewController which wires the cgo-backed implementations.
	screens   screenEnumerator
	windowsOf windowFactory
	observers observerRegistrar

	// onScreensChangedFn is the &c.onScreensChanged closure passed to
	// setOnScreensChanged. We hold it in a field so Release can pass nil
	// to detach symmetrically.
	onScreensChangedFn func()
}

// NewController constructs the production Controller backed by cgo. Logger
// fallback is slog.Default() if nil (mirrors state.NewRestoreState lines
// 31-35). The returned Controller has registered its onScreensChanged
// callback with the package-level activeOnScreensChanged registry.
func NewController(log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	return newControllerWithDeps(log,
		cgoScreenEnumerator{},
		cgoWindowFactory{},
		cgoObserverRegistrar{},
		250*time.Millisecond,
	)
}

// newControllerWithDeps is the test-internal constructor allowing fake
// dependencies. NOT exported. Test files in the same package use it directly.
func newControllerWithDeps(
	log *slog.Logger,
	screens screenEnumerator,
	windowsOf windowFactory,
	observers observerRegistrar,
	debounceWin time.Duration,
) *Controller {
	c := &Controller{
		log:         log,
		windows:     make(map[uint32]unsafe.Pointer),
		debounceWin: debounceWin,
		screens:     screens,
		windowsOf:   windowsOf,
		observers:   observers,
	}
	c.onScreensChangedFn = c.onScreensChanged
	setOnScreensChanged(&c.onScreensChangedFn)
	return c
}

// Name implements state.Releaser. The string "windows" is observable in
// the LIFO cleanup stderr log (acceptance test asserts ordering).
func (c *Controller) Name() string { return "windows" }

// CreateWindowsForAllScreens performs the cold-start reconcile. Returns
// ErrNoDisplays if [NSScreen screens] is empty — main.go maps that
// to exit code 2 + user-facing stderr message.
//
// MUST be called from the main goroutine BEFORE the "active" banner
// (ordering).
func (c *Controller) CreateWindowsForAllScreens() error {
	return c.reconcile(true)
}

// WindowCount returns the current size of the windows map under lock.
// Used by smoke tests to verify count == EnumerateScreensCount.
func (c *Controller) WindowCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.windows)
}

// reconcile is the single reconcile body shared between cold-start
// (CreateWindowsForAllScreens, coldStart=true) and runtime hot-plug
// (onScreensChanged → DispatchMain → reconcile, coldStart=false).
//
// full rebuild semantics: destroy all existing windows, enumerate
// current screens, create one window per screen. No incremental diff;
// forbade it.
//
// cold-start: 0 screens → return ErrNoDisplays.
// runtime: 0 screens → log.Warn, return nil, leave map empty.
// failure on createOverlayWindow: destroy all already-created in this
//
//	reconcile + return error. Caller decides what to do with it (cold-start
//	propagates to main.go → exit; runtime hot-plug logs and waits for next
//	event).
//
// MUST be called on the main goroutine.
func (c *Controller) reconcile(coldStart bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Step 1: destroy all current windows (full rebuild).
	for id, w := range c.windows {
		c.windowsOf.Close(w)
		delete(c.windows, id)
	}

	// Step 2: enumerate current screens.
	ids := c.screens.Enumerate()

	if len(ids) == 0 {
		if coldStart {
			return ErrNoDisplays // 
		}
		c.log.Warn("all displays disconnected; overlay invisible until hot-plug returns")
		return nil // 
	}

	// Step 3: create one NSWindow per screen.
	for i, id := range ids {
		w, err := c.windowsOf.Create(id)
		if err != nil {
			// abort whole reconcile. Destroy any windows already
			// created in this loop iteration (those at indices 0..i-1
			// which were stored in c.windows in earlier iterations).
			for stored, ww := range c.windows {
				c.windowsOf.Close(ww)
				delete(c.windows, stored)
			}
			return fmt.Errorf("create overlay window for displayID=%d (screen %d/%d): %w",
				id, i+1, len(ids), err)
		}
		c.windows[id] = w
	}
	return nil
}

// onScreensChanged is invoked by goCocoaOnScreensChanged (screens_darwin.go)
// every time the system reports a screen reconfiguration (NSNotif or
// CGDisplay callback, both routed through dispatch_async to main).
//
// We debounce 250ms trailing-edge: any rapid burst of events
// collapses into a single reconcile call after the burst settles.
//
// mitigation: Stop() the existing debouncer before creating a
// new AfterFunc — even if the prior one already fired, Stop is a safe no-op,
// and we avoid the time.Timer.Reset semantics gotcha (Reset on expired
// timer with un-drained channel is undefined per stdlib docs).
func (c *Controller) onScreensChanged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.debouncer != nil {
		c.debouncer.Stop()
	}
	c.debouncer = time.AfterFunc(c.debounceWin, func() {
		DispatchMain(func() {
			if err := c.reconcile(false); err != nil {
				c.log.Error("hot-plug reconcile failed", slog.Any("err", err))
			}
		})
	})
}

// Release implements state.Releaser. Two-layer idempotency:
//  1. atomic.Bool fast path (single CAS) — second-call no-op.
//  2. sync.Once around the body — defense in depth.
//
// Internal cleanup ordering:
//  1. debouncer.Stop() — prevents a late timer firing reconcile mid-Release.
//  2. setOnScreensChanged(nil) — detaches the package-level callback so any
//     already-dispatched goCocoaOnScreensChanged becomes a no-op.
//  3. unregisterScreenObservers via DispatchMain — must be on main thread.
//  4. close all windows via DispatchMain — must be on main thread.
//
// All Cocoa ops are routed through DispatchMain: inline if already
// on main (typical: defer rs.Cleanup() runs after [NSApp run] returns,
// already on main), otherwise dispatch_async.
func (c *Controller) Release() error {
	if !c.released.CompareAndSwap(false, true) {
		return nil // first idempotency layer (mock_releaser.go pattern)
	}
	var releaseErr error
	c.releaseOnce.Do(func() {
		// 1. Cancel debouncer (Go-side, no Cocoa).
		c.mu.Lock()
		if c.debouncer != nil {
			c.debouncer.Stop()
			c.debouncer = nil
		}
		c.mu.Unlock()

		// 2. Detach Go callback BEFORE unregistering C observers so any
		// in-flight dispatched event sees nil and silently returns.
		setOnScreensChanged(nil)

		// 3+4. Cocoa ops on main thread (inline-fast-path if already on main).
		DispatchMain(func() {
			if rc := c.observers.Unregister(); rc != 0 {
				releaseErr = fmt.Errorf("unregister screen observers: rc=%d", rc)
			}
			c.mu.Lock()
			for id, w := range c.windows {
				c.windowsOf.Close(w)
				delete(c.windows, id)
			}
			c.mu.Unlock()
		})
	})
	return releaseErr
}

// Compile-time check: Controller satisfies state.Releaser without import
// loop (state imports nothing from cocoa, but cocoa.Controller is held by
// main.go as state.Releaser). Mismatch surfaces here at build time.
var _ interface {
	Release() error
	Name() string
} = (*Controller)(nil)
