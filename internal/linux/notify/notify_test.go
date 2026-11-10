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

func TestHideRelease_TurnsBannersBackOn(t *testing.T) {
	ctx := context.Background()
	f := &fakeGSettings{value: "true"}
	r := &Runner{run: f.run}
	if !Shown(ctx, r) {
		t.Fatal("Shown = false with banners on")
	}
	rel := Hide(ctx, r, nil)
	if f.value != "false" {
		t.Errorf("after Hide show-banners = %s", f.value)
	}
	_ = rel.Release()
	_ = rel.Release()
	if f.value != "true" {
		t.Errorf("after Release show-banners = %s, want true", f.value)
	}
}

func TestShown_NothingToHide(t *testing.T) {
	ctx := context.Background()
	if Shown(ctx, &Runner{run: (&fakeGSettings{value: "false"}).run}) {
		t.Error("Shown = true with banners already off")
	}
	if Shown(ctx, &Runner{run: (&fakeGSettings{fail: true}).run}) {
		t.Error("Shown = true without GNOME settings")
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
