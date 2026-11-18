package nftrules

import (
	"net/netip"
	"strings"
	"testing"
)

func TestTextOrder(t *testing.T) {
	s := Spec{Port: 7410, Uplink: "wlp0s20f3", Bypass: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")},
		BypassPorts: []uint16{47984}, SkipMarks: [][2]uint32{{0x80000, 0xff0000}}}
	txt := Text(s)
	order := []string{"flags owner", "ct state invalid return", "meta l4proto != tcp", `oifname != "wlp0s20f3"`,
		"meta mark & 0x00ff0000 == 0x00080000", "ip daddr 192.168.0.0/16 return", "tcp dport 47984", "redirect to :7410"}
	last := -1
	for _, o := range order {
		i := strings.Index(txt, o)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in:\n%s", o, txt)
		}
		last = i
	}
	if strings.Contains(Text(Spec{Off: true}), "redirect to") {
		t.Fatal("paused spec must not redirect")
	}
	// build() must end with the redirect and have one rule per bypass entry.
	rules := build(s)
	if want := 1 + 1 + 1 + 1 + 1 + 1; len(rules) != want {
		t.Fatalf("got %d rules, want %d", len(rules), want)
	}
}
