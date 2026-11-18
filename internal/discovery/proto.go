// Package discovery is how a client finds its relay on the LAN without a
// configured address: the relay broadcasts signed beacons, and a client that
// just joined a network broadcasts a signed query that the relay answers at
// once. Both ends prove they hold the pairing key; nothing here is secret.
//
// Packets (UDP, port 7411, all integers big-endian):
//
//	beacon/answer: "TDB1" kind(1) time(8) nonce(16) relayID(8) ip(4) port(2) echo(16) mac(32)
//	query:         "TDQ1" time(8) nonce(16) mac(32)
//
// kind is 0 for a periodic beacon, 1 for an answer to a query; echo is the
// query's nonce in an answer and zero in a beacon. ip is the address the
// relay sent from, and the client only accepts a packet whose source address
// equals it, so a captured beacon replayed from another host is worthless.
// The MAC key is derived from the pairing key, never the key itself.
package discovery

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"time"
)

const (
	// Port is the UDP port beacons, queries and answers are sent to.
	Port = 7411
	// Interval between beacons.
	Interval = 5 * time.Second
	// MaxSkew bounds clock difference, as for the TCP handshake.
	MaxSkew = 120 * time.Second

	nonceLen  = 16
	macLen    = 32
	idLen     = 8
	beaconLen = 4 + 1 + 8 + nonceLen + idLen + 4 + 2 + nonceLen + macLen // 91
	queryLen  = 4 + 8 + nonceLen + macLen                                // 60
)

var (
	beaconMagic = [4]byte{'T', 'D', 'B', '1'}
	queryMagic  = [4]byte{'T', 'D', 'Q', '1'}
)

const (
	KindBeacon byte = 0
	KindAnswer byte = 1
)

var (
	ErrShort    = errors.New("discovery: short or unknown packet")
	ErrBadMAC   = errors.New("discovery: bad MAC (different key)")
	ErrStale    = errors.New("discovery: timestamp outside allowed skew")
	ErrReplay   = errors.New("discovery: replayed nonce")
	ErrMismatch = errors.New("discovery: claimed address differs from packet source")
)

// Key derives the discovery MAC key from the pairing key.
func Key(psk []byte) []byte {
	m := hmac.New(sha256.New, psk)
	m.Write([]byte("tandem discovery v1"))
	return m.Sum(nil)
}

// RelayID is a short stable identifier for a relay, from its key and host
// name. It only labels a relay in status output.
func RelayID(psk []byte, host string) [idLen]byte {
	m := hmac.New(sha256.New, psk)
	m.Write([]byte("tandem relay id\x00" + host))
	var id [idLen]byte
	copy(id[:], m.Sum(nil))
	return id
}

type Beacon struct {
	Kind  byte
	Time  time.Time
	Nonce [nonceLen]byte
	ID    [idLen]byte
	Addr  netip.AddrPort // the relay's TCP address: sending IP, listening port
	Echo  [nonceLen]byte
}

type Query struct {
	Time  time.Time
	Nonce [nonceLen]byte
}

func NewNonce() [nonceLen]byte {
	var n [nonceLen]byte
	_, _ = rand.Read(n[:])
	return n
}

func mac(key, b []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return m.Sum(nil)
}

// Marshal signs b with key.
func (b Beacon) Marshal(key []byte) []byte {
	out := make([]byte, 0, beaconLen)
	out = append(out, beaconMagic[:]...)
	out = append(out, b.Kind)
	out = binary.BigEndian.AppendUint64(out, uint64(b.Time.Unix()))
	out = append(out, b.Nonce[:]...)
	out = append(out, b.ID[:]...)
	a4 := b.Addr.Addr().As4()
	out = append(out, a4[:]...)
	out = binary.BigEndian.AppendUint16(out, b.Addr.Port())
	out = append(out, b.Echo[:]...)
	return append(out, mac(key, out)...)
}

func (q Query) Marshal(key []byte) []byte {
	out := make([]byte, 0, queryLen)
	out = append(out, queryMagic[:]...)
	out = binary.BigEndian.AppendUint64(out, uint64(q.Time.Unix()))
	out = append(out, q.Nonce[:]...)
	return append(out, mac(key, out)...)
}

func checkTime(t time.Time, now time.Time) error {
	if d := now.Sub(t); d > MaxSkew || d < -MaxSkew {
		return ErrStale
	}
	return nil
}

// ParseBeacon authenticates a beacon or answer received from src. The
// replay cache is checked last, so a forged packet cannot poison it.
func ParseBeacon(p []byte, key []byte, src netip.Addr, now time.Time, replay *Replay) (Beacon, error) {
	var b Beacon
	if len(p) != beaconLen || !bytes.Equal(p[:4], beaconMagic[:]) {
		return b, ErrShort
	}
	body := p[:beaconLen-macLen]
	if !hmac.Equal(mac(key, body), p[beaconLen-macLen:]) {
		return b, ErrBadMAC
	}
	b.Kind = p[4]
	b.Time = time.Unix(int64(binary.BigEndian.Uint64(p[5:13])), 0)
	copy(b.Nonce[:], p[13:29])
	copy(b.ID[:], p[29:37])
	b.Addr = netip.AddrPortFrom(netip.AddrFrom4([4]byte(p[37:41])), binary.BigEndian.Uint16(p[41:43]))
	copy(b.Echo[:], p[43:59])
	if err := checkTime(b.Time, now); err != nil {
		return b, err
	}
	if b.Addr.Addr() != src.Unmap() {
		return b, ErrMismatch
	}
	if replay != nil && !replay.Add(b.Nonce, b.Time) {
		return b, ErrReplay
	}
	return b, nil
}

func ParseQuery(p []byte, key []byte, now time.Time, replay *Replay) (Query, error) {
	var q Query
	if len(p) != queryLen || !bytes.Equal(p[:4], queryMagic[:]) {
		return q, ErrShort
	}
	body := p[:queryLen-macLen]
	if !hmac.Equal(mac(key, body), p[queryLen-macLen:]) {
		return q, ErrBadMAC
	}
	q.Time = time.Unix(int64(binary.BigEndian.Uint64(p[4:12])), 0)
	copy(q.Nonce[:], p[12:28])
	if err := checkTime(q.Time, now); err != nil {
		return q, err
	}
	if replay != nil && !replay.Add(q.Nonce, q.Time) {
		return q, ErrReplay
	}
	return q, nil
}

// Replay remembers nonces for twice the skew window; older packets are
// rejected as stale anyway. Bounded so a flood cannot grow it without limit.
type Replay struct {
	mu   sync.Mutex
	seen map[[nonceLen]byte]time.Time
}

const replayMax = 4096

func NewReplay() *Replay { return &Replay{seen: map[[nonceLen]byte]time.Time{}} }

// Add records n and reports whether it was new.
func (r *Replay) Add(n [nonceLen]byte, t time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[n]; ok {
		return false
	}
	if len(r.seen) >= replayMax {
		cut := time.Now().Add(-2 * MaxSkew)
		for k, v := range r.seen {
			if v.Before(cut) {
				delete(r.seen, k)
			}
		}
		if len(r.seen) >= replayMax {
			return false // flooded with fresh nonces: refuse rather than forget
		}
	}
	r.seen[n] = t
	return true
}
