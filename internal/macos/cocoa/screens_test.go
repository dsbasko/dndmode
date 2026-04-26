//go:build darwin

package cocoa

import (
	"sync/atomic"
	"testing"

	_ "github.com/dsbasko/dndmode/internal/runtimepin"
)

func TestSetOnScreensChanged_StoresAndClears(t *testing.T) {
	var hits atomic.Int32
	cb := func() { hits.Add(1) }
	setOnScreensChanged(&cb)

	got := activeOnScreensChanged.Load()
	if got == nil {
		t.Fatal("expected non-nil callback after set")
	}
	(*got)()
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}

	setOnScreensChanged(nil)
	if activeOnScreensChanged.Load() != nil {
		t.Error("expected nil after clear")
	}
}

func TestGoCocoaOnScreensChanged_NilCallback_NoOp(t *testing.T) {
	setOnScreensChanged(nil) // ensure clear

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("expected silent no-op on nil callback, got panic: %v", r)
		}
	}()
	goCocoaOnScreensChanged()
}

func TestGoCocoaOnScreensChanged_InvokesRegistered(t *testing.T) {
	var hits atomic.Int32
	cb := func() { hits.Add(1) }
	setOnScreensChanged(&cb)
	defer setOnScreensChanged(nil)

	goCocoaOnScreensChanged()
	goCocoaOnScreensChanged()
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}

// EnumerateScreensCount on dev machines returns >0; on headless CI may
// return 0. Test asserts non-negative count + same value across two calls.
func TestEnumerateScreensCount_NonNegative(t *testing.T) {
	n1 := EnumerateScreensCount()
	if n1 < 0 {
		t.Errorf("EnumerateScreensCount = %d, want >= 0", n1)
	}
	n2 := EnumerateScreensCount()
	if n1 != n2 {
		t.Errorf("EnumerateScreensCount inconsistent across calls: %d vs %d", n1, n2)
	}
}
