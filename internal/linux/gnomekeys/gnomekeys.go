//go:build linux

// Package gnomekeys is the Linux watch-mode trigger: it registers the
// activate_hotkey combination as a GNOME custom keyboard shortcut whose
// command is `dndmode --activate`. GNOME (gsd-media-keys) matches the
// combination and runs the command; dndmode itself never observes a
// keystroke while it waits — the property Carbon's RegisterEventHotKey gives
// the macOS build.
//
// The shortcut is stored under its own dconf path
// (.../custom-keybindings/dndmode/), so a shortcut the user created by hand
// is never touched, and it is visible in Settings → Keyboard → Custom
// Shortcuts while watch mode runs.
package gnomekeys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
)

const (
	listSchema  = "org.gnome.settings-daemon.plugins.media-keys"
	listKey     = "custom-keybindings"
	entrySchema = "org.gnome.settings-daemon.plugins.media-keys.custom-keybinding"

	// Path is the dconf path of dndmode's own shortcut entry.
	Path = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/dndmode/"

	// Name is the shortcut's label in GNOME Settings.
	Name = "dndmode"

	callTimeout = 5 * time.Second
)

// conflictSchemas hold GNOME's built-in shortcuts. A combination already
// bound in one of them would be taken by GNOME before ours.
var conflictSchemas = []string{
	"org.gnome.desktop.wm.keybindings",
	"org.gnome.shell.keybindings",
	"org.gnome.mutter.keybindings",
	"org.gnome.mutter.wayland.keybindings",
	"org.gnome.settings-daemon.plugins.media-keys",
}

// Sentinel errors. Use errors.Is.
var (
	ErrNoModifier   = errors.New("gnomekeys: activation combination needs at least one modifier (Ctrl, Option/Alt, Cmd/Super or Shift)")
	ErrComboTaken   = errors.New("gnomekeys: combination is already bound to another GNOME shortcut")
	ErrUnavailable  = errors.New("gnomekeys: GNOME custom shortcuts are not available (gsettings missing or not a GNOME session)")
	ErrUnmappedKey  = errors.New("gnomekeys: the activation key has no GNOME key name")
	ErrRegistration = errors.New("gnomekeys: writing the shortcut failed")
)

// keyName maps kVK_* codes to GDK key names.
var keyName = map[uint16]string{
	0x00: "a", 0x0B: "b", 0x08: "c", 0x02: "d", 0x0E: "e", 0x03: "f", 0x05: "g",
	0x04: "h", 0x22: "i", 0x26: "j", 0x28: "k", 0x25: "l", 0x2E: "m", 0x2D: "n",
	0x1F: "o", 0x23: "p", 0x0C: "q", 0x0F: "r", 0x01: "s", 0x11: "t", 0x20: "u",
	0x09: "v", 0x0D: "w", 0x07: "x", 0x10: "y", 0x06: "z",
	0x12: "1", 0x13: "2", 0x14: "3", 0x15: "4", 0x17: "5", 0x16: "6", 0x1A: "7",
	0x1C: "8", 0x19: "9", 0x1D: "0",
	0x7A: "F1", 0x78: "F2", 0x63: "F3", 0x76: "F4", 0x60: "F5", 0x61: "F6",
	0x62: "F7", 0x64: "F8", 0x65: "F9", 0x6D: "F10", 0x67: "F11", 0x6F: "F12",
	0x31: "space", 0x24: "Return", 0x30: "Tab", 0x35: "Escape", 0x33: "BackSpace",
	0x75: "Delete", 0x7B: "Left", 0x7C: "Right", 0x7D: "Down", 0x7E: "Up",
	0x1B: "minus", 0x18: "equal", 0x21: "bracketleft", 0x1E: "bracketright",
	0x29: "semicolon", 0x27: "apostrophe", 0x2B: "comma", 0x2F: "period",
	0x2C: "slash", 0x2A: "backslash", 0x32: "grave",
}

// Accelerator renders spec in GTK accelerator syntax ("<Control><Alt>d").
// option maps to Alt and cmd to Super, as everywhere in the Linux build.
func Accelerator(spec hotkey.Spec) (string, error) {
	if spec.Modifiers&(hotkey.ModCtrl|hotkey.ModOption|hotkey.ModCmd|hotkey.ModShift) == 0 {
		return "", ErrNoModifier
	}
	name, ok := keyName[spec.KeyCode]
	if !ok {
		return "", ErrUnmappedKey
	}
	var b strings.Builder
	for _, m := range []struct {
		bit hotkey.ModFlag
		tok string
	}{
		{hotkey.ModCtrl, "<Control>"},
		{hotkey.ModOption, "<Alt>"},
		{hotkey.ModCmd, "<Super>"},
		{hotkey.ModShift, "<Shift>"},
	} {
		if spec.Modifiers&m.bit != 0 {
			b.WriteString(m.tok)
		}
	}
	b.WriteString(name)
	return b.String(), nil
}

// normalize reduces an accelerator to a canonical, comparable form:
// sorted modifier set plus the lower-cased key.
func normalize(accel string) string {
	var mods []string
	rest := accel
	for strings.HasPrefix(rest, "<") {
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			break
		}
		m := strings.ToLower(rest[1:end])
		switch m {
		case "primary", "ctrl", "control":
			m = "control"
		case "mod1":
			m = "alt"
		case "mod4":
			m = "super"
		}
		mods = append(mods, m)
		rest = rest[end+1:]
	}
	slices.Sort(mods)
	mods = slices.Compact(mods)
	return strings.Join(mods, "+") + "+" + strings.ToLower(rest)
}

// parseStrv parses gsettings' rendering of an 'as' value: "['a', 'b']" or
// "@as []". Values never contain quotes here (dconf paths, accelerators).
func parseStrv(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '\'')
		if i < 0 {
			return out
		}
		s = s[i+1:]
		j := strings.IndexByte(s, '\'')
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}

// formatStrv renders a string list as a GVariant 'as' literal.
func formatStrv(v []string) string {
	if len(v) == 0 {
		return "@as []"
	}
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = "'" + s + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// shellQuote quotes s for the command line GNOME parses with
// g_shell_parse_argv.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]{}~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runFunc runs gsettings; injectable for tests.
type runFunc func(ctx context.Context, args ...string) (string, error)

func gsettings(ctx context.Context, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gsettings", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gsettings %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// client is a gsettings front end bound to a runner.
type client struct{ run runFunc }

func (c client) call(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.run(ctx, args...)
}

func entry() string { return entrySchema + ":" + Path }

// taken reports whether accel is bound anywhere except dndmode's own entry.
func (c client) taken(accel string) (bool, error) {
	want := normalize(accel)
	for _, schema := range conflictSchemas {
		out, err := c.call("list-recursively", schema)
		if err != nil {
			continue // schema not installed on this system
		}
		for line := range strings.SplitSeq(out, "\n") {
			fields := strings.SplitN(line, " ", 3)
			if len(fields) < 3 {
				continue
			}
			for _, v := range parseStrv(fields[2]) {
				if v != "" && normalize(v) == want {
					return true, nil
				}
			}
		}
	}
	list, err := c.call("get", listSchema, listKey)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	for _, p := range parseStrv(list) {
		if p == Path {
			continue
		}
		b, err := c.call("get", entrySchema+":"+p, "binding")
		if err != nil {
			continue
		}
		if v := parseStrv(b); len(v) == 1 && v[0] != "" && normalize(v[0]) == want {
			return true, nil
		}
	}
	return false, nil
}

// register writes dndmode's entry and links it into the custom list.
func (c client) register(accel, command string) error {
	list, err := c.call("get", listSchema, listKey)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	for _, kv := range [][2]string{{"name", Name}, {"command", command}, {"binding", accel}} {
		if _, err := c.call("set", entry(), kv[0], kv[1]); err != nil {
			return fmt.Errorf("%w: %w", ErrRegistration, err)
		}
	}
	paths := parseStrv(list)
	if !slices.Contains(paths, Path) {
		paths = append(paths, Path)
		if _, err := c.call("set", listSchema, listKey, formatStrv(paths)); err != nil {
			return fmt.Errorf("%w: %w", ErrRegistration, err)
		}
	}
	return nil
}

// remove unlinks dndmode's entry and resets its keys. Other entries in the
// list are written back untouched.
func (c client) remove() error {
	var errs []error
	if list, err := c.call("get", listSchema, listKey); err != nil {
		errs = append(errs, err)
	} else if paths := parseStrv(list); slices.Contains(paths, Path) {
		paths = slices.DeleteFunc(paths, func(p string) bool { return p == Path })
		if _, err := c.call("set", listSchema, listKey, formatStrv(paths)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, k := range []string{"binding", "command", "name"} {
		if _, err := c.call("reset", entry(), k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Registration is a live shortcut. It satisfies state.Releaser.
type Registration struct {
	c    client
	log  *slog.Logger
	once sync.Once
}

// Register binds spec to `<exe> --activate`. It refuses a combination GNOME
// or another custom shortcut already uses (ErrComboTaken): that one would
// fire instead of ours, and a lock that silently never engages is the
// failure watch mode must not have.
func Register(spec hotkey.Spec, exe string, log *slog.Logger) (*Registration, error) {
	if log == nil {
		log = slog.Default()
	}
	accel, err := Accelerator(spec)
	if err != nil {
		return nil, err
	}
	c := client{run: gsettings}
	if busy, err := c.taken(accel); err != nil {
		return nil, err
	} else if busy {
		return nil, ErrComboTaken
	}
	if err := c.register(accel, shellQuote(exe)+" --activate"); err != nil {
		_ = c.remove()
		return nil, err
	}
	log.Debug("gnome shortcut registered", slog.String("binding", accel), slog.String("path", Path))
	return &Registration{c: c, log: log}, nil
}

// Remove deletes a leftover registration (a watch process that was
// SIGKILLed cannot clean up after itself).
func Remove() error { return client{run: gsettings}.remove() }

// Name implements state.Releaser.
func (r *Registration) Name() string { return "global-hotkey" }

// Release implements state.Releaser. Idempotent.
func (r *Registration) Release() error {
	var err error
	r.once.Do(func() { err = r.c.remove() })
	return err
}
