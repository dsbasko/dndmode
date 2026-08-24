//go:build darwin

package globalhotkey

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Carbon -framework Foundation

#include <stdint.h>

extern int globalhotkey_install(uint32_t mods, uint32_t keycode);
extern int globalhotkey_uninstall(void);
*/
import "C"

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
)

// eventHotKeyExistsErr is Carbon's "somebody already owns this combination"
// status, from <MacErrors.h>. It is the one OSStatus worth naming, because
// it is the only one a user can act on — pick a different combination.
const eventHotKeyExistsErr = -9878

// fired carries one value per press. Package-level rather than per-
// Registration because the Carbon handler is a C function with no way to
// carry a receiver; ErrAlreadyRegistered keeps that from being ambiguous by
// allowing only one live registration at a time.
//
// atomic.Pointer rather than a plain channel variable: goGlobalHotkeyFired
// can in principle run concurrently with Release swapping the channel out,
// and a torn read of a channel header would be a data race rather than a
// missed keypress.
var fired atomic.Pointer[chan struct{}]

// registerMu serializes Register against Release so the "exactly one live
// registration" invariant cannot be lost to a race between them.
var registerMu sync.Mutex

// goGlobalHotkeyFired is called from globalhotkey_handler on the main run
// loop. The send is non-blocking: with capacity 1, a press arriving while an
// earlier one is still unread is dropped, which is the correct semantics for
// "the user asked for a shield" — asking twice is asking once.
//
//export goGlobalHotkeyFired
func goGlobalHotkeyFired() {
	ch := fired.Load()
	if ch == nil {
		return // released between the press and this call
	}
	select {
	case *ch <- struct{}{}:
	default:
	}
}

// Registration is the live Carbon hotkey. It satisfies state.Releaser, so
// the caller drops it into the same LIFO cleanup stack as every other
// acquired resource.
type Registration struct {
	spec hotkey.Spec
	log  *slog.Logger

	// released mirrors powerassert.Assertion: an atomic fast path for
	// repeat callers plus a mutex so the second caller blocks until the
	// first has actually finished unregistering, rather than returning
	// early while the C side is still mid-teardown.
	released atomic.Bool
	mu       sync.Mutex
}

// Register installs the activation combination and returns the channel that
// reports presses.
//
// Registration needs no NSApp and no particular goroutine. DELIVERY does:
// the Carbon handler is dispatched by the main run loop, so nothing arrives
// on the returned channel until cocoa.RunApp is spinning. Watch mode
// registers between cocoa.Init and cocoa.RunApp for that reason.
//
// The returned error is one of ErrNoModifier, ErrComboTaken,
// ErrAlreadyRegistered, or a wrapped ErrRegisterFailed carrying the raw
// OSStatus. All of them are safe to print verbatim: the activation
// combination is public (see the package doc).
func Register(spec hotkey.Spec, log *slog.Logger) (<-chan struct{}, *Registration, error) {
	if log == nil {
		log = slog.Default()
	}

	// Convert BEFORE taking the lock or touching C: a modifier-less
	// combination must be refused without leaving any state behind.
	mods, err := carbonModifiers(spec.Modifiers)
	if err != nil {
		return nil, nil, err
	}

	registerMu.Lock()
	defer registerMu.Unlock()

	if fired.Load() != nil {
		return nil, nil, ErrAlreadyRegistered
	}

	ch := make(chan struct{}, 1)
	fired.Store(&ch)

	if st := int(C.globalhotkey_install(C.uint32_t(mods), C.uint32_t(spec.KeyCode))); st != 0 {
		fired.Store(nil)
		if st == eventHotKeyExistsErr {
			return nil, nil, ErrComboTaken
		}
		return nil, nil, fmt.Errorf("%w: OSStatus %d", ErrRegisterFailed, st)
	}

	// Deliberately does not spell out the combination: the caller already
	// holds the user's own config string for it and prints that, which round-
	// trips better than anything reconstructed from a bitmask here.
	log.Debug("global hotkey registered")

	return ch, &Registration{spec: spec, log: log}, nil
}

// Name implements state.Releaser.
func (r *Registration) Name() string { return "global-hotkey" }

// Release unregisters the hotkey. Idempotent: repeat calls return nil
// without touching Carbon again.
func (r *Registration) Release() error {
	if r.released.Load() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released.Load() {
		return nil
	}

	// Clear the channel pointer FIRST. A press landing between here and the
	// C-side unregister then finds nil and returns, instead of queueing an
	// activation nobody will ever read.
	fired.Store(nil)

	st := int(C.globalhotkey_uninstall())
	r.released.Store(true)
	if st != 0 {
		return fmt.Errorf("globalhotkey: uninstall failed: OSStatus %d", st)
	}
	return nil
}

// Compile-time check: *Registration satisfies state.Releaser without
// importing the state package (which would create an import cycle). Mirrors
// powerassert/assertion.go.
var _ interface {
	Release() error
	Name() string
} = (*Registration)(nil)
