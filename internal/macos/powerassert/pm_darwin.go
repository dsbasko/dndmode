//go:build darwin

package powerassert

/*
#cgo CFLAGS: -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation

#include <IOKit/pwr_mgt/IOPMLib.h>
#include <stdint.h>

extern IOReturn powerassert_acquire(const char *name, IOPMAssertionID *out_id);
extern IOReturn powerassert_release(IOPMAssertionID id);
extern int      powerassert_count_by_name(const char *want_name, const char *want_type, int own_pid);
extern int      powerassert_enumerate_matching(const char *want_name, const char *want_type, int own_pid, int *out_pids, uint32_t *out_ids, int cap);
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// acquireRaw bridges to powerassert_acquire (C). Returns the IOPM
// assertion ID and an IOReturn-coded error. Caller wraps the ID in an
// Assertion struct.
//
// rc semantics: kIOReturnSuccess == 0; any non-zero value is an error
// formatted as `IOPMAssertionCreateWithName: rc=0x%x` so the IOReturn
// code is grep-able in production logs.
func acquireRaw(name string) (uint32, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var id C.IOPMAssertionID
	rc := C.powerassert_acquire(cname, &id)
	if rc != 0 {
		return 0, fmt.Errorf("IOPMAssertionCreateWithName: rc=0x%x", int32(rc))
	}
	return uint32(id), nil
}

// releaseRaw bridges to powerassert_release (C). Caller is responsible
// for idempotency at the Go level (see Assertion.Release).
//
// Apple does not publicly document the exact IOReturn value returned on
// double-release; if a non-zero value comes back we surface it formatted.
// The two-layer guard in Assertion.Release ensures we never actually
// double-release in production.
func releaseRaw(id uint32) error {
	rc := C.powerassert_release(C.IOPMAssertionID(id))
	if rc != 0 {
		return fmt.Errorf("IOPMAssertionRelease: rc=0x%x", int32(rc))
	}
	return nil
}

// countOwnByName returns the number of assertions held by the given PID
// whose AssertionName matches name AND whose AssertionType matches
// typeName. Used by the smoke test for own-PID re-read verification
// of the Acquire / Release roundtrip.
//
// Note the semantic difference from enumerateMatching: countOwnByName
// does NOT exclude own_pid — it counts assertions belonging to it, since
// the caller passes its own PID to verify "I just acquired one and it
// is now visible". Passing -1 as own_pid would degenerate to "count all
// matching assertions across all PIDs" but the smoke test never needs
// that path.
//
// Edge case: the underlying IOPMCopyAssertionsByProcess can return
// kIOReturnSuccess with a NULL or empty dictionary (no assertions on
// the system); C side returns 0 in that case per guidance ("treat
// NULL as success, zero orphans").
func countOwnByName(name, typeName string, ownPID int) int {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	cType := C.CString(typeName)
	defer C.free(unsafe.Pointer(cType))

	n := C.powerassert_count_by_name(cName, cType, C.int(ownPID))
	return int(n)
}

// enumerateMatching scans IOPMCopyAssertionsByProcess for assertions
// whose AssertionName + AssertionType match name + typeName and whose
// owning PID is NOT ownPID. Returns parallel slices of (pid, id) tuples
// describing assertions held by OTHER processes — the orphan-cleanup
// search space.
//
// Capacity is fixed at 64 — vastly more than any realistic dndmode dev
// machine could accumulate (the canonical case is a single orphan from
// the most recent SIGKILL'd instance). The C side clips silently if more
// than 64 match; the Go side returns whatever it got.
//
// Used by CleanupOrphans wrapper AND by the smoke test
// in this plan, so the function is exported into the package
// (unexported identifier, but visible to white-box tests).
func enumerateMatching(name, typeName string, ownPID int) (pids []int, ids []uint32, err error) {
	const cap = 64

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	cType := C.CString(typeName)
	defer C.free(unsafe.Pointer(cType))

	var pidBuf [cap]C.int
	var idBuf [cap]C.uint32_t

	n := C.powerassert_enumerate_matching(
		cName, cType, C.int(ownPID),
		&pidBuf[0], &idBuf[0], C.int(cap),
	)
	if n < 0 {
		return nil, nil, fmt.Errorf("powerassert_enumerate_matching: rc=%d", int(n))
	}

	count := int(n)
	pids = make([]int, count)
	ids = make([]uint32, count)
	for i := 0; i < count; i++ {
		pids[i] = int(pidBuf[i])
		ids[i] = uint32(idBuf[i])
	}
	return pids, ids, nil
}
