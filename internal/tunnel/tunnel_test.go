package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"vxlan-tap/internal/vxlan"
)

// fakeTAP is an in-memory TAP: frames pushed to in are returned by Read,
// frames written are delivered on out.
type fakeTAP struct {
	in, out   chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func newFakeTAP() *fakeTAP {
	return &fakeTAP{in: make(chan []byte, 16), out: make(chan []byte, 16), done: make(chan struct{})}
}

func (f *fakeTAP) Read(b []byte) (int, error) {
	select {
	case p := <-f.in:
		return copy(b, p), nil
	case <-f.done:
		return 0, io.ErrClosedPipe
	}
}

func (f *fakeTAP) Write(b []byte) (int, error) {
	select {
	case f.out <- bytes.Clone(b):
		return len(b), nil
	case <-f.done:
		return 0, io.ErrClosedPipe
	}
}

func (f *fakeTAP) Close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func loopback(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
}

// startPair runs two tunnels on loopback pointed at each other.
func startPair(t testing.TB, vniA, vniB uint32) (a, b *Tunnel, tapA, tapB *fakeTAP) {
	t.Helper()
	tapA, tapB = newFakeTAP(), newFakeTAP()
	tapA.in = make(chan []byte, 1024)
	tapB.out = make(chan []byte, 1024)
	var err error
	a, err = New(Config{Local: loopback(0), Remote: loopback(1), VNI: vniA, Logger: quiet}, tapA)
	if err != nil {
		t.Fatal(err)
	}
	b, err = New(Config{Local: loopback(0), Remote: a.LocalAddr(), VNI: vniB, Logger: quiet}, tapB)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Remote = b.LocalAddr()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, tun := range []*Tunnel{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tun.Run(ctx); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	return a, b, tapA, tapB
}

func frame(payload string) []byte {
	f := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // dst
		0x02, 0x00, 0x00, 0x00, 0x00, 0x01, // src
		0x08, 0x00, // IPv4
	}
	return append(f, payload...)
}

func recv(t *testing.T, ch chan []byte) []byte {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for frame")
		return nil
	}
}

func TestBidirectional(t *testing.T) {
	a, b, tapA, tapB := startPair(t, 100, 100)

	f1 := frame("a to b")
	tapA.in <- f1
	if got := recv(t, tapB.out); !bytes.Equal(got, f1) {
		t.Fatalf("B got % x, want % x", got, f1)
	}

	f2 := frame("b to a")
	tapB.in <- f2
	if got := recv(t, tapA.out); !bytes.Equal(got, f2) {
		t.Fatalf("A got % x, want % x", got, f2)
	}

	if a.Stats.TxPackets.Load() != 1 || b.Stats.RxPackets.Load() != 1 {
		t.Fatalf("stats: a.tx=%d b.rx=%d", a.Stats.TxPackets.Load(), b.Stats.RxPackets.Load())
	}
}

func TestWrongVNIDropped(t *testing.T) {
	_, b, tapA, tapB := startPair(t, 100, 200)
	tapA.in <- frame("x")
	waitFor(t, func() bool { return b.Stats.DropWrongVNI.Load() == 1 })
	select {
	case p := <-tapB.out:
		t.Fatalf("unexpected frame % x", p)
	default:
	}
}

func TestDropsMalformedAndForeign(t *testing.T) {
	_, b, _, tapB := startPair(t, 100, 100)

	// Malformed packets sent from the peer IP (127.0.0.1).
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(b.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Write([]byte{1, 2, 3}) // shorter than header
	hdr := make([]byte, vxlan.HeaderLen)
	vxlan.Encode(hdr, 100)
	c.Write(append(hdr, 1, 2, 3)) // inner frame too short
	waitFor(t, func() bool {
		return b.Stats.DropBadHeader.Load() == 1 && b.Stats.DropShortInner.Load() == 1
	})

	// A packet from a non-peer IP. 127.0.0.2 is loopback on Windows and
	// Linux; skip if the platform will not bind it.
	other, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.2")})
	if err != nil {
		t.Logf("skipping foreign-peer check: %v", err)
	} else {
		defer other.Close()
		other.WriteToUDPAddrPort(append(hdr, frame("spoof")...), b.LocalAddr())
		waitFor(t, func() bool { return b.Stats.DropWrongPeer.Load() == 1 })
	}

	select {
	case p := <-tapB.out:
		t.Fatalf("unexpected frame % x", p)
	default:
	}
}

func TestTAPReadErrorIsFatal(t *testing.T) {
	tap := newFakeTAP()
	tun, err := New(Config{Local: loopback(0), Remote: loopback(9), VNI: 1, Logger: quiet}, tap)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- tun.Run(context.Background()) }()
	tap.Close() // simulates the adapter disappearing
	select {
	case err := <-errc:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("got %v, want ErrClosedPipe", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// BenchmarkForward measures one-way forwarding through two tunnels over
// loopback UDP: fake TAP A -> UDP -> fake TAP B.
func BenchmarkForward(b *testing.B) {
	_, _, tapA, tapB := startPair(b, 1, 1)
	f := frame(string(make([]byte, 1400)))
	b.SetBytes(int64(len(f)))
	b.ResetTimer()
	go func() {
		for i := 0; i < b.N; i++ {
			tapA.in <- f
		}
	}()
	for i := 0; i < b.N; i++ {
		select {
		case <-tapB.out:
		case <-time.After(time.Second):
			// Loopback UDP can drop under load; count what arrived.
			b.ReportMetric(float64(i)/float64(b.N), "delivered")
			return
		}
	}
}
