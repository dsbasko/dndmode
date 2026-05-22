//go:build darwin

package focus

import "errors"

// ErrShortcutsMissing is returned by CheckShortcuts when one
// or both of the required user shortcuts (`dndmode-on` / `dndmode-off`)
// is absent from the user's Shortcuts library..
//
// CheckShortcuts wraps the sentinel as
// `fmt.Errorf("%w: need %s", ErrShortcutsMissing, missingNames)` so that
// cmd/dndmode/main.go can dispatch via
// `errors.Is(err, focus.ErrShortcutsMissing)` and map to exit code 6
// (`exitFocusSetup` per the design notes), printing the missing-shortcut
// names to stderr alongside the bootstrap instruction
// ("Open the Shortcuts app and import …").
//
// The sentinel is exported here (rather than inside checker.go in plan
//) so dependent..05-06 can reference it before
// CheckShortcuts itself lands.
var ErrShortcutsMissing = errors.New("required Shortcuts not found")
