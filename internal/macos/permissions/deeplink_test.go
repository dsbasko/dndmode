//go:build darwin

package permissions

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeExecRunner records the (name, args...) tuples it was asked to Start
// and optionally returns a configured error. It is safe for concurrent use
// because OpenAX / OpenIM are documented as callable from any goroutine
// (the real WaitForGrants invokes them on the main polling goroutine, but
// tests should still observe sequential ordering deterministically).
type fakeExecRunner struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeExecRunner) Start(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.err
}

func (f *fakeExecRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExecRunner) lastCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func TestDeepLinker_OpenAX_InvokesOpenWithAXURL(t *testing.T) {
	fake := &fakeExecRunner{}
	link := newDeepLinkerWithRunner(fake)

	if err := link.OpenAX(); err != nil {
		t.Fatalf("OpenAX returned error: %v", err)
	}

	if got, want := fake.callCount(), 1; got != want {
		t.Fatalf("call count = %d, want %d", got, want)
	}
	got := fake.lastCall()
	if !strings.HasPrefix(got, "open ") {
		t.Errorf("call = %q, want prefix %q", got, "open ")
	}
	if !strings.Contains(got, URLAccessibility) {
		t.Errorf("call = %q, want substring %q", got, URLAccessibility)
	}
	if !strings.Contains(got, "Privacy_Accessibility") {
		t.Errorf("call = %q, want substring %q", got, "Privacy_Accessibility")
	}
}

func TestDeepLinker_OpenIM_InvokesOpenWithIMURL(t *testing.T) {
	fake := &fakeExecRunner{}
	link := newDeepLinkerWithRunner(fake)

	if err := link.OpenIM(); err != nil {
		t.Fatalf("OpenIM returned error: %v", err)
	}

	if got, want := fake.callCount(), 1; got != want {
		t.Fatalf("call count = %d, want %d", got, want)
	}
	got := fake.lastCall()
	if !strings.HasPrefix(got, "open ") {
		t.Errorf("call = %q, want prefix %q", got, "open ")
	}
	if !strings.Contains(got, URLInputMonitor) {
		t.Errorf("call = %q, want substring %q", got, URLInputMonitor)
	}
	if !strings.Contains(got, "Privacy_ListenEvent") {
		t.Errorf("call = %q, want substring %q", got, "Privacy_ListenEvent")
	}
}

func TestDeepLinker_OpenAX_PropagatesError(t *testing.T) {
	sentinel := errors.New("simulated open failure")
	fake := &fakeExecRunner{err: sentinel}
	link := newDeepLinkerWithRunner(fake)

	err := link.OpenAX()
	if !errors.Is(err, sentinel) {
		t.Errorf("OpenAX err = %v, want errors.Is(_, sentinel)", err)
	}
}

func TestDeepLinker_OpenIM_PropagatesError(t *testing.T) {
	sentinel := errors.New("simulated open failure")
	fake := &fakeExecRunner{err: sentinel}
	link := newDeepLinkerWithRunner(fake)

	err := link.OpenIM()
	if !errors.Is(err, sentinel) {
		t.Errorf("OpenIM err = %v, want errors.Is(_, sentinel)", err)
	}
}

func TestDeepLinker_Constants_ExpectedURLSchemes(t *testing.T) {
	const (
		wantAX = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
		wantIM = "x-apple.systempreferences:com.apple.preference.security?Privacy_ListenEvent"
	)
	if URLAccessibility != wantAX {
		t.Errorf("URLAccessibility = %q, want %q", URLAccessibility, wantAX)
	}
	if URLInputMonitor != wantIM {
		t.Errorf("URLInputMonitor = %q, want %q", URLInputMonitor, wantIM)
	}
}

func TestNewDeepLinker_ProductionReturnsNonNil(t *testing.T) {
	link := NewDeepLinker()
	if link == nil {
		t.Fatal("NewDeepLinker() returned nil")
	}
}
