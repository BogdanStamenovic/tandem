package relay

import (
	"context"
	"log"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/config"
	"github.com/BogdanStamenovic/tandem/internal/discovery"
	"golang.org/x/sys/unix"
)

// beaconer advertises the relay on every local address that lies inside an
// allow prefix, and answers clients' discovery queries.
type beaconer struct {
	key    []byte
	id     [8]byte
	ifErr  atomic.Value // string: last interface-listing error, logged once
	port   uint16
	allow  []netip.Prefix
	replay *discovery.Replay

	sent, answered, rejected atomic.Int64
	lastAddrs                atomic.Value // []string
}

func newBeaconer(psk []byte, port uint16, allow []netip.Prefix) *beaconer {
	host, _ := os.Hostname()
	b := &beaconer{key: discovery.Key(psk), id: discovery.RelayID(psk, host), port: port, allow: allow, replay: discovery.NewReplay()}
	b.lastAddrs.Store([]string(nil))
	return b
}

// lanAddrs returns each local IPv4 prefix whose address is inside an allow
// prefix. Re-read every time, so a DHCP change of address is picked up at
// the next beacon without a restart.
func (b *beaconer) lanAddrs() []netip.Prefix {
	ifs, err := net.Interfaces()
	if err != nil {
		if prev, _ := b.ifErr.Load().(string); prev != err.Error() {
			b.ifErr.Store(err.Error())
			log.Printf("relay: cannot list interfaces, no beacons: %v", err)
		}
		return nil
	}
	var out []netip.Prefix
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil || !p.Addr().Is4() {
				continue
			}
			for _, al := range b.allow {
				if al.Contains(p.Addr()) {
					out = append(out, p)
					break
				}
			}
		}
	}
	return out
}

func broadcastOf(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	m := net.CIDRMask(p.Bits(), 32)
	for i := range a {
		a[i] |= ^m[i]
	}
	return netip.AddrFrom4(a)
}

func broadcastControl(_, _ string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	})
}

func (b *beaconer) beacon(kind byte, from netip.Addr, to netip.AddrPort, echo [16]byte) error {
	pkt := discovery.Beacon{Kind: kind, Time: time.Now(), Nonce: discovery.NewNonce(), ID: b.id,
		Addr: netip.AddrPortFrom(from, b.port), Echo: echo}.Marshal(b.key)
	d := net.Dialer{LocalAddr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(from, 0)), Control: broadcastControl}
	c, err := d.DialContext(context.Background(), "udp4", to.String())
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write(pkt)
	return err
}

func (b *beaconer) beaconLoop(stop <-chan struct{}) {
	var lastErr string
	for {
		var names []string
		for _, p := range b.lanAddrs() {
			names = append(names, p.String())
			if err := b.beacon(discovery.KindBeacon, p.Addr(), netip.AddrPortFrom(broadcastOf(p), discovery.Port), [16]byte{}); err != nil {
				if err.Error() != lastErr { // once per distinct error, not every 5 s
					log.Printf("relay: beacon from %s: %v", p.Addr(), err)
					lastErr = err.Error()
				}
				continue
			}
			b.sent.Add(1)
		}
		b.lastAddrs.Store(names)
		select {
		case <-stop:
			return
		case <-time.After(discovery.Interval):
		}
	}
}

// answerLoop serves queries on UDP Port. The answer comes from the local
// address on the querier's subnet, which is the address the client must use.
func (b *beaconer) answerLoop(stop <-chan struct{}) {
	lc := net.ListenConfig{Control: broadcastControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", netip.AddrPortFrom(netip.IPv4Unspecified(), discovery.Port).String())
	if err != nil {
		log.Printf("relay: discovery queries unavailable: %v (beacons still sent)", err)
		return
	}
	go func() { <-stop; pc.Close() }()
	buf := make([]byte, 512)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		sap := src.(*net.UDPAddr).AddrPort()
		q, err := discovery.ParseQuery(buf[:n], b.key, time.Now(), b.replay)
		if err != nil {
			b.rejected.Add(1)
			continue
		}
		from, ok := b.localFor(sap.Addr())
		if !ok {
			b.rejected.Add(1)
			continue
		}
		if err := b.beacon(discovery.KindAnswer, from, netip.AddrPortFrom(sap.Addr().Unmap(), sap.Port()), q.Nonce); err == nil {
			b.answered.Add(1)
		}
	}
}

func (b *beaconer) localFor(client netip.Addr) (netip.Addr, bool) {
	client = client.Unmap()
	for _, p := range b.lanAddrs() {
		if p.Masked().Contains(client) {
			return p.Addr(), true
		}
	}
	return netip.Addr{}, false
}

// Advertised is the address to hand a client when pairing: the listen
// address, or with 0.0.0.0 the first local address inside the allow list.
func Advertised(cfg *config.Config) string {
	ap, err := netip.ParseAddrPort(cfg.Listen)
	if err != nil || !ap.Addr().IsUnspecified() {
		return cfg.Listen
	}
	var allow []netip.Prefix
	for _, a := range cfg.Allow {
		if p, err := netip.ParsePrefix(a); err == nil {
			allow = append(allow, p)
		}
	}
	b := &beaconer{allow: allow}
	if ps := b.lanAddrs(); len(ps) > 0 {
		return netip.AddrPortFrom(ps[0].Addr(), ap.Port()).String()
	}
	return cfg.Listen
}
