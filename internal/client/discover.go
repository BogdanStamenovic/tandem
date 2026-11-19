package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/discovery"
	"golang.org/x/sys/unix"
)

// Where the relay address came from.
const (
	SrcBeacon = "beacon" // a periodic signed beacon from the relay
	SrcQuery  = "query"  // the relay's signed answer to our query
	SrcCached = "cached" // last address that worked on this (known) network
	SrcStatic = "static" // "relay" in the config, on a known network
	SrcFixed  = "fixed"  // discovery off: the static address, everywhere
)

const (
	homesFile = "discovery.json"
	maxHomes  = 16
	// A beacon this recent still vouches for its address after the network
	// fingerprint changes (the gateway's MAC often resolves a moment after
	// the link comes up). Three beacon intervals.
	beaconTrust = 3 * discovery.Interval
	queryWait   = 10 * time.Second

	queryMinEvery = 2 * time.Second
	queryMaxEvery = 60 * time.Second
)

// home is a network on which a relay with our key has proven itself.
type home struct {
	Relay    string    `json:"relay"`
	Verified time.Time `json:"verified"`
}

// Finder decides which relay address, if any, the client may contact.
//
// The rule that matters: on a network that is not a verified home, only an
// authenticated beacon or answer yields an address. Without one the client
// probes nothing, so a foreign network with some host at the old relay IP
// never sees a packet from tandem.
type Finder struct {
	key       []byte
	static    netip.AddrPort
	discover  bool
	stateFile string
	onChange  func(netip.AddrPort, string)

	mu        sync.Mutex
	cur       netip.AddrPort
	src       string
	since     time.Time
	lastHeard time.Time // last valid beacon/answer for cur
	heardFP   string    // the network fingerprint when it was heard
	id        string
	fp        string
	prefixes  []netip.Prefix
	homes     map[string]home
	pending   map[[16]byte]time.Time
	replay    *discovery.Replay
	pc        net.PacketConn
	stats     DiscoveryCounters
	lastQuery time.Time
	qEvery    time.Duration // QuerySoon's current spacing (backoff)
	fails     int // consecutive failed probes of a cached/static address
}

type DiscoveryCounters struct {
	Beacons  int64            `json:"beacons"`
	Answers  int64            `json:"answers"`
	Queries  int64            `json:"queries_sent"`
	Rejected map[string]int64 `json:"rejected"`
}

type DiscoveryStatus struct {
	Enabled     bool              `json:"enabled"`
	Address     string            `json:"address,omitempty"`
	Source      string            `json:"source,omitempty"`
	RelayID     string            `json:"relay_id,omitempty"`
	Since       string            `json:"since,omitempty"`
	LastHeard   string            `json:"last_heard,omitempty"`
	Network     string            `json:"network,omitempty"`
	KnownHome   bool              `json:"known_home"`
	Static      string            `json:"static,omitempty"`
	Counters    DiscoveryCounters `json:"counters"`
	Explanation string            `json:"explanation,omitempty"`
}

func NewFinder(psk []byte, static netip.AddrPort, discover bool, stateDir string, onChange func(netip.AddrPort, string)) *Finder {
	f := &Finder{
		key: discovery.Key(psk), static: static, discover: discover,
		stateFile: filepath.Join(stateDir, homesFile), onChange: onChange,
		homes: map[string]home{}, pending: map[[16]byte]time.Time{}, replay: discovery.NewReplay(),
		stats: DiscoveryCounters{Rejected: map[string]int64{}},
	}
	f.homes = loadHomes(f.stateFile)
	if !discover && static.IsValid() {
		f.cur, f.src, f.since = static, SrcFixed, time.Now()
	}
	return f
}

func loadHomes(path string) map[string]home {
	h := map[string]home{}
	b, err := os.ReadFile(path)
	if err == nil {
		var st struct {
			Homes map[string]home `json:"homes"`
		}
		if json.Unmarshal(b, &st) == nil && st.Homes != nil {
			h = st.Homes
		}
	}
	return h
}

func saveHomes(path string, homes map[string]home) error {
	// Keep the most recently verified few; a laptop meets many networks.
	if len(homes) > maxHomes {
		type kv struct {
			k string
			v home
		}
		var all []kv
		for k, v := range homes {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].v.Verified.After(all[j].v.Verified) })
		homes = map[string]home{}
		for _, e := range all[:maxHomes] {
			homes[e.k] = e.v
		}
	}
	b, err := json.MarshalIndent(struct {
		Homes map[string]home `json:"homes"`
	}{homes}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RecordHome marks the current network as one where relay answered an
// authenticated ping. setup and upgrade use it so that a relay that cannot
// be heard by broadcast is still usable on the network it was set up on.
func RecordHome(stateDir string, relay netip.AddrPort) (string, error) {
	ns, err := ProbeNet()
	if err != nil {
		return "", err
	}
	fp := ns.Fingerprint()
	if fp == "" {
		return "", errors.New("no network fingerprint (gateway MAC unknown)")
	}
	path := filepath.Join(stateDir, homesFile)
	h := loadHomes(path)
	h[fp] = home{Relay: relay.String(), Verified: time.Now()}
	return fp, saveHomes(path, h)
}

// Current returns the address to use and where it came from; invalid when
// the client must not contact any relay.
func (f *Finder) Current() (netip.AddrPort, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur, f.src
}

// set switches to addr; callers hold f.mu. Returns whether it changed.
func (f *Finder) set(addr netip.AddrPort, src string) bool {
	if addr == f.cur && src == f.src {
		return false
	}
	changedAddr := addr != f.cur
	f.cur, f.src = addr, src
	f.fails = 0
	if changedAddr {
		f.since = time.Now()
		if addr.IsValid() {
			log.Printf("client: relay address %s (from %s)", addr, src)
		} else {
			log.Printf("client: no relay address on this network (%s); probing nothing", src)
		}
	}
	return changedAddr
}

func (f *Finder) notify(addr netip.AddrPort, src string) {
	if f.onChange != nil {
		f.onChange(addr, src)
	}
}

// NetChanged is called with every network probe; it acts only when the
// network's fingerprint or addresses changed.
func (f *Finder) NetChanged(ns NetState) {
	if !f.discover {
		return
	}
	fp := ns.Fingerprint()
	f.mu.Lock()
	if fp == f.fp && samePrefixes(f.prefixes, ns.Prefixes) {
		f.mu.Unlock()
		return
	}
	old := f.fp
	f.fp, f.prefixes = fp, ns.Prefixes
	f.resetBackoff()
	var addr netip.AddrPort
	src := "unknown network"
	switch {
	// A beacon heard while the network was still unidentified (link up,
	// gateway MAC not resolved yet) vouches for its address once the
	// fingerprint appears. One heard on another identified network never
	// does: that is how the client would end up probing a foreign LAN.
	case (f.src == SrcBeacon || f.src == SrcQuery) && time.Since(f.lastHeard) < beaconTrust &&
		(f.heardFP == "" || f.heardFP == fp) && ns.onLinkSubnet(f.cur.Addr()):
		addr, src = f.cur, f.src
	case fp != "":
		// A fallback must also be on this network's subnet: a relay is
		// always on the client's LAN, and an off-link address would send
		// the probe to the gateway, i.e. out of this network.
		if h, ok := f.homes[fp]; ok {
			if ap, err := netip.ParseAddrPort(h.Relay); err == nil && ns.onLinkSubnet(ap.Addr()) {
				addr, src = ap, SrcCached
			} else if f.static.IsValid() && ns.onLinkSubnet(f.static.Addr()) {
				addr, src = f.static, SrcStatic
			}
		}
	}
	if ns.Uplink == "" {
		addr, src = netip.AddrPort{}, "offline"
	}
	changed := f.set(addr, src)
	f.mu.Unlock()
	if old != fp {
		log.Printf("client: network %q (known home: %v)", fp, f.isHome(fp))
	}
	if changed {
		f.notify(addr, src)
	}
	if ns.Uplink != "" {
		f.Query()
	}
}

func (f *Finder) isHome(fp string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.homes[fp]
	return ok && fp != ""
}

func samePrefixes(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Verified records that the current relay answered an authenticated
// handshake on the current network, making it a known home.
func (f *Finder) Verified(addr netip.AddrPort) {
	f.mu.Lock()
	fp := f.fp
	if fp == "" || addr != f.cur {
		f.mu.Unlock()
		return
	}
	prev, had := f.homes[fp]
	f.homes[fp] = home{Relay: addr.String(), Verified: time.Now()}
	snapshot := make(map[string]home, len(f.homes))
	for k, v := range f.homes {
		snapshot[k] = v
	}
	f.mu.Unlock()
	if !had || prev.Relay != addr.String() || time.Since(prev.Verified) > time.Hour {
		if err := saveHomes(f.stateFile, snapshot); err != nil {
			log.Printf("client: saving known networks: %v", err)
		}
	}
}

// Failed is called when the relay went down. A fallback address that stopped
// answering gives way to the other fallback, and a query goes out at once
// in case the relay moved.
func (f *Finder) Failed() {
	if !f.discover {
		return
	}
	f.mu.Lock()
	var changed bool
	onLink := func(a netip.Addr) bool { return NetState{Prefixes: f.prefixes}.onLinkSubnet(a) }
	if f.src == SrcCached && f.static.IsValid() && f.static != f.cur && onLink(f.static.Addr()) {
		changed = f.set(f.static, SrcStatic)
	} else if f.src == SrcStatic {
		if h, ok := f.homes[f.fp]; ok {
			if ap, err := netip.ParseAddrPort(h.Relay); err == nil && ap != f.cur && onLink(ap.Addr()) {
				changed = f.set(ap, SrcCached)
			}
		}
	}
	addr, src := f.cur, f.src
	f.mu.Unlock()
	if changed {
		f.notify(addr, src)
	}
	f.Query()
}

func reuseControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

// Listen binds the discovery socket. Done before the first network probe so
// the query sent on start has a socket to go out of and come back to.
func (f *Finder) Listen() {
	if !f.discover {
		return
	}
	lc := net.ListenConfig{Control: reuseControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", netip.AddrPortFrom(netip.IPv4Unspecified(), discovery.Port).String())
	if err != nil {
		log.Printf("client: discovery unavailable, cannot listen on UDP %d: %v (cached/static fallback only)", discovery.Port, err)
		return
	}
	f.mu.Lock()
	f.pc = pc
	f.mu.Unlock()
}

// Run reads beacons and answers until stop closes.
func (f *Finder) Run(stop <-chan struct{}) {
	f.mu.Lock()
	pc := f.pc
	f.mu.Unlock()
	if pc == nil {
		return
	}
	go func() { <-stop; pc.Close() }()
	buf := make([]byte, 512)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		f.receive(buf[:n], src.(*net.UDPAddr).AddrPort().Addr().Unmap())
	}
}

func (f *Finder) reject(why string) {
	f.mu.Lock()
	f.stats.Rejected[why]++
	f.mu.Unlock()
}

func (f *Finder) receive(p []byte, src netip.Addr) {
	if len(p) > 0 && p[0] == 'T' && len(p) > 3 && p[3] == '1' && p[2] == 'Q' {
		return // our own (or another client's) query echoed by broadcast
	}
	b, err := discovery.ParseBeacon(p, f.key, src, time.Now(), f.replay)
	if err != nil {
		switch {
		case errors.Is(err, discovery.ErrBadMAC):
			f.reject("bad_mac")
		case errors.Is(err, discovery.ErrStale):
			f.reject("stale")
		case errors.Is(err, discovery.ErrReplay):
			f.reject("replay")
		case errors.Is(err, discovery.ErrMismatch):
			f.reject("source_mismatch")
		default:
			f.reject("malformed")
		}
		return
	}
	if !b.Addr.Addr().IsPrivate() || b.Addr.Port() == 0 {
		f.reject("not_private")
		return
	}
	f.mu.Lock()
	if !(NetState{Prefixes: f.prefixes}).onLinkSubnet(b.Addr.Addr()) {
		f.mu.Unlock()
		f.reject("off_link")
		return
	}
	src2 := SrcBeacon
	if b.Kind == discovery.KindAnswer {
		if _, ok := f.pending[b.Echo]; !ok {
			f.mu.Unlock()
			f.reject("unsolicited_answer")
			return
		}
		delete(f.pending, b.Echo)
		src2 = SrcQuery
		f.stats.Answers++
	} else {
		f.stats.Beacons++
	}
	f.lastHeard = time.Now()
	f.heardFP = f.fp
	f.resetBackoff()
	f.id = hex.EncodeToString(b.ID[:])
	// Keep the label of the first way we learned an unchanged address, so a
	// steady stream of beacons does not churn the status.
	changed := false
	if b.Addr != f.cur || f.src == SrcCached || f.src == SrcStatic || !f.cur.IsValid() {
		changed = f.set(b.Addr, src2)
	}
	addr, how := f.cur, f.src
	f.mu.Unlock()
	if changed {
		f.notify(addr, how)
	}
}

// ProbeFailed is called for every failed probe or relay dial. Two in a row
// on a fallback address move to the other fallback: a cached address that
// never answers would otherwise never be "up" and so never "fail".
func (f *Finder) ProbeFailed() {
	f.mu.Lock()
	rotate := false
	if f.src == SrcCached || f.src == SrcStatic {
		f.fails++
		rotate = f.fails >= 2
	}
	f.mu.Unlock()
	if rotate {
		f.Failed()
	}
	f.QuerySoon()
}

// QuerySoon is Query at most once per 2 s, for callers that may fire often
// (every failed probe or relay dial).
//
// Unanswered, the spacing doubles up to queryMaxEvery: with archserver
// powered off for hours, a fixed 2 s spacing broadcast 13,952 queries
// overnight (2026-11-18). Any valid beacon or answer, or a network change,
// resets it, so the first query after a failure is still immediate.
func (f *Finder) QuerySoon() {
	f.mu.Lock()
	if f.qEvery == 0 {
		f.qEvery = queryMinEvery
	}
	if time.Since(f.lastQuery) < f.qEvery {
		f.mu.Unlock()
		return
	}
	if !f.lastQuery.IsZero() && f.lastHeard.Before(f.lastQuery) {
		f.qEvery = min(2*f.qEvery, queryMaxEvery)
	}
	f.lastQuery = time.Now()
	f.mu.Unlock()
	f.Query()
}

func (f *Finder) resetBackoff() { f.qEvery = queryMinEvery } // callers hold f.mu

// Query broadcasts a signed query on every uplink subnet. Two repeats cover
// a single lost packet on Wi-Fi.
func (f *Finder) Query() {
	if !f.discover {
		return
	}
	f.mu.Lock()
	pc, prefixes := f.pc, append([]netip.Prefix(nil), f.prefixes...)
	f.mu.Unlock()
	if pc == nil || len(prefixes) == 0 {
		return
	}
	go func() {
		for i, d := range []time.Duration{0, 300 * time.Millisecond, time.Second} {
			if i > 0 {
				time.Sleep(d)
				f.mu.Lock()
				heard := time.Since(f.lastHeard) < d+50*time.Millisecond && f.cur.IsValid()
				f.mu.Unlock()
				if heard {
					return
				}
			}
			q := discovery.Query{Time: time.Now(), Nonce: discovery.NewNonce()}
			f.mu.Lock()
			now := time.Now()
			for n, t := range f.pending {
				if now.Sub(t) > queryWait {
					delete(f.pending, n)
				}
			}
			f.pending[q.Nonce] = now
			f.stats.Queries++
			f.mu.Unlock()
			pkt := q.Marshal(f.key)
			for _, p := range prefixes {
				if p.Bits() >= 31 {
					continue
				}
				_, _ = pc.WriteTo(pkt, net.UDPAddrFromAddrPort(netip.AddrPortFrom(subnetBroadcast(p), discovery.Port)))
			}
		}
	}()
}

func subnetBroadcast(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	m := net.CIDRMask(p.Bits(), 32)
	for i := range a {
		a[i] |= ^m[i]
	}
	return netip.AddrFrom4(a)
}

func (f *Finder) Status() DiscoveryStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := DiscoveryStatus{Enabled: f.discover, Network: f.fp, RelayID: f.id}
	_, st.KnownHome = f.homes[f.fp]
	st.KnownHome = st.KnownHome && f.fp != ""
	if f.static.IsValid() {
		st.Static = f.static.String()
	}
	st.Counters = f.stats
	st.Counters.Rejected = map[string]int64{}
	for k, v := range f.stats.Rejected {
		st.Counters.Rejected[k] = v
	}
	if f.cur.IsValid() {
		st.Address, st.Source = f.cur.String(), f.src
		st.Since = time.Since(f.since).Round(time.Second).String()
	} else {
		st.Explanation = "no signed beacon heard and this network is not a known home: no relay is contacted"
		if f.src != "" {
			st.Explanation += " (" + f.src + ")"
		}
	}
	if !f.lastHeard.IsZero() {
		st.LastHeard = time.Since(f.lastHeard).Round(time.Second).String() + " ago"
	}
	return st
}
