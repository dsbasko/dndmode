//go:build linux && manual

package overlay

import (
	"runtime"
	"testing"
	"time"
)

// TestSmoke_Controller_ShowRelease puts a real shield on every monitor for
// two seconds. It needs a desktop session, so it sits behind the manual
// build tag like the macOS GUI smoke tests:
//
//	go test -tags=manual -run Smoke ./internal/linux/overlay
func TestSmoke_Controller_ShowRelease(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := Init(nil); err != nil {
		t.Fatal(err)
	}
	c := NewController("", nil)
	if err := c.CreateWindowsForAllScreens(); err != nil {
		t.Fatal(err)
	}
	if got, want := c.WindowCount(), monitorCount(); got != want {
		t.Errorf("windows = %d, monitors = %d", got, want)
	}
	done := make(chan struct{})
	time.AfterFunc(2*time.Second, func() { close(done) })
	Pump(done)

	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	if c.WindowCount() != 0 {
		t.Errorf("windows after Release = %d", c.WindowCount())
	}
	settle := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(settle) })
	Pump(settle)
}
