//go:build linux

// Package audio mutes the default output for a session and restores it on
// exit — the Linux counterpart of macos/audiomute. It drives PipeWire's
// wpctl (Ubuntu 24.04's default) and falls back to pactl, which also works
// against PipeWire's PulseAudio shim.
//
// Like the macOS package it is state-aware: the session records whether the
// output was already muted and the Releaser never unmutes what dndmode did
// not mute.
package audio

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

// releaseTimeout bounds the unmute on Cleanup. It runs on a fresh context:
// the session's is already cancelled by then.
const releaseTimeout = 5 * time.Second

// ErrNoMixer: neither wpctl nor pactl is installed.
var ErrNoMixer = errors.New("audio: neither wpctl nor pactl is installed")

// VolumeRunner reads and sets the mute state of the default output. It has
// the same method set as macos/audiomute.VolumeRunner.
type VolumeRunner interface {
	GetMuted(ctx context.Context) (bool, error)
	SetMuted(ctx context.Context, muted bool) error
}

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

type execRunner struct {
	tool string // "wpctl" or "pactl"
	run  runFunc
}

// NewExecRunner picks wpctl, else pactl. With neither installed every call
// fails with ErrNoMixer, which the session treats like any other mute
// failure: warn and leave the volume alone.
func NewExecRunner() VolumeRunner {
	for _, tool := range []string{"wpctl", "pactl"} {
		if _, err := exec.LookPath(tool); err == nil {
			return execRunner{tool: tool, run: execRun}
		}
	}
	return execRunner{}
}

// GetMuted implements VolumeRunner.
func (r execRunner) GetMuted(ctx context.Context) (bool, error) {
	switch r.tool {
	case "wpctl":
		out, err := r.run(ctx, "wpctl", "get-volume", "@DEFAULT_AUDIO_SINK@")
		if err != nil {
			return false, err
		}
		// "Volume: 0.40" or "Volume: 0.40 [MUTED]".
		if !bytes.HasPrefix(out, []byte("Volume:")) {
			return false, fmt.Errorf("wpctl: unexpected output %q", out)
		}
		return bytes.Contains(out, []byte("[MUTED]")), nil
	case "pactl":
		out, err := r.run(ctx, "pactl", "get-sink-mute", "@DEFAULT_SINK@")
		if err != nil {
			return false, err
		}
		// "Mute: yes" / "Mute: no" — pactl localises nothing here.
		s := strings.TrimSpace(string(out))
		switch s {
		case "Mute: yes":
			return true, nil
		case "Mute: no":
			return false, nil
		}
		return false, fmt.Errorf("pactl: unexpected output %q", s)
	}
	return false, ErrNoMixer
}

// SetMuted implements VolumeRunner.
func (r execRunner) SetMuted(ctx context.Context, muted bool) error {
	v := "0"
	if muted {
		v = "1"
	}
	var err error
	switch r.tool {
	case "wpctl":
		_, err = r.run(ctx, "wpctl", "set-mute", "@DEFAULT_AUDIO_SINK@", v)
	case "pactl":
		_, err = r.run(ctx, "pactl", "set-sink-mute", "@DEFAULT_SINK@", v)
	default:
		err = ErrNoMixer
	}
	return err
}

// Releaser restores the output on Cleanup. It satisfies state.Releaser.
type Releaser struct {
	runner     VolumeRunner
	priorMuted bool
	log        *slog.Logger
	mu         sync.Mutex
	done       bool
}

// NewReleaser returns a Releaser that unmutes on Release unless the output
// was already muted before the session (priorMuted).
func NewReleaser(runner VolumeRunner, priorMuted bool, log *slog.Logger) *Releaser {
	if log == nil {
		log = slog.Default()
	}
	return &Releaser{runner: runner, priorMuted: priorMuted, log: log}
}

// Name implements state.Releaser.
func (r *Releaser) Name() string { return "audio" }

// Release implements state.Releaser. Best-effort: a failure is logged, not
// returned, so it never blocks the rest of the teardown. Idempotent.
func (r *Releaser) Release() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil
	}
	r.done = true
	if r.priorMuted {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := r.runner.SetMuted(ctx, false); err != nil {
		r.log.Warn("restore system audio failed", slog.Any("err", err))
	}
	return nil
}
