// Package pipe joins two TCP connections and measures what flowed.
package pipe

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// idleCap is the longest gap between reads still counted as transfer time.
// Longer gaps are the application or server being idle, not the network
// being slow, so counting them would make a fast path look slow.
const idleCap = 250 * time.Millisecond

// Meter is what one direction moved.
type Meter struct {
	Bytes  int64
	Active time.Duration
	last   time.Time
}

func (m *Meter) tick(n int, now time.Time) {
	if !m.last.IsZero() {
		gap := now.Sub(m.last)
		if gap > idleCap {
			gap = idleCap
		}
		m.Active += gap
	}
	m.last = now
	m.Bytes += int64(n)
}

// Result of a finished pipe.
type Result struct {
	Up, Down Meter // Up: client->server. Down: server->client.
}

// Busier returns the direction that moved more bytes.
func (r Result) Busier() Meter {
	if r.Up.Bytes > r.Down.Bytes {
		return r.Up
	}
	return r.Down
}

// Join copies both ways until both directions finish, propagating half-closes.
// client is the application side, server the upstream side. measured selects
// metered copies (userspace buffer) instead of plain io.Copy (splice).
func Join(client, server *net.TCPConn, measured bool) Result {
	var res Result
	var wg sync.WaitGroup
	wg.Add(2)
	run := func(dst, src *net.TCPConn, m *Meter) {
		defer wg.Done()
		var err error
		if measured {
			err = copyMeter(dst, src, m)
		} else {
			var n int64
			n, err = io.Copy(dst, src)
			m.Bytes = n
		}
		if err != nil && !errors.Is(err, net.ErrClosed) {
			// A reset or timeout on one side: tear both down, and reset
			// rather than FIN so the application sees the failure.
			Abort(dst)
			Abort(src)
			return
		}
		_ = dst.CloseWrite()
	}
	go run(server, client, &res.Up)
	go run(client, server, &res.Down)
	wg.Wait()
	client.Close()
	server.Close()
	return res
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 128<<10); return &b }}

func copyMeter(dst, src *net.TCPConn, m *Meter) error {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, err := src.Read(buf)
		if n > 0 {
			m.tick(n, time.Now())
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Abort closes c with a RST.
func Abort(c *net.TCPConn) {
	_ = c.SetLinger(0)
	_ = c.Close()
}

// TuneLeg sets keepalive and TCP_USER_TIMEOUT so a peer that vanished
// silently (a relay that went to sleep) is noticed in ~timeout instead of the
// kernel default of ~15 minutes of retransmits.
func TuneLeg(c *net.TCPConn, timeout time.Duration) {
	_ = c.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 3 * time.Second, Count: 3})
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
		})
	}
}

// MarkControl returns a net.Dialer Control func setting SO_MARK.
func MarkControl(mark uint32) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark))
		})
		if err != nil {
			return err
		}
		return serr
	}
}
