//go:build darwin

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// This file keeps an EXISTING config.yml current as dndmode gains keys across
// releases. It is a separate concern from Load (which must work with any
// config, old or new) and from rewrite.go (which edits the secret).
//
// # Why anything is needed at all
//
// A missing key has never been a failure: every key added since v1 normalizes
// from absent to a documented default, and yaml.Strict() rejects only unknown
// keys, never absent ones. So an untouched config from the first release still
// starts today, and that property is not what this file exists to provide.
//
// What an old config loses is the DOCUMENTATION. config.yml is written once,
// at first run, and is roughly ninety percent commented explanation — for most
// users it is the only documentation they will ever read. A user who installed
// early and upgraded ten times has a file that describes the flags of the
// version they started with, and no way to discover the rest short of the
// README. Appending the missing sections is what makes the file catch up.
//
// # The one safe edit
//
// Migration ONLY appends commented-out sections. It never edits, reorders,
// reformats or uncomments an existing line, and it never writes a value.
//
// That restraint is not caution for its own sake — each of those operations is
// specifically unsafe here:
//
//   - Uncommenting a key would INVERT its meaning. Several defaults are the
//     defaults of an ABSENT key, not of a written zero value: `mute` absent is
//     true while `mute:` written empty is not, and `overlay_style` absent is
//     black. Writing out what the user left implicit silently changes
//     behaviour.
//   - Reformatting through goccy would destroy every comment in the file (the
//     reason rewrite.go does line surgery instead), which is exactly the
//     documentation this is meant to restore.
//   - A backup copy would be a second file containing whatever plaintext
//     secret the original holds, which is what --set-password exists to
//     remove. There is no .bak here for the same reason there is none there.
//
// Appending comments cannot change how a config parses, which is what makes it
// safe to do automatically at startup rather than behind a command the user
// has to know exists.

// migratableKeys are the config keys whose template sections may be appended
// to an existing file, in the order they should appear.
//
// unlock_code, unlock_salt and unlock_hash are deliberately ABSENT. The first
// is required and therefore always present; the other two are machine-written
// by --set-password and documenting them into a file that does not use them
// would invite hand-editing of a salted digest. `hotkey` is absent too — it is
// deprecated, and re-advertising it to someone who has already migrated off it
// would be a step backwards.
var migratableKeys = []string{
	"activate_hotkey",
	"overlay_style",
	"glass_blur",
	"terminal_language",
	"allow_display_sleep",
	"mute",
	"focus",
	"debug",
}

// sectionMarker is the prefix that opens a documentation block in
// defaultConfigTemplate, e.g. "# --- mute ------". Sections run from one
// marker to the next, which is what lets a whole block be lifted out intact,
// heading and trailing blank line included.
const sectionMarker = "# --- "

// renderedTemplate is defaultConfigTemplate with its single %s filled in.
// Sections are cut from the RENDERED text so an appended block never carries a
// stray format verb into a user's file.
func renderedTemplate() string {
	return fmt.Sprintf(defaultConfigTemplate, DefaultUnlockCode)
}

// templateSections splits the rendered template into key -> block, where the
// key is taken from the marker heading ("# --- mute ---" => "mute").
func templateSections() map[string]string {
	sections := make(map[string]string)
	var currentKey string
	var current []string

	flush := func() {
		if currentKey != "" {
			sections[currentKey] = strings.TrimRight(strings.Join(current, "\n"), "\n") + "\n"
		}
	}

	for line := range strings.SplitSeq(renderedTemplate(), "\n") {
		if after, ok := strings.CutPrefix(line, sectionMarker); ok {
			flush()
			// "mute ---------" => "mute"; the heading may also carry a
			// parenthetical, e.g. "activate_hotkey (watch mode only) ---".
			currentKey, _, _ = strings.Cut(strings.TrimSpace(after), " ")
			current = []string{line}
			continue
		}
		if currentKey != "" {
			current = append(current, line)
		}
	}
	flush()
	return sections
}

// mentionsKey reports whether raw already talks about key — either as a live
// setting (`mute: false`) or as a commented template line (`# mute: true`).
//
// Both count as "present", and that is the point: a user who read the
// documentation and left the key commented out has the documentation. Treating
// only live keys as present would re-append every block on every upgrade.
//
// The scan is line-oriented and anchored, so a key named inside prose — and
// this template is mostly prose — cannot be mistaken for a setting.
func mentionsKey(raw []byte, key string) bool {
	want := key + ":"
	for line := range bytes.Lines(raw) {
		trimmed := bytes.TrimSpace(line)
		// Strip any run of leading '#' plus spaces, so "#mute:", "# mute:" and
		// "#   mute:" are all recognised.
		trimmed = bytes.TrimLeft(trimmed, "#")
		trimmed = bytes.TrimSpace(trimmed)
		if bytes.HasPrefix(trimmed, []byte(want)) {
			return true
		}
	}
	return false
}

// MissingSections returns the migratable keys that raw does not mention yet,
// in template order. An empty result means the config is current.
func MissingSections(raw []byte) []string {
	var missing []string
	for _, key := range migratableKeys {
		if !mentionsKey(raw, key) {
			missing = append(missing, key)
		}
	}
	return missing
}

// AppendMissingSections returns raw with the documentation blocks for every
// missing key appended, plus the list of keys that were added. When nothing is
// missing it returns raw unchanged and a nil slice, which is the signal to
// skip the write entirely.
//
// The appended text is byte-for-byte the same block a fresh config would have
// received, so a migrated file and a newly created one document a key
// identically — there is no second wording to keep in sync.
func AppendMissingSections(raw []byte) ([]byte, []string) {
	missing := MissingSections(raw)
	if len(missing) == 0 {
		return raw, nil
	}

	sections := templateSections()
	var buf bytes.Buffer
	buf.Write(raw)

	// Exactly one blank line between what was there and what is being added,
	// regardless of how the user's file happened to end.
	if !bytes.HasSuffix(raw, []byte("\n")) {
		buf.WriteByte('\n')
	}

	added := make([]string, 0, len(missing))
	for _, key := range missing {
		block, ok := sections[key]
		if !ok {
			// A key listed in migratableKeys with no matching template
			// section. Skipping keeps a mismatch from writing a truncated
			// file; TestMigratableKeys_AllHaveTemplateSections fails the build
			// instead of letting it reach a user.
			continue
		}
		buf.WriteString("\n")
		buf.WriteString(block)
		added = append(added, key)
	}
	if len(added) == 0 {
		return raw, nil
	}
	return buf.Bytes(), added
}

// MigrateFile brings the config at path up to date in place, returning the
// keys whose documentation was appended. A current file is left untouched and
// reports no keys and no error.
//
// The write is the same atomic temp+rename writeAtomic gives every other
// publisher of this file, so a crash mid-migration leaves the original intact
// rather than a half-written config.
//
// Callers treat a returned error as advisory. Migration adds comments to a
// file that already works; refusing to start over it would trade a cosmetic
// shortfall for a total one.
func MigrateFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config for migration: %w", err)
	}

	migrated, added := AppendMissingSections(raw)
	if len(added) == 0 {
		return nil, nil
	}

	// Prove the edit changed nothing but comments before it reaches the disk.
	// The claim "appending comments cannot change how a config parses" is the
	// entire safety argument for doing this automatically at startup, so it is
	// verified rather than asserted — the same discipline verifyRewrite
	// applies to the secret.
	//
	// Whole-struct comparison rather than a field list, for the reason
	// withoutSecret gives: a field added later is covered without anyone
	// remembering to extend this.
	before, err := parseStrict(raw)
	if err != nil {
		// The file did not parse BEFORE the migration either, so this is not
		// something the migration broke. Leave it alone and let Load produce
		// the real diagnostic, which points at the offending line.
		return nil, fmt.Errorf("config does not parse; not migrating: %w", err)
	}
	after, err := parseStrict(migrated)
	if err != nil {
		return nil, fmt.Errorf("migrated config would not parse; left unchanged: %w", err)
	}
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New("migration would have changed a setting, not just comments; left unchanged")
	}

	if err := writeAtomic(path, migrated); err != nil {
		return nil, fmt.Errorf("write migrated config: %w", err)
	}
	return added, nil
}
