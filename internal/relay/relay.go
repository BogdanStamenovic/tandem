// Package relay is the archserver side: it accepts authenticated requests
// from paired clients on the LAN and dials the real destination for them.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/config"
	"github.com/BogdanStamenovic/tandem/internal/control"
	"github.com/BogdanStamenovic/tandem/internal/netpolicy"
	"github.com/BogdanStamenovic/tandem/internal/pair"
	"github.com/BogdanStamenovic/tandem/internal/pipe"
	"github.com/BogdanStamenovic/tandem/internal/sysd"
	"github.com/BogdanStamenovic/tandem/internal/wire"
	"golang.org/x/sys/unix"
)

const (
	maxConns      = 2048
	handshakeWait = 5 * time.Second
	upstreamDial  = 8 * time.Second
)

type Relay struct {
	cfg    *config.Config
	psk    []byte
	allow  []netip.Prefix
	replay *wire.ReplayCache
	sem    chan struct{}
	start  time.Time

	active, total, pings     atomic.Int64
	authFail, refused, dialF atomic.Int64
	bytesUp, bytesDown       atomic.Int64
	lastAuthLog              atomic.Int64
}

// Status is what `tandem status` shows on the relay.
type Status struct {
	Role          string   `json:"role"`
	Listen        string   `json:"listen"`
	Allow         []string `json:"allow"`
	Uptime        string   `json:"uptime"`
	Active        int64    `json:"active"`
	Total         int64    `json:"total"`
	Pings         int64    `json:"pings"`
	AuthFailures  int64    `json:"auth_failures"`
	Refused       int64    `json:"refused"`
	DialFailures  int64    `json:"dial_failures"`
	BytesToClient int64    `json:"bytes_to_client"`
	BytesUpstream int64    `json:"bytes_upstream"`
}

func Run(cfg *config.Config) error {
	psk, err := pair.Load(cfg.PSKFile)
	if err != nil {
		return fmt.Errorf("relay: key: %w (run `tandem pair --generate`)", err)
	}
	r := &Relay{cfg: cfg, psk: psk, replay: wire.NewReplayCache(), sem: make(chan struct{}, maxConns), start: time.Now()}
	for _, a := range cfg.Allow {
		r.allow = append(r.allow, netip.MustParsePrefix(a))
	}
	// IP_FREEBIND lets the relay start before the LAN address is configured
	// (boot ordering) without listening on every interface.
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
		})
	}}
	ln, err := lc.Listen(context.Background(), "tcp4", cfg.Listen)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", cfg.Listen, err)
	}
	cln, err := control.Serve(cfg.RunDir, func(req control.Request, uid int) (any, error) {
		if req.Cmd != "status" {
			return nil, fmt.Errorf("relay: unknown command %q", req.Cmd)
		}
		return r.status(), nil
	})
	if err != nil {
		log.Printf("relay: control socket unavailable: %v", err)
	} else {
		defer cln.Close()
	}
	log.Printf("relay: listening on %s, allowing %v", cfg.Listen, cfg.Allow)
	_ = sysd.Notify("READY=1")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		_ = sysd.Notify("STOPPING=1")
		ln.Close()
	}()
	if wd := sysd.WatchdogInterval(); wd > 0 {
		go func() {
			for range time.Tick(wd) {
				_ = sysd.Notify("WATCHDOG=1")
			}
		}()
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("relay: accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go r.handle(c.(*net.TCPConn))
	}
}

func (r *Relay) allowed(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range r.allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (r *Relay) status() Status {
	return Status{
		Role: "relay", Listen: r.cfg.Listen, Allow: r.cfg.Allow,
		Uptime: time.Since(r.start).Round(time.Second).String(),
		Active: r.active.Load(), Total: r.total.Load(), Pings: r.pings.Load(),
		AuthFailures: r.authFail.Load(), Refused: r.refused.Load(), DialFailures: r.dialF.Load(),
		BytesToClient: r.bytesDown.Load(), BytesUpstream: r.bytesUp.Load(),
	}
}

func (r *Relay) handle(c *net.TCPConn) {
	remote := c.RemoteAddr().(*net.TCPAddr).AddrPort()
	if !r.allowed(remote.Addr()) {
		pipe.Abort(c)
		return
	}
	_ = c.SetDeadline(time.Now().Add(handshakeWait))
	req, err := wire.ReadRequest(c, r.psk, r.replay)
	if err != nil {
		r.authFail.Add(1)
		// At most one log line a minute: a misbehaving peer must not fill the journal.
		if now := time.Now().Unix(); r.lastAuthLog.Load() < now-60 {
			r.lastAuthLog.Store(now)
			log.Printf("relay: rejected %s: %v", remote, err)
		}
		pipe.Abort(c)
		return
	}
	reply := func(st wire.Status) error {
		_, err := c.Write(wire.MarshalReply(r.psk, req, st))
		return err
	}
	if req.IsPing() {
		r.pings.Add(1)
		_ = reply(wire.StatusOK)
		c.Close()
		return
	}
	if !netpolicy.IsPublic(req.Dest.Addr()) || req.Dest.Port() == 0 {
		r.refused.Add(1)
		_ = reply(wire.StatusRefused)
		c.Close()
		return
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	default:
		_ = reply(wire.StatusUnavailable)
		c.Close()
		return
	}
	up, err := net.DialTimeout("tcp4", req.Dest.String(), upstreamDial)
	if err != nil {
		r.dialF.Add(1)
		_ = reply(wire.StatusDialFailed)
		c.Close()
		return
	}
	if err := reply(wire.StatusOK); err != nil {
		up.Close()
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	pipe.TuneLeg(c, 20*time.Second)
	r.active.Add(1)
	r.total.Add(1)
	res := pipe.Join(c, up.(*net.TCPConn), false)
	r.active.Add(-1)
	r.bytesUp.Add(res.Up.Bytes)
	r.bytesDown.Add(res.Down.Bytes)
}
