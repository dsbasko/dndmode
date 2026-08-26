//go:build darwin

package main

import (
	"flag"
	"io"
)

// usageText is what --help prints. Hand-written rather than
// flag.PrintDefaults for the same reason --status has a fixed shape: the
// reader wants the three ways to run the program and the handful of flags,
// grouped and aligned, not an alphabetical dump of every option with the
// binary's absolute path on top.
//
// Every registered flag must appear here as --<name>; Test_usageText_NamesEveryFlag
// walks the FlagSet to enforce it, so a flag added to defineFlags without a
// line below fails the build's tests rather than silently going undocumented.
const usageText = `dndmode - lock an unattended Mac without stopping the work it is doing.

Covers every display with a full-screen shield, blocks the keyboard and trackpad,
keeps the Mac awake and mutes it; background processes keep running. The shield
comes down when the unlock code from the config is typed - silently, no prompt.

Usage:
  dndmode [flags]            lock now; runs until the unlock code, a signal or --timer
  dndmode --watch [flags]    wait in the background for activate_hotkey, lock on each press
  dndmode --status           report the background watch process (exit 0 running, 9 not)
  dndmode --kill             stop the background watch process
  dndmode --set-password     capture a new unlock code and store it hashed in the config

Flags (per run; omitted = the config value):
  --style <look>    black | matrix | terminal[:go|python|typescript|rust|ys] | dvd |
                    glass[:radius] | none
  --mute <bool>     mute system audio for the session (true|false)
  --focus <bool>    turn Do Not Disturb on for the session (true|false)
  --timer <dur>     auto-unlock after a Go duration (30m, 1h30m, 90s), then exit 0
  --debug           print banners, diagnostics and logs; default is silent, exit codes only

--watch takes the same flags and applies them to every session it raises.
--status, --kill and --set-password take none of them except --debug.

Exit codes:
  0  ok                             5  another dndmode is running
  1  config or flag error           6  Shortcuts dndmode-on/off missing
  2  platform error                 7  state file or lock unusable
  3  permission wait aborted        8  internal panic
  4  secure input held / tap lost   9  --status: not watching

Config:  ~/.config/dndmode/config.yml (created on the first run; --help creates nothing)
Docs:    https://github.com/dsbasko/dndmode
`

// printUsage is installed as flag.Usage. The flag package calls it for -h /
// --help (then exits 0) and after a parse error (then exits 2), on the
// FlagSet's output — stderr unless a test redirects it.
func printUsage() {
	_, _ = io.WriteString(flag.CommandLine.Output(), usageText)
}
