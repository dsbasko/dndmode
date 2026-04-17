//go:build darwin

package state

import (
	"errors"
	"testing"
)

func TestIsLegal_Internal_AllPairs(t *testing.T) {
	// all 5×5 = 25 pairs explicitly enumerated. Catches any
	// future change to legal-transition whitelist.
	type pair struct {
		from, to State
		legal    bool
	}
	tests := []pair{
		// Idle → only PreFlight legal
		{StateIdle, StateIdle, false},
		{StateIdle, StatePreFlight, true},
		{StateIdle, StateActive, false},
		{StateIdle, StateExiting, false},
		{StateIdle, StateExitFast, false},

		// PreFlight → Active | Exiting | ExitFast
		{StatePreFlight, StateIdle, false},
		{StatePreFlight, StatePreFlight, false},
		{StatePreFlight, StateActive, true},
		{StatePreFlight, StateExiting, true},
		{StatePreFlight, StateExitFast, true},

		// Active → Exiting | ExitFast (no return to PreFlight)
		{StateActive, StateIdle, false},
		{StateActive, StatePreFlight, false},
		{StateActive, StateActive, false},
		{StateActive, StateExiting, true},
		{StateActive, StateExitFast, true},

		// Exiting → terminal (no transitions out)
		{StateExiting, StateIdle, false},
		{StateExiting, StatePreFlight, false},
		{StateExiting, StateActive, false},
		{StateExiting, StateExiting, false},
		{StateExiting, StateExitFast, false},

		// ExitFast → terminal
		{StateExitFast, StateIdle, false},
		{StateExitFast, StatePreFlight, false},
		{StateExitFast, StateActive, false},
		{StateExitFast, StateExiting, false},
		{StateExitFast, StateExitFast, false},
	}
	for _, p := range tests {
		name := p.from.String() + "→" + p.to.String()
		t.Run(name, func(t *testing.T) {
			got := isLegal(p.from, p.to)
			if got != p.legal {
				t.Errorf("isLegal(%s, %s) = %v, want %v", p.from, p.to, got, p.legal)
			}
		})
	}
}

func TestState_Internal_StringRepresentation(t *testing.T) {
	tests := []struct {
		s    State
		want string
	}{
		{StateIdle, "idle"},
		{StatePreFlight, "preflight"},
		{StateActive, "active"},
		{StateExiting, "exiting"},
		{StateExitFast, "exitfast"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.s.String(); got != tt.want {
				t.Errorf("State(%d).String() = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}

func TestMachine_TransitionTo_Internal_RejectsIllegal(t *testing.T) {
	m := NewMachine()

	// Try illegal transition Idle → Active (skip PreFlight).
	err := m.TransitionTo(StateActive)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("got %v, want errors.Is(err, ErrInvalidTransition)", err)
	}
	if m.Current() != StateIdle {
		t.Errorf("state mutated despite illegal transition: %s", m.Current())
	}

	// Legal path: Idle → PreFlight → Active → Exiting.
	if err := m.TransitionTo(StatePreFlight); err != nil {
		t.Fatalf("Idle→PreFlight: %v", err)
	}
	if err := m.TransitionTo(StateActive); err != nil {
		t.Fatalf("PreFlight→Active: %v", err)
	}
	if err := m.TransitionTo(StateExiting); err != nil {
		t.Fatalf("Active→Exiting: %v", err)
	}

	// Exiting is terminal.
	if err := m.TransitionTo(StateActive); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Exiting→Active should fail, got %v", err)
	}
}
