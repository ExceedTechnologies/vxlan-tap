package route

import (
	"errors"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Not wrapped by x/sys/windows.
var (
	iphlpapi                     = windows.NewLazySystemDLL("iphlpapi.dll")
	procInitializeIpForwardEntry = iphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateIpForwardEntry2    = iphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2    = iphlpapi.NewProc("DeleteIpForwardEntry2")
)

// Pinned is a host route installed by Pin.
type Pinned struct {
	row windows.MibIpForwardRow2

	Dest           netip.Prefix
	NextHop        netip.Addr // invalid means on-link
	InterfaceIndex uint32
	Existed        bool // the identical route was already present
}

// Pin installs a host route for remote via whatever route the interface
// owning local currently uses to reach it. Routes on other interfaces,
// including the TAP adapter, are not considered.
func Pin(local, remote netip.Addr) (*Pinned, error) {
	luid, err := interfaceFor(local)
	if err != nil {
		return nil, err
	}

	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(family(remote), &table); err != nil {
		return nil, fmt.Errorf("route: read routing table: %w", err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))

	var (
		cands []Route
		raws  []*windows.MibIpForwardRow2
	)
	rows := table.Rows()
	for i := range rows {
		row := &rows[i]
		if row.InterfaceLuid != luid {
			continue
		}
		dst, ok := addrFromSockaddr(&row.DestinationPrefix.Prefix)
		if !ok {
			continue
		}
		prefix, err := dst.Prefix(int(row.DestinationPrefix.PrefixLength))
		if err != nil {
			continue
		}
		nh, _ := addrFromSockaddr(&row.NextHop)
		if nh.IsUnspecified() {
			nh = netip.Addr{}
		}
		cands = append(cands, Route{Prefix: prefix, NextHop: nh, Metric: row.Metric})
		raws = append(raws, row)
	}

	var best Route
	var bestRaw *windows.MibIpForwardRow2
	if b, ok := Best(cands, remote); ok {
		best = b
		for i, c := range cands {
			if c == b {
				bestRaw = raws[i]
				break
			}
		}
	}
	if bestRaw == nil {
		return nil, fmt.Errorf("route: no route to %s on the interface that owns %s", remote, local)
	}

	p := &Pinned{
		Dest:           netip.PrefixFrom(remote, remote.BitLen()),
		NextHop:        best.NextHop,
		InterfaceIndex: bestRaw.InterfaceIndex,
	}
	procInitializeIpForwardEntry.Call(uintptr(unsafe.Pointer(&p.row)))
	p.row.InterfaceLuid = luid
	p.row.DestinationPrefix.Prefix = sockaddrFromAddr(remote)
	p.row.DestinationPrefix.PrefixLength = uint8(remote.BitLen())
	p.row.NextHop = bestRaw.NextHop // keeps the IPv6 scope of link-local gateways
	p.row.Metric = 0
	p.row.Protocol = windows.MIB_IPPROTO_NETMGMT
	p.row.Origin = windows.NlroManual

	if r, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&p.row))); r != 0 {
		err := windows.Errno(r)
		if !errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
			via := "on-link"
			if p.NextHop.IsValid() {
				via = p.NextHop.String()
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return nil, fmt.Errorf("route: add host route %s via %s: access denied; run as Administrator", p.Dest, via)
			}
			return nil, fmt.Errorf("route: add host route %s via %s: %w", p.Dest, via, err)
		}
		// Same prefix, next hop and interface: left behind by an earlier
		// run that did not shut down cleanly, or added by an administrator.
		// It already does the job; Existed tells the caller not to remove it.
		p.Existed = true
	}
	return p, nil
}

// Remove deletes the pinned route. A route that is already gone is not an
// error.
func (p *Pinned) Remove() error {
	r, _, _ := procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&p.row)))
	if r != 0 && !errors.Is(windows.Errno(r), windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("route: delete host route %s: %w", p.Dest, windows.Errno(r))
	}
	return nil
}

// interfaceFor returns the LUID of the interface that has addr assigned.
func interfaceFor(addr netip.Addr) (uint64, error) {
	var table *windows.MibUnicastIpAddressTable
	if err := windows.GetUnicastIpAddressTable(family(addr), &table); err != nil {
		return 0, fmt.Errorf("route: read address table: %w", err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)
	for i := range rows {
		a, ok := addrFromSockaddr((*windows.RawSockaddrInet)(unsafe.Pointer(&rows[i].Address)))
		if ok && a == addr {
			return rows[i].InterfaceLuid, nil
		}
	}
	return 0, fmt.Errorf("route: local_ip %s is not assigned to any interface", addr)
}

func family(a netip.Addr) uint16 {
	if a.Is4() {
		return windows.AF_INET
	}
	return windows.AF_INET6
}

func addrFromSockaddr(sa *windows.RawSockaddrInet) (netip.Addr, bool) {
	switch sa.Family {
	case windows.AF_INET:
		return netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(sa)).Addr), true
	case windows.AF_INET6:
		return netip.AddrFrom16((*windows.RawSockaddrInet6)(unsafe.Pointer(sa)).Addr), true
	}
	return netip.Addr{}, false
}

func sockaddrFromAddr(a netip.Addr) windows.RawSockaddrInet {
	var sa windows.RawSockaddrInet
	if a.Is4() {
		s := (*windows.RawSockaddrInet4)(unsafe.Pointer(&sa))
		s.Family = windows.AF_INET
		s.Addr = a.As4()
	} else {
		s := (*windows.RawSockaddrInet6)(unsafe.Pointer(&sa))
		s.Family = windows.AF_INET6
		s.Addr = a.As16()
	}
	return sa
}
