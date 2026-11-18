package client

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/netpolicy"
	"github.com/BogdanStamenovic/tandem/internal/pipe"
	"github.com/BogdanStamenovic/tandem/internal/wire"
)

const (
	relayDialTimeout = 300 * time.Millisecond // LAN; anything slower is "asleep"
	probeTimeout     = 500 * time.Millisecond
	maxProbeRTT      = 200 * time.Millisecond // a LAN relay slower than this is not worth using
	upAfter          = 2                      // consecutive good probes
	downAfter        = 2                      // consecutive bad probes or failed dials
)

// Health tracks whether the relay is reachable and responsive.
type Health struct {
	addr netip.AddrPort
	psk  []byte

	mu         sync.Mutex
	up         bool
	okStreak   int
	failStreak int
	rtt        time.Duration
	lastErr    string
	changed    time.Time
	kick       chan struct{}
	onChange   func(bool)
}

func NewHealth(addr netip.AddrPort, psk []byte, onChange func(bool)) *Health {
	return &Health{addr: addr, psk: psk, kick: make(chan struct{}, 1), changed: time.Now(), onChange: onChange}
}

type HealthStatus struct {
	Up      bool   `json:"up"`
	RTT     string `json:"rtt"`
	LastErr string `json:"last_error,omitempty"`
	Since   string `json:"since"`
}

func (h *Health) Status() HealthStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HealthStatus{h.up, h.rtt.Round(10 * time.Microsecond).String(), h.lastErr, time.Since(h.changed).Round(time.Second).String()}
}

func (h *Health) Up() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.up
}

// ReportFailure is called when a real connection could not reach the relay.
// Unlike a probe it marks the relay down at once: a LAN dial that times out
// or is refused is strong evidence, and waiting for a second failure let
// further connections pay the 300 ms dial timeout while the relay slept.
// A false alarm only means a few seconds of direct routing until two probes
// succeed again.
func (h *Health) ReportFailure(err error) {
	h.mu.Lock()
	h.failStreak = downAfter - 1
	h.mu.Unlock()
	h.record(false, 0, err)
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *Health) record(ok bool, rtt time.Duration, err error) {
	h.mu.Lock()
	was := h.up
	if ok {
		h.okStreak++
		h.failStreak = 0
		h.rtt = rtt
		h.lastErr = ""
		if h.okStreak >= upAfter {
			h.up = true
		}
	} else {
		h.failStreak++
		h.okStreak = 0
		if err != nil {
			h.lastErr = err.Error()
		}
		if h.failStreak >= downAfter {
			h.up = false
		}
	}
	now := h.up
	if was != now {
		h.changed = time.Now()
	}
	h.mu.Unlock()
	if was != now && h.onChange != nil {
		h.onChange(now)
	}
}

func (h *Health) probe() {
	start := time.Now()
	err := Ping(h.addr, h.psk, probeTimeout)
	rtt := time.Since(start)
	if err == nil && rtt > maxProbeRTT {
		err = errSlow{rtt}
	}
	h.record(err == nil, rtt, err)
}

type errSlow struct{ rtt time.Duration }

func (e errSlow) Error() string { return "relay too slow: probe took " + e.rtt.String() }

// Run probes forever: every 2 s while up, every 1 s while down.
func (h *Health) Run(stop <-chan struct{}) {
	for {
		h.probe()
		d := 2 * time.Second
		if !h.Up() {
			d = time.Second
		}
		select {
		case <-stop:
			return
		case <-time.After(d):
		case <-h.kick:
		}
	}
}

// Ping does one authenticated round trip to the relay.
func Ping(addr netip.AddrPort, psk []byte, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout, Control: pipe.MarkControl(netpolicy.OwnMark)}
	c, err := d.Dial("tcp4", addr.String())
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	req, err := wire.NewRequest(netip.AddrPort{})
	if err != nil {
		return err
	}
	if _, err := c.Write(req.Marshal(psk)); err != nil {
		return err
	}
	st, err := wire.ReadReply(c, psk, req)
	if err != nil {
		return err
	}
	if st != wire.StatusOK {
		return st2err(st)
	}
	return nil
}

type statusErr wire.Status

func (s statusErr) Error() string { return "relay: " + wire.Status(s).String() }

func st2err(s wire.Status) error { return statusErr(s) }
