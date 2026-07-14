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

// screenEnumerator returns the current list of CGDirectDisplayIDs plus a
// geometry Signature. The production implementation uses cgo to call
// [NSScreen screens]; tests inject a fake to drive reconcile transitions
// without a GUI session.
//
// Signature is a 64-bit change-detector over the FULL geometry of all screens
// (displayID + frame + backing scale). reconcile compares it across events to
// skip rebuilding a live overlay on a no-op reconfig — critically the menu-bar
// visibleFrame change from the Prohibited→Accessory activation flip at overlay
// start, which must NOT destroy the glass CABackdropLayer blur. Enumerate still
// supplies the ids the rebuild path keys windows by.
type screenEnumerator interface {
	Enumerate() []uint32
	Signature() uint64
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
func (cgoScreenEnumerator) Signature() uint64   { return screensGeometrySignature() }

// cgoWindowFactory is the production implementation backed by
// cocoa_create_overlay_window / cocoa_close_overlay_window (window_darwin.m).
// The style field (black|matrix|glass|terminal|dvd, already NormalizeOverlayStyle'd by main.go) is
// threaded into every Create — this keeps the style out of the windowFactory
// interface (Create(displayID uint32)), so the test fake and
// newControllerWithDeps signature are untouched (QUICK-gh8).
type cgoWindowFactory struct {
	style     string
	glassBlur float64 // CIGaussianBlur radius (points) for glass; ignored otherwise
	language  string  // source language for terminal (go|python|typescript|rust); ignored otherwise
}

func (f cgoWindowFactory) Create(displayID uint32) (unsafe.Pointer, error) {
	return createOverlayWindowStyled(displayID, f.style, f.glassBlur, f.language)
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

// mainDispatcher abstracts DispatchMain for unit-test injection. Production
// implementation routes through cocoa.DispatchMain (cgo + libdispatch); tests
// inject an inline-execute fake so that Release / debounce reconcile bodies
// run synchronously inside the test goroutine without requiring NSApp.run.
//
// This is a deliberate deviation from the original design: the
// plan called DispatchMain directly inside Release / onScreensChanged, which
// silently no-op'ed on test paths (async branch dispatches to the main queue
// that test runners do not pump). The seam preserves production behaviour
// while letting unit tests assert real Release / reconcile side-effects.
type mainDispatcher interface {
	Dispatch(fn func())
}

// cgoMainDispatcher is the production impl backed by cocoa.DispatchMain.
type cgoMainDispatcher struct{}

func (cgoMainDispatcher) Dispatch(fn func()) { DispatchMain(fn) }

// activationPolicy is the DI seam for the runtime Prohibited↔Accessory flip
// (revised). Production uses cgoActivationPolicy (NSApp setActivationPolicy
// via cgo); tests inject a fake that counts Foreground/Background. Mirrors the
// cursorHider seam — Foreground precedes the cursor Hide on overlay enter,
// Background follows the cursor Show on overlay teardown.
type activationPolicy interface {
	Foreground()
	Background()
}

// cgoActivationPolicy is the production implementation backed by appForeground /
// appBackground (app_darwin.m). Both MUST be invoked on the main goroutine.
type cgoActivationPolicy struct{}

func (cgoActivationPolicy) Foreground() { appForeground() }
func (cgoActivationPolicy) Background() { appBackground() }

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
//
// per + Phase 1 mock_releaser.go pattern.
type Controller struct {
	log *slog.Logger

	mu          sync.Mutex
	windows     map[uint32]unsafe.Pointer // displayID → boxed NSWindow*
	debouncer   *time.Timer
	debounceWin time.Duration

	// lastGeomSig / haveGeomSig cache the screen-geometry Signature of the most
	// recent rebuild. A runtime reconcile whose fresh Signature matches is a
	// no-op reconfig (e.g. the menu-bar visibleFrame change from the activation
	// flip at start) and is SKIPPED so the live overlay — and, for glass, its
	// CABackdropLayer blur — is not torn down and recreated. Guarded by c.mu.
	// haveGeomSig is false until the first rebuild records a baseline.
	lastGeomSig uint64
	haveGeomSig bool

	// cursorHidden guards the one-shot system-cursor hide. Set true after a
	// successful cold-start reconcile (CreateWindowsForAllScreens) hides the
	// cursor; reset false in Release after Show. Guarded by c.mu (D-design:
	// reuse the existing mutex, no new lock). Ensures Hide fires exactly once
	// per Active session (never per hot-plug rebuild) and Show fires only if a
	// hide actually happened.
	cursorHidden bool

	released    atomic.Bool
	releaseOnce sync.Once

	// Dependency injection seams for unit tests. Production callers use
	// NewController which wires the cgo-backed implementations.
	screens    screenEnumerator
	windowsOf  windowFactory
	observers  observerRegistrar
	dispatcher mainDispatcher
	cursor     cursorHider
	activation activationPolicy

	// onScreensChangedFn is the &c.onScreensChanged closure passed to
	// setOnScreensChanged. We hold it in a field so Release can pass nil
	// to detach symmetrically.
	onScreensChangedFn func()
}

// NewController constructs the production Controller backed by cgo. Logger
// fallback is slog.Default() if nil (mirrors state.NewRestoreState lines
// 31-35). The returned Controller has registered its onScreensChanged
// callback with the package-level activeOnScreensChanged registry.
//
// style selects the overlay look (black|matrix|glass|terminal|dvd, QUICK-gh8); the caller
// passes the NormalizeOverlayStyle'd value from config. glassBlur is the
// CIGaussianBlur radius (points) for the glass style (resolved by main.go from
// config glass_blur / the --style glass:N flag); it is ignored for every other
// style. language is the source language for the terminal style (go|python|
// typescript|rust, resolved by main.go from the --style terminal:<lang> suffix);
// ignored for every other style. All are threaded into the cgoWindowFactory so
// every per-display window is created with them, WITHOUT widening the
// windowFactory interface.
func NewController(style string, glassBlur float64, language string, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	return newControllerWithDeps(log,
		cgoScreenEnumerator{},
		cgoWindowFactory{style: style, glassBlur: glassBlur, language: language},
		cgoObserverRegistrar{},
		cgoMainDispatcher{},
		cgoCursorHider{},
		cgoActivationPolicy{},
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
	dispatcher mainDispatcher,
	cursor cursorHider,
	activation activationPolicy,
	debounceWin time.Duration,
) *Controller {
	c := &Controller{
		log:         log,
		windows:     make(map[uint32]unsafe.Pointer),
		debounceWin: debounceWin,
		screens:     screens,
		windowsOf:   windowsOf,
		observers:   observers,
		dispatcher:  dispatcher,
		cursor:      cursor,
		activation:  activation,
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
//
// On a successful cold-start reconcile it also flips the app activation policy
// to Accessory + active and hides the system mouse cursor exactly once. The
// activation flip (revised) MUST precede the hide: CGDisplayHideCursor is a
// no-op while the process is Prohibited (never the foreground app), so
// Foreground() runs first to make the hide take effect. Both are cosmetic to the
// shield (the WindowServer otherwise draws a stray arrow on the black overlay)
// and both live HERE, not in reconcile(), because reconcile is shared with the
// hot-plug rebuild path and would otherwise re-flip/re-hide on every replug. On
// ErrNoDisplays nothing is covered, so neither Foreground nor Hide fires
// and the error is propagated unchanged.
func (c *Controller) CreateWindowsForAllScreens() error {
	if err := c.reconcile(true); err != nil {
		return err // ErrNoDisplays (and any create failure) propagate; no foreground, no hide.
	}
	c.mu.Lock()
	// Foreground BEFORE Hide: the hide only works once the app is the active
	// (foreground) app. Reuse the single cursorHidden guard (design_decision
	// guard-reuse) — it now means "overlay active: we did Foreground + Hide".
	c.activation.Foreground()
	c.cursor.Hide()
	c.cursorHidden = true
	c.mu.Unlock()
	return nil
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

	// Step 1: enumerate screens + snapshot their geometry Signature BEFORE
	// touching the live windows, so a no-op reconfig bails out without teardown.
	ids := c.screens.Enumerate()
	sig := c.screens.Signature()

	// Step 2: no-op guard (runtime only). An unchanged geometry Signature means
	// nothing about the displays actually changed — the event is spurious, most
	// commonly the menu-bar visibleFrame change from the Prohibited→Accessory
	// activation flip fired right after cold-start (CreateWindowsForAllScreens).
	// Skip the full rebuild so the live overlay stays up; for overlay_style glass
	// this preserves the CABackdropLayer blur, which does NOT survive a
	// destroy+recreate. Cold-start never skips (haveGeomSig is false until the
	// first rebuild records a baseline). A real reconfig (resolution, mirror,
	// rearrange, connect/disconnect) changes the Signature and rebuilds as before.
	if !coldStart && c.haveGeomSig && sig == c.lastGeomSig {
		return nil
	}

	// Committed to a rebuild (or teardown): record the new baseline so the next
	// no-op event compares against it.
	c.lastGeomSig = sig
	c.haveGeomSig = true

	// Step 3: destroy all current windows (full rebuild — no incremental diff).
	for id, w := range c.windows {
		c.windowsOf.Close(w)
		delete(c.windows, id)
	}

	// Step 4: 0 screens → cold-start errors; runtime warns, leaves map empty.
	if len(ids) == 0 {
		if coldStart {
			return ErrNoDisplays //
		}
		c.log.Warn("all displays disconnected; overlay invisible until hot-plug returns")
		return nil //
	}

	// Step 5: create one NSWindow per screen.
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
		c.dispatcher.Dispatch(func() {
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
		c.dispatcher.Dispatch(func() {
			if rc := c.observers.Unregister(); rc != 0 {
				releaseErr = fmt.Errorf("unregister screen observers: rc=%d", rc)
			}
			c.mu.Lock()
			for id, w := range c.windows {
				c.windowsOf.Close(w)
				delete(c.windows, id)
			}
			c.mu.Unlock()

			// 5. Restore the system cursor and revert the activation policy iff
			// we hid/flipped (cursorHidden guard makes a Release-without-
			// successful-create a no-op). Show BEFORE Background: the cursor is
			// restored while the app is still foreground, then we drop back to
			// Prohibited (silent at-rest). The surrounding released-CAS +
			// releaseOnce.Do guarantee this closure runs exactly once, so Show +
			// Background each fire at most once (matching the single Foreground +
			// Hide on enter).
			c.mu.Lock()
			if c.cursorHidden {
				c.cursor.Show()
				c.activation.Background()
				c.cursorHidden = false
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
