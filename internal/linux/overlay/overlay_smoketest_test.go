//go:build linux && manual

package overlay

import (
	"runtime"
	"testing"
	"time"
)

// TestSmoke_Controller_ShowRelease puts a real shield of every style on
// every monitor for two seconds each. It needs a desktop session, so it sits
// behind the manual build tag like the macOS GUI smoke tests:
//
//	go test -tags=manual -run Smoke ./internal/linux/overlay
//
// The styles run in a loop rather than as subtests: t.Run starts a new
// goroutine, and every GTK call must stay on the thread that called Init.
func TestSmoke_Controller_ShowRelease(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := Init(nil); err != nil {
		t.Fatal(err)
	}
	for _, style := range Styles {
		c := NewController(style, 0, "ys", "", nil)
		if err := c.CreateWindowsForAllScreens(); err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		if got, want := c.WindowCount(), monitorCount(); got != want {
			t.Errorf("%s: windows = %d, monitors = %d", style, got, want)
		}
		pumpFor(2 * time.Second)

		if err := c.Release(); err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		if err := c.Release(); err != nil {
			t.Errorf("%s: second Release: %v", style, err)
		}
		if c.WindowCount() != 0 {
			t.Errorf("%s: windows after Release = %d", style, c.WindowCount())
		}
		pumpFor(200 * time.Millisecond)
	}
}

// pumpFor services the GTK main context for d.
func pumpFor(d time.Duration) {
	done := make(chan struct{})
	time.AfterFunc(d, func() { close(done) })
	Pump(done)
}
