//go:build darwin

package watch

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

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
