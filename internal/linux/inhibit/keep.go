//go:build linux

package inhibit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	// keepSettle is how long Keep waits for a chain that dies at once
	// (polkit denial, no logind session) before calling it held.
	keepSettle = 200 * time.Millisecond
	// keepRetryMin and keepRetryMax bound the delay between restarts of a
	// chain that died during the session.
	keepRetryMin = time.Second
	keepRetryMax = time.Minute
)

// Keeper holds the awake-lock for a whole session. A chain can die on its
// own long after Start returned, and nothing else would notice: with input
// grabbed GNOME counts the session as idle and may blank, lock or suspend
// while dndmode still reports active. Keeper restarts it until Release. It
// satisfies state.Releaser.
type Keeper struct {
	log   *slog.Logger
	start func() (*Process, error)

	mu       sync.Mutex
	cur      *Process
	released bool
	stop     chan struct{}
	done     chan struct{}
}

// Keep starts the chain for pid and keeps it running until Release or until
// ctx is cancelled. A chain that exits within keepSettle is an error: the
// lock was never held, the case macOS reports as a failed assertion.
func Keep(ctx context.Context, pid int, allowDisplaySleep bool, log *slog.Logger) (*Keeper, error) {
	if log == nil {
		log = slog.Default()
	}
	argv, err := buildArgv(pid, allowDisplaySleep)
	if err != nil {
		return nil, err
	}
	return keep(ctx, func() (*Process, error) { return launch(ctx, argv, log) }, log)
}

// keep is Keep with the chain launcher injected; tests pass fake chains.
func keep(ctx context.Context, start func() (*Process, error), log *slog.Logger) (*Keeper, error) {
	p, err := start()
	if err != nil {
		return nil, err
	}
	select {
	case <-p.Done():
		return nil, fmt.Errorf("inhibitor exited at once: %w", p.Err())
	case <-time.After(keepSettle):
	}
	k := &Keeper{log: log, start: start, cur: p, stop: make(chan struct{}), done: make(chan struct{})}
	go k.watch(ctx)
	return k, nil
}

// watch restarts the chain each time it dies, backing off between attempts.
func (k *Keeper) watch(ctx context.Context) {
	defer close(k.done)
	delay := keepRetryMin
	for {
		k.mu.Lock()
		p := k.cur
		k.mu.Unlock()
		if p != nil {
			select {
			case <-ctx.Done():
				return
			case <-k.stop:
				return
			case <-p.Done():
			}
			if ctx.Err() != nil {
				return
			}
			k.log.Warn("inhibitor exited during the session; restarting it", slog.Any("err", p.Err()))
		}
		select {
		case <-ctx.Done():
			return
		case <-k.stop:
			return
		case <-time.After(delay):
		}
		np, err := k.start()
		if err == nil {
			// A chain that starts and dies at once is the usual failure
			// (logind gone, polkit denial); it must back off like an exec
			// error, or it is relaunched every keepRetryMin all session.
			select {
			case <-np.Done():
				err = fmt.Errorf("inhibitor exited at once: %w", np.Err())
			case <-time.After(keepSettle):
			case <-ctx.Done():
			case <-k.stop:
			}
		}
		k.mu.Lock()
		switch {
		case k.released:
			k.mu.Unlock()
			if np != nil {
				_ = np.Release()
			}
			return
		case err != nil:
			k.cur = nil
			k.mu.Unlock()
			k.log.Warn("restart inhibitor failed", slog.Any("err", err), slog.Duration("retry_in", delay))
			delay = min(delay*2, keepRetryMax)
			continue
		}
		k.cur = np
		k.mu.Unlock()
		delay = keepRetryMin
	}
}

// Name implements state.Releaser.
func (k *Keeper) Name() string { return "inhibit" }

// Release stops restarting and stops the current chain. Idempotent.
func (k *Keeper) Release() error {
	k.mu.Lock()
	if k.released {
		k.mu.Unlock()
		return nil
	}
	k.released = true
	close(k.stop)
	p := k.cur
	k.mu.Unlock()
	<-k.done
	if p != nil {
		return p.Release()
	}
	return nil
}
