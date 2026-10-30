//go:build linux

package watch

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
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

// StartTime implements Prober. The result identifies the process rather than
// telling the time: a base derived from this boot's boot_id plus the start
// offset in clock ticks. Both are fixed for the life of the process, so two
// calls — from any process, before or after the wall clock is stepped —
// return the same instant, which is what Inspect's exact comparison needs.
// /proc/stat btime is not usable as the base: the kernel derives it from the
// wall clock, so NTP or a resume from suspend moves it.
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
	boot, err := bootID()
	if err != nil {
		return time.Time{}, err
	}
	return startInstant(boot, ticks), nil
}

// startInstant folds a boot identity and a start offset into one instant:
// the FNV-1a hash of bootID as whole seconds since the epoch, plus ticks.
// A reboot changes the base, so a record left from a previous boot does not
// match a new process that happens to get the same PID and offset.
func startInstant(bootID []byte, ticks uint64) time.Time {
	h := fnv.New32a()
	_, _ = h.Write(bootID)
	offset := time.Duration(ticks) * (time.Second / clockTicks)
	return time.Unix(int64(h.Sum32()), 0).Add(offset)
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
	bootRaw  []byte
	bootErr  error
)

// bootID reads /proc/sys/kernel/random/boot_id once per process. It is fixed
// for the lifetime of the kernel and unaffected by the wall clock.
func bootID() ([]byte, error) {
	bootOnce.Do(func() {
		raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			bootErr = fmt.Errorf("read boot_id: %w", err)
			return
		}
		if bootRaw = bytes.TrimSpace(raw); len(bootRaw) == 0 {
			bootErr = errors.New("boot_id is empty")
		}
	})
	return bootRaw, bootErr
}
