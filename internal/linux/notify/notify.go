//go:build linux

// Package notify hides GNOME notification banners for the session
// (Settings → Notifications → Do Not Disturb) by flipping
// org.gnome.desktop.notifications show-banners, and restores the previous
// value on exit.
//
// It runs on every session, not only with focus: true. GNOME Shell draws
// banners above every client window, and while it holds ordinary ones back
// over a fullscreen window, "“…” is ready" banners (an app window asking
// for attention, as messengers do on a new message) skip that check. The
// macOS shield sits above Notification Center, so hiding them is the Linux
// side of the same guarantee. Critical banners still show: GNOME exempts
// them from show-banners too.
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

// Shown reports whether banners are shown now, that is whether a session
// has to hide them. An unreadable setting (not a GNOME session) gives
// false: there is nothing to hide or to restore.
func Shown(ctx context.Context, r *Runner) bool {
	shown, err := r.Banners(ctx)
	return err == nil && shown
}

// Hide turns banners off and returns the Releaser that turns them back on.
// A failed write is only logged: the shield still covers everything else.
func Hide(ctx context.Context, r *Runner, log *slog.Logger) *Releaser {
	rel := NewReleaser(r, true, log)
	if err := r.SetBanners(ctx, false); err != nil {
		rel.log.Warn("hide notification banners failed", slog.Any("err", err))
	}
	return rel
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
