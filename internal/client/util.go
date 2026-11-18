package client

import (
	"context"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

func timeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// originalDst returns the destination the application dialled before the
// nat redirect (SO_ORIGINAL_DST, from conntrack). For a connection that was
// not redirected it is simply the local address.
func originalDst(c *net.TCPConn) (netip.AddrPort, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var ap netip.AddrPort
	var serr error
	err = rc.Control(func(fd uintptr) {
		// SO_ORIGINAL_DST returns a sockaddr_in; IPv6Mreq is the usual
		// 16-byte container for it: [2:4] port, [4:8] address.
		m, e := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if e != nil {
			serr = e
			return
		}
		b := m.Multiaddr
		ap = netip.AddrPortFrom(netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]}), uint16(b[2])<<8|uint16(b[3]))
	})
	if err != nil {
		return ap, err
	}
	return ap, serr
}
