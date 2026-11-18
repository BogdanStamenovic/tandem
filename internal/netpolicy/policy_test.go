package netpolicy

import (
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"1.1.1.1": true, "78.46.209.220": true, "203.0.113.10": true,
		"192.168.1.139": false, "10.0.0.1": false, "172.16.5.4": false,
		"127.0.0.1": false, "100.72.2.62": false, // tailnet (CGNAT)
		"169.254.1.1": false, "224.0.0.251": false, "255.255.255.255": false,
		"2a01:4f8::1":    false, // IPv6 is never relayed in v1
		"::ffff:1.1.1.1": true,
	}
	for s, want := range cases {
		if got := IsPublic(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", s, got, want)
		}
	}
}
