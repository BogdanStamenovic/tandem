package discovery

import (
	"net/netip"
	"testing"
	"time"
)

func TestBeaconRoundTrip(t *testing.T) {
	psk := []byte("0123456789abcdef0123456789abcdef")
	key := Key(psk)
	now := time.Now()
	src := netip.MustParseAddr("192.168.1.139")
	b := Beacon{Kind: KindBeacon, Time: now, Nonce: NewNonce(), ID: RelayID(psk, "archserver"),
		Addr: netip.AddrPortFrom(src, 7411)}
	p := b.Marshal(key)
	if len(p) != beaconLen {
		t.Fatalf("len %d", len(p))
	}
	r := NewReplay()
	got, err := ParseBeacon(p, key, src, now, r)
	if err != nil || got.Addr != b.Addr || got.ID != b.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := ParseBeacon(p, key, src, now, r); err != ErrReplay {
		t.Fatalf("replay: %v", err)
	}
	if _, err := ParseBeacon(p, Key([]byte("another key entirely, 32 bytes!!")), src, now, NewReplay()); err != ErrBadMAC {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := ParseBeacon(p, key, netip.MustParseAddr("192.168.1.66"), now, NewReplay()); err != ErrMismatch {
		t.Fatalf("other source: %v", err)
	}
	if _, err := ParseBeacon(p, key, src, now.Add(3*time.Minute), NewReplay()); err != ErrStale {
		t.Fatalf("stale: %v", err)
	}
	p[50] ^= 1
	if _, err := ParseBeacon(p, key, src, now, NewReplay()); err != ErrBadMAC {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := ParseBeacon(p[:20], key, src, now, NewReplay()); err != ErrShort {
		t.Fatalf("short: %v", err)
	}
}

func TestQuery(t *testing.T) {
	key := Key([]byte("0123456789abcdef0123456789abcdef"))
	q := Query{Time: time.Now(), Nonce: NewNonce()}
	p := q.Marshal(key)
	r := NewReplay()
	if got, err := ParseQuery(p, key, time.Now(), r); err != nil || got.Nonce != q.Nonce {
		t.Fatalf("%v", err)
	}
	if _, err := ParseQuery(p, key, time.Now(), r); err != ErrReplay {
		t.Fatalf("replay: %v", err)
	}
	// A beacon is not a query and vice versa.
	if _, err := ParseBeacon(p, key, netip.MustParseAddr("10.0.0.1"), time.Now(), nil); err != ErrShort {
		t.Fatalf("query as beacon: %v", err)
	}
}

func TestKeyIsNotPSK(t *testing.T) {
	psk := []byte("0123456789abcdef0123456789abcdef")
	if string(Key(psk)) == string(psk) {
		t.Fatal("discovery key equals the pairing key")
	}
}
