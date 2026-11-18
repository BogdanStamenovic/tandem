// Package decide chooses, per destination, whether a connection goes direct
// or through the relay, from measured throughput of past connections.
//
// It knows nothing about sockets or nftables: callers feed it observations
// and ask it for choices. Throughput samples are a screen, not ground truth:
// a connection that downloads slowly may be slow because the application read
// slowly, so only bulk transfers are counted and the result is smoothed.
package decide

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Path int

const (
	Direct Path = iota
	Relay
)

func (p Path) String() string {
	if p == Relay {
		return "relay"
	}
	return "direct"
}

func (p Path) other() Path { return 1 - p }

// Key groups destinations: the /24 and the port.
type Key struct {
	Net  netip.Prefix
	Port uint16
}

func KeyFor(ap netip.AddrPort) Key {
	a := ap.Addr().Unmap()
	bits := 24
	if a.Is6() {
		bits = 48
	}
	p, _ := a.Prefix(bits)
	return Key{Net: p, Port: ap.Port()}
}

func (k Key) String() string { return fmt.Sprintf("%s:%d", k.Net, k.Port) }

// Stat is the smoothed bulk throughput of one path for one key.
type Stat struct {
	Rate float64   `json:"rate_bps"` // bytes per second, EWMA
	N    int       `json:"n"`
	Last time.Time `json:"last"`
}

func (s *Stat) add(rate float64, alpha float64, now time.Time) {
	if s.N == 0 {
		s.Rate = rate
	} else {
		s.Rate = alpha*rate + (1-alpha)*s.Rate
	}
	s.N++
	s.Last = now
}

// Config tunes the engine. Zero values are replaced by defaults.
type Config struct {
	Alpha       float64       // EWMA weight of a new sample
	MinSamples  int           // samples per path before a key's verdict is trusted
	Margin      float64       // relative advantage needed to switch from Default
	Explore     float64       // probability of trying the losing path on a decided key
	ExploreNew  float64       // probability of trying the unmeasured path on a half-known key
	StaleAfter  time.Duration // a loser's samples older than this get re-explored harder
	Default     Path          // path for keys with no evidence at all
	MaxKeys     int
	MinBytes    int64         // smallest transfer that counts as a sample
	MinDuration time.Duration // shortest active time that counts as a sample
}

func (c *Config) fill() {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.Alpha, 0.3)
	def(&c.Margin, 0.10)
	def(&c.Explore, 0.05)
	def(&c.ExploreNew, 0.25)
	if c.MinSamples == 0 {
		c.MinSamples = 2
	}
	if c.StaleAfter == 0 {
		c.StaleAfter = 6 * time.Hour
	}
	if c.MaxKeys == 0 {
		c.MaxKeys = 4096
	}
	if c.MinBytes == 0 {
		c.MinBytes = 1 << 20
	}
	if c.MinDuration == 0 {
		c.MinDuration = 300 * time.Millisecond
	}
}

type entry struct {
	Paths [2]Stat `json:"paths"` // indexed by Path
}

// Engine is safe for concurrent use.
type Engine struct {
	cfg    Config
	mu     sync.Mutex
	keys   map[Key]*entry
	global [2]Stat
	rnd    func() float64
	// frozen is set when Load found a newer on-disk format (after a
	// rollback). Save then leaves the file alone instead of downgrading it.
	frozen bool
}

func New(cfg Config) *Engine {
	cfg.fill()
	return &Engine{cfg: cfg, keys: make(map[Key]*entry), rnd: rand.Float64}
}

// Choice is a decision with the reason for it, for `tandem why`.
type Choice struct {
	Path    Path
	Reason  string
	Explore bool
}

// Choose picks a path for a new connection to k.
func (e *Engine) Choose(k Key) Choice { return e.choose(k, true) }

// Predict is Choose without exploration: what tandem would normally do.
func (e *Engine) Predict(k Key) Choice { return e.choose(k, false) }

func (e *Engine) choose(k Key, explore bool) Choice {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	roll := func(p float64) bool { return explore && e.rnd() < p }
	ent := e.keys[k]
	var st [2]Stat
	if ent != nil {
		st = ent.Paths
	}
	known := func(p Path) bool { return st[p].N >= e.cfg.MinSamples }

	switch {
	case known(Direct) && known(Relay):
		best := e.better(st[Direct].Rate, st[Relay].Rate)
		loser := best.other()
		p := e.cfg.Explore
		if now.Sub(st[loser].Last) > e.cfg.StaleAfter {
			p = e.cfg.ExploreNew
		}
		if roll(p) {
			return Choice{loser, fmt.Sprintf("re-exploring %s (%s vs %s)", loser, fmtRate(st[loser].Rate), fmtRate(st[best].Rate)), true}
		}
		return Choice{best, fmt.Sprintf("measured for %s: relay %s (n=%d) vs direct %s (n=%d)", k, fmtRate(st[Relay].Rate), st[Relay].N, fmtRate(st[Direct].Rate), st[Direct].N), false}
	case known(Direct) || known(Relay):
		have := Direct
		if known(Relay) {
			have = Relay
		}
		if roll(e.cfg.ExploreNew) {
			return Choice{have.other(), fmt.Sprintf("exploring %s: only %s measured for %s", have.other(), have, k), true}
		}
		// One side measured: fall back to the global comparison for the other.
		g := e.globalBest()
		return Choice{g.Path, fmt.Sprintf("only %s measured for %s (%s); %s", have, k, fmtRate(st[have].Rate), g.Reason), false}
	default:
		g := e.globalBest()
		if roll(e.cfg.ExploreNew) {
			return Choice{g.Path.other(), "exploring: no samples for " + k.String(), true}
		}
		return Choice{g.Path, "no samples for " + k.String() + "; " + g.Reason, false}
	}
}

func (e *Engine) better(direct, relay float64) Path {
	// Default path wins ties within Margin, so a noise-level difference
	// does not flip the choice back and forth.
	if e.cfg.Default == Relay {
		if direct > relay*(1+e.cfg.Margin) {
			return Direct
		}
		return Relay
	}
	if relay > direct*(1+e.cfg.Margin) {
		return Relay
	}
	return Direct
}

func (e *Engine) globalBest() Choice {
	g := e.global
	if g[Direct].N >= e.cfg.MinSamples && g[Relay].N >= e.cfg.MinSamples {
		p := e.better(g[Direct].Rate, g[Relay].Rate)
		return Choice{p, fmt.Sprintf("overall relay %s vs direct %s", fmtRate(g[Relay].Rate), fmtRate(g[Direct].Rate)), false}
	}
	return Choice{e.cfg.Default, "default " + e.cfg.Default.String(), false}
}

// Sample is one finished connection.
type Sample struct {
	Key    Key
	Path   Path
	Bytes  int64         // bytes in the busier direction
	Active time.Duration // time that direction was actually moving data
}

// Observe records a finished connection. It reports whether it counted.
func (e *Engine) Observe(s Sample) bool {
	if s.Bytes < e.cfg.MinBytes || s.Active < e.cfg.MinDuration {
		return false
	}
	rate := float64(s.Bytes) / s.Active.Seconds()
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	ent := e.keys[s.Key]
	if ent == nil {
		if len(e.keys) >= e.cfg.MaxKeys {
			e.evictLocked()
		}
		ent = &entry{}
		e.keys[s.Key] = ent
	}
	ent.Paths[s.Path].add(rate, e.cfg.Alpha, now)
	e.global[s.Path].add(rate, e.cfg.Alpha/3, now)
	return true
}

func (e *Engine) evictLocked() {
	var oldK Key
	var oldT time.Time
	first := true
	for k, v := range e.keys {
		t := v.Paths[0].Last
		if v.Paths[1].Last.After(t) {
			t = v.Paths[1].Last
		}
		if first || t.Before(oldT) {
			oldK, oldT, first = k, t, false
		}
	}
	delete(e.keys, oldK)
}

// KeyStats is one row of the table, for status output.
type KeyStats struct {
	Key    string `json:"key"`
	Direct Stat   `json:"direct"`
	Relay  Stat   `json:"relay"`
}

// Lookup returns the stats for k.
func (e *Engine) Lookup(k Key) (direct, relay Stat) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ent := e.keys[k]; ent != nil {
		return ent.Paths[Direct], ent.Paths[Relay]
	}
	return
}

// Global returns the overall stats.
func (e *Engine) Global() (direct, relay Stat) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.global[Direct], e.global[Relay]
}

// Top returns up to n keys, most recently used first.
func (e *Engine) Top(n int) []KeyStats {
	e.mu.Lock()
	out := make([]KeyStats, 0, len(e.keys))
	for k, v := range e.keys {
		out = append(out, KeyStats{k.String(), v.Paths[Direct], v.Paths[Relay]})
	}
	e.mu.Unlock()
	last := func(s KeyStats) time.Time {
		if s.Relay.Last.After(s.Direct.Last) {
			return s.Relay.Last
		}
		return s.Direct.Last
	}
	sort.Slice(out, func(i, j int) bool { return last(out[i]).After(last(out[j])) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

type persisted struct {
	Version int               `json:"version"`
	Global  [2]Stat           `json:"global"`
	Keys    map[string]*entry `json:"keys"`
}

// Save writes the learned state atomically.
func (e *Engine) Save(path string) error {
	e.mu.Lock()
	if e.frozen {
		e.mu.Unlock()
		return nil
	}
	p := persisted{Version: 1, Global: e.global, Keys: make(map[string]*entry, len(e.keys))}
	for k, v := range e.keys {
		c := *v
		p.Keys[k.String()] = &c
	}
	e.mu.Unlock()
	b, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads state written by Save. A missing file is not an error; an
// unreadable one is reported and ignored, since learned data is a cache.
func (e *Engine) Load(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("decide: %s: %w", path, err)
	}
	if p.Version != 1 {
		e.mu.Lock()
		e.frozen = p.Version > 1
		e.mu.Unlock()
		return fmt.Errorf("decide: %s: unknown version %d, ignoring", path, p.Version)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.global = p.Global
	for ks, v := range p.Keys {
		k, err := parseKey(ks)
		if err != nil || v == nil {
			continue
		}
		e.keys[k] = v
	}
	return nil
}

func parseKey(s string) (Key, error) {
	// "1.2.3.0/24:443": split on the last colon (IPv6 prefixes contain colons).
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return Key{}, fmt.Errorf("bad key %q", s)
	}
	p, err := netip.ParsePrefix(s[:i])
	if err != nil {
		return Key{}, err
	}
	var port int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil || port < 0 || port > 65535 {
		return Key{}, fmt.Errorf("bad port in %q", s)
	}
	return Key{p, uint16(port)}, nil
}

func fmtRate(bps float64) string {
	return fmt.Sprintf("%.0f Mbit/s", bps*8/1e6)
}

// FmtRate formats a bytes/s rate as Mbit/s.
func FmtRate(bps float64) string { return fmtRate(bps) }
