//go:build darwin

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
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
// # The two safe edits
//
// Migration does exactly two things, and nothing else:
//
//  1. APPEND a commented-out documentation section for a key the file does not
//     mention yet.
//  2. RESPELL a value whose name changed between releases, from the table in
//     legacyValues — in the live setting line and in the documentation block
//     that describes it.
//
// It never reorders, reformats or uncomments an existing line, and it never
// writes a value the file did not already carry in some spelling.
//
// That restraint is not caution for its own sake — each of the excluded
// operations is specifically unsafe here:
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
// # Why a rename is still "changes nothing"
//
// Appending comments cannot change how a config parses at all. A respelling
// obviously can — `terminal_language: yc` and `terminal_language: ys` are
// different bytes — so it carries a narrower claim: it changes a value's
// SPELLING, never its MEANING, where meaning is whatever the Normalize*
// function for that key says it is. Both spellings normalize to the same
// setting, which is exactly why the old one is still accepted (see
// legacyTerminalLangYopta) and why the file can be brought up to date without
// asking anyone.
//
// MigrateFile proves that claim rather than asserting it, the same way it
// proves the append is inert: it parses before and after, folds both through
// foldLegacyValues, and refuses to write on any remaining difference. So the
// automatic startup edit is bounded by what the loader itself considers
// equivalent, not by how carefully the line surgery below was written.

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

// legacyValue is one renamed VALUE of one config key: the spelling some earlier
// release wrote into config.yml, and the spelling this one uses. The key is the
// YAML name, so the table doubles as the map from a rename to the struct field
// it may legitimately change (see foldLegacyValues).
type legacyValue struct {
	key  string
	from string
	to   string
}

// legacyValues is the complete set of value renames migration knows how to
// perform. It is a TABLE rather than a special case in the code because the
// safety proof reads from it: every difference it explains is permitted between
// the config before and after a migration, and every other difference is a bug
// that stops the write.
//
// Adding an entry therefore means three things, none optional: the loader must
// still ACCEPT `from` (a config nobody could migrate — no write permission, a
// busy publish lock, a symlink into a read-only checkout — has to keep working),
// the Normalize* function for that key must fold `from` into `to` (so nothing
// downstream ever sees two spellings), and `from` must be word-like, because the
// documentation pass below matches it on ASCII word boundaries.
//
// Removing an entry is a separate, later decision: it drops support for the old
// spelling outright, so it belongs in a release that can say so, not in the one
// that does the rename.
var legacyValues = []legacyValue{
	{key: "terminal_language", from: legacyTerminalLangYopta, to: TerminalLangYopta},
}

// liveKeyRe matches a line that SETS key at top level, splitting it into the
// "key:" prefix and everything after it.
//
// Anchored at column 0 with no leading-whitespace allowance, for the reason
// secretKeyRe is: an indented key belongs to some nested mapping this surgery
// does not understand, and a comment line starts with '#' rather than the key,
// so the anchor separates live settings from documentation for free. Whitespace
// before the colon is allowed because `terminal_language : ys` is YAML the
// loader accepts.
func liveKeyRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^(` + regexp.QuoteMeta(key) + `[ \t]*:[ \t]*)(.*)$`)
}

// wordRe matches word standing alone — not run together with an ASCII letter,
// digit or underscore on either side. It is what keeps the documentation pass
// from rewriting `yc` inside a longer word while still catching it after a
// colon (`terminal:yc`) or before a period (`... or yc.`).
func wordRe(word string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`)
}

// RenameLegacyValues returns raw with every legacy value spelling replaced by
// its current one, plus the keys it touched, in table order. Nothing to rename
// returns raw unchanged and a nil slice, which is the signal to skip the write.
//
// Two different edits, deliberately kept apart:
//
//   - The LIVE line (`terminal_language: yc`) has its value replaced, and only
//     when the value is exactly the legacy spelling. Indentation, quoting style
//     and any trailing comment survive verbatim, so the diff a user sees in a
//     dotfiles repo is the two characters that changed.
//   - COMMENT lines inside that key's own `# --- key ---` documentation block
//     get the word replaced. This is the block that explains what the values
//     mean, and leaving it describing a spelling the template no longer uses
//     would defeat the reason migration exists at all. The scope is the section
//     rather than the file, so a note the user wrote elsewhere is never touched.
//
// Pure, like RewriteSecretAsHash and for the same reason: it is the table of
// YAML shapes in the tests, not the caller, that specifies what the surgery
// does. Line terminators are preserved per line (splitLines), so a CRLF config
// stays CRLF.
func RenameLegacyValues(raw []byte) ([]byte, []string) {
	lines, ends := splitLines(raw)
	renamed := make([]string, 0, len(legacyValues))

	for _, lv := range legacyValues {
		keyRe, docRe := liveKeyRe(lv.key), wordRe(lv.from)
		touched := false
		section := ""

		for i, ln := range lines {
			if after, ok := strings.CutPrefix(ln, sectionMarker); ok {
				section, _, _ = strings.Cut(strings.TrimSpace(after), " ")
			}
			if strings.HasPrefix(strings.TrimSpace(ln), "#") {
				if section != lv.key {
					continue
				}
				if out := docRe.ReplaceAllString(ln, lv.to); out != ln {
					lines[i] = out
					touched = true
				}
				continue
			}
			if out, ok := respellSetting(ln, keyRe, lv); ok {
				lines[i] = out
				touched = true
			}
		}
		if touched {
			renamed = append(renamed, lv.key)
		}
	}

	if len(renamed) == 0 {
		return raw, nil
	}

	var buf bytes.Buffer
	for i, ln := range lines {
		buf.WriteString(ln)
		buf.WriteString(ends[i])
	}
	return buf.Bytes(), renamed
}

// respellSetting rewrites a live `key: <legacy>` line to `key: <current>`,
// reporting whether it matched. Anything that is not that exact shape — a
// different key, a different value, a flow mapping, a quoted scalar with a '#'
// inside it — falls through unchanged rather than being guessed at. Not
// matching is always safe: the old spelling still loads, so the worst outcome
// is a file that gets respelled on some later release instead of this one.
func respellSetting(line string, keyRe *regexp.Regexp, lv legacyValue) (string, bool) {
	m := keyRe.FindStringSubmatch(line)
	if m == nil {
		return line, false
	}
	value, comment := splitInlineComment(m[2])
	trimmed := strings.TrimRight(value, " \t")
	pad := value[len(trimmed):]

	quote := ""
	if len(trimmed) >= 2 && (trimmed[0] == '"' || trimmed[0] == '\'') &&
		trimmed[len(trimmed)-1] == trimmed[0] {
		quote = trimmed[:1]
		trimmed = trimmed[1 : len(trimmed)-1]
	}
	if trimmed != lv.from {
		return line, false
	}
	return m[1] + quote + lv.to + quote + pad + comment, true
}

// splitInlineComment splits the text after `key:` into the scalar and the
// trailing comment, comment included from its '#'. YAML only starts a comment
// at a '#' that opens the text or follows whitespace, which is the rule applied
// here — `a#b` is a scalar, not a value plus a comment.
//
// A '#' inside a quoted scalar would be split wrongly, and that is harmless by
// construction: the value extracted from such a line cannot equal a legacy
// spelling, so respellSetting declines and copies the line through untouched.
func splitInlineComment(s string) (value, comment string) {
	for i := range len(s) {
		if s[i] != '#' {
			continue
		}
		if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// foldLegacyValues returns cfg with every legacy spelling in legacyValues
// folded into its current one, which is the precise sense in which a rename
// changes nothing: two configs that differ only by a legacy spelling fold to
// the same struct.
//
// Driven off the legacyValues table through the yaml tags rather than a
// hand-written field list, so the set of differences the proof forgives is
// exactly the set of renames the surgery is allowed to make — a new entry
// cannot widen one without widening the other.
func foldLegacyValues(cfg Config) Config {
	v := reflect.ValueOf(&cfg).Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := v.Field(i)
		// The IsExported guard is not decoration: SetString on an unexported
		// field panics, and this runs inside a startup migration whose every
		// other failure mode is a logged skip.
		if f.Kind() != reflect.String || !t.Field(i).IsExported() {
			continue
		}
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		for _, lv := range legacyValues {
			if lv.key == name && f.String() == lv.from {
				f.SetString(lv.to)
			}
		}
	}
	return cfg
}

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

// MigrationResult reports what MigrateFile changed. The two lists are kept
// apart because they are different events for a reader of the debug log: gaining
// documentation for a key that did not exist yet is routine, while a value being
// respelled under the user is the one thing here that alters bytes they wrote.
//
// A zero MigrationResult means the file was already current.
type MigrationResult struct {
	// Documented lists the keys whose commented documentation block was
	// appended, in template order.
	Documented []string
	// Renamed lists the keys whose value spelling was brought up to date, in
	// legacyValues order.
	Renamed []string
}

// Changed reports whether the migration touched the file at all.
func (r MigrationResult) Changed() bool {
	return len(r.Documented) > 0 || len(r.Renamed) > 0
}

// MigrateFile brings the config at path up to date in place, reporting what it
// changed. A current file is left untouched and reports a zero result and no
// error.
//
// The renames run BEFORE the appends. Order matters in one direction only: a
// respelled line still mentions its key, so the append pass correctly leaves it
// alone; running the appends first would be equivalent here but would start
// depending on the fresh template never carrying a legacy spelling, which is a
// weaker thing to rely on than "the file is fixed before it is read again".
//
// The write is the same atomic temp+rename writeAtomic gives every other
// publisher of this file, so a crash mid-migration leaves the original intact
// rather than a half-written config.
//
// Callers treat a returned error as advisory. Migration updates a file that
// already works — every spelling it rewrites is one the loader still accepts —
// so refusing to start over it would trade a cosmetic shortfall for a total one.
func MigrateFile(path string) (MigrationResult, error) {
	var res MigrationResult

	raw, err := os.ReadFile(path)
	if err != nil {
		return res, fmt.Errorf("read config for migration: %w", err)
	}

	migrated, renamed := RenameLegacyValues(raw)
	migrated, added := AppendMissingSections(migrated)
	if len(renamed) == 0 && len(added) == 0 {
		return res, nil
	}

	// Prove the edit changed no SETTING before it reaches the disk. That claim
	// is the entire safety argument for doing this automatically at startup, so
	// it is verified rather than asserted — the same discipline verifyRewrite
	// applies to the secret.
	//
	// Whole-struct comparison rather than a field list, for the reason
	// withoutSecret gives: a field added later is covered without anyone
	// remembering to extend this. foldLegacyValues is what makes a rename fit
	// inside a whole-struct equality instead of punching a hole in it — it
	// forgives exactly the respellings legacyValues authorises and nothing else,
	// so a surgery that hit the wrong line still fails here.
	before, err := parseStrict(raw)
	if err != nil {
		// The file did not parse BEFORE the migration either, so this is not
		// something the migration broke. Leave it alone and let Load produce
		// the real diagnostic, which points at the offending line.
		return res, fmt.Errorf("config does not parse; not migrating: %w", err)
	}
	after, err := parseStrict(migrated)
	if err != nil {
		return res, fmt.Errorf("migrated config would not parse; left unchanged: %w", err)
	}
	if !reflect.DeepEqual(foldLegacyValues(before), foldLegacyValues(after)) {
		return res, errors.New("migration would have changed a setting, not just its spelling; left unchanged")
	}

	if err := writeAtomic(path, migrated); err != nil {
		return res, fmt.Errorf("write migrated config: %w", err)
	}
	res.Documented, res.Renamed = added, renamed
	return res, nil
}
