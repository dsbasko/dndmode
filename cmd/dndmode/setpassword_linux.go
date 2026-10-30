//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/dsbasko/dndmode/internal/linux/evdev"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
)

// captureFailure maps an evdev capture error onto the line to print and the
// exit code to return. Input outcomes are exitConfigErr — the config was not
// touched and the old code still works; the device outcomes keep the codes
// a session would return for the same failure.
func captureFailure(err error) (string, int) {
	switch {
	case errors.Is(err, evdev.ErrCaptureCancelled):
		return "cancelled — the config was not changed.", exitConfigErr
	case errors.Is(err, evdev.ErrCaptureMismatch):
		return "the two entries did not match — the config was not changed. Re-run to try again.", exitConfigErr
	case errors.Is(err, evdev.ErrCaptureTooLong):
		return "that unlock code has too many steps — the config was not changed.", exitConfigErr
	case errors.Is(err, evdev.ErrCaptureUnsupportedKey):
		return "a key that cannot be part of an unlock code was pressed — the config was not changed.", exitConfigErr
	case errors.Is(err, evdev.ErrCaptureTimedOut):
		return "timed out waiting for the unlock code — the config was not changed.", exitConfigErr
	case errors.Is(err, context.Canceled):
		return "aborted — the config was not changed.", exitConfigErr
	case errors.Is(err, evdev.ErrPermission):
		return fmt.Sprintf("%v — nothing was captured. %s", err, inputGroupHint), exitPermissionDenied
	case errors.Is(err, evdev.ErrBusy):
		return fmt.Sprintf("%v — nothing was captured. Close the program holding it, then re-run.", err), exitSecureInputConflict
	default:
		return fmt.Sprintf("capturing the unlock code failed: %v", err), exitPlatformErr
	}
}

// runSetPassword is the Linux --set-password branch. Same contract as on
// macOS (see setpassword.go): capture twice from real keystrokes, store a
// salted hash, start no session. The Accessibility wait becomes an access
// check on /dev/input, and Secure Event Input has no Linux counterpart.
func runSetPassword(ctx context.Context, fl setPasswordFlags, outW, errW io.Writer, debugOn *bool, log *slog.Logger) int {
	if name := fl.conflictingFlag(); name != "" {
		_, _ = fmt.Fprintf(errW,
			"dndmode: --set-password cannot be combined with %s — it rewrites the config and exits without starting a session.\n",
			name)
		return exitConfigErr
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: resolve home dir: %v.\n", err)
		return exitPlatformErr
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		_, _ = fmt.Fprintln(errW,
			"dndmode: --set-password is interactive — its prompts must reach a terminal, so stdout cannot be redirected to a file or a pipe.")
		return exitConfigErr
	}
	return runSetPasswordAt(ctx, filepath.Join(home, configRelPath), int(os.Stdin.Fd()), outW, errW, debugOn, log)
}

// runSetPasswordAt is runSetPassword with the config path and terminal fd
// injected. The ordering and every refusal mirror the macOS implementation;
// its comments explain each step.
func runSetPasswordAt(ctx context.Context, cfgPath string, ttyFD int, outW, errW io.Writer, debugOn *bool, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}

	loader, cfg, created, err := prepareConfigForCapture(cfgPath)
	if cfg.Debug {
		*debugOn = true
	}
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v\n", err)
		return exitConfigErr
	}
	if created {
		_, _ = fmt.Fprintf(errW, "dndmode: created default config at %s\n", cfgPath)
	}

	// A live session owns the input grab; a capture beside it would fail
	// with EBUSY at best — refuse before prompting.
	runtimeMgr := runtimepkg.NewManager(filepath.Join(filepath.Dir(cfgPath), filepath.Base(runtimeRelPath)), log)
	if detail, code := probeLivePeer(runtimeMgr, log); code != exitOK {
		_, _ = fmt.Fprintf(errW, "dndmode: %s. %s\n", detail, peerBeforeCapture)
		return code
	}

	if !term.IsTerminal(ttyFD) {
		_, _ = fmt.Fprintln(errW,
			"dndmode: --set-password is interactive — run it from a terminal, not from a pipe or a script.")
		return exitConfigErr
	}

	if err := evdev.CheckAccess(); err != nil {
		if errors.Is(err, evdev.ErrPermission) {
			_, _ = fmt.Fprintf(errW, "dndmode: %v. %s\n", err, inputGroupHint)
			return exitPermissionDenied
		}
		_, _ = fmt.Fprintf(errW, "dndmode: input devices unavailable: %v.\n", err)
		return exitPlatformErr
	}

	// The reservation: held from before the grab through the save, so a
	// session that starts meanwhile refuses at its publish step.
	releaseLock, lockErr := acquirePublishLock(filepath.Dir(cfgPath))
	defer releaseLock()
	if lockErr != nil {
		_, _ = fmt.Fprintf(errW,
			"dndmode: cannot claim the publish lock, so another dndmode cannot be ruled out: %v. %s\n",
			lockErr, lockBeforeCapture)
		return exitRuntimeJSON
	}
	if detail, code := probeLivePeer(runtimeMgr, log); code != exitOK {
		_, _ = fmt.Fprintf(errW, "dndmode: %s. %s\n", detail, peerBeforeCapture)
		return code
	}

	// Raw mode for the whole branch, restored with TCSETSF (tcsetattr +
	// TCSAFLUSH) so nothing typed around the grab is left in the input queue.
	old, err := unix.IoctlGetTermios(ttyFD, unix.TCGETS)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot read terminal state: %v.\n", err)
		return exitConfigErr
	}
	if _, err := term.MakeRaw(ttyFD); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: cannot switch the terminal to raw mode: %v.\n", err)
		return exitConfigErr
	}
	restore := func() { _ = unix.IoctlSetTermios(ttyFD, unix.TCSETSF, old) }

	// kill / SIGHUP during the capture: cancel it and put the terminal back.
	// commitMu makes "cancelled" and "saved" mutually exclusive.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	capCtx, capCancel := context.WithCancel(ctx)
	var commitMu sync.Mutex
	go func() {
		select {
		case <-sigCh:
			commitMu.Lock()
			capCancel()
			commitMu.Unlock()
			restore()
		case <-capCtx.Done():
		}
	}()
	defer func() {
		restore()
		capCancel()
		signal.Stop(sigCh)
	}()

	steps, err := evdev.CaptureConfirmed(capCtx, setPasswordPrompt(outW), log)
	if err != nil {
		msg, code := captureFailure(err)
		_, _ = fmt.Fprintf(errW, "dndmode: %s\r\n", msg)
		return code
	}
	defer func() { clear(steps) }()

	if verr := validateCapturedCode(steps); verr != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v.\r\n", verr)
		return exitConfigErr
	}

	// Last look for a session that predates the publish lock.
	if detail, code := probeLivePeer(runtimeMgr, log); code != exitOK {
		_, _ = fmt.Fprintf(errW, "dndmode: %s. %s\r\n", detail, peerAfterCapture)
		return code
	}

	commitMu.Lock()
	capErr := capCtx.Err()
	var serr error
	if capErr == nil {
		serr = loader.SaveUnlockHash(steps)
	}
	commitMu.Unlock()
	if capErr != nil {
		msg, code := captureFailure(capErr)
		_, _ = fmt.Fprintf(errW, "dndmode: %s\r\n", msg)
		return code
	}
	if serr != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v\r\n", serr)
		return exitConfigErr
	}
	_, _ = fmt.Fprintf(outW, resultUpdated, cfgPath)
	return exitOK
}
