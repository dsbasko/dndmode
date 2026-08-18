//go:build darwin

// This file holds the config-file SURGERY performed by `dndmode
// --set-password`: replacing whatever plaintext unlock secret a config
// carries with the `unlock_salt` / `unlock_hash` pair.
//
// The edit is deliberately line-based rather than an AST round-trip. Marshalling
// the parsed Config back out would drop every comment in the file — and the
// generated config.yml is roughly 90% comments, which is the only documentation
// most users ever read. It would also un-comment every zero-value key, flipping
// the absent-key defaults (mute nil => true, overlay_style "" => black). So the
// rewrite touches the lines it recognizes and copies every other byte through.
//
// Line surgery is fragile by nature, and that fragility is answered by
// verification rather than by cleverness: the rewritten bytes are re-parsed,
// diffed against the original field-by-field and resolved back into a working
// matcher.Verifier BEFORE the rename that publishes them. Any YAML shape the
// surgery mishandles is caught while the original file is still intact.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
	"github.com/goccy/go-yaml"
)

// secretKeyRe matches a line that OPENS a top-level unlock-secret key. Three
// properties of it are load-bearing:
//
//   - It is anchored at column 0 with no leading-whitespace allowance, so an
//     indented `unlock_code:` (a key of some nested mapping, or a root mapping
//     the user chose to indent) is left alone. Such a file is not one this
//     surgery understands, and the verification step rejects it rather than
//     letting a half-edit through.
//   - It requires the '#' of a comment to be absent, which comes for free from
//     the anchor: `# unlock_code: ...` starts with '#', not with the key. The
//     inserted block below deliberately mentions unlock_code inside a comment,
//     so a second run must not treat its own documentation as a secret.
//   - It allows whitespace before the colon, because `unlock_code : x` is legal
//     YAML that Load() accepts. Missing that spelling would mean deleting
//     nothing and appending a second secret — an ambiguous config.
var secretKeyRe = regexp.MustCompile(`^(?:unlock_code|hotkey|unlock_salt|unlock_hash)[ \t]*:`)

// hashBlockMarker is the first line of the generated block and doubles as its
// idempotency marker: RewriteSecretAsHash emits the explanatory comment only
// when this exact line is not already in the file. Without it, running
// `--set-password` twice would stack a second copy of the comment above the
// keys on every invocation.
const hashBlockMarker = "# --- unlock_salt / unlock_hash (written by `dndmode --set-password`) ---------"

// hashBlockComment documents the pair in the file itself, in the same voice as
// defaultConfigTemplate: the config is the manual. It explains the one thing a
// reader cannot deduce from the values — that they are two halves of one
// secret and that the plaintext is gone for good.
//
// No line may start with an unlock key name at column 0, or secretKeyRe would
// match this block's own documentation on the next run.
var hashBlockComment = []string{
	hashBlockMarker,
	"# The unlock secret as a random salt plus the SHA-256 digest of the key",
	"# sequence that was typed at `--set-password` time. The sequence itself is",
	"# NOT stored here and cannot be recovered from these two values — not even",
	"# its length.",
	"#",
	"# The pair is ONE secret: keep both keys or neither, and do not hand-edit",
	"# them. To change the code, run `dndmode --set-password` again. To go back",
	"# to a plaintext code, delete both lines and add an unlock_code line (the",
	"# grammar is documented in the generated config and in the README).",
}

// RewriteSecretAsHash returns raw with every top-level unlock-secret line
// removed and the `unlock_salt` / `unlock_hash` pair put in its place. It is a
// pure function of its inputs — no IO, no clock, no randomness — which is what
// lets the table of YAML shapes in rewrite_test.go be the real specification of
// the surgery, and what lets `--set-password` dry-run it against the loaded
// bytes BEFORE asking anyone to type a secret.
//
// The block lands at the position of the FIRST removed line so the document
// keeps its order: the secret stays where the reader (and the surrounding
// explanatory comments) expect it, rather than migrating to the bottom of the
// file on first use. A config with no secret at all — possible only through
// hand-editing, since Load() writes unlock_code into every generated file —
// gets the block appended instead.
//
// Removed lines are DELETED, never commented out: a commented-out `unlock_code`
// is the same plaintext in the same file, which would quietly cancel the entire
// point of the feature.
//
// The original newline style (LF vs CRLF) and the presence or absence of a
// final newline are both preserved, so the diff a user sees is confined to the
// secret and does not spread across every line of the file.
//
// The error path covers exactly one thing: a salt/hash pair that would not
// resolve back into a usable Verifier. Writing a config that cannot be
// unlocked is the worst failure this package can produce, so the check runs
// before a single byte is composed. YAML shapes the surgery cannot handle are
// NOT rejected here — they are caught by VerifyStructure, which reads the
// result rather than guessing at the input.
func RewriteSecretAsHash(raw []byte, saltB64, hashB64 string) ([]byte, error) {
	// decodeUnlockDigest is the same gate ResolveUnlockCode applies when
	// reading these keys back, so "would this file load?" is answered by the
	// loader's own code rather than by a second length table here. Its errors
	// name keys and widths only, never values.
	if _, err := decodeUnlockDigest(saltB64, hashB64); err != nil {
		return nil, fmt.Errorf("refusing to write an unusable unlock secret: %w", err)
	}

	lines, ends := splitLines(raw)
	nl := detectNewline(ends)
	block := hashBlock(saltB64, hashB64, hasMarker(lines))

	out := make([]string, 0, len(lines)+len(block)+1)
	inserted := false
	for i, ln := range lines {
		if secretKeyRe.MatchString(ln) {
			if !inserted {
				for _, b := range block {
					out = append(out, b+nl)
				}
				inserted = true
			}
			continue
		}
		out = append(out, ln+ends[i])
	}
	if !inserted {
		out = appendBlock(out, block, nl)
	}

	// A file that did not end in a newline must not gain one: the last line of
	// the output is either the original last line (whose own terminator was
	// empty and is preserved by the loop above) or a line this function added,
	// which is terminated unconditionally and has to be trimmed back here.
	if len(ends) > 0 && ends[len(ends)-1] == "" && len(out) > 0 {
		out[len(out)-1] = strings.TrimRight(out[len(out)-1], "\r\n")
	}
	return []byte(strings.Join(out, "")), nil
}

// appendBlock puts the hash block at the end of a file that carried no secret
// line, separated from the existing content by exactly one blank line.
//
// The terminator fix-up on the current last line is not cosmetic: if the file
// did not end in a newline, appending without one would concatenate the block's
// first line onto whatever was there and produce something that is not YAML at
// all. RewriteSecretAsHash trims the trailing newline back off the RESULT
// afterwards, so the "no final newline" property survives while the join stays
// correct.
func appendBlock(out, block []string, nl string) []string {
	if len(out) > 0 {
		last := out[len(out)-1]
		if !strings.HasSuffix(last, "\n") {
			last += nl
			out[len(out)-1] = last
		}
		if strings.TrimSpace(last) != "" {
			out = append(out, nl)
		}
	}
	for _, b := range block {
		out = append(out, b+nl)
	}
	return out
}

// hashBlock builds the lines to insert. The comment is skipped when the file
// already carries it (see hashBlockMarker) so repeated runs converge on a
// file that differs from the previous one only in the two values.
func hashBlock(saltB64, hashB64 string, skipComment bool) []string {
	var b []string
	if !skipComment {
		b = append(b, hashBlockComment...)
	}
	return append(b, "unlock_salt: "+saltB64, "unlock_hash: "+hashB64)
}

// hasMarker reports whether the generated comment block is already present.
func hasMarker(lines []string) bool {
	for _, ln := range lines {
		if ln == hashBlockMarker {
			return true
		}
	}
	return false
}

// splitLines splits raw into line CONTENTS and their individual terminators,
// so a rewrite can drop a line without disturbing the line endings of the ones
// it keeps. ends[i] is "\n", "\r\n", or "" for a final line with no terminator.
//
// Terminators are tracked per line rather than normalized to one style for the
// whole file because a config edited on two machines can legitimately carry
// both, and rewriting the unlock secret is no reason to touch every other line
// of a file the user may keep in version control.
func splitLines(raw []byte) (lines, ends []string) {
	s := string(raw)
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			ends = append(ends, "")
			break
		}
		content, end := s[:i], "\n"
		if strings.HasSuffix(content, "\r") {
			content, end = content[:len(content)-1], "\r\n"
		}
		lines = append(lines, content)
		ends = append(ends, end)
		s = s[i+1:]
	}
	return lines, ends
}

// detectNewline picks the terminator for lines this package ADDS: the first one
// the file already uses, or LF for a file with no line breaks at all. A CRLF
// config must not acquire a lone LF line in the middle of it.
func detectNewline(ends []string) string {
	for _, e := range ends {
		if e != "" {
			return e
		}
	}
	return "\n"
}

// VerifyStructure is the half of the pre-rename verification that does not need
// the captured keystrokes: it proves that newRaw parses, that it carries the
// intended digest pair and no plaintext secret, and that it agrees with oldRaw
// on every OTHER field.
//
// It is exported, and the capitalization is carrying weight rather than style.
// Its second caller is the `--set-password` command in package main, which
// dry-runs the surgery against the loaded bytes BEFORE the user types anything:
// a config whose YAML shape the line surgery cannot handle has to be rejected
// while rejecting it is still cheap, not after someone has entered a secret
// twice. Package main cannot reach an unexported helper.
//
// The split from the full verification is equally deliberate. At dry-run time
// there are no steps by construction and the salt/hash are placeholders, so the
// "does this digest match what was typed" check is unsatisfiable there; folding
// it in would make the dry run fail on every invocation and `--set-password`
// unusable for everyone.
//
// No error message contains a byte of file content. Field mismatches are
// reported as the FACT of a mismatch: naming the differing value would print
// the user's config to a terminal that, in this project's threat model, someone
// else may be reading.
func VerifyStructure(oldRaw, newRaw []byte, saltB64, hashB64 string) error {
	newCfg, err := parseStrict(newRaw)
	if err != nil {
		return fmt.Errorf("the rewritten config does not parse: %w", err)
	}
	switch {
	case newCfg.UnlockSalt != saltB64:
		return errors.New("the rewritten config does not carry the new unlock_salt")
	case newCfg.UnlockHash != hashB64:
		return errors.New("the rewritten config does not carry the new unlock_hash")
	case strings.TrimSpace(newCfg.UnlockCode) != "":
		return errors.New("the rewritten config still carries a plaintext unlock_code")
	case strings.TrimSpace(newCfg.Hotkey) != "":
		return errors.New("the rewritten config still carries a plaintext hotkey")
	}

	oldCfg, err := parseStrict(oldRaw)
	if err != nil {
		return fmt.Errorf("the original config does not parse: %w", err)
	}
	if !reflect.DeepEqual(withoutSecret(oldCfg), withoutSecret(newCfg)) {
		return errors.New(
			"rewriting the unlock secret would have changed an unrelated config key; " +
				"the file is left untouched — its top-level keys may be indented, quoted " +
				"or written in flow style, which this edit does not understand")
	}
	return nil
}

// verifyRewrite is the FULL pre-rename check: VerifyStructure plus the one
// thing only SaveUnlockHash can test — that the digest about to be written
// actually recognizes the sequence that was captured.
//
// That last check closes the loop end to end. Everything upstream of it
// (canonicalization, the modifier mask, base64, the surgery) could be
// self-consistently wrong and still produce a file that parses; only replaying
// the captured steps through the resolved Verifier proves the user will be able
// to unlock the machine. Getting it wrong is not a recoverable error: the
// plaintext is already gone from the file and the length is not stored.
func verifyRewrite(oldRaw, newRaw []byte, saltB64, hashB64 string, steps []hotkey.Spec) error {
	if err := VerifyStructure(oldRaw, newRaw, saltB64, hashB64); err != nil {
		return err
	}
	cfg, err := parseStrict(newRaw)
	if err != nil {
		return fmt.Errorf("the rewritten config does not parse: %w", err)
	}
	// Resolved through the production entry point on purpose: this asserts on
	// what startup will actually do with the file, not on what this package
	// believes it wrote.
	v, src, _, err := ResolveUnlockCode(&cfg)
	if err != nil {
		return fmt.Errorf("the rewritten config does not resolve to a usable unlock secret: %w", err)
	}
	if src != UnlockSourceHash {
		return fmt.Errorf("the rewritten config resolves its secret from %s, not %s", src, UnlockSourceHash)
	}
	d, ok := v.(*matcher.Digest)
	if !ok {
		return errors.New("the rewritten config did not resolve to a digest verifier")
	}
	if !d.Match(stepsAsEvents(steps)) {
		return errors.New("the digest written to the config does not match the unlock code that was typed")
	}
	return nil
}

// stepsAsEvents replays captured steps in the shape the poller hands a
// Verifier, so the verification exercises the real Match path rather than a
// digest-to-digest comparison that could not catch an encoding mismatch.
func stepsAsEvents(steps []hotkey.Spec) []matcher.KeyEvent {
	evs := make([]matcher.KeyEvent, len(steps))
	for i, s := range steps {
		evs[i] = matcher.KeyEvent{Modifiers: s.Modifiers, KeyCode: s.KeyCode}
	}
	return evs
}

// parseStrict parses config bytes under the same yaml.Strict() rules Load()
// uses, and formats failures with inclSource=false for the same reason: the
// source snippet a goccy error can carry would quote the lines AROUND the
// error, and in this file the line above almost anything is the secret.
func parseStrict(raw []byte) (Config, error) {
	var cfg Config
	if err := yaml.UnmarshalWithOptions(raw, &cfg, yaml.Strict()); err != nil {
		return Config{}, errors.New(yaml.FormatError(err, false /*colored*/, false /*inclSource*/))
	}
	return cfg, nil
}

// withoutSecret returns cfg with all four secret-bearing fields cleared, which
// is what makes "did the rewrite change anything else?" expressible as a single
// reflect.DeepEqual. Comparing whole structs rather than enumerating the fields
// to check is the point: a field added to Config later is compared
// automatically, where a hand-written list would silently stop covering it.
func withoutSecret(cfg Config) Config {
	cfg.Hotkey = ""
	cfg.UnlockCode = ""
	cfg.UnlockSalt = ""
	cfg.UnlockHash = ""
	return cfg
}

// SaveUnlockHash replaces the unlock secret in the config file with a freshly
// salted SHA-256 digest of steps. It is the write half of `--set-password`;
// nothing else in the codebase modifies an existing config.
//
// The sequence is: resolve symlinks, read, generate a salt, hash, rewrite the
// bytes, verify the result completely, and only then publish it with an atomic
// tmp+rename. Verification happens BEFORE the temporary file is created, so a
// rejected rewrite leaves neither a modified config nor a stray tmp file — the
// old file remains byte-for-byte intact and the user keeps a working unlock
// code.
//
// # Symlinks are resolved rather than refused
//
// Load() reads through os.ReadFile, so a config.yml that is a symlink into a
// dotfiles repository works today and users have every reason to set one up.
// os.Rename onto that path would replace the SYMLINK ENTRY with a regular file
// and never touch its target: the command would report success, the user would
// start unlocking by digest, and the plaintext secret would go on sitting in
// the dotfiles repo — which is, verbatim, the threat this feature exists to
// remove. Refusing symlinks outright would be honest but hostile, so the link
// is resolved and the target is rewritten in place. The tmp file is created in
// the TARGET's directory even when that is outside the config directory,
// because a cross-device rename would fail and cost the atomicity guarantee.
//
// # The dangling-symlink guard here is defence in depth
//
// It is not reachable through the command flow, and that is by design rather
// than by accident. On the command path Load() runs first, and os.ReadFile on a
// dangling symlink returns fs.ErrNotExist — so Load() takes its "no file" branch
// and writeDefault's rename replaces the symlink entry with a default config
// before this function is ever called. The command level closes that with an
// Lstat check ahead of Load(). The guard below only fires for a direct caller,
// and it is kept so this function is safe on its own terms: a function that
// silently did the wrong thing when called out of order would be a trap for the
// next caller.
//
// Errors never contain file content, a step, or either half of the pair.
func (l *Loader) SaveUnlockHash(steps []hotkey.Spec) error {
	target, err := filepath.EvalSymlinks(l.path)
	if err != nil {
		return fmt.Errorf(
			"resolve config path %s: %w (a broken symlink cannot be rewritten in place)",
			l.path, err)
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("read config %s: %w", target, err)
	}

	salt := make([]byte, matcher.SaltLen)
	if _, rerr := rand.Read(salt); rerr != nil {
		return fmt.Errorf("generate unlock salt: %w", rerr)
	}
	saltB64 := base64.StdEncoding.EncodeToString(salt)
	hashB64 := base64.StdEncoding.EncodeToString(matcher.HashSteps(salt, steps))

	newRaw, err := RewriteSecretAsHash(raw, saltB64, hashB64)
	if err != nil {
		return err
	}
	if verr := verifyRewrite(raw, newRaw, saltB64, hashB64, steps); verr != nil {
		return fmt.Errorf("refusing to rewrite %s: %w", target, verr)
	}
	if werr := writeAtomic(target, newRaw); werr != nil {
		return werr
	}
	return nil
}
