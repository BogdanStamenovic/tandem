package client

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// NetState is what the client knows about the laptop's routing.
type NetState struct {
	Uplink string // physical interface carrying the default route, "" if none
	VPN    string // why tandem must stay off because of another VPN, "" if none
}

// probeDest is only used for a routing-table lookup; no packet is sent.
const probeDest = "1.1.1.1"

// ProbeNet asks the kernel where a public destination would be routed. If it
// leaves through anything but a plain Ethernet/Wi-Fi interface - a tun device
// (OpenVPN, tailscale exit node), WireGuard, PPP, an IPsec tunnel - another
// VPN owns the default route and tandem steps aside entirely.
func ProbeNet() (NetState, error) {
	ctx, cancel := timeoutCtx(2 * time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", "-j", "route", "get", probeDest).Output()
	if err != nil {
		// No route at all: offline. Nothing to redirect.
		return NetState{}, nil
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal(out, &routes); err != nil || len(routes) == 0 {
		return NetState{}, fmt.Errorf("netwatch: cannot parse `ip route get`: %v", err)
	}
	dev := routes[0].Dev
	if why := vpnLike(dev); why != "" {
		return NetState{VPN: why}, nil
	}
	// Policy-based IPsec (strongSwan without an xfrm interface) routes
	// through the physical NIC; its xfrm policies give it away.
	if pol, err := exec.CommandContext(ctx, "ip", "xfrm", "policy").Output(); err == nil && strings.TrimSpace(string(pol)) != "" {
		return NetState{VPN: "IPsec xfrm policies are installed"}, nil
	}
	return NetState{Uplink: dev}, nil
}

func vpnLike(dev string) string {
	sys := "/sys/class/net/" + dev
	typ, _ := os.ReadFile(sys + "/type")
	t := strings.TrimSpace(string(typ))
	if t != "1" { // ARPHRD_ETHER; wg/tun are 65534, ppp 512, ip tunnels 768+
		return fmt.Sprintf("default route goes through %s (link type %s), another VPN is active", dev, t)
	}
	if _, err := os.Stat(sys + "/tun_flags"); err == nil {
		return fmt.Sprintf("default route goes through tap device %s, another VPN is active", dev)
	}
	return ""
}
