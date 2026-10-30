//go:build linux

package main

import "testing"

func TestLinuxComboLabel(t *testing.T) {
	cases := map[string]string{
		"Ctrl+Option+Cmd+D": "Ctrl+Alt+Super+D",
		"cmd+shift+f5":      "Super+Shift+f5",
		"ctrl+option+x":     "Ctrl+Alt+x",
	}
	for in, want := range cases {
		if got := linuxComboLabel(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
