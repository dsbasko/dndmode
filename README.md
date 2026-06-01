# dndmode

> Lock your unattended Apple Silicon MacBook without interrupting background processes.

## What is dndmode

`dndmode` is a CLI utility for Apple Silicon macOS that locks the unattended MacBook
(keyboard/trackpad blocked, black overlay on every connected display) without
interrupting background processes such as AI agents. The target user is a developer
running an agent in YOLO mode who needs to step away from the keyboard without
killing the long-running task and without leaving the machine wide open to a passerby.

## How it works

- Per-screen `NSWindow` overlay drawn at `CGShieldingWindowLevel()` covers every
  connected display (built-in + external) and survives Mission Control / Spaces /
  full-screen apps.
- HID-level `CGEventTap` intercepts keyboard, mouse, scroll, and media keys
  (NOTE: placeholder mock in v1.0 — see [Known limitations](#known-limitations-v10)).
- `IOPMAssertion` (`kIOPMAssertPreventUserIdleSystemSleep`) keeps the system awake
  for the entire session.
- Two macOS Shortcuts named `dndmode-on` and `dndmode-off` toggle Do Not Disturb
  Focus around the locked window so notifications do not leak.
- A configurable hotkey (default `Ctrl+Option+Cmd+X`) ends the locked state from
  the keyboard; `Ctrl-C` in the originating terminal also unwinds cleanly.

## Requirements

- macOS 14 Sonoma or newer.
- Apple Silicon (`arm64`) — no Intel support.
- Accessibility + Input Monitoring TCC permissions (granted on first run via
  System Settings).
- Two macOS Shortcuts named exactly `dndmode-on` and `dndmode-off` (setup
  instructions in [First-run setup](#first-run-setup)).
- Go 1.26+ if building from source.

## Install

Pick ONE install path and stay on it — mixing `go install` and `make install`
yields two separate binaries (`~/go/bin/dndmode` and `/usr/local/bin/dndmode`)
with different cdhashes, and TCC treats them as different apps (each needs its
own Accessibility + Input Monitoring grant).

- **From source (recommended for stable TCC across rebuilds):**
  ```bash
  git clone https://github.com/dsbasko/dndmode
  cd dndmode
  make install
  ```
  Builds with ad-hoc codesign identifier `com.dsbasko.dndmode` and copies the
  binary to `/usr/local/bin/dndmode` via `sudo cp`. Subsequent
  `git pull && make install` upgrades preserve TCC grants because the codesign
  identifier (and therefore cdhash) is stable across rebuilds. Always invoke
  `/usr/local/bin/dndmode` (ensure `/usr/local/bin` precedes `~/go/bin` in
  `$PATH`).
- **Quick (`go install`):** `go install github.com/dsbasko/dndmode@latest`
  installs the binary into `$(go env GOPATH)/bin/dndmode`. Each
  `go install ...@latest` rebuild changes the binary's cdhash → TCC re-prompts
  for Accessibility + Input Monitoring on every upgrade. **Caveat:** running
  `make install` from a clone afterwards does NOT fix the GOPATH-bin binary —
  it creates a SECOND binary at `/usr/local/bin/dndmode` with its own (stable)
  cdhash. If `~/go/bin` precedes `/usr/local/bin` in `$PATH`, you still execute
  the unstable GOPATH copy. Workaround: either delete the GOPATH copy
  (`rm "$(go env GOPATH)/bin/dndmode"`), put `/usr/local/bin` first in
  `$PATH`, or always invoke `/usr/local/bin/dndmode` explicitly. After every
  subsequent `go install ...@latest` upgrade, re-run `make install` to refresh
  the `/usr/local/bin` copy. See [Troubleshooting](#troubleshooting) for the
  cdhash / TCC mechanics.

Note: Homebrew is **not** supported in v1 (requires Apple Developer ID; deferred to
v2).

## First-run setup

1. Install dndmode (see [Install](#install)).
2. Run `dndmode` for the first time. It will prompt for Accessibility permission.
   Click **Open System Settings** and enable `dndmode` in
   **Privacy & Security → Accessibility**.
3. The polling loop will then ask for Input Monitoring. Same flow: enable
   `dndmode` in **Privacy & Security → Input Monitoring**.
4. Open the **Shortcuts** app. Create a new shortcut: add the **Set Focus**
   action → choose **Do Not Disturb** → **Turn On Until Turned Off** → save as
   `dndmode-on`. Repeat with **Turn Off** and save as `dndmode-off`.
5. Run `dndmode` again. You should see `dndmode: active. press Ctrl-C.` on stdout.
   The default hotkey `Ctrl+Option+Cmd+X` exits the locked state. Customize via
   `~/.config/dndmode/config.yml`.

## Usage

- **Start:** `dndmode` (foreground; the terminal blocks until the session ends).
- **Exit:** press the configured hotkey (default `Ctrl+Option+Cmd+X`), or `Ctrl-C`
  in the terminal where dndmode runs.
- **Configuration:** `~/.config/dndmode/config.yml` (created on first run with the
  default hotkey).

### Exit codes

| Code | Constant                  | Meaning                                                                      |
| ---- | ------------------------- | ---------------------------------------------------------------------------- |
| 0    | `exitOK`                  | Success (clean exit via hotkey or SIGINT).                                   |
| 1    | `exitConfigErr`           | Config error: bad YAML or modifier-only hotkey in `config.yml`.              |
| 2    | `exitPlatformErr`         | Platform error: not arm64, macOS < 14, or IOKit fundamentals failed.         |
| 3    | `exitPermissionDenied`    | SIGINT received while waiting for Accessibility / Input Monitoring grants.   |
| 4    | `exitSecureInputConflict` | Another app holds Secure Event Input (Terminal sudo, password fields, 1Password). |
| 5    | `exitConcurrentInstance`  | Another live `dndmode` instance detected (LIFE-12 / orphan IOPMAssertion).   |
| 6    | `exitFocusSetup`          | Required Shortcuts `dndmode-on` or `dndmode-off` not found.                  |
| 7    | `exitRuntimeJSON`         | Cannot delete stale `~/.config/dndmode/runtime.json` (permission / IO).      |

## Threat model

### What dndmode DOES protect against

- Casual passerby trying to interact with unlocked MacBook
- Family member / colleague clicking around while AI agent runs
- Display power button / Mission Control / Cmd+Tab probing
- macOS Focus notifications leaking info during DND

### What dndmode does NOT protect against

- Touch ID / biometric unlock (impossible to block without root)
- Power button hold (hard shutdown — out of scope)
- Recovery mode (Cmd+R on boot — out of scope)
- Hardware key-loggers / DMA via Thunderbolt
- Malware with root privileges
- Physical access >5 minutes
- Remote SSH / VNC sessions (target is local console only)

### Per-component coverage

| Component             | Protects against                         | Limitations                                                  |
| --------------------- | ---------------------------------------- | ------------------------------------------------------------ |
| Overlay (Phase 2)     | Visual access to desktop                 | Bypassable via Cmd+Tab in v1 (Phase 4 closes this).          |
| HID tap (Phase 4)     | Keyboard + mouse + scroll + media        | NOT IMPLEMENTED in v1 — placeholder mock.                    |
| IOPM Assertion (Phase 3) | System idle sleep                     | Display can still sleep (intentional).                       |
| Focus (Phase 5)       | Notification banners                     | DND only — does not silence audio.                           |

**Disclaimer:** dndmode is a soft-lock for cooperative environments, not red-team-grade
hardware protection. Use at your own risk.

The binary has zero network dependencies — verify with `make audit-net` after install
(DIST-04 invariant).

## Troubleshooting

### TCC permissions broken after `go install` upgrade

Each `go install github.com/dsbasko/dndmode@latest` rebuild changes the
binary's cdhash, which TCC (macOS privacy database) uses for identity.
Without stable codesign, TCC sees a "new app" and revokes Accessibility
+ Input Monitoring on every upgrade.

**Workaround 1 (recommended):** Use `make install` after `go install`.
This re-applies the stable ad-hoc codesign with identifier
`com.dsbasko.dndmode`, preserving TCC permissions across rebuilds.

**Workaround 2 (nuclear):** Reset TCC entries and re-grant:

```bash
tccutil reset Accessibility com.dsbasko.dndmode
tccutil reset ListenEvent com.dsbasko.dndmode
./dndmode  # will re-prompt for permissions
```

Apple Developer ID (planned for v2) eliminates this issue entirely.

### Required Shortcuts not found (exit 6)

Re-create `dndmode-on` / `dndmode-off` via the Shortcuts app — see
[First-run setup](#first-run-setup) step 4. Empirical: `shortcuts run "<missing>"`
exits with status 1; dndmode reports this as exit 6 with the create-shortcut guide
on stderr.

### Secure Event Input conflict (exit 4)

Find the app holding SecureInput (typically a Terminal sudo prompt, password
manager, or active password field), dismiss it, then re-run dndmode. To inspect:

```bash
ioreg -l -w 0 | grep SecureInput
```

### Another instance is already active (exit 5)

Find the running dndmode and signal it to exit:

```bash
pgrep -x dndmode      # find the PID(s)
pkill -TERM dndmode   # ask it to exit cleanly
```

Or wait for it to exit normally, then re-run.

### Cannot delete stale runtime file (exit 7)

Manually clean up, then re-run:

```bash
rm -f ~/.config/dndmode/runtime.json
```

Causes: read-only filesystem, ACL denying delete, or disk full.

### Cocoa smoke tests panic with NSWindow main-thread error

These tests are gated by the `manual` build tag in v1.0. `go test ./...` (default)
skips them. To run intentionally from a GUI session:

```bash
go test -tags=manual ./internal/macos/cocoa/...
```

### Uninstall

```bash
sudo rm /usr/local/bin/dndmode
# Optional: also reset TCC entries
tccutil reset Accessibility com.dsbasko.dndmode
tccutil reset ListenEvent com.dsbasko.dndmode
```

## Known limitations (v1.0)

- **Keyboard / mouse blocking is NOT implemented in v1.0** — the input layer is
  currently a placeholder mock-tap. `Cmd+Tab`, `Cmd+Q`, and other system shortcuts
  still function. The visual overlay is in place, but a determined local user can
  switch apps. Phase 4 (CGEventTap-based input blocking) is the planned scope of
  v1.1.
- No prior-Focus snapshot/restore — after exit, Focus is always set to "no focus"
  (deliberate v1 limitation, FOC-04). v2-FOC-snapshot will add restore.
- No daemon mode — foreground process only. The terminal where dndmode launched
  must stay open. v2 may add launchd integration.
- No audio mute — Focus DND suppresses notification *banners* but not sounds.
  Background music or system beeps still play (intentional: AI agents may need to
  vocalize).
- macOS Sequoia 15.x signing requirement — unsigned binaries refuse to launch.
  `make build` applies ad-hoc codesign automatically; `go install` relies on Go's
  linker-signed signature (sufficient to launch, but not for TCC stability — see
  [Troubleshooting](#troubleshooting)).
- Two `dndmode` instances cannot run concurrently — the second instance exits 5
  with instructions (LIFE-12 enforcement).

## License

dndmode is released under the MIT License. See [LICENSE](./LICENSE) for full text.

© 2026 Dmitriy Basenko.
