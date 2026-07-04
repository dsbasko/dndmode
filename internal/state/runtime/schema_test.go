//go:build darwin

package runtime_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/state/runtime"
)

// fixedStartedAt is a deterministic timestamp used across schema tests
// so JSON output is stable across runs (RFC3339, UTC).
var fixedStartedAt = time.Date(2026, 5, 14, 9, 42, 13, 0, time.UTC)

// TestSnapshot_NilPriorFocus_MarshalsAsNull verifies the design notes:
// PriorFocus *string with nil value renders as JSON `null`, not as the
// empty string. v1 (deferred) always writes nil.
//
// Validation map ID: 5-04-01.
func TestSnapshot_NilPriorFocus_MarshalsAsNull(t *testing.T) {
	t.Parallel()

	s := runtime.Snapshot{
		PID:         12345,
		StartedAt:   fixedStartedAt,
		PriorFocus:  nil,
		AssertionID: 67890,
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "\"prior_focus\": null") {
		t.Errorf("output does not contain `\"prior_focus\": null`; got:\n%s", got)
	}
	if strings.Contains(got, "\"prior_focus\": \"\"") {
		t.Errorf("output rendered nil pointer as empty string (must be JSON null); got:\n%s", got)
	}
}

// TestSnapshot_StringPriorFocus_MarshalsAsQuoted verifies the v2 path:
// a non-nil *string renders as a quoted JSON string. The current code
// never populates PriorFocus (deferred), but the contract must
// hold so future code lighting up snapshot/restore does not require
// schema migration.
func TestSnapshot_StringPriorFocus_MarshalsAsQuoted(t *testing.T) {
	t.Parallel()

	pf := "Work"
	s := runtime.Snapshot{
		PID:         42,
		StartedAt:   fixedStartedAt,
		PriorFocus:  &pf,
		AssertionID: 99,
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "\"prior_focus\": \"Work\"") {
		t.Errorf("output does not contain `\"prior_focus\": \"Work\"`; got:\n%s", got)
	}
}

// TestSnapshot_Unmarshal_NullPriorFocus_NilPointer verifies the
// inverse: a `null` JSON value parses into a nil *string pointer.
func TestSnapshot_Unmarshal_NullPriorFocus_NilPointer(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "pid": 100,
  "started_at": "2026-05-14T09:42:13Z",
  "prior_focus": null,
  "assertion_id": 200
}`)
	var s runtime.Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.PriorFocus != nil {
		t.Errorf("PriorFocus = %v; want nil", s.PriorFocus)
	}
}

// TestSnapshot_Unmarshal_StringPriorFocus_Populated verifies that a
// quoted JSON string parses into a non-nil *string pointing to the
// expected value.
func TestSnapshot_Unmarshal_StringPriorFocus_Populated(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "pid": 100,
  "started_at": "2026-05-14T09:42:13Z",
  "prior_focus": "Work",
  "assertion_id": 200
}`)
	var s runtime.Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.PriorFocus == nil {
		t.Fatal("PriorFocus = nil; want non-nil pointer to \"Work\"")
	}
	if *s.PriorFocus != "Work" {
		t.Errorf("*PriorFocus = %q; want %q", *s.PriorFocus, "Work")
	}
}

// TestSnapshot_Unmarshal_MissingPriorFocus_NilPointer verifies v0
// upgrade compatibility: a JSON document without the prior_focus key
// (a hypothetical older schema) parses into a nil pointer rather than
// failing.
func TestSnapshot_Unmarshal_MissingPriorFocus_NilPointer(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "pid": 100,
  "started_at": "2026-05-14T09:42:13Z",
  "assertion_id": 200
}`)
	var s runtime.Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.PriorFocus != nil {
		t.Errorf("PriorFocus = %v; want nil (missing key in source)", s.PriorFocus)
	}
}

// TestSnapshot_String_RendersDiagnostic verifies the String() method
// emits a single-line deterministic diagnostic containing all four
// fields. Format used by slog stderr lines in main.go.
func TestSnapshot_String_RendersDiagnostic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		s    runtime.Snapshot
		want []string // substrings that MUST appear
	}{
		{
			name: "nil prior focus",
			s: runtime.Snapshot{
				PID:         12345,
				StartedAt:   fixedStartedAt,
				PriorFocus:  nil,
				AssertionID: 67890,
			},
			want: []string{
				"pid=12345",
				"started_at=2026-05-14T09:42:13Z",
				"prior_focus=null",
				"assertion_id=67890",
				"prior_muted=null",
			},
		},
		{
			name: "string prior focus",
			s: func() runtime.Snapshot {
				pf := "Work"
				return runtime.Snapshot{
					PID:         42,
					StartedAt:   fixedStartedAt,
					PriorFocus:  &pf,
					AssertionID: 99,
				}
			}(),
			want: []string{
				"pid=42",
				"prior_focus=\"Work\"",
				"assertion_id=99",
				"prior_muted=null",
			},
		},
		{
			name: "prior muted true",
			s: func() runtime.Snapshot {
				pm := true
				return runtime.Snapshot{
					PID:         42,
					StartedAt:   fixedStartedAt,
					PriorFocus:  nil,
					AssertionID: 99,
					PriorMuted:  &pm,
				}
			}(),
			want: []string{
				"prior_muted=true",
			},
		},
		{
			name: "prior muted false",
			s: func() runtime.Snapshot {
				pm := false
				return runtime.Snapshot{
					PID:         42,
					StartedAt:   fixedStartedAt,
					PriorFocus:  nil,
					AssertionID: 99,
					PriorMuted:  &pm,
				}
			}(),
			want: []string{
				"prior_muted=false",
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.s.String()
			if strings.Contains(got, "\n") {
				t.Errorf("String() contained newline; must be single-line:\n%s", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("String() missing substring %q; got: %s", w, got)
				}
			}
		})
	}
}

// TestSnapshot_RoundTrip_PreservesAllFields verifies that marshal →
// unmarshal preserves all field values. Uses time.Time.Equal to
// guard against the monotonic-clock side-channel that strips during
// JSON RFC3339 serialization.
func TestSnapshot_RoundTrip_PreservesAllFields(t *testing.T) {
	t.Parallel()

	pf := "Personal"
	pm := true
	orig := runtime.Snapshot{
		PID:         7,
		StartedAt:   fixedStartedAt,
		PriorFocus:  &pf,
		AssertionID: 0xDEADBEEF,
		PriorMuted:  &pm,
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got runtime.Snapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.PID != orig.PID {
		t.Errorf("PID: got %d, want %d", got.PID, orig.PID)
	}
	if !got.StartedAt.Equal(orig.StartedAt) {
		t.Errorf("StartedAt: got %v, want %v", got.StartedAt, orig.StartedAt)
	}
	if got.PriorFocus == nil || *got.PriorFocus != *orig.PriorFocus {
		t.Errorf("PriorFocus: got %v, want pointer to %q", got.PriorFocus, *orig.PriorFocus)
	}
	if got.AssertionID != orig.AssertionID {
		t.Errorf("AssertionID: got %d, want %d", got.AssertionID, orig.AssertionID)
	}
	if got.PriorMuted == nil || *got.PriorMuted != *orig.PriorMuted {
		t.Errorf("PriorMuted: got %v, want pointer to %t", got.PriorMuted, *orig.PriorMuted)
	}
}

// TestSnapshot_PriorMuted_Marshal verifies that PriorMuted *bool renders
// as JSON `null` when nil and as `true`/`false` when populated. nil is
// the v1 default (audio untouched) and must not render as `false`.
func TestSnapshot_PriorMuted_Marshal(t *testing.T) {
	t.Parallel()

	mkBool := func(b bool) *bool { return &b }

	cases := []struct {
		name    string
		muted   *bool
		want    string // substring that MUST appear
		notWant string // substring that MUST NOT appear ("" = skip)
	}{
		{name: "nil renders null", muted: nil, want: "\"prior_muted\": null", notWant: "\"prior_muted\": false"},
		{name: "true renders true", muted: mkBool(true), want: "\"prior_muted\": true"},
		{name: "false renders false", muted: mkBool(false), want: "\"prior_muted\": false"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := runtime.Snapshot{
				PID:         1,
				StartedAt:   fixedStartedAt,
				PriorFocus:  nil,
				AssertionID: 2,
				PriorMuted:  tt.muted,
			}
			data, err := json.MarshalIndent(s, "", "  ")
			if err != nil {
				t.Fatalf("MarshalIndent: %v", err)
			}
			got := string(data)
			if !strings.Contains(got, tt.want) {
				t.Errorf("output missing %q; got:\n%s", tt.want, got)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("output unexpectedly contains %q; got:\n%s", tt.notWant, got)
			}
		})
	}
}

// TestSnapshot_PriorMuted_Unmarshal verifies the inverse parse:
// `null`/`true`/`false` parse into nil/true/false pointers respectively.
func TestSnapshot_PriorMuted_Unmarshal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		raw      string
		wantNil  bool
		wantBool bool
	}{
		{name: "null is nil", raw: "null", wantNil: true},
		{name: "true is non-nil true", raw: "true", wantNil: false, wantBool: true},
		{name: "false is non-nil false", raw: "false", wantNil: false, wantBool: false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(`{
  "pid": 100,
  "started_at": "2026-05-14T09:42:13Z",
  "prior_focus": null,
  "assertion_id": 200,
  "prior_muted": ` + tt.raw + `
}`)
			var s runtime.Snapshot
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if tt.wantNil {
				if s.PriorMuted != nil {
					t.Errorf("PriorMuted = %v; want nil", s.PriorMuted)
				}
				return
			}
			if s.PriorMuted == nil {
				t.Fatalf("PriorMuted = nil; want pointer to %t", tt.wantBool)
			}
			if *s.PriorMuted != tt.wantBool {
				t.Errorf("*PriorMuted = %t; want %t", *s.PriorMuted, tt.wantBool)
			}
		})
	}
}

// TestSnapshot_Unmarshal_MissingPriorMuted_NilPointer verifies backward
// compatibility: an old runtime.json written before the prior_muted key
// existed parses into a nil pointer (audio untouched) rather than
// failing.
func TestSnapshot_Unmarshal_MissingPriorMuted_NilPointer(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "pid": 100,
  "started_at": "2026-05-14T09:42:13Z",
  "prior_focus": null,
  "assertion_id": 200
}`)
	var s runtime.Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.PriorMuted != nil {
		t.Errorf("PriorMuted = %v; want nil (missing key in source)", s.PriorMuted)
	}
}
