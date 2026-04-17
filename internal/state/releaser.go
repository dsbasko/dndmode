//go:build darwin

// Package state provides the Releaser contract, RestoreState LIFO cleanup
// registry, and the dndmode lifecycle state machine.
package state

//go:generate mockgen -source=releaser.go -destination=mocks/releaser.go -package=mocks

// Releaser is implemented by anything that holds a system resource that
// must be released on shutdown. Phase 1 implementations are in-memory mocks
// (MockReleaser); Phase 2-5 add real releasers (NSWindow, IOPMAssertion,
// CGEventTap, runtime.json).
type Releaser interface {
	// Release releases the resource. Implementations MUST be idempotent
	// (safe to call multiple times). Subsequent calls return nil without
	// re-attempting the release. Idempotency is typically implemented via
	// an internal atomic.Bool guard (see MockReleaser for reference impl).
	Release() error

	// Name returns a stable human-readable identifier for logs and debug
	// output (e.g. "mock-tap", "ns-window:1234", "iopm-assertion").
	Name() string
}
