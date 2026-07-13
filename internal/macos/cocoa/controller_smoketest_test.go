//go:build darwin && manual

package cocoa_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/dsbasko/dndmode/internal/runtimepin" // pins main goroutine

	"github.com/dsbasko/dndmode/internal/macos/cocoa"
)

// TestSmoke_Controller_FullPath validates end-to-end:
//   - cocoa.Init succeeds
//   - controller.CreateWindowsForAllScreens creates exactly EnumerateScreensCount windows
//   - controller.WindowCount matches
//   - first Release closes all + returns nil
// - second Release is a no-op (idempotency)
//   - WindowCount == 0 after Release
//
// HEADLESS=1 → t.Skip (smoke requires GUI session).
func TestSmoke_Controller_FullPath(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := cocoa.Init(log); err != nil {
		t.Fatalf("cocoa.Init: %v", err)
	}

	n := cocoa.EnumerateScreensCount()
	if n == 0 {
		t.Skip("no displays attached")
	}

	c := cocoa.NewController("black", 0, "", log)
	if err := c.CreateWindowsForAllScreens(); err != nil {
		// On a no-display CI machine we'd skip above; if we reach here
		// with err == ErrNoDisplays it's likely a race vs hot-plug — fail
		// loudly so dev can investigate.
		if errors.Is(err, cocoa.ErrNoDisplays) {
			t.Fatalf("CreateWindowsForAllScreens returned ErrNoDisplays despite EnumerateScreensCount=%d", n)
		}
		t.Fatalf("CreateWindowsForAllScreens: %v", err)
	}
	if got := c.WindowCount(); got != n {
		t.Errorf("WindowCount = %d, want %d", got, n)
	}

	// Brief visual verification window on dev machine; CI proceeds without lingering.
	time.Sleep(100 * time.Millisecond)

	if err := c.Release(); err != nil {
		t.Errorf("first Release: %v", err)
	}
	if err := c.Release(); err != nil {
		t.Errorf("second Release (must be no-op): %v", err)
	}
	if got := c.WindowCount(); got != 0 {
		t.Errorf("WindowCount after Release = %d, want 0", got)
	}
}

// TestSmoke_RunApp_CtxCancel_Returns validates:
//   - cocoa.RunApp(ctx) blocks
//   - ctx.Cancel() triggers synthetic NSEvent → [NSApp run] returns
//   - RunApp returns nil (ctx-driven exit)
func TestSmoke_RunApp_CtxCancel_Returns(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := cocoa.Init(log); err != nil {
		t.Fatalf("cocoa.Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	err := cocoa.RunApp(ctx)
	if err != nil {
		t.Errorf("RunApp returned err on ctx-cancel: %v", err)
	}
}

// TestSmoke_RunApp_DoubleInvocation_Panics validates the single-shot guard.
// Calling RunApp twice CONCURRENTLY must panic immediately on the second
// caller (per contract). We start the first via goroutine, give it a
// moment to acquire the runMu lock, then invoke RunApp on the test
// goroutine and expect panic.
//
// Note: we deliberately DON'T cancel the first ctx — both will be torn
// down via t.Cleanup. The first RunApp blocks on [NSApp run]; we'll
// terminate it manually.
func TestSmoke_RunApp_DoubleInvocation_Panics(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("smoke test requires GUI session; HEADLESS=1")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := cocoa.Init(log); err != nil {
		t.Fatalf("cocoa.Init: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := make(chan error, 1)
	go func() {
		first <- cocoa.RunApp(ctx1)
	}()
	// Give RunApp #1 a moment to take the runMu lock.
	time.Sleep(100 * time.Millisecond)

	defer func() {
		// After we cancel ctx1, RunApp #1 will return; drain its result.
		cancel1()
		select {
		case <-first:
		case <-time.After(2 * time.Second):
			t.Errorf("RunApp #1 did not return after ctx cancel")
		}

		r := recover()
		if r == nil {
			t.Errorf("expected panic from RunApp double-invocation, got none")
		}
	}()

	// Second RunApp must panic.
	_ = cocoa.RunApp(context.Background())
}
