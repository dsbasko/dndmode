//go:build darwin

package main

import (
	"flag"
	"strings"
	"testing"
	"unicode/utf8"
)

// Test_usageText_NamesEveryFlag pins --help to the FlagSet: a flag registered
// in defineFlags has to be documented as --<name>, and a --<name> in the text
// has to be a real flag, so the help cannot drift from the program in either
// direction.
func Test_usageText_NamesEveryFlag(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("dndmode", flag.ContinueOnError)
	defineFlags(fs)

	registered := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		registered[f.Name] = true
		if !strings.Contains(usageText, "--"+f.Name) {
			t.Errorf("--help does not mention --%s", f.Name)
		}
	})

	// Every --word in the text must be a registered flag (or --help itself).
	for _, field := range strings.Fields(usageText) {
		name, ok := strings.CutPrefix(field, "--")
		if !ok {
			continue
		}
		name = strings.TrimRight(name, ".,;:)")
		if name == "help" || registered[name] {
			continue
		}
		t.Errorf("--help mentions --%s, which is not a registered flag", name)
	}
}

// Test_usageText_Shape: the text has to fit a default terminal and render as
// a block, not a paragraph — every line under 100 columns, none with trailing
// whitespace, and the layout sections present.
func Test_usageText_Shape(t *testing.T) {
	t.Parallel()
	for i, line := range strings.Split(usageText, "\n") {
		if w := utf8.RuneCountInString(line); w > 100 {
			t.Errorf("line %d is %d columns wide: %q", i+1, w, line)
		}
		if strings.TrimRight(line, " \t") != line {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
	}
	for _, section := range []string{"Usage:", "Flags", "Exit codes:", "Config:", "Docs:"} {
		if !strings.Contains(usageText, section) {
			t.Errorf("--help lacks the %q section", section)
		}
	}
	if !strings.HasPrefix(usageText, "dndmode - ") {
		t.Errorf("--help does not open with the program name: %q", usageText[:40])
	}
	if !strings.HasSuffix(usageText, "\n") {
		t.Error("--help does not end with a newline")
	}
}

// Test_defineFlags_Defaults pins the zero defaults run() relies on: every
// boolean off, every tri-state string empty ("use the config").
func Test_defineFlags_Defaults(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("dndmode", flag.ContinueOnError)
	fl := defineFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]*bool{"debug": fl.debug, "set-password": fl.setPassword, "watch": fl.watch, "kill": fl.kill, "status": fl.status} {
		if v == nil || *v {
			t.Errorf("--%s defaults to %v, want false", name, v)
		}
	}
	for name, v := range map[string]*string{"style": fl.style, "mute": fl.mute, "focus": fl.focus, "timer": fl.timer} {
		if v == nil || *v != "" {
			t.Errorf("--%s defaults to %v, want empty", name, v)
		}
	}
}
