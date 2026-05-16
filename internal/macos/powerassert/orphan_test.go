//go:build darwin

package powerassert_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/dsbasko/dndmode/internal/macos/powerassert"
	"github.com/dsbasko/dndmode/internal/macos/powerassert/mocks"
)

// testDeps groups the gomock controller, three DI mocks and a captured
// slog buffer for assertion of log lines emitted by CleanupOrphans.
//
// Mirrors the cocoa/controller_test.go:138-164 testDeps pattern: one
// helper produces all dependencies wired together, t.Cleanup handles the
// gomock controller lifecycle, callers focus on the EXPECT() programming
// for their specific scenario.
type testDeps struct {
	ctrl     *gomock.Controller
	mockEnum *mocks.MockAssertionEnumerator
	mockRel  *mocks.MockAssertionReleaser
	mockLive *mocks.MockLiveChecker
	logBuf   *bytes.Buffer
	log      *slog.Logger
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &testDeps{
		ctrl:     ctrl,
		mockEnum: mocks.NewMockAssertionEnumerator(ctrl),
		mockRel:  mocks.NewMockAssertionReleaser(ctrl),
		mockLive: mocks.NewMockLiveChecker(ctrl),
		logBuf:   logBuf,
		log:      logger,
	}
}

// TestCleanupOrphans_EmptyEnumeration_ReturnsNilNoReleases verifies the
// happy-path: enumeration returned zero orphans → no liveness probes, no
// release calls, no error, no "released" log line emitted. This is the
// dominant case on a freshly-rebooted machine where no previous dndmode
// instance ever ran.
func TestCleanupOrphans_EmptyEnumeration_ReturnsNilNoReleases(t *testing.T) {
	td := newTestDeps(t)

	td.mockEnum.EXPECT().
		Enumerate("dndmode active", "PreventUserIdleSystemSleep", os.Getpid()).
		Return(nil, nil)
	// rel + live deliberately NOT EXPECT — gomock controller will fail the
	// test if either is invoked on the empty-orphan path.

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v, want nil", err)
	}
	if strings.Contains(td.logBuf.String(), "released orphan assertion") {
		t.Errorf("log contained 'released orphan assertion' but no orphans were enumerated:\n%s", td.logBuf.String())
	}
}

// TestCleanupOrphans_SingleDeadOrphan_ReleasedAndLogged verifies the
// canonical path: one dead-PID orphan → IsAlive false → Release
// called → log.Info "released orphan assertion" with PID + ID fields.
func TestCleanupOrphans_SingleDeadOrphan_ReleasedAndLogged(t *testing.T) {
	td := newTestDeps(t)

	orphans := []powerassert.Orphan{{PID: 12345, ID: 0xabcd}}

	td.mockEnum.EXPECT().
		Enumerate("dndmode active", "PreventUserIdleSystemSleep", os.Getpid()).
		Return(orphans, nil)
	td.mockLive.EXPECT().IsAlive(12345).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xabcd)).Return(nil)

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v, want nil", err)
	}
	logStr := td.logBuf.String()
	if !strings.Contains(logStr, "released orphan assertion") {
		t.Errorf("log missing 'released orphan assertion':\n%s", logStr)
	}
	if !strings.Contains(logStr, "pid=12345") {
		t.Errorf("log missing pid=12345 field:\n%s", logStr)
	}
}

// TestCleanupOrphans_LiveOrphan_BailsWithConcurrentInstance verifies
// live-PID match → ErrConcurrentInstance wrapped with PID;
// Release is NOT invoked on the live orphan (gomock auto-fails if it is).
func TestCleanupOrphans_LiveOrphan_BailsWithConcurrentInstance(t *testing.T) {
	td := newTestDeps(t)

	orphans := []powerassert.Orphan{{PID: 99999, ID: 0x1234}}

	td.mockEnum.EXPECT().
		Enumerate("dndmode active", "PreventUserIdleSystemSleep", os.Getpid()).
		Return(orphans, nil)
	td.mockLive.EXPECT().IsAlive(99999).Return(true)
	// rel deliberately NOT EXPECT — live-PID bail must NOT release.

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err == nil {
		t.Fatalf("CleanupOrphans returned nil; want ErrConcurrentInstance wrap")
	}
	if !errors.Is(err, powerassert.ErrConcurrentInstance) {
		t.Errorf("errors.Is(err, ErrConcurrentInstance) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("error message missing PID 99999: %q", err.Error())
	}
}

// TestCleanupOrphans_MultipleOrphans_LiveAfterDead_PartialProcessing
// verifies that the live-PID bail in is a *short-circuit*: once
// the second orphan triggers ErrConcurrentInstance, the third orphan is
// never inspected (gomock auto-fails the test if IsAlive/Release is
// invoked on the unprocessed orphan).
func TestCleanupOrphans_MultipleOrphans_LiveAfterDead_PartialProcessing(t *testing.T) {
	td := newTestDeps(t)

	orphans := []powerassert.Orphan{
		{PID: 1111, ID: 0xa}, // dead → released
		{PID: 2222, ID: 0xb}, // live → bail
		{PID: 3333, ID: 0xc}, // never reached
	}

	td.mockEnum.EXPECT().
		Enumerate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(orphans, nil)

	// Order doesn't matter to gomock by default, but the SHAPE of EXPECTs
	// asserts only the calls we expect happen.
	td.mockLive.EXPECT().IsAlive(1111).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xa)).Return(nil)
	td.mockLive.EXPECT().IsAlive(2222).Return(true)
	// No EXPECT for PID 3333 — short-circuit must skip it.

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err == nil {
		t.Fatalf("CleanupOrphans returned nil; want ErrConcurrentInstance wrap")
	}
	if !errors.Is(err, powerassert.ErrConcurrentInstance) {
		t.Errorf("errors.Is(err, ErrConcurrentInstance) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "2222") {
		t.Errorf("error message missing PID 2222: %q", err.Error())
	}
	// Sanity: first orphan WAS released before bail.
	if !strings.Contains(td.logBuf.String(), "released orphan assertion") {
		t.Errorf("log missing 'released orphan assertion' for first dead orphan:\n%s", td.logBuf.String())
	}
}

// TestCleanupOrphans_ReleaseFails_WarnAndContinue verifies:
// when Release returns an error on a dead orphan, log.Warn fires and the
// loop CONTINUES to the next orphan. Final return is nil — the overall
// PreFlight step is not failed by a transient IOKit release error.
func TestCleanupOrphans_ReleaseFails_WarnAndContinue(t *testing.T) {
	td := newTestDeps(t)

	orphans := []powerassert.Orphan{
		{PID: 1111, ID: 0xa},
		{PID: 2222, ID: 0xb},
	}

	td.mockEnum.EXPECT().Enumerate(gomock.Any(), gomock.Any(), gomock.Any()).Return(orphans, nil)
	td.mockLive.EXPECT().IsAlive(1111).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xa)).Return(errors.New("simulated rc=0xdeadbeef"))
	td.mockLive.EXPECT().IsAlive(2222).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xb)).Return(nil)

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err != nil {
		t.Fatalf("CleanupOrphans returned err=%v; says overall return must be nil on release failure", err)
	}

	logStr := td.logBuf.String()
	if !strings.Contains(logStr, "release orphan failed") {
		t.Errorf("log missing 'release orphan failed' warn line:\n%s", logStr)
	}
	if !strings.Contains(logStr, "level=WARN") {
		t.Errorf("log missing 'level=WARN' on release failure:\n%s", logStr)
	}
	// Second orphan still released after warn.
	if !strings.Contains(logStr, "released orphan assertion") {
		t.Errorf("log missing 'released orphan assertion' for second orphan:\n%s", logStr)
	}
}

// TestCleanupOrphans_EnumerateError_ReturnsWrappedError verifies that an
// enumerate error is wrapped with the "enumerate assertions:" prefix and
// neither IsAlive nor Release is ever called.
func TestCleanupOrphans_EnumerateError_ReturnsWrappedError(t *testing.T) {
	td := newTestDeps(t)

	underlying := errors.New("IOPMCopyAssertionsByProcess: rc=0x123")
	td.mockEnum.EXPECT().
		Enumerate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, underlying)
	// rel + live NOT EXPECT — never reached.

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err == nil {
		t.Fatalf("CleanupOrphans returned nil; want wrapped enumerate error")
	}
	if !strings.Contains(err.Error(), "enumerate assertions") {
		t.Errorf("error missing 'enumerate assertions' prefix: %q", err.Error())
	}
	if !errors.Is(err, underlying) {
		t.Errorf("errors.Is(err, underlying) = false; err = %v", err)
	}
	// Sanity: the underlying message is still grep-able.
	if !strings.Contains(err.Error(), "IOPMCopyAssertionsByProcess") {
		t.Errorf("error missing underlying IOReturn text: %q", err.Error())
	}
}

// TestCleanupOrphans_NilLog_FallsBackToDefault verifies the nil-logger
// fallback (mirrors NewController + state.NewRestoreState convention).
// Passing nil must not panic; CleanupOrphans falls back to slog.Default().
func TestCleanupOrphans_NilLog_FallsBackToDefault(t *testing.T) {
	td := newTestDeps(t)

	td.mockEnum.EXPECT().
		Enumerate(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("CleanupOrphans with nil log panicked: %v", r)
		}
	}()

	if err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, nil); err != nil {
		t.Errorf("CleanupOrphans(nil log): %v, want nil", err)
	}
}

// TestCleanupOrphans_AllDead_AllReleased verifies the multi-orphan happy
// path: N dead orphans → N Release calls → N "released orphan assertion"
// log lines → nil return. Reinforces path symmetry with the
// single-orphan test above.
func TestCleanupOrphans_AllDead_AllReleased(t *testing.T) {
	td := newTestDeps(t)

	orphans := []powerassert.Orphan{
		{PID: 1, ID: 0xa1},
		{PID: 2, ID: 0xa2},
		{PID: 3, ID: 0xa3},
	}

	td.mockEnum.EXPECT().Enumerate(gomock.Any(), gomock.Any(), gomock.Any()).Return(orphans, nil)
	td.mockLive.EXPECT().IsAlive(1).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xa1)).Return(nil)
	td.mockLive.EXPECT().IsAlive(2).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xa2)).Return(nil)
	td.mockLive.EXPECT().IsAlive(3).Return(false)
	td.mockRel.EXPECT().Release(uint32(0xa3)).Return(nil)

	err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v, want nil", err)
	}
	if got := strings.Count(td.logBuf.String(), "released orphan assertion"); got != 3 {
		t.Errorf("released-orphan log lines = %d, want 3:\n%s", got, td.logBuf.String())
	}
}

// TestCleanupOrphans_EnumerateOwnPIDExcluded verifies the contract: the
// PID passed to AssertionEnumerator.Enumerate is os.Getpid() — ensures
// our own freshly-acquired assertion is excluded from the orphan search
// on subsequent CleanupOrphans calls within the same process lifetime.
//
// This is enforced by the gomock matcher os.Getpid() in earlier tests
// (Enumerate("dndmode active", "PreventUserIdleSystemSleep", os.Getpid())),
// but isolating the contract here makes the regression test explicit.
func TestCleanupOrphans_EnumerateOwnPIDExcluded(t *testing.T) {
	td := newTestDeps(t)

	td.mockEnum.EXPECT().
		Enumerate("dndmode active", "PreventUserIdleSystemSleep", os.Getpid()).
		Return(nil, nil)

	if err := powerassert.CleanupOrphans(td.mockEnum, td.mockRel, td.mockLive, td.log); err != nil {
		t.Fatalf("CleanupOrphans: %v, want nil", err)
	}
	// gomock would have failed the test if Enumerate was called with a
	// PID other than os.Getpid().
}

// TestKernLiveChecker_OwnPID_ReturnsAlive exercises the production
// kernLiveChecker against the test process itself — `syscall.Kill(getpid(), 0)`
// always returns nil for the calling process, so IsAlive(os.Getpid())
// must return true.
func TestKernLiveChecker_OwnPID_ReturnsAlive(t *testing.T) {
	t.Parallel()
	lc := powerassert.NewKernLiveChecker()
	if !lc.IsAlive(os.Getpid()) {
		t.Errorf("IsAlive(os.Getpid()=%d) = false; want true (we are alive)", os.Getpid())
	}
}

// TestKernLiveChecker_LaunchdPID_ReturnsAlive exercises a stable
// always-alive PID: launchd (PID 1) is the macOS init process and
// guaranteed to exist throughout the system uptime. kill(1, 0) returns
// EPERM for non-root callers — kernLiveChecker treats EPERM as alive
// (conservative), so IsAlive(1) must return true.
func TestKernLiveChecker_LaunchdPID_ReturnsAlive(t *testing.T) {
	t.Parallel()
	lc := powerassert.NewKernLiveChecker()
	if !lc.IsAlive(1) {
		t.Errorf("IsAlive(1=launchd) = false; want true (launchd is always alive; EPERM = conservative alive per)")
	}
}

// TestKernLiveChecker_PIDZero_ReturnsAlive exercises the PID-0 edge case.
// On macOS, kill(0, 0) does not return ESRCH — it returns EPERM or nil
// depending on the calling context (PID 0 is the kernel idle task; not
// signal-deliverable). kernLiveChecker maps any non-ESRCH result to
// alive (conservative — never release on uncertain). Documents the
// conservative branch.
func TestKernLiveChecker_PIDZero_ReturnsAlive(t *testing.T) {
	t.Parallel()
	lc := powerassert.NewKernLiveChecker()
	if !lc.IsAlive(0) {
		t.Errorf("IsAlive(0) = false; want true (PID 0 = kernel; non-ESRCH errno → conservative alive)")
	}
}
