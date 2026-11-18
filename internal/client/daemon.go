// Package client is the laptop side: it owns the redirect rules, accepts
// redirected connections and sends each one direct or through the relay.
package client

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/config"
	"github.com/BogdanStamenovic/tandem/internal/control"
	"github.com/BogdanStamenovic/tandem/internal/decide"
	"github.com/BogdanStamenovic/tandem/internal/netpolicy"
	"github.com/BogdanStamenovic/tandem/internal/nftrules"
	"github.com/BogdanStamenovic/tandem/internal/pair"
	"github.com/BogdanStamenovic/tandem/internal/pipe"
	"github.com/BogdanStamenovic/tandem/internal/sysd"
	"github.com/BogdanStamenovic/tandem/internal/wire"
)

const (
	directDialTimeout = 10 * time.Second
	relayReplyTimeout = 10 * time.Second // covers the relay's own upstream dial
	legTimeout        = 20 * time.Second // TCP_USER_TIMEOUT on the relay leg
	selfCheckMsg      = "tandem-ok\n"
	drainOnStop       = 8 * time.Second
	statsFile         = "stats.json"
	disabledFile      = "disabled"
)

type Daemon struct {
	cfg    *config.Config
	psk    []byte
	relay  netip.AddrPort
	eng    *decide.Engine
	rules  *nftrules.Rules
	health *Health
	ln     *net.TCPListener
	local  netip.AddrPort

	bypass      []netip.Prefix
	bypassPorts []uint16

	mu       sync.Mutex
	net      NetState
	netErr   string
	ruleErr  string
	started  time.Time
	recent   []ConnRecord
	reconcil chan struct{}

	active   [2]atomic.Int64
	total    [2]atomic.Int64
	bytes    [2]atomic.Int64
	fallback atomic.Int64
	failed   atomic.Int64
	// forced pins every eligible connection to one path (-1 = auto). For
	// benchmarks; not persisted, so a restart always returns to auto.
	forced atomic.Int32
}

// ConnRecord is one finished (or failed) connection, for status and why.
type ConnRecord struct {
	At       time.Time `json:"at"`
	Dest     string    `json:"dest"`
	Path     string    `json:"path"`
	Reason   string    `json:"reason"`
	Fallback string    `json:"fallback,omitempty"`
	Bytes    int64     `json:"bytes"`
	Rate     string    `json:"rate,omitempty"`
	Sampled  bool      `json:"sampled"`
}

func Run(cfg *config.Config) error {
	psk, err := pair.Load(cfg.PSKFile)
	if err != nil {
		return fmt.Errorf("client: key: %w (run `tandem pair`)", err)
	}
	def := decide.Relay
	if cfg.Default == "direct" {
		def = decide.Direct
	}
	d := &Daemon{
		cfg:      cfg,
		psk:      psk,
		relay:    netip.MustParseAddrPort(cfg.Relay),
		eng:      decide.New(decide.Config{Default: def}),
		started:  time.Now(),
		reconcil: make(chan struct{}, 1),
	}
	d.forced.Store(-1)
	d.bypass = append(d.bypass, netpolicy.Bypass...)
	for _, n := range cfg.BypassNets {
		d.bypass = append(d.bypass, netip.MustParsePrefix(n))
	}
	d.bypass = append(d.bypass, netip.PrefixFrom(d.relay.Addr(), 32))
	d.bypassPorts = append(append(d.bypassPorts, netpolicy.BypassPorts...), cfg.BypassPorts...)

	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	if err := d.eng.Load(filepath.Join(cfg.StateDir, statsFile)); err != nil {
		log.Printf("client: ignoring learned stats: %v", err)
	}

	d.local = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), cfg.LocalPort)
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(d.local))
	if err != nil {
		return fmt.Errorf("client: listen %s: %w", d.local, err)
	}
	d.ln = ln
	if d.rules, err = nftrules.Open(); err != nil {
		ln.Close()
		return fmt.Errorf("client: nftables: %w", err)
	}
	d.health = NewHealth(d.relay, psk, func(up bool) {
		log.Printf("client: relay %s is now %s", d.relay, map[bool]string{true: "up", false: "down"}[up])
		d.kickReconcile()
	})

	stop := make(chan struct{})
	go d.acceptLoop()
	go d.health.Run(stop)
	go d.reconcileLoop(stop)
	go d.saveLoop(stop)
	cln, err := control.Serve(cfg.RunDir, d.handleControl)
	if err != nil {
		log.Printf("client: control socket unavailable: %v", err)
	}
	if wd := sysd.WatchdogInterval(); wd > 0 {
		go d.watchdog(wd, stop)
	}
	_ = sysd.Notify("READY=1")
	log.Printf("client: listening on %s, relay %s", d.local, d.relay)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	s := <-sig
	log.Printf("client: %v: pausing redirect, draining", s)
	_ = sysd.Notify("STOPPING=1")
	close(stop)
	// Pause first: new connections go direct in the kernel, while the ones
	// already redirected keep their NAT mapping until they finish or the
	// drain times out. The table itself goes when the process exits.
	if err := d.rules.Pause(); err != nil {
		log.Printf("client: pausing rules: %v", err)
	}
	if cln != nil {
		cln.Close()
	}
	if err := d.eng.Save(filepath.Join(cfg.StateDir, statsFile)); err != nil {
		log.Printf("client: saving stats: %v", err)
	}
	deadline := time.Now().Add(drainOnStop)
	for time.Now().Before(deadline) && d.active[0].Load()+d.active[1].Load() > 0 {
		time.Sleep(200 * time.Millisecond)
	}
	ln.Close()
	if err := d.rules.Close(); err != nil {
		log.Printf("client: removing rules: %v", err)
	}
	return nil
}

func (d *Daemon) kickReconcile() {
	select {
	case d.reconcil <- struct{}{}:
	default:
	}
}

func (d *Daemon) disabled() bool {
	_, err := os.Stat(filepath.Join(d.cfg.StateDir, disabledFile))
	return err == nil
}

// want reports whether the redirect rules should be installed, and why not.
func (d *Daemon) want() (bool, string) {
	d.mu.Lock()
	ns := d.net
	d.mu.Unlock()
	switch {
	case d.disabled():
		return false, "disabled (tandem enable to turn back on)"
	case ns.VPN != "":
		return false, ns.VPN
	case ns.Uplink == "":
		return false, "no default route"
	case !d.health.Up():
		return false, "relay " + d.relay.String() + " unreachable"
	}
	return true, ""
}

func (d *Daemon) spec(uplink string) nftrules.Spec {
	return nftrules.Spec{
		Port:        d.cfg.LocalPort,
		Uplink:      uplink,
		Bypass:      d.bypass,
		BypassPorts: d.bypassPorts,
		SkipMarks:   [][2]uint32{{netpolicy.OwnMark, netpolicy.OwnMask}, {netpolicy.TailscaleMark, netpolicy.TailscaleMask}},
	}
}

func (d *Daemon) reconcileLoop(stop <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		ns, err := ProbeNet()
		d.mu.Lock()
		d.net = ns
		d.netErr = ""
		if err != nil {
			d.netErr = err.Error()
		}
		d.mu.Unlock()

		ok, why := d.want()
		installed, cur := d.rules.Installed()
		var rerr error
		switch {
		case ok && (!installed || cur.Uplink != ns.Uplink):
			rerr = d.rules.Apply(d.spec(ns.Uplink))
			if rerr == nil {
				log.Printf("client: rules installed on %s", ns.Uplink)
			}
		case !ok && installed:
			rerr = d.rules.Pause()
			if rerr == nil {
				log.Printf("client: redirect paused, new connections go direct: %s", why)
			}
		}
		d.mu.Lock()
		d.ruleErr = ""
		if rerr != nil {
			d.ruleErr = rerr.Error()
			log.Printf("client: rules: %v", rerr)
		}
		d.mu.Unlock()
		select {
		case <-stop:
			return
		case <-t.C:
		case <-d.reconcil:
		}
	}
}

func (d *Daemon) saveLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := d.eng.Save(filepath.Join(d.cfg.StateDir, statsFile)); err != nil {
				log.Printf("client: saving stats: %v", err)
			}
		}
	}
}

// watchdog pings systemd only while the accept loop answers a real
// connection. A hung daemon stops pinging, systemd kills it, and the kernel
// drops the owned table with it.
func (d *Daemon) watchdog(every time.Duration, stop <-chan struct{}) {
	buf := make([]byte, len(selfCheckMsg))
	for {
		c, err := net.DialTimeout("tcp4", d.local.String(), time.Second)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, err = c.Read(buf)
			c.Close()
			if err == nil && string(buf) == selfCheckMsg {
				_ = sysd.Notify("WATCHDOG=1")
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
	}
}

func (d *Daemon) acceptLoop() {
	for {
		c, err := d.ln.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("client: accept: %v", err)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		go d.handle(c)
	}
}

func (d *Daemon) handle(c *net.TCPConn) {
	dst, err := originalDst(c)
	if err != nil || dst == d.local || dst.Addr().IsLoopback() {
		// Not redirected: someone connected to the port itself. The only
		// legitimate caller is our own watchdog.
		if err == nil {
			_, _ = c.Write([]byte(selfCheckMsg))
		}
		c.Close()
		return
	}
	key := decide.KeyFor(dst)
	var choice decide.Choice
	switch {
	case !netpolicy.IsPublic(dst.Addr()) || netpolicy.IsBypassPort(dst.Port()):
		choice = decide.Choice{Path: decide.Direct, Reason: "never relayed (private address or bypass port)"}
	case !d.health.Up():
		choice = decide.Choice{Path: decide.Direct, Reason: "relay down"}
	case d.forced.Load() >= 0:
		p := decide.Path(d.forced.Load())
		choice = decide.Choice{Path: p, Reason: "forced " + p.String() + " (tandem force auto to undo)"}
	default:
		choice = d.eng.Choose(key)
	}
	rec := ConnRecord{At: time.Now(), Dest: dst.String(), Path: choice.Path.String(), Reason: choice.Reason}

	var up *net.TCPConn
	path := choice.Path
	if path == decide.Relay {
		up, err = d.dialRelay(dst)
		if err != nil {
			rec.Fallback = err.Error()
			d.fallback.Add(1)
			path = decide.Direct
		}
	}
	if path == decide.Direct {
		dl := net.Dialer{Timeout: directDialTimeout, Control: pipe.MarkControl(netpolicy.OwnMark)}
		var uc net.Conn
		uc, err = dl.Dial("tcp4", dst.String())
		if err != nil {
			d.failed.Add(1)
			rec.Path, rec.Reason = "failed", err.Error()
			d.remember(rec)
			pipe.Abort(c) // the application sees a reset, as close to "refused" as we can get now
			return
		}
		up = uc.(*net.TCPConn)
	}
	rec.Path = path.String()

	d.active[path].Add(1)
	d.total[path].Add(1)
	res := pipe.Join(c, up, true)
	d.active[path].Add(-1)
	busy := res.Busier()
	d.bytes[path].Add(res.Up.Bytes + res.Down.Bytes)
	rec.Bytes = res.Up.Bytes + res.Down.Bytes
	if busy.Active > 0 {
		rec.Rate = decide.FmtRate(float64(busy.Bytes) / busy.Active.Seconds())
	}
	// Fallbacks are not a fair sample of either path; forced runs are, and
	// a benchmark is exactly the data the engine wants.
	if rec.Fallback == "" {
		rec.Sampled = d.eng.Observe(decide.Sample{Key: key, Path: path, Bytes: busy.Bytes, Active: busy.Active})
	}
	d.remember(rec)
}

func (d *Daemon) dialRelay(dst netip.AddrPort) (*net.TCPConn, error) {
	dl := net.Dialer{Timeout: relayDialTimeout, Control: pipe.MarkControl(netpolicy.OwnMark)}
	nc, err := dl.Dial("tcp4", d.relay.String())
	if err != nil {
		d.health.ReportFailure(err)
		return nil, fmt.Errorf("relay unreachable: %v", err)
	}
	c := nc.(*net.TCPConn)
	req, err := wire.NewRequest(dst)
	if err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(relayReplyTimeout))
	if _, err := c.Write(req.Marshal(d.psk)); err != nil {
		c.Close()
		d.health.ReportFailure(err)
		return nil, fmt.Errorf("relay write: %v", err)
	}
	st, err := wire.ReadReply(c, d.psk, req)
	if err != nil {
		c.Close()
		d.health.ReportFailure(err)
		return nil, fmt.Errorf("relay reply: %v", err)
	}
	if st != wire.StatusOK {
		c.Close()
		return nil, fmt.Errorf("relay: %s", st)
	}
	_ = c.SetDeadline(time.Time{})
	pipe.TuneLeg(c, legTimeout)
	return c, nil
}

var debug = os.Getenv("TANDEM_DEBUG") != ""

func (d *Daemon) remember(r ConnRecord) {
	if debug {
		log.Printf("conn %s path=%s bytes=%d rate=%s reason=%q fallback=%q", r.Dest, r.Path, r.Bytes, r.Rate, r.Reason, r.Fallback)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recent = append(d.recent, r)
	if len(d.recent) > 200 {
		d.recent = d.recent[len(d.recent)-200:]
	}
}
