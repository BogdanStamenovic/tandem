// Package config is tandem's on-disk configuration: /etc/tandem/tandem.json
// plus the pre-shared key in /etc/tandem/psk.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

const (
	DefaultDir       = "/etc/tandem"
	DefaultStateDir  = "/var/lib/tandem"
	DefaultRunDir    = "/run/tandem"
	DefaultRelayPort = 7411
	DefaultLocalPort = 7410
)

type Role string

const (
	RoleClient Role = "client"
	RoleRelay  Role = "relay"
)

type Config struct {
	Role Role `json:"role"`

	// Client: where the relay listens, e.g. "192.168.1.139:7411".
	Relay string `json:"relay,omitempty"`
	// Client: local port the redirect points at (bound on 127.0.0.1).
	LocalPort uint16 `json:"local_port,omitempty"`
	// Client: path when nothing has been measured, "relay" or "direct".
	Default string `json:"default,omitempty"`
	// Client: extra TCP ports that always go direct.
	BypassPorts []uint16 `json:"bypass_ports,omitempty"`
	// Client: extra IPv4 prefixes that always go direct.
	BypassNets []string `json:"bypass_nets,omitempty"`

	// Relay: listen address. Should be the LAN address.
	Listen string `json:"listen,omitempty"`
	// Relay: source prefixes allowed to connect (in addition to the PSK).
	Allow []string `json:"allow,omitempty"`

	PSKFile  string `json:"psk_file,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
	RunDir   string `json:"run_dir,omitempty"`
}

// Path returns the config file path, honouring $TANDEM_CONFIG.
func Path() string {
	if p := os.Getenv("TANDEM_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(DefaultDir, "tandem.json")
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Fill(filepath.Dir(path))
	return &c, c.Validate()
}

// Fill sets defaults for unset fields; dir is the config directory.
func (c *Config) Fill(dir string) {
	if c.Role == RoleClient {
		if c.LocalPort == 0 {
			c.LocalPort = DefaultLocalPort
		}
		if c.Default == "" {
			c.Default = "relay"
		}
	}
	if c.PSKFile == "" {
		c.PSKFile = filepath.Join(dir, "psk")
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if c.RunDir == "" {
		c.RunDir = DefaultRunDir
	}
}

func (c *Config) Validate() error {
	switch c.Role {
	case RoleClient:
		if _, err := netip.ParseAddrPort(c.Relay); err != nil {
			return fmt.Errorf("config: relay %q: need IP:port (%v)", c.Relay, err)
		}
		if c.Default != "relay" && c.Default != "direct" {
			return fmt.Errorf("config: default must be relay or direct, not %q", c.Default)
		}
		for _, n := range c.BypassNets {
			if _, err := netip.ParsePrefix(n); err != nil {
				return fmt.Errorf("config: bypass_nets: %v", err)
			}
		}
	case RoleRelay:
		if _, err := netip.ParseAddrPort(c.Listen); err != nil {
			return fmt.Errorf("config: listen %q: need IP:port (%v)", c.Listen, err)
		}
		if len(c.Allow) == 0 {
			return errors.New("config: relay needs at least one allow prefix")
		}
		for _, n := range c.Allow {
			if _, err := netip.ParsePrefix(n); err != nil {
				return fmt.Errorf("config: allow: %v", err)
			}
		}
	default:
		return fmt.Errorf("config: role must be client or relay, not %q", c.Role)
	}
	return nil
}

// Save writes c to path (0644; the key lives in its own 0600 file).
func (c *Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
