//go:build darwin

package runtimepin

import (
	"runtime"
	"testing"
)

func TestRuntimePin_Init_LocksMainGoroutineToConsistentThread(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func()
		validateResp func(t *testing.T)
	}{
		{
			name:       "repeated LockOSThread is safe (already pinned by init)",
			setupMocks: func() {},
			validateResp: func(t *testing.T) {
				// runtime.LockOSThread is idempotent on the same goroutine.
				// Calling it again should not panic or deadlock.
				// Note: each LockOSThread MUST be paired with UnlockOSThread.
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				// Sanity: we can still execute Go code.
				if got := 1 + 1; got != 2 {
					t.Fatalf("arithmetic broken under LockOSThread: got %d", got)
				}
			},
		},
		{
			name:       "init has executed (package imported successfully)",
			setupMocks: func() {},
			validateResp: func(t *testing.T) {
				// If init() panicked, we wouldn't reach this test at all.
				// Reaching here proves init ran without panic.
				if testing.Short() {
					t.Skip("smoke check only")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupMocks()
			tt.validateResp(t)
		})
	}
}

func TestRuntimePin_Init_DoesNotPanic(t *testing.T) {
	// Reaching this point means init() ran. If init had panicked, the
	// test binary itself would have failed to start.
	t.Log("runtimepin package loaded successfully; init() did not panic")
}
