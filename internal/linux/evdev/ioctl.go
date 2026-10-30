//go:build linux

package evdev

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Event types and codes from <linux/input-event-codes.h>.
const (
	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02
	evAbs = 0x03
	evMax = 0x1f

	synDropped = 3

	keyMax = 0x2ff
)

// eventSize is sizeof(struct input_event): a struct timeval followed by
// __u16 type, __u16 code and __s32 value.
var eventSize = int(unsafe.Sizeof(unix.Timeval{})) + 8

// ioc builds an ioctl request number the way <asm-generic/ioctl.h> does.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

const (
	iocWrite = 1
	iocRead  = 2
)

// eviocgrab is EVIOCGRAB: _IOW('E', 0x90, int).
var eviocgrab = ioc(iocWrite, 'E', 0x90, unsafe.Sizeof(int32(0)))

// eviocgbit is EVIOCGBIT(ev, len): the capability bitmap for event type ev.
func eviocgbit(ev, size uintptr) uintptr { return ioc(iocRead, 'E', 0x20+ev, size) }

// eviocgkey is EVIOCGKEY(len): the bitmap of keys currently held down.
func eviocgkey(size uintptr) uintptr { return ioc(iocRead, 'E', 0x18, size) }

// eviocgname is EVIOCGNAME(len): the device name.
func eviocgname(size uintptr) uintptr { return ioc(iocRead, 'E', 0x06, size) }

// ioctlPtr issues an ioctl whose argument is a pointer to buf.
func ioctlPtr(fd int, req uintptr, buf []byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

// grab takes (on=true) or drops the exclusive grab of the device: while it
// is held, the kernel delivers the device's events to this fd only — not to
// the compositor, the console or any other reader.
func grab(fd int, on bool) error {
	var v uintptr
	if on {
		v = 1
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), eviocgrab, v)
	if errno != 0 {
		return errno
	}
	return nil
}

// bitmap reads a capability or state bitmap of nbits bits.
func bitmap(fd int, req func(uintptr) uintptr, nbits int) ([]byte, error) {
	buf := make([]byte, nbits/8+1)
	if err := ioctlPtr(fd, req(uintptr(len(buf))), buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// testBit reports whether bit n is set in b.
func testBit(b []byte, n int) bool {
	return n/8 < len(b) && b[n/8]&(1<<(n%8)) != 0
}

// deviceName returns the human-readable device name, or "" on failure.
func deviceName(fd int) string {
	buf := make([]byte, 256)
	if err := ioctlPtr(fd, eviocgname(uintptr(len(buf))), buf); err != nil {
		return ""
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// heldKeys returns the codes of the keys currently held on the device.
func heldKeys(fd int) ([]uint16, error) {
	b, err := bitmap(fd, eviocgkey, keyMax)
	if err != nil {
		return nil, err
	}
	var held []uint16
	for code := 0; code <= keyMax; code++ {
		if testBit(b, code) {
			held = append(held, uint16(code))
		}
	}
	return held, nil
}
