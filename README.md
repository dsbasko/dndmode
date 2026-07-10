# dndmode

> Lock your unattended Apple Silicon MacBook without killing the work it is doing.

[![Platform](https://img.shields.io/badge/platform-macOS%2014%2B%20%C2%B7%20arm64-black)](#requirements)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8)](go.mod)
[![Network](https://img.shields.io/badge/network-zero%20calls-success)](#no-network)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

`dndmode` covers every display with a full-screen shield, blocks the keyboard and
trackpad at the HID level, keeps the Mac awake, and silences it - all while your
background processes keep running untouched. You step away, the machine looks and
behaves as if it is locked, and the long job you left running (an AI agent in YOLO
mode, a build, a render) never gets interrupted. You come back, type your hotkey,
and everything is exactly where you left it.

It is a foreground CLI. No daemon, no launchd, no menu-bar icon. You run it, you
watch it, you end it.

```bash
dndmode            # lock now, run until the unlock hotkey
dndmode --timer 1h # lock now, auto-unlock after an hour
```

---

## Contents

- [Why](#why)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Install](#install)
- [First-run setup](#first-run-setup)
- [Usage](#usage)
- [Configuration](#configuration)
- [Overlay styles](#overlay-styles)
- [Exit codes](#exit-codes)
- [Threat model](#threat-model)
- [Troubleshooting](#troubleshooting)
- [Uninstall](#uninstall)
- [Building from source](#building-from-source)
- [Known limitations](#known-limitations)
- [License](#license)

---

## Why

You are running an AI agent in YOLO mode, or any long unattended task, and you need
to leave the laptop. Two bad options remain:

- Lock the screen (`Ctrl+Cmd+Q`). Safe, but macOS may suspend or throttle the
  session, and a locked screen is an invitation to walk away and forget the job
  is even alive.
- Leave it open. The job keeps running, but anyone passing by can touch the
  keyboard, click a dialog, or read what is on screen.

`dndmode` is the third option: the job keeps running at full speed, and the machine
is covered and inert to input until you return and enter your hotkey. It is a
soft-lock for cooperative spaces (home, office, a coworking desk), not
hardware-grade protection - see the [threat model](#threat-model).

## How it works

Four layers run for the length of a session, plus a crash-safety layer underneath
them.

**Shield overlay.** One borderless `NSWindow` per attached display, drawn at
`CGShieldingWindowLevel()` (above the screen saver) with the collection behavior
`canJoinAllSpaces | stationary | fullScreenAuxiliary | ignoresCycle`. That places
the shield over the menu bar, Dock, Mission Control, Spotlight, Cmd+Tab, and the
Force Quit dialog, on every Space and next to full-screen apps. Plugging or
unplugging a display, changing resolution, or rearranging monitors rebuilds the
overlay within 250 ms. The system cursor is hidden while the shield is up.

**Input lock.** Two `CGEventTap`s, both `kCGHeadInsertEventTap` with
`kCGEventTapOptionDefault` - the suppression-capable mode, not listen-only. The
primary tap sits at `kCGHIDEventTap`; its callback returns `NULL` for all 15
intercepted event types (key down/up, modifier changes, every mouse button, drag,
move, scroll, and system-defined media keys), so nothing reaches WindowServer.
Cmd+Tab, Cmd+Q, and the rest are dead. Exactly one event passes the filter: a
key-down that matches your configured unlock hotkey, which is turned into an
internal exit signal and is itself swallowed so it never leaks into the app
underneath. A second tap at `kCGSessionEventTap` swallows the trackpad gesture
stream (the session-level gesture and dock-control events WindowServer synthesizes
past the HID tap point), so three- and four-finger swipes for Mission Control,
App Exposé and Space switching, and the Launchpad pinch die before the Dock sees
them. Both taps are silent on wrong input by design - a watcher gets no side
channel. A GCD watchdog probes every 5 s and re-enables both taps if macOS
silently disabled them; after 5 consecutive failed re-enables of the primary tap
(about 25 s) it gives up and ends the session with a distinct exit code. An
`NSWorkspace` observer re-arms both taps after sleep or fast user switching.

**Awake lock.** One IOKit power assertion named `dndmode active` (visible in
`pmset -g assertions`). By default it is `kIOPMAssertPreventUserIdleDisplaySleep`,
so the display stays lit and the system stays awake. Set `allow_display_sleep: true`
to switch to `kIOPMAssertPreventUserIdleSystemSleep` instead, which lets the display
idle-off while the system keeps running.

**Silence.** System audio is muted for the session (default `mute: true`) and
restored on exit, so notification sounds and beeps stay quiet. The mute is
state-aware: it records whether audio was already muted before the session and never
unmutes what it did not mute. Notification *banners* never show because the shield
sits above Notification Center. Do Not Disturb Focus is a separate opt-in
(`focus: false` by default) covered [below](#focus--do-not-disturb).

**Crash safety.** A snapshot at `~/.config/dndmode/runtime.json` records the pid,
the assertion id, and the prior audio/Focus state. A second instance detects the
first and refuses to start (exit 5). If a session is `kill -9`'d, the next launch
reads the snapshot, releases the orphaned power assertion by its exact id,
conditionally restores audio and Focus, and deletes the file - so a hard kill never
leaves the Mac stuck awake or muted.

## Requirements

- macOS 14 (Sonoma) or newer.
- Apple Silicon (`arm64`). Intel is not supported.
- Accessibility and Input Monitoring permissions, granted on first run through
  System Settings. Both are required for the input lock; the awake-only `none` mode
  needs neither.
- Two macOS Shortcuts named exactly `dndmode-on` and `dndmode-off`, but only when
  `focus: true`. The default configuration needs no Shortcuts.
- Go 1.26+ only if you build from source.

<a id="no-network"></a>The binary makes zero network calls and has no network code
in its dependency closure. Verify it yourself with `make audit-net` (static
dependency check) and `make audit-net-runtime` (live socket check against a running
instance).

## Install

Pick one install path and stay on it. Mixing `go install` and `make install`
produces two separate binaries (`~/go/bin/dndmode` and `/usr/local/bin/dndmode`)
with different code signatures, and macOS TCC treats them as different apps - each
needs its own Accessibility and Input Monitoring grant.

**From source (recommended - stable permissions across upgrades):**

```bash
git clone https://github.com/dsbasko/dndmode
cd dndmode
make install
```

`make install` builds with the ad-hoc codesign identifier `com.dsbasko.dndmode` and
copies the binary to `/usr/local/bin/dndmode`. Because that identifier (and the
resulting cdhash) is stable across rebuilds, a later `git pull && make install`
keeps your TCC grants. Make sure `/usr/local/bin` comes before `~/go/bin` in
`$PATH`, or always call `/usr/local/bin/dndmode` explicitly.

**Quick (`go install`):**

```bash
go install github.com/dsbasko/dndmode@latest
```

This drops the binary in `$(go env GOPATH)/bin`. Every `@latest` rebuild changes the
cdhash, so TCC sees a new app and re-prompts for Accessibility and Input Monitoring
on each upgrade. If that annoys you, run `make install` from a clone once and use the
stable `/usr/local/bin` copy. See [Troubleshooting](#tcc-permissions-lost-after-a-go-install-upgrade)
for the mechanics.

Homebrew is not supported yet - it needs an Apple Developer ID, which is deferred.

## First-run setup

1. Install dndmode (see [Install](#install)).
2. Run `dndmode`. It prompts for Accessibility. Click **Open System Settings** and
   enable dndmode under **Privacy & Security → Accessibility**.
3. It then waits for Input Monitoring. Enable dndmode under
   **Privacy & Security → Input Monitoring**. There is no system prompt for this one -
   dndmode opens the pane, and the run continues once you flip the switch.
4. Only if you want Focus/DND (`focus: true` or `--focus=true`): open the
   **Shortcuts** app and create two shortcuts. First, add the **Set Focus** action,
   choose **Do Not Disturb → Turn On Until Turned Off**, save it as `dndmode-on`.
   Then a second shortcut that turns it **Off**, saved as `dndmode-off`. With the
   default `focus: false` you can skip this entirely.
5. Run `dndmode` again. With `--debug` you will see `dndmode: active. press Ctrl-C.`.
   The default hotkey `Ctrl+Option+Cmd+X` ends the lock.

## Usage

**Start:** `dndmode`. It runs in the foreground and blocks the terminal until the
session ends.

**End a session, any of:**

- Press the unlock hotkey (default `Ctrl+Option+Cmd+X`).
- Press `Ctrl-C` (or send `SIGTERM`/`SIGHUP`) in the terminal running dndmode.
- Set a deadline with `--timer` and let it expire.

**Flags.** Every flag is per-run. The tri-state flags fall back to the config file
when omitted.

| Flag | Values | Default | Effect |
| --- | --- | --- | --- |
| `--style` | `black` \| `matrix` \| `glass` \| `none` | config | Overlay look for this run; wins over `overlay_style`. |
| `--mute` | `true` \| `false` | config | Mute system audio for this run. |
| `--focus` | `true` \| `false` | config | Toggle Do Not Disturb for this run. |
| `--timer` | Go duration (`30m`, `1h30m`, `90s`) | off | Auto-unlock after the duration, then exit `0`. |
| `--debug` | (boolean) | off | Un-silence banners, diagnostics, and logs. |

A few notes on behavior:

- **`--timer`** starts counting once dndmode is active, so time spent granting
  permissions never eats into it. It works with every overlay style, including
  `none`. There is deliberately no config key - typing the flag is the opt-in.
- **Quiet by default.** dndmode prints nothing to stdout or stderr and reports
  outcome only through its [exit code](#exit-codes). This is a security default:
  with `glass` or `none` the terminal stays visible while dndmode runs, and a
  printed banner would leak your unlock hotkey to anyone watching. Pass `--debug`
  (or set `debug: true`) to turn output back on when a run exits non-zero and you
  need to see why.
- **Invalid flag values** (`--timer 5x`, `--mute banana`, `--style neon`) exit with
  the config-error code `1`, and print the reason on stderr only under `--debug`.

## Configuration

The config file lives at `~/.config/dndmode/config.yml` and is created with defaults
on first run. Only `hotkey` is written as an active key; every other setting is shown
commented at its default, so uncommenting a line only ever overrides.

```yaml
# ~/.config/dndmode/config.yml

# Unlock hotkey. Requires at least one modifier plus one key.
# Modifiers: ctrl, option, cmd, shift, fn.
# Keys: a-z, 0-9, f1-f12, arrows, space, return/enter, tab, escape/esc,
#       delete, forwarddelete, and punctuation ( - = [ ] ; ' , . / \ ` ).
hotkey: Ctrl+Option+Cmd+X

# Overlay look: black (default) | matrix | glass | none
# overlay_style: black

# false (default) keeps the display awake; true lets it idle-off while the
# system stays awake. Note the inverted sense of the name.
# allow_display_sleep: false

# Mute system audio for the session and restore it on exit (default true).
# mute: true

# Toggle Do Not Disturb via the dndmode-on / dndmode-off Shortcuts (default
# false). Enabling it syncs DND to your other Apple devices over iCloud.
# focus: false

# Print banners, diagnostics, and debug logs (default false = silent).
# debug: false
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `hotkey` | string | `Ctrl+Option+Cmd+X` | The combination that ends the lock. |
| `overlay_style` | string | `black` | Shield look, or `none` for awake-only mode. |
| `allow_display_sleep` | bool | `false` | `false` keeps the display awake; `true` lets it idle-off. |
| `mute` | bool | `true` | Mute system audio for the session. |
| `focus` | bool | `false` | Toggle Do Not Disturb via Shortcuts. |
| `debug` | bool | `false` | Un-silence output. |

The hotkey is matched by physical key position, so it behaves the same on a US,
Russian, or AZERTY layout. Caps Lock and the numeric-keypad flag are ignored during
matching, so a stray Caps Lock can never lock you out.

The YAML parser is strict about unknown keys but not about values: a misspelled key
(`overaly_style`) is rejected on load, while a bad value (`overlay_style: blak`) is
caught a moment later. Either way the process exits `1` with a line-and-column error
under `--debug`.

### Focus / Do Not Disturb

Focus is off by default and opt-in for one reason: macOS syncs it across your Apple
devices over iCloud ("Share Across Devices"). Turning DND on at the Mac would
silently turn it on your iPhone too, and there is no API to enable Focus on this
device only. The default `mute: true` already covers the local goal - silencing
sounds - without touching your other devices.

When you do enable it, dndmode runs the `dndmode-on` Shortcut at startup and
`dndmode-off` on exit. It checks that both shortcuts exist up front, before locking
anything, and exits `6` with setup instructions if either is missing. It does not
remember or restore whatever Focus you had before - on exit it simply turns Focus
off (see [Known limitations](#known-limitations)).

## Overlay styles

| Style | Look | Bleeds through? | Input blocked? |
| --- | --- | --- | --- |
| `black` | Opaque black shield (default). | No | Yes |
| `matrix` | Green digital rain over an opaque black shield. Cosmetic. | No | Yes |
| `glass` | Frosted `NSVisualEffectView`; the blurred desktop shows through. | Yes, by design | Yes |
| `none` | No overlay. Awake-only mode - see below. | n/a | No |

`glass` is the only style that is not opaque. It trades the no-bleed-through
guarantee for the look; input is still fully blocked underneath.

**Awake-only mode (`none`).** `overlay_style: none` (or `dndmode --style none`) turns
dndmode into a thin [`caffeinate(8)`](https://ss64.com/mac/caffeinate.html) wrapper.
It does not draw an overlay, does not block the keyboard or trackpad, does not mute
audio, and does not touch Focus - so it needs no Accessibility permission. It only
holds a system-awake assertion for as long as it runs. Under the hood it runs
`caffeinate -d -i -s -w <pid>` (the `-d` is dropped when `allow_display_sleep: true`);
`-w <pid>` ties the assertion to dndmode's lifetime so it self-releases even after a
`kill -9`. There is no hotkey in this mode - there is no event tap to observe one -
so you exit with `Ctrl-C` or `--timer`.

## Exit codes

dndmode's exit code is its primary contract. In the default silent mode it is the
only thing it tells you.

| Code | Meaning |
| --- | --- |
| `0` | Clean exit via the hotkey, a signal, or `--timer` expiry. |
| `1` | Config error: bad YAML, an invalid hotkey, or an invalid flag value. |
| `2` | Platform error: not arm64, macOS < 14, IOKit/Cocoa failure, or (in `none` mode) an unexpected `caffeinate` death. |
| `3` | Interrupted while waiting for Accessibility / Input Monitoring grants. |
| `4` | Secure Event Input is held by another app, or the input tap was silently disabled and the watchdog gave up. |
| `5` | Another live dndmode instance is already running. |
| `6` | Required Shortcuts `dndmode-on` / `dndmode-off` not found (only when `focus: true`). |
| `7` | Cannot delete a stale `~/.config/dndmode/runtime.json`. |
| `8` | Internal panic, recovered after cleanup. |

## Threat model

### What dndmode protects against

- A passerby, family member, or colleague touching the keyboard or trackpad while an
  agent runs. Keyboard, mouse, scroll, media keys, Cmd+Tab, and Cmd+Q are all
  blocked at the HID level; trackpad gestures (Mission Control / App Exposé /
  Spaces swipes, Launchpad pinch) are blocked at the session level.
- Visual access to the desktop on every connected display, including probing through
  Mission Control, Spotlight, or the Force Quit dialog.
- Notification banners (hidden under the shield) and sounds (audio muted for the
  session), with Focus/DND optionally on top.

### What dndmode does not protect against

- Touch ID / biometric unlock (impossible to block without root).
- Power-button hold (hard shutdown) and Recovery mode (Cmd+R at boot).
- Hardware keyloggers or DMA over Thunderbolt.
- Malware running as root.
- Sustained physical access.
- Remote SSH / VNC sessions - the target is the local console only.

### Per-layer coverage

| Layer | Covers | Notes |
| --- | --- | --- |
| Shield overlay | Visual access to every display | `glass` shows a blurred desktop by design. |
| Input lock (`CGEventTap`) | Keyboard, mouse, scroll, media, Cmd+Tab, Cmd+Q | Needs Accessibility; self-heals via a watchdog and a wake observer. |
| Awake lock (IOPMAssertion) | System and display idle sleep | Display kept awake by default; `allow_display_sleep` flips it. |
| Audio mute | Notification sounds and beeps | On by default, restored on exit; skipped in `none` mode. |
| Focus/DND | Notification banners | Opt-in; DND syncs to iPhone over iCloud. |

dndmode is a soft-lock for cooperative environments, not red-team-grade hardware
protection. Use it at your own risk.

## Troubleshooting

### TCC permissions lost after a `go install` upgrade

Each `go install ...@latest` rebuild changes the binary's cdhash, which TCC uses as
identity. Without a stable signature it sees a new app and revokes Accessibility and
Input Monitoring on every upgrade.

Fix it by using the stable ad-hoc signature:

```bash
make install   # re-applies identifier com.dsbasko.dndmode, preserving grants
```

Or reset the entries and re-grant from scratch:

```bash
tccutil reset Accessibility com.dsbasko.dndmode
tccutil reset ListenEvent com.dsbasko.dndmode
dndmode        # re-prompts for permissions
```

### Required Shortcuts not found (exit 6)

Re-create `dndmode-on` and `dndmode-off` in the Shortcuts app - see
[First-run setup](#first-run-setup) step 4. dndmode prints the missing names and a
create-shortcut guide on stderr under `--debug`.

### Secure Event Input conflict (exit 4)

Another app holds Secure Event Input - usually a Terminal `sudo` prompt, a password
manager, or an active password field. Dismiss it and re-run. To find the holder:

```bash
ioreg -l -w 0 | grep SecureInput
```

Exit `4` also fires if the input tap was silently disabled and the watchdog could not
bring it back. In that case, re-run and check that Accessibility is still granted.

### Another instance is already active (exit 5)

```bash
pgrep -x dndmode      # find the pid(s)
pkill -TERM dndmode   # ask it to exit cleanly
```

Or wait for it to finish, then re-run.

### Cannot delete a stale runtime file (exit 7)

```bash
rm -f ~/.config/dndmode/runtime.json
```

Causes are a read-only filesystem, an ACL denying delete, or a full disk.

## Uninstall

```bash
sudo rm /usr/local/bin/dndmode
# optional: also clear the permission entries
tccutil reset Accessibility com.dsbasko.dndmode
tccutil reset ListenEvent com.dsbasko.dndmode
```

## Building from source

The project is Go 1.26 with cgo bridging into AppKit, Quartz, IOKit, and
ApplicationServices through raw Objective-C files. It builds for `darwin/arm64` only.

```bash
make build         # CGO build + ad-hoc codesign into ./dndmode
make test          # unit tests with -race
make test-cover    # tests with a coverage summary
make lint          # go vet + golangci-lint
make acceptance    # subprocess acceptance tests (build tag: acceptance)
make audit-net     # assert zero network dependencies in the binary
make clean         # remove the binary, coverage, and generated mocks
```

GUI smoke tests that create real `NSWindow`s are gated behind the `manual` build
tag, so `go test ./...` skips them. Run them intentionally from a GUI session:

```bash
go test -tags=manual ./internal/macos/cocoa/...
```

Rough layout:

```
cmd/dndmode/          CLI entry point, startup pipeline, LIFO teardown
internal/config/      YAML config + hotkey string parser
internal/matcher/     pure-Go hotkey matching model
internal/macos/
  cocoa/              per-screen shield windows and overlay styles
  eventtap/           CGEventTap input lock + watchdog + wake re-arm
  powerassert/        IOPMAssertion awake lock + orphan cleanup
  caffeinate/         awake-only (none) mode wrapper
  audiomute/          system audio mute and restore
  focus/              Do Not Disturb via the Shortcuts CLI
  permissions/        Accessibility, Input Monitoring, platform, secure-input gates
internal/state/       teardown registry + runtime.json crash recovery
internal/supervisor/  single-point shutdown fan-in
```

## Known limitations

- **No prior-Focus restore.** With `focus: true`, dndmode turns Focus off on exit
  rather than restoring whatever you had before. Audio, by contrast, is restored:
  a session that finds audio already muted leaves it muted.
- **`glass` bleeds through.** It shows a blurred desktop on purpose. Use `black` or
  `matrix` if you need the desktop fully hidden.
- **Foreground only.** No daemon or launchd mode. The terminal that launched dndmode
  must stay open.
- **One instance at a time.** A second dndmode exits `5` with instructions.
- **Signing.** Recent macOS refuses unsigned binaries. `make build` applies ad-hoc
  codesigning; `go install` relies on Go's linker-signed signature, which launches
  but is not stable enough for TCC across upgrades.

## License

Released under the MIT License. See [LICENSE](LICENSE) for the full text.

© 2026 Dmitriy Basenko.
