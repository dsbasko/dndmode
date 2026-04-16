//go:build darwin

package state

import (
	"errors"
	"fmt"
	"sync"
)

// State enumerates the dndmode lifecycle states. There is no
// return path from Exiting/ExitFast — these are terminal.
type State int

const (
	StateIdle State = iota
	StatePreFlight
	StateActive
	StateExiting
	StateExitFast
)

// String returns a stable lowercase name for logs.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StatePreFlight:
		return "preflight"
	case StateActive:
		return "active"
	case StateExiting:
		return "exiting"
	case StateExitFast:
		return "exitfast"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// ErrInvalidTransition is returned by TransitionTo when the requested
// (current → want) transition is not in the legal whitelist.
var ErrInvalidTransition = errors.New("state: invalid transition")

// Machine is a thread-safe state container. New machines start in StateIdle.
type Machine struct {
	mu      sync.Mutex
	current State
}

// NewMachine returns a Machine in StateIdle.
func NewMachine() *Machine { return &Machine{current: StateIdle} }

// Current returns the current state under lock.
func (m *Machine) Current() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// TransitionTo attempts to move from current → want. Returns
// ErrInvalidTransition if the transition is not allowed.
func (m *Machine) TransitionTo(want State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isLegal(m.current, want) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, m.current, want)
	}
	m.current = want
	return nil
}

// isLegal returns true iff (from → to) is a permitted transition.
//
// Legal transitions (Phase 1 baseline; Phase 4 may extend):
//
//	Idle → PreFlight
//	PreFlight → Active | Exiting | ExitFast
//	Active → Exiting | ExitFast
//	Exiting → (terminal — nothing legal)
//	ExitFast → (terminal — nothing legal)
func isLegal(from, to State) bool {
	switch from {
	case StateIdle:
		return to == StatePreFlight
	case StatePreFlight:
		return to == StateActive || to == StateExiting || to == StateExitFast
	case StateActive:
		return to == StateExiting || to == StateExitFast
	case StateExiting, StateExitFast:
		return false
	default:
		return false
	}
}
