//go:build linux

package watch

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat starttime. It is 100
// on every architecture Linux ships for; the kernel ABI fixes it there
// rather than exposing sysconf(_SC_CLK_TCK) through a syscall.
const clockTicks = 100

type kernProber struct{}

// NewKernProber returns the production Prober: kill(pid, 0) to tell a dead
// PID from a live one, then /proc/<pid>/stat for the start time.
func NewKernProber() Prober { return kernProber{} }

// StartTime implements Prober. The result is boot time (whole seconds, from
// /proc/stat btime) plus the process start offset in clock ticks, so two
// calls for the same process always return the same instant — the property
// Inspect's comparison depends on.
func (kernProber) StartTime(pid int) (time.Time, error) {
	// PID 0 means "my process group" to kill(2) and negative PIDs are
	// broadcasts; neither is something a watch process can have written.
	if pid <= 0 {
		return time.Time{}, fmt.Errorf("%w: invalid pid %d", ErrNoProcess, pid)
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return time.Time{}, ErrNoProcess
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return time.Time{}, ErrNoProcess
		}
		return time.Time{}, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	state, ticks, err := parseProcStat(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse /proc/%d/stat: %w", pid, err)
	}
	// A zombie still answers kill(pid, 0) but holds nothing; for every
	// question this package asks, it is gone.
	if state == 'Z' || state == 'X' {
		return time.Time{}, ErrNoProcess
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	offset := time.Duration(ticks) * (time.Second / clockTicks)
	return boot.Add(offset), nil
}

// parseProcStat extracts the state (field 3) and starttime (field 22) from
// /proc/<pid>/stat. The command name (field 2) is parenthesised and may hold
// spaces or parentheses itself, so parsing starts after the LAST ')'.
func parseProcStat(raw []byte) (byte, uint64, error) {
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 || end+2 >= len(raw) {
		return 0, 0, errors.New("no command field")
	}
	fields := bytes.Fields(raw[end+2:])
	// fields[0] is field 3 (state); starttime is field 22 → fields[19].
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, errors.New("too few fields")
	}
	ticks, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("starttime: %w", err)
	}
	return fields[0][0], ticks, nil
}

var (
	bootOnce sync.Once
	bootAt   time.Time
	bootErr  error
)

// bootTime reads btime from /proc/stat once per process. It is fixed for the
// lifetime of the kernel, and reading it once keeps every StartTime result
// on the same base.
func bootTime() (time.Time, error) {
	bootOnce.Do(func() {
		raw, err := os.ReadFile("/proc/stat")
		if err != nil {
			bootErr = fmt.Errorf("read /proc/stat: %w", err)
			return
		}
		for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
			rest, ok := bytes.CutPrefix(line, []byte("btime "))
			if !ok {
				continue
			}
			sec, err := strconv.ParseInt(string(bytes.TrimSpace(rest)), 10, 64)
			if err != nil {
				bootErr = fmt.Errorf("parse btime: %w", err)
				return
			}
			bootAt = time.Unix(sec, 0)
			return
		}
		bootErr = errors.New("/proc/stat has no btime line")
	})
	return bootAt, bootErr
}
