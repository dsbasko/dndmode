//go:build darwin

package eventtap

import (
	"sync/atomic"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// releaserTestDeps groups dependencies for Releaser unit tests per
// Go-test conventions. The DI seam (newReleaserWithDeps in tap_darwin.go)
// lets us inject fake disable/uninstall closures that record their
// invocation order — so we can verify disable-first ordering and
// two-layer idempotency without invoking the real cgo bridge.
type releaserTestDeps struct {
	disableCalls          atomic.Int64
	uninstallCalls        atomic.Int64
	gestureDisableCalls   atomic.Int64
	gestureUninstallCalls atomic.Int64
	wipeRingCalls         atomic.Int64

	// callOrder records the sequence of "disable" / "gesture-disable" /
	// "wipe-ring" / "gesture-uninstall" / "uninstall" strings, in the order the fake
	// closures were invoked. Slice append is NOT goroutine-safe in general,
	// but in the Release path the closures are invoked from the same
	// goroutine (mutex-serialised), so a plain slice is sufficient. The
	// race tests use atomic counters above.
	callOrder []string

	releaser *Releaser
}

// newReleaserTestDeps constructs a Releaser via the DI seam with fake
// closures that record their invocation count + order into the testDeps
// struct. stopPoller/pollerDone channels are pre-populated and pollerDone
// is pre-closed so Release does not block on the poller-wait step (the
// poller itself is exercised in poller_test.go, against a fake ring).
func newReleaserTestDeps(t *testing.T) *releaserTestDeps {
	t.Helper()
	d := &releaserTestDeps{}
	stopPoller := make(chan struct{})
	pollerDone := make(chan struct{})
	close(pollerDone) // simulate a poller that has already exited
	disableFn := func() {
		d.disableCalls.Add(1)
		d.callOrder = append(d.callOrder, "disable")
	}
	uninstallFn := func() {
		d.uninstallCalls.Add(1)
		d.callOrder = append(d.callOrder, "uninstall")
	}
	d.releaser = newReleaserWithDeps(disableFn, uninstallFn, stopPoller, pollerDone, nil)
	// Gesture-tap closures are wired via direct field assignment (same
	// package) rather than widening newReleaserWithDeps: the production
	// installInternal path sets these fields the same way, and existing
	// callers of the seam stay source-compatible.
	d.releaser.gestureDisableFn = func() {
		d.gestureDisableCalls.Add(1)
		d.callOrder = append(d.callOrder, "gesture-disable")
	}
	d.releaser.gestureUninstallFn = func() {
		d.gestureUninstallCalls.Add(1)
		d.callOrder = append(d.callOrder, "gesture-uninstall")
	}
	d.releaser.wipeRingFn = func() {
		d.wipeRingCalls.Add(1)
		d.callOrder = append(d.callOrder, "wipe-ring")
	}
	return d
}

// TestReleaser_Release_IsIdempotent verifies the two-layer guard contract
// (pattern mirrored from powerassert.Assertion): the first Release()
// invokes disableFn + uninstallFn exactly once; subsequent Release() calls
// return nil without invoking either closure again.
//
// This pins the production-critical invariant from: even if the
// supervisor cleanup chain and ctx-watcher goroutine both invoke
// Release nearly simultaneously, the underlying CGEventTapEnable +
// CFRelease primitives fire exactly once.
func TestReleaser_Release_IsIdempotent(t *testing.T) {
	t.Parallel()

	d := newReleaserTestDeps(t)

	if err := d.releaser.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if got := d.disableCalls.Load(); got != 1 {
		t.Errorf("after 1st Release, disableCalls = %d, want 1", got)
	}
	if got := d.uninstallCalls.Load(); got != 1 {
		t.Errorf("after 1st Release, uninstallCalls = %d, want 1", got)
	}

	// Second + third invocations: must return nil and MUST NOT call the
	// closures again (released.Load() fast-path engaged).
	if err := d.releaser.Release(); err != nil {
		t.Errorf("second Release: %v (must be nil — idempotent)", err)
	}
	if err := d.releaser.Release(); err != nil {
		t.Errorf("third Release: %v (must be nil — idempotent)", err)
	}
	if got := d.disableCalls.Load(); got != 1 {
		t.Errorf("after 3 Release calls, disableCalls = %d, want 1 (gate must block)", got)
	}
	if got := d.uninstallCalls.Load(); got != 1 {
		t.Errorf("after 3 Release calls, uninstallCalls = %d, want 1 (gate must block)", got)
	}
	if got := d.gestureDisableCalls.Load(); got != 1 {
		t.Errorf("after 3 Release calls, gestureDisableCalls = %d, want 1 (gate must block)", got)
	}
	if got := d.gestureUninstallCalls.Load(); got != 1 {
		t.Errorf("after 3 Release calls, gestureUninstallCalls = %d, want 1 (gate must block)", got)
	}
	if got := d.wipeRingCalls.Load(); got != 1 {
		t.Errorf("after 3 Release calls, wipeRingCalls = %d, want 1 (gate must block)", got)
	}
}

// TestReleaser_Release_DisableBeforeUninstall verifies two ordering
// invariants at once:
//
//  1. disable-first — BOTH taps are disabled (Step 1) before any CFRelease
//     teardown, so keyboard AND trackpad gestures recover immediately even
//     if subsequent CF teardown fails;
//  2. gesture-uninstall BEFORE main uninstall (Step 2a before Steps 2+3) —
//     the main uninstall ends with CFRunLoopStop on the SHARED worker run
//     loop; the gesture source must leave that loop before it can be torn
//     down (see gesturetap_uninstall_c ordering contract);
//  3. wipe-ring AFTER both disables and BEFORE any uninstall — the
//     keystroke ring holds the tail of what the user typed, ending with the
//     unlock code itself. It must be cleared as soon as no writer is left
//     (both taps disabled) rather than waiting for the CF teardown, which
//     can be slow or fail outright. Wiping any earlier — while a tap is
//     still live — would make the wiper a second writer to the ring and
//     break the single-writer premise eventtap_snapshot relies on.
//
// The fake closures append their tags to callOrder; the test asserts the
// exact five-step sequence.
func TestReleaser_Release_DisableBeforeUninstall(t *testing.T) {
	t.Parallel()

	d := newReleaserTestDeps(t)
	if err := d.releaser.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	want := []string{"disable", "gesture-disable", "wipe-ring", "gesture-uninstall", "uninstall"}
	if len(d.callOrder) != len(want) {
		t.Fatalf("callOrder = %v, want %v (len mismatch)", d.callOrder, want)
	}
	for i := range want {
		if d.callOrder[i] != want[i] {
			t.Errorf("callOrder[%d] = %q, want %q (disable-first, wipe-after-disable, gesture-before-main)", i, d.callOrder[i], want[i])
		}
	}
}

// TestReleaser_Name_ReturnsEventtap verifies the LIFO log
// contract: Name() returns "eventtap" so main.go's stderr line
// "released releaser=eventtap" pins the push order.
// Replaces the Phase 3 "mock-tap" placeholder.
func TestReleaser_Name_ReturnsEventtap(t *testing.T) {
	t.Parallel()

	d := newReleaserTestDeps(t)
	if got := d.releaser.Name(); got != "eventtap" {
		t.Errorf("Name() = %q, want %q", got, "eventtap")
	}
}

// TestUserIntentionalMask_MatchesMatcherPackage pins the bit-for-bit
// equality between the Go-side `matcher.UserIntentionalMask` and the
// C-side `USER_INTENTIONAL_MASK` constant in tap_darwin.m. Both must
// produce 0x009E0000 (Shift|Control|Alternate|Command|SecondaryFn):
//
//	Shift        = 0x00020000
//	Control      = 0x00040000
//	Alternate    = 0x00080000
//	Command      = 0x00100000
//	SecondaryFn  = 0x00800000
//	OR-sum       = 0x009E0000
//
// Drift (e.g. a reviewer adding NumPad 0x200000 to the Go side without
// updating the .m file) would silently produce a hotkey that never
// matches on systems where the unmasked bit is set. This test pins the
// constant on the Go side; the C side is enforced by code-review of
// tap_darwin.m (which also lists the same 5 bits explicitly).
func TestUserIntentionalMask_MatchesMatcherPackage(t *testing.T) {
	t.Parallel()

	const want hotkey.ModFlag = 0x00020000 | // Shift
		0x00040000 | // Control
		0x00080000 | // Alternate (Option)
		0x00100000 | // Command
		0x00800000 //   SecondaryFn (Fn)

	if got := matcher.UserIntentionalMask; got != want {
		t.Errorf("matcher.UserIntentionalMask = 0x%08x, want 0x%08x (Shift|Control|Alternate|Command|SecondaryFn)",
			uint64(got), uint64(want))
	}

	// Sanity: every individual hotkey.ModFlag constant must be a single
	// bit within the mask. Drift on hotkey/hotkey.go (e.g. ModCmd changing
	// value) would trip this AND the matcher mask AND tap_darwin.m's
	// USER_INTENTIONAL_MASK simultaneously — a 3-way drift detector.
	individual := []struct {
		name string
		flag hotkey.ModFlag
	}{
		{"ModShift", hotkey.ModShift},
		{"ModCtrl", hotkey.ModCtrl},
		{"ModOption", hotkey.ModOption},
		{"ModCmd", hotkey.ModCmd},
		{"ModFn", hotkey.ModFn},
	}
	for _, ind := range individual {
		if ind.flag&want != ind.flag {
			t.Errorf("%s = 0x%x is NOT within UserIntentionalMask 0x%x", ind.name, uint64(ind.flag), uint64(want))
		}
	}
}

// TestKeyEventFromRecord_MapsFieldsVerbatim pins the field mapping between a
// C ring record and its Go mirror. This is the half of the snapshot
// conversion that can be tested without cgo (test files cannot `import "C"`
// at all, so a C.dnd_keyrec_t can never be constructed here) — and it is the
// half where the interesting mistakes live: swapping the two fields, or
// narrowing a value on the way across.
//
// A regression here is silent and total: every recorded keystroke would
// carry a wrong keycode or wrong modifiers, so the unlock code would simply
// never match and the machine would stay locked.
func TestKeyEventFromRecord_MapsFieldsVerbatim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flags   uint64
		keycode uint16
		want    matcher.KeyEvent
	}{
		{
			name:    "zero record",
			flags:   0,
			keycode: 0,
			want:    matcher.KeyEvent{Modifiers: 0, KeyCode: 0},
		},
		{
			name:    "bare key",
			flags:   0,
			keycode: 1, // kVK_ANSI_S
			want:    matcher.KeyEvent{Modifiers: 0, KeyCode: 1},
		},
		{
			name:    "modified key",
			flags:   uint64(hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd),
			keycode: 7, // kVK_ANSI_X
			want: matcher.KeyEvent{
				Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd,
				KeyCode:   7,
			},
		},
		{
			// The callback masks before recording, but the conversion must
			// not silently launder a value that arrives unmasked — that
			// would hide a C-side regression from every Go test.
			// MatchTail is the one place masking happens on this side.
			name:    "unmasked system bits survive the conversion",
			flags:   uint64(hotkey.ModShift) | 0x10000, // + CapsLock
			keycode: 49,                                // kVK_Space
			want: matcher.KeyEvent{
				Modifiers: hotkey.ModShift | 0x10000,
				KeyCode:   49,
			},
		},
		{
			// Widest values either field can carry. A conversion that
			// truncated (e.g. uint16 → uint8, or ModFlag declared 32-bit)
			// shows up here rather than on an exotic keyboard.
			name:    "widest values are not truncated",
			flags:   ^uint64(0),
			keycode: ^uint16(0),
			want: matcher.KeyEvent{
				Modifiers: hotkey.ModFlag(^uint64(0)),
				KeyCode:   ^uint16(0),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := keyEventFromRecord(tt.flags, tt.keycode)
			if got != tt.want {
				t.Errorf("keyEventFromRecord(0x%x, %d) = %+v, want %+v",
					tt.flags, tt.keycode, got, tt.want)
			}
		})
	}
}

// TestSnapshot_FillsWholeBufferAndReusesIt exercises the real cgo snapshot
// against a ring no tap has ever written to: with no Install in this test
// binary the C-side ring and counter are still their BSS zero values, so the
// expected result is fully determined.
//
// What it pins:
//
//   - every one of the ringCap slots is written on each call. The buffer is
//     poisoned first, so a conversion loop that stopped short (or that sized
//     itself from the Go slice instead of the C array) leaves poison behind
//     and fails here. A short copy would mean the poller matches against
//     stale records from a previous tick;
//   - the caller's backing array is reused rather than replaced. The poller
//     allocates the buffer once and relies on snapshot writing in place —
//     this is the "no allocations in hot path" contract from matcher.go;
//   - the returned counter is the press count (0 here, since nothing has
//     been recorded), not an index or a slot count.
//
// NOT t.Parallel: it reads process-global C state.
func TestSnapshot_FillsWholeBufferAndReusesIt(t *testing.T) {
	buf := make([]matcher.KeyEvent, ringCap)
	poison := matcher.KeyEvent{Modifiers: 0xDEAD, KeyCode: 0xBEEF}
	for i := range buf {
		buf[i] = poison
	}
	before := &buf[0]

	cur := snapshot(buf)

	if cur != 0 {
		t.Errorf("snapshot returned %d, want 0 (no tap installed in this test "+
			"binary, so no key press can have been recorded)", cur)
	}
	if len(buf) != ringCap {
		t.Fatalf("len(buf) = %d after snapshot, want %d (buffer must not be resliced)", len(buf), ringCap)
	}
	for i, ev := range buf {
		if ev != (matcher.KeyEvent{}) {
			t.Errorf("buf[%d] = %+v, want zero value: the C ring is empty, so "+
				"a non-zero slot means the conversion did not write this "+
				"index (poison was %+v)", i, ev, poison)
		}
	}
	if &buf[0] != before {
		t.Errorf("snapshot replaced the caller's backing array; it must write in place")
	}

	// Second call on the same buffer: reuse must be safe and idempotent.
	// The poller calls snapshot on every tick with the same slice.
	if cur2 := snapshot(buf); cur2 != cur {
		t.Errorf("second snapshot returned %d, want %d (counter must not move without key presses)", cur2, cur)
	}
	if &buf[0] != before {
		t.Errorf("second snapshot replaced the caller's backing array; it must write in place")
	}
}

// TestSnapshot_ShortBuffer_Panics pins the guard on the one input that would
// corrupt memory rather than merely misbehave: the C side memcpys
// DND_RING_CAP records into the pointer it is given, so a shorter Go buffer
// would be written past its end. Failing loudly at the call is the only
// acceptable outcome.
//
// NOT t.Parallel: it reads process-global C state.
func TestSnapshot_ShortBuffer_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("snapshot(short buffer) did not panic; the C memcpy would "+
				"have written %d records past the end of the slice", ringCap-1)
		}
	}()

	snapshot(make([]matcher.KeyEvent, ringCap-1))
}

// TestSeq_NoTapInstalled_IsZero pins the other half of the poller's cgo
// bridge. pollSequence takes its starting `lastSeq` from this call, so a
// wrapper that returned garbage (wrong C type width, sign extension) would
// make the poller either skip real keystrokes or replay phantom ones.
//
// NOT t.Parallel: it reads process-global C state.
func TestSeq_NoTapInstalled_IsZero(t *testing.T) {
	if got := seq(); got != 0 {
		t.Errorf("seq() = %d, want 0 (no tap installed in this test binary)", got)
	}
}
