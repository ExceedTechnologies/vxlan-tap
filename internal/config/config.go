// Package config loads and validates the vxlan-tap YAML configuration.
package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/netip"
	"os"

	"gopkg.in/yaml.v3"

	"vxlan-tap/internal/vxlan"
)

type Config struct {
	LocalIP  string `yaml:"local_ip"`
	RemoteIP string `yaml:"remote_ip"`
	VNI      *int64 `yaml:"vni"`
	Port     int    `yaml:"port"`
	TAP      string `yaml:"tap"`
	LogLevel string `yaml:"log_level"`
	LogFile  string `yaml:"log_file"`

	// PinRemoteRoute adds a host route to remote_ip via the current
	// underlay gateway while the tunnel is up. Defaults to true.
	PinRemoteRoute *bool `yaml:"pin_remote_route"`

	// Parsed values, filled in by Validate.
	Local  netip.Addr `yaml:"-"`
	Remote netip.Addr `yaml:"-"`
	Level  slog.Level `yaml:"-"`
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes YAML, applies defaults and validates.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// PinRoute reports whether to pin the remote route. Only valid after Validate.
func (c *Config) PinRoute() bool { return *c.PinRemoteRoute }

// VNIValue returns the configured VNI. Only valid after Validate.
func (c *Config) VNIValue() uint32 { return uint32(*c.VNI) }

func (c *Config) Validate() error {
	var err error
	if c.Local, err = parseIP("local_ip", c.LocalIP); err != nil {
		return err
	}
	if c.Remote, err = parseIP("remote_ip", c.RemoteIP); err != nil {
		return err
	}
	if c.Local.Is4() != c.Remote.Is4() {
		return fmt.Errorf("config: local_ip and remote_ip must be the same address family")
	}
	if c.Local == c.Remote {
		return fmt.Errorf("config: local_ip and remote_ip must differ")
	}
	if c.VNI == nil {
		return fmt.Errorf("config: vni is required")
	}
	if *c.VNI < 0 || *c.VNI > vxlan.MaxVNI {
		return fmt.Errorf("config: vni %d out of range 0..%d", *c.VNI, vxlan.MaxVNI)
	}
	if c.Port == 0 {
		c.Port = vxlan.DefaultPort
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: port %d out of range 1..65535", c.Port)
	}
	if c.PinRemoteRoute == nil {
		t := true
		c.PinRemoteRoute = &t
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if err := c.Level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return fmt.Errorf("config: log_level %q: must be debug, info, warn or error", c.LogLevel)
	}
	return nil
}

func parseIP(field, s string) (netip.Addr, error) {
	if s == "" {
		return netip.Addr{}, fmt.Errorf("config: %s is required", field)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("config: %s: %w", field, err)
	}
	if a.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("config: %s: zones are not supported", field)
	}
	return a.Unmap(), nil
}
