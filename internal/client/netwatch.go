package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// NetState is what the client knows about the laptop's routing.
type NetState struct {
	Uplink string // physical interface carrying the default route, "" if none
	VPN    string // why tandem must stay off because of another VPN, "" if none

	// For discovery: which network this is.
	Gateway    netip.Addr
	GatewayMAC string
	Prefixes   []netip.Prefix // the uplink's IPv4 addresses, sorted
}

// Fingerprint names the network: the default gateway's MAC address and the
// uplink's subnets. "" when the gateway's MAC is not known yet. A replaced
// router gets a new fingerprint (the cached relay is forgotten there until a
// beacon or ping re-verifies it); several access points behind one router
// share one.
func (n NetState) Fingerprint() string {
	if n.GatewayMAC == "" || len(n.Prefixes) == 0 {
		return ""
	}
	fp := n.GatewayMAC
	for _, p := range n.Prefixes {
		fp += " " + p.Masked().String()
	}
	return fp
}

func (n NetState) onLinkSubnet(a netip.Addr) bool {
	for _, p := range n.Prefixes {
		if p.Masked().Contains(a) {
			return true
		}
	}
	return false
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
		Dev     string `json:"dev"`
		Gateway string `json:"gateway"`
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
	ns := NetState{Uplink: dev}
	ns.Gateway, _ = netip.ParseAddr(routes[0].Gateway)
	if ifc, err := net.InterfaceByName(dev); err == nil {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
				ns.Prefixes = append(ns.Prefixes, p)
			}
		}
		sort.Slice(ns.Prefixes, func(i, j int) bool { return ns.Prefixes[i].Addr().Less(ns.Prefixes[j].Addr()) })
	}
	if ns.Gateway.IsValid() {
		ns.GatewayMAC = neighMAC(ctx, dev, ns.Gateway)
	}
	return ns, nil
}

// neighMAC is the gateway's link-layer address from the neighbour table.
// Any traffic to the internet resolves it; an incomplete or failed entry
// has no address and gives "".
func neighMAC(ctx context.Context, dev string, gw netip.Addr) string {
	out, err := exec.CommandContext(ctx, "ip", "-j", "neigh", "show", gw.String(), "dev", dev).Output()
	if err != nil {
		return ""
	}
	var ns []struct {
		Lladdr string `json:"lladdr"`
	}
	if json.Unmarshal(out, &ns) != nil {
		return ""
	}
	for _, n := range ns {
		if n.Lladdr != "" {
			return n.Lladdr
		}
	}
	return ""
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

// WatchNetlink calls onEvent for every link, IPv4 address or IPv4 route
// change, so a move to another network is acted on at once instead of at
// the next 2 s poll. It only wakes the caller; the poll does the reading.
func WatchNetlink(onEvent func(), stop <-chan struct{}) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV4_ROUTE}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return err
	}
	go func() { <-stop; unix.Close(fd) }()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, _, err := unix.Recvfrom(fd, buf, 0); err != nil {
				if err == unix.EINTR || err == unix.ENOBUFS {
					onEvent() // ENOBUFS: we missed some; treat it as a change
					continue
				}
				return
			}
			onEvent()
		}
	}()
	return nil
}
