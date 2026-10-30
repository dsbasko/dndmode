//go:build linux

// Package notify is the Linux "focus" option: it hides GNOME notification
// banners for the session (Settings → Notifications → Do Not Disturb) by
// flipping org.gnome.desktop.notifications show-banners, and restores the
// previous value on exit.
//
// Unlike the macOS Focus it needs no setup (no Shortcuts) and does not sync
// to other devices, but it is still opt-in (focus: true) to keep the two
// platforms' defaults identical.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	schema = "org.gnome.desktop.notifications"
	key    = "show-banners"

	// releaseTimeout bounds the restore on Cleanup; it runs on a fresh
	// context because the session's is already cancelled by then.
	releaseTimeout = 5 * time.Second
)

// Names accepted by Run, matching the macOS Shortcuts so crash recovery can
// call the same thing on both platforms.
const (
	On  = "dndmode-on"
	Off = "dndmode-off"
)

// ErrUnavailable: the GNOME notification setting cannot be read — not a
// GNOME session, or gsettings missing.
var ErrUnavailable = errors.New("notify: GNOME notification settings are not available")

// runFunc runs a command and returns its stdout; injectable for tests.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Runner reads and writes show-banners through gsettings.
type Runner struct {
	run runFunc
}

// NewRunner returns the gsettings-backed Runner.
func NewRunner() *Runner { return &Runner{run: execRun} }

// Banners reports whether notification banners are currently shown.
func (r *Runner) Banners(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "gsettings", "get", schema, key)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("%w: unexpected value %q", ErrUnavailable, out)
}

// SetBanners shows or hides notification banners.
func (r *Runner) SetBanners(ctx context.Context, show bool) error {
	_, err := r.run(ctx, "gsettings", "set", schema, key, fmt.Sprint(show))
	return err
}

// Run implements the recovery seam (runtime.FocusRunner): "dndmode-on"
// hides banners, "dndmode-off" shows them.
func (r *Runner) Run(ctx context.Context, name string) error {
	switch name {
	case On:
		return r.SetBanners(ctx, false)
	case Off:
		return r.SetBanners(ctx, true)
	}
	return fmt.Errorf("notify: unknown toggle %q", name)
}

// Check is the pre-flight: the setting must be readable before a session
// relies on it.
func Check(ctx context.Context, r *Runner) error {
	_, err := r.Banners(ctx)
	return err
}

// Activate hides banners and returns whether they were shown before, for
// the Releaser to restore.
func Activate(ctx context.Context, r *Runner) (prior bool, err error) {
	prior, err = r.Banners(ctx)
	if err != nil {
		return true, err
	}
	return prior, r.SetBanners(ctx, false)
}

// Releaser restores the banner setting on Cleanup. It satisfies
// state.Releaser.
type Releaser struct {
	r     *Runner
	prior bool
	log   *slog.Logger
	once  sync.Once
}

// NewReleaser returns a Releaser that sets show-banners back to prior.
func NewReleaser(r *Runner, prior bool, log *slog.Logger) *Releaser {
	if log == nil {
		log = slog.Default()
	}
	return &Releaser{r: r, prior: prior, log: log}
}

// Name implements state.Releaser.
func (rel *Releaser) Name() string { return "focus" }

// Release implements state.Releaser. Best-effort and idempotent.
func (rel *Releaser) Release() error {
	rel.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if err := rel.r.SetBanners(ctx, rel.prior); err != nil {
			rel.log.Warn("restore notification banners failed", slog.Any("err", err))
		}
	})
	return nil
}
