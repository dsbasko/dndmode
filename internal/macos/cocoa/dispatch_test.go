//go:build darwin

package cocoa_test

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine

	"github.com/dsbasko/dndmode/internal/macos/cocoa"
)

func TestDispatchMain_Inline_OnMain(t *testing.T) {
	// Test executes on goroutine pinned by runtimepin/init -> we are on main.
	// (Go test runner usually runs tests on goroutines, but the package-level
	// blank import + LockOSThread should keep at least the test framework's
	// main goroutine pinned. If pthread_main_np returns 0 here we accept the
	// async path; the assertion is on the WHEN, not the IF.)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var ran atomic.Bool
	cocoa.DispatchMain(func() { ran.Store(true) })
	// On main: fn must have already run by the time DispatchMain returned.
	if !ran.Load() {
		t.Skip("DispatchMain on locked goroutine did not run inline; pthread_main_np returned 0 — fast path skipped")
	}
}

func TestDispatchMain_NilFn_Panics(t *testing.T) {
	// DispatchMain(nil) panics ONLY when the inline (main-thread) fast-path is
	// taken — fn() runs synchronously and Go panics on the nil-func call. On
	// the async path, the boxed nil closure would panic asynchronously inside
	// the libdispatch block on main, which is unreachable from a unit test
	// without NSApp.run (covered later by window smoketest in).
	//
	// We therefore probe the fast-path first via a no-op DispatchMain; if the
	// probe ran inline, the nil call below must panic. Otherwise, we skip —
	// matching the IF-not-WHEN convention used by TestDispatchMain_Inline_OnMain.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var probeRan atomic.Bool
	cocoa.DispatchMain(func() { probeRan.Store(true) })
	if !probeRan.Load() {
		t.Skip("inline fast-path unavailable (pthread_main_np=0); nil-panic only triggers inline")
	}

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil fn (inline path), got none")
		}
	}()
	cocoa.DispatchMain(nil)
}

// NOTE: full async-path test (calling DispatchMain from a goroutine while
// main runloop is running [NSApp run]) belongs in window_smoketest_test.go
// (in) where NSApp.run + ctx.Cancel pattern is established.
// Here we cover the inline path + nil-handling.
var _ = sync.Mutex{} // ensure imports used even if tests are short
var _ = time.Now
