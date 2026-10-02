// Package tunnel forwards Ethernet frames between a TAP device and a
// point-to-point VXLAN peer.
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

// Config describes one point-to-point VXLAN tunnel.
type Config struct {
	Local  netip.AddrPort // underlay address to bind
	Remote netip.AddrPort // remote VTEP
	VNI    uint32
	Logger *slog.Logger
}

// Stats are the tunnel counters.
type Stats struct {
	TxPackets, TxBytes atomic.Uint64 // TAP -> UDP
	RxPackets, RxBytes atomic.Uint64 // UDP -> TAP

	DropShortFrame atomic.Uint64 // TAP frame shorter than an Ethernet header
	DropWrongPeer  atomic.Uint64 // UDP from someone other than Remote
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
	return &Tunnel{cfg: cfg, log: cfg.Logger, tap: dev, conn: conn}, nil
}

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

	t.log.Info("tunnel up", "local", t.LocalAddr(), "remote", t.cfg.Remote, "vni", t.cfg.VNI)

	ticker := time.NewTicker(statsEvery)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			t.log.Debug("stats", t.Stats.logAttrs()...)
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
		if _, err := t.conn.WriteToUDPAddrPort(buf[:vxlan.HeaderLen+n], t.cfg.Remote); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Transient (e.g. no route, ICMP unreachable); keep going.
			t.Stats.TxErrors.Add(1)
			t.log.Debug("UDP send failed", "err", err)
			continue
		}
		t.Stats.TxPackets.Add(1)
		t.Stats.TxBytes.Add(uint64(n))
	}
}

func (t *Tunnel) udpToTAP(ctx context.Context, fail func(error)) {
	buf := make([]byte, maxDatagram)
	remote := t.cfg.Remote.Addr()
	for {
		n, from, err := t.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// On Windows a previous send to an unreachable port surfaces
			// here as WSAECONNRESET; it is not fatal.
			t.Stats.RxErrors.Add(1)
			t.log.Debug("UDP receive failed", "err", err)
			continue
		}
		if from.Addr().Unmap() != remote {
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
