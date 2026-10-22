//go:build linux

package evdev

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dsbasko/dndmode/internal/config/hotkey"
	"github.com/dsbasko/dndmode/internal/matcher"
)

// inputDir is where the kernel's evdev nodes live.
const inputDir = "/dev/input"

// releaseWait bounds how long the initial grab waits for keys that are
// already held to come up. Grabbing a device while a key is down hides the
// key-UP from the compositor, which then keeps auto-repeating it — the
// Return that launched `dndmode` from a terminal would flood that terminal
// with newlines for the whole session.
const releaseWait = 3 * time.Second

// rescanInterval is the safety-net cadence for picking up hot-plugged
// devices; inotify on /dev/input normally gets there first.
const rescanInterval = time.Second

// Sentinel errors. Use errors.Is.
var (
	// ErrPermission: an input device exists that this user cannot open, so
	// a keyboard plugged into it would bypass the lock.
	ErrPermission = errors.New("evdev: no access to /dev/input (add the user to the 'input' group and log in again)")
	// ErrBusy: another process already holds an exclusive grab of a device.
	ErrBusy = errors.New("evdev: an input device is exclusively grabbed by another process")
	// ErrNoDevices: there is no input device to grab at all.
	ErrNoDevices = errors.New("evdev: no input devices found under /dev/input")
)

// device is one grabbed /dev/input/eventN node.
type device struct {
	path string
	name string
	fd   int
	f    *os.File
}

// rawEvent is what a device reader hands to the tracker goroutine.
type rawEvent struct {
	dev     *device
	code    uint16
	value   int32
	gone    bool // the device disappeared (unplugged) or failed
	dropped bool // the kernel dropped events (SYN_DROPPED); resync needed
}

// grabber holds the exclusive grab of every input device and turns their
// key events into a stream of recorded key presses.
type grabber struct {
	log     *slog.Logger
	raw     chan rawEvent
	presses chan matcher.KeyEvent
	stop    chan struct{}
	wg      sync.WaitGroup

	mu      sync.Mutex
	devs    map[string]*device
	closed  bool
	inotify *os.File
}

// openGrabber grabs every input device that exists now and keeps grabbing
// the ones that appear later, until close.
//
// It fails closed: a device that cannot be opened (ErrPermission) or is
// grabbed by someone else (ErrBusy) aborts the whole call with nothing held,
// because a lock with a hole in it is worse than no lock.
func openGrabber(log *slog.Logger) (*grabber, error) {
	if log == nil {
		log = slog.Default()
	}
	paths, err := eventNodes()
	if err != nil {
		return nil, err
	}
	g := &grabber{
		log:     log,
		raw:     make(chan rawEvent, 256),
		presses: make(chan matcher.KeyEvent, 64),
		stop:    make(chan struct{}),
		devs:    map[string]*device{},
	}

	var cands []*device
	fail := func(err error) (*grabber, error) {
		for _, d := range cands {
			_ = grab(d.fd, false)
			_ = unix.Close(d.fd)
		}
		return nil, err
	}
	for _, p := range paths {
		d, err := openDevice(p)
		switch {
		case errors.Is(err, errNotInput):
			continue
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
			return fail(fmt.Errorf("%w: %s", ErrPermission, p))
		case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENODEV):
			continue // unplugged while we were listing
		case err != nil:
			return fail(fmt.Errorf("open %s: %w", p, err))
		}
		cands = append(cands, d)
	}
	if len(cands) == 0 {
		return nil, ErrNoDevices
	}

	waitReleased(cands, releaseWait, log)

	for _, d := range cands {
		if err := grab(d.fd, true); err != nil {
			if errors.Is(err, unix.EBUSY) {
				return fail(fmt.Errorf("%w: %s (%s)", ErrBusy, d.path, d.name))
			}
			return fail(fmt.Errorf("grab %s: %w", d.path, err))
		}
	}

	for _, d := range cands {
		d.f = os.NewFile(uintptr(d.fd), d.path)
		g.devs[d.path] = d
		g.startReader(d)
		log.Debug("evdev: grabbed", slog.String("path", d.path), slog.String("name", d.name))
	}

	g.wg.Go(g.track)
	g.startHotplug()
	return g, nil
}

// errNotInput marks a node that produces no key, pointer or touch events
// (lid switch, audio jack, ...). Those are left alone.
var errNotInput = errors.New("not an input device")

// openDevice opens one event node non-blocking and checks that it is worth
// grabbing.
func openDevice(path string) (*device, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	caps, err := bitmap(fd, func(n uintptr) uintptr { return eviocgbit(0, n) }, evMax)
	if err != nil || !(testBit(caps, evKey) || testBit(caps, evRel) || testBit(caps, evAbs)) {
		_ = unix.Close(fd)
		return nil, errNotInput
	}
	return &device{path: path, name: deviceName(fd), fd: fd}, nil
}

// eventNodes lists /dev/input/event*.
func eventNodes() ([]string, error) {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", inputDir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "event") {
			out = append(out, filepath.Join(inputDir, e.Name()))
		}
	}
	return out, nil
}

// waitReleased polls until no key is held on any device, or until max.
func waitReleased(devs []*device, max time.Duration, log *slog.Logger) {
	deadline := time.Now().Add(max)
	for {
		busy := false
		for _, d := range devs {
			if held, err := heldKeys(d.fd); err == nil && len(held) > 0 {
				busy = true
				break
			}
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			log.Warn("evdev: keys still held, grabbing anyway")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startReader runs the read loop of one device.
func (g *grabber) startReader(d *device) {
	g.wg.Go(func() {
		buf := make([]byte, eventSize*64)
		ts := eventSize - 8
		for {
			n, err := d.f.Read(buf)
			if err != nil {
				g.send(rawEvent{dev: d, gone: true})
				g.forget(d, err)
				return
			}
			for off := 0; off+eventSize <= n; off += eventSize {
				typ := binary.NativeEndian.Uint16(buf[off+ts:])
				code := binary.NativeEndian.Uint16(buf[off+ts+2:])
				val := int32(binary.NativeEndian.Uint32(buf[off+ts+4:]))
				switch {
				case typ == evKey:
					g.send(rawEvent{dev: d, code: code, value: val})
				case typ == evSyn && code == synDropped:
					g.send(rawEvent{dev: d, dropped: true})
				}
			}
		}
	})
}

// send forwards ev to the tracker unless the grabber is shutting down.
func (g *grabber) send(ev rawEvent) {
	select {
	case g.raw <- ev:
	case <-g.stop:
	}
}

// forget drops a device whose reader ended. During close the error is just
// the fd being closed under the reader; otherwise the device went away.
func (g *grabber) forget(d *device, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	if g.devs[d.path] == d {
		delete(g.devs, d.path)
		_ = d.f.Close()
		g.log.Debug("evdev: device gone", slog.String("path", d.path), slog.Any("err", err))
	}
}

// held returns the keys currently down on d, for SYN_DROPPED resync.
func (g *grabber) held(d *device) []uint16 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.devs[d.path] != d {
		return nil
	}
	keys, _ := heldKeys(d.fd)
	return keys
}

// track is the single goroutine that owns modifier state.
func (g *grabber) track() {
	t := newTracker()
	for {
		select {
		case <-g.stop:
			return
		case ev := <-g.raw:
			ke, ok := t.feed(ev, g.held)
			if !ok {
				continue
			}
			select {
			case g.presses <- ke:
			case <-g.stop:
				return
			}
		}
	}
}

// startHotplug grabs devices that appear while the grabber is open: inotify
// on /dev/input for promptness, a periodic rescan as the safety net (udev
// may fix the node's permissions after the CREATE event fired).
func (g *grabber) startHotplug() {
	ifd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err == nil {
		if _, werr := unix.InotifyAddWatch(ifd, inputDir, unix.IN_CREATE|unix.IN_ATTRIB); werr != nil {
			_ = unix.Close(ifd)
			err = werr
		}
	}
	if err != nil {
		g.log.Warn("evdev: inotify unavailable, hot-plugged devices are picked up by rescan only", slog.Any("err", err))
	} else {
		g.inotify = os.NewFile(uintptr(ifd), "inotify")
		g.wg.Go(func() {
			buf := make([]byte, 4096)
			for {
				if _, err := g.inotify.Read(buf); err != nil {
					return
				}
				g.rescan()
			}
		})
	}
	g.wg.Go(func() {
		t := time.NewTicker(rescanInterval)
		defer t.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-t.C:
				g.rescan()
			}
		}
	})
}

// rescan grabs every event node that is not grabbed yet.
func (g *grabber) rescan() {
	paths, err := eventNodes()
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	for _, p := range paths {
		if _, ok := g.devs[p]; ok {
			continue
		}
		d, err := openDevice(p)
		if err != nil {
			continue // not an input device, or udev has not opened it up yet
		}
		if err := grab(d.fd, true); err != nil {
			_ = unix.Close(d.fd)
			g.log.Warn("evdev: cannot grab hot-plugged device", slog.String("path", p), slog.Any("err", err))
			continue
		}
		d.f = os.NewFile(uintptr(d.fd), d.path)
		g.devs[p] = d
		g.startReader(d)
		g.log.Debug("evdev: grabbed hot-plugged device", slog.String("path", p), slog.String("name", d.name))
	}
}

// close drops every grab and waits for all goroutines. Idempotent.
func (g *grabber) close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.stop)
	for _, d := range g.devs {
		_ = grab(d.fd, false)
		_ = d.f.Close()
	}
	if g.inotify != nil {
		_ = g.inotify.Close()
	}
	g.mu.Unlock()
	g.wg.Wait()
}

// tracker turns raw key events into recorded presses carrying the
// modifiers held at that moment, across all devices.
type tracker struct {
	held map[*device]map[uint16]bool
}

func newTracker() *tracker { return &tracker{held: map[*device]map[uint16]bool{}} }

// feed applies one raw event. It returns a KeyEvent and true only for a
// fresh press (not auto-repeat) of a recordable key: holding a key down
// therefore contributes exactly one step, as on macOS.
func (t *tracker) feed(ev rawEvent, resync func(*device) []uint16) (matcher.KeyEvent, bool) {
	switch {
	case ev.gone:
		delete(t.held, ev.dev)
		return matcher.KeyEvent{}, false
	case ev.dropped:
		m := map[uint16]bool{}
		for _, c := range resync(ev.dev) {
			m[c] = true
		}
		t.held[ev.dev] = m
		return matcher.KeyEvent{}, false
	}
	m := t.held[ev.dev]
	if m == nil {
		m = map[uint16]bool{}
		t.held[ev.dev] = m
	}
	switch ev.value {
	case 0:
		delete(m, ev.code)
		return matcher.KeyEvent{}, false
	case 1:
		m[ev.code] = true
	default: // 2 = auto-repeat
		return matcher.KeyEvent{}, false
	}
	if !recordable(ev.code) {
		return matcher.KeyEvent{}, false
	}
	return toKeyEvent(ev.code, t.mods()), true
}

// mods ORs the modifier bits of every key held on any device.
func (t *tracker) mods() hotkey.ModFlag {
	var m hotkey.ModFlag
	for _, keys := range t.held {
		for c := range keys {
			m |= modifierBit[c]
		}
	}
	return m
}

// DeviceNames lists the names of the devices dndmode would grab, for
// diagnostics. Errors are skipped.
func DeviceNames() []string {
	paths, _ := eventNodes()
	var names []string
	for _, p := range paths {
		d, err := openDevice(p)
		if err != nil {
			continue
		}
		names = append(names, d.name)
		_ = unix.Close(d.fd)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// CheckAccess reports whether every input device can be opened, without
// grabbing anything. It is the pre-flight equivalent of the macOS
// Accessibility / Input Monitoring check.
func CheckAccess() error {
	paths, err := eventNodes()
	if err != nil {
		return err
	}
	opened := 0
	for _, p := range paths {
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		switch {
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
			return fmt.Errorf("%w: %s", ErrPermission, p)
		case err != nil:
			continue
		}
		_ = unix.Close(fd)
		opened++
	}
	if opened == 0 {
		return ErrNoDevices
	}
	return nil
}
