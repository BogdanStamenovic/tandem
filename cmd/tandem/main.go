// Command tandem relays a laptop's TCP connections through a faster machine
// on the same LAN when that is measurably faster. See README.md.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/client"
	"github.com/BogdanStamenovic/tandem/internal/config"
	"github.com/BogdanStamenovic/tandem/internal/control"
	"github.com/BogdanStamenovic/tandem/internal/decide"
	"github.com/BogdanStamenovic/tandem/internal/netpolicy"
	"github.com/BogdanStamenovic/tandem/internal/nftrules"
	"github.com/BogdanStamenovic/tandem/internal/pair"
	"github.com/BogdanStamenovic/tandem/internal/relay"
	"github.com/BogdanStamenovic/tandem/internal/setup"
)

// Exit codes: 0 ok, 1 runtime failure, 2 usage error, 3 daemon not running.
const (
	exitFail     = 1
	exitUsage    = 2
	exitNoDaemon = 3
)

var version = "dev"

const usage = `tandem - relay TCP through a faster LAN machine when it is faster

Usage:
  tandem status [--json]            what the local daemon is doing
  tandem why HOST [--port N]        which path connections to HOST take, and why
  tandem disable [--dry-run]        turn redirection off (persists across reboots)
  tandem enable [--dry-run]         turn it back on
  tandem rules                      print the nft rules the client installs
  tandem force relay|direct|auto    pin new connections to one path (benchmarks; resets on restart)
  tandem pair --ssh HOST            (client, root) fetch address and key from the relay
  tandem pair --key HEX             (client, root) store a key given by hand
  tandem pair --show | --export     (relay, root) print the key / address+key as JSON
  tandem pair --generate            (relay, root) new key; every client must pair again
  tandem setup [flags]              install or reconfigure (ownbox install runs this)
  tandem upgrade                    replace binary and unit, keep state (ownbox update)
  tandem teardown [--purge yes|no]  remove units, binary and rules (ownbox remove)
  tandem run                        the daemon (systemd runs this)
  tandem version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "why":
		err = cmdWhy(args)
	case "disable", "enable":
		err = cmdToggle(cmd, args)
	case "rules":
		err = cmdRules(args)
	case "force":
		if len(args) != 1 {
			err = usageErr{errors.New("usage: tandem force relay|direct|auto")}
			break
		}
		var ch client.Change
		if err = call(control.Request{Cmd: "force", IP: args[0]}, &ch); err == nil {
			fmt.Println(ch.Message)
		}
	case "pair":
		err = cmdPair(args)
	case "setup", "upgrade", "teardown":
		err = cmdSetup(cmd, args)
	case "version", "--version":
		fmt.Println("tandem", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "tandem: unknown command %q\n\n%s", cmd, usage)
		os.Exit(exitUsage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tandem:", err)
		var u usageErr
		switch {
		case errors.As(err, &u):
			os.Exit(exitUsage)
		case errors.Is(err, errNoDaemon):
			os.Exit(exitNoDaemon)
		}
		os.Exit(exitFail)
	}
}

type usageErr struct{ error }

var errNoDaemon = errors.New("daemon not running")

func parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return usageErr{err}
	}
	return nil
}

func loadConfig() (*config.Config, error) {
	c, err := config.Load(config.Path())
	if err != nil {
		return nil, fmt.Errorf("%w (not set up? run `tandem setup`)", err)
	}
	return c, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	psk := fs.String("psk-file", "", "override the key file (systemd credentials)")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *psk != "" {
		cfg.PSKFile = *psk
	}
	if cfg.Role == config.RoleRelay {
		return relay.Run(cfg)
	}
	return client.Run(cfg)
}

func runDir() string {
	if c, err := config.Load(config.Path()); err == nil {
		return c.RunDir
	}
	return config.DefaultRunDir
}

func call(req control.Request, out any) error {
	resp, err := control.Call(runDir(), req)
	if err != nil {
		return fmt.Errorf("%w (%v); is the service running? systemctl status 'tandem-*'", errNoDaemon, err)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return json.Unmarshal(resp.Data, out)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "raw JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := call(control.Request{Cmd: "status"}, &raw); err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var probe struct{ Role string }
	_ = json.Unmarshal(raw, &probe)
	if probe.Role == "relay" {
		var s relay.Status
		_ = json.Unmarshal(raw, &s)
		fmt.Printf("tandem relay on %s (allow %s), up %s\n", s.Listen, strings.Join(s.Allow, ","), s.Uptime)
		fmt.Printf("  connections: %d active, %d total; %d pings\n", s.Active, s.Total, s.Pings)
		fmt.Printf("  rejected: %d auth, %d refused destinations, %d upstream dial failures\n", s.AuthFailures, s.Refused, s.DialFailures)
		fmt.Printf("  bytes: %s to clients, %s upstream\n", human(s.BytesToClient), human(s.BytesUpstream))
		fmt.Printf("  discovery: beacons from %s (%d sent), %d queries answered, %d rejected\n",
			strings.Join(s.BeaconAddrs, ", "), s.BeaconsSent, s.QueriesAnswered, s.QueriesRejected)
		return nil
	}
	var s client.Status
	_ = json.Unmarshal(raw, &s)
	state := "ACTIVE: redirecting new TCP connections on " + s.Uplink
	if !s.Active {
		state = "inactive (all traffic direct): " + s.Inactive
	}
	fmt.Printf("tandem client, up %s\n  %s\n", s.Uptime, state)
	if s.Forced != "" {
		fmt.Printf("  FORCED: every eligible connection goes %s (tandem force auto)\n", s.Forced)
	}
	h := "down"
	if s.Health.Up {
		h = "up, rtt " + s.Health.RTT
	}
	ds := s.Discovery
	if s.Relay == "" {
		fmt.Printf("  relay: none on this network: %s\n", ds.Explanation)
	} else {
		fmt.Printf("  relay %s (from %s", s.Relay, ds.Source)
		if ds.LastHeard != "" && (ds.Source == "beacon" || ds.Source == "query") {
			fmt.Printf(", last beacon %s", ds.LastHeard)
		}
		fmt.Printf("): %s (for %s)", h, s.Health.Since)
		if s.Health.LastErr != "" {
			fmt.Printf(", last error: %s", s.Health.LastErr)
		}
		fmt.Println()
	}
	if ds.Enabled {
		var rej []string
		for k, v := range ds.Counters.Rejected {
			rej = append(rej, fmt.Sprintf("%s %d", k, v))
		}
		sort.Strings(rej)
		home := "unknown network"
		if ds.KnownHome {
			home = "known home network"
		}
		fmt.Printf("  discovery: %s; %d beacons, %d answers to %d queries; rejected: %s; static fallback %s\n",
			home, ds.Counters.Beacons, ds.Counters.Answers, ds.Counters.Queries, orNone(strings.Join(rej, ", ")), orNone(ds.Static))
	} else {
		fmt.Println("  discovery: off (\"discovery\": false); the static address is used everywhere")
	}
	if s.RuleError != "" {
		fmt.Printf("  rule error: %s\n", s.RuleError)
	}
	c := s.Conns
	fmt.Printf("  connections: relay %d active / %d total, direct %d active / %d total, %d fell back to direct, %d failed\n",
		c["relay_active"], c["relay_total"], c["direct_active"], c["direct_total"], c["fallbacks"], c["failed"])
	fmt.Printf("  bytes: relay %s, direct %s\n", human(s.Bytes["relay"]), human(s.Bytes["direct"]))
	fmt.Printf("  learned overall: relay %s, direct %s\n", s.GlobalRate["relay"], s.GlobalRate["direct"])
	if len(s.Top) > 0 {
		fmt.Println("\n  destination (/24:port)       relay              direct")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, k := range s.Top {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", k.Key, statStr(k.Relay), statStr(k.Direct))
		}
		tw.Flush()
	}
	if len(s.Recent) > 0 {
		fmt.Println("\n  recent connections:")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, r := range s.Recent {
			extra := r.Reason
			if r.Fallback != "" {
				extra = "fell back: " + r.Fallback
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.At.Local().Format("15:04:05"), r.Dest, r.Path, human(r.Bytes), r.Rate, extra)
		}
		tw.Flush()
	}
	return nil
}

func statStr(s decide.Stat) string {
	if s.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%s (n=%d)", decide.FmtRate(s.Rate), s.N)
}

func human(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KiB", "MiB", "GiB"} {
		if f < 1024 || u == "GiB" {
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return ""
}

func cmdWhy(args []string) error {
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	port := fs.Uint("port", 443, "destination port")
	asJSON := fs.Bool("json", false, "raw JSON")
	// Allow `tandem why HOST --port 80` as well as flags first.
	var host string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if host == "" && fs.NArg() > 0 {
		host = fs.Arg(0)
	}
	if host == "" {
		return usageErr{errors.New("usage: tandem why HOST [--port N]")}
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		fmt.Sscanf(p, "%d", port)
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		ips, err := net.LookupIP(host)
		if err != nil {
			return err
		}
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip); ok {
				addrs = append(addrs, a.Unmap())
			}
		}
	}
	for _, a := range addrs {
		var w client.Why
		if err := call(control.Request{Cmd: "why", IP: a.String(), Port: uint16(*port)}, &w); err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.Marshal(w)
			fmt.Println(string(b))
			continue
		}
		fmt.Printf("%s (%s) -> %s\n  %s\n", w.Dest, host, strings.ToUpper(w.Verdict), w.Reason)
		if w.Relay.N+w.Direct.N > 0 {
			fmt.Printf("  measured for %s: relay %s, direct %s\n", w.Key, statStr(w.Relay), statStr(w.Direct))
		}
		for _, r := range w.Recent {
			fmt.Printf("  %s %s via %s, %s %s\n", r.At.Local().Format("15:04:05"), r.Dest, r.Path, human(r.Bytes), r.Rate)
		}
	}
	return nil
}

func cmdToggle(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show what would change")
	if err := parse(fs, args); err != nil {
		return err
	}
	var ch client.Change
	err := call(control.Request{Cmd: cmd, DryRun: *dry}, &ch)
	if errors.Is(err, errNoDaemon) {
		// No daemon: flip the flag file directly so it holds at next start.
		cfg, cerr := loadConfig()
		if cerr != nil {
			return err
		}
		path := cfg.StateDir + "/disabled"
		if *dry {
			fmt.Printf("daemon not running; would %s %s\n", map[bool]string{true: "create", false: "delete"}[cmd == "disable"], path)
			return nil
		}
		if cmd == "disable" {
			err = os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
		} else if err = os.Remove(path); os.IsNotExist(err) {
			err = nil
		}
		if err != nil {
			return fmt.Errorf("daemon not running and cannot write %s: %w (try sudo)", path, err)
		}
		fmt.Printf("daemon not running; %sd for its next start\n", cmd)
		return nil
	}
	if err != nil {
		return err
	}
	prefix := ""
	if ch.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Println(prefix + ch.Message)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func cmdRules(args []string) error {
	var ch client.Change
	if err := call(control.Request{Cmd: "rules"}, &ch); err == nil {
		fmt.Print(ch.Rules)
		return nil
	}
	// No daemon: render from config, with a placeholder uplink.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	byp := append([]netip.Prefix{}, netpolicy.Bypass...)
	if ap, err := netip.ParseAddrPort(cfg.Relay); err == nil {
		byp = append(byp, netip.PrefixFrom(ap.Addr(), 32))
	}
	fmt.Println("# daemon not running; uplink shown as UPLINK")
	fmt.Print(nftrules.Text(nftrules.Spec{Port: cfg.LocalPort, Uplink: "UPLINK", Bypass: byp,
		BypassPorts: append(append([]uint16{}, netpolicy.BypassPorts...), cfg.BypassPorts...),
		SkipMarks:   [][2]uint32{{netpolicy.OwnMark, netpolicy.OwnMask}, {netpolicy.TailscaleMark, netpolicy.TailscaleMask}}}))
	return nil
}

func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	sshT := fs.String("ssh", "", "client: ssh target of the relay")
	key := fs.String("key", "", "client: key in hex")
	show := fs.Bool("show", false, "relay: print the key")
	export := fs.Bool("export", false, "relay: print listen address and key as JSON")
	gen := fs.Bool("generate", false, "relay: generate a new key")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	restart := func() error {
		unit := "tandem-" + string(cfg.Role) + ".service"
		if exec("systemctl", "is-active", "--quiet", unit) == nil {
			fmt.Println("restarting", unit)
			return exec("systemctl", "restart", unit)
		}
		return nil
	}
	switch {
	case *show || *export:
		k, err := pair.Load(cfg.PSKFile)
		if err != nil {
			return err
		}
		hexKey := fmt.Sprintf("%x", k)
		if *show {
			fmt.Println(hexKey)
			return nil
		}
		if cfg.Role != config.RoleRelay {
			return errors.New("--export only makes sense on the relay")
		}
		return json.NewEncoder(os.Stdout).Encode(pair.Export{Listen: relay.Advertised(cfg), Key: hexKey})
	case *gen:
		if cfg.Role != config.RoleRelay {
			return errors.New("--generate is for the relay; clients pair with --ssh or --key")
		}
		k, err := pair.Generate()
		if err != nil {
			return err
		}
		if err := pair.Store(cfg.PSKFile, k); err != nil {
			return err
		}
		fmt.Println("new key stored; every client must pair again")
		return restart()
	case *sshT != "" || *key != "":
		if cfg.Role != config.RoleClient {
			return errors.New("pairing with a relay is for clients")
		}
		k := *key
		if *sshT != "" {
			ex, err := pair.FetchSSH(*sshT)
			if err != nil {
				return err
			}
			k = ex.Key
			if ap, err := netip.ParseAddrPort(ex.Listen); err == nil && !ap.Addr().IsUnspecified() && ex.Listen != cfg.Relay {
				fmt.Printf("relay address %s -> %s\n", cfg.Relay, ex.Listen)
				cfg.Relay = ex.Listen
				if err := cfg.Save(config.Path()); err != nil {
					return err
				}
			}
		}
		if err := pair.Store(cfg.PSKFile, k); err != nil {
			return err
		}
		kb, _ := pair.Parse(k)
		if cfg.Relay == "" {
			fmt.Println("stored; no static address, the client finds the relay by beacons")
		} else if err := client.Ping(netip.MustParseAddrPort(cfg.Relay), kb, 2*time.Second); err != nil {
			fmt.Printf("stored, but the relay did not answer: %v\n", err)
		} else {
			fmt.Println("paired; relay answered")
			// It answered here, so this network is one where the static
			// address may be used if beacons are not heard.
			if fp, err := client.RecordHome(cfg.StateDir, netip.MustParseAddrPort(cfg.Relay)); err == nil {
				fmt.Printf("recorded this network as home: %s\n", fp)
			} else {
				fmt.Printf("could not record this network: %v (beacons still work)\n", err)
			}
		}
		return restart()
	}
	return usageErr{errors.New("pair: give --ssh, --key, --show, --export or --generate")}
}

func cmdSetup(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var o setup.Options
	fs.StringVar(&o.Role, "role", "", "relay or client")
	fs.StringVar(&o.Relay, "relay", "", "client: relay IP:port")
	fs.StringVar(&o.Listen, "listen", "", "relay: listen IP:port")
	fs.StringVar(&o.Allow, "allow", "", "relay: allowed client prefixes, comma separated")
	fs.StringVar(&o.SSH, "ssh", "", "client: pair over ssh with this host")
	fs.StringVar(&o.Key, "key", "", "client: key in hex")
	fs.StringVar(&o.Enable, "enable", "", "start now and at boot: yes or no")
	fs.BoolVar(&o.NonInteractive, "non-interactive", false, "never prompt, even on a terminal")
	fs.BoolVar(&o.DryRun, "dry-run", false, "print what would be done")
	purge := fs.String("purge", "", "teardown: also delete config and learned data (yes/no)")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch cmd {
	case "setup":
		return setup.Install(o)
	case "upgrade":
		return setup.Upgrade(o)
	}
	return setup.Teardown(o, *purge)
}
