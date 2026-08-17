//go:build darwin

package matcher_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"strings"
	"testing"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// fixedSalt is a deterministic stand-in for the crypto/rand salt so the
// golden vectors below stay stable. Its value is arbitrary; what matters
// is that it is exactly matcher.SaltLen bytes.
var fixedSalt = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
}

// wordSteps is "s w o r d" as physical key positions — the passphrase the
// config template uses as its example.
var wordSteps = []hotkey.Spec{
	{Modifiers: 0, KeyCode: kvkS},
	{Modifiers: 0, KeyCode: kvkW},
	{Modifiers: 0, KeyCode: 0x1F}, // kVK_ANSI_O
	{Modifiers: 0, KeyCode: 0x0F}, // kVK_ANSI_R
	{Modifiers: 0, KeyCode: kvkD},
}

// eventsFor converts specs to the KeyEvent values the ring would hold for
// a clean keyboard — no system bits set. Tests that care about system bits
// decorate the result themselves.
func eventsFor(steps []hotkey.Spec) []matcher.KeyEvent {
	out := make([]matcher.KeyEvent, len(steps))
	for i, s := range steps {
		out[i] = matcher.KeyEvent{Modifiers: s.Modifiers, KeyCode: s.KeyCode}
	}
	return out
}

// wantPreimage rebuilds the canonical preimage independently of the
// production encoder, so the golden test below compares against an
// expectation derived from the SPEC rather than from the code it checks.
// If encodeStep's field order, endianness or width drifts, this disagrees.
func wantPreimage(t *testing.T, salt []byte, steps []hotkey.Spec) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("dndmode-unlock-v1\x00")
	b.Write(salt)
	b.WriteByte(byte(len(steps)))
	for _, s := range steps {
		var step [6]byte
		binary.BigEndian.PutUint32(step[0:4], uint32(s.Modifiers&matcher.UserIntentionalMask))
		binary.BigEndian.PutUint16(step[4:6], s.KeyCode)
		b.Write(step[:])
	}
	return b.Bytes()
}

// TestHashSteps_Golden pins the preimage layout: domain prefix, salt, the
// one-byte step count, then fixed-width big-endian steps. The step count
// inside the preimage is what makes cross-length collisions impossible
// while keeping the length off disk, so a change here is a change to the
// stored format and must be accompanied by a new hashDomain.
func TestHashSteps_Golden(t *testing.T) {
	tests := []struct {
		name  string
		steps []hotkey.Spec
	}{
		{name: "empty sequence", steps: nil},
		{name: "single bare key", steps: []hotkey.Spec{{Modifiers: 0, KeyCode: kvkS}}},
		{
			name:  "single chord",
			steps: []hotkey.Spec{{Modifiers: hotkey.ModCtrl | hotkey.ModOption | hotkey.ModCmd, KeyCode: kvkX}},
		},
		{name: "five bare keys", steps: wordSteps},
		{
			name: "mixed chords and bare keys",
			steps: []hotkey.Spec{
				{Modifiers: hotkey.ModCtrl, KeyCode: kvkS},
				{Modifiers: 0, KeyCode: kvkW},
				{Modifiers: hotkey.ModCmd | hotkey.ModShift, KeyCode: kvkZ},
				{Modifiers: 0, KeyCode: kvkA},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := sha256.Sum256(wantPreimage(t, fixedSalt, tc.steps))
			got := matcher.HashSteps(fixedSalt, tc.steps)
			if !bytes.Equal(got, want[:]) {
				t.Errorf("HashSteps digest = %s, want %s",
					hex.EncodeToString(got), hex.EncodeToString(want[:]))
			}
		})
	}
}

// TestHashSteps_LengthIsCommitted is the concrete reason the step count
// lives in the preimage: without it, appending a step whose encoding is
// all zeroes would extend the preimage without changing what it means, and
// two different-length sequences could agree. With it, every length is a
// distinct domain.
func TestHashSteps_LengthIsCommitted(t *testing.T) {
	short := []hotkey.Spec{{Modifiers: 0, KeyCode: kvkS}}
	long := []hotkey.Spec{{Modifiers: 0, KeyCode: kvkS}, {Modifiers: 0, KeyCode: 0}}

	if bytes.Equal(matcher.HashSteps(fixedSalt, short), matcher.HashSteps(fixedSalt, long)) {
		t.Error("sequences of different length produced the same digest; the step count is not in the preimage")
	}
}

// TestHashSteps_SaltSeparates: the same secret on two machines must not
// produce the same stored digest, which is the entire job of the salt.
func TestHashSteps_SaltSeparates(t *testing.T) {
	other := make([]byte, matcher.SaltLen)
	copy(other, fixedSalt)
	other[0] ^= 0xff

	if bytes.Equal(matcher.HashSteps(fixedSalt, wordSteps), matcher.HashSteps(other, wordSteps)) {
		t.Error("same steps under different salts produced the same digest")
	}
}

// TestDigest_MaskAppliedOnBothSides is the lockout guard, stated as a
// test, and it is the most important one in this file.
//
// The recording side hashes []hotkey.Spec; the matching side hashes
// []KeyEvent read out of the ring. macOS decorates the latter with bits
// nobody pressed. If those two paths disagreed about masking, a user who
// recorded their code with CapsLock on — or whose code contains an arrow
// key, which always arrives carrying SecondaryFn — would commit a digest
// that can never be matched, and the shield would go up with no way to
// lower it.
func TestDigest_MaskAppliedOnBothSides(t *testing.T) {
	// Every system bit macOS is known to set on its own.
	const (
		flagCapsLock     hotkey.ModFlag = 0x10000
		flagNumericPad   hotkey.ModFlag = 0x200000
		flagHelp         hotkey.ModFlag = 0x400000
		flagNonCoalesced hotkey.ModFlag = 0x100
	)
	systemBits := hotkey.ModFn | flagCapsLock | flagNumericPad | flagHelp | flagNonCoalesced

	steps := []hotkey.Spec{
		{Modifiers: hotkey.ModCtrl, KeyCode: kvkS},
		{Modifiers: 0, KeyCode: kvkW},
		{Modifiers: hotkey.ModCmd | hotkey.ModShift, KeyCode: kvkZ},
		{Modifiers: 0, KeyCode: kvkF1}, // the F-key case: always Fn-decorated
	}

	d, err := matcher.NewDigest(fixedSalt, matcher.HashSteps(fixedSalt, steps))
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}

	tail := eventsFor(steps)
	for i := range tail {
		tail[i].Modifiers |= systemBits
	}

	if !d.Match(tail) {
		t.Error("digest recorded from Specs did not match the same keys decorated with system bits — this is a silent lockout")
	}
}

// TestUserIntentionalMask_FitsUint32 pins the assumption encodeStep makes
// when it narrows ModFlag (uint64) to uint32. Adding a modifier bit above
// 2^32 to the mask would silently drop it from every preimage while the
// comparison path still honoured it — the two sides would stop agreeing.
func TestUserIntentionalMask_FitsUint32(t *testing.T) {
	if bits.Len64(uint64(matcher.UserIntentionalMask)) > 32 {
		t.Fatalf("UserIntentionalMask = %#x does not fit in uint32; encodeStep's narrowing conversion would drop bits",
			uint64(matcher.UserIntentionalMask))
	}
}

// TestNewDigest_RejectsWrongWidths: a truncated salt or digest in the
// config is a corrupt secret. Accepting it would build a Verifier that can
// never match, which is a lockout dressed as a successful start.
func TestNewDigest_RejectsWrongWidths(t *testing.T) {
	goodSum := matcher.HashSteps(fixedSalt, wordSteps)

	tests := []struct {
		name string
		salt []byte
		sum  []byte
	}{
		{name: "nil salt", salt: nil, sum: goodSum},
		{name: "short salt", salt: fixedSalt[:8], sum: goodSum},
		{name: "long salt", salt: append(append([]byte{}, fixedSalt...), 0x00), sum: goodSum},
		{name: "nil hash", salt: fixedSalt, sum: nil},
		{name: "short hash", salt: fixedSalt, sum: goodSum[:16]},
		{name: "long hash", salt: fixedSalt, sum: append(append([]byte{}, goodSum...), 0x00)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := matcher.NewDigest(tc.salt, tc.sum)
			if err == nil {
				t.Fatal("NewDigest accepted a wrong-width input")
			}
			if d != nil {
				t.Error("NewDigest returned a non-nil Digest alongside the error")
			}
		})
	}
}

// TestNewDigest_ErrorNeverEchoesInput mirrors the parser's rule: a
// diagnostic must not put any part of the stored secret material into the
// scrollback. The digest is not itself secret, but the habit is, and an
// error that prints bytes is one refactor away from printing the wrong
// ones.
func TestNewDigest_ErrorNeverEchoesInput(t *testing.T) {
	salt := []byte("SALTYSALTYSALTY!")
	sum := []byte("HASHYHASHY")

	_, err := matcher.NewDigest(salt[:4], sum)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, forbidden := range []string{"SALT", "HASH", hex.EncodeToString(salt), hex.EncodeToString(sum)} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("error message %q contains input material %q", msg, forbidden)
		}
	}
}

// TestNewDigest_CopiesInputs: the caller decodes base64 into slices it may
// reuse or zero. A Digest that aliased them would start matching something
// else the moment the caller moved on.
func TestNewDigest_CopiesInputs(t *testing.T) {
	salt := make([]byte, matcher.SaltLen)
	copy(salt, fixedSalt)
	sum := matcher.HashSteps(fixedSalt, wordSteps)

	d, err := matcher.NewDigest(salt, sum)
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}

	// Scribble over both inputs the way a caller reusing a buffer would.
	for i := range salt {
		salt[i] = 0xff
	}
	for i := range sum {
		sum[i] = 0xff
	}

	if !d.Match(eventsFor(wordSteps)) {
		t.Error("Digest stopped matching after its inputs were overwritten; it aliased them instead of copying")
	}
}

// TestDigest_Match covers the negative space: what must NOT match.
func TestDigest_Match(t *testing.T) {
	d, err := matcher.NewDigest(fixedSalt, matcher.HashSteps(fixedSalt, wordSteps))
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}

	tooLong := make([]matcher.KeyEvent, hotkey.MaxSteps+1)

	tests := []struct {
		name string
		tail []matcher.KeyEvent
		want bool
	}{
		{name: "exact sequence", tail: eventsFor(wordSteps), want: true},
		{name: "empty tail", tail: nil, want: false},
		{name: "tail longer than MaxSteps", tail: tooLong, want: false},
		{
			name: "one keycode changed",
			tail: func() []matcher.KeyEvent {
				e := eventsFor(wordSteps)
				e[2].KeyCode = kvkA
				return e
			}(),
			want: false,
		},
		{
			name: "one user-intentional modifier added",
			tail: func() []matcher.KeyEvent {
				e := eventsFor(wordSteps)
				e[1].Modifiers |= hotkey.ModShift
				return e
			}(),
			want: false,
		},
		{
			name: "correct prefix, one step short",
			tail: eventsFor(wordSteps[:4]),
			want: false,
		},
		{
			name: "correct sequence with one extra step appended",
			tail: append(eventsFor(wordSteps), matcher.KeyEvent{KeyCode: kvkA}),
			want: false,
		},
		{
			name: "same keys in a different order",
			tail: func() []matcher.KeyEvent {
				e := eventsFor(wordSteps)
				e[0], e[1] = e[1], e[0]
				return e
			}(),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.Match(tc.tail); got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDigest_Bounds: a Digest carries no length information, so it must
// offer the whole admissible range to the poller. MaxLen is also what the
// poller clamps the readable ring window by, so it has to be the true
// worst case.
func TestDigest_Bounds(t *testing.T) {
	d, err := matcher.NewDigest(fixedSalt, matcher.HashSteps(fixedSalt, wordSteps))
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}
	if got := d.MinLen(); got != 1 {
		t.Errorf("MinLen = %d, want 1", got)
	}
	if got := d.MaxLen(); got != hotkey.MaxSteps {
		t.Errorf("MaxLen = %d, want %d", got, hotkey.MaxSteps)
	}
}

// TestSequence_Bounds: a plaintext code has exactly one admissible window
// size, so the poller's length loop stays a single iteration and costs
// what it did before Verifier existed.
func TestSequence_Bounds(t *testing.T) {
	s := matcher.NewSequence(wordSteps)
	if got := s.MinLen(); got != len(wordSteps) {
		t.Errorf("MinLen = %d, want %d", got, len(wordSteps))
	}
	if got := s.MaxLen(); got != len(wordSteps) {
		t.Errorf("MaxLen = %d, want %d", got, len(wordSteps))
	}
}

// TestSequence_MaxLen_ZeroForEmpty pins the condition installInternal's
// ErrEmptyUnlockCode guard will be rewritten to use once its
// []hotkey.Spec parameter is gone. A nil interface check alone would admit
// NewSequence(nil): a non-nil Verifier that matches nothing. Installing a
// tap over it raises the shield with no way to lower it, which is the
// worst outcome this project has.
func TestSequence_MaxLen_ZeroForEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []hotkey.Spec
	}{
		{name: "nil steps", steps: nil},
		{name: "empty steps", steps: []hotkey.Spec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := matcher.NewSequence(tc.steps)
			if s == nil {
				t.Fatal("NewSequence returned nil")
			}
			if got := s.MaxLen(); got != 0 {
				t.Errorf("MaxLen = %d, want 0 — the empty-verifier guard depends on this", got)
			}
			if s.Match(nil) {
				t.Error("an empty Sequence matched an empty tail")
			}
		})
	}
}

// TestSequence_Match_MatchesMatchTail: Match is the interface spelling of
// MatchTail and must not drift from it.
func TestSequence_Match_MatchesMatchTail(t *testing.T) {
	s := matcher.NewSequence(wordSteps)
	for _, tail := range [][]matcher.KeyEvent{
		eventsFor(wordSteps),
		eventsFor(wordSteps[:2]),
		nil,
		{{Modifiers: hotkey.ModCtrl, KeyCode: kvkA}},
	} {
		if got, want := s.Match(tail), s.MatchTail(tail); got != want {
			t.Errorf("Match = %v but MatchTail = %v for tail %v", got, want, tail)
		}
	}
}
