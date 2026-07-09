//go:build darwin

package main

import (
	"testing"
	"time"
)

// Test_resolveBoolFlag pins the --mute/--focus tri-state precedence: an empty
// flag value falls back to the config default, a non-empty value is parsed via
// strconv.ParseBool, and junk surfaces an error so main() can exit 1. This is a
// pure decision point that the GUI-gated acceptance tests only exercise
// indirectly (and auto-skip on headless CI), so it is unit-tested directly here.
func Test_resolveBoolFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		flagVal       string
		configDefault bool
		want          bool
		wantErr       bool
	}{
		{name: "empty flag falls back to config default true", flagVal: "", configDefault: true, want: true},
		{name: "empty flag falls back to config default false", flagVal: "", configDefault: false, want: false},
		{name: "flag true overrides config false", flagVal: "true", configDefault: false, want: true},
		{name: "flag false overrides config true", flagVal: "false", configDefault: true, want: false},
		{name: "flag 1 parses as true", flagVal: "1", configDefault: false, want: true},
		{name: "flag 0 parses as false", flagVal: "0", configDefault: true, want: false},
		{name: "flag T parses as true", flagVal: "T", configDefault: false, want: true},
		{name: "flag F parses as false", flagVal: "F", configDefault: true, want: false},
		{name: "junk flag returns error", flagVal: "banana", configDefault: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveBoolFlag(tt.flagVal, tt.configDefault)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveBoolFlag(%q, %v) err = nil, want error", tt.flagVal, tt.configDefault)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBoolFlag(%q, %v) unexpected err = %v", tt.flagVal, tt.configDefault, err)
			}
			if got != tt.want {
				t.Errorf("resolveBoolFlag(%q, %v) = %v, want %v", tt.flagVal, tt.configDefault, got, tt.want)
			}
		})
	}
}

// Test_parseTimer pins the --timer flag resolution: an empty flag is disarmed
// (0, no error), a valid Go duration parses to that duration, and junk / zero /
// negative values surface an error so main() can exit 1 naming --timer. Like
// resolveBoolFlag this is a pure decision point the GUI-gated acceptance tests
// only exercise indirectly, so it is unit-tested directly here.
func Test_parseTimer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flagVal string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty disarms (no timer)", flagVal: "", want: 0},
		{name: "minutes", flagVal: "30m", want: 30 * time.Minute},
		{name: "compound hours+minutes", flagVal: "1h30m", want: 90 * time.Minute},
		{name: "seconds", flagVal: "90s", want: 90 * time.Second},
		{name: "sub-second still valid", flagVal: "500ms", want: 500 * time.Millisecond},
		{name: "bare zero rejected", flagVal: "0", wantErr: true},
		{name: "zero seconds rejected", flagVal: "0s", wantErr: true},
		{name: "negative rejected", flagVal: "-5m", wantErr: true},
		{name: "unknown unit rejected", flagVal: "5x", wantErr: true},
		{name: "missing unit rejected", flagVal: "30", wantErr: true},
		{name: "junk rejected", flagVal: "banana", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseTimer(tt.flagVal)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTimer(%q) err = nil, want error", tt.flagVal)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTimer(%q) unexpected err = %v", tt.flagVal, err)
			}
			if got != tt.want {
				t.Errorf("parseTimer(%q) = %v, want %v", tt.flagVal, got, tt.want)
			}
		})
	}
}
