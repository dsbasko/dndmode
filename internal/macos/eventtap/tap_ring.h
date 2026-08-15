//go:build darwin

// tap_ring.h declares the keystroke ring that carries key presses from the
// CGEventTap callback (tap_darwin.m — the single writer, running on the tap
// worker thread) to the poller goroutine (poller.go via the cgo bridge in
// tap_darwin.go — the single reader).
//
// The declarations live in a shared header instead of being duplicated in
// the .m file and the cgo preamble of tap_darwin.go for one reason: the Go
// side needs `C.dnd_keyrec_t` to exist, and the capacity must be ONE value,
// not two that can silently drift. With this header there are exactly two
// copies of the capacity in the tree — DND_RING_CAP here and `ringCap` in
// poller.go — and `ring_guard_test.go` pins them to each other, so a change
// to either without the other fails the unit tests instead of corrupting
// the unlock path at runtime.
//
// Sizing: DND_RING_CAP is a power of two so the ring index is `seq & (CAP-1)`
// (a mask, not a division) inside the callback, which must stay allocation-
// and branch-cheap. 64 is 2x `hotkey.MaxSteps` (32) — the longest unlock
// code fits in the ring twice over, which is the headroom the matcher's
// correctness argument in the plan relies on. `ring_guard_test.go` pins that
// relation too.

#ifndef DNDMODE_TAP_RING_H
#define DNDMODE_TAP_RING_H

#include <stdint.h>

// DND_RING_CAP is the number of key-press records the ring holds. MUST be a
// power of two (indexing is done with `& (DND_RING_CAP - 1)`) and MUST equal
// `ringCap` in poller.go.
#define DND_RING_CAP 64

// dnd_keyrec_t is one recorded key press — the C twin of
// `matcher.KeyEvent`. `flags` is CGEventFlags already masked with
// USER_INTENTIONAL_MASK by the callback; `keycode` is the macOS virtual
// keyCode (kVK_*).
//
// Field order and types are chosen so the struct is naturally aligned on
// arm64 (uint64 first, then uint16 + 6 bytes of tail padding = 16 bytes).
// Natural alignment is what makes the deliberately-unsynchronised memcpy in
// `eventtap_snapshot` benign in practice — see the invariant comment there.
typedef struct {
    uint64_t flags;
    uint16_t keycode;
} dnd_keyrec_t;

#endif  // DNDMODE_TAP_RING_H
