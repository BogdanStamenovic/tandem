package control

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(c *net.UnixConn) int {
	rc, err := c.SyscallConn()
	if err != nil {
		return -1
	}
	uid := -1
	_ = rc.Control(func(fd uintptr) {
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
			uid = int(cred.Uid)
		}
	})
	return uid
}
