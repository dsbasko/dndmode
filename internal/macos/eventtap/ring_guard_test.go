//go:build darwin

package eventtap

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"unsafe"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// The keystroke ring is described by two constants that live on opposite
// sides of the cgo boundary: DND_RING_CAP in tap_ring.h (used to size the C
// array and its memcpy) and ringCap in poller.go (used to size the Go
// snapshot buffer and to mask ring indices). Nothing in the compiler relates
// them, so a change to one alone is a silent, memory-unsafe divergence: too
// small a Go buffer and eventtap_snapshot's memcpy writes past its end; too
// large and the tail of every snapshot is stale.
//
// The tests below are the only thing keeping those two values equal, plus
// the two structural properties the design depends on — power-of-two
// capacity and 2x headroom over the longest allowed unlock code.
//
// The C value is read by parsing tap_ring.h rather than through
// `C.DND_RING_CAP`: cgo is not supported in _test.go files at all ("use of
// cgo in test ... not supported"), so the header text is the closest the
// test can get to what the compiler sees. The header is a stable, one-line
// #define with no conditional compilation, which is what makes parsing it
// safe here.

// ringHeaderPath is the header shared by tap_darwin.m and (from the wiring
// step onwards) the cgo preamble of tap_darwin.go. Relative to the package
// directory, which is `go test`'s working directory.
const ringHeaderPath = "tap_ring.h"

var (
	ringCapDefineRe = regexp.MustCompile(`(?m)^#define\s+DND_RING_CAP\s+(\d+)\s*$`)
	keyrecStructRe  = regexp.MustCompile(`(?s)typedef struct \{(.*?)\} dnd_keyrec_t;`)
)

// readRingHeader returns the contents of tap_ring.h, failing the test if it
// is missing — a rename that leaves the constants unpinned is itself a
// regression worth failing on.
func readRingHeader(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(ringHeaderPath)
	if err != nil {
		t.Fatalf("read %s: %v (the header is the single source of truth for "+
			"the ring layout; these guard tests cannot run without it)",
			ringHeaderPath, err)
	}
	return string(b)
}

// TestRingCap_MatchesCConstant pins the Go-side ring capacity to the
// DND_RING_CAP macro in tap_ring.h.
func TestRingCap_MatchesCConstant(t *testing.T) {
	m := ringCapDefineRe.FindStringSubmatch(readRingHeader(t))
	if m == nil {
		t.Fatalf("no `#define DND_RING_CAP <n>` line found in %s", ringHeaderPath)
	}
	cCap, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("DND_RING_CAP = %q is not an integer: %v", m[1], err)
	}
	if cCap != ringCap {
		t.Fatalf("DND_RING_CAP = %d, ringCap = %d: the C ring and the Go "+
			"snapshot buffer must have identical capacity — update both "+
			"tap_ring.h and poller.go", cCap, ringCap)
	}
}

// TestRingCap_IsPowerOfTwo pins the property that makes `seq & ringMask` a
// valid index. The callback indexes the ring with a mask because it must
// stay branch- and division-free under the nosplit invariant; a capacity
// that is not a power of two would make that mask silently address the
// wrong slot rather than fail to compile.
func TestRingCap_IsPowerOfTwo(t *testing.T) {
	if ringCap <= 0 || ringCap&(ringCap-1) != 0 {
		t.Fatalf("ringCap = %d: must be a power of two — ring indexing uses "+
			"`seq & ringMask`, which is only equivalent to `seq %% ringCap` "+
			"for powers of two", ringCap)
	}
	if ringMask != ringCap-1 {
		t.Fatalf("ringMask = %d, want %d (ringCap-1)", ringMask, ringCap-1)
	}
}

// TestRingCap_HasHeadroomForMaxSteps pins the 2x margin the matcher's
// correctness argument rests on: the longest unlock code the config accepts
// (hotkey.MaxSteps) must fit in the ring twice over, so a tail window of
// MaxSteps records can never be half-overwritten by the writer while the
// poller assembles it. Raising hotkey.MaxSteps without growing the ring
// would reintroduce exactly that race.
func TestRingCap_HasHeadroomForMaxSteps(t *testing.T) {
	if hotkey.MaxSteps*2 > ringCap {
		t.Fatalf("hotkey.MaxSteps*2 = %d > ringCap = %d: the ring must hold "+
			"the longest unlock code twice over", hotkey.MaxSteps*2, ringCap)
	}
}

// TestKeyrec_FieldsMatchKeyEvent pins the C record against its Go mirror.
// eventtap_snapshot memcpys raw dnd_keyrec_t values that the Go side reads
// field-by-field into matcher.KeyEvent, so the field widths have to agree: a
// wider C keycode would feed truncated codes into the matcher and make the
// unlock code silently unmatchable.
func TestKeyrec_FieldsMatchKeyEvent(t *testing.T) {
	m := keyrecStructRe.FindStringSubmatch(readRingHeader(t))
	if m == nil {
		t.Fatalf("no `typedef struct { … } dnd_keyrec_t;` found in %s", ringHeaderPath)
	}
	body := m[1]

	var ev matcher.KeyEvent
	tests := []struct {
		field   string
		decl    string
		cBytes  uintptr
		goBytes uintptr
	}{
		{field: "flags/Modifiers", decl: "uint64_t flags;", cBytes: 8, goBytes: unsafe.Sizeof(ev.Modifiers)},
		{field: "keycode/KeyCode", decl: "uint16_t keycode;", cBytes: 2, goBytes: unsafe.Sizeof(ev.KeyCode)},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if !regexp.MustCompile(regexp.QuoteMeta(tt.decl)).MatchString(body) {
				t.Fatalf("dnd_keyrec_t does not declare %q; matcher.KeyEvent "+
					"expects a %d-byte field there", tt.decl, tt.goBytes)
			}
			if tt.cBytes != tt.goBytes {
				t.Fatalf("%s: C field is %d bytes, Go field is %d — the "+
					"snapshot conversion would truncate", tt.field, tt.cBytes, tt.goBytes)
			}
		})
	}
}
