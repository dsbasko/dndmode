//go:build darwin

package runtime

import (
	"fmt"
	"time"
)

// Snapshot is the runtime.json schema per the design notes. Locked in v1 to
// exactly four fields; future v2 evolutions (e.g. populating
// PriorFocus) reuse the existing schema without migration because
// PriorFocus is nullable from day one.
//
//   - PID: os.Getpid() at Write time. RecoverFromCrash uses this to
// drive the kill(pid, 0) liveness probe.
//   - StartedAt: time.Now().UTC() at Write time. RFC3339-marshalled
//     via time.Time's stdlib MarshalJSON. Logged for diagnostics; not
//     dispatched on.
// - PriorFocus: *string. v1 (deferred): always nil → marshals
//     as JSON `null`. Future v2-FOC-snapshot: pointer to the string
//     name of the Focus that was active before dndmode started; the
//     recovery path would restore it. No schema migration required.
//   - AssertionID: uint32 (Go-level IOPMAssertionID). Recorded after
// powerassert.Acquire so recovery can release the
//     orphaned assertion by EXACT id, rather than the Phase 3
//     name+type+dead-PID heuristic.
//   - PriorMuted: *bool. The system-audio mute state captured at start,
//     before dndmode muted for the session. nil ⇒ audio was never
//     touched (mute disabled, GetMuted failed, or an old runtime.json
//     written before this field existed). false ⇒ audio was unmuted at
//     start, so exit/recovery must unmute. true ⇒ audio was already
//     muted, so exit/recovery leaves it muted. Nullable from day one ⇒
//     no migration; old files unmarshal with nil.
//
// The JSON tags are stable user-facing contract. Renaming a tag is a
// breaking schema change requiring migration logic.
type Snapshot struct {
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	PriorFocus  *string   `json:"prior_focus"`
	AssertionID uint32    `json:"assertion_id"`
	PriorMuted  *bool     `json:"prior_muted"`
}

// String returns a single-line diagnostic suitable for slog. Format is
// deterministic so log-grep over stderr produces stable matches.
// PriorFocus renders as `null` when nil, else as the quoted string
// content. PriorMuted renders as `null` when nil, else as `true`/`false`.
func (s Snapshot) String() string {
	pf := "null"
	if s.PriorFocus != nil {
		pf = fmt.Sprintf("%q", *s.PriorFocus)
	}
	pm := "null"
	if s.PriorMuted != nil {
		pm = fmt.Sprintf("%t", *s.PriorMuted)
	}
	return fmt.Sprintf("Snapshot{pid=%d, started_at=%s, prior_focus=%s, assertion_id=%d, prior_muted=%s}",
		s.PID,
		s.StartedAt.UTC().Format(time.RFC3339),
		pf,
		s.AssertionID,
		pm,
	)
}
