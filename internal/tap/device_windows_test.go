package tap

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestDevice exercises a real TAP-Windows6 adapter. It needs an elevated
// prompt and an adapter not used by anything else:
//
//	set VXLAN_TAP_TEST=1   (optionally VXLAN_TAP_ADAPTER=<name or GUID>)
//	go test ./internal/tap -run TestDevice -v
func TestDevice(t *testing.T) {
	if os.Getenv("VXLAN_TAP_TEST") == "" {
		t.Skip("set VXLAN_TAP_TEST=1 to run against a real TAP adapter")
	}
	a, err := Find(os.Getenv("VXLAN_TAP_ADAPTER"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Open(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("opened %s %s", a.Name, a.GUID)

	mac, err := d.MAC()
	if err != nil {
		t.Fatal(err)
	}
	mtu, err := d.MTU()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MAC %s, MTU %d", mac, mtu)

	// Broadcast frames with the IEEE local experimental EtherType, which
	// the Windows stack ignores. More than numWrites exercises slot reuse.
	frame := make([]byte, 60)
	copy(frame, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02, 0, 0, 0, 0, 1, 0x88, 0xb5})
	for i := 0; i < 4*numWrites; i++ {
		if _, err := d.Write(frame); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Close must cancel the pending reads and return promptly, and a
	// blocked Read must be released with ErrClosed.
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, bufSize)
		for {
			if _, err := d.Read(buf); err != nil {
				readErr <- err
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	closed := make(chan struct{})
	go func() { d.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Read after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read not released by Close")
	}
	if _, err := d.Write(frame); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
	if n := d.AsyncWriteErrors(); n != 0 {
		t.Fatalf("%d async write errors", n)
	}
}

// BenchmarkDeviceWrite measures frames/s the driver accepts from Write.
// Same requirements as TestDevice.
func BenchmarkDeviceWrite(b *testing.B) {
	if os.Getenv("VXLAN_TAP_TEST") == "" {
		b.Skip("set VXLAN_TAP_TEST=1 to run against a real TAP adapter")
	}
	a, err := Find(os.Getenv("VXLAN_TAP_ADAPTER"))
	if err != nil {
		b.Fatal(err)
	}
	d, err := Open(a)
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	frame := make([]byte, 1464)
	copy(frame, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02, 0, 0, 0, 0, 1, 0x88, 0xb5})
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Write(frame); err != nil {
			b.Fatal(err)
		}
	}
}
