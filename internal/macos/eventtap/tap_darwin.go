//go:build darwin

package eventtap

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework CoreGraphics -framework Foundation -framework ApplicationServices

#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

extern int  eventtap_install_c(uint64_t flags, uint16_t keycode, CFMachPortRef *out_tap);
extern int  eventtap_register_worker_runloop(CFMachPortRef tap, CFRunLoopRef *out_loop);
extern void eventtap_uninstall_c(CFMachPortRef tap);
extern int  eventtap_is_enabled(CFMachPortRef tap);
extern void eventtap_enable(CFMachPortRef tap, int enable);
extern int  eventtap_test_set_expected(uint64_t flags, uint16_t keycode);
*/
import "C"

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
)

// matched is the package-level latch flipped by the //export Go helper
// eventtap_matched, which is invoked from the CGEventTap C callback on its
// worker thread when (modifiers, keyCode) match the configured Spec.
//
// The poller goroutine (Wave 1 04-02; runtime.LockOSThread() + 10ms ticker)
// reads matched.Load() and, on true, performs a non-blocking
// `select { case sink <- struct{}{}: default: }` to the supervisor's
// ExitTrigger channel before atomically clearing the latch.
//
// atomic.Bool is the ONLY storage primitive permitted in the //export Go
// callback per (nosplit invariant; the design notes forbidden-operations
// list). No mutex, no channel, no slog — a single atomic store is the
// entire body of eventtap_matched.
var matched atomic.Bool

// eventtap_matched is the cgo entry point invoked from the C-side
// `eventtap_callback` in tap_darwin.m when an incoming CGEvent matches the
// configured (modifiers, keyCode). It fires on the CGEventTap worker thread
// — NOT main, NOT a Go-scheduled goroutine — so the body MUST satisfy the
// callback contract:
//
//   - No Go memory allocation (would race against the Go scheduler GC barrier
//     while the worker thread is not Go-scheduled).
//   - No blocking primitive (channel send, mutex lock, condition var).
//   - No syscall, no slog, no panic.
//   - Single atomic store is verified safe in production by
//     pqrs-org/osx-event-observer-examples and lwouis/alt-tab-macos (both
//     ship at high event-rates without races under `-race` builds — see
// the design notes reference patterns).
//
// Per INP-05, the Wave 1 04-02 plan acceptance gate verifies this body is
// EXACTLY `matched.Store(true)` and refuses any extension. The poller
// goroutine on the Go side does ALL the post-match work (forward to sink,
// clear the latch, log).
//
//export eventtap_matched
func eventtap_matched() {
	matched.Store(true)
}

// Releaser is the active CGEventTap handle and implements state.Releaser
// (Release() error + Name() string). It is produced by Install and consumed
// by main.go's RestoreState LIFO Cleanup chain (tap is released first
// to restore input, then the watchdog timer cancels, then the wake observer
// unsubscribes).
//
// Two-layer idempotency mirrors powerassert.Assertion (assertion.go lines
// 163-188): atomic.Bool fast-path Load short-circuits repeat Cleanup
// invocations; sync.Mutex slow-path serialises concurrent first-time
// callers so that NO caller returns before the underlying CGEventTapEnable
// / CFRelease / dispatch_source_cancel chain completes. This matters in
// the shutdown path where the ctx-watcher goroutine and the
// supervisor cleanup chain may both invoke Release nearly simultaneously.
//
// Field unsafe.Pointer'ы хранят raw `CFMachPortRef` / `CFRunLoopSourceRef` /
// `CFRunLoopRef` указатели в виде opaque pointers — Wave 1 04-02 будет
// делать `CFRelease` через cgo bridge. Хранить их прямо как `C.CFMachPortRef`
// в Go struct'е нельзя (cgo поля не сериализуются нормально между
// файлами + появляются false-positive в `go vet`).
type Releaser struct {
	// tap is the underlying CFMachPortRef returned by CGEventTapCreate.
	// Wave 1 04-02 populates this from eventtap_install_c's out_tap; nil
	// until install succeeds. Released via CFRelease in Release().
	tap unsafe.Pointer

	// source is the CFRunLoopSourceRef created from the tap and added to
	// the worker run loop. Released after CFRunLoopRemoveSource in Release.
	source unsafe.Pointer

	// workerLoop is the CFRunLoopRef of the dedicated poller-thread run
	// loop into which the tap source is inserted (D-02 — NOT main loop).
	// Stopped via CFRunLoopStop in Release before CFRelease on source/tap.
	workerLoop unsafe.Pointer

	// log is the slog.Logger used for warnings during Release (wake
	// recovery failure, watchdog threshold hit). Mirror of
	// powerassert.Acquire's logger-fallback convention: nil → slog.Default().
	log *slog.Logger

	// released is the fast-path hint flag — set to true AFTER the cgo
	// teardown chain has fully completed under mu. atomic.Load lets repeat
	// callers (e.g. ctx-watcher + Cleanup chain hitting Release in close
	// succession) skip the mutex entirely once teardown is permanently done.
	released atomic.Bool

	// mu serialises concurrent Release callers. Pre- style sync.Once
	// + atomic.Bool had a serialization race (callers returning before the
	// underlying release completed); the Mutex pattern documented in
	// powerassert/assertion.go is the canonical fix.
	mu sync.Mutex
}

// Name implements state.Releaser. Returns "eventtap" — used by main.go's
// LIFO Cleanup logger for "released releaser=eventtap" line which the
// acceptance test (Phase 1 TST-04) parses to verify push order.
func (r *Releaser) Name() string { return "eventtap" }

// Release implements state.Releaser. Two-layer idempotency.
//
// Wave 0: this is a stub returning nil — Wave 1 04-02 will implement the
// full cgo teardown sequence (D-08):
//
//  1. CGEventTapEnable(r.tap, false) — stop processing events first so the
//     callback cannot fire while we tear down the run loop source.
//  2. CFRunLoopStop(r.workerLoop) — wake the poller-thread run loop so it
//     returns from CFRunLoopRun(); the goroutine then exits cleanly.
//  3. CFRunLoopRemoveSource(workerLoop, source, kCFRunLoopCommonModes).
//  4. CFRelease(source) + CFRelease(tap).
//  5. watchdog_stop() — cancel the GCD timer.
//  6. wake_observer_remove() — unsubscribe NSWorkspace observers.
//
// All steps are guarded by the two-layer pattern so the underlying C
// calls execute exactly once even under racing Cleanup invocations.
func (r *Releaser) Release() error {
	// Fast path: hint flag. Cheap Load — once released is durably set
	// (after the winner stored it under mu), any repeat caller skips
	// the mutex entirely.
	if r.released.Load() {
		return nil
	}
	// Slow path: serialise concurrent first-time callers via the mutex.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check under the mutex — another goroutine may have won
	// between our Load and our Lock.
	if r.released.Load() {
		return nil
	}
	// Wave 0 stub: no cgo teardown yet. Wave 1 04-02 will perform the
	// 6-step sequence described above between this point and the Store.
	r.released.Store(true)
	return nil
}

// Install installs a CGEventTap at kCGHIDEventTap level configured to match
// the given hotkey.Spec. On a successful (modifiers, keyCode) match the C
// callback flips the package-level atomic.Bool matched; a polling goroutine
// (Wave 1 04-02) reads it and forwards a struct{} send to sink (capacity 1,
// idempotent select-default — D-02 / D-04). The watchdog (Wave 1 04-03) and
// wake observer (Wave 1 04-04) are both started inside Install so that the
// returned Releaser owns the complete cleanup chain.
//
// Logger fallback: nil → slog.Default() (mirrors powerassert.Acquire +
// state.NewRestoreState + cocoa.NewController convention).
//
// MUST be called from the main goroutine. Wave 1 04-02's implementation
// touches CFRunLoopGetMain() indirectly through the worker-thread setup
// and registers the wake observer on the main NSNotificationCenter, both
// main-thread-only paths.
//
// Wave 0 stub: returns nil, ErrTapInstallFailed unconditionally so any
// accidental wire-up in main.go surfaces immediately rather than silently
// installing a non-functional Releaser. Wave 1 04-02 replaces this body
// with the full install sequence and returns the populated *Releaser.
func Install(spec hotkey.Spec, sink chan<- struct{}, log *slog.Logger) (*Releaser, error) {
	_ = spec
	_ = sink
	if log == nil {
		log = slog.Default()
	}
	_ = log
	// Reference the cgo bindings so the C symbols stay live in the
	// final binary even when Wave 0 has not yet wired them through real
	// install logic. Without this reference, the linker may drop the
	// _cgo_export.h-emitted eventtap_matched stub during dead-code
	// elimination on some toolchain combinations, breaking Wave 1's
	// independent build of tap_darwin.m.
	if false {
		var tap C.CFMachPortRef
		var loop C.CFRunLoopRef
		C.eventtap_install_c(C.uint64_t(0), C.uint16_t(0), &tap)
		C.eventtap_register_worker_runloop(tap, &loop)
		C.eventtap_uninstall_c(tap)
		_ = C.eventtap_is_enabled(tap)
		C.eventtap_enable(tap, C.int(0))
		_ = C.eventtap_test_set_expected(C.uint64_t(0), C.uint16_t(0))
	}
	return nil, ErrTapInstallFailed
}

// Compile-time check: *Releaser satisfies the state.Releaser interface shape
// without importing the state package (would create an import cycle —
// cmd/dndmode/main.go is the only caller that holds *Releaser as
// state.Releaser). Mismatch surfaces here at build time. Mirrors
// powerassert/assertion.go:195-198 verbatim.
var _ interface {
	Release() error
	Name() string
} = (*Releaser)(nil)
