//go:build darwin
// +build darwin

#import <Carbon/Carbon.h>
#import <stdint.h>

// _cgo_export.h gives this file goGlobalHotkeyFired — the single Go call the
// handler makes. Importing it here is exactly what internal/macos/eventtap
// forbids in its own .m, and the difference is deliberate rather than an
// oversight: that file's callback runs on a foreign worker thread in the hot
// path of every keystroke in the system, so it must make zero Go calls. This
// handler runs on the thread Go started, at human chord-pressing rate. See
// the package doc.
#import "_cgo_export.h"

// One registration per process — the Go side enforces this with
// ErrAlreadyRegistered before ever reaching here. Both handles are kept so
// uninstall can undo exactly what install did, in reverse.
static EventHotKeyRef  g_hotkey  = NULL;
static EventHandlerRef g_handler = NULL;

// globalhotkey_handler is invoked by the main run loop when the registered
// combination is pressed. It does nothing but tell Go, because everything
// that decides anything lives on the Go side.
//
// Returning noErr (rather than eventNotHandledErr) claims the event, so the
// combination does not additionally reach whatever application is frontmost.
static OSStatus globalhotkey_handler(EventHandlerCallRef callRef,
                                     EventRef event,
                                     void *userData) {
    goGlobalHotkeyFired();
    return noErr;
}

// globalhotkey_install registers `mods` + `keycode` system-wide and returns
// the OSStatus verbatim: 0 (noErr) on success, eventHotKeyExistsErr (-9878)
// when the combination is already taken, anything else on a real failure.
// Interpreting the status is the Go side's job.
//
// `mods` is Carbon's modifier encoding (cmdKey/shiftKey/optionKey/controlKey),
// NOT CGEventFlags — carbonModifiers in carbon.go does that conversion and is
// the only supported way to produce this argument.
//
// The handler is installed BEFORE the hotkey so that no press can arrive
// with nowhere to go; on a failed registration the handler is removed again,
// leaving the process exactly as it was found.
int globalhotkey_install(uint32_t mods, uint32_t keycode) {
    if (g_hotkey != NULL || g_handler != NULL) {
        return paramErr; // Go guards this; belt and braces.
    }

    EventTypeSpec spec;
    spec.eventClass = kEventClassKeyboard;
    spec.eventKind  = kEventHotKeyPressed;

    OSStatus st = InstallEventHandler(GetApplicationEventTarget(),
                                      NewEventHandlerUPP(globalhotkey_handler),
                                      1, &spec, NULL, &g_handler);
    if (st != noErr) {
        g_handler = NULL;
        return (int)st;
    }

    EventHotKeyID hkid;
    hkid.signature = 'dndm';
    hkid.id        = 1;

    st = RegisterEventHotKey((UInt32)keycode, (UInt32)mods, hkid,
                             GetApplicationEventTarget(), 0, &g_hotkey);
    if (st != noErr) {
        RemoveEventHandler(g_handler);
        g_handler = NULL;
        g_hotkey  = NULL;
        return (int)st;
    }
    return (int)noErr;
}

// globalhotkey_uninstall reverses install in LIFO order: the hotkey stops
// being matched first, then the handler that would have received it goes
// away. Doing it the other way round would leave a live registration whose
// handler had already been removed.
//
// Idempotent — a second call finds both handles NULL and reports success.
// The Go-side Registration.Release relies on that.
int globalhotkey_uninstall(void) {
    OSStatus first = noErr;

    if (g_hotkey != NULL) {
        OSStatus st = UnregisterEventHotKey(g_hotkey);
        if (st != noErr && first == noErr) {
            first = st;
        }
        g_hotkey = NULL;
    }
    if (g_handler != NULL) {
        OSStatus st = RemoveEventHandler(g_handler);
        if (st != noErr && first == noErr) {
            first = st;
        }
        g_handler = NULL;
    }
    return (int)first;
}
