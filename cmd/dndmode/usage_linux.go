//go:build linux

package main

import (
	"flag"
	"io"
)

// usageText is the Linux --help. Same commands as on macOS; the differences
// are the supported styles, the modifier names and what "focus" means.
const usageText = `dndmode - lock an unattended Linux desktop without stopping the work it is doing.

Covers every monitor with a black shield, grabs the keyboard, mouse and touchpad
(evdev), keeps the session awake and mutes it; background processes keep running.
The shield comes down when the unlock code from the config is typed - silently,
no prompt.

Usage:
  dndmode [flags]            lock now; runs until the unlock code, a signal or --timer
  dndmode --watch [flags]    wait in the background for activate_hotkey, lock on each press
  dndmode --status           report the background watch process (exit 0 running, 9 not)
  dndmode --kill             stop the background watch process
  dndmode --set-password     capture a new unlock code and store it hashed in the config

Flags (per run; omitted = the config value):
  --style <look>    black | dvd | matrix | terminal[:<lang>] | gold | bamboo |
                    dino | glass[:radius] | none;
                    <lang> = go|python|typescript|rust|ys|sql|psql|mysql|oracle|
                             varaq|elvish|php
  --mute <bool>     mute the default audio output for the session (true|false)
  --focus <bool>    hide GNOME notification banners for the session (true|false)
  --timer <dur>     auto-unlock after a Go duration (30m, 1h30m, 90s), then exit 0
  --debug           print banners, diagnostics and logs; default is silent, exit codes only

--watch takes the same flags and applies them to every session it raises.
--status, --kill and --set-password take none of them except --debug.

Keys: the config grammar is shared with macOS - option is Alt, cmd is Super.
Access: the user must be in the 'input' group (sudo usermod -aG input $USER,
then log out and back in).

Exit codes:
  0  ok                             5  another dndmode is running
  1  config or flag error           6  GNOME notification settings unavailable
  2  platform error                 7  state file or lock unusable
  3  no access to /dev/input        8  internal panic
  4  input device grabbed elsewhere 9  --status: not watching

Config:  ~/.config/dndmode/config.yml (created on the first run; --help creates nothing)
Docs:    https://github.com/dsbasko/dndmode
`

// printUsage writes usageText to the flag package's output.
func printUsage() {
	_, _ = io.WriteString(flag.CommandLine.Output(), usageText)
}
