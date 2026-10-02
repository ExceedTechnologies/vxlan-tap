package route

import (
	"net/netip"
	"testing"
)

func r(prefix, nh string, metric uint32) Route {
	var a netip.Addr
	if nh != "" {
		a = netip.MustParseAddr(nh)
	}
	return Route{Prefix: netip.MustParsePrefix(prefix), NextHop: a, Metric: metric}
}

func TestBest(t *testing.T) {
	dst := netip.MustParseAddr("203.0.113.7")
	tests := []struct {
		name   string
		routes []Route
		want   string // next hop, "" = no route, "on-link" = no next hop
	}{
		{"default only", []Route{r("0.0.0.0/0", "192.168.1.1", 25)}, "192.168.1.1"},
		{"longest prefix wins", []Route{
			r("0.0.0.0/0", "192.168.1.1", 1),
			r("203.0.113.0/24", "192.168.1.254", 100),
		}, "192.168.1.254"},
		{"metric breaks tie", []Route{
			r("0.0.0.0/0", "192.168.1.1", 50),
			r("0.0.0.0/0", "192.168.1.2", 10),
		}, "192.168.1.2"},
		{"on-link subnet", []Route{
			r("0.0.0.0/0", "192.168.1.1", 1),
			r("203.0.113.0/24", "", 1),
		}, "on-link"},
		{"stale host route ignored", []Route{
			r("0.0.0.0/0", "192.168.1.1", 25),
			r("203.0.113.7/32", "10.9.9.9", 0),
		}, "192.168.1.1"},
		{"non-matching", []Route{r("10.0.0.0/8", "192.168.1.1", 1)}, ""},
	}
	for _, tt := range tests {
		got, ok := Best(tt.routes, dst)
		var s string
		switch {
		case !ok:
		case !got.NextHop.IsValid():
			s = "on-link"
		default:
			s = got.NextHop.String()
		}
		if s != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, s, tt.want)
		}
	}
}

func TestBestIPv6(t *testing.T) {
	dst := netip.MustParseAddr("2001:db8::7")
	got, ok := Best([]Route{
		r("::/0", "fe80::1", 1),
		r("2001:db8::/32", "fe80::2", 1),
	}, dst)
	if !ok || got.NextHop.String() != "fe80::2" {
		t.Fatalf("got %+v, %v", got, ok)
	}
}
