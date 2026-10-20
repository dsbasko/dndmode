//go:build linux

package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeGSettings keeps show-banners in memory.
type fakeGSettings struct {
	value string
	fail  bool
	calls []string
}

func (f *fakeGSettings) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.fail {
		return nil, errors.New("no schema")
	}
	if args[0] == "set" {
		f.value = args[3]
		return nil, nil
	}
	return []byte(f.value + "\n"), nil
}

func TestActivateRelease_RestoresPrior(t *testing.T) {
	ctx := context.Background()
	for _, prior := range []string{"true", "false"} {
		f := &fakeGSettings{value: prior}
		r := &Runner{run: f.run}
		was, err := Activate(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if f.value != "false" {
			t.Errorf("after Activate show-banners = %s", f.value)
		}
		rel := NewReleaser(r, was, nil)
		_ = rel.Release()
		_ = rel.Release()
		if f.value != prior {
			t.Errorf("after Release show-banners = %s, want %s", f.value, prior)
		}
	}
}

func TestCheck_Unavailable(t *testing.T) {
	r := &Runner{run: (&fakeGSettings{fail: true}).run}
	if err := Check(context.Background(), r); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestRun_RecoveryNames(t *testing.T) {
	f := &fakeGSettings{value: "true"}
	r := &Runner{run: f.run}
	_ = r.Run(context.Background(), On)
	_ = r.Run(context.Background(), Off)
	want := []string{
		"gsettings set org.gnome.desktop.notifications show-banners false",
		"gsettings set org.gnome.desktop.notifications show-banners true",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %v", f.calls)
	}
	if err := r.Run(context.Background(), "bogus"); err == nil {
		t.Error("unknown toggle accepted")
	}
}
