//go:build darwin

package config

import (
	"strings"
	"testing"
)

// TestMigratableKeys_AllHaveTemplateSections is the pin that keeps the two
// halves of migration in step. AppendMissingSections silently skips a key with
// no matching template block — the alternative would be writing a truncated
// file into someone's config — so without this test a renamed heading would
// turn migration into a no-op for that key and nothing would say so.
func TestMigratableKeys_AllHaveTemplateSections(t *testing.T) {
	sections := templateSections()
	for _, key := range migratableKeys {
		block, ok := sections[key]
		if !ok {
			t.Errorf("migratableKeys lists %q but the template has no %q%s section heading",
				key, sectionMarker, key)
			continue
		}
		if !strings.Contains(block, key+":") {
			t.Errorf("template section for %q does not contain a %q line; "+
				"appending it would document a key the user cannot then find", key, key+":")
		}
	}
}

// TestTemplateSections_CarryNoFormatVerb guards the one way an appended block
// could corrupt a user's file: defaultConfigTemplate is fed through Sprintf,
// and a section lifted from the UNRENDERED text would paste a literal %s (or,
// worse, a %!s(MISSING)) into config.yml.
func TestTemplateSections_CarryNoFormatVerb(t *testing.T) {
	for key, block := range templateSections() {
		if strings.Contains(block, "%s") || strings.Contains(block, "%!") {
			t.Errorf("template section %q carries an unrendered format verb:\n%s", key, block)
		}
	}
}

func TestMentionsKey(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		key  string
		want bool
	}{
		{"live setting", "mute: false\n", "mute", true},
		{"commented template line", "# mute: true\n", "mute", true},
		{"commented, no space", "#mute: true\n", "mute", true},
		{"commented, extra spaces", "#   mute: true\n", "mute", true},
		{"indented", "  mute: false\n", "mute", true},
		{"absent", "unlock_code: a b c d\n", "mute", false},
		// The template is mostly prose, so a key named mid-sentence must not
		// count — otherwise the very documentation that mentions a key would
		// suppress that key's own section.
		{"named in prose", "# Ignored entirely when mute is off.\n", "mute", false},
		// Anchored at the start of the line: a longer key that merely ends
		// with a shorter one is a different key.
		{"different key with shared suffix", "terminal_language: go\n", "language", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mentionsKey([]byte(tt.raw), tt.key); got != tt.want {
				t.Errorf("mentionsKey(%q, %q) = %v, want %v", tt.raw, tt.key, got, tt.want)
			}
		})
	}
}

// TestFreshTemplate_NeedsNoMigration closes the loop: a config written by
// writeDefault today must already mention every migratable key, or a brand-new
// install would be "migrated" on its second run.
func TestFreshTemplate_NeedsNoMigration(t *testing.T) {
	if missing := MissingSections([]byte(renderedTemplate())); len(missing) != 0 {
		t.Errorf("a freshly rendered config reports missing sections: %v", missing)
	}
}
