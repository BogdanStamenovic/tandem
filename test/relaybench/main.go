// relaybench is a load generator for measuring the relay alone, without the
// client daemon or nftables in the way. It speaks tandem's handshake itself.
//
//	relaybench serve -addr 203.0.113.10:5001            # the "internet" end
//	relaybench run -relay R -psk F -dest D -mode down   # through the relay
//	relaybench run -dest D -mode down                   # direct, for comparison
//
// Modes: down (server streams to client), up (client streams to server),
// rr (1-byte request/response round trips: latency under whatever else runs),
// conn (open, handshake, one round trip, close: per-connection cost).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/pair"
	"github.com/BogdanStamenovic/tandem/internal/wire"
)

var buf = make([]byte, 256<<10)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: relaybench serve|run ...")
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "run":
		run(os.Args[2:])
	default:
		log.Fatal("usage: relaybench serve|run ...")
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":5001", "listen address")
	_ = fs.Parse(args)
	ln, err := net.Listen("tcp4", *addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			var cmd [1]byte
			if _, err := io.ReadFull(c, cmd[:]); err != nil {
				return
			}
			switch cmd[0] {
			case 'D':
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			case 'U':
				_, _ = io.Copy(io.Discard, c)
			case 'E':
				b := make([]byte, 1)
				for {
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					if _, err := c.Write(b); err != nil {
						return
					}
				}
			}
		}(c)
	}
}

type opts struct {
	relay string
	psk   []byte
	dest  netip.AddrPort
}

// dial returns a connection to dest, through the relay if one is set.
func (o opts) dial() (net.Conn, error) {
	if o.relay == "" {
		return net.DialTimeout("tcp4", o.dest.String(), 5*time.Second)
	}
	c, err := net.DialTimeout("tcp4", o.relay, 5*time.Second)
	if err != nil {
		return nil, err
	}
	req, err := wire.NewRequest(o.dest)
	if err != nil {
		c.Close()
		return nil, err
	}
	if _, err := c.Write(req.Marshal(o.psk)); err != nil {
		c.Close()
		return nil, err
	}
	st, err := wire.ReadReply(c, o.psk, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if st != wire.StatusOK {
		c.Close()
		return nil, fmt.Errorf("relay: %v", st)
	}
	return c, nil
}

type result struct {
	Mode    string  `json:"mode"`
	Conns   int     `json:"conns"`
	Seconds float64 `json:"seconds"`
	Mbits   float64 `json:"mbit_s,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Ops     int     `json:"ops,omitempty"`
	OpsSec  float64 `json:"ops_s,omitempty"`
	P50ms   float64 `json:"p50_ms,omitempty"`
	P99ms   float64 `json:"p99_ms,omitempty"`
	Errors  int64   `json:"errors"`
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	relay := fs.String("relay", "", "relay address (empty: direct)")
	pskFile := fs.String("psk", "", "pre-shared key file")
	dest := fs.String("dest", "", "destination ip:port")
	mode := fs.String("mode", "down", "down|up|rr|conn")
	conns := fs.Int("conns", 1, "parallel connections")
	dur := fs.Duration("dur", 10*time.Second, "duration")
	_ = fs.Parse(args)
	o := opts{relay: *relay, dest: netip.MustParseAddrPort(*dest)}
	if *relay != "" {
		k, err := pair.Load(*pskFile)
		if err != nil {
			log.Fatal(err)
		}
		o.psk = k
	}
	var total atomic.Int64
	var errs atomic.Int64
	var mu sync.Mutex
	var lat []time.Duration
	deadline := time.Now().Add(*dur)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []time.Duration
			defer func() { mu.Lock(); lat = append(lat, mine...); mu.Unlock() }()
			if *mode == "conn" {
				for time.Now().Before(deadline) {
					t0 := time.Now()
					c, err := o.dial()
					if err != nil {
						errs.Add(1)
						continue
					}
					b := []byte{'E', 'x'}
					_, err = c.Write(b)
					if err == nil {
						_, err = io.ReadFull(c, b[:1])
					}
					c.Close()
					if err != nil {
						errs.Add(1)
						continue
					}
					mine = append(mine, time.Since(t0))
				}
				return
			}
			c, err := o.dial()
			if err != nil {
				errs.Add(1)
				log.Print(err)
				return
			}
			defer c.Close()
			_ = c.SetDeadline(deadline)
			switch *mode {
			case "down":
				_, _ = c.Write([]byte{'D'})
				rb := make([]byte, 256<<10)
				for {
					n, err := c.Read(rb)
					total.Add(int64(n))
					if err != nil {
						return
					}
				}
			case "up":
				_, _ = c.Write([]byte{'U'})
				for {
					n, err := c.Write(buf)
					total.Add(int64(n))
					if err != nil {
						return
					}
				}
			case "rr":
				_, _ = c.Write([]byte{'E'})
				b := []byte{'x'}
				for time.Now().Before(deadline) {
					t0 := time.Now()
					if _, err := c.Write(b); err != nil {
						return
					}
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					mine = append(mine, time.Since(t0))
				}
			}
		}()
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	r := result{Mode: *mode, Conns: *conns, Seconds: secs, Errors: errs.Load()}
	if b := total.Load(); b > 0 {
		r.Bytes = b
		r.Mbits = float64(b) * 8 / secs / 1e6
	}
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		r.Ops = len(lat)
		r.OpsSec = float64(len(lat)) / secs
		r.P50ms = float64(lat[len(lat)/2].Microseconds()) / 1000
		r.P99ms = float64(lat[len(lat)*99/100].Microseconds()) / 1000
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
}
