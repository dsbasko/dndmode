//go:build linux

// Package overlay is the Linux shield: one borderless black GTK4 window per
// monitor, fullscreen on that monitor, with the pointer hidden over it. It
// works on Wayland and X11 alike. Plugging, unplugging or rearranging
// monitors rebuilds the shield after a 250 ms debounce.
//
// The shield is only the visual half of the lock: input is blocked by the
// evdev grab, so nothing can move, close or switch away from the windows.
//
// Threading mirrors the macOS cocoa package: Init and every GTK call run on
// the UI thread (the main goroutine, pinned to thread 0 by runtimepin);
// other goroutines reach it through DispatchMain.
//
// overlay_style black and dvd are implemented on Linux (dvd.go).
package overlay

/*
#cgo pkg-config: gtk4
#include <stdlib.h>
#include "overlay_linux.h"
*/
import "C"

import (
	"context"
	"errors"
	"log/slog"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"
)

// Sentinel errors. Use errors.Is.
var (
	// ErrNoDisplay: GTK could not open a display (no Wayland or X11
	// session reachable from this process).
	ErrNoDisplay = errors.New("overlay: cannot open a display (no WAYLAND_DISPLAY / DISPLAY?)")
	// ErrNotInitialized: RunApp before Init.
	ErrNotInitialized = errors.New("overlay: RunApp called before Init")
)

var (
	initOnce sync.Once
	initErr  error
	inited   bool

	runOnce sync.Once
)

// Init initialises GTK. It must be called on the main goroutine; repeated
// calls return the first result.
func Init(log *slog.Logger) error {
	initOnce.Do(func() {
		if C.overlay_init() != 0 {
			initErr = ErrNoDisplay
			return
		}
		inited = true
		if log != nil {
			log.Debug("gtk initialised")
		}
	})
	return initErr
}

// RunApp runs the GTK main loop on the calling (main) goroutine until ctx is
// cancelled. Single-shot per process.
func RunApp(ctx context.Context) error {
	if !inited {
		return ErrNotInitialized
	}
	ran := false
	runOnce.Do(func() { ran = true })
	if !ran {
		panic("overlay: RunApp called twice")
	}
	stop := context.AfterFunc(ctx, func() { DispatchMain(func() { C.overlay_quit() }) })
	defer stop()
	if ctx.Err() != nil {
		return nil
	}
	C.overlay_run()
	return nil
}

// Pump services the UI thread until done is closed. It is for the window
// after RunApp has returned while another goroutine still needs
// DispatchMain — a watch session tearing its shield down at process exit.
func Pump(done <-chan struct{}) {
	if !inited {
		<-done
		return
	}
	for {
		select {
		case <-done:
			return
		default:
		}
		if C.overlay_iterate() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

//export goOverlayIdle
func goOverlayIdle(h C.uintptr_t) {
	handle := cgo.Handle(h)
	fn := handle.Value().(func())
	handle.Delete()
	fn()
}

// DispatchMain runs fn on the UI thread: inline when already there,
// otherwise queued on the GTK main context.
func DispatchMain(fn func()) {
	if fn == nil {
		panic("overlay: DispatchMain(nil)")
	}
	if !inited || C.overlay_on_main_thread() != 0 {
		fn()
		return
	}
	C.overlay_dispatch(C.uintptr_t(cgo.NewHandle(fn)))
}

// dispatchSync runs fn on the UI thread and waits for it.
func dispatchSync(fn func()) {
	done := make(chan struct{})
	DispatchMain(func() {
		defer close(done)
		fn()
	})
	<-done
}

// Beep rings the display bell. Safe from any goroutine.
func Beep() {
	if !inited {
		return
	}
	DispatchMain(func() { C.overlay_beep() })
}

// active is the controller that receives monitor-change rebuilds. Only one
// shield exists at a time.
var (
	activeMu sync.Mutex
	active   *Controller
)

//export goOverlayMonitorsChanged
func goOverlayMonitorsChanged() {
	activeMu.Lock()
	c := active
	activeMu.Unlock()
	if c != nil {
		c.rebuild()
	}
}

// Controller owns the shield windows of one session. It satisfies
// state.Releaser.
type Controller struct {
	log       *slog.Logger
	style     string
	startupID string

	mu       sync.Mutex
	windows  []unsafe.Pointer
	released bool
}

// Styles lists the overlay styles this package draws.
var Styles = []string{"black", "dvd"}

// NewController returns a controller with no windows yet. style is one of
// Styles; anything else draws black. startupID is an xdg-activation token
// (may be empty); passing the one GNOME gave the `--activate` command lets
// the compositor raise the shield above the focused window without a
// focus-stealing refusal.
func NewController(style, startupID string, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	return &Controller{log: log, style: style, startupID: startupID}
}

var (
	dvdMaskOnce   sync.Once
	dvdMaskOK     bool
	dvdLogoAspect float64
)

// loadDVDMask hands the decoded logo to C once per process. UI thread only.
func loadDVDMask(log *slog.Logger) bool {
	dvdMaskOnce.Do(func() {
		alpha, w, h, err := dvdLogoAlpha()
		if err != nil {
			log.Warn("dvd logo unusable, drawing a plain black shield", slog.Any("err", err))
			return
		}
		C.overlay_set_dvd_mask((*C.uchar)(unsafe.Pointer(&alpha[0])), C.int(w), C.int(h))
		dvdLogoAspect = float64(w) / float64(h)
		dvdMaskOK = true
	})
	return dvdMaskOK
}

//export goDVDTick
func goDVDTick(h C.uintptr_t, boundsW, boundsH C.double, out *C.dvd_frame) {
	v := cgo.Handle(h).Value().(*dvdView)
	v.tick(float64(boundsW), float64(boundsH))
	c := v.st.color()
	*out = C.dvd_frame{
		x: C.double(v.st.x), y: C.double(v.st.y),
		w: C.double(v.st.w), h: C.double(v.st.h),
		r: C.double(c.r), g: C.double(c.g), b: C.double(c.b),
	}
}

//export goDVDFree
func goDVDFree(h C.uintptr_t) { cgo.Handle(h).Delete() }

// Name implements state.Releaser.
func (c *Controller) Name() string { return "windows" }

// CreateWindowsForAllScreens builds one shield per monitor and starts
// following monitor changes. Zero monitors is not an error: the shield
// appears when one is attached. Must run on the UI thread.
func (c *Controller) CreateWindowsForAllScreens() error {
	c.build()
	activeMu.Lock()
	active = c
	activeMu.Unlock()
	C.overlay_watch_monitors(1)
	return nil
}

// build creates a window per monitor. UI thread only.
func (c *Controller) build() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return
	}
	var sid *C.char
	if c.startupID != "" {
		sid = C.CString(c.startupID)
		defer C.free(unsafe.Pointer(sid))
	}
	style := C.int(C.OVERLAY_STYLE_BLACK)
	if c.style == "dvd" && loadDVDMask(c.log) {
		style = C.OVERLAY_STYLE_DVD
	}
	n := int(C.overlay_monitor_count())
	for i := range n {
		// Each monitor gets its own logo on its own path.
		var anim cgo.Handle
		if style == C.OVERLAY_STYLE_DVD {
			anim = cgo.NewHandle(&dvdView{logoAspect: dvdLogoAspect})
		}
		w := C.overlay_window_create(C.int(i), sid, style, C.uintptr_t(anim))
		if w == nil {
			if anim != 0 {
				anim.Delete()
			}
			continue
		}
		c.windows = append(c.windows, w)
	}
	c.log.Debug("shield windows created", slog.Int("monitors", n), slog.Int("windows", len(c.windows)))
}

// destroy closes every window. UI thread only.
func (c *Controller) destroy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.windows {
		C.overlay_window_destroy(w)
	}
	c.windows = nil
}

// rebuild re-creates the shield after a monitor change. Called on the UI
// thread by the debounce timer.
func (c *Controller) rebuild() {
	c.destroy()
	c.build()
}

// WindowCount reports how many shield windows exist.
func (c *Controller) WindowCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.windows)
}

// Release closes the shield. Idempotent; safe from any goroutine.
func (c *Controller) Release() error {
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return nil
	}
	c.released = true
	c.mu.Unlock()

	activeMu.Lock()
	if active == c {
		active = nil
	}
	activeMu.Unlock()

	dispatchSync(func() {
		C.overlay_watch_monitors(0)
		c.destroy()
	})
	return nil
}

// monitorCount reports the attached monitors; for tests, which cannot use
// cgo themselves.
func monitorCount() int { return int(C.overlay_monitor_count()) }
