//go:build darwin

package matcher

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
)

const (
	// SaltLen is the length in bytes of the per-config random salt stored
	// under `unlock_salt`. 16 bytes of crypto/rand is far past what a
	// precomputation attack could tabulate, and the value is not secret —
	// it exists so two machines with the same unlock code do not produce
	// the same digest.
	SaltLen = 16

	// hashLen is the SHA-256 output width. Named rather than spelled
	// sha256.Size at every use so NewDigest's length check reads as a
	// statement about the stored format, not about the hash function.
	hashLen = sha256.Size

	// stepLen is the canonical width of one encoded step: 4 bytes of
	// user-intentional modifier bits, then 2 bytes of virtual keycode,
	// both big-endian. Fixed width is what makes the encoding injective —
	// a variable-length encoding would let two different sequences produce
	// the same byte string.
	stepLen = 6

	// hashDomain prefixes every preimage. It pins the scheme: if the
	// algorithm ever changes (a slow KDF, a different canonicalization),
	// the new version gets a new domain string and old digests simply stop
	// matching instead of silently colliding across schemes. Trailing NUL
	// keeps the prefix unambiguously terminated.
	hashDomain = "dndmode-unlock-v1\x00"

	// preimageMax is the largest preimage HashSteps can build: domain +
	// salt + the one-byte step count + MaxSteps encoded steps. Declared so
	// Digest.Match can size a stack array and stay allocation-free in the
	// poller's hot path.
	preimageMax = len(hashDomain) + SaltLen + 1 + hotkey.MaxSteps*stepLen
)

// Verifier decides whether a tail of recorded keystrokes is the unlock
// secret. It exists because there are two storable forms of that secret —
// the plaintext `unlock_code` (a *Sequence) and the salted digest written
// by `--set-password` (a *Digest) — and the poller must not branch on
// which one it holds.
//
// MinLen and MaxLen bound the window sizes the poller assembles from the
// ring snapshot; it tries every length in [MinLen, MaxLen] ending at each
// new keystroke. A *Sequence reports the same value for both, so it costs
// exactly one window per keystroke, unchanged from before this interface
// existed.
//
// Match MUST be a pure function: no IO, no syscalls, no allocations, safe
// to call concurrently. The poller calls it on a 10ms tick, and anything
// that blocks there delays a legitimate unlock.
type Verifier interface {
	MinLen() int
	MaxLen() int
	Match(tail []KeyEvent) bool
}

// Compile-time proof that both storable forms satisfy the interface. If a
// method signature drifts, this fails at build time rather than at the
// call site in the poller.
var (
	_ Verifier = (*Sequence)(nil)
	_ Verifier = (*Digest)(nil)
)

// encodeStep writes the canonical 6-byte encoding of one step into dst,
// which MUST be at least stepLen long.
//
// The mask is applied HERE, before encoding, and that ordering is the
// whole point of routing both sides through this one function. macOS sets
// modifier bits nobody pressed — CapsLock (0x10000), NumPad (0x200000),
// SecondaryFn (0x800000) on the entire function-key group, Help
// (0x400000), NX_NONCOALSESCED (0x100). If `--set-password` hashed raw
// CGEventFlags, a user who happened to have CapsLock on while recording
// would commit a digest carrying 0x10000; the matching side strips that
// bit, so the two could never agree and the machine would be shielded
// forever against a secret its owner typed correctly. Masking on both
// sides is the fail-safe direction, and it is only guaranteed by there
// being a single encoder.
//
// The mask fits in 32 bits (Shift 0x20000 … Cmd 0x100000), so the
// narrowing conversion below cannot lose a set bit — pinned by
// TestUserIntentionalMask_FitsUint32.
func encodeStep(mods hotkey.ModFlag, keycode uint16, dst []byte) {
	binary.BigEndian.PutUint32(dst[0:4], uint32(mods&UserIntentionalMask))
	binary.BigEndian.PutUint16(dst[4:6], keycode)
}

// HashSteps returns the SHA-256 digest of the canonical preimage for the
// given unlock steps under the given salt:
//
//	SHA-256( hashDomain || salt || uint8(len(steps)) || step[0] || … )
//
// The step count is inside the preimage but is never written to disk. It
// commits the digest to one specific length — so no window of a different
// length can collide with it — while leaving the config file with no hint
// about how long the secret is. That combination is deliberate: the
// project treats the secret's length as part of the secret.
//
// Callers pass a salt of SaltLen bytes; a shorter one is accepted here
// (the digest simply commits to what it was given) because the length gate
// belongs to NewDigest, which is what reads untrusted config values.
func HashSteps(salt []byte, steps []hotkey.Spec) []byte {
	var buf [preimageMax]byte
	n := copy(buf[:], hashDomain)
	n += copy(buf[n:], salt)
	buf[n] = byte(len(steps))
	n++
	for _, s := range steps {
		encodeStep(s.Modifiers, s.KeyCode, buf[n:n+stepLen])
		n += stepLen
	}
	sum := sha256.Sum256(buf[:n])
	return sum[:]
}

// Digest is the Verifier backed by a stored salt and SHA-256 digest — the
// form `--set-password` writes and `unlock_hash` carries. Unlike Sequence
// it does not know the secret's length, so it offers every window from 1
// to hotkey.MaxSteps and lets the hash decide.
//
// Immutable after construction (NewDigest copies both inputs), so Match is
// safe to call from any goroutine without locking.
type Digest struct {
	salt []byte
	sum  []byte
}

// NewDigest returns a Digest bound to copies of the given salt and
// expected digest. It rejects anything that is not exactly SaltLen and
// hashLen bytes: a short salt or a truncated digest in the config is a
// corrupt secret, and accepting it would produce a Verifier that can never
// match — a silent lockout.
//
// The error names the field and its expected width and never echoes a
// byte of either value.
func NewDigest(salt, sum []byte) (*Digest, error) {
	if len(salt) != SaltLen {
		return nil, fmt.Errorf("unlock salt must be %d bytes, got %d", SaltLen, len(salt))
	}
	if len(sum) != hashLen {
		return nil, fmt.Errorf("unlock hash must be %d bytes, got %d", hashLen, len(sum))
	}
	d := &Digest{
		salt: make([]byte, SaltLen),
		sum:  make([]byte, hashLen),
	}
	copy(d.salt, salt)
	copy(d.sum, sum)
	return d, nil
}

// MinLen is 1: a digest carries no length information, so the poller must
// offer the shortest possible window. Lengths 2 and 3 can never match in
// practice — config.ValidateUnlockCode rejected them when the secret was
// recorded — but special-casing them here would put a second copy of that
// policy in a package that has no business knowing it.
func (d *Digest) MinLen() int { return 1 }

// MaxLen is hotkey.MaxSteps, the longest unlock code the grammar admits.
// It is also what the poller uses to clamp the readable ring window, so it
// must be the true worst case rather than a typical value.
func (d *Digest) MaxLen() int { return hotkey.MaxSteps }

// Match reports whether tail hashes to the stored digest under the stored
// salt. It rebuilds the canonical preimage through the same encodeStep the
// recording side used, so the modifier mask is applied identically on both
// sides.
//
// Comparison is constant-time. The attacker in the threat model reads the
// config file rather than timing the poller, so this is not load-bearing —
// but it costs nothing and keeps the code honest about what it is
// comparing.
//
// An empty tail, or one longer than MaxLen, returns false without
// hashing: neither can be the secret, and the early exit keeps a
// misconfigured caller from paying for a pointless hash on every tick.
//
// Pure function — no IO, no allocations (the preimage lives in a stack
// array). O(len(tail)).
func (d *Digest) Match(tail []KeyEvent) bool {
	if len(tail) == 0 || len(tail) > hotkey.MaxSteps {
		return false
	}
	var buf [preimageMax]byte
	n := copy(buf[:], hashDomain)
	n += copy(buf[n:], d.salt)
	buf[n] = byte(len(tail))
	n++
	for _, e := range tail {
		encodeStep(e.Modifiers, e.KeyCode, buf[n:n+stepLen])
		n += stepLen
	}
	sum := sha256.Sum256(buf[:n])
	return subtle.ConstantTimeCompare(sum[:], d.sum) == 1
}
