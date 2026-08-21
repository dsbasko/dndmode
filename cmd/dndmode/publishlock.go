//go:build darwin

package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// publishLockName is the file both paths flock, a sibling of runtime.json in
// ~/.config/dndmode. It is a DEDICATED file rather than config.yml or
// runtime.json for one reason: flock(2) locks an inode, not a name, and both of
// those files are replaced by temp+rename on every write — the moment
// SaveUnlockHash or Manager.Write lands, a lock held on the old inode guards
// nothing and a lock taken on the new one excludes nobody. This file is created
// once and never rewritten, moved or deleted, so every process that opens it
// contends for the same inode.
//
// It is also never cleaned up, and that is deliberate: an empty 0600 byte-less
// file is the whole cost, while removing it would reintroduce exactly the
// inode-identity hole it exists to avoid (unlink + recreate hands the next
// caller a different inode to lock while the previous holder still owns the
// old one).
const publishLockName = "runtime.lock"

// errPublishLockBusy is returned when the lock is held by another process and
// stayed held for the whole bounded wait. It is a distinct sentinel because the
// two callers phrase it differently — a session says "another dndmode is
// starting or writing its password", --set-password says "a session claimed the
// keyboard while you were typing" — but neither may confuse it with "the lock
// file itself is unusable".
var errPublishLockBusy = errors.New("publish lock held by another process")

// errPublishLockUnusable wraps every failure that is NOT "someone else has it":
// the lock file could not be opened, or flock refused for a reason other than
// EWOULDBLOCK. It exists because the two call sites reach the lock through
// different plumbing — the publish section switches on the acquire error
// directly, while the recovery section gets it back through withPublishLock
// mixed in with the recovery's own errors — and the second one cannot tell
// "the coordination is unavailable" (exit 7, name the directory) from "recovery
// itself failed" (exit 2) without a sentinel to match on.
var errPublishLockUnusable = errors.New("publish lock unusable")

// publishLockWait bounds how long acquirePublishLock retries.
//
// It is NOT sized to outlast a legitimate hold, and cannot be: the session side
// holds the lock for a handful of syscalls, but --set-password holds it from
// before its input tap goes in until the new hash is renamed into place, which
// is however long the operator takes to type a secret twice — bounded only by
// the capture ceilings, on the order of minutes. Waiting that out would leave
// the caller looking hung for exactly as long.
//
// So two seconds buys the ordinary case (a peer a rename away from done) and
// then refuses, which is recoverable: both callers tell the user to re-run, and
// the message on the session path already names a --set-password as one of the
// two possible holders. Blocking indefinitely there would not be recoverable.
const publishLockWait = 2 * time.Second

// publishLockPoll is the retry cadence inside publishLockWait. flock(2) has no
// timed variant on Darwin, and the blocking LOCK_EX form would park the OS
// thread with no way to give up, so the wait is spelled out as LOCK_NB plus
// sleep rather than handed to the kernel.
const publishLockPoll = 20 * time.Millisecond

// acquirePublishLock takes the exclusive advisory lock that serializes the two
// operations which must never interleave:
//
//   - a session resolving the unlock verifier and publishing runtime.json
//     (cmd/dndmode/main.go Step 13.3), and
//   - --set-password checking for a live peer, capturing under its own input
//     tap, and renaming the new hash into config.yml (runSetPasswordAt).
//
// Without it those two overlap in a way neither can detect, and they do it
// twice over.
//
// The first way is about the secret. A session resolves the OLD verifier at
// Step 5b, then spends an unbounded stretch in WaitForGrants, recovery and the
// IOPMAssertion acquire; --set-password probes runtime.json during that
// stretch, sees nothing, and saves. The session then publishes and raises a
// shield that answers only to a code the config no longer names, while the
// person who just set the new one is told it took. Neither side did anything
// wrong and neither side can see the other, because runtime.json — the only
// thing they share — is written after the damage is already decided.
//
// The second way is about the keyboard, and it is why --set-password takes the
// lock BEFORE its capture rather than just before its save. Its capture tap is
// head-inserted at kCGHIDEventTap and returns NULL for every event, so a
// capture running beside a live session sits in front of that session's tap and
// eats the unlock code its owner is typing: the shield is up, the machine looks
// dead, and it stays that way until a capture ceiling fires. Holding the lock
// across the capture is what makes the session refuse to start instead — before
// it raises anything — and what keeps two --set-password runs from capturing at
// once and racing their saves.
//
// # Why flock and not a PID file
//
// The lock has to be released by a process that is SIGKILLed mid-capture, and
// it has to be released without anyone deciding whether a recorded PID is
// stale. flock(2) is released by the kernel on last-close, which covers exit,
// crash and kill alike; a PID file would need the same liveness guess that
// runtime.json already needs and would fail in the same way.
//
// Advisory, not mandatory: nothing stops a third program from rewriting
// config.yml behind both of them. The lock coordinates dndmode with dndmode,
// which is the only case where both sides are ours to fix.
//
// The returned release closure is idempotent and NEVER nil — on every failure
// it is a no-op — so callers can defer it unconditionally and still call it
// explicitly at the end of a section that must not hold the lock for the rest
// of the process's life.
func acquirePublishLock(dir string) (release func(), err error) {
	noop := func() {}
	path := filepath.Join(dir, publishLockName)
	// O_CREATE, never O_TRUNC: the file's contents are irrelevant (nothing ever
	// writes to it) and truncating would be a write to a file another process
	// is holding a lock on for reasons that have nothing to do with its bytes.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return noop, fmt.Errorf("%w: open %s: %w", errPublishLockUnusable, path, err)
	}

	deadline := time.Now().Add(publishLockWait)
	for {
		lerr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if lerr == nil {
			var once bool
			return func() {
				if once {
					return
				}
				once = true
				// Close drops the lock. Unlocking first would be a second
				// syscall that can only fail in ways Close covers anyway.
				_ = f.Close()
			}, nil
		}
		// EWOULDBLOCK is the only "someone else has it" answer flock gives;
		// anything else (EBADF, EOPNOTSUPP on a filesystem without flock) is a
		// broken lock rather than a busy one, and retrying it would just burn
		// the whole wait before reporting the same thing.
		if !errors.Is(lerr, unix.EWOULDBLOCK) {
			_ = f.Close()
			return noop, fmt.Errorf("%w: lock %s: %w", errPublishLockUnusable, path, lerr)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return noop, errPublishLockBusy
		}
		time.Sleep(publishLockPoll)
	}
}

// withPublishLock runs fn while holding the publish lock for dir and drops the
// lock before returning, whatever fn does. On a lock failure fn does NOT run
// and the lock error is returned unchanged, so the caller can match
// errPublishLockBusy / errPublishLockUnusable on it.
//
// It exists for crash recovery, which is the third operation on runtime.json
// and the one that is easiest to miss: it READS the snapshot, spends up to two
// five-second best-effort subprocesses acting on what it read, and only then
// DELETES the file — and what it deletes is whatever is at the path when the
// unlink runs, not the snapshot it read. Two sessions started together both
// find the same stale file and both take the dead-PID branch; whichever is out
// first can be through the publish section with its own runtime.json renamed
// into place before the second one reaches its delete. That delete then removes
// the LIVE session's file, the under-lock peer re-check that follows reads an
// empty path, and the second session raises a second shield beside the first —
// which lands right back at the invisible live session this lock was added to
// prevent, since --set-password probes that same path and finds nothing.
//
// The fix is serialization rather than a compare-and-delete on the snapshot,
// because Manager.Write — the only other writer of that path — already takes
// this lock: putting recovery's whole read-decide-delete inside it makes the
// interleaving impossible instead of unlikely.
//
// Whole-function scope matters for a second reason. flock(2) tracks the open
// file DESCRIPTION, so a caller still holding this lock when it reaches the
// publish section would open the file again and deadlock against itself — a
// bounded wait, then exit 5, on every single start. The defer here is what
// makes "released before the caller moves on" structural instead of a rule
// someone has to remember at four return sites.
func withPublishLock(dir string, fn func() error) error {
	release, err := acquirePublishLock(dir)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// configFingerprint is the SHA-256 of config.yml's raw bytes, taken so a
// session can tell whether the file it resolved its unlock verifier from is
// still the file on disk when it comes time to publish runtime.json.
//
// Raw bytes rather than a re-resolve: re-running config.ResolveUnlockCode would
// apply the unlock_code / unlock_hash / hotkey precedence table a second time,
// which its doc comment specifically asks callers not to do, and would leave
// the session silently adopting a secret its operator has not seen it accept.
// A byte comparison answers the only question that matters here — "did this
// file change under me?" — and answers it for every key at once.
//
// An unreadable file returns a sentinel digest rather than an error, so the
// caller has one comparison instead of two branches. The sentinel is a hash of
// a fixed domain string rather than of no input at all, because sha256 of the
// empty byte slice is a perfectly reachable digest of a real (empty) config and
// must not be borrowed for "I could not look". A session whose config vanished
// between Load and publish therefore sees a mismatch and refuses, which is the
// conservative answer; two consecutive unreadable calls compare equal, which
// cannot arise here because the STARTUP side never reads the path at all — it
// fingerprints the bytes Load parsed (see configFingerprintOf).
func configFingerprint(path string) [sha256.Size]byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return sha256.Sum256([]byte("dndmode-config-unreadable-v1"))
	}
	return configFingerprintOf(b)
}

// configFingerprintOf is the STARTUP half of that comparison, and it takes
// bytes rather than a path on purpose: the fingerprint has to describe the
// exact config.yml that produced the verifier this session will match against,
// and the only place those bytes exist is the return value of
// config.Loader.LoadWithSource. Re-reading the path here instead would reopen
// the split it closes — SaveUnlockHash publishes by atomic rename, so a read
// taken one statement after Load can already be the NEXT file, leaving the
// session carrying the OLD secret under the NEW file's fingerprint and sailing
// through the re-check at publish time. See LoadWithSource for the full shape.
//
// One line, but a named one: "both halves hash the same way" is then something
// a test can pin rather than something two call sites happen to agree on.
func configFingerprintOf(b []byte) [sha256.Size]byte {
	return sha256.Sum256(b)
}
