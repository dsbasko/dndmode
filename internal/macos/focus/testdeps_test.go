//go:build darwin

// Shared test helper for `package focus_test`. Declared once here so all
// downstream Phase 5 *_test.go files in the same _test package
// (shortcuts_test.go, checker_test.go,
// focus_test.go, releaser_test.go) share the same
// `testDeps` / `newTestDeps` symbols — Go forbids redeclaring a type
// across multiple files in the same _test package, so this is the
// single declaration site.
//
// `fakeRunner` is a hand-written ShortcutsRunner stand-in used by tests
// where strict gomock EXPECT counters complicate the assertion (e.g. the
// concurrent-callers serialization test in 's releaser_test.go,
// which mirrors the powerassert/assertion_test.go fakeReleaser pattern).
// and use mockRunner exclusively; uses both.

package focus_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/dsbasko/dndmode/internal/macos/focus/mocks"
)

// testDeps groups the gomock controller + MockShortcutsRunner + a captured
// slog buffer for any test in `package focus_test`.
type testDeps struct {
	ctrl       *gomock.Controller
	mockRunner *mocks.MockShortcutsRunner
	logBuf     *bytes.Buffer
	log        *slog.Logger
}

// newTestDeps constructs the shared test dependencies for a single
// test case. gomock.NewController(t) installs a t.Cleanup hook that
// asserts all configured expectations were met, so callers do not
// need to call ctrl.Finish() manually.
func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &testDeps{
		ctrl:       ctrl,
		mockRunner: mocks.NewMockShortcutsRunner(ctrl),
		logBuf:     buf,
		log:        log,
	}
}

// fakeRunner is a hand-written ShortcutsRunner stand-in for the concurrent
// serialization test in 's releaser_test.go. It delegates List
// and Run to the function fields, which lets the test plug in atomic
// counters / sleep / error injection without the strict EXPECT semantics
// of gomock. nil func fields are treated as "no-op success" (nil err,
// nil names) so callers only override the field they care about.
type fakeRunner struct {
	listFn func(context.Context) ([]string, error)
	runFn  func(context.Context, string) error
}

// List implements focus.ShortcutsRunner. Honors ctx-cancellation
// before invoking the closure — production exec.CommandContext
// behaves the same way: a pre-cancelled ctx makes List return ctx.Err()
// without spawning a subprocess.
func (f *fakeRunner) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.listFn == nil {
		return nil, nil
	}
	return f.listFn(ctx)
}

// Run implements focus.ShortcutsRunner. Honors ctx-cancellation
// before invoking the closure — production exec.CommandContext
// SIGKILLs a subprocess spawned with a pre-cancelled ctx, so the fake
// must mirror that contract for tests that exercise cancellation paths.
func (f *fakeRunner) Run(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.runFn == nil {
		return nil
	}
	return f.runFn(ctx, name)
}
