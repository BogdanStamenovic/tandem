// vminit is PID 1 of a throwaway VM that stands in for a small single-board
// relay box (test/smallhw.sh vm). It is the whole userspace: no shell, no
// busybox. It loads the virtio-net modules, configures eth0 from the kernel
// command line, serves a snapshot of the guest's own CPU and memory counters
// on :9100, and runs the relay.
//
// Kernel command line: tandem.ip=A.B.C.D/NN tandem.gw=A.B.C.D
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func say(f string, a ...any) { fmt.Fprintf(os.Stderr, "vminit: "+f+"\n", a...) }

func main() {
	_ = unix.Mount("proc", "/proc", "proc", 0, "")
	_ = unix.Mount("sysfs", "/sys", "sysfs", 0, "")
	_ = unix.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	_ = unix.Mount("tmpfs", "/run", "tmpfs", 0, "")
	for _, m := range []string{"failover", "net_failover", "virtio_net"} {
		f, err := os.Open("/lib/" + m + ".ko")
		if err != nil {
			say("open %s: %v", m, err)
			continue
		}
		if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil && err != unix.EEXIST {
			say("load %s: %v", m, err)
		}
		f.Close()
	}
	args := map[string]string{}
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		for _, f := range strings.Fields(string(b)) {
			if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(k, "tandem.") {
				args[k] = v
			}
		}
	}
	ifUp("lo")
	for i := 0; i < 100; i++ {
		if _, err := net.InterfaceByName("eth0"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pfx := netip.MustParsePrefix(args["tandem.ip"])
	if err := setAddr("eth0", pfx); err != nil {
		say("addr: %v", err)
	}
	ifUp("eth0")
	if err := addDefault(netip.MustParseAddr(args["tandem.gw"])); err != nil {
		say("route: %v", err)
	}
	go statServer(pfx.Addr().String() + ":9100")
	for {
		cmd := exec.Command("/tandem", "run")
		cmd.Env = []string{"TANDEM_CONFIG=/etc/tandem.json", "HOME=/run"}
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Run()
		say("relay exited: %v (pid %d); restarting in 1s", err, cmd.ProcessState.Pid())
		time.Sleep(time.Second)
	}
}

// statServer answers every connection with /proc/stat's cpu line, meminfo
// and the relay's status, then closes. The host diffs two snapshots.
func statServer(addr string) {
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		say("stat: %v", err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		var b strings.Builder
		if s, err := os.ReadFile("/proc/stat"); err == nil {
			b.WriteString(strings.SplitN(string(s), "\n", 2)[0] + "\n")
		}
		if s, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, l := range strings.Split(string(s), "\n") {
				if strings.HasPrefix(l, "MemTotal") || strings.HasPrefix(l, "MemAvailable") || strings.HasPrefix(l, "MemFree") {
					b.WriteString(l + "\n")
				}
			}
		}
		if ents, err := os.ReadDir("/proc"); err == nil {
			for _, e := range ents {
				exe, err := os.Readlink("/proc/" + e.Name() + "/exe")
				if err != nil || exe != "/tandem" {
					continue
				}
				if s, err := os.ReadFile("/proc/" + e.Name() + "/status"); err == nil {
					for _, l := range strings.Split(string(s), "\n") {
						if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") || strings.HasPrefix(l, "Threads") {
							b.WriteString(l + "\n")
						}
					}
				}
				if fds, err := os.ReadDir("/proc/" + e.Name() + "/fd"); err == nil {
					fmt.Fprintf(&b, "FDs:\t%d\n", len(fds))
				}
			}
		}
		_, _ = c.Write([]byte(b.String()))
		c.Close()
	}
}

func ioctlSock() int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		panic(err)
	}
	return fd
}

func ifUp(name string) {
	fd := ioctlSock()
	defer unix.Close(fd)
	ifr, _ := unix.NewIfreq(name)
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		say("flags %s: %v", name, err)
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		say("up %s: %v", name, err)
	}
}

func setAddr(name string, p netip.Prefix) error {
	fd := ioctlSock()
	defer unix.Close(fd)
	ifr, _ := unix.NewIfreq(name)
	if err := ifr.SetInet4Addr(p.Addr().AsSlice()); err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, ifr); err != nil {
		return err
	}
	mask := net.CIDRMask(p.Bits(), 32)
	ifr, _ = unix.NewIfreq(name)
	_ = ifr.SetInet4Addr(mask)
	return unix.IoctlIfreq(fd, unix.SIOCSIFNETMASK, ifr)
}

// addDefault adds "default via gw" with one RTM_NEWROUTE netlink message.
func addDefault(gw netip.Addr) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	rt := unix.RtMsg{Family: unix.AF_INET, Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_BOOT,
		Scope: unix.RT_SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	attr := make([]byte, 8)
	binary.NativeEndian.PutUint16(attr[0:2], 8)
	binary.NativeEndian.PutUint16(attr[2:4], unix.RTA_GATEWAY)
	copy(attr[4:], gw.AsSlice())
	body := append((*[unix.SizeofRtMsg]byte)(unsafe.Pointer(&rt))[:], attr...)
	hdr := unix.NlMsghdr{Len: uint32(unix.SizeofNlMsghdr + len(body)), Type: unix.RTM_NEWROUTE,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_CREATE | unix.NLM_F_ACK, Seq: 1}
	msg := append((*[unix.SizeofNlMsghdr]byte)(unsafe.Pointer(&hdr))[:], body...)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	if n >= unix.SizeofNlMsghdr+4 {
		if code := int32(binary.NativeEndian.Uint32(buf[unix.SizeofNlMsghdr:])); code != 0 {
			return unix.Errno(-code)
		}
	}
	return nil
}
