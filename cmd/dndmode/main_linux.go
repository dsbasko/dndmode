//go:build linux

// Linux entry point. The command line, config, exit codes, watch daemon,
// --status / --kill and --set-password contracts are the macOS ones (see
// main.go for the full rationale of every step); only the platform layer
// differs:
//
//	shield      GTK4 window per monitor           (internal/linux/overlay)
//	input lock  EVIOCGRAB on /dev/input/event*    (internal/linux/evdev)
//	awake lock  systemd-inhibit + gnome-session   (internal/linux/inhibit)
//	mute        wpctl / pactl                     (internal/linux/audio)
//	focus       GNOME show-banners                (internal/linux/notify)
//	watch key   GNOME custom shortcut + SIGUSR1   (internal/linux/gnomekeys)
//
// Only overlay_style black, dvd, matrix, terminal and none are available.
package main

import (
	// Pins the main goroutine to thread 0 for GTK. Must stay first.
	_ "github.com/dsbasko/dndmode/internal/runtimepin"

	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/linux/audio"
	"github.com/dsbasko/dndmode/internal/linux/evdev"
	"github.com/dsbasko/dndmode/internal/linux/gnomekeys"
	"github.com/dsbasko/dndmode/internal/linux/inhibit"
	"github.com/dsbasko/dndmode/internal/linux/notify"
	"github.com/dsbasko/dndmode/internal/linux/overlay"
	"github.com/dsbasko/dndmode/internal/state"
	runtimepkg "github.com/dsbasko/dndmode/internal/state/runtime"
	watchpkg "github.com/dsbasko/dndmode/internal/state/watch"
	"github.com/dsbasko/dndmode/internal/supervisor"
)

// run is the Linux counterpart of main.go's run(). Step numbers match the
// macOS ones so the two can be read side by side.
func run() int {
	var debugOn bool
	outW := &gatedWriter{w: os.Stdout, on: &debugOn}
	errW := &gatedWriter{w: os.Stderr, on: &debugOn}

	// Registered first so it runs last, after Cleanup.
	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: PANIC: %v\n%s\n", r, debug.Stack())
			os.Exit(exitInternalErr)
		}
	}()

	// Signal handlers must outlive the cleanup chain (see main.go).
	var stopSignals func()
	defer func() {
		if stopSignals != nil {
			stopSignals()
		}
	}()

	// --- Step 1: flags ---
	fl := defineFlags(flag.CommandLine)
	// --activate is Linux-only: the command the GNOME shortcut runs.
	activate := flag.Bool("activate", false, "internal: sent by the GNOME shortcut to the --watch process")
	flag.Usage = printUsage
	flag.Parse()
	debugOn = *fl.debug

	// --- Step 2: logger ---
	log := slog.New(slog.NewTextHandler(errW, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// --- Step 2.5 / 2.6 / 2.65: one-shot commands ---
	if *fl.setPassword {
		return runSetPassword(context.Background(), setPasswordFlags{
			style:  *fl.style,
			timer:  *fl.timer,
			mute:   *fl.mute,
			focus:  *fl.focus,
			watch:  *fl.watch,
			kill:   *fl.kill,
			status: *fl.status,
		}, os.Stdout, errW, &debugOn, log)
	}
	if *fl.kill || *fl.status {
		return runWatchControl(controlFlags{
			style:       *fl.style,
			timer:       *fl.timer,
			mute:        *fl.mute,
			focus:       *fl.focus,
			watch:       *fl.watch,
			setPassword: *fl.setPassword,
			kill:        *fl.kill,
			status:      *fl.status,
		}, os.Stdout, os.Stderr, log)
	}
	if *activate {
		return runActivate(os.Stdout, os.Stderr, log)
	}

	// --- Step 2.7: --watch launcher ---
	var readyPipe *os.File
	if *fl.watch {
		pipe, isDaemon, perr := daemonReadyPipe()
		if perr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "dndmode: %v\n", perr)
			return exitPlatformErr
		}
		if !isDaemon {
			return spawnWatchDaemon(os.Stderr)
		}
		readyPipe = pipe
	}

	// --- Step 3: RestoreState ---
	rs := state.NewRestoreState(log)
	defer func() {
		if err := rs.Cleanup(); err != nil {
			log.Error("cleanup returned aggregated error", slog.Any("err", err))
		}
		_, _ = fmt.Fprintln(outW, "dndmode: cleaning up… done.")
	}()

	// --- Step 4-5: config ---
	home, err := os.UserHomeDir()
	if err != nil {
		log.Error("resolve home dir", slog.Any("err", err))
		return exitPlatformErr
	}
	cfgPath := filepath.Join(home, configRelPath)
	loader := config.NewLoader(cfgPath)
	cfg, cfgRaw, created, err := loader.LoadWithSource()
	if err != nil {
		_, _ = fmt.Fprintln(errW, err)
		return exitConfigErr
	}
	if cfg.Debug {
		debugOn = true
	}

	// --- Step 5.9: bring an older config.yml up to date ---
	if !created {
		if release, lockErr := acquirePublishLock(filepath.Dir(cfgPath)); lockErr == nil {
			mig, migErr := config.MigrateFile(cfgPath)
			release()
			switch {
			case migErr != nil:
				log.Debug("config migration skipped", slog.Any("err", migErr))
			case mig.Changed():
				if newCfg, newRaw, _, rerr := loader.LoadWithSource(); rerr != nil {
					log.Debug("re-read after migration failed; continuing with the loaded config", slog.Any("err", rerr))
				} else {
					cfg, cfgRaw = newCfg, newRaw
				}
			}
		} else {
			log.Debug("config migration skipped: publish lock unavailable", slog.Any("err", lockErr))
		}
	}

	// --- Step 5a: fingerprint ---
	cfgFingerprint := configFingerprintOf(cfgRaw)

	// --- Step 5b: unlock code ---
	unlockVerifier, unlockSource, unlockWeak, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: %v. Fix %s.\n", err, cfgPath)
		return exitConfigErr
	}

	// --- Step 5b.1: overlay style ---
	overlayStyle := cfg.OverlayStyle
	styleSource := "config"
	var langOverride string // set only by a --style terminal:<lang> suffix
	if *fl.style != "" {
		base, _, lo, perr := parseStyleFlag(*fl.style)
		if perr != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: invalid --style %q: %v.\n", *fl.style, perr)
			return exitConfigErr
		}
		overlayStyle = base
		langOverride = lo
		styleSource = "flag"
	}
	if err := config.ValidateOverlayStyle(overlayStyle); err != nil {
		if styleSource == "flag" {
			_, _ = fmt.Fprintf(errW, "dndmode: invalid --style %q: %v.\n", overlayStyle, err)
		} else {
			_, _ = fmt.Fprintf(errW, "dndmode: invalid overlay_style %q: %v. Fix overlay_style in %s (valid on Linux: black, dvd, matrix, terminal, none).\n", overlayStyle, err, cfgPath)
		}
		return exitConfigErr
	}
	overlayStyle = config.NormalizeOverlayStyle(overlayStyle)
	if overlayStyle != config.OverlayStyleNone && !slices.Contains(overlay.Styles, overlayStyle) {
		src := "overlay_style in " + cfgPath
		if styleSource == "flag" {
			src = "--style"
		}
		_, _ = fmt.Fprintf(errW, "dndmode: overlay style %q is not available on Linux (use black, dvd, matrix, terminal or none). Fix %s.\n", overlayStyle, src)
		return exitConfigErr
	}

	// Terminal source language, resolved like on macOS: the --style suffix
	// wins, then terminal_language from the config (validated here so a
	// typo fails fast), default go.
	terminalLanguage := cfg.TerminalLanguage
	if langOverride != "" {
		terminalLanguage = langOverride
	} else if err := config.ValidateTerminalLanguage(terminalLanguage); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid terminal_language %q: %v. Fix terminal_language in %s.\n", terminalLanguage, err, cfgPath)
		return exitConfigErr
	}
	terminalLanguage = config.NormalizeTerminalLanguage(terminalLanguage)

	// --- Step 5b.2: timer ---
	timerDur, err := parseTimer(*fl.timer)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid --timer %q: %v.\n", *fl.timer, err)
		return exitConfigErr
	}

	// --- Step 5b.3: activation combination (--watch) ---
	var activateSpec hotkey.Spec
	if *fl.watch {
		activateSpec, err = config.ResolveActivateHotkey(&cfg, unlockVerifier)
		if err != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: %v. Fix %s.\n", err, cfgPath)
			return exitConfigErr
		}
		if overlayStyle == config.OverlayStyleNone {
			if styleSource == "flag" {
				_, _ = fmt.Fprintln(errW, "dndmode: --watch cannot be combined with --style none: awake-only mode has nothing to lock on a keypress.")
			} else {
				_, _ = fmt.Fprintf(errW, "dndmode: --watch cannot be used with overlay_style none: awake-only mode has nothing to lock on a keypress. Change overlay_style in %s or pass --style.\n", cfgPath)
			}
			return exitConfigErr
		}
	}

	// --- Step 6: banner ---
	if created {
		_, _ = fmt.Fprintf(outW, "dndmode: created default config at %s\n", cfgPath)
	}
	_, _ = fmt.Fprintf(outW, "dndmode: config=%s unlock_code=ok (source=%s) overlay_style=%s (%s)\n",
		cfgPath, unlockSource, overlayStyle, styleSource)
	if unlockSource == config.UnlockSourceHotkey {
		_, _ = fmt.Fprintf(outW,
			"dndmode: the 'hotkey' config key is deprecated — rename it to 'unlock_code' (the same value is a 1-step code).\n")
	}
	if unlockWeak {
		_, _ = fmt.Fprintf(outW,
			"dndmode: warning — the unlock code is short; %d steps or more is strongly recommended (every keypress is a fresh match attempt).\n",
			config.WeakUnlockSteps)
	}
	if overlayStyle == config.OverlayStyleTerminal {
		_, _ = fmt.Fprintf(outW, "dndmode: terminal_language=%s\n", terminalLanguage)
	}
	if timerDur > 0 {
		_, _ = fmt.Fprintf(outW, "dndmode: timer=%s — auto-disable after this long\n", timerDur)
	}

	// --- Step 3 (signals): cancel is the shutdown request, stop only
	// unregisters (deferred at the top) ---
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, stop := signal.NotifyContext(rootCtx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stopSignals = stop

	// --- Step 8: platform — a shield needs a graphical session ---
	if overlayStyle != config.OverlayStyleNone && os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "" {
		_, _ = fmt.Fprintln(errW, "dndmode: no graphical session (neither WAYLAND_DISPLAY nor DISPLAY is set). Run it from the desktop session, or use --style none.")
		return exitPlatformErr
	}

	// --- Step 5d: one watch process at a time ---
	watchMgr := watchpkg.NewManager(filepath.Join(home, watchRecordRelPath), log)
	if *fl.watch {
		if st, werr := watchpkg.Inspect(watchMgr, watchpkg.NewKernProber()); werr != nil {
			log.Warn("watch pre-check inconclusive", slog.Any("err", werr))
		} else if st.Running {
			_, _ = fmt.Fprintf(os.Stderr,
				"dndmode: already watching (PID %d). Run `dndmode --kill` to stop it first, or `dndmode --status` to see it.\n",
				st.Record.PID)
			return exitConcurrentInstance
		}
	}

	// --- Step 8a: none — awake-only ---
	if overlayStyle == config.OverlayStyleNone {
		return runInhibitOnly(ctx, cancel, cfg.AllowDisplaySleep, timerDur, rs, log, outW, errW)
	}

	// --- Step 8b: mute / focus ---
	effectiveMute, err := resolveBoolFlag(*fl.mute, config.NormalizeMute(cfg.Mute))
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid --mute %q: %v.\n", *fl.mute, err)
		return exitConfigErr
	}
	effectiveFocus, err := resolveBoolFlag(*fl.focus, cfg.Focus)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: invalid --focus %q: %v.\n", *fl.focus, err)
		return exitConfigErr
	}

	// --- Step 5c: single instance ---
	runtimeMgr := runtimepkg.NewManager(filepath.Join(home, runtimeRelPath), log)
	liveChecker := runtimepkg.NewKernLiveChecker()
	if alive, peerPID, err := runtimepkg.IsLiveInstance(runtimeMgr, liveChecker, log); err != nil {
		log.Warn("pre-check inconclusive", slog.Any("err", err))
	} else if alive {
		_, _ = fmt.Fprintf(errW,
			"dndmode: another instance is already active (PID=%d). Send SIGTERM or wait for its exit, then re-run.\n",
			peerPID)
		return exitConcurrentInstance
	}

	// --- Step 9: access to /dev/input ---
	// The Accessibility wait has no Linux equivalent worth waiting on: group
	// membership only takes effect at the next login, so say how and exit.
	if err := evdev.CheckAccess(); err != nil {
		if errors.Is(err, evdev.ErrPermission) {
			_, _ = fmt.Fprintf(errW, "dndmode: %v. %s\n", err, inputGroupHint)
			return exitPermissionDenied
		}
		_, _ = fmt.Fprintf(errW, "dndmode: input devices unavailable: %v.\n", err)
		return exitPlatformErr
	}

	// --- Step 9.5: notification settings (focus) ---
	focusRunner := notify.NewRunner()
	if effectiveFocus {
		if err := notify.Check(ctx, focusRunner); err != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: --focus needs GNOME notification settings: %v. Run with --focus=false.\n", err)
			return exitFocusSetup
		}
	}

	// --- Step 10.5: crash recovery, under the publish lock ---
	if err := withPublishLock(filepath.Dir(runtimeMgr.Path()), func() error {
		return runtimepkg.RecoverFromCrash(ctx, runtimeMgr,
			runtimepkg.NewNoopReleaser(), // the inhibitor dies with its owner
			focusRunner,
			audio.NewExecRunner(),
			liveChecker,
			log,
		)
	}); err != nil {
		switch {
		case errors.Is(err, errPublishLockBusy):
			_, _ = fmt.Fprintln(errW,
				"dndmode: another dndmode is publishing its state, or a --set-password is setting a new unlock code. Wait for it to finish, then re-run.")
			return exitConcurrentInstance
		case errors.Is(err, errPublishLockUnusable):
			_, _ = fmt.Fprintf(errW,
				"dndmode: cannot claim the publish lock, so crash recovery cannot be serialized against a concurrent dndmode or --set-password: %v.\n"+
					"Check that %s is writable, then re-run.\n",
				err, filepath.Dir(runtimeMgr.Path()))
			return exitRuntimeJSON
		case errors.Is(err, runtimepkg.ErrConcurrentInstance):
			_, _ = fmt.Fprintf(errW, "dndmode: %v. Send SIGTERM or wait for its exit, then re-run.\n", err)
			return exitConcurrentInstance
		case errors.Is(err, runtimepkg.ErrFileDeletePersistent):
			_, _ = fmt.Fprintf(errW,
				"dndmode: cannot delete stale runtime file (%s): %v.\nRun: rm -f %s\nThen re-run dndmode.\n",
				runtimeMgr.Path(), err, runtimeMgr.Path())
			return exitRuntimeJSON
		}
		_, _ = fmt.Fprintf(errW, "dndmode: crash recovery failed: %v\n", err)
		return exitPlatformErr
	}

	// startupID is the xdg-activation token of the press that raised the
	// current watch session; empty for one-shot runs.
	var startupID string

	// --- Session (Steps 12-19) ---
	runSession := func(ctx context.Context, cancel context.CancelFunc, rs *state.RestoreState, ownsRunLoop bool) int {
		// Step 12.
		rs.Push(runtimeMgr)

		// Step 13: awake lock.
		inh, err := inhibit.Start(ctx, os.Getpid(), cfg.AllowDisplaySleep, log)
		if err != nil {
			runtimeMgr.Disown()
			_, _ = fmt.Fprintf(errW, "dndmode: acquire awake-lock failed: %v. Install systemd or gnome-session and re-run.\n", err)
			return exitPlatformErr
		}
		rs.Push(inh)

		// Step 13.3: prior mute state, then publish runtime.json under the lock.
		muteRunner := audio.NewExecRunner()
		var priorMuted *bool
		if effectiveMute {
			if got, gerr := muteRunner.GetMuted(ctx); gerr != nil {
				log.Warn("query system mute state failed; skipping audio mute", slog.Any("err", gerr))
			} else {
				priorMuted = &got
			}
		}

		releaseLock, lockErr := acquirePublishLock(filepath.Dir(runtimeMgr.Path()))
		switch {
		case lockErr == nil:
			defer releaseLock()
		case errors.Is(lockErr, errPublishLockBusy):
			runtimeMgr.Disown()
			_, _ = fmt.Fprintln(errW,
				"dndmode: another dndmode is publishing its state, or a --set-password is setting a new unlock code. Wait for it to finish, then re-run.")
			return exitConcurrentInstance
		default:
			runtimeMgr.Disown()
			_, _ = fmt.Fprintf(errW,
				"dndmode: cannot claim the publish lock, so a concurrent dndmode or --set-password cannot be ruled out: %v.\n"+
					"Check that %s is writable, then re-run.\n",
				lockErr, filepath.Dir(runtimeMgr.Path()))
			return exitRuntimeJSON
		}
		if alive, peerPID, perr := runtimepkg.IsLiveInstance(runtimeMgr, liveChecker, log); perr != nil {
			log.Warn("peer re-check inconclusive", slog.Any("err", perr))
		} else if alive {
			runtimeMgr.Disown()
			releaseLock()
			_, _ = fmt.Fprintf(errW,
				"dndmode: another instance is already active (PID=%d). Send SIGTERM or wait for its exit, then re-run.\n",
				peerPID)
			return exitConcurrentInstance
		}
		if configFingerprint(cfgPath) != cfgFingerprint {
			runtimeMgr.Disown()
			releaseLock()
			_, _ = fmt.Fprintf(errW,
				"dndmode: %s changed while dndmode was starting, so the unlock code loaded at startup may no longer be the one in the file. Nothing was locked — re-run.\n",
				cfgPath)
			return exitConfigErr
		}
		if err := runtimeMgr.Write(runtimepkg.Snapshot{
			PID:          os.Getpid(),
			StartedAt:    time.Now().UTC(),
			PriorFocus:   nil,
			AssertionID:  0, // no assertion on Linux; the inhibitor dies with us
			PriorMuted:   priorMuted,
			FocusEnabled: &effectiveFocus,
		}); err != nil {
			runtimeMgr.Disown()
			releaseLock()
			_, _ = fmt.Fprintf(errW, "dndmode: write runtime.json failed: %v. Inspect %s and re-run.\n", err, runtimeMgr.Path())
			return exitPlatformErr
		}
		releaseLock()

		// Step 13.7: focus + mute, best-effort, releasers pushed regardless.
		if effectiveFocus {
			prior, ferr := notify.Activate(ctx, focusRunner)
			if ferr != nil {
				log.Warn("focus activate failed", slog.Any("err", ferr))
			}
			rs.Push(notify.NewReleaser(focusRunner, prior, log))
		}
		if priorMuted != nil {
			if err := muteRunner.SetMuted(ctx, true); err != nil {
				log.Warn("system audio mute failed", slog.Any("err", err))
			}
			rs.Push(audio.NewReleaser(muteRunner, *priorMuted, log))
		}

		// Step 14: GTK, on the UI thread.
		var initErr error
		dispatchMainSync(func() { initErr = overlay.Init(log) })
		if initErr != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: overlay init failed: %v.\n", initErr)
			return exitPlatformErr
		}

		// Step 15: shield windows.
		controller := overlay.NewController(overlayStyle, terminalLanguage, startupID, log)
		var createErr error
		dispatchMainSync(func() { createErr = controller.CreateWindowsForAllScreens() })
		if createErr != nil {
			_, _ = fmt.Fprintf(errW, "dndmode: create overlay windows failed: %v. Reconnect displays and re-run.\n", createErr)
			return exitPlatformErr
		}
		if controller.WindowCount() == 0 {
			_, _ = fmt.Fprintln(errW,
				"dndmode: no monitor attached. Input is locked anyway; the overlay appears when a monitor returns.")
		}
		rs.Push(controller)

		// Step 16: supervisor.
		stopper := &cancelStopper{cancel: cancel}
		sup := supervisor.New(log, stopper)
		sup.Start(ctx)

		// Step 17: input grab.
		grabRel, err := evdev.Install(unlockVerifier, sup.ExitTrigger(), log)
		if err != nil {
			stopper.RequestStop("input grab failed")
			sup.Wait()
			switch {
			case errors.Is(err, evdev.ErrPermission):
				_, _ = fmt.Fprintf(errW, "dndmode: %v. %s\n", err, inputGroupHint)
				return exitPermissionDenied
			case errors.Is(err, evdev.ErrBusy):
				_, _ = fmt.Fprintf(errW, "dndmode: %v. Close the program holding it (another locker, a remapper), then re-run.\n", err)
				return exitSecureInputConflict
			}
			_, _ = fmt.Fprintf(errW, "dndmode: grab input devices failed: %v\n", err)
			return exitPlatformErr
		}
		rs.Push(grabRel)

		if os.Getenv("DNDMODE_TEST_PANIC") == "1" {
			panic("test panic (DNDMODE_TEST_PANIC=1)")
		}

		// Step 17.9: timer, counted from the moment the lock is live.
		stopTimer := armTimer(timerDur, cancel, log)
		defer stopTimer()

		// Step 18.
		_, _ = fmt.Fprintln(outW, "dndmode: active. type the unlock code to exit.")

		// Step 19.
		if ownsRunLoop {
			if err := overlay.RunApp(ctx); err != nil {
				stopper.RequestStop("gtk exit: " + err.Error())
			}
		}
		sup.Wait()
		return exitOK
	}

	if !*fl.watch {
		return runSession(ctx, cancel, rs, true)
	}

	// --- Watch mode ---
	if err := overlay.Init(log); err != nil {
		_, _ = fmt.Fprintf(errW, "dndmode: overlay init failed: %v.\n", err)
		return exitPlatformErr
	}
	watchOutW, watchErrW := io.Writer(os.Stdout), io.Writer(os.Stderr)
	comboLabel := config.DefaultActivateHotkey
	if cfg.ActivateHotkey != nil {
		comboLabel = *cfg.ActivateHotkey
	}
	comboLabel = linuxComboLabel(comboLabel)

	// SIGUSR1 must be caught before the shortcut can send it: its default
	// action is to terminate.
	presses, stopPresses := activationPresses()
	defer stopPresses()

	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(watchErrW, "dndmode: cannot locate my own executable for the shortcut: %v\n", err)
		return exitPlatformErr
	}
	hotkeyReg, err := gnomekeys.Register(activateSpec, exe, log)
	if err != nil {
		switch {
		case errors.Is(err, gnomekeys.ErrComboTaken):
			_, _ = fmt.Fprintf(watchErrW,
				"dndmode: %s is already bound to a GNOME shortcut. Pick a different activate_hotkey in %s, or free it in Settings → Keyboard.\n",
				comboLabel, cfgPath)
			return exitConfigErr
		case errors.Is(err, gnomekeys.ErrUnmappedKey), errors.Is(err, gnomekeys.ErrNoModifier):
			_, _ = fmt.Fprintf(watchErrW, "dndmode: cannot register %s: %v. Fix activate_hotkey in %s.\n", comboLabel, err, cfgPath)
			return exitConfigErr
		}
		_, _ = fmt.Fprintf(watchErrW, "dndmode: cannot register %s: %v\n", comboLabel, err)
		return exitPlatformErr
	}
	rs.Push(hotkeyReg)

	if code := becomeWatchDaemon(daemonDeps{
		dir:        filepath.Dir(cfgPath),
		record:     watchMgr,
		comboLabel: comboLabel,
		ready:      readyPipe,
		rs:         rs,
		term:       watchOutW,
		errW:       watchErrW,
		log:        log,
	}); code != exitOK {
		return code
	}

	var sessions sync.WaitGroup
	sessions.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-presses:
				if ctx.Err() != nil {
					return
				}
				startupID = takeActivationToken(filepath.Dir(cfgPath))
				runOneWatchSession(ctx, watchSessionDeps{
					loader:         loader,
					cfgPath:        cfgPath,
					cfg:            &cfg,
					unlockVerifier: &unlockVerifier,
					cfgFingerprint: &cfgFingerprint,
					log:            log,
					outW:           watchOutW,
					errW:           watchErrW,
					session:        runSession,
					comboLabel:     comboLabel,
				})
				// A press during the session is not a request for another.
				select {
				case <-presses:
				default:
				}
			}
		}
	})

	runErr := overlay.RunApp(ctx)

	// A session tearing down at exit still needs the UI thread for its
	// windows: keep servicing it until the session goroutine is done.
	sessionsDone := make(chan struct{})
	go func() {
		sessions.Wait()
		close(sessionsDone)
	}()
	overlay.Pump(sessionsDone)

	if runErr != nil {
		_, _ = fmt.Fprintf(watchErrW, "dndmode: run loop exited unexpectedly: %v\n", runErr)
		return exitPlatformErr
	}
	return exitOK
}

// platformBeep is the audible half of watchSessionDeps.fail.
func platformBeep() { overlay.Beep() }

// dispatchMainSync runs fn on the UI thread and waits for it; inline when
// already there (the one-shot path, before the main loop starts).
func dispatchMainSync(fn func()) {
	done := make(chan struct{})
	overlay.DispatchMain(func() {
		defer close(done)
		fn()
	})
	<-done
}

// runInhibitOnly is overlay_style none: hold the awake-lock and nothing
// else until a signal or the timer — the Linux runCaffeinateOnly.
func runInhibitOnly(ctx context.Context, cancel context.CancelFunc, allowDisplaySleep bool, timerDur time.Duration, rs *state.RestoreState, log *slog.Logger, outW, errW io.Writer) int {
	proc, err := inhibit.Start(ctx, os.Getpid(), allowDisplaySleep, log)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return exitOK
		}
		_, _ = fmt.Fprintf(errW, "dndmode: start inhibitor failed: %v.\n", err)
		return exitPlatformErr
	}
	rs.Push(proc)

	stopTimer := armTimer(timerDur, cancel, log)
	defer stopTimer()

	_, _ = fmt.Fprintln(outW, "dndmode: active (awake-only, no overlay). press Ctrl-C.")

	select {
	case <-ctx.Done():
		return exitOK
	case <-proc.Done():
		if ctx.Err() != nil {
			return exitOK
		}
		log.Warn("inhibitor exited unexpectedly", slog.Any("err", proc.Err()))
		return exitPlatformErr
	}
}
