//go:build darwin

// Package supervisor implements the dndmode shutdown fan-in. A single
// goroutine select's on three trigger sources — POSIX signals (SIGINT,
// SIGTERM, SIGHUP), the exitTrigger channel (used by matcher in Phase 4
// and by panicking goroutines via runWithRecover), and ctx.Done() — and
// calls Stopper.RequestStop exactly once (sync.Once).
package supervisor

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
)

//go:generate mockgen -source=supervisor.go -destination=mocks/stopper.go -package=mocks

// Stopper abstracts the side effect of "stop the application." Phase 1
// implementation triggers ctx cancel + restoreState.Cleanup; Phase 4 will
// add CGEventTap.Disable + NSApp.Stop + postEvent:atStart:YES.
type Stopper interface {
	RequestStop(reason string)
}

// Supervisor is the dndmode lifecycle fan-in. New supervisors are not
// started until Start(ctx) is called.
type Supervisor struct {
	log         *slog.Logger
	stopper     Stopper
	exitTrigger chan struct{}
	wg          sync.WaitGroup
	once        sync.Once
}

// New constructs a Supervisor with the given logger and Stopper. log MUST
// be non-nil (use slog.New(slog.NewTextHandler(os.Stderr,...)) per).
func New(log *slog.Logger, stopper Stopper) *Supervisor {
	return &Supervisor{
		log:         log,
		stopper:     stopper,
		exitTrigger: make(chan struct{}, 1), // cap=1
	}
}

// ExitTrigger returns the channel that matcher (Phase 4) and other
// shutdown triggers send on to request stop. Use a non-blocking send
// (cap=1; second send is a no-op since first hasn't been drained yet —
// semantically equivalent to "stop already requested, second request
// is collapsed").
func (s *Supervisor) ExitTrigger() chan<- struct{} { return s.exitTrigger }

// Start launches the supervisor goroutine. Returns immediately.
// Use Wait to block until the goroutine exits.
func (s *Supervisor) Start(ctx context.Context) {
	s.wg.Add(1)
	go runWithRecover("supervisor", s.log, &s.wg, func() {
		s.run(ctx)
	})
}

// Wait blocks until the supervisor goroutine has exited (i.e., until a
// trigger has fired AND RequestStop has been called).
func (s *Supervisor) Wait() { s.wg.Wait() }

// run is the supervisor goroutine body. It select's on three sources and
// calls fireStop with a reason describing which fired.
func (s *Supervisor) run(ctx context.Context) {
	sigCh := make(chan os.Signal, 1) // cap=1; P1.2 — never unbuffered
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh) // P1.4: prevent leak; second SIGINT after this
	// goes to default Go handler (fast exit).

	select {
	case sig := <-sigCh:
		s.fireStop("signal:" + sig.String())
	case <-s.exitTrigger:
		s.fireStop("exit-trigger")
	case <-ctx.Done():
		s.fireStop("ctx-canceled:" + ctx.Err().Error())
	}
}

// fireStop calls Stopper.RequestStop exactly once (sync.Once). Repeated
// triggers (e.g., second SIGINT during shutdown) are absorbed silently.
func (s *Supervisor) fireStop(reason string) {
	s.once.Do(func() {
		s.log.Info("requesting stop", slog.String("reason", reason))
		s.stopper.RequestStop(reason)
	})
}

// runWithRecover wraps a goroutine body with panic recovery. On
// panic, the stack is logged via slog and the goroutine exits gracefully —
// it does NOT propagate the panic. Phase 4 will extend this to actively
// send to exitTrigger on panic; Phase 1 logs only.
//
// Always called with a *sync.WaitGroup the caller has already Add(1)'d.
// The wg.Done() is deferred FIRST so it runs even if recover() saves us.
func runWithRecover(name string, log *slog.Logger, wg *sync.WaitGroup, fn func()) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Error("goroutine panic recovered",
				slog.String("goroutine", name),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	fn()
}
