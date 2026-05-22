//go:build darwin

package permissions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChecker advances a per-CALL index when IsAXTrusted / IsIMGranted is
// invoked. The slices encode the call-by-call answer; the algorithm in
// WaitForGrants performs:
//
//   - 1 initial-assessment call (Pre-loop): IsAXTrusted + IsIMGranted.
//   - N polling-cycle calls (Loop body): IsAXTrusted + IsIMGranted per cycle.
//
// So a slice of length N+1 encodes initial + N polling cycles. If the
// algorithm makes more calls than the slice length (e.g. SIGINT race where
// extra ticks might fire before context cancellation propagates), the
// helper returns the LAST slice value (treated as a steady-state).
type fakeChecker struct {
	axStates []bool
	imStates []bool
	axIdx    atomic.Int64
	imIdx    atomic.Int64
}

func newFakeChecker(axSeq, imSeq []bool) *fakeChecker {
	return &fakeChecker{axStates: axSeq, imStates: imSeq}
}

func (f *fakeChecker) IsAXTrusted() bool {
	i := f.axIdx.Add(1) - 1
	if int(i) >= len(f.axStates) {
		return f.axStates[len(f.axStates)-1]
	}
	return f.axStates[i]
}

func (f *fakeChecker) IsIMGranted() bool {
	i := f.imIdx.Add(1) - 1
	if int(i) >= len(f.imStates) {
		return f.imStates[len(f.imStates)-1]
	}
	return f.imStates[i]
}

type fakeLinker struct {
	axCalls atomic.Int64
	imCalls atomic.Int64
	axErr   error
	imErr   error
}

func (l *fakeLinker) OpenAX() error {
	l.axCalls.Add(1)
	return l.axErr
}

func (l *fakeLinker) OpenIM() error {
	l.imCalls.Add(1)
	return l.imErr
}

type fakeStatusWriter struct {
	mu         sync.Mutex
	updates    [][2]bool
	finalCalls atomic.Int64
	// events records the call sequence as opaque labels — used by
	// TestWaitForGrants_EntryBanner_CalledBeforeInitialProbe to assert
	// ordering of EntryBanner / Update / Final without coupling to
	// timestamps. Labels: "entry", "update(ax,im)", "final".
	events []string
}

func (f *fakeStatusWriter) Update(ax, im bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, [2]bool{ax, im})
	f.events = append(f.events, fmt.Sprintf("update(%t,%t)", ax, im))
}

func (f *fakeStatusWriter) Final() {
	f.mu.Lock()
	f.events = append(f.events, "final")
	f.mu.Unlock()
	f.finalCalls.Add(1)
}

// EntryBanner records the call without touching updates (no state
// change) — see TestWaitForGrants_EntryBanner_CalledBeforeInitialProbe.
func (f *fakeStatusWriter) EntryBanner() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "entry")
}

func (f *fakeStatusWriter) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

func (f *fakeStatusWriter) updateAt(idx int) [2]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updates[idx]
}

func (f *fakeStatusWriter) eventSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	copy(out, f.events)
	return out
}

type waitTestDeps struct {
	checker *fakeChecker
	linker  *fakeLinker
	status  *fakeStatusWriter
	logBuf  *bytes.Buffer
	log     *slog.Logger
}

func newWaitTestDeps(t *testing.T, axSeq, imSeq []bool) *waitTestDeps {
	t.Helper()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &waitTestDeps{
		checker: newFakeChecker(axSeq, imSeq),
		linker:  &fakeLinker{},
		status:  &fakeStatusWriter{},
		logBuf:  logBuf,
		log:     logger,
	}
}

func TestWaitForGrants_BothGrantedAtEntry_ReturnsNilImmediately(t *testing.T) {
	td := newWaitTestDeps(t, []bool{true}, []bool{true})
	var promptCalls atomic.Int64
	prompt := func() { promptCalls.Add(1) }

	err := WaitForGrants(context.Background(), td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}
	if got := promptCalls.Load(); got != 0 {
		t.Errorf("prompt called %d times, want 0 (AX already granted)", got)
	}
	if got := td.linker.axCalls.Load(); got != 0 {
		t.Errorf("OpenAX called %d times, want 0 (AX already granted)", got)
	}
	if got := td.linker.imCalls.Load(); got != 0 {
		t.Errorf("OpenIM called %d times, want 0 (IM already granted)", got)
	}
	if got := td.status.finalCalls.Load(); got != 1 {
		t.Errorf("Final called %d times, want 1", got)
	}
}

func TestWaitForGrants_AXMissingAtEntry_PromptAndOpenAXOnce(t *testing.T) {
	// 1 initial + 2 polling = 3 calls. AX grants on cycle 2 (slice idx 2).
	td := newWaitTestDeps(t,
		[]bool{false, false, true},
		[]bool{true, true, true},
	)
	var promptCalls atomic.Int64
	prompt := func() { promptCalls.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}
	if got, want := promptCalls.Load(), int64(1); got != want {
		t.Errorf("prompt calls = %d, want %d (one-shot)", got, want)
	}
	if got, want := td.linker.axCalls.Load(), int64(1); got != want {
		t.Errorf("OpenAX calls = %d, want %d (one-shot)", got, want)
	}
	if got, want := td.linker.imCalls.Load(), int64(0); got != want {
		t.Errorf("OpenIM calls = %d, want %d (IM granted at entry)", got, want)
	}
	if got, want := td.status.finalCalls.Load(), int64(1); got != want {
		t.Errorf("Final calls = %d, want %d", got, want)
	}
}

func TestWaitForGrants_IMMissingAtEntry_OpenIMOnceNoPromptCallForAX(t *testing.T) {
	// 1 initial + 2 polling = 3 calls. IM grants on cycle 2.
	td := newWaitTestDeps(t,
		[]bool{true, true, true},
		[]bool{false, false, true},
	)
	var promptCalls atomic.Int64
	prompt := func() { promptCalls.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}
	if got := promptCalls.Load(); got != 0 {
		t.Errorf("prompt calls = %d, want 0 (AX granted)", got)
	}
	if got := td.linker.axCalls.Load(); got != 0 {
		t.Errorf("OpenAX calls = %d, want 0 (AX granted at entry)", got)
	}
	if got, want := td.linker.imCalls.Load(), int64(1); got != want {
		t.Errorf("OpenIM calls = %d, want %d (one-shot)", got, want)
	}
}

func TestWaitForGrants_SIGINT_ReturnsCtxErr(t *testing.T) {
	// Indeterminate cycle count under SIGINT race. Pre-fill with falses so
	// any extra calls reach the last-value guard (still false).
	td := newWaitTestDeps(t,
		[]bool{false, false, false, false},
		[]bool{false, false, false, false},
	)
	prompt := func() {}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 25*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForGrants err = %v, want context.Canceled", err)
	}
	if got := td.status.finalCalls.Load(); got != 0 {
		t.Errorf("Final calls = %d, want 0 (never reached grants)", got)
	}
}

func TestWaitForGrants_GrantEdge_LogsOncePerKind(t *testing.T) {
	// 1 initial + 3 polling = 4 calls.
	// AX grants on cycle 1 (slice idx 1); IM grants on cycle 3 (slice idx 3).
	td := newWaitTestDeps(t,
		[]bool{false, true, true, true},
		[]bool{false, false, false, true},
	)
	prompt := func() {}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}

	logStr := td.logBuf.String()
	if got, want := strings.Count(logStr, `msg="permission granted"`), 2; got != want {
		t.Errorf("'permission granted' count = %d, want %d\nlog:\n%s", got, want, logStr)
	}
	if !strings.Contains(logStr, "kind=ax") {
		t.Errorf("log missing kind=ax\nlog:\n%s", logStr)
	}
	if !strings.Contains(logStr, "kind=im") {
		t.Errorf("log missing kind=im\nlog:\n%s", logStr)
	}
}

func TestWaitForGrants_OpenAXFailure_LogsWarnAndContinues(t *testing.T) {
	// 1 initial + 1 polling = 2 calls. AX grants on cycle 1.
	td := newWaitTestDeps(t,
		[]bool{false, true},
		[]bool{true, true},
	)
	td.linker.axErr = errors.New("simulated /usr/bin/open missing")
	prompt := func() {}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil (polling continues past linker failure)", err)
	}

	logStr := td.logBuf.String()
	if !strings.Contains(logStr, "open AX settings") {
		t.Errorf("log missing 'open AX settings'\nlog:\n%s", logStr)
	}
	if !strings.Contains(logStr, "level=WARN") {
		t.Errorf("log missing 'level=WARN'\nlog:\n%s", logStr)
	}
}

func TestWaitForGrants_OpenIMFailure_LogsWarnAndContinues(t *testing.T) {
	// 1 initial + 1 polling = 2 calls. IM grants on cycle 1.
	td := newWaitTestDeps(t,
		[]bool{true, true},
		[]bool{false, true},
	)
	td.linker.imErr = errors.New("simulated /usr/bin/open missing")
	prompt := func() {}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}
	logStr := td.logBuf.String()
	if !strings.Contains(logStr, "open IM settings") {
		t.Errorf("log missing 'open IM settings'\nlog:\n%s", logStr)
	}
	if !strings.Contains(logStr, "level=WARN") {
		t.Errorf("log missing 'level=WARN'\nlog:\n%s", logStr)
	}
}

func TestWaitForGrants_StatusUpdateInvokedEveryCycle(t *testing.T) {
	// 1 initial + 2 polling = 3 calls. AX grants on cycle 2; IM grants on cycle 1.
	td := newWaitTestDeps(t,
		[]bool{false, false, true},
		[]bool{false, true, true},
	)
	prompt := func() {}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, prompt, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}

	if got := td.status.updateCount(); got != 3 {
		t.Errorf("Update calls = %d, want 3 (1 initial + 2 polling)", got)
	}
	first := td.status.updateAt(0)
	if first != [2]bool{false, false} {
		t.Errorf("initial Update = %v, want {false, false}", first)
	}
	last := td.status.updateAt(td.status.updateCount() - 1)
	if last != [2]bool{true, true} {
		t.Errorf("final Update = %v, want {true, true}", last)
	}
}

func TestWaitForGrants_NilPrompt_NoPanic(t *testing.T) {
	// 1 initial + 1 polling = 2 calls. AX grants on cycle 1.
	td := newWaitTestDeps(t,
		[]bool{false, true},
		[]bool{true, true},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitForGrants(ctx, td.checker, td.linker, td.status, nil, td.log, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}
}

func TestWaitForGrants_NilLog_FallsBackToDefault(t *testing.T) {
	td := newWaitTestDeps(t, []bool{true}, []bool{true})
	prompt := func() {}

	err := WaitForGrants(context.Background(), td.checker, td.linker, td.status, prompt, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForGrants err = %v, want nil", err)
	}
}

func TestNewCgoChecker_ReturnsImplWithoutPanic(t *testing.T) {
	chk := NewCgoChecker()
	if chk == nil {
		t.Fatal("NewCgoChecker() returned nil")
	}
	// Note: actual cgo invocation is covered by permissions_smoketest_test.go
	// (TestSmoke_AX_IsTrusted_NonPanic + TestSmoke_IM_CheckAccess_NonPanic).
	// This test only verifies the constructor wires the impl correctly.
}

// TestWaitForGrants_EntryBanner_CalledBeforeInitialProbe verifies
// fix: WaitForGrants invokes status.EntryBanner exactly ONCE, before the
// initial AX/IM probe, regardless of cold-start grant state. The fake
// StatusWriter records "entry", "update(...)", "final" labels in call
// order, and we assert that "entry" precedes every "update".
//
// Two scenarios covered via subtests:
//   - both granted at entry → ["entry", "update(true,true)", "final"]
//   - polling required      → ["entry", "update(false,false)", "update(...)", ..., "final"]
//
// This is the regression gate against the pre- silent state where
// the TTY user saw \r-overwrite cycles immediately without the "we are
// waiting for grants…" banner — a the UI spec "Polling entry banner (TTY)"
// violation.
func TestWaitForGrants_EntryBanner_CalledBeforeInitialProbe(t *testing.T) {
	t.Run("both_granted_at_entry", func(t *testing.T) {
		td := newWaitTestDeps(t, []bool{true}, []bool{true})
		err := WaitForGrants(context.Background(), td.checker, td.linker, td.status, func() {}, td.log, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("WaitForGrants err = %v, want nil", err)
		}
		got := td.status.eventSnapshot()
		want := []string{"entry", "update(true,true)", "final"}
		if !equalStringSlices(got, want) {
			t.Errorf("event sequence = %v, want %v", got, want)
		}
	})

	t.Run("polling_until_granted", func(t *testing.T) {
		// 1 initial + 2 polling = 3 IsAXTrusted / IsIMGranted calls.
		td := newWaitTestDeps(t,
			[]bool{false, false, true},
			[]bool{false, true, true},
		)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		err := WaitForGrants(ctx, td.checker, td.linker, td.status, func() {}, td.log, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("WaitForGrants err = %v, want nil", err)
		}
		got := td.status.eventSnapshot()
		if len(got) == 0 || got[0] != "entry" {
			t.Fatalf("event[0] = %q, want %q (EntryBanner must precede first probe)", firstOrEmpty(got), "entry")
		}
		// Exactly one "entry" — no re-banner on polling cycles.
		entryCount := 0
		for _, e := range got {
			if e == "entry" {
				entryCount++
			}
		}
		if entryCount != 1 {
			t.Errorf("entry banner count = %d, want 1; full sequence = %v", entryCount, got)
		}
		// Final must be the last event.
		if got[len(got)-1] != "final" {
			t.Errorf("last event = %q, want %q; full sequence = %v", got[len(got)-1], "final", got)
		}
	})
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
