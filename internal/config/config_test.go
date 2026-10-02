package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte("local_ip: 10.0.0.1\nremote_ip: 10.0.0.2\nvni: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 4789 || c.Level != slog.LevelInfo || c.VNIValue() != 100 || !c.PinRoute() {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Local.String() != "10.0.0.1" || c.Remote.String() != "10.0.0.2" {
		t.Fatalf("bad addrs: %v %v", c.Local, c.Remote)
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(`
local_ip: "fd00::1"
remote_ip: "fd00::2"
vni: 0
port: 8472
tap: "Ethernet 3"
log_level: debug
log_file: C:\vxlan.log
pin_remote_route: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8472 || c.Level != slog.LevelDebug || c.TAP != "Ethernet 3" || c.VNIValue() != 0 || c.PinRoute() {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestParseErrors(t *testing.T) {
	base := "local_ip: 10.0.0.1\nremote_ip: 10.0.0.2\nvni: 100\n"
	tests := []struct {
		name, yaml, want string
	}{
		{"missing local", "remote_ip: 10.0.0.2\nvni: 1\n", "local_ip is required"},
		{"bad remote", "local_ip: 10.0.0.1\nremote_ip: nope\nvni: 1\n", "remote_ip"},
		{"mixed family", "local_ip: 10.0.0.1\nremote_ip: fd00::2\nvni: 1\n", "same address family"},
		{"same ip", "local_ip: 10.0.0.1\nremote_ip: 10.0.0.1\nvni: 1\n", "must differ"},
		{"missing vni", "local_ip: 10.0.0.1\nremote_ip: 10.0.0.2\n", "vni is required"},
		{"vni too big", "local_ip: 10.0.0.1\nremote_ip: 10.0.0.2\nvni: 16777216\n", "out of range"},
		{"negative vni", "local_ip: 10.0.0.1\nremote_ip: 10.0.0.2\nvni: -1\n", "out of range"},
		{"bad port", base + "port: 70000\n", "port"},
		{"bad level", base + "log_level: loud\n", "log_level"},
		{"unknown field", base + "bogus: 1\n", "bogus"},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.yaml))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want error containing %q", tt.name, err, tt.want)
		}
	}
}
