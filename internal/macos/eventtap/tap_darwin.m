// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>
#import <IOKit/hidsystem/ev_keymap.h>  // NX_SYSDEFINED (== 14) media-key event type
#import <stdint.h>
#import "_cgo_export.h"  // generated header for //export eventtap_matched

// USER_INTENTIONAL_MASK is the bit-for-bit twin of `matcher.UserIntentionalMask`
// on the Go side. The callback strips system bits (CapsLock 0x10000,
// NumPad 0x200000, Help 0x400000, NX_NONCOALSESCEDMASK 0x100, …) before
// comparing against the configured Spec, because macOS sets those bits
// independently of user intent. Drift between this constant
// and `matcher.UserIntentionalMask` would silently break the hotkey for users
// with CapsLock or NumPad toggled — `TestUserIntentionalMask_MatchesMatcherPackage`
// in tap_test.go pins both to the same hex literal so any future divergence
// trips a unit-test rather than locking a customer out at runtime.
//
// Values are taken from CGEventTypes.h (HIGH confidence per the design notes):
//   kCGEventFlagMaskShift        = 0x00020000
//   kCGEventFlagMaskControl      = 0x00040000
//   kCGEventFlagMaskAlternate    = 0x00080000  (Option)
//   kCGEventFlagMaskCommand      = 0x00100000
//   kCGEventFlagMaskSecondaryFn  = 0x00800000
// OR-sum = 0x009E0000.
static const uint64_t USER_INTENTIONAL_MASK =
    (uint64_t)kCGEventFlagMaskShift     |
    (uint64_t)kCGEventFlagMaskControl   |
    (uint64_t)kCGEventFlagMaskAlternate |
    (uint64_t)kCGEventFlagMaskCommand   |
    (uint64_t)kCGEventFlagMaskSecondaryFn;

// expected_flags and expected_keycode are the (modifiers, keyCode) baseline
// the callback compares incoming CGEvents against. `eventtap_install_c` sets
// these from the Go-side hotkey.Spec via cgo before the tap is added to the
// worker run loop. `volatile` because the callback (worker thread) reads
// them while the main thread writes them; on macOS/ARM64 both fit in a
// single naturally-aligned load/store so no atomic primitive is required —
// the read pattern is "load once, compare", never "read-modify-write".
//
// Go side pre-masks `spec.Modifiers` with `matcher.UserIntentionalMask` BEFORE
// passing them in, so the callback compares against an already-masked value
// (the design notes): the incoming event's flags are masked at read time, the
// expected value is masked once at Install time.
static volatile uint64_t expected_flags  = 0;
static volatile uint16_t expected_keycode = 0;

// g_tap, g_source, g_worker_runloop hold the per-process tap state. There is
// exactly one active CGEventTap per dndmode process (single Install
// per Releaser; second concurrent install would conflict on the same
// HID-level slot).
//
// g_worker_runloop is captured by `eventtap_register_worker_runloop` (called
// from the worker goroutine AFTER its `runtime.LockOSThread()`). The tap
// source is added to THIS run loop, NOT `CFRunLoopGetMain()`, so that
// CGEvent dispatch happens off the main thread and AppKit on the main thread
// stays responsive (the design notes).
static CFMachPortRef      g_tap            = NULL;
static CFRunLoopSourceRef g_source         = NULL;
static CFRunLoopRef       g_worker_runloop = NULL;

// eventtap_callback is the CGEventTap callback. It fires on the worker
// thread that runs `g_worker_runloop` (NOT main). Contract per
// (the design notes):
//
//   1. If `type == kCGEventTapDisabledByTimeout` OR
//      `kCGEventTapDisabledByUserInput` → inline `CGEventTapEnable(g_tap, true)`
// and propagate the event as-is (the design notes "event field is undocumented
// for these types; return as-is"; per UserInput is normal — the
//      callback always heals, the watchdog only counts silent disables).
//   2. If `type == kCGEventFlagsChanged` → return NULL (suppress without
// match-testing). Flag-only events have no keyCode.
//   3. If `type == kCGEventKeyDown` → mask incoming flags via
//      USER_INTENTIONAL_MASK; read keyCode; if both equal the Install-time
//      globals → call `eventtap_matched()` (//export Go helper that flips
//      `matched` atomic.Bool — see tap_darwin.go).
//   4. Unconditional `return NULL` at the end — all keyboard / mouse / scroll
// events are swallowed ("all input blocked except the configured
//      hotkey"); matched events are ALSO swallowed so the trigger combo does
//      not leak into the underlying app (e.g. a stray Cmd+X reaching a text
//      editor).
//
// nosplit invariant: this body MUST NOT acquire Go locks, allocate Go
// memory, log, or call dispatch_async. The only Go-side call is the no-arg,
// no-return `eventtap_matched()` which does a single atomic store. Pre-fix
// experimentation in the design notes confirmed this is the only callback shape
// that survives `-race` under load.
//
// silent fail: no NSLog / printf / fprintf — wrong-key presses leave
// no observable side channel. `--debug` mode is deferred per the design notes.
static CGEventRef eventtap_callback(CGEventTapProxy proxy,
                                    CGEventType type,
                                    CGEventRef event,
                                    void *userInfo) {
    (void)proxy;
    (void)userInfo;

    // disable recovery. Inline re-enable from the callback's own
    // thread; propagate the event as-is per the design notes (the `event` field is
    // undocumented for these types but pqrs-org/Karabiner production code
    // returns it without issue — A7 assumption verified there).
    if (type == kCGEventTapDisabledByTimeout ||
        type == kCGEventTapDisabledByUserInput) {
        if (g_tap != NULL) {
            CGEventTapEnable(g_tap, true);
        }
        return event;
    }

    // FlagsChanged events carry only modifier-state transitions — no keyCode.
    // Suppress them without match-testing. Returning the event
    // would let modifier state leak to the app even when the rest of the tap
    // is blocking input.
    if (type == kCGEventFlagsChanged) {
        return NULL;
    }

    // match path: only KeyDown can match the configured hotkey.
    // KeyUp / mouse / scroll / NX_SYSDEFINED all fall through to the final
    // unconditional `return NULL` below.
    if (type == kCGEventKeyDown) {
        uint64_t flags = (uint64_t)CGEventGetFlags(event) & USER_INTENTIONAL_MASK;
        int64_t  keycode = CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);
        if (flags == expected_flags && (uint16_t)keycode == expected_keycode) {
            // //export Go helper — body is exactly `matched.Store(true)`,
            // enforced by gold-grep in tap_test.go (Threat).
            eventtap_matched();
        }
    }

    //: ALWAYS return NULL after the match-test branch. Every
    // keyboard / mouse / scroll / media event is suppressed; the matched
    // event is swallowed too so the hotkey combo does not surface in any app.
    return NULL;
}

// eventtap_install_c installs the CGEventTap at kCGHIDEventTap with
// kCGHeadInsertEventTap placement and the suppression-capable
// kCGEventTapOptionDefault (all three constants are grep-pinned by
// the acceptance criteria).
//
// Returns 0 on success; non-zero on failure (Go side wraps in
// ErrTapInstallFailed):
//
//   1 — CGEventTapCreate returned NULL. Triggers per errors.go comment:
//       Accessibility revoked, SecureEventInput active, or kernel out of mach
//       ports (Daniel Raffel TIL 2026-02-19).
//   2 — CFMachPortCreateRunLoopSource returned NULL. Extremely rare —
//       indicates CoreFoundation allocator exhaustion. We tear down the tap
//       and reset `g_tap` to NULL so a retry starts from a clean slate.
//
// The function:
//   1. Records (flags, keycode) into the static globals BEFORE creating the
//      tap — once the source is added to the run loop, the callback may fire
//      on the very first event and MUST read coherent values.
//   2. Builds the 15-bit event mask via CGEventMaskBit() over the table in
// the design notes (KeyDown/Up, FlagsChanged, all 9 mouse events, MouseMoved,
//      ScrollWheel, NX_SYSDEFINED for media keys).
//   3. Creates the tap with suppression-capable Default option (NOT
//      ListenOnly — that downgrades to Input Monitoring permission and we
//      need Accessibility to block events).
//   4. Creates the run-loop source (NOT added to the loop here — the worker
//      goroutine does that via `eventtap_register_worker_runloop` after it
//      calls `runtime.LockOSThread()` and obtains its own CFRunLoop).
//   5. Enables the tap — CGEventTapCreate returns a disabled tap; we MUST
//      explicitly enable before events flow.
int eventtap_install_c(uint64_t flags, uint16_t keycode, CFMachPortRef *out_tap) {
    expected_flags  = flags;
    expected_keycode = keycode;

    // 15 event types per the design notes block every keyboard, mouse,
    // scroll, and system-defined (media) event the tap can see.
    CGEventMask mask =
        CGEventMaskBit(kCGEventKeyDown)          |
        CGEventMaskBit(kCGEventKeyUp)            |
        CGEventMaskBit(kCGEventFlagsChanged)     |
        CGEventMaskBit(kCGEventLeftMouseDown)    |
        CGEventMaskBit(kCGEventLeftMouseUp)      |
        CGEventMaskBit(kCGEventLeftMouseDragged) |
        CGEventMaskBit(kCGEventRightMouseDown)   |
        CGEventMaskBit(kCGEventRightMouseUp)     |
        CGEventMaskBit(kCGEventRightMouseDragged)|
        CGEventMaskBit(kCGEventOtherMouseDown)   |
        CGEventMaskBit(kCGEventOtherMouseUp)     |
        CGEventMaskBit(kCGEventOtherMouseDragged)|
        CGEventMaskBit(kCGEventMouseMoved)       |
        CGEventMaskBit(kCGEventScrollWheel)      |
        CGEventMaskBit(NX_SYSDEFINED);            // 14 — media/function keys

    g_tap = CGEventTapCreate(
        kCGHIDEventTap,                  // before WindowServer Bluetooth-safe
        kCGHeadInsertEventTap,           // front of chain — first to see events
        kCGEventTapOptionDefault,        // suppression-capable (NOT ListenOnly)
        mask,                            // 
        eventtap_callback,
        NULL);                           // refcon — unused (uses statics)
    if (g_tap == NULL) {
        return 1;
    }

    g_source = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, g_tap, 0);
    if (g_source == NULL) {
        CFRelease(g_tap);
        g_tap = NULL;
        return 2;
    }

    // CGEventTapCreate returns the tap in a DISABLED state. Without this
    // call, the source can be added to a run loop but no callback ever
    // fires — and the silent failure looks identical to the post-install
    // success path until the first key press never matches.
    CGEventTapEnable(g_tap, true);

    *out_tap = g_tap;
    return 0;
}

// eventtap_register_worker_runloop is called from the worker goroutine AFTER
// it calls `runtime.LockOSThread()`. It captures `CFRunLoopGetCurrent()`
// (which is the fresh run loop of the locked OS thread — empty by default)
// and adds the tap source to it with kCFRunLoopCommonModes (the design notes
//).
//
// The captured `g_worker_runloop` is the handle that `eventtap_uninstall_c`
// uses to stop the loop on Release — `CFRunLoopStop` is documented
// thread-safe so it can be called from the main goroutine teardown path even
// though the loop runs on the worker thread.
//
// `out_loop` mirrors `g_worker_runloop` back to Go so the Releaser struct
// can hold a Go-side opaque pointer alongside the C-side static, which lets
// the unit tests assert that the field is non-nil after Install.
//
// Returns 0 unconditionally — `CFRunLoopGetCurrent` cannot fail and
// `CFRunLoopAddSource` has no error return. The integer return type is
// preserved for symmetry with `eventtap_install_c` and future-proofing.
int eventtap_register_worker_runloop(CFMachPortRef tap, CFRunLoopRef *out_loop) {
    (void)tap; // tap is captured via the static `g_tap` set in install_c.

    g_worker_runloop = CFRunLoopGetCurrent();
    if (g_source != NULL) {
        CFRunLoopAddSource(g_worker_runloop, g_source, kCFRunLoopCommonModes);
    }
    if (out_loop != NULL) {
        *out_loop = g_worker_runloop;
    }
    return 0;
}

// eventtap_uninstall_c is the symmetric teardown of `eventtap_install_c` +
// `eventtap_register_worker_runloop`. Order matches the design notes:
//
//   1. Tap is already disabled by the Go-side `Releaser.Release()` BEFORE
//      this call (disable-first ordering — keyboard recovers immediately
//      even if any of the steps below fail).
//   2. Remove the run-loop source from the worker loop — once removed, the
//      tap source mach-port stops being polled by the loop.
//   3. CFRelease the source (we own one reference; the run loop's reference
//      was dropped by RemoveSource above).
//   4. Disable + CFRelease the tap mach-port. CGEventTapEnable(tap, false)
//      is idempotent so the Go-side disable + this disable double-call is
//      safe; we re-call here for defensive symmetry.
//   5. Stop the worker run loop — Run() returns, the worker goroutine exits,
//      Go runtime reaps the locked OS thread.
//
// All globals are nulled out so a hypothetical re-Install starts from a
// clean slate.
//
// Idempotency: nil-checks on every CF release path mean it is safe to call
// this with a NULL tap or after a prior call — the Go-side two-layer guard
// already serialises Release callers, but the C side is defensive in case
// of future test fixtures that exercise this directly.
void eventtap_uninstall_c(CFMachPortRef tap) {
    if (g_worker_runloop != NULL && g_source != NULL) {
        CFRunLoopRemoveSource(g_worker_runloop, g_source, kCFRunLoopCommonModes);
    }
    if (g_source != NULL) {
        CFRelease(g_source);
        g_source = NULL;
    }
    if (tap != NULL) {
        CGEventTapEnable(tap, false);
        CFRelease(tap);
    }
    g_tap = NULL;
    if (g_worker_runloop != NULL) {
        // CFRunLoopStop is documented thread-safe — safe to invoke from the
        // main goroutine while the worker goroutine is blocked in
        // CFRunLoopRun. The worker loop wakes, Run() returns, the goroutine
        // exits, and Go reaps its locked OS thread.
        CFRunLoopStop(g_worker_runloop);
        g_worker_runloop = NULL;
    }
}

// eventtap_is_enabled wraps `CGEventTapIsEnabled` for the watchdog probe
//. Returns 1 if enabled, 0 if disabled. nil-tap returns 0.
//
// This is the only health probe Daniel Raffel's TIL identified as reliable
// against the silent-disable race (CGEventTapCreate returns non-NULL on a
// dead tap when the ad-hoc identity has been re-signed without a TCC
// refresh; only this call detects the dead state).
int eventtap_is_enabled(CFMachPortRef tap) {
    if (tap == NULL) {
        return 0;
    }
    return CGEventTapIsEnabled(tap) ? 1 : 0;
}

// eventtap_enable wraps `CGEventTapEnable` for the disable-first Release
// path and the watchdog re-enable path. The underlying
// Quartz call is documented idempotent so the Go-side concurrent-cleanup
// guard does not need to debounce repeated invocations.
void eventtap_enable(CFMachPortRef tap, int enable) {
    if (tap == NULL) {
        return;
    }
    CGEventTapEnable(tap, enable != 0);
}

// removed the eventtap_test_set_expected test-only setter. It had
// no caller (no Go-side `C.eventtap_test_set_expected` invocation in
// tap_test.go or anywhere else), so it was dead in both the production
// AND test binary. Keeping it always-compiled added a small but real
// attack surface: a process-injected adversary could rewrite
// (expected_flags, expected_keycode) at runtime without going through
// Accessibility / Install / a configured Spec, locking out the
// legitimate user with a custom hotkey. The "1KB binary savings" comment
// optimized the wrong variable (correctness/security > size). If a
// future test wants the setter back, prefer either (a) a build-tag
// gated `*_darwin_test.m` companion file that ships ONLY in test
// binaries, or (b) a `#ifdef DNDMODE_TEST_HELPERS` guard wired to a
// dedicated `#cgo test_helpers CFLAGS: -DDNDMODE_TEST_HELPERS`. Both
// keep the helper out of the production binary.
