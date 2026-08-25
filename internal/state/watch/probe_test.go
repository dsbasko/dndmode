//go:build darwin

package watch_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/state/watch"
)

// fakeProber scripts StartTime answers per PID. A PID with no entry is gone.
type fakeProber struct {
	starts map[int]time.Time
	err    error // returned for every PID when non-nil (inconclusive probe)
	calls  int
	// after is consulted from the given call number onward, so a test can
	// make a process "exit" partway through Stop's polling.
	after       int
	afterStarts map[int]time.Time
}

func (p *fakeProber) StartTime(pid int) (time.Time, error) {
	p.calls++
	if p.err != nil {
		return time.Time{}, p.err
	}
	table := p.starts
	if p.after > 0 && p.calls > p.after {
		table = p.afterStarts
	}
	if t, ok := table[pid]; ok {
		return t, nil
	}
	return time.Time{}, watch.ErrNoProcess
}

type fakeSignaller struct {
	sent []syscall.Signal
	pids []int
	err  error
}

func (s *fakeSignaller) Signal(pid int, sig syscall.Signal) error {
	s.pids = append(s.pids, pid)
	s.sent = append(s.sent, sig)
	return s.err
}

var t0 = time.Date(2026, 8, 26, 9, 0, 0, 500000000, time.UTC)

func newMgr(t *testing.T) *watch.Manager {
	t.Helper()
	return watch.NewManager(filepath.Join(t.TempDir(), "watch.json"), nil)
}

func write(t *testing.T, mgr *watch.Manager, rec watch.Record) {
	t.Helper()
	if err := mgr.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestInspect_NoRecord(t *testing.T) {
	t.Parallel()
	st, err := watch.Inspect(newMgr(t), &fakeProber{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if st.Running || st.Stale {
		t.Errorf("Inspect with no record = %+v, want neither running nor stale", st)
	}
}

func TestInspect_Running(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0, ActivateHotkey: "Ctrl+D"})

	st, err := watch.Inspect(mgr, &fakeProber{starts: map[int]time.Time{100: t0}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !st.Running || st.Stale {
		t.Errorf("status = %+v, want running", st)
	}
	if st.Record.ActivateHotkey != "Ctrl+D" {
		t.Errorf("record not carried through: %+v", st.Record)
	}
}

// TestInspect_ReusedPIDIsStale is the reason the record carries a kernel start
// time at all: the PID is alive, but it is somebody else now.
func TestInspect_ReusedPIDIsStale(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})

	st, err := watch.Inspect(mgr, &fakeProber{starts: map[int]time.Time{100: t0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if st.Running || !st.Stale {
		t.Errorf("status = %+v, want stale", st)
	}
}

func TestInspect_DeadPIDIsStale(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})

	st, err := watch.Inspect(mgr, &fakeProber{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !st.Stale || st.Running {
		t.Errorf("status = %+v, want stale", st)
	}
	// Inspect is read-only: the stale record must still be there.
	if _, err := mgr.Read(); err != nil {
		t.Errorf("Inspect removed the record: %v", err)
	}
}

// TestInspect_ZeroProcStartFallsBackToPID covers a record written by a process
// that could not read its own start time: the PID alone decides.
func TestInspect_ZeroProcStartFallsBackToPID(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100})

	st, err := watch.Inspect(mgr, &fakeProber{starts: map[int]time.Time{100: t0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !st.Running {
		t.Errorf("status = %+v, want running (no start time to disagree with)", st)
	}
}

func TestInspect_InvalidPIDIsStale(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		mgr := newMgr(t)
		write(t, mgr, watch.Record{PID: pid})
		p := &fakeProber{}
		st, err := watch.Inspect(mgr, p)
		if err != nil {
			t.Fatalf("pid %d: err = %v", pid, err)
		}
		if !st.Stale {
			t.Errorf("pid %d: status = %+v, want stale", pid, st)
		}
		if p.calls != 0 {
			t.Errorf("pid %d: prober was asked about an invalid pid", pid)
		}
	}
}

func TestInspect_Inconclusive(t *testing.T) {
	t.Parallel()
	t.Run("unreadable record", func(t *testing.T) {
		t.Parallel()
		mgr := newMgr(t)
		if err := os.WriteFile(mgr.Path(), []byte("nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := watch.Inspect(mgr, &fakeProber{}); err == nil {
			t.Error("malformed record did not produce an error")
		}
	})
	t.Run("prober cannot tell", func(t *testing.T) {
		t.Parallel()
		mgr := newMgr(t)
		write(t, mgr, watch.Record{PID: 100})
		boom := errors.New("sysctl exploded")
		_, err := watch.Inspect(mgr, &fakeProber{err: boom})
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want wrapped prober error", err)
		}
	})
}

func TestStop_NotRunning(t *testing.T) {
	t.Parallel()
	sig := &fakeSignaller{}
	res, err := watch.Stop(newMgr(t), &fakeProber{}, sig, watch.StopOptions{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Outcome != watch.NotRunning {
		t.Errorf("outcome = %v, want NotRunning", res.Outcome)
	}
	if len(sig.sent) != 0 {
		t.Errorf("signalled %v with no record", sig.pids)
	}
}

func TestStop_StaleRemoved(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})
	sig := &fakeSignaller{}

	res, err := watch.Stop(mgr, &fakeProber{}, sig, watch.StopOptions{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Outcome != watch.StaleRemoved || res.Record.PID != 100 {
		t.Errorf("result = %+v, want StaleRemoved for pid 100", res)
	}
	if len(sig.sent) != 0 {
		t.Errorf("a stale record was signalled: %v", sig.pids)
	}
	if _, err := mgr.Read(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale record not removed: %v", err)
	}
}

// TestStop_Stopped: the process is alive on the first probes and gone after
// the signal has had a moment to land; Stop reports Stopped and clears the
// record the process did not get to remove itself.
func TestStop_Stopped(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})
	p := &fakeProber{
		starts:      map[int]time.Time{100: t0},
		after:       3, // Inspect + two polls see it alive, then it is gone
		afterStarts: map[int]time.Time{},
	}
	sig := &fakeSignaller{}

	res, err := watch.Stop(mgr, p, sig, watch.StopOptions{Wait: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Outcome != watch.Stopped {
		t.Errorf("outcome = %v, want Stopped", res.Outcome)
	}
	if len(sig.sent) != 1 || sig.sent[0] != syscall.SIGTERM || sig.pids[0] != 100 {
		t.Errorf("signals = %v to %v, want one SIGTERM to 100", sig.sent, sig.pids)
	}
	if _, err := mgr.Read(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record not removed after the process exited: %v", err)
	}
}

// TestStop_ReusedPIDAfterExitCountsAsStopped: the PID came back with a new
// start time before the poll noticed it was gone. That is still "our process
// exited".
func TestStop_ReusedPIDAfterExitCountsAsStopped(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})
	p := &fakeProber{
		starts:      map[int]time.Time{100: t0},
		after:       2,
		afterStarts: map[int]time.Time{100: t0.Add(time.Minute)},
	}
	res, err := watch.Stop(mgr, p, &fakeSignaller{}, watch.StopOptions{Wait: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Outcome != watch.Stopped {
		t.Errorf("outcome = %v, want Stopped", res.Outcome)
	}
}

func TestStop_Timeout(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})
	p := &fakeProber{starts: map[int]time.Time{100: t0}}

	_, err := watch.Stop(mgr, p, &fakeSignaller{}, watch.StopOptions{Wait: 5 * time.Millisecond, Poll: time.Millisecond})
	if !errors.Is(err, watch.ErrStopTimeout) {
		t.Fatalf("err = %v, want ErrStopTimeout", err)
	}
	// The process still owns its record.
	if _, err := mgr.Read(); err != nil {
		t.Errorf("record removed although the process is still alive: %v", err)
	}
}

func TestStop_SignalFailed(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})
	p := &fakeProber{starts: map[int]time.Time{100: t0}}

	_, err := watch.Stop(mgr, p, &fakeSignaller{err: syscall.EPERM}, watch.StopOptions{Wait: time.Millisecond, Poll: time.Millisecond})
	if !errors.Is(err, watch.ErrSignalFailed) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err = %v, want ErrSignalFailed wrapping EPERM", err)
	}
	if _, err := mgr.Read(); err != nil {
		t.Errorf("record removed although nothing was stopped: %v", err)
	}
}

// TestStop_DoesNotRemoveASuccessorsRecord pins the guard in the cleanup: a
// new --watch that wrote its own record while we waited must not lose it.
func TestStop_DoesNotRemoveASuccessorsRecord(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})

	successor := watch.NewManager(mgr.Path(), nil)
	p := &fakeProber{
		starts:      map[int]time.Time{100: t0},
		after:       2,
		afterStarts: map[int]time.Time{},
	}
	sig := &fakeSignaller{}
	// Simulate the successor publishing right after the signal lands.
	sigThenPublish := signalFunc(func(pid int, s syscall.Signal) error {
		_ = sig.Signal(pid, s)
		return successor.Write(watch.Record{PID: 200, ProcStartedAt: t0.Add(time.Hour)})
	})

	res, err := watch.Stop(mgr, p, sigThenPublish, watch.StopOptions{Wait: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Outcome != watch.Stopped {
		t.Errorf("outcome = %v, want Stopped", res.Outcome)
	}
	cur, err := successor.Read()
	if err != nil {
		t.Fatalf("successor's record is gone: %v", err)
	}
	if cur.PID != 200 {
		t.Errorf("record now names pid %d, want the successor's 200", cur.PID)
	}
}

type signalFunc func(pid int, sig syscall.Signal) error

func (f signalFunc) Signal(pid int, sig syscall.Signal) error { return f(pid, sig) }

func TestStop_InconclusiveDoesNotSignal(t *testing.T) {
	t.Parallel()
	mgr := newMgr(t)
	write(t, mgr, watch.Record{PID: 100, ProcStartedAt: t0})
	sig := &fakeSignaller{}

	_, err := watch.Stop(mgr, &fakeProber{err: errors.New("cannot tell")}, sig, watch.StopOptions{})
	if err == nil {
		t.Fatal("inconclusive probe did not produce an error")
	}
	if len(sig.sent) != 0 {
		t.Errorf("signalled %v on an inconclusive probe", sig.pids)
	}
}

// TestKernProber_Self is the one real-kernel test: our own PID is alive and
// has a start time; a PID that cannot exist is ErrNoProcess.
func TestKernProber_Self(t *testing.T) {
	t.Parallel()
	p := watch.NewKernProber()
	start, err := p.StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	if start.IsZero() || start.After(time.Now().Add(time.Minute)) {
		t.Errorf("start time of self = %v, implausible", start)
	}
	// Stable across calls — the comparison in Inspect depends on it.
	again, err := p.StartTime(os.Getpid())
	if err != nil || !again.Equal(start) {
		t.Errorf("second StartTime(self) = %v, %v; want %v", again, err, start)
	}
	// PID_MAX on Darwin is 99998; anything above cannot exist.
	if _, err := p.StartTime(1 << 20); !errors.Is(err, watch.ErrNoProcess) {
		t.Errorf("StartTime(impossible pid) err = %v, want ErrNoProcess", err)
	}
	for _, bad := range []int{0, -5} {
		if _, err := p.StartTime(bad); !errors.Is(err, watch.ErrNoProcess) {
			t.Errorf("StartTime(%d) err = %v, want ErrNoProcess", bad, err)
		}
	}
}
