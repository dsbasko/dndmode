//go:build linux

package gnomekeys

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
)

func TestAccelerator(t *testing.T) {
	cases := map[string]string{
		"Ctrl+Option+Cmd+D": "<Control><Alt><Super>d",
		"shift+cmd+f5":      "<Super><Shift>F5",
		"ctrl+/":            "<Control>slash",
	}
	for in, want := range cases {
		spec, err := hotkey.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Accelerator(spec)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", in, got, err, want)
		}
	}
	bare, _ := hotkey.ParseStep("d")
	if _, err := Accelerator(bare); !errors.Is(err, ErrNoModifier) {
		t.Errorf("modifierless: %v", err)
	}
}

func TestNormalize(t *testing.T) {
	if normalize("<Primary><Alt>T") != normalize("<Alt><Control>t") {
		t.Error("Primary/Control and case must compare equal")
	}
	if normalize("<Super>l") == normalize("<Super><Shift>l") {
		t.Error("different modifier sets compared equal")
	}
}

func TestStrv(t *testing.T) {
	if got := parseStrv("@as []"); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
	in := []string{"/a/b/", "/c/"}
	if got := parseStrv(formatStrv(in)); !slices.Equal(got, in) {
		t.Errorf("roundtrip: %v", got)
	}
	if formatStrv(nil) != "@as []" {
		t.Error("empty list must be typed")
	}
}

func TestShellQuote(t *testing.T) {
	if shellQuote("/usr/local/bin/dndmode") != "/usr/local/bin/dndmode" {
		t.Error("plain path quoted")
	}
	if got := shellQuote("/home/a b/it's"); got != `'/home/a b/it'\''s'` {
		t.Errorf("got %s", got)
	}
}

// fakeDconf is an in-memory gsettings.
type fakeDconf struct {
	vals map[string]string // "schema key" → value
}

func (f *fakeDconf) run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "get":
		if v, ok := f.vals[args[1]+" "+args[2]]; ok {
			return v, nil
		}
		return "''", nil
	case "set":
		f.vals[args[1]+" "+args[2]] = args[3]
		return "", nil
	case "reset":
		delete(f.vals, args[1]+" "+args[2])
		return "", nil
	case "list-recursively":
		var out []string
		for k, v := range f.vals {
			if strings.HasPrefix(k, args[1]+" ") {
				out = append(out, k+" "+v)
			}
		}
		return strings.Join(out, "\n"), nil
	}
	return "", errors.New("unexpected")
}

func TestRegisterRemove_KeepsUserEntries(t *testing.T) {
	user := "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/custom0/"
	f := &fakeDconf{vals: map[string]string{
		listSchema + " " + listKey:               "['" + user + "']",
		entrySchema + ":" + user + " binding":    "'<Super><Shift>m'",
		"org.gnome.desktop.wm.keybindings close": "['<Alt>F4']",
	}}
	c := client{run: f.run}

	for _, accel := range []string{"<Alt>F4", "<Shift><Super>M"} {
		if busy, err := c.taken(accel); err != nil || !busy {
			t.Errorf("%s: taken = %v, %v; want true", accel, busy, err)
		}
	}
	if busy, _ := c.taken("<Control><Alt><Super>d"); busy {
		t.Error("free combination reported taken")
	}

	if err := c.register("<Control><Alt><Super>d", "dndmode --activate"); err != nil {
		t.Fatal(err)
	}
	list := parseStrv(f.vals[listSchema+" "+listKey])
	if !slices.Equal(list, []string{user, Path}) {
		t.Fatalf("list after register: %v", list)
	}
	if f.vals[entry()+" command"] != "dndmode --activate" {
		t.Errorf("command = %q", f.vals[entry()+" command"])
	}
	// Our own entry never counts as a conflict.
	if busy, _ := c.taken("<Control><Alt><Super>d"); busy {
		t.Error("own registration reported as a conflict")
	}

	if err := c.remove(); err != nil {
		t.Fatal(err)
	}
	list = parseStrv(f.vals[listSchema+" "+listKey])
	if !slices.Equal(list, []string{user}) {
		t.Errorf("list after remove: %v", list)
	}
	if _, ok := f.vals[entry()+" binding"]; ok {
		t.Error("binding not reset")
	}
}
