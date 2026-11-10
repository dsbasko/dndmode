//go:build linux

package overview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeShell keeps OverviewActive in memory. closeAfter is how many reads
// after the write still see the overview open (the hide animation).
type fakeShell struct {
	active     bool
	closeAfter int
	stuck      bool
	fail       bool
	sets       int
}

func (f *fakeShell) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if f.fail {
		return nil, errors.New("no session bus")
	}
	if name != "gdbus" {
		return nil, errors.New("unexpected command " + name)
	}
	call := strings.Join(args, " ")
	if strings.Contains(call, propsIface+".Set") {
		f.sets++
		if !strings.HasSuffix(call, prop+" <false>") {
			return nil, errors.New("unexpected set: " + call)
		}
		return []byte("()\n"), nil
	}
	if !strings.Contains(call, propsIface+".Get") {
		return nil, errors.New("unexpected call: " + call)
	}
	if f.active && f.sets > 0 && !f.stuck {
		if f.closeAfter == 0 {
			f.active = false
		} else {
			f.closeAfter--
		}
	}
	return []byte("(<" + map[bool]string{true: "true", false: "false"}[f.active] + ">,)\n"), nil
}

func newTestRunner(f *fakeShell) *Runner {
	return &Runner{run: f.run, poll: time.Millisecond, wait: 50 * time.Millisecond}
}

func TestHide_ClosedOverviewIsLeftAlone(t *testing.T) {
	f := &fakeShell{}
	if err := Hide(context.Background(), newTestRunner(f)); err != nil {
		t.Fatal(err)
	}
	if f.sets != 0 {
		t.Errorf("sets = %d, want 0", f.sets)
	}
}

func TestHide_WaitsForTheAnimation(t *testing.T) {
	f := &fakeShell{active: true, closeAfter: 3}
	if err := Hide(context.Background(), newTestRunner(f)); err != nil {
		t.Fatal(err)
	}
	if f.sets != 1 || f.active {
		t.Errorf("sets = %d, active = %v; want 1, false", f.sets, f.active)
	}
}

func TestHide_GivesUpOnAStuckOverview(t *testing.T) {
	f := &fakeShell{active: true, stuck: true}
	if err := Hide(context.Background(), newTestRunner(f)); !errors.Is(err, ErrStillOpen) {
		t.Errorf("err = %v, want ErrStillOpen", err)
	}
}

func TestHide_NoShell(t *testing.T) {
	f := &fakeShell{fail: true}
	if err := Hide(context.Background(), newTestRunner(f)); err == nil {
		t.Error("err = nil without a session bus")
	}
}
