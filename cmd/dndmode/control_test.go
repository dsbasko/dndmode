//go:build darwin

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
	watchpkg "github.com/dsbasko/dndmode/internal/state/watch"
)

func Test_controlFlags_conflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		flags     controlFlags
		wantSelf  string
		wantOther string
	}{
		{name: "status alone", flags: controlFlags{status: true}, wantSelf: "--status"},
		{name: "kill alone", flags: controlFlags{kill: true}, wantSelf: "--kill"},
		{name: "both", flags: controlFlags{kill: true, status: true}, wantSelf: "--kill", wantOther: "--status"},
		{name: "kill + set-password", flags: controlFlags{kill: true, setPassword: true}, wantSelf: "--kill", wantOther: "--set-password"},
		{name: "status + watch", flags: controlFlags{status: true, watch: true}, wantSelf: "--status", wantOther: "--watch"},
		{name: "kill + style", flags: controlFlags{kill: true, style: "matrix"}, wantSelf: "--kill", wantOther: "--style"},
		{name: "status + timer", flags: controlFlags{status: true, timer: "30m"}, wantSelf: "--status", wantOther: "--timer"},
		{name: "kill + mute", flags: controlFlags{kill: true, mute: "false"}, wantSelf: "--kill", wantOther: "--mute"},
		{name: "status + focus", flags: controlFlags{status: true, focus: "true"}, wantSelf: "--status", wantOther: "--focus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			self, other := tt.flags.conflict()
			if self != tt.wantSelf || other != tt.wantOther {
				t.Errorf("conflict() = %q, %q; want %q, %q", self, other, tt.wantSelf, tt.wantOther)
			}
		})
	}
}

func Test_setPasswordFlags_RefusesControlFlags(t *testing.T) {
	t.Parallel()
	if got := (setPasswordFlags{kill: true}).conflictingFlag(); got != "--kill" {
		t.Errorf("--set-password --kill: conflictingFlag() = %q, want --kill", got)
	}
	if got := (setPasswordFlags{status: true}).conflictingFlag(); got != "--status" {
		t.Errorf("--set-password --status: conflictingFlag() = %q, want --status", got)
	}
}

func TestRunWatchControl_ConflictExits1(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	code := runWatchControl(controlFlags{kill: true, status: true}, &out, &errb, nil)
	if code != exitConfigErr {
		t.Errorf("exit = %d, want %d", code, exitConfigErr)
	}
	if !strings.Contains(errb.String(), "--kill cannot be combined with --status") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// --- fakes ------------------------------------------------------------------

type ctlProber struct {
	starts map[int]time.Time
	err    error
	calls  int
	after  int
	then   map[int]time.Time
}

func (p *ctlProber) StartTime(pid int) (time.Time, error) {
	p.calls++
	if p.err != nil {
		return time.Time{}, p.err
	}
	table := p.starts
	if p.after > 0 && p.calls > p.after {
		table = p.then
	}
	if t, ok := table[pid]; ok {
		return t, nil
	}
	return time.Time{}, watchpkg.ErrNoProcess
}

type ctlSignaller struct {
	pids []int
	sigs []syscall.Signal
	err  error
}

func (s *ctlSignaller) Signal(pid int, sig syscall.Signal) error {
	s.pids = append(s.pids, pid)
	s.sigs = append(s.sigs, sig)
	return s.err
}

type ctlLive map[int]bool

func (l ctlLive) IsAlive(pid int) bool { return l[pid] }

var ctlStart = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

type ctlHarness struct {
	deps     controlDeps
	out, err bytes.Buffer
	prober   *ctlProber
	sig      *ctlSignaller
	live     ctlLive
}

func newCtlHarness(t *testing.T) *ctlHarness {
	t.Helper()
	dir := t.TempDir()
	h := &ctlHarness{
		prober: &ctlProber{starts: map[int]time.Time{}},
		sig:    &ctlSignaller{},
		live:   ctlLive{},
	}
	h.deps = controlDeps{
		watch:   watchpkg.NewManager(filepath.Join(dir, "watch.json"), nil),
		runtime: runtimepkg.NewManager(filepath.Join(dir, "runtime.json"), nil),
		prober:  h.prober,
		live:    h.live,
		sig:     h.sig,
		now:     func() time.Time { return ctlStart.Add(90 * time.Minute) },
		stop:    watchpkg.StopOptions{Wait: 200 * time.Millisecond, Poll: time.Millisecond},
	}
	return h
}

func (h *ctlHarness) running(t *testing.T, pid int) {
	t.Helper()
	if err := h.deps.watch.Write(watchpkg.Record{
		PID: pid, StartedAt: ctlStart, ProcStartedAt: ctlStart.Add(-time.Second),
		ActivateHotkey: "Ctrl+Option+Cmd+D", LogPath: "/x/watch.log",
	}); err != nil {
		t.Fatal(err)
	}
	h.prober.starts[pid] = ctlStart.Add(-time.Second)
}

// --- status -----------------------------------------------------------------

func TestRunStatus_NotWatching(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	code := runStatus(h.deps, &h.out, &h.err)
	if code != exitNotWatching {
		t.Errorf("exit = %d, want %d", code, exitNotWatching)
	}
	if got := h.out.String(); !strings.HasPrefix(got, "dndmode: not watching\n") || !strings.Contains(got, "dndmode --watch") {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunStatus_RunningShieldDown(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)

	code := runStatus(h.deps, &h.out, &h.err)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, h.err.String())
	}
	out := h.out.String()
	if !strings.HasPrefix(out, "dndmode: watching\n") {
		t.Errorf("stdout does not open with the headline:\n%s", out)
	}
	for _, want := range []string{
		"  pid     4242\n",
		"  uptime  1h 30m  (since ",
		"  hotkey  Ctrl+Option+Cmd+D",
		"  shield  down\n",
		"  log     /x/watch.log\n",
		"  stop    dndmode --kill\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestRunStatus_RunningShieldUp(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)
	if err := h.deps.runtime.Write(runtimepkg.Snapshot{PID: 4242, StartedAt: ctlStart.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.live[4242] = true

	if code := runStatus(h.deps, &h.out, &h.err); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if out := h.out.String(); !strings.Contains(out, "  shield  UP — locked since ") {
		t.Errorf("stdout does not report the shield:\n%s", out)
	}
}

func TestRunStatus_RunningWithSeparateSession(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)
	if err := h.deps.runtime.Write(runtimepkg.Snapshot{PID: 777, StartedAt: ctlStart}); err != nil {
		t.Fatal(err)
	}
	h.live[777] = true

	if code := runStatus(h.deps, &h.out, &h.err); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if out := h.out.String(); !strings.Contains(out, "  shield  down — a separate dndmode session is active (PID 777)") {
		t.Errorf("stdout:\n%s", out)
	}
}

// A runtime.json whose PID is dead is a crash leftover, not a shield.
func TestRunStatus_DeadSessionSnapshotReadsAsDown(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)
	if err := h.deps.runtime.Write(runtimepkg.Snapshot{PID: 4242, StartedAt: ctlStart}); err != nil {
		t.Fatal(err)
	}
	// h.live[4242] deliberately false.
	if code := runStatus(h.deps, &h.out, &h.err); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if out := h.out.String(); !strings.Contains(out, "  shield  down\n") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestRunStatus_Stale(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	if err := h.deps.watch.Write(watchpkg.Record{PID: 4242, StartedAt: ctlStart, ProcStartedAt: ctlStart}); err != nil {
		t.Fatal(err)
	}
	// prober knows no pid 4242 → gone.
	code := runStatus(h.deps, &h.out, &h.err)
	if code != exitNotWatching {
		t.Errorf("exit = %d, want %d", code, exitNotWatching)
	}
	if out := h.out.String(); !strings.HasPrefix(out, "dndmode: not watching\n") || !strings.Contains(out, "  stale   record for PID 4242, which is gone") {
		t.Errorf("stdout = %q", out)
	}
	// Read-only.
	if _, err := h.deps.watch.Read(); err != nil {
		t.Errorf("--status removed the record: %v", err)
	}
}

func TestRunStatus_Inconclusive(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	if err := os.WriteFile(h.deps.watch.Path(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := runStatus(h.deps, &h.out, &h.err)
	if code != exitRuntimeJSON {
		t.Errorf("exit = %d, want %d", code, exitRuntimeJSON)
	}
	if !strings.Contains(h.err.String(), h.deps.watch.Path()) {
		t.Errorf("stderr does not name the file: %q", h.err.String())
	}
}

// --- kill -------------------------------------------------------------------

func TestRunKill_NotWatching(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	if code := runKill(h.deps, &h.out, &h.err); code != exitOK {
		t.Errorf("exit = %d, want 0 (idempotent)", code)
	}
	if got := h.out.String(); got != "dndmode: not watching\n" {
		t.Errorf("stdout = %q", got)
	}
	if len(h.sig.pids) != 0 {
		t.Errorf("signalled %v", h.sig.pids)
	}
}

func TestRunKill_Stopped(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)
	h.prober.after, h.prober.then = 2, map[int]time.Time{} // gone after the signal

	code := runKill(h.deps, &h.out, &h.err)
	if code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, h.err.String())
	}
	if len(h.sig.pids) != 1 || h.sig.pids[0] != 4242 || h.sig.sigs[0] != syscall.SIGTERM {
		t.Errorf("signals = %v/%v, want one SIGTERM to 4242", h.sig.pids, h.sig.sigs)
	}
	if got := h.out.String(); got != "dndmode: stopped watching (PID 4242)\n" {
		t.Errorf("stdout = %q", got)
	}
	if _, err := h.deps.watch.Read(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record still present: %v", err)
	}
}

func TestRunKill_StaleRemoved(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	if err := h.deps.watch.Write(watchpkg.Record{PID: 4242, ProcStartedAt: ctlStart}); err != nil {
		t.Fatal(err)
	}
	code := runKill(h.deps, &h.out, &h.err)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if got := h.out.String(); got != "dndmode: not watching — cleared a stale record for PID 4242\n" {
		t.Errorf("stdout = %q", got)
	}
	if len(h.sig.pids) != 0 {
		t.Errorf("a stale record was signalled: %v", h.sig.pids)
	}
}

func TestRunKill_Timeout(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242) // never goes away

	code := runKill(h.deps, &h.out, &h.err)
	if code != exitPlatformErr {
		t.Errorf("exit = %d, want %d", code, exitPlatformErr)
	}
	if errs := h.err.String(); !strings.Contains(errs, "kill -9 4242") {
		t.Errorf("stderr = %q", errs)
	}
	if _, err := h.deps.watch.Read(); err != nil {
		t.Errorf("record removed although the process is still alive: %v", err)
	}
}

func TestRunKill_SignalRefused(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)
	h.sig.err = syscall.EPERM

	code := runKill(h.deps, &h.out, &h.err)
	if code != exitPlatformErr {
		t.Errorf("exit = %d, want %d", code, exitPlatformErr)
	}
	if errs := h.err.String(); !strings.Contains(errs, "cannot stop") {
		t.Errorf("stderr = %q", errs)
	}
}

func TestRunKill_InconclusiveSignalsNothing(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t)
	h.running(t, 4242)
	h.prober.err = errors.New("sysctl unavailable")

	code := runKill(h.deps, &h.out, &h.err)
	if code != exitRuntimeJSON {
		t.Errorf("exit = %d, want %d", code, exitRuntimeJSON)
	}
	if len(h.sig.pids) != 0 {
		t.Errorf("signalled %v on an inconclusive probe", h.sig.pids)
	}
}

func Test_formatUptime(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "0s"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3*time.Minute + 20*time.Second, "3m"},
		{90*time.Minute + 1500*time.Millisecond, "1h 30m"},
		{23*time.Hour + 59*time.Minute, "23h 59m"},
		{26 * time.Hour, "1d 2h"},
		{3*24*time.Hour + 5*time.Minute, "3d 0h"},
	} {
		if got := formatUptime(tt.d); got != tt.want {
			t.Errorf("formatUptime(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func Test_abbreviateHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := abbreviateHome(filepath.Join(home, ".config", "dndmode", "watch.log")); got != "~/.config/dndmode/watch.log" {
		t.Errorf("abbreviateHome(under home) = %q", got)
	}
	if got := abbreviateHome("/var/log/x"); got != "/var/log/x" {
		t.Errorf("abbreviateHome(elsewhere) = %q", got)
	}
	// A sibling of $HOME that merely shares its prefix is not under it.
	if got := abbreviateHome(home + "2/x"); got != home+"2/x" {
		t.Errorf("abbreviateHome(prefix-sibling) = %q", got)
	}
}
