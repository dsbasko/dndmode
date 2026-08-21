//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dsbasko/dndmode/internal/config"
)

// Test_acquirePublishLock_Excludes pins the property the whole file exists for:
// while one holder has the lock, a second caller cannot get it.
//
// flock(2) associates a lock with the open file DESCRIPTION, not with the
// process, so two independent os.OpenFile calls contend even inside one test
// binary — which is what makes this testable at all without spawning a child.
// The corollary matters for the production code: it is the second OPEN that
// blocks, so a caller that reuses a descriptor it already holds would deadlock
// against itself. Neither call site does; each takes the lock once.
func Test_acquirePublishLock_Excludes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	start := time.Now()
	release2, err2 := acquirePublishLock(dir)
	waited := time.Since(start)
	if !errors.Is(err2, errPublishLockBusy) {
		release2()
		t.Fatalf("second acquire = %v, want errPublishLockBusy", err2)
	}
	// The bounded wait has to be a wait, not an immediate refusal: the holders
	// this contends with are a rename away from done, and a caller that gave up
	// on the first EWOULDBLOCK would turn ordinary microsecond overlap into a
	// user-visible failure.
	if waited < publishLockWait {
		t.Errorf("gave up after %v, want at least the full %v wait", waited, publishLockWait)
	}
	// Never nil, on any path — both call sites defer it unconditionally.
	if release2 == nil {
		t.Fatal("release is nil on the busy path; a deferred call would panic")
	}
	release2()

	release()
}

// Test_acquirePublishLock_ReleaseAdmitsTheNextCaller is the other half: the
// lock is a hand-off, not a one-shot. A release that did not actually drop the
// lock would make the FIRST --set-password after a session start refuse
// forever, which is a failure mode nothing else in the binary would explain.
func Test_acquirePublishLock_ReleaseAdmitsTheNextCaller(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	release2, err2 := acquirePublishLock(dir)
	if err2 != nil {
		t.Fatalf("acquire after release: %v", err2)
	}
	release2()
}

// Test_acquirePublishLock_ReleaseIsIdempotent pins the contract run() depends
// on: the section releases explicitly after Manager.Write AND has a deferred
// release behind it, so the closure is called twice on every successful start.
// A second Close on the same *os.File returns an error rather than closing an
// unrelated descriptor that has meanwhile reused the fd number, but the guard
// is what makes that true by construction instead of by luck.
func Test_acquirePublishLock_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release()
	release()

	// Still genuinely released, not wedged by the repeats.
	release2, err2 := acquirePublishLock(dir)
	if err2 != nil {
		t.Fatalf("acquire after repeated release: %v", err2)
	}
	release2()
}

// Test_acquirePublishLock_UnusableDirIsNotBusy separates the two failures every
// call site treats differently. Both refuse, but they refuse with different
// exit codes and different sentences: busy means a peer is mid-publish or
// mid-capture and the answer is "wait, then re-run" (exit 5), while an unusable
// lock file means the coordination is unavailable and the answer names the
// directory to fix (exit 7). Collapsing them would tell a user with an
// unwritable ~/.config to keep waiting for a peer that does not exist.
//
// Both halves are pinned here, sentinel and non-sentinel, because the recovery
// call site reaches this error through withPublishLock — mixed in with
// RecoverFromCrash's own sentinels — and can only dispatch on
// errPublishLockUnusable.
func Test_acquirePublishLock_UnusableDirIsNotBusy(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "no-such-dir")
	release, err := acquirePublishLock(missing)
	if err == nil {
		release()
		t.Fatal("acquire on a missing directory succeeded")
	}
	if errors.Is(err, errPublishLockBusy) {
		t.Errorf("an unopenable lock file reported as busy: %v", err)
	}
	if !errors.Is(err, errPublishLockUnusable) {
		t.Errorf("acquire on a missing directory = %v, want errPublishLockUnusable", err)
	}
	if release == nil {
		t.Fatal("release is nil on the open-failure path; a deferred call would panic")
	}
	release()
}

// Test_acquirePublishLock_DoesNotTruncateTheLockFile pins O_CREATE-without-
// O_TRUNC. Nothing reads the file's bytes, so truncation would be harmless
// today — but it would be a WRITE issued to a file another process is holding a
// lock on, and the next person to put something in there (a PID, a timestamp)
// would find it silently erased by every acquire.
func Test_acquirePublishLock_DoesNotTruncateTheLockFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, publishLockName)
	if err := os.WriteFile(path, []byte("marker"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "marker" {
		t.Errorf("lock file contents = %q, want %q", got, "marker")
	}
}

// Test_withPublishLock_RunsFnUnderTheLock is the property Step 10.5 depends on:
// while fn runs, nobody else can be in the publish section. Crash recovery
// deletes runtime.json, and a delete that overlaps a peer's rename removes the
// LIVE session's snapshot — after which the peer re-check reads an empty path
// and waves a second shield through.
func Test_withPublishLock_RunsFnUnderTheLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran := false
	err := withPublishLock(dir, func() error {
		ran = true
		// A contender inside fn must be refused — this is the whole point.
		release, cerr := acquirePublishLock(dir)
		release()
		if !errors.Is(cerr, errPublishLockBusy) {
			return fmt.Errorf("contender during fn = %v, want errPublishLockBusy", cerr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withPublishLock: %v", err)
	}
	if !ran {
		t.Fatal("fn never ran")
	}
}

// Test_withPublishLock_ReleasesBeforeReturning pins the anti-deadlock half.
// flock(2) tracks the open file description, so a lock still held here would
// make the SECOND acquire — the publish section at Step 13.3, in the same
// process, moments later — wait out the bounded retry and refuse with exit 5 on
// every single start. That would not look like a locking bug; it would look
// like dndmode never starting.
func Test_withPublishLock_ReleasesBeforeReturning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := withPublishLock(dir, func() error { return nil }); err != nil {
		t.Fatalf("withPublishLock: %v", err)
	}

	// Same shape as the Step 13.3 acquire that follows recovery.
	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("acquire after withPublishLock: %v", err)
	}
	release()
}

// Test_withPublishLock_ReleasesAfterFnError pins the same release on the path
// that actually matters more: recovery returning ErrFileDeletePersistent or a
// live-peer refusal still has to leave the lock droppable, or the refusal
// message would be followed by a wedged lock file for whoever comes next.
func Test_withPublishLock_ReleasesAfterFnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sentinel := errors.New("recovery blew up")
	if err := withPublishLock(dir, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("withPublishLock = %v, want the fn error verbatim", err)
	}

	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("acquire after a failed fn: %v", err)
	}
	release()
}

// Test_withPublishLock_BusyDoesNotRunFn pins fail-closed: recovery must not
// touch runtime.json when the lock could not be taken. Running fn anyway would
// make the lock decorative on exactly the path it was added for — the peer
// holding it is the one whose file the delete would hit.
func Test_withPublishLock_BusyDoesNotRunFn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	release, err := acquirePublishLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ran := false
	got := withPublishLock(dir, func() error {
		ran = true
		return nil
	})
	if ran {
		t.Error("fn ran while the lock was held by someone else")
	}
	if !errors.Is(got, errPublishLockBusy) {
		t.Errorf("withPublishLock = %v, want errPublishLockBusy", got)
	}
}

// Test_withPublishLock_UnusableIsDistinguishable pins the sentinel the recovery
// call site dispatches on. Its error is a union — lock failures and
// RecoverFromCrash's own sentinels arrive through the same return value — so a
// broken lock that is not matchable would fall through to the generic exit-2
// "crash recovery failed" instead of the exit-7 sentence that names the
// directory to fix.
func Test_withPublishLock_UnusableIsDistinguishable(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "no-such-dir")
	ran := false
	got := withPublishLock(missing, func() error {
		ran = true
		return nil
	})
	if ran {
		t.Error("fn ran even though the lock file could not be opened")
	}
	if !errors.Is(got, errPublishLockUnusable) {
		t.Errorf("withPublishLock = %v, want errPublishLockUnusable", got)
	}
	if errors.Is(got, errPublishLockBusy) {
		t.Errorf("an unopenable lock file reported as busy: %v", got)
	}
}

// Test_configFingerprint_DistinguishesEveryOutcome pins the three answers the
// Step 13.3 comparison depends on.
//
// The unreadable case is the one worth spelling out. It must NOT equal the
// digest of an empty file: an empty config.yml is a legal file that Load
// accepts, so borrowing sha256("") for "I could not look" would let a session
// whose config was deleted mid-startup compare equal against a config that was
// merely emptied — and publish a shield keyed to a secret that is no longer on
// disk.
func Test_configFingerprint_DistinguishesEveryOutcome(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	if err := os.WriteFile(path, []byte("unlock_code: ctrl+option+cmd+x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := configFingerprint(path)

	if got := configFingerprint(path); got != before {
		t.Error("fingerprint changed without the file changing")
	}

	if err := os.WriteFile(path, []byte("unlock_code: ctrl+option+cmd+y\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after := configFingerprint(path)
	if after == before {
		t.Error("fingerprint survived a rewrite; a concurrent --set-password would go unnoticed")
	}

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("empty: %v", err)
	}
	empty := configFingerprint(path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	unreadable := configFingerprint(path)
	if unreadable == empty {
		t.Error("an unreadable config hashes the same as an empty one")
	}
	if unreadable == before || unreadable == after {
		t.Error("an unreadable config hashes the same as a real one")
	}
}

// Test_configFingerprintOf_AgreesWithTheFileHalf pins the coupling the two
// halves of the publish-time comparison depend on: run() fingerprints the bytes
// config.Loader.LoadWithSource parsed, Step 13.3 fingerprints the file on disk,
// and the comparison between them is only meaningful while both hash the same
// way. A domain prefix, a normalization step or a different hash on either side
// would make every session refuse to start with "config.yml changed" on a file
// nobody touched.
//
// Both of Load's paths are covered, because they produce the bytes differently:
// an existing config comes back from os.ReadFile, while a first run comes back
// from the template writeDefault just rendered and never re-reads.
func Test_configFingerprintOf_AgreesWithTheFileHalf(t *testing.T) {
	t.Parallel()

	t.Run("existing config", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(path, []byte("unlock_code: ctrl+option+cmd+x\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		_, raw, created, err := config.NewLoader(path).LoadWithSource()
		if err != nil {
			t.Fatalf("LoadWithSource: %v", err)
		}
		if created {
			t.Fatal("created=true for a file that already existed")
		}
		if got, want := configFingerprintOf(raw), configFingerprint(path); got != want {
			t.Error("the parsed bytes and the file on disk hash differently; " +
				"the Step 13.3 comparison would fire on an untouched config")
		}
	})

	t.Run("first run", func(t *testing.T) {
		t.Parallel()

		// A subdirectory that does not exist yet, so writeDefault's MkdirAll
		// runs too — the first-run path in full.
		path := filepath.Join(t.TempDir(), "dndmode", "config.yml")

		_, raw, created, err := config.NewLoader(path).LoadWithSource()
		if err != nil {
			t.Fatalf("LoadWithSource: %v", err)
		}
		if !created {
			t.Fatal("created=false for a config that did not exist")
		}
		if got, want := configFingerprintOf(raw), configFingerprint(path); got != want {
			t.Error("the bytes writeDefault returned differ from the ones it published")
		}
	})
}

// Test_configFingerprintOf_SeesEveryRewrite is the property that makes the
// startup half worth taking at all: the digest must follow the bytes, so that a
// --set-password landing between startup and publish is caught. It is the
// byte-level twin of Test_configFingerprint_DistinguishesEveryOutcome, which
// covers the path-reading half.
func Test_configFingerprintOf_SeesEveryRewrite(t *testing.T) {
	t.Parallel()

	before := configFingerprintOf([]byte("unlock_code: ctrl+option+cmd+x\n"))
	if again := configFingerprintOf([]byte("unlock_code: ctrl+option+cmd+x\n")); again != before {
		t.Error("identical bytes hashed differently")
	}
	if after := configFingerprintOf([]byte("unlock_code: ctrl+option+cmd+y\n")); after == before {
		t.Error("a changed secret hashed the same; a concurrent --set-password would go unnoticed")
	}
}
