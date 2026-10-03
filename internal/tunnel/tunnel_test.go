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
	a, err = New(Config{Local: loopback(0), Remotes: []netip.AddrPort{loopback(1)}, VNI: vniA, Logger: quiet}, tapA)
	if err != nil {
		t.Fatal(err)
	}
	b, err = New(Config{Local: loopback(0), Remotes: []netip.AddrPort{a.LocalAddr()}, VNI: vniB, Logger: quiet}, tapB)
	if err != nil {
		t.Fatal(err)
	}
	a.setPeers([]netip.AddrPort{b.LocalAddr()})
	runAll(t, a, b)
	return a, b, tapA, tapB
}

func runAll(t testing.TB, tuns ...*Tunnel) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, tun := range tuns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tun.Run(ctx); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func frame(payload string) []byte {
	return frameTo(broadcast, macA, payload)
}

var (
	broadcast = mac{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	macA      = mac{0x02, 0, 0, 0, 0, 0x0a}
	macB      = mac{0x02, 0, 0, 0, 0, 0x0b}
	macC      = mac{0x02, 0, 0, 0, 0, 0x0c}
	macX      = mac{0x02, 0, 0, 0, 0, 0xee} // never seen
)

func frameTo(dst, src mac, payload string) []byte {
	f := append(dst[:], src[:]...)
	f = append(f, 0x08, 0x00) // IPv4
	return append(f, payload...)
}

func expectNone(t *testing.T, ch chan []byte) {
	t.Helper()
	select {
	case p := <-ch:
		t.Fatalf("unexpected frame % x", p)
	case <-time.After(100 * time.Millisecond):
	}
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

// TestMultiplePeers runs a hub (A) with two spokes (B, C) on distinct
// loopback IPs, since peers are told apart by IP address.
func TestMultiplePeers(t *testing.T) {
	ips := []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}
	var (
		tuns [3]*Tunnel
		taps [3]*fakeTAP
	)
	for i, ip := range ips {
		taps[i] = newFakeTAP()
		local := netip.AddrPortFrom(netip.MustParseAddr(ip), 0)
		tun, err := New(Config{Local: local, Remotes: []netip.AddrPort{loopback(1)}, VNI: 7, Logger: quiet}, taps[i])
		if err != nil {
			for _, prev := range tuns[:i] {
				prev.conn.Close()
				prev.tap.Close()
			}
			t.Skipf("cannot bind %s: %v", ip, err)
		}
		tuns[i] = tun
	}
	a, b, c := tuns[0], tuns[1], tuns[2]
	if err := a.setPeers([]netip.AddrPort{b.LocalAddr(), c.LocalAddr()}); err != nil {
		t.Fatal(err)
	}
	b.setPeers([]netip.AddrPort{a.LocalAddr()})
	c.setPeers([]netip.AddrPort{a.LocalAddr()})
	runAll(t, a, b, c)
	tapA, tapB, tapC := taps[0], taps[1], taps[2]

	// Broadcast from A is flooded to both spokes.
	bc := frameTo(broadcast, macA, "hello all")
	tapA.in <- bc
	if got := recv(t, tapB.out); !bytes.Equal(got, bc) {
		t.Fatalf("B got % x", got)
	}
	if got := recv(t, tapC.out); !bytes.Equal(got, bc) {
		t.Fatalf("C got % x", got)
	}

	// Unknown unicast is flooded too.
	tapA.in <- frameTo(macX, macA, "who")
	recv(t, tapB.out)
	recv(t, tapC.out)

	// A learns macB and macC from their replies...
	tapB.in <- frameTo(macA, macB, "from B")
	recv(t, tapA.out)
	tapC.in <- frameTo(macA, macC, "from C")
	recv(t, tapA.out)

	// ...and then sends unicast only to the right peer.
	toB := frameTo(macB, macA, "just B")
	tapA.in <- toB
	if got := recv(t, tapB.out); !bytes.Equal(got, toB) {
		t.Fatalf("B got % x", got)
	}
	expectNone(t, tapC.out)

	toC := frameTo(macC, macA, "just C")
	tapA.in <- toC
	if got := recv(t, tapC.out); !bytes.Equal(got, toC) {
		t.Fatalf("C got % x", got)
	}
	expectNone(t, tapB.out)

	if n := a.Stats.TxFlooded.Load(); n != 2 {
		t.Fatalf("tx_flooded = %d, want 2", n)
	}
}

func TestFDB(t *testing.T) {
	f := newFDB()
	sec := int64(time.Second)
	if _, ok := f.lookup(macB, 0); ok {
		t.Fatal("empty FDB hit")
	}
	f.learn(macB, 1, 0)
	if p, ok := f.lookup(macB, 10*sec); !ok || p != 1 {
		t.Fatalf("lookup = %d, %v", p, ok)
	}
	f.learn(macB, 0, 20*sec) // moved to another peer
	if p, _ := f.lookup(macB, 20*sec); p != 0 {
		t.Fatalf("after move: peer %d", p)
	}
	age := int64(fdbAgeing)
	if _, ok := f.lookup(macB, 20*sec+age); ok {
		t.Fatal("entry did not age out")
	}
	f.learn(macC, 1, 20*sec+age)
	if n := f.expire(20*sec + age); n != 1 {
		t.Fatalf("expire left %d entries, want 1", n)
	}
}

func TestDuplicatePeerRejected(t *testing.T) {
	r := loopback(4789)
	if _, err := New(Config{Local: loopback(0), Remotes: []netip.AddrPort{r, r}, Logger: quiet}, newFakeTAP()); err == nil {
		t.Fatal("duplicate remote accepted")
	}
}

func TestTAPReadErrorIsFatal(t *testing.T) {
	tap := newFakeTAP()
	tun, err := New(Config{Local: loopback(0), Remotes: []netip.AddrPort{loopback(9)}, VNI: 1, Logger: quiet}, tap)
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
