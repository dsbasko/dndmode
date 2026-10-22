//go:build linux

package evdev

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// grammarKeys is every key name the unlock-code grammar accepts.
var grammarKeys = []string{
	"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m",
	"n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z",
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
	"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12",
	"space", "return", "tab", "escape", "delete", "forwarddelete",
	"left", "right", "up", "down",
	"-", "=", "[", "]", ";", "'", ",", ".", "/", "\\", "`",
}

// TestKeymap_CoversGrammar: every key the config can name must be typeable,
// or an accepted unlock code could never be entered.
func TestKeymap_CoversGrammar(t *testing.T) {
	reachable := map[uint16]bool{}
	for _, mac := range macKeyCode {
		if reachable[mac] {
			t.Errorf("kVK 0x%02X is produced by two evdev codes", mac)
		}
		reachable[mac] = true
	}
	for _, name := range grammarKeys {
		spec, err := hotkey.ParseStep(name)
		if err != nil {
			t.Fatalf("ParseStep(%q): %v", name, err)
		}
		if !reachable[spec.KeyCode] {
			t.Errorf("key %q (kVK 0x%02X) has no evdev code", name, spec.KeyCode)
		}
	}
}

// TestTracker_ModifiersAndRepeat checks modifier accumulation across
// devices, auto-repeat suppression and the non-recorded keys.
func TestTracker_ModifiersAndRepeat(t *testing.T) {
	kbd, other := &device{path: "kbd"}, &device{path: "other"}
	tr := newTracker()
	noResync := func(*device) []uint16 { return nil }
	feed := func(d *device, code uint16, v int32) (matcher.KeyEvent, bool) {
		return tr.feed(rawEvent{dev: d, code: code, value: v}, noResync)
	}

	if _, ok := feed(kbd, 29, 1); ok { // left ctrl down
		t.Fatal("a modifier press must not be recorded")
	}
	if _, ok := feed(other, 125, 1); ok { // super on another device
		t.Fatal("a modifier press must not be recorded")
	}
	ke, ok := feed(kbd, 45, 1) // x
	if !ok {
		t.Fatal("x press not recorded")
	}
	want := matcher.KeyEvent{Modifiers: hotkey.ModCtrl | hotkey.ModCmd, KeyCode: 0x07}
	if ke != want {
		t.Errorf("got %+v, want %+v", ke, want)
	}
	if _, ok := feed(kbd, 45, 2); ok {
		t.Error("auto-repeat must not be recorded")
	}
	if _, ok := feed(kbd, 58, 1); ok {
		t.Error("caps lock must not be recorded")
	}
	if _, ok := feed(kbd, 0x110, 1); ok {
		t.Error("mouse button must not be recorded")
	}

	// The other device disappears: its Super must stop counting.
	tr.feed(rawEvent{dev: other, gone: true}, noResync)
	feed(kbd, 45, 0)
	ke, _ = feed(kbd, 45, 1)
	if ke.Modifiers != hotkey.ModCtrl {
		t.Errorf("modifiers after unplug = %#x, want ctrl only", ke.Modifiers)
	}

	// SYN_DROPPED resync replaces the device's state.
	tr.feed(rawEvent{dev: kbd, dropped: true}, func(*device) []uint16 { return []uint16{42} })
	ke, _ = feed(kbd, 30, 1)
	if ke.Modifiers != hotkey.ModShift || ke.KeyCode != 0x00 {
		t.Errorf("after resync got %+v, want shift+a", ke)
	}

	// An unknown key is recorded, as a code no step can equal.
	ke, ok = feed(kbd, 99, 1) // KEY_SYSRQ
	if !ok || ke.KeyCode != UnknownKeyCode {
		t.Errorf("unknown key = %+v, %v; want UnknownKeyCode recorded", ke, ok)
	}
}

func mustSeq(t *testing.T, code string) *matcher.Sequence {
	t.Helper()
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatal(err)
	}
	return matcher.NewSequence(steps)
}

func keys(t *testing.T, code string) []matcher.KeyEvent {
	t.Helper()
	steps, err := hotkey.ParseSequence(code)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]matcher.KeyEvent, len(steps))
	for i, s := range steps {
		out[i] = matcher.KeyEvent{Modifiers: s.Modifiers, KeyCode: s.KeyCode}
	}
	return out
}

func TestMatchAny_TailOnly(t *testing.T) {
	v := mustSeq(t, "s w o r d")
	hist := make([]matcher.KeyEvent, 0, historyCap)
	for _, ke := range keys(t, "q q s w o r") {
		hist = pushHistory(hist, ke)
		if matchAny(hist, v) {
			t.Fatal("matched before the code was complete")
		}
	}
	hist = pushHistory(hist, keys(t, "d")[0])
	if !matchAny(hist, v) {
		t.Fatal("complete code at the tail did not match")
	}
	hist = pushHistory(hist, keys(t, "x")[0])
	if matchAny(hist, v) {
		t.Fatal("a stray key after the code must break the tail")
	}
}

func TestPushHistory_Bounded(t *testing.T) {
	hist := make([]matcher.KeyEvent, 0, historyCap)
	for i := range historyCap * 3 {
		hist = pushHistory(hist, matcher.KeyEvent{KeyCode: uint16(i)})
	}
	if len(hist) != historyCap || hist[len(hist)-1].KeyCode != historyCap*3-1 {
		t.Fatalf("len=%d last=%d", len(hist), hist[len(hist)-1].KeyCode)
	}
}

func feedChan(evs ...matcher.KeyEvent) <-chan matcher.KeyEvent {
	ch := make(chan matcher.KeyEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	return ch
}

func TestCollectSteps(t *testing.T) {
	ret := matcher.KeyEvent{KeyCode: keyReturn}
	esc := matcher.KeyEvent{KeyCode: keyEscape}
	ctx := context.Background()

	code := keys(t, "s w ctrl+o shift+return")
	steps, err := collectSteps(ctx, feedChan(append(code, ret)...), time.Second, time.Second)
	if err != nil || len(steps) != 4 || steps[3].Modifiers != hotkey.ModShift {
		t.Fatalf("got %v, %v", steps, err)
	}

	if _, err := collectSteps(ctx, feedChan(code[0], esc), time.Second, time.Second); !errors.Is(err, ErrCaptureCancelled) {
		t.Errorf("escape: %v", err)
	}
	unk := matcher.KeyEvent{KeyCode: UnknownKeyCode}
	if _, err := collectSteps(ctx, feedChan(unk), time.Second, time.Second); !errors.Is(err, ErrCaptureUnsupportedKey) {
		t.Errorf("unknown key: %v", err)
	}
	long := make([]matcher.KeyEvent, hotkey.MaxSteps+1)
	for i := range long {
		long[i] = keys(t, "a")[0]
	}
	if _, err := collectSteps(ctx, feedChan(long...), time.Second, time.Second); !errors.Is(err, ErrCaptureTooLong) {
		t.Errorf("too long: %v", err)
	}
	if _, err := collectSteps(ctx, feedChan(), 20*time.Millisecond, time.Second); !errors.Is(err, ErrCaptureTimedOut) {
		t.Errorf("idle: %v", err)
	}
}

func TestConfirmPasses(t *testing.T) {
	ret := matcher.KeyEvent{KeyCode: keyReturn}
	code := keys(t, "s w o r d f")
	ctx := context.Background()

	in := append(append(append([]matcher.KeyEvent{}, code...), ret), append(code, ret)...)
	var prompts []int
	steps, err := confirmPasses(ctx, feedChan(in...), func(p int) { prompts = append(prompts, p) }, time.Second, time.Second)
	if err != nil || len(steps) != len(code) || len(prompts) != 2 {
		t.Fatalf("got %v, %v, prompts %v", steps, err, prompts)
	}

	other := keys(t, "s w o r d g")
	in = append(append(append([]matcher.KeyEvent{}, code...), ret), append(other, ret)...)
	if _, err := confirmPasses(ctx, feedChan(in...), nil, time.Second, time.Second); !errors.Is(err, ErrCaptureMismatch) {
		t.Errorf("mismatch: %v", err)
	}
}
