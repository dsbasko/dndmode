//go:build darwin

// Package caffeinate runs /usr/bin/caffeinate(8) as a child process to keep the
// system awake for the lifetime of dndmode's overlay_style=none mode.
//
// This is deliberately the ONE place in dndmode that uses the caffeinate
// subprocess instead of the in-process IOKit IOPMAssertion (internal/macos/
// powerassert). The "none" mode is a thin awake-only wrapper — no Focus/DND, no
// CGEventTap, no overlay window — so a self-contained external assertion that is
// trivially observable in `pmset -g assertions` and self-cleaning on crash is a
// good fit here, even though powerassert remains the choice for the full
// locking mode.
//
// Crash safety: caffeinate is launched with `-w <dndmode pid>`. If dndmode is
// hard-killed (SIGKILL — neither defers nor ctx-cancel run), the child is
// reparented to launchd but its `-w` watch notices our PID is gone and drops
// the assertion on its own. No orphaned caffeinate, no stuck awake-lock.
package caffeinate
