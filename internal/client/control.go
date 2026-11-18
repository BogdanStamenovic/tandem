package client

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/BogdanStamenovic/tandem/internal/control"
	"github.com/BogdanStamenovic/tandem/internal/decide"
	"github.com/BogdanStamenovic/tandem/internal/netpolicy"
	"github.com/BogdanStamenovic/tandem/internal/nftrules"
)

type Status struct {
	Role       string            `json:"role"`
	Relay      string            `json:"relay"`
	Uptime     string            `json:"uptime"`
	Active     bool              `json:"active"`
	Inactive   string            `json:"inactive_reason,omitempty"`
	Disabled   bool              `json:"disabled"`
	Uplink     string            `json:"uplink,omitempty"`
	VPN        string            `json:"vpn,omitempty"`
	RuleError  string            `json:"rule_error,omitempty"`
	NetError   string            `json:"net_error,omitempty"`
	Health     HealthStatus      `json:"relay_health"`
	Conns      map[string]int64  `json:"connections"`
	Bytes      map[string]int64  `json:"bytes"`
	GlobalRate map[string]string `json:"global_rate"`
	Top        []decide.KeyStats `json:"top_destinations"`
	Recent     []ConnRecord      `json:"recent"`
}

func (d *Daemon) status() Status {
	ok, why := d.want()
	installed, _ := d.rules.Installed()
	d.mu.Lock()
	ns, rerr, nerr := d.net, d.ruleErr, d.netErr
	recent := append([]ConnRecord(nil), d.recent...)
	d.mu.Unlock()
	if len(recent) > 15 {
		recent = recent[len(recent)-15:]
	}
	gd, gr := d.eng.Global()
	if ok && !installed {
		why = "rules not installed yet"
	}
	return Status{
		Role: "client", Relay: d.relay.String(),
		Uptime: time.Since(d.started).Round(time.Second).String(),
		Active: installed, Inactive: why, Disabled: d.disabled(),
		Uplink: ns.Uplink, VPN: ns.VPN, RuleError: rerr, NetError: nerr,
		Health: d.health.Status(),
		Conns: map[string]int64{
			"relay_active": d.active[decide.Relay].Load(), "direct_active": d.active[decide.Direct].Load(),
			"relay_total": d.total[decide.Relay].Load(), "direct_total": d.total[decide.Direct].Load(),
			"fallbacks": d.fallback.Load(), "failed": d.failed.Load(),
		},
		Bytes: map[string]int64{"relay": d.bytes[decide.Relay].Load(), "direct": d.bytes[decide.Direct].Load()},
		GlobalRate: map[string]string{
			"relay":  fmt.Sprintf("%s (n=%d)", decide.FmtRate(gr.Rate), gr.N),
			"direct": fmt.Sprintf("%s (n=%d)", decide.FmtRate(gd.Rate), gd.N),
		},
		Top:    d.eng.Top(10),
		Recent: recent,
	}
}

type Why struct {
	Dest    string       `json:"dest"`
	Key     string       `json:"key"`
	Verdict string       `json:"verdict"`
	Reason  string       `json:"reason"`
	Direct  decide.Stat  `json:"direct"`
	Relay   decide.Stat  `json:"relay"`
	Recent  []ConnRecord `json:"recent"`
}

func (d *Daemon) why(ap netip.AddrPort) Why {
	key := decide.KeyFor(ap)
	w := Why{Dest: ap.String(), Key: key.String()}
	w.Direct, w.Relay = d.eng.Lookup(key)
	installed, _ := d.rules.Installed()
	ok, why := d.want()
	switch {
	case !netpolicy.IsPublic(ap.Addr()):
		w.Verdict, w.Reason = "direct", "private, loopback, tailnet or non-IPv4 address: never relayed"
	case netpolicy.IsBypassPort(ap.Port()) || contains(d.bypassPorts, ap.Port()):
		w.Verdict, w.Reason = "direct", fmt.Sprintf("port %d is on the bypass list", ap.Port())
	case !ok:
		w.Verdict, w.Reason = "direct", "tandem is inactive: "+why
	case !installed:
		w.Verdict, w.Reason = "direct", "rules not installed yet"
	default:
		c := d.eng.Predict(key)
		w.Verdict, w.Reason = c.Path.String(), c.Reason
	}
	d.mu.Lock()
	for _, r := range d.recent {
		if rap, err := netip.ParseAddrPort(r.Dest); err == nil && decide.KeyFor(rap) == key {
			w.Recent = append(w.Recent, r)
		}
	}
	d.mu.Unlock()
	return w
}

func contains(ps []uint16, p uint16) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

type Change struct {
	DryRun  bool   `json:"dry_run"`
	Changed bool   `json:"changed"`
	Message string `json:"message"`
	Rules   string `json:"rules,omitempty"`
}

func (d *Daemon) handleControl(req control.Request, uid int) (any, error) {
	switch req.Cmd {
	case "status":
		return d.status(), nil
	case "why":
		a, err := netip.ParseAddr(req.IP)
		if err != nil {
			return nil, err
		}
		port := req.Port
		if port == 0 {
			port = 443
		}
		return d.why(netip.AddrPortFrom(a, port)), nil
	case "rules":
		d.mu.Lock()
		up := d.net.Uplink
		d.mu.Unlock()
		return Change{DryRun: true, Rules: nftrules.Text(d.spec(up))}, nil
	case "disable", "enable":
		// Any local user may toggle tandem: disabling only makes traffic go
		// direct, and this is a single-user laptop.
		path := filepath.Join(d.cfg.StateDir, disabledFile)
		was := d.disabled()
		want := req.Cmd == "disable"
		ch := Change{DryRun: req.DryRun, Changed: was != want}
		switch {
		case !ch.Changed:
			ch.Message = "already " + req.Cmd + "d"
		case req.DryRun && want:
			ch.Message = "would create " + path + " and remove the redirect rules; all new connections would go direct"
		case req.DryRun:
			ch.Message = "would delete " + path + "; rules return once the relay is healthy and no other VPN is active"
		case want:
			if err := os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
				return nil, err
			}
			ch.Message = "disabled; redirect rules removed, new connections go direct"
		default:
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			ch.Message = "enabled"
		}
		if ch.Changed && !req.DryRun {
			d.kickReconcile()
			time.Sleep(300 * time.Millisecond) // let the reconcile land so status is truthful
		}
		return ch, nil
	}
	return nil, fmt.Errorf("unknown command %q", req.Cmd)
}
