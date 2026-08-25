//go:build darwin

package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ErrNoProcess reports that no process with the given PID exists. It is the
// one answer Prober.StartTime gives that Inspect acts on without hesitation:
// a record naming a PID that is gone is stale, whatever else is in it.
var ErrNoProcess = errors.New("no such process")

// Prober answers "when did this PID start?". The kernel-backed implementation
// is NewKernProber; tests inject a fake.
type Prober interface {
	// StartTime returns the kernel start time of pid, ErrNoProcess when there
	// is no such process, and any other error when the answer could not be
	// established (the caller must then treat the record as inconclusive
	// rather than as stale or live).
	StartTime(pid int) (time.Time, error)
}

type kernProber struct{}

// NewKernProber returns the production Prober: kill(pid, 0) to tell a dead
// PID from a live one, then sysctl kern.proc.pid for the start time.
func NewKernProber() Prober { return kernProber{} }

func (kernProber) StartTime(pid int) (time.Time, error) {
	// PID 0 means "my process group" to kill(2) and negative PIDs are
	// broadcasts; neither is something a watch process can have written.
	if pid <= 0 {
		return time.Time{}, fmt.Errorf("%w: invalid pid %d", ErrNoProcess, pid)
	}
	// ESRCH is the only definite "gone". EPERM means the PID exists but
	// belongs to another user — still a process, so fall through and let the
	// start time decide. sysctl works for any PID regardless of ownership.
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return time.Time{}, ErrNoProcess
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// x/sys returns EIO when the kernel answers with zero bytes, which is
		// what kern.proc.pid does for a PID that vanished between the kill
		// above and here. ESRCH covers kernels that say so outright.
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return time.Time{}, ErrNoProcess
		}
		return time.Time{}, fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	// A zombie still answers kill(pid, 0) and still reports its start time,
	// but it holds nothing — no hotkey, no record it could remove — and its
	// parent will reap it. For every question this package asks, it is gone.
	if kp.Proc.P_stat == procStatZombie {
		return time.Time{}, ErrNoProcess
	}
	tv := kp.Proc.P_starttime
	return time.Unix(tv.Sec, int64(tv.Usec)*int64(time.Microsecond)), nil
}

// procStatZombie is SZOMB from <sys/proc.h>, the p_stat value of a process
// that has exited and awaits its parent's wait(2). x/sys/unix does not export
// the constant.
const procStatZombie = 5

// Signaller delivers a signal to a PID. The kernel-backed implementation is
// NewKernSignaller; tests inject a fake.
type Signaller interface {
	Signal(pid int, sig syscall.Signal) error
}

type kernSignaller struct{}

// NewKernSignaller returns the production Signaller wrapping kill(2).
func NewKernSignaller() Signaller { return kernSignaller{} }

func (kernSignaller) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// Status is what Inspect learned from the record. Exactly one of Running and
// Stale is true when a record exists; both are false when there is none.
type Status struct {
	// Record is the record as read. Meaningful only when Running or Stale.
	Record Record
	// Running: a process matching the record — same PID, and the same kernel
	// start time when the record carries one — is alive.
	Running bool
	// Stale: a record exists but names a process that is gone, or a PID that
	// now belongs to a process started at a different time.
	Stale bool
}

// Inspect reads the record and decides whether the process it names is still
// the one that wrote it. It never modifies anything on disk.
//
// It returns an error only when the answer could not be established: an
// unreadable or malformed record, or a Prober that could neither confirm nor
// deny the PID. Callers on the control path fail closed on that (a `--kill`
// must not signal a PID it cannot vouch for); a starting `--watch` may warn
// and continue, because the Carbon registration that follows refuses on its
// own if a real watch process is holding the combination.
func Inspect(mgr *Manager, prober Prober) (Status, error) {
	rec, err := mgr.Read()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, nil
		}
		return Status{}, err
	}
	if rec.PID <= 0 {
		// A record with no usable PID cannot describe a live process, and
		// probing 0 or a negative number would ask the kernel about a process
		// group instead. Stale, not inconclusive: there is nothing to vouch for.
		return Status{Record: rec, Stale: true}, nil
	}
	start, err := prober.StartTime(rec.PID)
	if err != nil {
		if errors.Is(err, ErrNoProcess) {
			return Status{Record: rec, Stale: true}, nil
		}
		return Status{}, fmt.Errorf("probe pid %d: %w", rec.PID, err)
	}
	if !rec.ProcStartedAt.IsZero() && !start.Equal(rec.ProcStartedAt) {
		return Status{Record: rec, Stale: true}, nil
	}
	return Status{Record: rec, Running: true}, nil
}

// Outcome is what Stop did.
type Outcome int

const (
	// NotRunning: there was no record, so there was nothing to stop.
	NotRunning Outcome = iota
	// StaleRemoved: the record named a process that was already gone (or a
	// reused PID); the record was removed and nothing was signalled.
	StaleRemoved
	// Stopped: the process was signalled and has exited.
	Stopped
)

// ErrStopTimeout reports that the process was signalled but was still alive
// when the wait ran out. The record is left in place: the process still owns
// it and may yet remove it on its way out.
var ErrStopTimeout = errors.New("watch process did not exit in time")

// ErrSignalFailed wraps a kill(2) failure — EPERM, most likely, for a record
// written by another user's dndmode.
var ErrSignalFailed = errors.New("signal failed")

// StopOptions tunes Stop. Zero values pick production defaults.
type StopOptions struct {
	// Signal to deliver. Zero → SIGTERM, which the watch process handles
	// exactly like Ctrl-C used to: sessions unwind, the record is removed.
	Signal syscall.Signal
	// Wait bounds how long Stop polls for the process to disappear. Zero →
	// DefaultStopWait.
	Wait time.Duration
	// Poll is the cadence of that polling. Zero → DefaultStopPoll.
	Poll time.Duration
}

// DefaultStopWait is sized for a watch process caught with a shield up: its
// session teardown runs two best-effort subprocesses (Focus off, audio
// unmute) of up to five seconds each before the process can exit.
const DefaultStopWait = 15 * time.Second

// DefaultStopPoll is the cadence Stop re-probes the PID at while waiting.
const DefaultStopPoll = 50 * time.Millisecond

// Result is what Stop returns alongside its error.
type Result struct {
	Outcome Outcome
	// Record is the record that was acted on. Zero when Outcome is NotRunning.
	Record Record
}

// Stop makes "not running" true: it signals the process the record names and
// waits for it to go away, or removes a record that no longer names a live
// one. A missing record is success — Stop is meant to be idempotent, so a
// script can run it unconditionally before a `--watch`.
//
// The record is removed by the process itself on a clean exit. Stop removes it
// afterwards only if it STILL names the PID that just exited: a `--watch`
// started in the same instant could already have written its own record at
// that path, and deleting it would leave a live watch process invisible to
// every probe.
//
// Errors: ErrStopTimeout (process still alive after the wait; record left in
// place), ErrSignalFailed (kill refused), or a wrapped read/probe failure from
// Inspect, on which nothing was signalled.
func Stop(mgr *Manager, prober Prober, sig Signaller, opts StopOptions) (Result, error) {
	if opts.Signal == 0 {
		opts.Signal = syscall.SIGTERM
	}
	if opts.Wait <= 0 {
		opts.Wait = DefaultStopWait
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultStopPoll
	}

	st, err := Inspect(mgr, prober)
	if err != nil {
		return Result{}, err
	}
	switch {
	case st.Stale:
		if err := mgr.Release(); err != nil {
			return Result{Outcome: StaleRemoved, Record: st.Record}, err
		}
		return Result{Outcome: StaleRemoved, Record: st.Record}, nil
	case !st.Running:
		return Result{Outcome: NotRunning}, nil
	}

	rec := st.Record
	if err := sig.Signal(rec.PID, opts.Signal); err != nil {
		return Result{Record: rec}, fmt.Errorf("%w: pid %d: %w", ErrSignalFailed, rec.PID, err)
	}

	deadline := time.Now().Add(opts.Wait)
	for {
		start, perr := prober.StartTime(rec.PID)
		switch {
		case errors.Is(perr, ErrNoProcess):
			return Result{Outcome: Stopped, Record: rec}, removeIfStillOurs(mgr, rec)
		case perr == nil && !rec.ProcStartedAt.IsZero() && !start.Equal(rec.ProcStartedAt):
			// The PID was reused already; the process we signalled is gone.
			return Result{Outcome: Stopped, Record: rec}, removeIfStillOurs(mgr, rec)
		}
		if !time.Now().Before(deadline) {
			return Result{Record: rec}, fmt.Errorf("%w: pid %d after %s", ErrStopTimeout, rec.PID, opts.Wait)
		}
		time.Sleep(opts.Poll)
	}
}

// removeIfStillOurs deletes the record only while it still names rec's PID.
// The process normally removes its own record, so the common result is
// fs.ErrNotExist, which counts as done.
func removeIfStillOurs(mgr *Manager, rec Record) error {
	cur, err := mgr.Read()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// Unreadable now — it was readable a moment ago, so something is
		// rewriting it. Leave it alone rather than delete what we cannot read.
		return nil
	}
	if cur.PID != rec.PID {
		return nil
	}
	return mgr.Release()
}
