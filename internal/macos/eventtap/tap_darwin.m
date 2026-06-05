// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>
#import <stdint.h>
#import "_cgo_export.h"  // generated header for //export eventtap_matched (Wave 1 04-02)

// expected_flags and expected_keycode are the (modifiers, keyCode) baseline
// the callback compares incoming CGEvents against. Wave 1 04-02
// `eventtap_install_c` will set these from the Go-side hotkey.Spec via cgo
// before installing the tap. `volatile` because the callback (worker thread)
// reads them while the main thread writes them; on macOS/ARM64 both fit in a
// single naturally-aligned load/store so no atomic primitive is required —
// the read pattern is "load once, compare", never "read-modify-write".
//
// They are zero-initialised here and remain zero until install runs; the
// stub callback ignores them anyway and always returns NULL.
static volatile uint64_t expected_flags  = 0;
static volatile uint16_t expected_keycode = 0;

// g_tap, g_source, g_worker_runloop hold the per-process tap state. There is
// exactly one active CGEventTap per dndmode process (single Install
// per Releaser; second concurrent install would conflict on the same
// HID-level slot). Wave 1 04-02 populates these; Release nils them out.
//
// g_worker_runloop is captured under the dedicated CFRunLoop that the poller
// goroutine spins (D-02 `runtime.LockOSThread()` + `CFRunLoopRun()` in the
// worker thread); the tap source is added to THIS run loop, NOT
// `CFRunLoopGetMain()`, so that CGEvent dispatch happens off the main
// thread and AppKit on the main thread stays responsive (RESEARCH §5).
static CFMachPortRef     g_tap            = NULL;
static CFRunLoopSourceRef g_source        = NULL;
static CFRunLoopRef      g_worker_runloop = NULL;

// eventtap_callback is the CGEventTap callback. It fires on the worker
// thread that runs `g_worker_runloop` (NOT main). Wave 0 is a strict
// placeholder: it always returns NULL, suppressing every event the tap
// would otherwise see. **This is the desired Phase 4 INP-04 contract** —
// the tap is the input firewall, NOT a passthrough. Wave 1 04-02 expands
// the body to:
//
//   1. If `type == kCGEventTapDisabledByTimeout` or
//      `kCGEventTapDisabledByUserInput` → call `CGEventTapEnable(g_tap, true)`
//      and return NULL (self-heal silent-disable; D-11 — UserInput is normal,
//      D-09 — timeout counts towards watchdog).
//   2. Else read `CGEventGetFlags(event)` + `CGEventGetIntegerValueField(
//      event, kCGKeyboardEventKeycode)`, mask out non-user-intentional bits
//      (UserIntentionalMask from internal/matcher), and compare against
//      `expected_flags` + `expected_keycode`.
//   3. On match → call `eventtap_matched()` (//export Go helper that writes
//      to `matched` atomic.Bool) and return NULL. The poller goroutine
//      reads the atomic and forwards to `Supervisor.ExitTrigger()`.
//   4. On no match → return NULL (event suppressed regardless — INP-04
//      "all input blocked except the configured hotkey").
//
// INP-05 nosplit invariant: the body MUST NOT acquire Go locks, allocate
// Go memory, log, or call `dispatch_async`. The only Go-side call is the
// no-arg, no-return `eventtap_matched()` which does a single atomic store.
// Pre-fix experimentation in RESEARCH §4 confirmed this is the only
// callback shape that survives `-race` under load.
static CGEventRef eventtap_callback(CGEventTapProxy proxy,
                                    CGEventType type,
                                    CGEventRef event,
                                    void *userInfo) {
    // Wave 0 stub — Wave 1 04-02 will materialise the comparison + match
    // signalling described above. (void)-cast the unused parameters to
    // suppress -Wunused-parameter under -Wall.
    (void)proxy;
    (void)type;
    (void)event;
    (void)userInfo;
    return NULL;
}

// eventtap_install_c installs the CGEventTap and returns 0 on success, a
// non-zero error code on failure. Wave 1 04-02 implements:
//
//   1. Write `flags` + `keycode` to the static globals.
//   2. `CGEventTapCreate(kCGHIDEventTap, kCGHeadInsertEventTap,
//       kCGEventTapOptionDefault, eventMask, eventtap_callback, NULL)`.
//      `eventMask` covers KeyDown | KeyUp | FlagsChanged + all mouse events
//      + system-defined (media keys) per CONTEXT D-01 + RESEARCH §4.
//   3. On NULL return → return 1 (Go side wraps `ErrTapInstallFailed`).
//   4. Create a run loop source, add it to `g_worker_runloop`
//      (NOT `CFRunLoopGetMain()` — see callback comment above).
//   5. Enable the tap via `CGEventTapEnable(g_tap, true)`.
//   6. Write the tap handle to `*out_tap` (so Go side can pass it to
//      watchdog_start + wake_observer_install).
//
// Wave 0 returns 1 ("not implemented") unconditionally so any accidental
// production call surfaces immediately rather than silently no-op'ing.
int eventtap_install_c(uint64_t flags, uint16_t keycode, CFMachPortRef *out_tap) {
    (void)flags;
    (void)keycode;
    (void)out_tap;
    return 1;
}

// eventtap_uninstall_c is the symmetric teardown. Wave 1 04-02 implements:
//
//   1. `CGEventTapEnable(tap, false)` — stop processing events first so the
//      callback cannot fire while we are tearing down its run loop source.
//   2. `CFRunLoopRemoveSource(g_worker_runloop, g_source, kCFRunLoopCommonModes)`.
//   3. `CFRelease(g_source)` + `CFRelease(g_tap)` — symmetric to the Create
//      calls. ARC does not manage CF objects.
//   4. Zero out the globals so a subsequent (theoretical) re-Install starts
//      from a clean slate.
//
// Wave 0 is a no-op.
void eventtap_uninstall_c(CFMachPortRef tap) {
    (void)tap;
}

// eventtap_is_enabled is the watchdog probe target. Wave 1 04-03 wires this
// to the GCD timer callback (5s cadence, D-12). Returns 1 if enabled, 0 if
// disabled (silent-disable race, Daniel Raffel TIL 2026-02-19).
//
// Wave 0 returns 0 so the watchdog's accept criterion never spuriously
// fires before Wave 1 implements the real bridge.
int eventtap_is_enabled(CFMachPortRef tap) {
    (void)tap;
    return 0;
}

// eventtap_enable wraps `CGEventTapEnable(tap, enable ? true : false)` for
// the watchdog re-enable path and the wake-handler re-arm path. Wave 1 04-03
// + 04-04 will both call this; the underlying Quartz call is idempotent so
// concurrent re-enables are safe.
//
// Wave 0 is a no-op.
void eventtap_enable(CFMachPortRef tap, int enable) {
    (void)tap;
    (void)enable;
}
