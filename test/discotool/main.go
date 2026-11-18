// discotool sends and captures discovery packets for test/netns.sh.
//
//	discotool forge  -from IP -claim IP:PORT -to IP:PORT   # beacon signed with a random key
//	discotool sniff                                        # print the first beacon heard, hex
//	discotool send   -from IP -to IP:PORT -hex HEX         # send exact bytes (replay)
//	discotool verify -psk FILE -src IP -hex HEX            # check a captured beacon
//	discotool query  -psk FILE -from IP -to BCAST:PORT     # signed query, wait for the answer
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/discovery"
	"github.com/BogdanStamenovic/tandem/internal/pair"
	"golang.org/x/sys/unix"
)

func control(_, _ string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	})
}

func send(from, to string, p []byte) {
	d := net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP(from)}, Control: control}
	c, err := d.DialContext(context.Background(), "udp4", to)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(p); err != nil {
		log.Fatal(err)
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: discotool forge|sniff|send ...")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	from := fs.String("from", "", "source IP")
	to := fs.String("to", "", "destination IP:port")
	claim := fs.String("claim", "", "address the forged beacon claims")
	hx := fs.String("hex", "", "packet bytes")
	pskFile := fs.String("psk", "", "pairing key file")
	srcIP := fs.String("src", "", "source address the packet came from")
	_ = fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "forge":
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		b := discovery.Beacon{Kind: discovery.KindBeacon, Time: time.Now(), Nonce: discovery.NewNonce(),
			Addr: netip.MustParseAddrPort(*claim)}
		send(*from, *to, b.Marshal(discovery.Key(k)))
	case "send":
		p, err := hex.DecodeString(*hx)
		if err != nil {
			log.Fatal(err)
		}
		send(*from, *to, p)
	case "query":
		k, err := pair.Load(*pskFile)
		if err != nil {
			log.Fatal(err)
		}
		key := discovery.Key(k)
		lc := net.ListenConfig{Control: control}
		pc, err := lc.ListenPacket(context.Background(), "udp4", *from+":0")
		if err != nil {
			log.Fatal(err)
		}
		q := discovery.Query{Time: time.Now(), Nonce: discovery.NewNonce()}
		dst, _ := net.ResolveUDPAddr("udp4", *to)
		t0 := time.Now()
		if _, err := pc.WriteTo(q.Marshal(key), dst); err != nil {
			log.Fatal(err)
		}
		_ = pc.SetDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 512)
		for {
			n, src, err := pc.ReadFrom(buf)
			if err != nil {
				fmt.Println("no answer:", err)
				os.Exit(1)
			}
			b, err := discovery.ParseBeacon(buf[:n], key, src.(*net.UDPAddr).AddrPort().Addr(), time.Now(), nil)
			if err != nil || b.Kind != discovery.KindAnswer || b.Echo != q.Nonce {
				continue
			}
			fmt.Printf("answer from %s in %s: relay %s, id %x, nonce echoed\n", src, time.Since(t0).Round(time.Microsecond), b.Addr, b.ID)
			return
		}
	case "verify":
		p, err := hex.DecodeString(*hx)
		if err != nil {
			log.Fatal(err)
		}
		k, err := pair.Load(*pskFile)
		if err != nil {
			log.Fatal(err)
		}
		b, err := discovery.ParseBeacon(p, discovery.Key(k), netip.MustParseAddr(*srcIP), time.Now(), nil)
		if err != nil {
			fmt.Println("INVALID:", err)
			os.Exit(1)
		}
		fmt.Printf("valid: kind %d, relay %s, id %x, age %s\n", b.Kind, b.Addr, b.ID, time.Since(b.Time).Round(time.Second))
	case "sniff":
		lc := net.ListenConfig{Control: control}
		pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", discovery.Port))
		if err != nil {
			log.Fatal(err)
		}
		_ = pc.SetDeadline(time.Now().Add(15 * time.Second))
		buf := make([]byte, 512)
		for {
			n, src, err := pc.ReadFrom(buf)
			if err != nil {
				log.Fatal(err)
			}
			if n > 4 && string(buf[:4]) == "TDB1" && buf[4] == discovery.KindBeacon {
				fmt.Println(src.(*net.UDPAddr).IP, hex.EncodeToString(buf[:n]))
				return
			}
		}
	default:
		log.Fatal("usage: discotool forge|sniff|send ...")
	}
}
