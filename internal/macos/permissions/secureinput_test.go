//go:build darwin

// secureinput_test.go holds the compile-time signature assertion for
// IsSecureEventInputActive. There is no pure-Go logic to unit-test here —
// the function is a 1-line cgo wrapper around Carbon's
// IsSecureEventInputEnabled. Substantive testing happens in
// permissions_smoketest_test.go via TestSmoke_SecureEventInput_IsActive_NonPanic.
package permissions_test

import (
	"testing"

	"github.com/dsbasko/dndmode/internal/macos/permissions"
)

// TestIsSecureEventInputActive_Signature is a compile-time pin on the
// public signature `func() bool`. If permissions.IsSecureEventInputActive
// is ever changed to take an argument or return a different type, this
// file fails to compile — surfacing the API break at the source, not at
// the call site in cmd/dndmode/main.go.
func TestIsSecureEventInputActive_Signature(t *testing.T) {
	t.Helper()
	var _ func() bool = permissions.IsSecureEventInputActive
}
