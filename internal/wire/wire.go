// Package wire is the client<->relay handshake: one authenticated request
// naming the destination, one authenticated reply, then raw bytes.
//
// It is deliberately not encryption. The payload is whatever the application
// sent (usually TLS already); the handshake only proves that both ends hold
// the pre-shared key, so the relay is not an open proxy and a LAN impostor
// cannot pose as the relay.
package wire

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"
)

var magic = [4]byte{'T', 'D', 'M', '1'}

const (
	nonceLen = 16
	macLen   = 32
	// MaxSkew bounds clock difference between the boxes. Both run NTP; the
	// window plus the nonce cache is what stops replays.
	MaxSkew = 120 * time.Second
)

// Status is the relay's answer.
type Status byte

const (
	StatusOK          Status = 0
	StatusDialFailed  Status = 1
	StatusRefused     Status = 2 // destination not allowed (private, etc.)
	StatusUnavailable Status = 3 // relay overloaded
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusDialFailed:
		return "upstream dial failed"
	case StatusRefused:
		return "destination refused by relay policy"
	case StatusUnavailable:
		return "relay overloaded"
	}
	return fmt.Sprintf("status(%d)", byte(s))
}

// Request is what the client sends. A zero Dest is a ping (health probe).
type Request struct {
	Time  time.Time
	Nonce [nonceLen]byte
	Dest  netip.AddrPort
}

func (r Request) IsPing() bool { return !r.Dest.IsValid() }

// NewRequest builds a request for dest (zero value = ping).
func NewRequest(dest netip.AddrPort) (Request, error) {
	r := Request{Time: time.Now(), Dest: dest}
	_, err := rand.Read(r.Nonce[:])
	return r, err
}

func (r Request) body() []byte {
	var b bytes.Buffer
	b.Write(magic[:])
	_ = binary.Write(&b, binary.BigEndian, r.Time.Unix())
	b.Write(r.Nonce[:])
	switch {
	case !r.Dest.IsValid():
		b.WriteByte(0)
	case r.Dest.Addr().Unmap().Is4():
		b.WriteByte(4)
		a := r.Dest.Addr().Unmap().As4()
		b.Write(a[:])
	default:
		b.WriteByte(6)
		a := r.Dest.Addr().As16()
		b.Write(a[:])
	}
	if r.Dest.IsValid() {
		_ = binary.Write(&b, binary.BigEndian, r.Dest.Port())
	}
	return b.Bytes()
}

// Marshal returns the request with its MAC.
func (r Request) Marshal(psk []byte) []byte {
	body := r.body()
	return append(body, mac(psk, body)...)
}

var (
	ErrBadMagic = errors.New("wire: bad magic")
	ErrBadMAC   = errors.New("wire: authentication failed")
	ErrStale    = errors.New("wire: timestamp outside allowed skew")
	ErrReplay   = errors.New("wire: replayed nonce")
)

// ReadRequest reads and authenticates a request. The caller sets deadlines.
func ReadRequest(rd io.Reader, psk []byte, replay *ReplayCache) (Request, error) {
	var r Request
	hdr := make([]byte, 4+8+nonceLen+1)
	if _, err := io.ReadFull(rd, hdr); err != nil {
		return r, err
	}
	if !bytes.Equal(hdr[:4], magic[:]) {
		return r, ErrBadMagic
	}
	var addrLen int
	switch hdr[len(hdr)-1] {
	case 0:
	case 4:
		addrLen = 4 + 2
	case 6:
		addrLen = 16 + 2
	default:
		return r, fmt.Errorf("wire: bad address family %d", hdr[len(hdr)-1])
	}
	rest := make([]byte, addrLen+macLen)
	if _, err := io.ReadFull(rd, rest); err != nil {
		return r, err
	}
	body := append(hdr, rest[:addrLen]...)
	if !hmac.Equal(mac(psk, body), rest[addrLen:]) {
		return r, ErrBadMAC
	}
	r.Time = time.Unix(int64(binary.BigEndian.Uint64(hdr[4:12])), 0)
	copy(r.Nonce[:], hdr[12:12+nonceLen])
	switch addrLen {
	case 6:
		r.Dest = netip.AddrPortFrom(netip.AddrFrom4([4]byte(rest[:4])), binary.BigEndian.Uint16(rest[4:6]))
	case 18:
		r.Dest = netip.AddrPortFrom(netip.AddrFrom16([16]byte(rest[:16])), binary.BigEndian.Uint16(rest[16:18]))
	}
	if d := time.Since(r.Time); d > MaxSkew || d < -MaxSkew {
		return r, ErrStale
	}
	if replay != nil && !replay.Add(r.Nonce, r.Time) {
		return r, ErrReplay
	}
	return r, nil
}

// MarshalReply returns the relay's authenticated reply to req.
func MarshalReply(psk []byte, req Request, st Status) []byte {
	body := replyBody(req, st)
	return append([]byte{byte(st)}, mac(psk, body)...)
}

// ReadReply reads and verifies the relay's reply to req.
func ReadReply(rd io.Reader, psk []byte, req Request) (Status, error) {
	buf := make([]byte, 1+macLen)
	if _, err := io.ReadFull(rd, buf); err != nil {
		return 0, err
	}
	st := Status(buf[0])
	if !hmac.Equal(mac(psk, replyBody(req, st)), buf[1:]) {
		return 0, ErrBadMAC
	}
	return st, nil
}

func replyBody(req Request, st Status) []byte {
	b := []byte("TDMR")
	b = append(b, req.Nonce[:]...)
	return append(b, byte(st))
}

func mac(psk, data []byte) []byte {
	h := hmac.New(sha256.New, psk)
	h.Write(data)
	return h.Sum(nil)
}

// ReplayCache remembers nonces seen within the skew window.
type ReplayCache struct {
	mu   sync.Mutex
	seen map[[nonceLen]byte]time.Time
	last time.Time
}

func NewReplayCache() *ReplayCache {
	return &ReplayCache{seen: make(map[[nonceLen]byte]time.Time)}
}

// Add records nonce; false means it was already seen.
func (c *ReplayCache) Add(n [nonceLen]byte, t time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.last) > 10*time.Second {
		for k, v := range c.seen {
			if now.Sub(v) > 2*MaxSkew+time.Minute {
				delete(c.seen, k)
			}
		}
		c.last = now
	}
	if _, dup := c.seen[n]; dup {
		return false
	}
	c.seen[n] = t
	return true
}
