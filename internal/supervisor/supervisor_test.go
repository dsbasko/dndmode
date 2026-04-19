//go:build darwin

package supervisor_test

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/supervisor"
	"github.com/dsbasko/dndmode/internal/supervisor/mocks"
	"go.uber.org/mock/gomock"
)

type testDeps struct {
	logBuffer   *bytes.Buffer
	logger      *slog.Logger
	mockStopper *mocks.MockStopper
	supervisor  *supervisor.Supervisor
	ctrl        *gomock.Controller
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	ctrl := gomock.NewController(t)
	stopper := mocks.NewMockStopper(ctrl)
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &testDeps{
		logBuffer:   buf,
		logger:      logger,
		mockStopper: stopper,
		supervisor:  supervisor.New(logger, stopper),
		ctrl:        ctrl,
	}
}

func TestSupervisor_Run_AllTriggers(t *testing.T) {
	tests := []struct {
		name         string
		setupMocks   func(td *testDeps)
		trigger      func(td *testDeps, cancel context.CancelFunc)
		validateResp func(t *testing.T, td *testDeps)
	}{
		{
			name: "ctx cancel triggers RequestStop exactly once",
			setupMocks: func(td *testDeps) {
				td.mockStopper.EXPECT().
					RequestStop(gomock.Any()).
					Times(1)
			},
			trigger: func(_ *testDeps, cancel context.CancelFunc) {
				cancel()
			},
			validateResp: func(_ *testing.T, _ *testDeps) {},
		},
		{
			name: "exitTrigger send triggers RequestStop exactly once",
			setupMocks: func(td *testDeps) {
				td.mockStopper.EXPECT().
					RequestStop(gomock.Any()).
					Times(1)
			},
			trigger: func(td *testDeps, _ context.CancelFunc) {
				td.supervisor.ExitTrigger() <- struct{}{}
			},
			validateResp: func(_ *testing.T, _ *testDeps) {},
		},
		{
			name: "SIGTERM triggers RequestStop exactly once",
			setupMocks: func(td *testDeps) {
				td.mockStopper.EXPECT().
					RequestStop(gomock.Any()).
					Times(1)
			},
			trigger: func(_ *testDeps, _ context.CancelFunc) {
				// Self-deliver SIGTERM. The supervisor's signal.Notify
				// must be set up before this fires — Start() does it
				// synchronously inside the goroutine, but since we call
				// trigger() AFTER a small wait below, the race is benign.
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
			},
			validateResp: func(_ *testing.T, _ *testDeps) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			tt.setupMocks(td)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			td.supervisor.Start(ctx)

			// Give the goroutine a moment to enter select + signal.Notify.
			// In practice <1ms; 50ms is generous and not flaky.
			time.Sleep(50 * time.Millisecond)

			tt.trigger(td, cancel)

			done := make(chan struct{})
			go func() {
				td.supervisor.Wait()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("supervisor did not exit within 2s after trigger")
			}

			tt.validateResp(t, td)
		})
	}
}

func TestSupervisor_Run_DoubleTriggerCallsRequestStopOnce(t *testing.T) {
	td := newTestDeps(t)
	td.mockStopper.EXPECT().
		RequestStop(gomock.Any()).
		Times(1) // sync.Once enforces exactly-once even under double trigger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	td.supervisor.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	cancel()

	// Try to send a second trigger after ctx-cancel. The select has
	// already taken the ctx.Done() branch; this send goes into the
	// buffered exitTrigger and is never read (cap=1). sync.Once on
	// fireStop ensures RequestStop is not called twice.
	go func() {
		select {
		case td.supervisor.ExitTrigger() <- struct{}{}:
		default:
		}
	}()

	done := make(chan struct{})
	go func() {
		td.supervisor.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit within 2s")
	}
	// gomock auto-verifies Times(1) on Controller.Finish via t.Cleanup.
}

func TestSupervisor_ChannelCaps(t *testing.T) {
	// exitTrigger cap=1.
	td := newTestDeps(t)
	ch := td.supervisor.ExitTrigger()

	// reflect.ValueOf(ch).Cap() returns the channel capacity; works on
	// chan<- direction too.
	v := reflect.ValueOf(ch)
	if v.Kind() != reflect.Chan {
		t.Fatalf("ExitTrigger() kind = %s, want Chan", v.Kind())
	}
	if got := v.Cap(); got != 1 {
		t.Errorf("exitTrigger cap = %d, want 1", got)
	}
}

func TestRunWithRecover_PanicLoggedNotPropagated(t *testing.T) {
	// panic in non-main goroutine wrapped by runWithRecover is
	// logged but does NOT propagate.
	//
	// We test runWithRecover indirectly via Supervisor.Start — Phase 1
	// supervisor.run() doesn't panic on its own, so we use a Stopper
	// implementation that panics on RequestStop. The panic should be
	// caught by runWithRecover (which wraps Start's goroutine),
	// logged with "goroutine panic recovered", and Wait() should return.
	td := newTestDeps(t)

	// Stopper.RequestStop panics deliberately.
	td.mockStopper.EXPECT().
		RequestStop(gomock.Any()).
		Do(func(_ string) {
			panic("test-injected panic in stopper")
		}).
		Times(1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	td.supervisor.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel() // triggers ctx.Done() → fireStop → RequestStop → panic

	done := make(chan struct{})
	go func() {
		td.supervisor.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after panic — runWithRecover failed")
	}

	// Verify the panic was logged with the expected fields.
	logs := td.logBuffer.String()
	if !bytes.Contains(td.logBuffer.Bytes(), []byte("goroutine panic recovered")) {
		t.Errorf("log buffer missing 'goroutine panic recovered': %s", logs)
	}
	if !bytes.Contains(td.logBuffer.Bytes(), []byte("supervisor")) {
		t.Errorf("log buffer missing goroutine name 'supervisor': %s", logs)
	}
	if !bytes.Contains(td.logBuffer.Bytes(), []byte("test-injected panic in stopper")) {
		t.Errorf("log buffer missing panic value: %s", logs)
	}
}

func TestSupervisor_New_ExitTriggerHasCapOne(t *testing.T) {
	// Sanity: invariant — make(chan struct{}, 1) is the only cap
	// New() should ever produce. Catches accidental refactor that breaks
	// cap.
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	stopper := mocks.NewMockStopper(ctrl)
	sup := supervisor.New(logger, stopper)

	// Use reflection to peek into exit trigger capacity.
	ch := sup.ExitTrigger()
	if got := reflect.ValueOf(ch).Cap(); got != 1 {
		t.Errorf("New().ExitTrigger() cap = %d, want 1", got)
	}
}
