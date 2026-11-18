package wire

import (
	"bytes"
	"net/netip"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	psk := []byte("0123456789abcdef0123456789abcdef")
	for _, d := range []netip.AddrPort{{}, netip.MustParseAddrPort("1.2.3.4:443"), netip.MustParseAddrPort("[2001:db8::1]:80")} {
		req, _ := NewRequest(d)
		rc := NewReplayCache()
		got, err := ReadRequest(bytes.NewReader(req.Marshal(psk)), psk, rc)
		if err != nil {
			t.Fatal(err)
		}
		if got.Dest != d || got.Nonce != req.Nonce {
			t.Fatalf("got %v want %v", got.Dest, d)
		}
		if _, err := ReadRequest(bytes.NewReader(req.Marshal(psk)), psk, rc); err != ErrReplay {
			t.Fatalf("replay not detected: %v", err)
		}
		st, err := ReadReply(bytes.NewReader(MarshalReply(psk, got, StatusDialFailed)), psk, req)
		if err != nil || st != StatusDialFailed {
			t.Fatal(st, err)
		}
	}
}

func TestRejects(t *testing.T) {
	psk := []byte("0123456789abcdef0123456789abcdef")
	req, _ := NewRequest(netip.MustParseAddrPort("1.2.3.4:443"))
	if _, err := ReadRequest(bytes.NewReader(req.Marshal([]byte("wrong"))), psk, nil); err != ErrBadMAC {
		t.Fatalf("wrong key accepted: %v", err)
	}
	b := req.Marshal(psk)
	b[len(b)-40] ^= 1 // flip a port bit
	if _, err := ReadRequest(bytes.NewReader(b), psk, nil); err != ErrBadMAC {
		t.Fatalf("tampered request accepted: %v", err)
	}
	old := req
	old.Time = time.Now().Add(-10 * time.Minute)
	if _, err := ReadRequest(bytes.NewReader(old.Marshal(psk)), psk, nil); err != ErrStale {
		t.Fatalf("stale request accepted: %v", err)
	}
	if _, err := ReadReply(bytes.NewReader(MarshalReply([]byte("x"), req, StatusOK)), psk, req); err != ErrBadMAC {
		t.Fatalf("forged reply accepted: %v", err)
	}
}
