//go:build darwin

// Package config loads and writes the dndmode YAML configuration. The
// config schema in v1 is intentionally minimal (just `hotkey`); migration
// to nested/versioned schema is deferred (the design notes).
//
// Hot-reload is NOT supported: Load() is invoked exactly once at
// PreFlight. Loader has no Watch/Reload/Subscribe methods by design.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

const (
	// DefaultHotkey is the hotkey written to a freshly-created config.yml
	//. User can edit the file post-creation.
	DefaultHotkey = "Ctrl+Option+Cmd+X"

	// configDirPerm is 0o700 — owner read/write/execute only (
	// mitigation: world cannot read user config).
	configDirPerm fs.FileMode = 0o700
	// configFilePerm is 0o600 — owner read/write only.
	configFilePerm fs.FileMode = 0o600
)

// Config is the v1 dndmode configuration schema. Add fields cautiously —
// forward-compat trojan keys are rejected by yaml.Strict().
type Config struct {
	Hotkey string `yaml:"hotkey"`
}

// Loader reads a single YAML file at a fixed path. NewLoader does not touch
// disk; only Load() performs IO. Loader is single-use semantically; calling
// Load() multiple times will re-read the file each time, but this is NOT a
// hot-reload mechanism: production callers invoke Load() once at
// PreFlight only.
type Loader struct {
	path string
}

// NewLoader constructs a Loader for the given absolute path. The path is
// not validated until Load() is called.
func NewLoader(path string) *Loader {
	return &Loader{path: path}
}

// Path returns the configured path (for diagnostics / banner output).
func (l *Loader) Path() string { return l.path }

// Load returns the parsed config. If the file does not exist, it writes a
// default config to disk (creating parent dirs as needed) and returns the
// default with created=true. On YAML syntax error, returns a wrapped error
// whose message contains the goccy-formatted line:col + source snippet
//.
func (l *Loader) Load() (Config, bool, error) {
	raw, err := os.ReadFile(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		def := Config{Hotkey: DefaultHotkey}
		if werr := writeDefault(l.path, def); werr != nil {
			return Config{}, false, fmt.Errorf("write default config: %w", werr)
		}
		return def, true, nil
	case err != nil:
		return Config{}, false, fmt.Errorf("read config %s: %w", l.path, err)
	}

	var cfg Config
	// yaml.Strict() rejects unknown YAML keys (mitigation:
	// forward-compat trojan keys cannot smuggle behavior into v1).
	if perr := yaml.UnmarshalWithOptions(raw, &cfg, yaml.Strict()); perr != nil {
		// goccy pretty errors with line:col + source snippet.
		// color=false in v1 (P1.6 — TTY detection deferred to Phase 6).
		pretty := yaml.FormatError(perr, false /*colored*/, true /*inclSource*/)
		return Config{}, false, fmt.Errorf("parse config %s:\n%s", l.path, pretty)
	}
	return cfg, false, nil
}

// writeDefault creates the parent directory (0o700) and writes the default
// config via atomic tmp+rename (protects against concurrent dndmode
// starts; the loser of the rename race still ends up with a valid file).
func writeDefault(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirPerm); err != nil {
		return fmt.Errorf("mkdir parent %s: %w", dir, err)
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal default: %w", err)
	}
	// Use a PID-suffixed tmp name so that two dndmode processes racing on
	// a fresh path do not stomp on each other's tmp file. The rename step
	// is atomic on APFS, so the loser's rename simply replaces the winner's
	// already-published file with an identical body — readers always see a
	// fully-written file.
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, body, configFilePerm); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
