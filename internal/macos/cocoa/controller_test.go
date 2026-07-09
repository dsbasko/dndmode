//go:build darwin

package cocoa

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	_ "github.com/dsbasko/dndmode/internal/runtimepin"
)

// fakeEnumerator is a test-injectable screenEnumerator. Returns a snapshot
// of the current ids slice (atomic swap on transitions for hot-plug
// simulations).
type fakeEnumerator struct {
	mu  sync.Mutex
	ids []uint32
}

func (f *fakeEnumerator) Enumerate() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint32, len(f.ids))
	copy(out, f.ids)
	return out
}

func (f *fakeEnumerator) set(ids []uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = ids
}

// fakeWindowFactory records Create/Close calls, allows failure injection.
//
// Pseudo-handles: each successful Create allocates a small heap byte, and
// hands its address as the unsafe.Pointer "handle". This avoids `go vet`'s
// "possible misuse of unsafe.Pointer" warning that triggers on the
// uintptr→unsafe.Pointer conversion pattern (since the cgo runtime cannot
// see synthesised numeric pointers as live references).
type fakeWindowFactory struct {
	mu          sync.Mutex
	creates     []uint32
	closes      []unsafe.Pointer
	failOnIndex int // -1 disables; 0-based index into Create call sequence
	createCount int
	handles     []*byte // keep heap-allocated bytes alive so handles stay valid
}

func newFakeWindowFactory() *fakeWindowFactory {
	return &fakeWindowFactory{failOnIndex: -1}
}

func (f *fakeWindowFactory) Create(displayID uint32) (unsafe.Pointer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.createCount
	f.createCount++
	f.creates = append(f.creates, displayID)
	if f.failOnIndex == idx {
		return nil, errors.New("fake create failure")
	}
	b := new(byte)
	f.handles = append(f.handles, b)
	return unsafe.Pointer(b), nil
}

func (f *fakeWindowFactory) Close(w unsafe.Pointer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, w)
}

// CreateCount returns f.createCount under lock — required by the race
// detector when the field is read outside the goroutine that mutates it
// (e.g. TestController_Debounce_TrailingEdge polls the count from the test
// goroutine while time.AfterFunc fires Create on a timer goroutine).
func (f *fakeWindowFactory) CreateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount
}

// CloseCount returns len(f.closes) under lock for the same reason as
// CreateCount.
func (f *fakeWindowFactory) CloseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.closes)
}

// fakeObservers tracks register/unregister calls.
type fakeObservers struct {
	mu          sync.Mutex
	registers   int
	unregisters int
	regErr      int // returned from Register
	unregErr    int // returned from Unregister
}

func (f *fakeObservers) Register() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers++
	return f.regErr
}

func (f *fakeObservers) Unregister() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregisters++
	return f.unregErr
}

// callLog is a shared ordered event recorder threaded into both the cursor and
// activation fakes so order assertions (Foreground-before-Hide on enter,
// Show-before-Background on teardown) are cheap. Each fake appends its event
// name ("foreground"/"hide"/"show"/"background") under lock when the log is
// non-nil; events() returns a snapshot for the ordering scans.
type callLog struct {
	mu     sync.Mutex
	events []string
}

func (l *callLog) append(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, name)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

// fakeCursorHider is a test-injectable cursorHider counting Hide/Show calls.
// Mirrors fakeObservers (sync.Mutex + counters); the HideCount/ShowCount
// accessors read under lock for the race detector, mirroring
// fakeWindowFactory.CreateCount. The optional log records Hide/Show in the
// shared ordered call-log for ordering assertions.
type fakeCursorHider struct {
	mu    sync.Mutex
	hides int
	shows int
	log   *callLog
}

func (f *fakeCursorHider) Hide() {
	f.mu.Lock()
	f.hides++
	f.mu.Unlock()
	if f.log != nil {
		f.log.append("hide")
	}
}

func (f *fakeCursorHider) Show() {
	f.mu.Lock()
	f.shows++
	f.mu.Unlock()
	if f.log != nil {
		f.log.append("show")
	}
}

func (f *fakeCursorHider) HideCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hides
}

func (f *fakeCursorHider) ShowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shows
}

// fakeActivationPolicy is a test-injectable activationPolicy counting
// Foreground/Background calls. Clone of fakeCursorHider (sync.Mutex + counters
// + accessors under lock for the race detector); the optional log records
// Foreground/Background in the shared ordered call-log.
type fakeActivationPolicy struct {
	mu          sync.Mutex
	foregrounds int
	backgrounds int
	log         *callLog
}

func (f *fakeActivationPolicy) Foreground() {
	f.mu.Lock()
	f.foregrounds++
	f.mu.Unlock()
	if f.log != nil {
		f.log.append("foreground")
	}
}

func (f *fakeActivationPolicy) Background() {
	f.mu.Lock()
	f.backgrounds++
	f.mu.Unlock()
	if f.log != nil {
		f.log.append("background")
	}
}

func (f *fakeActivationPolicy) ForegroundCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.foregrounds
}

func (f *fakeActivationPolicy) BackgroundCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backgrounds
}

// inlineDispatcher executes Dispatch'd functions synchronously inside the
// caller's goroutine. Production uses cgoMainDispatcher (DispatchMain) which
// hops to the main run loop; tests do not run NSApp.run, so we collapse the
// dispatch into an inline call so Release / debounce-reconcile bodies are
// observable.
type inlineDispatcher struct {
	mu    sync.Mutex
	calls int
}

func (d *inlineDispatcher) Dispatch(fn func()) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	fn()
}

type testDeps struct {
	controller *Controller
	enumerator *fakeEnumerator
	factory    *fakeWindowFactory
	observers  *fakeObservers
	dispatcher *inlineDispatcher
	cursor     *fakeCursorHider
	activation *fakeActivationPolicy
	callLog    *callLog
	logBuf     *bytes.Buffer
}

func newTestDeps(t *testing.T, debounceWin time.Duration) *testDeps {
	t.Helper()
	enum := &fakeEnumerator{}
	fac := newFakeWindowFactory()
	obs := &fakeObservers{}
	disp := &inlineDispatcher{}
	cl := &callLog{}
	cur := &fakeCursorHider{log: cl}
	act := &fakeActivationPolicy{log: cl}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if debounceWin == 0 {
		debounceWin = 250 * time.Millisecond
	}
	c := newControllerWithDeps(logger, enum, fac, obs, disp, cur, act, debounceWin)
	t.Cleanup(func() {
		// Detach in case test forgot Release.
		setOnScreensChanged(nil)
	})
	return &testDeps{controller: c, enumerator: enum, factory: fac, observers: obs, dispatcher: disp, cursor: cur, activation: act, callLog: cl, logBuf: logBuf}
}

func TestController_Reconcile_FullRebuild(t *testing.T) {
	td := newTestDeps(t, 0)
	defer td.controller.Release()

	td.enumerator.set([]uint32{1, 2, 3})
	if err := td.controller.reconcile(true); err != nil {
		t.Fatalf("reconcile #1: %v", err)
	}
	if got := td.controller.WindowCount(); got != 3 {
		t.Errorf("WindowCount #1 = %d, want 3", got)
	}

	// Transition: [1,2,3] → [4,5] (full rebuild → close all 3, create 2)
	td.enumerator.set([]uint32{4, 5})
	closesBefore := td.factory.CloseCount()
	if err := td.controller.reconcile(false); err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	closeDelta := td.factory.CloseCount() - closesBefore
	if closeDelta < 3 {
		t.Errorf("expected at least 3 closes between reconciles, got %d", closeDelta)
	}
	if got := td.controller.WindowCount(); got != 2 {
		t.Errorf("WindowCount #2 = %d, want 2", got)
	}

	// Transition: [4,5] → [6] (close 2, create 1)
	td.enumerator.set([]uint32{6})
	if err := td.controller.reconcile(false); err != nil {
		t.Fatalf("reconcile #3: %v", err)
	}
	if got := td.controller.WindowCount(); got != 1 {
		t.Errorf("WindowCount #3 = %d, want 1", got)
	}
}

func TestController_Reconcile_AbortOnCreateFail(t *testing.T) {
	td := newTestDeps(t, 0)
	defer td.controller.Release()

	td.factory.failOnIndex = 1 // 2nd of 3 fails
	td.enumerator.set([]uint32{10, 20, 30})

	err := td.controller.reconcile(true)
	if err == nil {
		t.Fatal("reconcile returned nil error on inject failure")
	}
	if !strings.Contains(err.Error(), "displayID=20") {
		t.Errorf("error missing displayID context: %v", err)
	}
	if got := td.controller.WindowCount(); got != 0 {
		t.Errorf("WindowCount after abort = %d, want 0 (all destroyed)", got)
	}
}

func TestController_Reconcile_ColdStart_NoDisplays(t *testing.T) {
	td := newTestDeps(t, 0)
	defer td.controller.Release()

	td.enumerator.set([]uint32{})
	err := td.controller.reconcile(true)
	if !errors.Is(err, ErrNoDisplays) {
		t.Errorf("err = %v, want errors.Is ErrNoDisplays", err)
	}
	if got := td.controller.WindowCount(); got != 0 {
		t.Errorf("WindowCount = %d, want 0", got)
	}
}

func TestController_Reconcile_RuntimeDrop_NoDisplays(t *testing.T) {
	td := newTestDeps(t, 0)
	defer td.controller.Release()

	// Start with 2 displays.
	td.enumerator.set([]uint32{100, 200})
	if err := td.controller.reconcile(true); err != nil {
		t.Fatalf("cold-start: %v", err)
	}

	// Runtime drop to 0.
	td.enumerator.set([]uint32{})
	err := td.controller.reconcile(false)
	if err != nil {
		t.Errorf("runtime reconcile with 0 displays returned err: %v (should be nil)", err)
	}
	if got := td.controller.WindowCount(); got != 0 {
		t.Errorf("WindowCount = %d, want 0", got)
	}
	if !strings.Contains(td.logBuf.String(), "all displays disconnected") {
		t.Errorf("expected slog.Warn 'all displays disconnected' in log; got:\n%s", td.logBuf.String())
	}
}

func TestController_Debounce_TrailingEdge(t *testing.T) {
	td := newTestDeps(t, 50*time.Millisecond)
	defer td.controller.Release()

	td.enumerator.set([]uint32{1, 2})
	if err := td.controller.reconcile(true); err != nil {
		t.Fatalf("cold-start: %v", err)
	}

	creates0 := td.factory.CreateCount()
	// Fire 5 events in 30ms — all within debounce window.
	for range 5 {
		td.controller.onScreensChanged()
		time.Sleep(5 * time.Millisecond)
	}
	// During the burst no reconcile should have fired (debounce still pending).
	creates1 := td.factory.CreateCount()
	if creates1 != creates0 {
		t.Errorf("reconcile fired during debounce burst: creates0=%d creates1=%d", creates0, creates1)
	}

	// Wait past debounce window.
	time.Sleep(120 * time.Millisecond)

	creates2 := td.factory.CreateCount()
	// One reconcile = createCount += len(ids) = +2. Allow >=2 in case timing
	// produced exactly one or somehow two (we tolerate one-extra to keep
	// test non-flaky on busy CI but assert NOT 5 reconciles worth).
	delta := creates2 - creates0
	if delta < 2 {
		t.Errorf("trailing-edge reconcile did not fire: createCount delta = %d, want >= 2", delta)
	}
	if delta > 4 {
		t.Errorf("debounce did not collapse burst: createCount delta = %d, want <= 4 (one or two reconciles, not 5)", delta)
	}
}

func TestController_WindowsByDisplayID(t *testing.T) {
	td := newTestDeps(t, 0)
	defer td.controller.Release()

	td.enumerator.set([]uint32{0xAA, 0xBB})
	if err := td.controller.reconcile(true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	td.controller.mu.Lock()
	_, hasAA := td.controller.windows[0xAA]
	_, hasBB := td.controller.windows[0xBB]
	count := len(td.controller.windows)
	td.controller.mu.Unlock()

	if !hasAA || !hasBB {
		t.Errorf("windows map missing keys: hasAA=%v hasBB=%v", hasAA, hasBB)
	}
	if count != 2 {
		t.Errorf("windows count = %d, want 2", count)
	}
}

func TestController_Release_Idempotent(t *testing.T) {
	td := newTestDeps(t, 0)

	td.enumerator.set([]uint32{1, 2, 3})
	if err := td.controller.reconcile(true); err != nil {
		t.Fatalf("cold-start: %v", err)
	}

	// First Release.
	if err := td.controller.Release(); err != nil {
		t.Errorf("first Release: %v", err)
	}
	td.observers.mu.Lock()
	unregs1 := td.observers.unregisters
	td.observers.mu.Unlock()
	td.factory.mu.Lock()
	closes1 := len(td.factory.closes)
	td.factory.mu.Unlock()

	if unregs1 != 1 {
		t.Errorf("unregister calls after 1st Release = %d, want 1", unregs1)
	}
	if closes1 < 3 {
		t.Errorf("close calls after 1st Release = %d, want >= 3", closes1)
	}
	if got := td.controller.WindowCount(); got != 0 {
		t.Errorf("WindowCount after 1st Release = %d, want 0", got)
	}

	// Second Release — must be no-op.
	if err := td.controller.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	td.observers.mu.Lock()
	unregs2 := td.observers.unregisters
	td.observers.mu.Unlock()
	td.factory.mu.Lock()
	closes2 := len(td.factory.closes)
	td.factory.mu.Unlock()

	if unregs2 != 1 {
		t.Errorf("unregister calls after 2nd Release = %d, want 1 (idempotent)", unregs2)
	}
	if closes2 != closes1 {
		t.Errorf("close calls after 2nd Release = %d, want %d (idempotent)", closes2, closes1)
	}
}

// assertImmediatelyBefore scans the ordered call-log snapshot and asserts that
// `pred` occurs immediately before `succ` (i.e. the entry at index-1 of succ is
// pred). Used to prove Foreground-before-Hide on enter and Show-before-Background
// on teardown without timing assumptions.
func assertImmediatelyBefore(t *testing.T, events []string, pred, succ string) {
	t.Helper()
	for i, e := range events {
		if e == succ {
			if i == 0 || events[i-1] != pred {
				t.Errorf("expected %q immediately before %q; got events=%v", pred, succ, events)
			}
			return
		}
	}
	t.Errorf("event %q not found in call-log; got events=%v", succ, events)
}

// TestController_CursorHide covers the coupled one-shot activation-flip +
// cursor hide/show wiring (revised): on a successful cold-start the app
// flips to Accessory+active (Foreground) BEFORE the cursor Hide, exactly once,
// never on ErrNoDisplays, never per hot-plug rebuild; on Release the cursor is
// restored (Show) BEFORE reverting to Prohibited (Background), exactly once iff a
// hide/flip happened, idempotent across repeated Release.
func TestController_CursorHide(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(td *testDeps)
		validate func(t *testing.T, td *testDeps)
	}{
		{
			// Case 1: successful cold-start flips to foreground then hides, both
			// exactly once; no show / background yet. Foreground precedes Hide.
			name: "cold-start with screens hides once",
			setup: func(td *testDeps) {
				td.enumerator.set([]uint32{1, 2})
				if err := td.controller.CreateWindowsForAllScreens(); err != nil {
					t.Fatalf("CreateWindowsForAllScreens: %v", err)
				}
			},
			validate: func(t *testing.T, td *testDeps) {
				if got := td.cursor.HideCount(); got != 1 {
					t.Errorf("HideCount = %d, want 1", got)
				}
				if got := td.cursor.ShowCount(); got != 0 {
					t.Errorf("ShowCount = %d, want 0", got)
				}
				if got := td.activation.ForegroundCount(); got != 1 {
					t.Errorf("ForegroundCount = %d, want 1", got)
				}
				if got := td.activation.BackgroundCount(); got != 0 {
					t.Errorf("BackgroundCount = %d, want 0", got)
				}
				assertImmediatelyBefore(t, td.callLog.snapshot(), "foreground", "hide")
			},
		},
		{
			// Case 2: ErrNoDisplays cold-start must NOT flip foreground NOR hide
			// (nothing covered).
			name: "cold-start no displays does not hide",
			setup: func(td *testDeps) {
				td.enumerator.set([]uint32{})
				err := td.controller.CreateWindowsForAllScreens()
				if !errors.Is(err, ErrNoDisplays) {
					t.Fatalf("err = %v, want errors.Is ErrNoDisplays", err)
				}
			},
			validate: func(t *testing.T, td *testDeps) {
				if got := td.cursor.HideCount(); got != 0 {
					t.Errorf("HideCount = %d, want 0 (no hide on ErrNoDisplays)", got)
				}
				if got := td.cursor.ShowCount(); got != 0 {
					t.Errorf("ShowCount = %d, want 0", got)
				}
				if got := td.activation.ForegroundCount(); got != 0 {
					t.Errorf("ForegroundCount = %d, want 0 (no foreground on ErrNoDisplays)", got)
				}
			},
		},
		{
			// Case 3: Release after a successful create restores the cursor (Show)
			// then reverts to Prohibited (Background), each exactly once. Show
			// precedes Background.
			name: "release after create shows once",
			setup: func(td *testDeps) {
				td.enumerator.set([]uint32{1})
				if err := td.controller.CreateWindowsForAllScreens(); err != nil {
					t.Fatalf("CreateWindowsForAllScreens: %v", err)
				}
				if err := td.controller.Release(); err != nil {
					t.Fatalf("Release: %v", err)
				}
			},
			validate: func(t *testing.T, td *testDeps) {
				if got := td.cursor.HideCount(); got != 1 {
					t.Errorf("HideCount = %d, want 1", got)
				}
				if got := td.cursor.ShowCount(); got != 1 {
					t.Errorf("ShowCount = %d, want 1", got)
				}
				if got := td.activation.BackgroundCount(); got != 1 {
					t.Errorf("BackgroundCount = %d, want 1", got)
				}
				assertImmediatelyBefore(t, td.callLog.snapshot(), "show", "background")
			},
		},
		{
			// Case 4: Release twice still shows + backgrounds exactly once
			// (idempotency).
			name: "double release shows once",
			setup: func(td *testDeps) {
				td.enumerator.set([]uint32{1})
				if err := td.controller.CreateWindowsForAllScreens(); err != nil {
					t.Fatalf("CreateWindowsForAllScreens: %v", err)
				}
				if err := td.controller.Release(); err != nil {
					t.Fatalf("Release #1: %v", err)
				}
				if err := td.controller.Release(); err != nil {
					t.Fatalf("Release #2: %v", err)
				}
			},
			validate: func(t *testing.T, td *testDeps) {
				if got := td.cursor.ShowCount(); got != 1 {
					t.Errorf("ShowCount after double Release = %d, want 1 (idempotent)", got)
				}
				if got := td.activation.BackgroundCount(); got != 1 {
					t.Errorf("BackgroundCount after double Release = %d, want 1 (idempotent)", got)
				}
			},
		},
		{
			// Case 5: hot-plug rebuilds after a successful cold-start must NOT
			// re-hide NOR re-foreground — Hide and Foreground stay at exactly 1.
			name: "hot-plug rebuilds do not re-hide",
			setup: func(td *testDeps) {
				td.enumerator.set([]uint32{1, 2})
				if err := td.controller.CreateWindowsForAllScreens(); err != nil {
					t.Fatalf("CreateWindowsForAllScreens: %v", err)
				}
				// Three hot-plug rebuilds via reconcile(false) (the inline
				// dispatcher runs synchronously, so no debounce sleep needed).
				td.enumerator.set([]uint32{3})
				for i := range 3 {
					if err := td.controller.reconcile(false); err != nil {
						t.Fatalf("reconcile(false) #%d: %v", i+1, err)
					}
				}
			},
			validate: func(t *testing.T, td *testDeps) {
				if got := td.cursor.HideCount(); got != 1 {
					t.Errorf("HideCount after hot-plug rebuilds = %d, want 1 (not per-rebuild)", got)
				}
				if got := td.activation.ForegroundCount(); got != 1 {
					t.Errorf("ForegroundCount after hot-plug rebuilds = %d, want 1 (cold-start only, not per-rebuild)", got)
				}
			},
		},
		{
			// Case 6: Release WITHOUT a successful create (cursorHidden==false)
			// must NOT show NOR background.
			name: "release without create does not show",
			setup: func(td *testDeps) {
				if err := td.controller.Release(); err != nil {
					t.Fatalf("Release: %v", err)
				}
			},
			validate: func(t *testing.T, td *testDeps) {
				if got := td.cursor.HideCount(); got != 0 {
					t.Errorf("HideCount = %d, want 0", got)
				}
				if got := td.cursor.ShowCount(); got != 0 {
					t.Errorf("ShowCount = %d, want 0 (no hide → no show)", got)
				}
				if got := td.activation.BackgroundCount(); got != 0 {
					t.Errorf("BackgroundCount = %d, want 0 (no foreground → no background)", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t, 0)
			tt.setup(td)
			tt.validate(t, td)
		})
	}
}
