// Package setup installs, upgrades and removes tandem on a machine: config,
// key, root-owned binary and systemd unit. ownbox's lifecycle commands call
// it through `tandem setup|upgrade|teardown`.
package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/client"
	"github.com/BogdanStamenovic/tandem/internal/config"
	"github.com/BogdanStamenovic/tandem/internal/pair"
)

const (
	BinDir  = "/usr/local/lib/tandem"
	BinPath = BinDir + "/tandem"
	unitDir = "/etc/systemd/system"
)

type Options struct {
	Role           string
	Relay          string // client: relay IP:port
	Listen         string // relay: listen IP:port
	Allow          string // relay: allowed source prefix(es), comma separated
	SSH            string // client: ssh target to pair with
	Key            string // client: key given directly
	Enable         string // "yes", "no" or "" (ask / decide)
	NonInteractive bool
	DryRun         bool
}

// fromEnv fills unset options from TANDEM_* variables, the non-interactive
// interface ownbox --yes runs use.
func (o *Options) fromEnv() {
	set := func(dst *string, env string) {
		if *dst == "" {
			*dst = os.Getenv(env)
		}
	}
	set(&o.Role, "TANDEM_ROLE")
	set(&o.Relay, "TANDEM_RELAY")
	set(&o.Listen, "TANDEM_LISTEN")
	set(&o.Allow, "TANDEM_ALLOW")
	set(&o.SSH, "TANDEM_PAIR_SSH")
	set(&o.Key, "TANDEM_PSK")
	set(&o.Enable, "TANDEM_ENABLE")
}

// relayHost is the machine name that defaults to the relay role and that
// clients pair with. It is identity, not mechanism, so it is configurable.
func relayHost() string {
	if h := os.Getenv("TANDEM_RELAY_HOST"); h != "" {
		return h
	}
	return "archserver"
}

func unitName(r config.Role) string { return "tandem-" + string(r) + ".service" }

type plan struct {
	a     *Asker
	o     Options
	steps []string
}

func (p *plan) do(desc string, fn func() error) error {
	p.steps = append(p.steps, desc)
	if p.o.DryRun {
		p.a.Say("  [dry-run] %s", desc)
		return nil
	}
	p.a.Say("  %s", desc)
	return fn()
}

// Install is `tandem setup`: interactive on a terminal, env/default driven
// otherwise. Rerunning it is safe; existing config and key are offered as
// the defaults.
func Install(o Options) error {
	o.fromEnv()
	a := NewAsker(o.NonInteractive)
	p := &plan{a: a, o: o}
	if os.Geteuid() != 0 && !o.DryRun {
		return errors.New("setup must run as root (the lifecycle script uses sudo)")
	}
	cfgPath := config.Path()
	old, _ := config.Load(cfgPath)
	host, _ := os.Hostname()
	mode := "interactive"
	if !a.Interactive {
		mode = "non-interactive: answers come from TANDEM_* variables or defaults"
	}
	a.Say("tandem setup on %s (%s)", host, mode)

	// Role.
	defRole := o.Role
	if defRole == "" && old != nil {
		defRole = string(old.Role)
	}
	if defRole == "" {
		defRole = "client"
		if strings.EqualFold(host, relayHost()) {
			defRole = "relay"
		}
	}
	if defRole == "server" {
		defRole = "relay"
	}
	if !a.Interactive && o.Role == "" && (old == nil) {
		a.Say("  role guessed from hostname: %s is %s the relay host (TANDEM_RELAY_HOST=%s)", host, map[bool]string{true: "", false: "not"}[defRole == "relay"], relayHost())
	}
	roleS, err := a.Choice("Is this machine the relay (server, dials out) or a client (the laptop)?", []string{"relay", "client"}, defRole)
	if err != nil {
		return err
	}
	cfg := &config.Config{Role: config.Role(strings.ToLower(roleS))}
	if old != nil && old.Role == cfg.Role {
		c := *old
		cfg = &c
	}

	var keyHex string
	if old != nil {
		if k, err := os.ReadFile(oldPSK(old)); err == nil {
			keyHex = strings.TrimSpace(string(k))
		}
	}
	var pingErr error = errors.New("not checked")

	switch cfg.Role {
	case config.RoleRelay:
		if err := askRelay(a, o, cfg, &keyHex); err != nil {
			return err
		}
		pingErr = nil
	case config.RoleClient:
		if err := askClient(a, o, cfg, &keyHex); err != nil {
			return err
		}
		if keyHex != "" {
			k, _ := pair.Parse(keyHex)
			start := time.Now()
			pingErr = client.Ping(netip.MustParseAddrPort(cfg.Relay), k, 2*time.Second)
			if pingErr == nil {
				a.Say("  relay %s answered an authenticated ping in %s", cfg.Relay, time.Since(start).Round(time.Millisecond/10))
			} else {
				a.Say("  relay %s did not answer an authenticated ping: %v", cfg.Relay, pingErr)
			}
		} else {
			pingErr = errors.New("no key")
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Enable now?
	enable := false
	switch strings.ToLower(o.Enable) {
	case "yes", "y", "1", "true":
		enable = true
	case "no", "n", "0", "false":
	default:
		def := keyHex != "" && pingErr == nil
		if cfg.Role == config.RoleClient && !a.Interactive {
			a.Say("  enable decision: %v (enabled only when the relay answered with this key; set TANDEM_ENABLE to override)", def)
		}
		q := "Start tandem now and at boot?"
		if cfg.Role == config.RoleClient {
			q = "Start tandem now and at boot? It redirects this machine's outbound TCP; stopping the service removes every rule"
		}
		if enable, err = a.YesNo(q, def); err != nil {
			return err
		}
	}
	if enable && keyHex == "" {
		return errors.New("cannot enable without a key")
	}

	// Apply.
	a.Say("Applying:")
	if keyHex != "" {
		if err := p.do("write key to "+cfg.PSKFile+" (0600 root)", func() error { return pair.Store(cfg.PSKFile, keyHex) }); err != nil {
			return err
		}
	}
	if err := p.do("write config "+cfgPath, func() error { return cfg.Save(cfgPath) }); err != nil {
		return err
	}
	if err := installFiles(p, cfg.Role); err != nil {
		return err
	}
	other := config.RoleRelay
	if cfg.Role == config.RoleRelay {
		other = config.RoleClient
	}
	if _, err := os.Stat(filepath.Join(unitDir, unitName(other))); err == nil {
		if err := p.do("remove the "+string(other)+" unit (role changed)", func() error { return removeUnit(other) }); err != nil {
			return err
		}
	}
	if enable {
		if err := p.do("systemctl enable "+unitName(cfg.Role)+" and (re)start it", func() error {
			if err := systemctl("enable", unitName(cfg.Role)); err != nil {
				return err
			}
			return systemctl("restart", unitName(cfg.Role))
		}); err != nil {
			return err
		}
	} else {
		a.Say("  not starting the service. Later: sudo systemctl enable --now %s", unitName(cfg.Role))
	}
	summary(a, cfg, enable)
	return nil
}

func oldPSK(c *config.Config) string { return c.PSKFile }

func askRelay(a *Asker, o Options, cfg *config.Config, keyHex *string) error {
	lanIP, lanNet := lanAddress()
	defListen := o.Listen
	if defListen == "" {
		defListen = cfg.Listen
	}
	if defListen == "" && lanIP.IsValid() {
		defListen = netip.AddrPortFrom(lanIP, config.DefaultRelayPort).String()
	}
	l, err := a.String("Listen address (use the LAN address, never 0.0.0.0)", defListen, func(v string) error {
		ap, err := netip.ParseAddrPort(v)
		if err != nil {
			return fmt.Errorf("need IP:port: %v", err)
		}
		if ap.Addr().IsUnspecified() {
			return errors.New("refusing to listen on every interface")
		}
		return nil
	})
	if err != nil {
		return err
	}
	cfg.Listen = l
	defAllow := o.Allow
	if defAllow == "" && len(cfg.Allow) > 0 {
		defAllow = strings.Join(cfg.Allow, ",")
	}
	if defAllow == "" && lanNet.IsValid() {
		defAllow = lanNet.String()
	}
	al, err := a.String("Client source addresses allowed (comma-separated prefixes)", defAllow, func(v string) error {
		for _, s := range strings.Split(v, ",") {
			if _, err := netip.ParsePrefix(strings.TrimSpace(s)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	cfg.Allow = nil
	for _, s := range strings.Split(al, ",") {
		cfg.Allow = append(cfg.Allow, strings.TrimSpace(s))
	}
	if cfg.PSKFile == "" {
		cfg.PSKFile = filepath.Join(config.DefaultDir, "psk")
	}
	newKey := *keyHex == ""
	if !newKey {
		keep, err := a.YesNo("Keep the existing key? (no = generate a new one; every client must pair again)", true)
		if err != nil {
			return err
		}
		newKey = !keep
	}
	if newKey {
		k, err := pair.Generate()
		if err != nil {
			return err
		}
		*keyHex = k
		a.Say("  generated a new key")
	}
	return nil
}

func askClient(a *Asker, o Options, cfg *config.Config, keyHex *string) error {
	if cfg.PSKFile == "" {
		cfg.PSKFile = filepath.Join(config.DefaultDir, "psk")
	}
	if o.Key != "" {
		if _, err := pair.Parse(o.Key); err != nil {
			return err
		}
		*keyHex = o.Key
	}
	pairNow := *keyHex == "" || o.SSH != ""
	if !pairNow && a.Interactive {
		again, err := a.YesNo("This machine is already paired with "+cfg.Relay+". Pair again?", false)
		if err != nil {
			return err
		}
		pairNow = again
	}
	defRelay := o.Relay
	if defRelay == "" {
		defRelay = cfg.Relay
	}
	if pairNow && o.Key == "" {
		target := o.SSH
		if target == "" {
			target = relayHost()
		}
		t, err := a.String("Pair over ssh with which host? (it runs `sudo -n tandem pair --export` there; type - to paste a key instead)", target, nil)
		if err != nil {
			return err
		}
		if t != "-" && t != "" {
			a.Say("  fetching address and key from %s over ssh...", t)
			ex, err := pair.FetchSSH(t)
			if err != nil {
				a.Say("  pairing over ssh failed: %v", err)
			} else {
				*keyHex = ex.Key
				if o.Relay == "" {
					defRelay = ex.Listen
				}
				a.Say("  got the key; relay listens on %s", ex.Listen)
			}
		}
		if *keyHex == "" && a.Interactive {
			k, err := a.String("Paste the key (on the relay: sudo tandem pair --show), or leave empty to skip", "", func(v string) error {
				if v == "" {
					return nil
				}
				_, err := pair.Parse(v)
				return err
			})
			if err != nil {
				return err
			}
			*keyHex = k
		}
		if *keyHex == "" {
			a.Say("  no key: the client will be installed but not started. Pair later with: sudo tandem pair --ssh %s", relayHost())
		}
	}
	r, err := a.String("Relay address (IP:port on the LAN)", defRelay, func(v string) error {
		ap, err := netip.ParseAddrPort(v)
		if err != nil {
			return fmt.Errorf("need IP:port: %v", err)
		}
		if !ap.Addr().IsPrivate() {
			return errors.New("the relay must be a private LAN address")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r == "" {
		return errors.New("no relay address: set TANDEM_RELAY or pair with TANDEM_PAIR_SSH")
	}
	cfg.Relay = r
	return nil
}

// lanAddress returns the source address and connected prefix used to reach
// the internet.
func lanAddress() (netip.Addr, netip.Prefix) {
	out, err := exec.Command("ip", "-j", "route", "get", "1.1.1.1").Output()
	if err != nil {
		return netip.Addr{}, netip.Prefix{}
	}
	var r []struct {
		Dev     string `json:"dev"`
		Prefsrc string `json:"prefsrc"`
	}
	if json.Unmarshal(out, &r) != nil || len(r) == 0 {
		return netip.Addr{}, netip.Prefix{}
	}
	ip, err := netip.ParseAddr(r[0].Prefsrc)
	if err != nil {
		return netip.Addr{}, netip.Prefix{}
	}
	ifc, err := net.InterfaceByName(r[0].Dev)
	if err != nil {
		return ip, netip.Prefix{}
	}
	addrs, _ := ifc.Addrs()
	for _, ad := range addrs {
		if p, err := netip.ParsePrefix(ad.String()); err == nil && p.Addr() == ip {
			return ip, p.Masked()
		}
	}
	return ip, netip.Prefix{}
}

func installFiles(p *plan, role config.Role) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := p.do("install binary "+BinPath+" (root-owned; the service never runs the user-writable checkout)", func() error {
		return installBinary(self)
	}); err != nil {
		return err
	}
	unit := clientUnit
	if role == config.RoleRelay {
		unit = relayUnit
	}
	path := filepath.Join(unitDir, unitName(role))
	return p.do("write "+path+" and reload systemd", func() error {
		if err := writeAtomic(path, []byte(unit), 0o644); err != nil {
			return err
		}
		return systemctl("daemon-reload")
	})
}

func installBinary(src string) error {
	if s, _ := filepath.EvalSymlinks(src); s == BinPath {
		return nil
	}
	if err := os.MkdirAll(BinDir, 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := BinPath + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Rename, not overwrite: a running daemon keeps its old inode.
	return os.Rename(tmp, BinPath)
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func unitState(role config.Role) (enabled, active bool) {
	enabled = exec.Command("systemctl", "is-enabled", "--quiet", unitName(role)).Run() == nil
	active = exec.Command("systemctl", "is-active", "--quiet", unitName(role)).Run() == nil
	return
}

func removeUnit(role config.Role) error {
	_ = exec.Command("systemctl", "disable", "--now", unitName(role)).Run()
	if err := os.Remove(filepath.Join(unitDir, unitName(role))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return systemctl("daemon-reload")
}

func summary(a *Asker, cfg *config.Config, enabled bool) {
	a.Say("")
	a.Say("Done. Role: %s.", cfg.Role)
	switch cfg.Role {
	case config.RoleRelay:
		a.Say("  Relay listens on %s for %s.", cfg.Listen, strings.Join(cfg.Allow, ", "))
		a.Say("  Pair a client by running `ownbox install tandem` on it (or: sudo tandem pair --ssh <this host>).")
	case config.RoleClient:
		a.Say("  Relay: %s. Check with: tandem status / tandem why HOST", cfg.Relay)
		a.Say("  Turn off at any time: tandem disable")
		a.Say("  Remove every rule by hand: sudo systemctl stop tandem-client; sudo nft delete table ip tandem")
	}
	if !enabled {
		a.Say("  Service is installed but not running.")
	}
}

// Upgrade is `tandem upgrade`, run by ownbox update: replace the binary and
// unit, keep config, key and learned data, restart the service if it was
// running. Without an existing config it is a fresh Install.
func Upgrade(o Options) error {
	cfg, err := config.Load(config.Path())
	if err != nil {
		fmt.Printf("no usable config (%v): running setup instead\n", err)
		return Install(o)
	}
	a := NewAsker(true)
	p := &plan{a: a, o: o}
	if os.Geteuid() != 0 && !o.DryRun {
		return errors.New("upgrade must run as root")
	}
	a.Say("tandem upgrade (%s): config, key and learned data in %s are kept", cfg.Role, cfg.StateDir)
	enabled, active := unitState(cfg.Role)
	if err := installFiles(p, cfg.Role); err != nil {
		return err
	}
	if active || enabled {
		if err := p.do("restart "+unitName(cfg.Role), func() error { return systemctl("restart", unitName(cfg.Role)) }); err != nil {
			return err
		}
	} else {
		a.Say("  %s was not running; left stopped", unitName(cfg.Role))
	}
	return nil
}

// Teardown is `tandem teardown`, run by ownbox remove: stop and remove the
// units, the binary and any rules. Config, key and learned data are only
// deleted when the user says so (or TANDEM_PURGE=1 without a terminal).
func Teardown(o Options, purge string) error {
	a := NewAsker(o.NonInteractive)
	p := &plan{a: a, o: o}
	if os.Geteuid() != 0 && !o.DryRun {
		return errors.New("teardown must run as root")
	}
	if purge == "" {
		purge = os.Getenv("TANDEM_PURGE")
	}
	a.Say("tandem teardown")
	for _, r := range []config.Role{config.RoleClient, config.RoleRelay} {
		if _, err := os.Stat(filepath.Join(unitDir, unitName(r))); err == nil {
			if err := p.do("stop, disable and delete "+unitName(r), func() error { return removeUnit(r) }); err != nil {
				return err
			}
		}
	}
	_ = p.do("delete nft table ip tandem if present", func() error {
		_ = exec.Command("nft", "delete", "table", "ip", "tandem").Run()
		return nil
	})
	if err := p.do("remove "+BinDir, func() error { return os.RemoveAll(BinDir) }); err != nil {
		return err
	}
	var del bool
	switch strings.ToLower(purge) {
	case "1", "yes", "y", "true":
		del = true
	case "0", "no", "n", "false":
	default:
		var err error
		del, err = a.YesNo("Also delete config, pairing key and learned routing data (/etc/tandem, /var/lib/tandem)?", false)
		if err != nil {
			return err
		}
	}
	if del {
		for _, d := range []string{config.DefaultDir, config.DefaultStateDir, "/var/lib/private/tandem"} {
			if err := p.do("delete "+d, func() error { return os.RemoveAll(d) }); err != nil {
				return err
			}
		}
	} else {
		a.Say("  kept /etc/tandem and /var/lib/tandem (delete with TANDEM_PURGE=1 or answer yes)")
	}
	return nil
}
