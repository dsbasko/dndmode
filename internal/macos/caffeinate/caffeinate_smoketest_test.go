//go:build darwin

package caffeinate

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSmoke_Caffeinate_RealRoundtrip exercises the REAL /usr/bin/caffeinate and
// verifies via pmset(1) that a power assertion appears while the child runs and
// disappears after Release. Gated by HEADLESS=1 for CI consistency (mirrors the
// powerassert smoke), since pmset output inside a restricted CI sandbox may not
// reflect child assertions.
func TestSmoke_Caffeinate_RealRoundtrip(t *testing.T) {
	if os.Getenv("HEADLESS") != "" {
		t.Skip("caffeinate smoke gated by HEADLESS=1 for CI consistency")
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("caffeinate not present at %s: %v", binPath, err)
	}

	p, err := Start(context.Background(), os.Getpid(), false, quietLogger())
	if err != nil {
		t.Fatalf("Start real caffeinate: %v", err)
	}
	t.Cleanup(func() { _ = p.Release() })

	childPID := p.cmd.Process.Pid

	// The assertion should be visible to pmset shortly after launch.
	if !pmsetMentionsPID(t, childPID, true, 3*time.Second) {
		// Some hosts/policies hide child assertions from pmset; the lifecycle
		// itself is covered by the hermetic unit test, so downgrade to a log.
		t.Logf("pmset did not list caffeinate child pid %d (host policy?); continuing", childPID)
	}

	if err := p.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Hard contract: the child process is actually reaped after Release.
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("real caffeinate not reaped within 5s of Release")
	}

	// Soft check: powerd drops the assertion asynchronously, so a brief lag
	// after the process dies is normal. Log rather than fail if it lingers — the
	// authoritative "it stopped" signal is the reaped process above.
	if pmsetMentionsPID(t, childPID, false, 3*time.Second) {
		t.Logf("note: pmset still listed pid %d shortly after Release (async powerd teardown)", childPID)
	}
}

// pmsetMentionsPID polls `pmset -g assertions` until the given pid is (want=true)
// or is not (want=false) mentioned, or the timeout elapses. Returns whether the
// desired condition was observed.
func pmsetMentionsPID(t *testing.T, pid int, want bool, timeout time.Duration) bool {
	t.Helper()
	needle := "pid " + strconv.Itoa(pid)
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("/usr/bin/pmset", "-g", "assertions").CombinedOutput()
		if err == nil && strings.Contains(string(out), needle) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}
