// Package route pins a host route to the remote VTEP via the underlay, so
// that routes installed on the TAP adapter (e.g. a default gateway) cannot
// capture the tunnel's own encapsulated traffic.
package route

import "net/netip"

// Route is a simplified routing table entry.
type Route struct {
	Prefix  netip.Prefix
	NextHop netip.Addr // invalid or unspecified means on-link
	Metric  uint32
}

// Best returns the route for dst by longest-prefix match, breaking ties
// with the lowest metric. Host routes for dst itself are skipped so that a
// stale pinned route (e.g. left by a crash before the gateway changed) does
// not determine the new next hop.
func Best(routes []Route, dst netip.Addr) (Route, bool) {
	var best Route
	found := false
	for _, r := range routes {
		if !r.Prefix.Contains(dst) || r.Prefix.Bits() == dst.BitLen() {
			continue
		}
		if !found ||
			r.Prefix.Bits() > best.Prefix.Bits() ||
			r.Prefix.Bits() == best.Prefix.Bits() && r.Metric < best.Metric {
			best, found = r, true
		}
	}
	return best, found
}
