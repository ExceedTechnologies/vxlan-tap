// Package tunnel forwards Ethernet frames between a TAP device and one or
// more VXLAN peers. With several peers it learns which peer each overlay
// MAC address is behind and floods broadcast, multicast and unknown
// unicast frames to all of them.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"vxlan-tap/internal/vxlan"
)

const (
	minFrame    = 14 // Ethernet header
	maxDatagram = 65535
	statsEvery  = 60 * time.Second

	// Socket buffer size. The OS default is small enough that a burst
	// arriving while a frame is being handed to the TAP overflows it, and
	// TCP inside the tunnel treats that loss as congestion.
	sockBuf = 4 << 20
)

// Config describes one VXLAN tunnel.
type Config struct {
	Local   netip.AddrPort   // underlay address to bind
	Remotes []netip.AddrPort // remote VTEPs; at most one per IP address
	VNI     uint32
	Logger  *slog.Logger
}

// Stats are the tunnel counters.
type Stats struct {
	TxPackets, TxBytes atomic.Uint64 // TAP -> UDP, per datagram sent
	RxPackets, RxBytes atomic.Uint64 // UDP -> TAP
	TxFlooded          atomic.Uint64 // TAP frames sent to every peer

	DropShortFrame atomic.Uint64 // TAP frame shorter than an Ethernet header
	DropWrongPeer  atomic.Uint64 // UDP from an address that is not a peer
	DropBadHeader  atomic.Uint64 // malformed VXLAN header
	DropWrongVNI   atomic.Uint64
	DropShortInner atomic.Uint64 // inner frame shorter than an Ethernet header
	TxErrors       atomic.Uint64
	RxErrors       atomic.Uint64
}

func (s *Stats) logAttrs() []any {
	return []any{
		"tx_packets", s.TxPackets.Load(), "tx_bytes", s.TxBytes.Load(),
		"rx_packets", s.RxPackets.Load(), "rx_bytes", s.RxBytes.Load(),
		"tx_flooded", s.TxFlooded.Load(),
		"drop_short_frame", s.DropShortFrame.Load(),
		"drop_wrong_peer", s.DropWrongPeer.Load(),
		"drop_bad_header", s.DropBadHeader.Load(),
		"drop_wrong_vni", s.DropWrongVNI.Load(),
		"drop_short_inner", s.DropShortInner.Load(),
		"tx_errors", s.TxErrors.Load(), "rx_errors", s.RxErrors.Load(),
	}
}

// Tunnel bridges a TAP device and a UDP socket.
type Tunnel struct {
	cfg   Config
	log   *slog.Logger
	tap   io.ReadWriteCloser
	conn  *net.UDPConn
	Stats Stats

	peers  []netip.AddrPort
	peerOf map[netip.Addr]int // peer IP -> index in peers
	fdb    *fdb               // nil with a single peer
	epoch  time.Time
}

// New binds the UDP socket. The tunnel takes ownership of dev and closes it
// when Run returns (or immediately, if New fails).
func New(cfg Config, dev io.ReadWriteCloser) (*Tunnel, error) {
	if cfg.VNI > vxlan.MaxVNI {
		dev.Close()
		return nil, vxlan.ErrBadVNI
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	t := &Tunnel{cfg: cfg, log: cfg.Logger, tap: dev, epoch: time.Now()}
	if err := t.setPeers(cfg.Remotes); err != nil {
		dev.Close()
		return nil, err
	}
	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(cfg.Local))
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("tunnel: listen on %s: %w", cfg.Local, err)
	}
	if err := conn.SetReadBuffer(sockBuf); err != nil {
		cfg.Logger.Warn("could not enlarge UDP receive buffer", "err", err)
	}
	if err := conn.SetWriteBuffer(sockBuf); err != nil {
		cfg.Logger.Warn("could not enlarge UDP send buffer", "err", err)
	}
	t.conn = conn
	return t, nil
}

func (t *Tunnel) setPeers(remotes []netip.AddrPort) error {
	if len(remotes) == 0 {
		return errors.New("tunnel: no remote VTEPs")
	}
	t.peers = make([]netip.AddrPort, len(remotes))
	t.peerOf = make(map[netip.Addr]int, len(remotes))
	for i, r := range remotes {
		r = netip.AddrPortFrom(r.Addr().Unmap(), r.Port())
		if _, dup := t.peerOf[r.Addr()]; dup {
			return fmt.Errorf("tunnel: remote %s listed twice", r.Addr())
		}
		t.peers[i] = r
		t.peerOf[r.Addr()] = i
	}
	t.fdb = nil
	if len(t.peers) > 1 {
		t.fdb = newFDB()
	}
	return nil
}

// now is a monotonic clock in nanoseconds for FDB ageing.
func (t *Tunnel) now() int64 { return int64(time.Since(t.epoch)) }

// LocalAddr returns the bound UDP address.
func (t *Tunnel) LocalAddr() netip.AddrPort {
	return t.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Run forwards traffic until ctx is cancelled or a fatal error occurs. It
// closes the TAP device and the socket before returning.
func (t *Tunnel) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		runErr  error
	)
	fail := func(err error) {
		errOnce.Do(func() { runErr = err })
		cancel()
	}

	wg.Add(2)
	go func() { defer wg.Done(); t.tapToUDP(ctx, fail) }()
	go func() { defer wg.Done(); t.udpToTAP(ctx, fail) }()

	t.log.Info("tunnel up", "local", t.LocalAddr(), "remotes", t.peers, "vni", t.cfg.VNI)

	ticker := time.NewTicker(statsEvery)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			attrs := t.Stats.logAttrs()
			if t.fdb != nil {
				attrs = append(attrs, "fdb_entries", t.fdb.expire(t.now()))
			}
			t.log.Debug("stats", attrs...)
		}
	}

	t.conn.Close()
	t.tap.Close()
	wg.Wait()
	t.log.Info("tunnel down", t.Stats.logAttrs()...)
	return runErr
}

func (t *Tunnel) tapToUDP(ctx context.Context, fail func(error)) {
	buf := make([]byte, vxlan.HeaderLen+maxDatagram)
	vxlan.Encode(buf, t.cfg.VNI) // header is constant; frames go after it
	frameBuf := buf[vxlan.HeaderLen:]
	for {
		n, err := t.tap.Read(frameBuf)
		if err != nil {
			if ctx.Err() == nil {
				fail(fmt.Errorf("tunnel: TAP read: %w", err))
			}
			return
		}
		if n < minFrame {
			t.Stats.DropShortFrame.Add(1)
			continue
		}
		pkt := buf[:vxlan.HeaderLen+n]
		if len(t.peers) == 1 {
			t.send(ctx, pkt, 0)
			continue
		}
		frame := frameBuf[:n]
		if frame[0]&1 == 0 { // unicast destination
			if p, ok := t.fdb.lookup(mac(frame[0:6]), t.now()); ok {
				t.send(ctx, pkt, p)
				continue
			}
		}
		t.Stats.TxFlooded.Add(1)
		for p := range t.peers {
			t.send(ctx, pkt, p)
		}
	}
}

// send transmits one encapsulated frame to peer p. Errors are transient
// (no route, ICMP unreachable) and only counted.
func (t *Tunnel) send(ctx context.Context, pkt []byte, p int) {
	if _, err := t.conn.WriteToUDPAddrPort(pkt, t.peers[p]); err != nil {
		if ctx.Err() == nil {
			t.Stats.TxErrors.Add(1)
			t.log.Debug("UDP send failed", "peer", t.peers[p], "err", err)
		}
		return
	}
	t.Stats.TxPackets.Add(1)
	t.Stats.TxBytes.Add(uint64(len(pkt) - vxlan.HeaderLen))
}

func (t *Tunnel) udpToTAP(ctx context.Context, fail func(error)) {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := t.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Go disables the ICMP unreachable reports (WSAECONNRESET,
			// WSAENETRESET) that Windows would otherwise deliver here, so
			// what remains, such as the underlay address going away,
			// repeats on every call. Retrying would spin; fail instead.
			fail(fmt.Errorf("tunnel: UDP receive: %w", err))
			return
		}
		peer, ok := t.peerOf[from.Addr().Unmap()]
		if !ok {
			t.Stats.DropWrongPeer.Add(1)
			continue
		}
		vni, frame, err := vxlan.Decode(buf[:n])
		if err != nil {
			t.Stats.DropBadHeader.Add(1)
			continue
		}
		if vni != t.cfg.VNI {
			t.Stats.DropWrongVNI.Add(1)
			continue
		}
		if len(frame) < minFrame {
			t.Stats.DropShortInner.Add(1)
			continue
		}
		if t.fdb != nil && frame[6]&1 == 0 { // learn unicast source MACs
			t.fdb.learn(mac(frame[6:12]), peer, t.now())
		}
		if _, err := t.tap.Write(frame); err != nil {
			if ctx.Err() != nil {
				return
			}
			// A dead device also fails Read, which ends the tunnel; a
			// single rejected frame should not.
			t.Stats.RxErrors.Add(1)
			t.log.Debug("TAP write failed", "err", err)
			continue
		}
		t.Stats.RxPackets.Add(1)
		t.Stats.RxBytes.Add(uint64(len(frame)))
	}
}
