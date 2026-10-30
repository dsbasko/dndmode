//go:build linux

package watch

import (
	"testing"
	"time"
)

func TestStartInstant(t *testing.T) {
	boot := []byte("8b50ff9a-4f00-42e3-9929-6bf82b1c0b95")
	a := startInstant(boot, 12345)
	if !a.Equal(startInstant(boot, 12345)) {
		t.Error("same boot and ticks give different instants")
	}
	if got := startInstant(boot, 12445).Sub(a); got != time.Second {
		t.Errorf("100 ticks later = %v, want 1s", got)
	}
	if a.Equal(startInstant([]byte("0f3a2c1e-0000-4000-8000-000000000000"), 12345)) {
		t.Error("another boot gives the same instant")
	}
}
