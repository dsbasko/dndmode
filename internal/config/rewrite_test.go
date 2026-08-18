//go:build darwin

package config_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config"
	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// wantComment is the exact comment block RewriteSecretAsHash emits above the
// pair. It is spelled out here rather than exported from the package on
// purpose: its FIRST line doubles as the idempotency marker that stops a
// second `--set-password` run from stacking another copy of the comment above
// the keys. Editing that text has to fail a test rather than silently strand
// every config an earlier version wrote — those files keep the old marker, and
// the next run would no longer recognise it.
var wantComment = []string{
	"# --- unlock_salt / unlock_hash (written by `dndmode --set-password`) ---------",
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

// wantBlock renders the lines RewriteSecretAsHash is expected to insert,
// terminated with nl. withComment is false only for a file that already
// carries the marker.
func wantBlock(nl string, withComment bool, saltB64, hashB64 string) string {
	var b strings.Builder
	if withComment {
		for _, l := range wantComment {
			b.WriteString(l + nl)
		}
	}
	b.WriteString("unlock_salt: " + saltB64 + nl)
	b.WriteString("unlock_hash: " + hashB64 + nl)
	return b.String()
}

// dropFinalNewline expresses the "the file did not end in a newline and must
// not start" expectation without repeating the terminator in every want value.
func dropFinalNewline(s, nl string) string { return strings.TrimSuffix(s, nl) }

// The surgery is specified by its output bytes, not by properties of them: a
// line-based edit that gets the POSITION or the LINE ENDINGS subtly wrong still
// satisfies every "contains unlock_hash" style assertion, and those are exactly
// the failures verification later has to catch. Comparing whole files keeps the
// table honest about what the user's config actually looks like afterwards.
func TestRewriteSecretAsHash(t *testing.T) {
	salt, hash := digestPair(t, mustParse(t, "s w o r d f"))
	lf := wantBlock("\n", true, salt, hash)
	crlf := wantBlock("\r\n", true, salt, hash)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// The ordinary case: the pair lands where unlock_code was, so the
			// comments the generated config keeps ABOVE the secret still
			// introduce a secret rather than a stranger.
			name: "plain unlock_code between other lines",
			in:   "# top\nunlock_code: s w o r d f\ndebug: true\n",
			want: "# top\n" + lf + "debug: true\n",
		},
		{
			name: "quoted value",
			in:   "unlock_code: \"s w o r d f\"\n",
			want: lf,
		},
		{
			// The trailing comment goes with the line. Keeping it would leave a
			// dangling '# my code' above a hash it no longer describes.
			name: "trailing comment on the secret line",
			in:   "unlock_code: s w o r d f # my code\n",
			want: lf,
		},
		{
			// A value opening with a YAML indicator has to be quoted in the
			// file; the surgery is indifferent to the value's shape, which is
			// the point of asserting it.
			name: "leading dash in the value",
			in:   "unlock_code: \"- a b c d\"\n",
			want: lf,
		},
		{
			name: "space before the colon",
			in:   "unlock_code : s w o r d f\n",
			want: lf,
		},
		{
			name: "CRLF file keeps CRLF",
			in:   "debug: true\r\nunlock_code: s w o r d f\r\n",
			want: "debug: true\r\n" + crlf,
		},
		{
			name: "no final newline stays without one",
			in:   "debug: true\nunlock_code: s w o r d f",
			want: dropFinalNewline("debug: true\n"+lf, "\n"),
		},
		{
			name: "legacy hotkey key is a secret too",
			in:   "hotkey: Ctrl+Option+Cmd+X\nfocus: true\n",
			want: lf + "focus: true\n",
		},
		{
			// Both plaintext spellings present: every one is removed, and the
			// pair is inserted ONCE, at the first of them.
			name: "unlock_code and hotkey at once",
			in:   "unlock_code: s w o r d f\ndebug: true\nhotkey: ctrl+x\n",
			want: lf + "debug: true\n",
		},
		{
			// Re-running with the SAME pair is a fixed point: the marker is
			// already there, so no second comment block is emitted.
			name: "already hashed with the same pair",
			in:   "# top\n" + lf + "debug: true\n",
			want: "# top\n" + lf + "debug: true\n",
		},
		{
			// The block's own documentation mentions unlock_code. If a '#'
			// line could match, the second run would eat its own comment.
			name: "unlock_code inside a comment is not a secret",
			in:   "# unlock_code: s w o r d f\ndebug: true\n",
			want: "# unlock_code: s w o r d f\ndebug: true\n\n" + lf,
		},
		{
			// Column 0 is the whole recognition rule. An indented key belongs
			// to a document shape this edit does not understand, so it is left
			// alone and VerifyStructure refuses the result.
			name: "indented key is not top level",
			in:   "  unlock_code: s w o r d f\n  debug: true\n",
			want: "  unlock_code: s w o r d f\n  debug: true\n\n" + lf,
		},
		{
			name: "no secret at all is appended after a blank line",
			in:   "debug: true\n",
			want: "debug: true\n\n" + lf,
		},
		{
			// Appending has to terminate the previous line or the block would
			// be glued onto it — while the file still must not end in a newline.
			name: "no secret and no final newline",
			in:   "debug: true",
			want: dropFinalNewline("debug: true\n\n"+lf, "\n"),
		},
		{
			name: "empty file gets the block with no leading blank line",
			in:   "",
			want: lf,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.RewriteSecretAsHash([]byte(tt.in), salt, hash)
			if err != nil {
				t.Fatalf("RewriteSecretAsHash: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("output mismatch\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// A second --set-password over an already-hashed file must converge: the
// result differs from the previous one in the two VALUES and nothing else. A
// rewrite that re-emitted the comment would grow the file by ten lines on
// every run, and one that appended instead of replacing would leave two pairs
// behind — an ambiguous secret ResolveUnlockCode rejects at the next startup.
func TestRewriteSecretAsHash_RepeatRunReplacesInPlace(t *testing.T) {
	salt1, hash1 := digestPair(t, mustParse(t, "s w o r d f"))
	salt2, hash2 := digestPair(t, mustParse(t, "a b c d e f"))

	const src = "# top\nunlock_code: s w o r d f\ndebug: true\n"
	once, err := config.RewriteSecretAsHash([]byte(src), salt1, hash1)
	if err != nil {
		t.Fatalf("first rewrite: %v", err)
	}
	twice, err := config.RewriteSecretAsHash(once, salt2, hash2)
	if err != nil {
		t.Fatalf("second rewrite: %v", err)
	}

	want := "# top\n" + wantBlock("\n", true, salt2, hash2) + "debug: true\n"
	if string(twice) != want {
		t.Errorf("second run did not replace in place\n got %q\nwant %q", twice, want)
	}
	if n := strings.Count(string(twice), wantComment[0]); n != 1 {
		t.Errorf("comment block appears %d times, want exactly 1", n)
	}
}

// Writing a pair that cannot be read back is the worst outcome available to
// this package — the plaintext is gone and the machine can no longer be
// unlocked — so the check runs before a byte is composed, through the same
// decoder ResolveUnlockCode uses at startup.
func TestRewriteSecretAsHash_RejectsUnusableSecret(t *testing.T) {
	salt, hash := digestPair(t, mustParse(t, "s w o r d f"))
	shortSalt := base64.StdEncoding.EncodeToString([]byte("quokka"))
	shortHash := base64.StdEncoding.EncodeToString([]byte("narwhal"))

	cases := []struct {
		name       string
		salt, hash string
	}{
		{"salt is not base64", "zebraquokkanarwhal!!", hash},
		{"hash is not base64", salt, "axolotlpangolinseahorse!!"},
		{"salt is the wrong width", shortSalt, hash},
		{"hash is the wrong width", salt, shortHash},
		{"both empty", "", ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.RewriteSecretAsHash([]byte("unlock_code: s w o r d f\n"), tt.salt, tt.hash)
			if err == nil {
				t.Fatalf("expected an error, got output %q", got)
			}
			if got != nil {
				t.Errorf("expected no output alongside the error, got %q", got)
			}
		})
	}
}

// VerifyStructure is what turns a fragile line edit into a safe one, so it has
// to ACCEPT every shape the surgery genuinely handles. A verifier that
// rejected valid rewrites would make --set-password fail for everyone whose
// config is not the generated template.
func TestVerifyStructure_AcceptsOwnRewrite(t *testing.T) {
	salt, hash := digestPair(t, mustParse(t, "s w o r d f"))

	cases := []struct {
		name string
		in   string
	}{
		{"generated-shape config", "# top\nunlock_code: s w o r d f\ndebug: true\n"},
		{"quoted value", "unlock_code: \"s w o r d f\"\n"},
		{"trailing comment", "unlock_code: s w o r d f # my code\noverlay_style: matrix\n"},
		{"CRLF", "debug: true\r\nunlock_code: s w o r d f\r\n"},
		{"no final newline", "debug: true\nunlock_code: s w o r d f"},
		{"legacy hotkey", "hotkey: Ctrl+Option+Cmd+X\nfocus: true\n"},
		{"space before the colon", "unlock_code : s w o r d f\n"},
		{"every other key set", "unlock_code: s w o r d f\noverlay_style: glass\nglass_blur: 24\n" +
			"terminal_language: rust\nallow_display_sleep: true\nmute: false\nfocus: true\ndebug: true\n"},
		{"already hashed", "# top\n" + wantBlock("\n", true, salt, hash) + "debug: true\n"},
		{"secret only in a comment", "# unlock_code: s w o r d f\ndebug: true\n"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out, err := config.RewriteSecretAsHash([]byte(tt.in), salt, hash)
			if err != nil {
				t.Fatalf("RewriteSecretAsHash: %v", err)
			}
			if verr := config.VerifyStructure([]byte(tt.in), out, salt, hash); verr != nil {
				t.Fatalf("VerifyStructure rejected a valid rewrite: %v", verr)
			}
		})
	}
}

// The other half of the contract: shapes the line surgery cannot handle must
// be REFUSED, not half-edited. Each of these produces bytes that look
// plausible and would leave the plaintext secret in the file (or destroy an
// unrelated key), which is precisely why the check reads the result instead of
// trusting the edit.
func TestVerifyStructure_RejectsBadRewrite(t *testing.T) {
	salt, hash := digestPair(t, mustParse(t, "s w o r d f"))
	_, otherHash := digestPair(t, mustParse(t, "a b c d e f"))
	// digestPair pins its salt, so a DIFFERENT salt has to be built by hand.
	otherSalt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5A}, matcher.SaltLen))

	cases := []struct {
		name         string
		old, new     string
		salt, hash   string
		wantContains string
	}{
		{
			// A quoted key is the same key to YAML but not to a '^unlock_code'
			// anchor, so the plaintext survives the edit.
			name: "quoted key leaves the plaintext behind",
			old:  "\"unlock_code\": s w o r d f\n",
			new:  "\"unlock_code\": s w o r d f\n\n" + wantBlock("\n", true, salt, hash),
			salt: salt, hash: hash,
			wantContains: "plaintext unlock_code",
		},
		{
			name: "quoted legacy key leaves the plaintext behind",
			old:  "\"hotkey\": Ctrl+Option+Cmd+X\n",
			new:  "\"hotkey\": Ctrl+Option+Cmd+X\n\n" + wantBlock("\n", true, salt, hash),
			salt: salt, hash: hash,
			wantContains: "plaintext hotkey",
		},
		{
			name: "an unrelated key changed",
			old:  "unlock_code: s w o r d f\ndebug: true\n",
			new:  wantBlock("\n", true, salt, hash) + "debug: false\n",
			salt: salt, hash: hash,
			wantContains: "unrelated config key",
		},
		{
			name: "an unrelated key disappeared",
			old:  "unlock_code: s w o r d f\noverlay_style: matrix\n",
			new:  wantBlock("\n", true, salt, hash),
			salt: salt, hash: hash,
			wantContains: "unrelated config key",
		},
		{
			name: "the written salt is not the one that was asked for",
			old:  "unlock_code: s w o r d f\n",
			new:  wantBlock("\n", true, otherSalt, hash),
			salt: salt, hash: hash,
			wantContains: "unlock_salt",
		},
		{
			name: "the written hash is not the one that was asked for",
			old:  "unlock_code: s w o r d f\n",
			new:  wantBlock("\n", true, salt, otherHash),
			salt: salt, hash: hash,
			wantContains: "unlock_hash",
		},
		{
			name: "the result does not parse",
			old:  "unlock_code: s w o r d f\n",
			new:  "unlock_salt: [\n",
			salt: salt, hash: hash,
			wantContains: "does not parse",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := config.VerifyStructure([]byte(tt.old), []byte(tt.new), tt.salt, tt.hash)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("error %q does not mention %q", err, tt.wantContains)
			}
		})
	}
}

// An entirely indented root mapping is legal YAML that Load() accepts, and it
// is the shape the column-0 anchor deliberately does not touch. Going through
// the real rewrite (rather than hand-built bytes) pins that the pair of them
// composes into a refusal instead of a half-edit.
func TestVerifyStructure_RejectsIndentedRootMapping(t *testing.T) {
	salt, hash := digestPair(t, mustParse(t, "s w o r d f"))
	const in = "  unlock_code: s w o r d f\n  debug: true\n"

	out, err := config.RewriteSecretAsHash([]byte(in), salt, hash)
	if err != nil {
		t.Fatalf("RewriteSecretAsHash: %v", err)
	}
	if verr := config.VerifyStructure([]byte(in), out, salt, hash); verr == nil {
		t.Fatal("expected an indented root mapping to be refused")
	}
}

// writeConfigAt drops a config file with the standard 0600 mode and returns
// its path.
func writeConfigAt(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// tmpLeftovers returns the tmp files SaveUnlockHash may have failed to clean
// up in dir. A stray tmp file is not cosmetic here: os.CreateTemp gives it
// 0600, but it would hold the REWRITTEN config next to the original, and a
// half-published secret is exactly what the atomic rename exists to prevent.
func tmpLeftovers(t *testing.T, dir, base string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, base+".tmp.*"))
	if err != nil {
		t.Fatalf("glob tmp files: %v", err)
	}
	return matches
}

// The whole point of the feature, end to end: a config carrying a plaintext
// secret plus a full set of unrelated keys comes back with a digest, every
// other key untouched, 0600 preserved, and a Verifier that recognises the
// sequence that was hashed.
func TestLoader_SaveUnlockHash_Success(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dndmode")
	const body = "# my config\n" +
		"unlock_code: ctrl+s option+w cmd+o shift+r\n" +
		"overlay_style: glass\n" +
		"glass_blur: 24\n" +
		"terminal_language: rust\n" +
		"allow_display_sleep: true\n" +
		"mute: false\n" +
		"focus: true\n" +
		"debug: true\n"
	path := writeConfigAt(t, dir, "config.yml", body)

	steps := mustParse(t, "ctrl+s option+w cmd+o shift+r")
	loader := config.NewLoader(path)
	if err := loader.SaveUnlockHash(steps); err != nil {
		t.Fatalf("SaveUnlockHash: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %04o, want 0600", perm)
	}
	if left := tmpLeftovers(t, dir, "config.yml"); len(left) != 0 {
		t.Errorf("tmp files left behind: %v", left)
	}

	cfg, created, err := loader.Load()
	if err != nil {
		t.Fatalf("Load after SaveUnlockHash: %v", err)
	}
	if created {
		t.Fatal("Load reported the file as created; the rewrite must not have removed it")
	}
	if cfg.UnlockCode != "" || cfg.Hotkey != "" {
		t.Errorf("plaintext secret survived: unlock_code=%q hotkey=%q", cfg.UnlockCode, cfg.Hotkey)
	}
	// Every unrelated key has to come back exactly as written — the rewrite
	// is allowed to touch the secret and nothing else.
	if cfg.OverlayStyle != config.OverlayStyleGlass {
		t.Errorf("overlay_style = %q, want glass", cfg.OverlayStyle)
	}
	if got := config.NormalizeGlassBlur(cfg.GlassBlur); got != 24 {
		t.Errorf("glass_blur = %v, want 24", got)
	}
	if cfg.TerminalLanguage != config.TerminalLangRust {
		t.Errorf("terminal_language = %q, want rust", cfg.TerminalLanguage)
	}
	if !cfg.AllowDisplaySleep || !cfg.Focus || !cfg.Debug {
		t.Errorf("toggles lost: allow_display_sleep=%v focus=%v debug=%v",
			cfg.AllowDisplaySleep, cfg.Focus, cfg.Debug)
	}
	if config.NormalizeMute(cfg.Mute) {
		t.Error("mute: false was lost")
	}

	v, src, weak, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		t.Fatalf("ResolveUnlockCode after SaveUnlockHash: %v", err)
	}
	if src != config.UnlockSourceHash {
		t.Errorf("source = %q, want %q", src, config.UnlockSourceHash)
	}
	if weak {
		t.Error("a digest source can never report weak: it stores no length")
	}
	if _, ok := v.(*matcher.Digest); !ok {
		t.Fatalf("verifier is %T, want *matcher.Digest", v)
	}
	if !v.Match(keyEvents(steps)) {
		t.Fatal("the saved digest does not recognise the sequence it was built from")
	}
}

// The generated default config is the file almost every user actually has, so
// the upgrade path over it — comments and all — is pinned separately from the
// hand-written fixture above.
func TestLoader_SaveUnlockHash_OverGeneratedDefault(t *testing.T) {
	d := newTestDeps(t)
	if _, created, err := d.loader.Load(); err != nil || !created {
		t.Fatalf("Load: created=%v err=%v", created, err)
	}

	steps := mustParse(t, "ctrl+s option+w cmd+o shift+r")
	if err := d.loader.SaveUnlockHash(steps); err != nil {
		t.Fatalf("SaveUnlockHash: %v", err)
	}

	cfg, _, err := d.loader.Load()
	if err != nil {
		t.Fatalf("Load after SaveUnlockHash: %v", err)
	}
	v, src, _, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		t.Fatalf("ResolveUnlockCode: %v", err)
	}
	if src != config.UnlockSourceHash || !v.Match(keyEvents(steps)) {
		t.Fatalf("default config did not upgrade to a working digest (source=%q)", src)
	}

	// The template's documentation must survive: it is the only manual most
	// users read, and an edit that ate it would be silent.
	raw, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"# dndmode configuration", "# --- overlay_style", "# --- debug"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the rewrite lost the %q section of the generated config", want)
		}
	}
}

// The threat model in one assertion: after --set-password, no fragment of the
// sequence the user typed is readable in the file. Checked against the
// RESOLVED target rather than the config path, because a symlinked config
// would otherwise let the plaintext survive one indirection away.
func TestLoader_SaveUnlockHash_NoPlaintextSurvives(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dndmode")
	const code = "ctrl+s option+w cmd+o shift+r"
	path := writeConfigAt(t, dir, "config.yml", "unlock_code: "+code+"\ndebug: true\n")

	if err := config.NewLoader(path).SaveUnlockHash(mustParse(t, code)); err != nil {
		t.Fatalf("SaveUnlockHash: %v", err)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(raw)

	if strings.Contains(got, code) {
		t.Error("the whole unlock code is still in the file")
	}
	// Every step, not a sample: a rewrite that truncated the value instead of
	// deleting the line would pass a whole-string check.
	for _, step := range strings.Fields(code) {
		if strings.Contains(got, step) {
			t.Errorf("step %q survived in the rewritten config", step)
		}
	}
}

// A config.yml symlinked into a dotfiles repository is the exact scenario the
// feature exists for, and the one a naive os.Rename gets backwards: it would
// replace the LINK with a regular file and leave the plaintext sitting in the
// repo, reporting success. The link must survive as a link, and the TARGET
// must be the thing that was rewritten.
func TestLoader_SaveUnlockHash_RewritesThroughSymlink(t *testing.T) {
	root := t.TempDir()
	const code = "ctrl+s option+w cmd+o shift+r"
	target := writeConfigAt(t, filepath.Join(root, "dotfiles"), "dndmode.yml",
		"unlock_code: "+code+"\ndebug: true\n")

	linkDir := filepath.Join(root, "config", "dndmode")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(linkDir, "config.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := config.NewLoader(link).SaveUnlockHash(mustParse(t, code)); err != nil {
		t.Fatalf("SaveUnlockHash: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file; the target still holds the plaintext")
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if strings.Contains(string(raw), code) {
		t.Error("the plaintext is still in the symlink target")
	}
	if !strings.Contains(string(raw), "unlock_hash: ") {
		t.Error("the target did not receive the digest")
	}
	// The tmp file has to be created next to the TARGET, or the rename would
	// cross a filesystem boundary and stop being atomic.
	if left := tmpLeftovers(t, filepath.Dir(target), "dndmode.yml"); len(left) != 0 {
		t.Errorf("tmp files left next to the target: %v", left)
	}
	if left := tmpLeftovers(t, linkDir, "config.yml"); len(left) != 0 {
		t.Errorf("tmp files left next to the link: %v", left)
	}
}

// A dangling symlink is unreachable through the command flow (Load() runs
// first and replaces it), so this guard exists for a direct caller. It must
// fail loudly and create nothing: writing through it would put the config
// wherever the broken link happens to point.
func TestLoader_SaveUnlockHash_BrokenSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "config.yml")
	missing := filepath.Join(root, "gone.yml")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := config.NewLoader(link).SaveUnlockHash(mustParse(t, "s w o r d f"))
	if err == nil {
		t.Fatal("expected a dangling symlink to be refused")
	}
	if _, serr := os.Lstat(missing); serr == nil {
		t.Error("the missing target was created")
	}
	entries, rerr := os.ReadDir(root)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	if len(entries) != 1 {
		t.Errorf("directory gained entries: %d, want just the symlink", len(entries))
	}
}

// Verification runs BEFORE the tmp file is created, so a refusal leaves the
// original byte-for-byte intact and no debris beside it. This is what makes
// line surgery safe to attempt at all: the failure mode is "nothing happened",
// never "half a config".
func TestLoader_SaveUnlockHash_RefusalLeavesFileUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dndmode")
	// A quoted key parses as unlock_code but does not match the column-0
	// anchor, so the surgery would leave the plaintext in place.
	const body = "\"unlock_code\": s w o r d f\ndebug: true\n"
	path := writeConfigAt(t, dir, "config.yml", body)

	err := config.NewLoader(path).SaveUnlockHash(mustParse(t, "s w o r d f"))
	if err == nil {
		t.Fatal("expected the rewrite to be refused")
	}

	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if string(raw) != body {
		t.Errorf("the original config was modified\n got %q\nwant %q", raw, body)
	}
	if left := tmpLeftovers(t, dir, "config.yml"); len(left) != 0 {
		t.Errorf("tmp files left behind: %v", left)
	}
}

// The no-echo rule reaches the write path too. A diagnostic printed here lands
// in the same scrollback as everything else, and the value it would quote is
// the secret the command was invoked to hide.
func TestSaveUnlockHash_ErrorsNeverEchoFileContent(t *testing.T) {
	const code = "ctrl+s option+w cmd+o shift+r"
	steps := mustParse(t, code)
	salt, hash := digestPair(t, steps)

	// Bodies that exercise a different refusal each: plaintext survives the
	// edit, an indented mapping the anchor cannot see, and a document the
	// rewritten form no longer parses as.
	bodies := map[string]string{
		"quoted key":            "\"unlock_code\": " + code + "\ndebug: true\n",
		"quoted legacy key":     "\"hotkey\": ctrl+option+cmd+x\nunlock_code: " + code + "\n",
		"indented root mapping": "  unlock_code: " + code + "\n  debug: true\n",
	}

	var msgs []string
	for name, body := range bodies {
		dir := filepath.Join(t.TempDir(), name)
		path := writeConfigAt(t, dir, "config.yml", body)
		err := config.NewLoader(path).SaveUnlockHash(steps)
		if err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		msgs = append(msgs, err.Error())
	}
	if _, err := config.RewriteSecretAsHash([]byte("unlock_code: "+code+"\n"), "!!!", hash); err != nil {
		msgs = append(msgs, err.Error())
	} else {
		t.Fatal("expected invalid base64 to be refused")
	}

	// Every step is searched for, not a sample — and so are both halves of the
	// pair, which are file content even though they are not the secret itself.
	forbidden := append(strings.Fields(code), code, salt, hash)
	for _, msg := range msgs {
		for _, bad := range forbidden {
			if strings.Contains(msg, bad) {
				t.Errorf("error message leaks %q:\n%s", bad, msg)
			}
		}
	}
}

// A digest built from steps whose modifiers were NOT masked would verify
// against itself and fail against the poller. Feeding the full verification a
// sequence carrying system bits (CapsLock, NumPad, Fn, Help) pins that the
// mask is applied on the recording side too — the failure it prevents is a
// machine that can never be unlocked with the code its owner just typed.
func TestLoader_SaveUnlockHash_MasksSystemModifierBits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dndmode")
	path := writeConfigAt(t, dir, "config.yml", "unlock_code: s w o r d f\n")

	const systemBits = hotkey.ModFlag(0x10000 | 0x200000 | 0x800000 | 0x400000)
	steps := mustParse(t, "ctrl+s w o r d f")
	dirty := make([]hotkey.Spec, len(steps))
	for i, s := range steps {
		dirty[i] = hotkey.Spec{Modifiers: s.Modifiers | systemBits, KeyCode: s.KeyCode}
	}

	if err := config.NewLoader(path).SaveUnlockHash(dirty); err != nil {
		t.Fatalf("SaveUnlockHash with system bits set: %v", err)
	}

	cfg, _, err := config.NewLoader(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v, _, _, err := config.ResolveUnlockCode(&cfg)
	if err != nil {
		t.Fatalf("ResolveUnlockCode: %v", err)
	}
	// The CLEAN sequence must match: that is what the poller will deliver
	// once the tap has masked the event flags.
	if !v.Match(keyEvents(steps)) {
		t.Fatal("a code recorded with CapsLock on cannot be typed without it — silent lockout")
	}
}
