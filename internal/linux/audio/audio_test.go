//go:build linux

package audio

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeRun answers with out for every call and records the argv.
func fakeRun(out string, err error, calls *[]string) runFunc {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		return []byte(out), err
	}
}

func TestGetMuted(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		tool, out string
		want      bool
		wantErr   bool
	}{
		{"wpctl", "Volume: 0.40\n", false, false},
		{"wpctl", "Volume: 0.40 [MUTED]\n", true, false},
		{"wpctl", "garbage", false, true},
		{"pactl", "Mute: yes\n", true, false},
		{"pactl", "Mute: no\n", false, false},
		{"pactl", "Stummschaltung: ja", false, true},
	}
	for _, c := range cases {
		var calls []string
		r := execRunner{tool: c.tool, run: fakeRun(c.out, nil, &calls)}
		got, err := r.GetMuted(ctx)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s %q: got %v, %v", c.tool, c.out, got, err)
		}
	}
	if _, err := (execRunner{}).GetMuted(ctx); !errors.Is(err, ErrNoMixer) {
		t.Errorf("no tool: %v", err)
	}
}

func TestSetMuted(t *testing.T) {
	var calls []string
	r := execRunner{tool: "wpctl", run: fakeRun("", nil, &calls)}
	_ = r.SetMuted(context.Background(), true)
	_ = r.SetMuted(context.Background(), false)
	want := []string{"wpctl set-mute @DEFAULT_AUDIO_SINK@ 1", "wpctl set-mute @DEFAULT_AUDIO_SINK@ 0"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}
}

func TestReleaser(t *testing.T) {
	var calls []string
	r := execRunner{tool: "wpctl", run: fakeRun("", nil, &calls)}

	rel := NewReleaser(r, false, nil)
	_ = rel.Release()
	_ = rel.Release()
	if len(calls) != 1 || !strings.HasSuffix(calls[0], " 0") {
		t.Errorf("prior unmuted: calls %v, want one unmute", calls)
	}

	calls = nil
	_ = NewReleaser(r, true, nil).Release()
	if len(calls) != 0 {
		t.Errorf("prior muted: calls %v, want none", calls)
	}
}
