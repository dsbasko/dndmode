//go:build darwin

package focus_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/dsbasko/dndmode/internal/macos/focus"
)

// TestCheckShortcuts_BothPresent_Nil — validation map ID 5-02-03.
// When both `dndmode-on` and `dndmode-off` are present in the user's
// Shortcuts library, CheckShortcuts returns nil regardless of other
// shortcuts coexisting in the same list.
func TestCheckShortcuts_BothPresent_Nil(t *testing.T) {
	td := newTestDeps(t)
	td.mockRunner.EXPECT().List(gomock.Any()).Return(
		[]string{"dndmode-on", "Other Shortcut", "dndmode-off"}, nil,
	)

	err := focus.CheckShortcuts(context.Background(), td.mockRunner)
	if err != nil {
		t.Fatalf("CheckShortcuts returned %v; want nil", err)
	}
}

// TestCheckShortcuts_OneMissing_WrapsErrShortcutsMissing —
// validation map ID 5-02-02. Two sub-cases cover the symmetry of
// dndmode-on missing vs dndmode-off missing.
func TestCheckShortcuts_OneMissing_WrapsErrShortcutsMissing(t *testing.T) {
	cases := []struct {
		name        string
		listReturns []string
		wantInMsg   string
		notInMsg    string
	}{
		{
			name:        "dndmode-off missing",
			listReturns: []string{"dndmode-on"},
			wantInMsg:   "dndmode-off",
			notInMsg:    "dndmode-on",
		},
		{
			name:        "dndmode-on missing",
			listReturns: []string{"dndmode-off", "Other Shortcut"},
			wantInMsg:   "dndmode-on",
			notInMsg:    "dndmode-off",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDeps(t)
			td.mockRunner.EXPECT().List(gomock.Any()).Return(tt.listReturns, nil)

			err := focus.CheckShortcuts(context.Background(), td.mockRunner)
			if err == nil {
				t.Fatal("CheckShortcuts returned nil; want ErrShortcutsMissing-wrapped error")
			}
			if !errors.Is(err, focus.ErrShortcutsMissing) {
				t.Errorf("errors.Is(err, ErrShortcutsMissing) = false; want true (err=%v)", err)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("err message %q does not mention missing shortcut %q",
					err.Error(), tt.wantInMsg)
			}
			if strings.Contains(err.Error(), tt.notInMsg) {
				t.Errorf("err message %q mentions present shortcut %q",
					err.Error(), tt.notInMsg)
			}
		})
	}
}

// TestCheckShortcuts_BothMissing_WrapsErrShortcutsMissing —
// validation map ID 5-02-01. When neither shortcut is present, the
// error names both in sorted (alphabetical) order: `dndmode-off`
// lexically precedes `dndmode-on` because `f` < `n`.
func TestCheckShortcuts_BothMissing_WrapsErrShortcutsMissing(t *testing.T) {
	td := newTestDeps(t)
	td.mockRunner.EXPECT().List(gomock.Any()).Return(
		[]string{"Other Shortcut"}, nil,
	)

	err := focus.CheckShortcuts(context.Background(), td.mockRunner)
	if err == nil {
		t.Fatal("CheckShortcuts returned nil; want ErrShortcutsMissing-wrapped error")
	}
	if !errors.Is(err, focus.ErrShortcutsMissing) {
		t.Errorf("errors.Is(err, ErrShortcutsMissing) = false; want true (err=%v)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "dndmode-on") {
		t.Errorf("err message %q does not mention dndmode-on", msg)
	}
	if !strings.Contains(msg, "dndmode-off") {
		t.Errorf("err message %q does not mention dndmode-off", msg)
	}
	// Sort assertion: dndmode-off comes before dndmode-on lexically.
	offIdx := strings.Index(msg, "dndmode-off")
	onIdx := strings.Index(msg, "dndmode-on")
	if offIdx == -1 || onIdx == -1 || offIdx > onIdx {
		t.Errorf("expected dndmode-off to appear before dndmode-on (sorted); msg=%q", msg)
	}
}

// TestCheckShortcuts_ListError_PropagatesWrapped — runner.List failure
// is wrapped as `list shortcuts: %w` and is NOT classified as
// ErrShortcutsMissing (main.go maps it to exitPlatformErr, not exit 6).
func TestCheckShortcuts_ListError_PropagatesWrapped(t *testing.T) {
	td := newTestDeps(t)
	wantErr := errors.New("exec failed")
	td.mockRunner.EXPECT().List(gomock.Any()).Return(nil, wantErr)

	err := focus.CheckShortcuts(context.Background(), td.mockRunner)
	if err == nil {
		t.Fatal("CheckShortcuts returned nil; want list-error wrap")
	}
	if errors.Is(err, focus.ErrShortcutsMissing) {
		t.Errorf("list error misclassified as ErrShortcutsMissing: %v", err)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err does not wrap underlying exec error: %v", err)
	}
	if !strings.Contains(err.Error(), "list shortcuts:") {
		t.Errorf("err message %q does not prefix with 'list shortcuts:'", err.Error())
	}
}

// TestCheckShortcuts_WhitespaceFiltered_Nil — the design notes:
// whitespace-only lines (trailing newline from final `\n`, accidental
// blank lines) must NOT produce false missing-detection.
func TestCheckShortcuts_WhitespaceFiltered_Nil(t *testing.T) {
	td := newTestDeps(t)
	td.mockRunner.EXPECT().List(gomock.Any()).Return(
		[]string{"", "  ", "dndmode-on", "\t", "dndmode-off", ""}, nil,
	)

	err := focus.CheckShortcuts(context.Background(), td.mockRunner)
	if err != nil {
		t.Fatalf("CheckShortcuts returned %v; want nil (whitespace should be filtered)", err)
	}
}

// TestCheckShortcuts_ExtraShortcuts_Nil — presence of unrelated user
// shortcuts is harmless. The check is "both required names present",
// not "exactly these two names exist".
func TestCheckShortcuts_ExtraShortcuts_Nil(t *testing.T) {
	td := newTestDeps(t)
	td.mockRunner.EXPECT().List(gomock.Any()).Return(
		[]string{"dndmode-on", "dndmode-off", "Random1", "Random2", "Random3"}, nil,
	)

	err := focus.CheckShortcuts(context.Background(), td.mockRunner)
	if err != nil {
		t.Fatalf("CheckShortcuts returned %v; want nil (extra shortcuts harmless)", err)
	}
}
