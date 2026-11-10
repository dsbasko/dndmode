//go:build linux && manual

package overview

import (
	"context"
	"testing"
	"time"
)

// TestSmoke_HideClosesRealOverview opens the overview of the running GNOME
// Shell and checks that Hide closes it. Needs a GNOME session.
func TestSmoke_HideClosesRealOverview(t *testing.T) {
	ctx := context.Background()
	r := NewRunner()
	if _, err := r.run(ctx, "gdbus", "call", "--session",
		"--dest", dest, "--object-path", objectPath,
		"--method", propsIface+".Set", dest, prop, "<true>"); err != nil {
		t.Skipf("no GNOME Shell: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if open, err := r.active(ctx); err != nil || !open {
		t.Fatalf("overview did not open: open=%v err=%v", open, err)
	}
	if err := Hide(ctx, r); err != nil {
		t.Fatal(err)
	}
	if open, _ := r.active(ctx); open {
		t.Error("overview still open after Hide")
	}
}
