//go:build darwin

package eventtap

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// releaserTestDeps groups dependencies for Releaser unit tests per
// Go-test conventions. The DI seam (newReleaserWithDeps in tap_darwin.go)
// lets us inject fake disable/uninstall closures that record their
// invocation order — so we can verify disable-first ordering and
// two-layer idempotency without invoking the real cgo bridge.
type releaserTestDeps struct {
	disableCalls   atomic.Int64
	uninstallCalls atomic.Int64

	// callOrder records the sequence of "disable" / "uninstall" strings,
	// in the order the fake closures were invoked. Slice append is NOT
	// goroutine-safe in general, but in the Release path the two closures
	// are invoked from the same goroutine (mutex-serialised), so a plain
	// slice is sufficient. The race tests use atomic counters above.
	callOrder []string

	releaser *Releaser
}

// newReleaserTestDeps constructs a Releaser via the DI seam with fake
// closures that record their invocation count + order into the testDeps
// struct. stopPoller/pollerDone channels are pre-populated and pollerDone
// is pre-closed so Release does not block on the poller-wait step (the
// poller is exercised separately in pollMatched tests below).
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
}

// TestReleaser_Release_DisableBeforeUninstall verifies the invariant:
// the tap is disabled BEFORE CFRelease teardown so the keyboard recovers
// immediately even if subsequent CF teardown fails. The fake closures
// append "disable"/"uninstall" to callOrder; the test asserts the slice
// is exactly ["disable", "uninstall"] in that order.
func TestReleaser_Release_DisableBeforeUninstall(t *testing.T) {
	t.Parallel()

	d := newReleaserTestDeps(t)
	if err := d.releaser.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	want := []string{"disable", "uninstall"}
	if len(d.callOrder) != len(want) {
		t.Fatalf("callOrder = %v, want %v (len mismatch)", d.callOrder, want)
	}
	for i := range want {
		if d.callOrder[i] != want[i] {
			t.Errorf("callOrder[%d] = %q, want %q (disable-first)", i, d.callOrder[i], want[i])
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

// TestPoller_AtomicTrigger_SendsToSink verifies the matched-to-sink fan-out
// path: when the C callback flips `matched.Store(true)`, the poller goroutine
// observes it on the next ticker fire and pushes a struct{} send to sink
// within ~5×pollInterval = 50ms (the Phase 4 success criterion #2 budget).
func TestPoller_AtomicTrigger_SendsToSink(t *testing.T) {
	t.Parallel()

	var flag atomic.Bool
	flag.Store(true)
	sink := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		pollMatched(stop, &flag, sink, nil)
	}()

	select {
	case <-sink:
		// success — got the signal
	case <-time.After(5 * pollInterval):
		t.Fatalf("sink did not receive within %v (poller stuck?)", 5*pollInterval)
	}

	close(stop)
	select {
	case <-done:
		// poller goroutine exited cleanly
	case <-time.After(5 * pollInterval):
		t.Fatalf("poller goroutine did not exit within %v after close(stop)", 5*pollInterval)
	}

	// CompareAndSwap(true, false) must have cleared the flag after sending.
	if flag.Load() {
		t.Errorf("flag = true after poller fired, want false (CAS must reset)")
	}
}

// TestPoller_StopChannel_StopsPolling verifies clean shutdown: closing
// the stop channel exits the goroutine within one tick interval even when
// no match has been observed. This is the LIFO Cleanup chain's exit path
// (Release closes stopPoller as step 3 of the teardown sequence).
func TestPoller_StopChannel_StopsPolling(t *testing.T) {
	t.Parallel()

	var flag atomic.Bool // stays false
	sink := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		pollMatched(stop, &flag, sink, nil)
	}()

	// Let the poller run for ~3 ticks without setting flag.
	time.Sleep(3 * pollInterval)
	close(stop)

	select {
	case <-done:
		// success — goroutine exited within bounded time
	case <-time.After(5 * pollInterval):
		t.Fatalf("poller did not exit within %v after close(stop)", 5*pollInterval)
	}

	// Sink must NOT have received anything — flag was never true.
	select {
	case <-sink:
		t.Errorf("sink received a signal but flag was never true")
	default:
		// expected — no spurious send
	}
}

// TestPoller_FullSinkBuffer_DoesNotBlock verifies non-blocking-send
// semantics: if `sink` is already full (cap=1, prefilled), the poller's
// `select-default` branch swallows the send instead of blocking the
// goroutine. This is critical for a stuck poller would prevent
// the worker goroutine from exiting and leak the locked OS thread.
func TestPoller_FullSinkBuffer_DoesNotBlock(t *testing.T) {
	t.Parallel()

	var flag atomic.Bool
	flag.Store(true)
	sink := make(chan struct{}, 1)
	sink <- struct{}{} // pre-fill so the poller hits the default branch

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		pollMatched(stop, &flag, sink, nil)
	}()

	// Wait at least 2 ticks so the poller has had multiple chances to
	// hit the full-sink default branch; it MUST not block.
	time.Sleep(3 * pollInterval)
	close(stop)

	select {
	case <-done:
		// success — goroutine exited despite full sink
	case <-time.After(5 * pollInterval):
		t.Fatalf("poller blocked on full sink; did not exit within %v after close(stop)", 5*pollInterval)
	}

	// Sink still holds exactly one element (the pre-fill). No additional
	// signal was pushed because each CAS(true,false) → default-drop pair
	// "consumes" the flag without enqueueing.
	if got := len(sink); got != 1 {
		t.Errorf("sink len = %d, want 1 (pre-fill should remain; default branch must drop)", got)
	}
}

// TestEventTapMatched_Function_StoresAtomic validates the //export Go
// callback body. Threat requires the body be EXACTLY
// `matched.Store(true)` — this test verifies the SEMANTIC contract: calling
// the function flips the package-global atomic.Bool to true.
//
// The body cannot be grepped from this Go test (the eventtap_matched body
// is in tap_darwin.go which is the same package — the test can call it
// directly as a regular Go function since //export funcs are still
// reachable from Go). A reviewer who extends the body with allocation /
// channel send / panic will break the production CGEventTap callback path
// on a real signed binary; this unit test is the early-warning system.
func TestEventTapMatched_Function_StoresAtomic(t *testing.T) {
	// NOT t.Parallel — touches the package-global `matched`.

	matched.Store(false) // baseline
	eventtap_matched()
	if !matched.Load() {
		t.Errorf("matched = false after eventtap_matched(), want true (body must be matched.Store(true))")
	}

	// Restore baseline for other tests / production wire-up.
	matched.Store(false)
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
