//go:build linux

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dsbasko/dndmode/internal/linux/gnomekeys"
	watchpkg "github.com/dsbasko/dndmode/internal/state/watch"
)

const (
	// activateTokenRelPath carries the xdg-activation token from the
	// `--activate` command GNOME runs to the watch process. A token lets
	// the compositor raise the shield above the focused window instead of
	// refusing it as focus stealing.
	activateTokenRelPath = ".config/dndmode/activate.token"

	// activateTokenTTL: a token older than this belongs to an earlier press.
	activateTokenTTL = 10 * time.Second

	// inputGroupHint is the one fix for every evdev permission failure.
	inputGroupHint = "Run: sudo usermod -aG input $USER — then log out and back in."
)

// runActivate is `dndmode --activate`, the command the GNOME shortcut runs:
// it tells the watch process to raise a session (SIGUSR1) and exits. It
// starts nothing itself. With no watch process running it removes the
// leftover shortcut (a watch process that was SIGKILLed could not) and
// exits exitNotWatching.
func runActivate(outW, errW io.Writer, log *slog.Logger) int {
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: resolve home dir: %v.\n", err)
		return exitPlatformErr
	}
	mgr := watchpkg.NewManager(filepath.Join(home, watchRecordRelPath), log)
	st, err := watchpkg.Inspect(mgr, watchpkg.NewKernProber())
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot tell whether a watch process is running: %v.\n", err)
		return exitRuntimeJSON
	}
	if !st.Running {
		if rerr := gnomekeys.Remove(); rerr != nil {
			log.Debug("remove leftover shortcut", slog.Any("err", rerr))
		}
		_, _ = fmt.Fprintln(outW, "dndmode: not watching — removed the leftover shortcut. Start it with `dndmode --watch`.")
		return exitNotWatching
	}
	if tok := os.Getenv("XDG_ACTIVATION_TOKEN"); tok != "" {
		path := filepath.Join(home, activateTokenRelPath)
		if werr := os.WriteFile(path, []byte(tok), 0o600); werr != nil {
			log.Debug("write activation token", slog.Any("err", werr))
		}
	}
	if err := syscall.Kill(st.Record.PID, syscall.SIGUSR1); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot signal the watch process (PID %d): %v\n", st.Record.PID, err)
		return exitPlatformErr
	}
	return exitOK
}

// linuxComboLabel respells a combination in the names a Linux keyboard
// carries: option → Alt, cmd → Super. The config grammar keeps the macOS
// names so one config works on both platforms; only what is printed changes.
func linuxComboLabel(s string) string {
	parts := strings.Split(s, "+")
	for i, p := range parts {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "option":
			parts[i] = "Alt"
		case "cmd":
			parts[i] = "Super"
		case "ctrl":
			parts[i] = "Ctrl"
		case "shift":
			parts[i] = "Shift"
		}
	}
	return strings.Join(parts, "+")
}

// takeActivationToken returns and removes a fresh token left by
// runActivate, or "".
func takeActivationToken(dir string) string {
	path := filepath.Join(dir, filepath.Base(activateTokenRelPath))
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil || time.Since(fi.ModTime()) > activateTokenTTL {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// activationPresses turns SIGUSR1 into presses. The channel holds one
// pending press: a second press while a session is starting is the same
// request, not a queued second shield.
func activationPresses() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	presses := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				select {
				case presses <- struct{}{}:
				default:
				}
			}
		}
	}()
	return presses, func() {
		signal.Stop(sig)
		close(done)
	}
}
