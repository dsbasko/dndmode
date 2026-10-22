//go:build darwin || linux

package config

import (
	"strings"
	"testing"
)

// TestTemplate_DocumentsEveryStyle: the overlay_style block of a fresh config
// describes every value ValidateOverlayStyle accepts, so a new style is not
// only reachable from --help.
func TestTemplate_DocumentsEveryStyle(t *testing.T) {
	block, ok := templateSections()["overlay_style"]
	if !ok {
		t.Fatal("template has no overlay_style section")
	}
	for _, style := range []string{
		OverlayStyleBlack, OverlayStyleMatrix, OverlayStyleTerminal, OverlayStyleDVD,
		OverlayStyleGold, OverlayStyleBamboo, OverlayStyleDino, OverlayStyleGlass, OverlayStyleNone,
	} {
		if ValidateOverlayStyle(style) != nil {
			t.Errorf("%q is listed here but not valid", style)
		}
		if !strings.Contains(block, "#   "+style+" ") {
			t.Errorf("overlay_style section does not describe %q", style)
		}
	}
}
