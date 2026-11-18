// Package netpolicy holds the address and port classes tandem never relays.
package netpolicy

import "net/netip"

// Bypass lists IPv4 destinations that always go direct: loopback, RFC1918,
// link-local, CGNAT (which is where the tailnet lives: 100.64.0.0/10),
// multicast, reserved and "this network". The relay also refuses these as
// destinations, so it cannot be used to reach archserver's own LAN or
// loopback services.
var Bypass = mustPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"224.0.0.0/4",
	"240.0.0.0/4",
)

// BypassPorts are TCP destination ports that always go direct.
// 47984/47989/48010 are Moonlight/Sunshine's TCP control ports.
var BypassPorts = []uint16{47984, 47989, 48010}

// TailscaleMark and TailscaleMask match the fwmark tailscaled puts on its own
// sockets (see its `ip rule` entries 5210-5250). Those connections are tailnet
// plumbing and are never redirected.
const (
	TailscaleMark uint32 = 0x80000
	TailscaleMask uint32 = 0xff0000
)

// OwnMark is set (SO_MARK) on every socket the tandem client dials itself,
// so its outbound connections are not redirected back into it.
const (
	OwnMark uint32 = 0x7a4e0000
	OwnMask uint32 = 0xffff0000
)

// IsPublic reports whether addr is a destination tandem may relay.
func IsPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.Is4() {
		// v1 relays IPv4 only; IPv6 is never redirected on the client.
		return false
	}
	for _, p := range Bypass {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// IsBypassPort reports whether port always goes direct.
func IsBypassPort(port uint16) bool {
	for _, p := range BypassPorts {
		if p == port {
			return true
		}
	}
	return false
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}
