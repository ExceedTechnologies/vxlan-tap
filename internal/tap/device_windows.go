package tap

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CTL_CODE(FILE_DEVICE_UNKNOWN, fn, METHOD_BUFFERED, FILE_ANY_ACCESS)
func tapIoctl(fn uint32) uint32 { return 0x22<<16 | fn<<2 }

var (
	ioctlGetMAC         = tapIoctl(1)
	ioctlGetMTU         = tapIoctl(3)
	ioctlSetMediaStatus = tapIoctl(6)
)

// ErrClosed is returned by Read and Write after Close.
var ErrClosed = errors.New("tap: device closed")

const (
	// Requests kept in flight per direction. One at a time serialises every
	// frame behind a driver round trip; a handful hides that latency.
	numReads  = 32
	numWrites = 32
	bufSize   = 1 << 16 // largest frame the driver can carry
	batchSize = 64      // completions dequeued per wakeup
)

// Not wrapped by x/sys/windows.
var procGetQueuedCompletionStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetQueuedCompletionStatusEx")

// overlappedEntry is OVERLAPPED_ENTRY.
type overlappedEntry struct {
	key      uintptr
	ov       *windows.Overlapped
	internal uintptr // NTSTATUS of the completed request
	n        uint32
}

// op is one overlapped request slot. ov must be the first field: completions
// are mapped back to their op by address.
type op struct {
	ov    windows.Overlapped
	write bool
	buf   []byte
	n     int
	err   error
}

// Device is an open TAP adapter. Reads and writes are overlapped and
// completed through an I/O completion port, with numReads reads always
// pending and up to numWrites writes in flight. One goroutine may Read
// while another Writes; Close may be called from any goroutine.
type Device struct {
	Adapter
	h    windows.Handle
	port windows.Handle

	// Issuers hold mu.RLock while starting a request so that Close (which
	// takes the write lock) knows nothing new can start once closed is set.
	mu       sync.RWMutex
	closed   atomic.Bool
	inflight atomic.Int64

	ops    []*op    // keeps every slot reachable while the kernel uses it
	readyR chan *op // completed reads, in completion order
	freeW  chan *op // idle write slots

	loopDone  chan struct{}
	loopErr   error
	writeErrs atomic.Uint64
	closeOnce sync.Once
}

// Open opens the adapter, sets its media status to connected and starts
// reading.
func Open(a Adapter) (*Device, error) {
	path, err := windows.UTF16PtrFromString(`\\.\Global\` + a.GUID + `.tap`)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_SYSTEM|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return nil, fmt.Errorf("tap: open %s (%s): access denied; run as Administrator", a.Name, a.GUID)
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
			return nil, fmt.Errorf("tap: open %s (%s): not found; is the adapter disabled?", a.Name, a.GUID)
		case errors.Is(err, windows.ERROR_GEN_FAILURE):
			return nil, fmt.Errorf("tap: open %s (%s): adapter is already in use by another program", a.Name, a.GUID)
		}
		return nil, fmt.Errorf("tap: open %s (%s): %w", a.Name, a.GUID, err)
	}

	d := &Device{
		Adapter:  a,
		h:        h,
		readyR:   make(chan *op, numReads),
		freeW:    make(chan *op, numWrites),
		loopDone: make(chan struct{}),
	}
	if err := d.setMediaStatus(true); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if d.port, err = windows.CreateIoCompletionPort(h, 0, 0, 1); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("tap: CreateIoCompletionPort: %w", err)
	}

	for i := 0; i < numWrites; i++ {
		o := &op{write: true, buf: make([]byte, bufSize)}
		d.ops = append(d.ops, o)
		d.freeW <- o
	}
	go d.loop()
	for i := 0; i < numReads; i++ {
		o := &op{buf: make([]byte, bufSize)}
		d.ops = append(d.ops, o)
		if err := d.start(o, o.buf); err != nil {
			d.Close()
			return nil, fmt.Errorf("tap: start read: %w", err)
		}
	}
	return d, nil
}

// start issues an overlapped read or write on o. Its completion, including
// immediate success, is always delivered through the port.
func (d *Device) start(o *op, b []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed.Load() {
		return ErrClosed
	}
	o.ov = windows.Overlapped{}
	d.inflight.Add(1)
	var err error
	if o.write {
		err = windows.WriteFile(d.h, b, nil, &o.ov)
	} else {
		err = windows.ReadFile(d.h, b, nil, &o.ov)
	}
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		d.inflight.Add(-1)
		return err
	}
	return nil
}

// loop dequeues completions and hands them to Read (via readyR) or back to
// the write pool. It exits once the device is closed and nothing is in
// flight, so no slot is reused or freed while the kernel still owns it.
func (d *Device) loop() {
	defer close(d.loopDone)
	var entries [batchSize]overlappedEntry
	for {
		var n uint32
		r, _, e := procGetQueuedCompletionStatusEx.Call(uintptr(d.port),
			uintptr(unsafe.Pointer(&entries[0])), batchSize,
			uintptr(unsafe.Pointer(&n)), windows.INFINITE, 0)
		if r == 0 {
			d.loopErr = fmt.Errorf("tap: GetQueuedCompletionStatusEx: %w", e)
			return
		}
		for _, ent := range entries[:n] {
			if ent.ov == nil {
				continue // wakeup posted by Close
			}
			o := (*op)(unsafe.Pointer(ent.ov))
			o.n = int(ent.n)
			o.err = nil
			if st := windows.NTStatus(ent.internal); st != 0 {
				o.err = st.Errno()
			}
			d.inflight.Add(-1)
			if o.write {
				if o.err != nil && !d.closed.Load() {
					d.writeErrs.Add(1)
				}
				d.freeW <- o // capacity numWrites: never blocks
			} else {
				d.readyR <- o // capacity numReads: never blocks
			}
		}
		if d.closed.Load() && d.inflight.Load() == 0 {
			return
		}
	}
}

// Read reads one Ethernet frame (without FCS) into b.
func (d *Device) Read(b []byte) (int, error) {
	var o *op
	select {
	case o = <-d.readyR:
	case <-d.loopDone:
		select {
		case o = <-d.readyR: // drain what completed before the loop ended
		default:
			return 0, d.exitErr()
		}
	}
	if o.err != nil {
		if errors.Is(o.err, windows.ERROR_OPERATION_ABORTED) || d.closed.Load() {
			return 0, ErrClosed
		}
		err := o.err
		d.start(o, o.buf)
		return 0, err
	}
	n := copy(b, o.buf[:o.n])
	if err := d.start(o, o.buf); err != nil && !errors.Is(err, ErrClosed) {
		return n, fmt.Errorf("tap: restart read: %w", err)
	}
	return n, nil
}

// Write queues one Ethernet frame (without FCS) to the adapter and returns
// without waiting for the driver to finish with it. It blocks only when
// numWrites frames are already in flight. Failures reported after the
// fact are counted by AsyncWriteErrors.
func (d *Device) Write(b []byte) (int, error) {
	if len(b) > bufSize {
		return 0, fmt.Errorf("tap: frame of %d bytes exceeds %d", len(b), bufSize)
	}
	var o *op
	select {
	case o = <-d.freeW:
	case <-d.loopDone:
		return 0, d.exitErr()
	}
	n := copy(o.buf, b)
	if err := d.start(o, o.buf[:n]); err != nil {
		d.freeW <- o
		return 0, err
	}
	return n, nil
}

// AsyncWriteErrors returns how many queued writes the driver later failed.
func (d *Device) AsyncWriteErrors() uint64 { return d.writeErrs.Load() }

func (d *Device) exitErr() error {
	if d.loopErr != nil && !d.closed.Load() {
		return d.loopErr
	}
	return ErrClosed
}

// MAC returns the adapter's hardware address.
func (d *Device) MAC() (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	if _, err := d.ioctl(ioctlGetMAC, nil, buf); err != nil {
		return nil, fmt.Errorf("tap: get MAC: %w", err)
	}
	return net.HardwareAddr(buf), nil
}

// MTU returns the MTU reported by the TAP driver.
func (d *Device) MTU() (uint32, error) {
	var mtu uint32
	if _, err := d.ioctl(ioctlGetMTU, nil, (*[4]byte)(unsafe.Pointer(&mtu))[:]); err != nil {
		return 0, fmt.Errorf("tap: get MTU: %w", err)
	}
	return mtu, nil
}

func (d *Device) setMediaStatus(up bool) error {
	var status uint32
	if up {
		status = 1
	}
	b := (*[4]byte)(unsafe.Pointer(&status))[:]
	if _, err := d.ioctl(ioctlSetMediaStatus, b, b); err != nil {
		return fmt.Errorf("tap: set media status: %w", err)
	}
	return nil
}

// ioctl issues a synchronous DeviceIoControl. The low bit set on the event
// handle keeps its completion out of the I/O completion port.
func (d *Device) ioctl(code uint32, in, out []byte) (uint32, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)
	ov := windows.Overlapped{HEvent: ev | 1}

	var inPtr, outPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	if len(out) > 0 {
		outPtr = &out[0]
	}
	var n uint32
	err = windows.DeviceIoControl(d.h, code, inPtr, uint32(len(in)), outPtr, uint32(len(out)), &n, &ov)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		if _, err = windows.WaitForSingleObject(ev, windows.INFINITE); err == nil {
			err = windows.GetOverlappedResult(d.h, &ov, &n, false)
		}
	}
	return n, err
}

// Close cancels pending I/O, waits for the driver to release every buffer,
// sets media status to disconnected and closes the device. It is safe to
// call more than once.
func (d *Device) Close() error {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed.Store(true)
		d.mu.Unlock()
		windows.CancelIoEx(d.h, nil)
		windows.PostQueuedCompletionStatus(d.port, 0, 0, nil) // wake loop if idle
		<-d.loopDone
		d.setMediaStatus(false)
		windows.CloseHandle(d.port)
		windows.CloseHandle(d.h)
	})
	return nil
}
