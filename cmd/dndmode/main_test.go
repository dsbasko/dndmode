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

// Test_parseStyleFlag pins the --style parsing: a bare style passes through with
// no blur override; a "glass:<n>" suffix yields the parsed radius; the suffix is
// rejected on any non-glass base or with a bad/out-of-range radius. The base
// style is NOT validated here (main() runs ValidateOverlayStyle) so e.g.
// "neon" passes through untouched. Pure decision point, unit-tested directly.
func Test_parseStyleFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       string
		wantBase string
		wantBlur *float64 // nil = expect no override
		wantLang string   // "" = expect no language
		wantErr  bool
	}{
		{name: "bare glass → no override", in: "glass", wantBase: "glass", wantBlur: nil},
		{name: "bare black → no override", in: "black", wantBase: "black", wantBlur: nil},
		{name: "bare none → no override", in: "none", wantBase: "none", wantBlur: nil},
		{name: "bare dvd → no override", in: "dvd", wantBase: "dvd", wantBlur: nil},
		{name: "unknown base passes through (validated later)", in: "neon", wantBase: "neon", wantBlur: nil},
		{name: "glass:24 → 24", in: "glass:24", wantBase: "glass", wantBlur: fptr(24)},
		{name: "glass:8.5 float → 8.5", in: "glass:8.5", wantBase: "glass", wantBlur: fptr(8.5)},
		{name: "glass:0 accepted", in: "glass:0", wantBase: "glass", wantBlur: fptr(0)},
		{name: "glass with spaces around n", in: "glass: 20 ", wantBase: "glass", wantBlur: fptr(20)},
		{name: "suffix on black rejected", in: "black:10", wantErr: true},
		{name: "suffix on dvd rejected", in: "dvd:foo", wantErr: true},
		{name: "suffix on unknown base rejected", in: "neon:10", wantErr: true},
		{name: "non-numeric radius rejected", in: "glass:abc", wantErr: true},
		{name: "empty radius rejected", in: "glass:", wantErr: true},
		{name: "negative radius rejected", in: "glass:-4", wantErr: true},
		{name: "over-max radius rejected", in: "glass:100000", wantErr: true},
		{name: "double colon rejected", in: "glass:16:9", wantErr: true},
		{name: "bare terminal → no language", in: "terminal", wantBase: "terminal", wantLang: ""},
		{name: "terminal:go", in: "terminal:go", wantBase: "terminal", wantLang: "go"},
		{name: "terminal:python", in: "terminal:python", wantBase: "terminal", wantLang: "python"},
		{name: "terminal:typescript", in: "terminal:typescript", wantBase: "terminal", wantLang: "typescript"},
		{name: "terminal:rust", in: "terminal:rust", wantBase: "terminal", wantLang: "rust"},
		{name: "terminal language with spaces", in: "terminal: rust ", wantBase: "terminal", wantLang: "rust"},
		{name: "terminal empty suffix → default", in: "terminal:", wantBase: "terminal", wantLang: ""},
		{name: "unknown terminal language rejected", in: "terminal:ruby", wantErr: true},
		{name: "terminal double colon rejected", in: "terminal:go:extra", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			base, blur, lang, err := parseStyleFlag(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseStyleFlag(%q) err = nil, want error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStyleFlag(%q) unexpected err = %v", tt.in, err)
			}
			if base != tt.wantBase {
				t.Errorf("parseStyleFlag(%q) base = %q, want %q", tt.in, base, tt.wantBase)
			}
			if lang != tt.wantLang {
				t.Errorf("parseStyleFlag(%q) lang = %q, want %q", tt.in, lang, tt.wantLang)
			}
			switch {
			case tt.wantBlur == nil && blur != nil:
				t.Errorf("parseStyleFlag(%q) blur = %g, want nil", tt.in, *blur)
			case tt.wantBlur != nil && blur == nil:
				t.Errorf("parseStyleFlag(%q) blur = nil, want %g", tt.in, *tt.wantBlur)
			case tt.wantBlur != nil && blur != nil && *blur != *tt.wantBlur:
				t.Errorf("parseStyleFlag(%q) blur = %g, want %g", tt.in, *blur, *tt.wantBlur)
			}
		})
	}
}

// fptr returns a pointer to v — a tiny helper for the *float64 want-fields above.
func fptr(v float64) *float64 { return &v }
