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
mode, a build, a render) never gets interrupted. You come back, type your unlock
code, and everything is exactly where you left it.

It is a foreground CLI. No daemon, no launchd, no menu-bar icon. You run it, you
watch it, you end it.

```bash
dndmode            # lock now, run until the unlock code is typed
dndmode --timer 1h # lock now, auto-unlock after an hour
```

**Quick install** via the personal Homebrew tap (other methods and the permission
notes are under [Install](#install)):

```bash
brew install dsbasko/tap/dndmode   # install
brew upgrade dndmode               # update to the latest release
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
is covered and inert to input until you return and type your unlock code. It is a
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
Cmd+Tab, Cmd+Q, and the rest are dead. **No event passes the filter, ever** - not
even the keys of your unlock code. Instead, the callback appends each key-down to
a 64-entry in-memory ring (modifier flags plus the physical key code, nothing
else), and a Go goroutine polls that ring every 10 ms and checks whether the
*tail* of what you typed equals your unlock code. When it does, the session ends.
Because matching happens after the fact rather than in the interception path,
there is no "correct key" that behaves differently from a wrong one, and no
keystroke ever surfaces in the app underneath. Held-down keys are dropped before
they reach the ring, so leaning on a key cannot be used to sweep the code space.
A second tap at `kCGSessionEventTap` swallows the trackpad gesture
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

Pick one install path and stay on it. Mixing them (`brew`, `go install`,
`make install`) produces separate binaries (`/opt/homebrew/bin/dndmode`,
`~/go/bin/dndmode`, `/usr/local/bin/dndmode`) with different code signatures, and
macOS TCC treats each as a different app - each needs its own Accessibility and
Input Monitoring grant.

**Homebrew (easiest):**

```bash
brew install dsbasko/tap/dndmode   # install
brew upgrade dndmode               # update to the latest release
```

Installs from the personal tap
[`dsbasko/homebrew-tap`](https://github.com/dsbasko/homebrew-tap) (not
homebrew-core), building from source and re-applying the ad-hoc codesign identifier
`com.dsbasko.dndmode`. Each `brew upgrade` rebuilds with a fresh cdhash, so macOS
re-prompts for Accessibility and Input Monitoring after an upgrade (the formula says
so in its caveats). If you want grants that survive upgrades, use the source build
below.

**From source (most stable permissions across upgrades):**

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

All three paths build from source (there is no pre-signed bottle - the binary is
ad-hoc signed locally), so none require an Apple Developer ID.

### Upgrading from a `hotkey` config

Configs written by earlier versions use a `hotkey:` key holding one chord. It
still works - a single combination is just a 1-step unlock code - but it is
deprecated, and `--debug` says so on every start. The file is not rewritten and
the chord keeps unlocking.

**One exception: `fn` is no longer a modifier.** It still parses, but it
contributes nothing, because macOS raises the Fn bit for every F-key, arrow and
Forward Delete on its own - a chord could demand it, but you could not withhold
it, so a step written without it would be unmatchable on exactly those keys.
Two consequences for an existing `hotkey:` value:

| Old value | Now |
| --- | --- |
| `hotkey: ctrl+fn+x` | Loads, but unlocks on **Ctrl+X** - the Fn is not required any more |
| `hotkey: fn+x` | **Startup error** (exit `1`) - no modifier is left, and a bare key must not unlock the shield |

If your chord relies on `fn`, this is the moment to migrate it to a real
`unlock_code` rather than to re-add a modifier. Run with `--debug` to see the
error; startup is silent by default.

Migrate by renaming the key, then lengthening the value:

```yaml
# before
hotkey: Ctrl+Option+Cmd+X

# after
unlock_code: s w o r d f i s h
```

Renaming is not always enough: **spaces separate steps** in `unlock_code`, so
any spaces around the `+` have to go. `hotkey: Ctrl + Option + X` is a legal
chord, but as an `unlock_code` it reads as five steps - the first of them a bare
`Ctrl` - and startup fails with exit `1`.

Setting **both** keys is a startup error (exit `1`) - an ambiguous unlock
secret is not resolvable, and guessing wrong here locks the machine with a code
the owner does not believe is in effect. See
[The unlock code](#the-unlock-code) for the grammar and the length rules.

## First-run setup

1. Install dndmode (see [Install](#install)).
2. Run `dndmode`. This first launch writes the default config to
   `~/.config/dndmode/config.yml` (the file does not exist until dndmode runs -
   `dndmode --help` only prints usage and creates nothing), then prompts for
   Accessibility. Click **Open System Settings** and enable dndmode under
   **Privacy & Security → Accessibility**.
3. It then waits for Input Monitoring. Enable dndmode under
   **Privacy & Security → Input Monitoring**. There is no system prompt for this one -
   dndmode opens the pane, and the run continues once you flip the switch.
4. Only if you want Focus/DND (`focus: true` or `--focus=true`): open the
   **Shortcuts** app and create two shortcuts. First, add the **Set Focus** action,
   choose **Do Not Disturb → Turn On Until Turned Off**, save it as `dndmode-on`.
   Then a second shortcut that turns it **Off**, saved as `dndmode-off`. With the
   default `focus: false` you can skip this entirely.
5. Run `dndmode` again. With `--debug` you will see `dndmode: active. press Ctrl-C.`.
   The default unlock code `Ctrl+Option+Cmd+X` ends the lock.

> **Change your unlock code before you rely on it.** The generated config ships
> with the well-known default `Ctrl+Option+Cmd+X` - a code of exactly one step,
> which is the shape that motivated unlock codes in the first place: a single
> combination falls to someone walking the keyboard "like a piano" with the
> modifiers held. Anyone who knows the default can unlock the shield outright.
> Open `~/.config/dndmode/config.yml` and set `unlock_code` to a private
> sequence of **6 or more steps** (`unlock_code: s w o r d f i s h`); the
> grammar, the key list, and the length table are in
> [Configuration](#configuration). Running with the default is accepted, but
> `--debug` prints a warning about it on every start.

## Usage

**Start:** `dndmode`. It runs in the foreground and blocks the terminal until the
session ends.

**End a session, any of:**

- Type the unlock code (default `Ctrl+Option+Cmd+X`). There is no prompt and no
  feedback - just type it; the shield ends the moment the tail matches.
- Press `Ctrl-C` (or send `SIGTERM`/`SIGHUP`) in the terminal running dndmode.
- Set a deadline with `--timer` and let it expire.

**Flags.** Every flag is per-run. The tri-state flags fall back to the config file
when omitted.

| Flag | Values | Default | Effect |
| --- | --- | --- | --- |
| `--style` | `black` \| `matrix` \| `terminal`[`:go`\|`python`\|`typescript`\|`rust`] \| `dvd` \| `glass`[`:radius`] \| `none` | config | Overlay look for this run; wins over `overlay_style`. `terminal:<lang>` picks the source language (default `go`). |
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
  printed banner would leak your unlock code to anyone watching. Even under
  `--debug` the banner prints only the *number* of steps and which config key
  they came from - never the code itself. Pass `--debug`
  (or set `debug: true`) to turn output back on when a run exits non-zero and you
  need to see why.
- **Invalid flag values** (`--timer 5x`, `--mute banana`, `--style neon`) exit with
  the config-error code `1`, and print the reason on stderr only under `--debug`.

## Configuration

The config file lives at `~/.config/dndmode/config.yml` and is created with defaults
on first run. Only `unlock_code` is written as an active key; every other setting is
shown commented at its default, so uncommenting a line only ever overrides. The
default `unlock_code` is `Ctrl+Option+Cmd+X` — change it to a private sequence, since
it is the only secret that ends a locked session.

```yaml
# dndmode configuration
# Location: ~/.config/dndmode/config.yml  (auto-created on first run)
#
# Every field except 'unlock_code' is OPTIONAL. Uncomment a line and change its
# value to override the default shown next to it. Unknown keys are REJECTED
# (strict parsing): a typo aborts startup with an error pointing at the line.
# Most fields also have a per-run CLI flag that overrides the file for that
# launch only.

# --- unlock_code (REQUIRED) --------------------------------------------------
# The secret that unlocks and exits the locked state. It is a SEQUENCE of
# steps typed one after another — a passphrase, not a single chord.
#
# Grammar: steps separated by spaces; each step is "(<mod>+)*<key>".
#   Modifiers (case-insensitive): ctrl, option, cmd, shift
#   Keys: a-z, 0-9, f1-f12, space, return (alias enter), tab, escape (alias
#         esc), delete, forwarddelete, left, right, up, down,
#         and the punctuation - = [ ] ; ' , . / \ backtick
#   Modifiers inside a step are OPTIONAL, so both 's' and 'ctrl+s' are steps.
#   A literal space is only a SEPARATOR; the space key itself is 'space'.
#   'fn' is still accepted in a step but carries NO meaning: macOS raises the
#   Fn bit for every F-key, arrow and Forward Delete on its own, so it cannot
#   express intent. Write 'up', not 'fn+up'.
#
# QUOTING: this is a YAML value, so if the code STARTS with one of - [ ] '
# or a backtick, wrap the whole value in double quotes — unquoted, YAML reads
# those as list/quote syntax and startup fails with a parse error, not with a
# dndmode message. Anywhere but the first character they are fine bare. Also
# note that ' #' (space then hash) starts a YAML comment and would silently
# truncate the rest of the code.
#   unlock_code: "- a b c d"           # leading punctuation: quote it
#
# Examples:
#   unlock_code: s w o r d f i s h     # a passphrase
#   unlock_code: ctrl+s w o r d cmd+z  # mixed
#   unlock_code: Ctrl+Option+Cmd+X     # a single chord = a code of length 1
#
# Length rules (dndmode matches the TAIL of everything typed, so every single
# keypress is a fresh attempt — short codes fall fast):
#   1 step      : allowed only WITH modifiers (the legacy hotkey shape). Weak.
#   2-3 steps   : REJECTED — too weak to be worth the illusion of a passphrase.
#   4-32 steps  : accepted. 6 or more is STRONGLY recommended: at 6 steps a
#                 brute force needs ~250 days at 100 keypresses/sec, at 4 it
#                 needs under 5 hours.
# Change the default below before you rely on it — it ships the same chord on
# every machine.
#
# Matched by PHYSICAL key position, not by the character produced: on a RU
# layout 'unlock_code: s w o r d' is typed with the keys ы ц о р в.
# Every step is matched EXACTLY: a modifier you happen to be holding (e.g. Cmd)
# breaks a step declared without it. CapsLock, NumPad and Fn bits are ignored,
# so CapsLock can never lock you out and 'up' or 'down' work as plain steps.
#
# AVOID f1-f12. With the macOS default "Use F1, F2, etc. keys as standard
# function keys" turned OFF, the F-row is delivered as a system media key, NOT
# as a key press — dndmode never sees it. A code containing 'f1' parses fine,
# starts with no warning, and then CANNOT BE TYPED while locked unless you hold
# Fn (or flip that switch in System Settings > Keyboard). The arrows and
# forwarddelete are NOT affected: those are real key presses that merely carry
# the Fn bit, which is stripped.
unlock_code: Ctrl+Option+Cmd+X

# --- hotkey (DEPRECATED) -----------------------------------------------------
# The pre-sequence single-combination key. Still read, so upgrading does not
# break an existing config, but setting BOTH it and unlock_code is an error
# (an ambiguous unlock secret is not resolvable). Migrate by renaming the key —
# with one caveat: spaces separate STEPS in unlock_code, so any spaces around the
# '+' must go ('Ctrl + X' is legal here, but reads as three steps below).
# hotkey: Ctrl+Option+Cmd+X

# --- overlay_style -----------------------------------------------------------
# Look of the full-screen shield that covers every attached display.
#   black  : opaque black shield (default). Nothing bleeds through.
#   matrix : animated green "digital rain" over the black shield (cosmetic
#            only; every blocking guarantee is identical to black).
#   terminal : scrolling stream of syntax-highlighted pseudo-source that types
#            itself out behind a blinking caret over the black shield (cosmetic
#            only; opaque, every blocking guarantee is identical to black).
#            Language is set by terminal_language below (default go); the
#            --style terminal:<lang> flag overrides it for a single run.
#   dvd    : a "DVD VIDEO" logo bounces around the black shield, changing color
#            on every edge hit (the old-DVD-player screensaver). Cosmetic only;
#            opaque, every blocking guarantee is identical to black.
#   glass  : TRANSPARENT frosted glass — the blurred desktop shows through.
#            Trades the no-bleed-through guarantee for the look; keyboard and
#            trackpad are still fully blocked. Blur strength = glass_blur below.
#            Captures + blurs the desktop, so it needs the Screen Recording
#            permission; without it, falls back to a plain system frost.
#   none   : awake-only mode. NO overlay, NO input blocking, NO Focus, NO audio
#            mute — dndmode just holds the machine awake (like caffeinate).
#            Needs no Accessibility permission; exit with Ctrl-C only (there is
#            no unlock code because there is no event tap to observe it).
# Per-run override: --style <value>. For glass the radius can be appended:
#   --style glass:24 overrides glass_blur for this run only (--style glass uses
#   the glass_blur value below, or its default).
# overlay_style: black

# --- glass_blur --------------------------------------------------------------
# Blur radius (in points) for overlay_style 'glass' — how strongly the desktop
# behind the shield is blurred. Only used by 'glass'; ignored otherwise.
#   Lower  (~8)  : sharper — more detail, text starts to become legible.
#   Default (16) : shapes recognizable, text unreadable.
#   Higher (~30) : everything dissolves into a smooth frost.
# Per-run override: the --style glass:<radius> flag (e.g. --style glass:24).
# glass_blur: 16

# --- terminal_language -------------------------------------------------------
# Source language rendered by overlay_style 'terminal': go (default), python,
# typescript or rust. Each has its own compiled-in corpus + syntax highlighting.
# Only used by 'terminal'; ignored otherwise.
# Per-run override: the --style terminal:<lang> flag (e.g. --style terminal:rust).
# terminal_language: go

# --- allow_display_sleep -----------------------------------------------------
# INVERTED toggle controlling the DISPLAY (the system stays awake either way).
#   false : keep the display awake too (default).
#   true  : let the display dim / sleep while background work keeps running —
#           saves the panel when you only need the machine, not the screen.
# allow_display_sleep: false

# --- mute --------------------------------------------------------------------
# System audio muting for the session.
#   true  : mute on start, restore the prior volume on exit (default). Audio
#           already muted before start is left muted — the session never
#           unmutes what it did not mute.
#   false : leave the volume untouched.
# Ignored entirely in overlay_style 'none'. Per-run override: --mute=true|false
# mute: true

# --- focus -------------------------------------------------------------------
# Do Not Disturb Focus (opt-in).
#   false : leave Focus untouched (default).
#   true  : toggle the 'dndmode-on' / 'dndmode-off' Shortcuts, which sync DND
#           across your Apple devices via iCloud. Those two Shortcuts must
#           already exist (see README "First-run setup") or startup aborts with
#           exit code 6.
# Ignored entirely in overlay_style 'none'. Per-run override: --focus=true|false
# focus: false

# --- debug -------------------------------------------------------------------
# Console output gate.
#   false : SILENT (default). Nothing is printed to stdout / stderr; outcome is
#           reported through the exit code only. This is a security default —
#           in 'none' / 'glass' mode the terminal stays visible, so a startup
#           banner would otherwise leak the unlock code to a bystander.
#   true  : un-silence the full startup / cleanup banners and debug logging.
# Per-run equivalent: the --debug flag (either source enables output).
# debug: false
```

### The unlock code

`unlock_code` is a **sequence of steps typed one after another** - a passphrase,
not a single chord. dndmode watches the tail of everything you type and ends the
session the moment that tail equals your code. There is no prompt, no progress
indicator, no "wrong code" feedback, and no way to reset a half-typed attempt -
you just keep typing until it matches.

**Grammar.** Steps are separated by spaces; each step is `(<mod>+)*<key>`.
Modifiers inside a step are optional, so both `s` and `ctrl+s` are valid steps.
A literal space is only a separator - the space *key* is written `space`.

| Part | Accepted values |
| --- | --- |
| Modifiers (case-insensitive, optional per step) | `ctrl`, `option`, `cmd`, `shift` |
| Keys | `a`-`z`, `0`-`9`, `f1`-`f12`, `space`, `return` (alias `enter`), `tab`, `escape` (alias `esc`), `delete`, `forwarddelete`, `left`, `right`, `up`, `down`, and the punctuation `-` `=` `[` `]` `;` `'` `,` `.` `/` `\` `` ` `` |

```yaml
unlock_code: s w o r d f i s h     # a passphrase — the recommended shape
unlock_code: ctrl+s w o r d cmd+z  # modifiers on some steps, bare keys on others
unlock_code: Ctrl+Option+Cmd+X     # a single chord = a code of length 1
unlock_code: "- a b c d"           # leading punctuation must be quoted (see below)
```

**Quoting.** The value is a YAML scalar. If the code *starts* with `-`, `[`,
`]`, `'` or `` ` ``, wrap it in double quotes - unquoted, YAML parses those as
list or quote syntax and startup dies with a YAML error rather than a dndmode
one. The same characters are fine bare anywhere after the first position. A
` #` (space then hash) always starts a YAML comment and would silently cut the
code short. Since startup is silent without `--debug`, a quoting mistake shows
up only as exit `1`.

**Length rules.** Every keypress is a fresh match attempt, so short codes fall
fast. The numbers below assume an alphabet of ~36 (letters and digits) and an
automated typist at 100 keypresses/second.

| Steps | Status | Time to exhaust |
| --- | --- | --- |
| 1 | Accepted **only with modifiers** - the legacy `hotkey` shape. Weak, and warned about under `--debug`. | minutes, by hand |
| 2-3 | **Rejected** at startup (exit `1`) - too weak to be worth the illusion of a passphrase | ~8 minutes at 3 steps |
| 4-5 | Accepted, warned about under `--debug` | ~5 hours at 4 steps, ~7 days at 5 |
| **6-32** | Accepted, **recommended** | ~250 days at 6 steps, centuries at 8 |

Codes longer than 32 steps are rejected. A 1-step code without modifiers is
rejected too: a bare key would unlock on the first thing a bystander types.

**Which key is in effect.** `unlock_code` and the deprecated `hotkey` resolve
through one table, and setting both is a startup error - an ambiguous unlock
secret is not resolvable.

| `unlock_code` | `hotkey` | Result |
| --- | --- | --- |
| set | absent | Used. The normal path. |
| absent | set | Used as a 1-step code; `--debug` prints a deprecation warning. |
| set | set | **Error**, exit `1`. Delete the `hotkey` line. |
| absent | absent | **Error**, exit `1`. |

Migration is a rename, with one caveat: **spaces separate steps** in
`unlock_code`, so any spaces around the `+` have to go first. `hotkey: Ctrl + X`
is a legal chord but reads as three steps under `unlock_code`, and startup fails
with exit `1`.

**Three things that will trip you up:**

- **Steps are physical key positions, not characters.** The code is matched by
  key position so it behaves identically on a US, Russian, or AZERTY layout -
  but that also means `unlock_code: s w o r d` is typed on a Russian layout with
  the keys **ы ц о р в**. Pick your code by where your fingers go, not by what
  appears on screen.
- **A stray modifier breaks a step.** Each step is matched exactly. A step
  written without modifiers matches only a press with *no* modifiers held, so if
  you are resting on Cmd or Shift when you type it, that step does not count and
  the tail resets. Write the modifier into the step (`cmd+s`) if you intend to
  hold it.
- **Holding a key does not help.** Auto-repeat presses are dropped before they
  reach the matcher, so a held key contributes exactly one step. Genuine double
  letters (`hello`) are unaffected - auto-repeat only engages well past the speed
  of a real double tap.

Caps Lock, the numeric-keypad flag, the Fn flag, and the other system-set
modifier bits are stripped before matching, so a stray Caps Lock can never lock
you out. The Fn one matters more than it looks: macOS raises that bit for every
key of the function-key group - `f1`-`f12`, the arrows, `forwarddelete` -
whether or not you are holding Fn. Because it is stripped, the arrows and
`forwarddelete` work as plain steps (`unlock_code: s w up down` is fine) and
`fn` in a step is accepted but means nothing.

> **Do not put `f1`-`f12` in your unlock code.** Stripping the Fn *bit* is not
> enough for the F-row, because on the macOS default - "Use F1, F2, etc. keys as
> standard function keys" turned **off** in System Settings > Keyboard - the
> F-row is not a key press at all. It is delivered as a system media event
> (brightness, Mission Control, volume), which dndmode blocks but never records.
> A code containing `f1` parses, validates and starts without a single warning,
> and then cannot be typed: the only way in is to hold Fn for every F-step, or
> to turn that setting on beforehand. Get it wrong and the screen stays locked
> with no keyboard and no Ctrl-C - the way out is SSH from another machine or a
> hard power-off.

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
| `terminal` | Syntax-highlighted source code that types itself out and scrolls up, over an opaque black shield. Cosmetic. | No | Yes |
| `dvd` | A "DVD VIDEO" logo bounces around an opaque black shield, changing color on every edge hit. Cosmetic. | No | Yes |
| `glass` | Frosted `NSVisualEffectView`; the blurred desktop shows through. | Yes, by design | Yes |
| `none` | No overlay. Awake-only mode - see below. | n/a | No |

`glass` is the only style that is not opaque. It trades the no-bleed-through
guarantee for the look; input is still fully blocked underneath.

**`terminal`.** A second animated style for anyone who finds `matrix` too loud. It
renders a scrolling stream of fake source code - lines type out one character at a
time (~160 WPM) behind a blinking caret, then jump-scroll up as new lines arrive,
with light syntax highlighting over a dark editor palette, in a centred code
column. Like `matrix`, it is a purely cosmetic content swap on top of the same
opaque black shield, so every blocking guarantee (no bleed-through, HID input lock,
shield window level) is identical to `black`. The scrolling text is fully synthetic
and compiled in - no real file, project, or system data is ever read or shown, and
no line shows a package/module/import declaration - and the animation is ambient: it
never reacts to input, so it leaks no signal that keystrokes are being intercepted.

Pick the language two ways (same precedence as glass `glass_blur` vs
`--style glass:<radius>`): set `terminal_language:` in `config.yml` for the
default, and/or append `--style terminal:<lang>` to override it for a single run.
Valid values are `go` (default), `python`, `typescript`, and `rust`; a bare
`--style terminal` with no config key renders Go. Each language has its own
compiled-in corpus, large enough that the stream does not repeat for over two
hours, and its own syntax highlighting.

```
dndmode --style terminal            # Go (default)
dndmode --style terminal:python
dndmode --style terminal:typescript
dndmode --style terminal:rust
```

**`dvd`.** The old-DVD-player screensaver: the DVD-VIDEO logo drifts diagonally
across the shield, bounces off every edge, and cycles to the next color in a neon
palette on each bounce - and when it lands exactly in a corner it flashes white for
a moment (the payoff everyone waited for). The logo itself is the real mark,
compiled into the binary as a monochrome mask (no runtime file, no network) and
recolored on the fly, so it is a faithful silhouette rather than a hand-drawn
lookalike. Like `matrix` and `terminal` it is a purely cosmetic content swap on top
of the same opaque black shield, so every blocking guarantee (no bleed-through, HID
input lock, shield window level) is identical to `black`, and the animation is
ambient - it never reacts to input. Each display gets its own logo bouncing on its
own path. There are no knobs; it is just `dndmode --style dvd` (or
`overlay_style: dvd`).

**Awake-only mode (`none`).** `overlay_style: none` (or `dndmode --style none`) turns
dndmode into a thin [`caffeinate(8)`](https://ss64.com/mac/caffeinate.html) wrapper.
It does not draw an overlay, does not block the keyboard or trackpad, does not mute
audio, and does not touch Focus - so it needs no Accessibility permission. It only
holds a system-awake assertion for as long as it runs. Under the hood it runs
`caffeinate -d -i -s -w <pid>` (the `-d` is dropped when `allow_display_sleep: true`);
`-w <pid>` ties the assertion to dndmode's lifetime so it self-releases even after a
`kill -9`. There is no unlock code in this mode - there is no event tap to observe one -
so you exit with `Ctrl-C` or `--timer`.

## Exit codes

dndmode's exit code is its primary contract. In the default silent mode it is the
only thing it tells you.

| Code | Meaning |
| --- | --- |
| `0` | Clean exit via the unlock code, a signal, or `--timer` expiry. |
| `1` | Config error: bad YAML, an invalid or ambiguous unlock code, or an invalid flag value. |
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
- A short unlock code against someone willing to sit and type. Matching is on the
  tail of the keystroke stream, so every keypress is a fresh attempt and there is
  no lockout or rate limit. A 1-step code (the shipped default) or a 4-step one
  is minutes-to-hours of effort; see the length table under
  [Configuration](#the-unlock-code).

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

### The unlock code is rejected at startup (exit 1)

dndmode exits `1` before touching any permission or display. Run it with
`--debug` to see which check failed - the message names the offending config
key and the 1-based position of the bad step, and never echoes the value.

| Message | Cause |
| --- | --- |
| `both unlock_code and the deprecated hotkey` | Delete the `hotkey` line. |
| `sets neither unlock_code nor hotkey` | Add an `unlock_code` line. |
| `step N: hotkey: unknown token` | Step N is not a key name. Only US-ANSI names are accepted (`x`, `f1`, `space`) - see the key table in [The unlock code](#the-unlock-code). |
| `... is too short` | 2-3 steps are rejected outright. Use 4 or more; 6 or more is recommended. |
| `must carry at least one modifier` | A 1-step code has to be a chord - a bare key would unlock on the first thing a bystander types. |
| `too many steps` | More than 32 steps. |

### dndmode starts but the code never unlocks

There is no feedback by design: a wrong code looks exactly like no input at
all. Exit with `Ctrl-C` in the terminal that launched it, then check, in order:

1. **Layout.** Steps are physical key positions - `s w o r d` is typed **ы ц о
   р в** on a Russian layout.
2. **A modifier you are holding.** A step written without modifiers matches
   only a press with none held. Resting on Cmd or Shift silently breaks it.
3. **The count.** `--debug` prints `unlock_code=N steps (source=…)` at startup
   without printing the code itself. If N is not what you expect, the file is
   not the one you edited.
4. **Keep typing.** There is no reset and no timeout - matching is on the tail
   of everything typed, so a mistake costs you nothing but a retype.

Caps Lock, Num Lock and Fn are stripped before matching and cannot be the
cause.

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
internal/config/      YAML config + unlock-code sequence parser
internal/matcher/     pure-Go sliding-window unlock-code matcher
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
- **`glass` bleeds through.** It shows a blurred desktop on purpose. Use `black`,
  `matrix`, or `terminal` if you need the desktop fully hidden.
- **Foreground only.** No daemon or launchd mode. The terminal that launched dndmode
  must stay open.
- **Needs an active display.** dndmode shields the screens you can see; it does not
  keep a MacBook running with the lid shut. With the lid closed and no external
  monitor there is nothing to draw on, so startup aborts with exit `2` (`no displays
  detected`). The awake-lock also does not defeat clamshell sleep - closing the lid
  on battery still sleeps the Mac. Run it lid-open, or in clamshell with an external
  display on AC power.
- **One instance at a time.** A second dndmode exits `5` with instructions.
- **Signing.** Recent macOS refuses unsigned binaries. `make build` applies ad-hoc
  codesigning; `go install` relies on Go's linker-signed signature, which launches
  but is not stable enough for TCC across upgrades.

## License

Released under the MIT License. See [LICENSE](LICENSE) for the full text.

© 2026 Dmitriy Basenko.
