//go:build darwin

// Package watch owns the on-disk record of a background `dndmode --watch`
// process — ~/.config/dndmode/watch.json — and the two questions asked of it:
// "is that process still running?" (Inspect) and "make it stop" (Stop).
//
// # Why a record and not pgrep
//
// `--status` and `--kill` run in a different process from the one they
// describe, so they need a handle. A process name is not one: two dndmode
// binaries (brew and /usr/local) can coexist, a one-shot session is also
// called dndmode, and a name match would happily SIGTERM a session that is
// shielding the machine. The record names ONE process, written by that
// process itself when it is ready and removed by it on the way out.
//
// # Why the record carries the kernel start time
//
// A PID alone is not an identity either. A watch process SIGKILLed hours ago
// leaves its record behind, and by the time `--kill` reads it the number can
// belong to anything. So the record also stores the process start time as the
// kernel reports it (kern.proc.pid → p_starttime), and Inspect treats a PID
// whose start time differs as gone. That comparison is exact — microsecond
// equality, not a tolerance — because both sides read the same kernel field.
//
// # What lives elsewhere
//
// The record says nothing about whether the shield is up. That is
// runtime.json's job (internal/state/runtime), written for the length of a
// session and deleted when it ends; a watch process that is idle has no
// runtime.json at all. cmd/dndmode reads both to describe the full state.
//
// Liveness and signalling are behind small interfaces (Prober, Signaller) so
// Inspect and Stop are unit-tested against fakes; the kernel-backed
// implementations are one syscall each.
package watch
