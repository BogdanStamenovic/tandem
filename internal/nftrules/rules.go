// Package nftrules installs the client's redirect rules.
//
// The table is created with NFT_TABLE_F_OWNER over a netlink socket this
// process keeps open. The kernel then destroys the table the moment that
// socket closes, which includes the process dying from SIGKILL or a crash.
// That is the core of tandem's fail-open guarantee: a dead daemon cannot
// leave rules behind that redirect traffic into a closed port.
//
// Upstream google/nftables v0.3.0 hardcodes table flags to zero, hence the
// one-line patched copy in third_party/nftables.
package nftrules

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

const (
	TableName = "tandem"
	chainName = "redirect"
	// NFT_TABLE_F_OWNER, include/uapi/linux/netfilter/nf_tables.h (Linux 5.12+).
	tableFlagOwner = 0x2
)

// Spec describes the rules.
type Spec struct {
	// Off keeps the table and its (empty) nat chain but redirects nothing.
	// Deleting the chain instead would break connections already redirected:
	// when the last nat chain on a hook goes away the kernel unregisters its
	// NAT hooks, and existing conntrack entries stop being translated, so
	// their replies never reach the application.
	Off         bool
	Port        uint16         // local port the daemon listens on (127.0.0.1)
	Uplink      string         // only redirect traffic routed out of this interface
	Bypass      []netip.Prefix // IPv4 destinations left alone
	BypassPorts []uint16
	SkipMarks   [][2]uint32 // (value, mask) pairs left alone
}

// Rules owns the netlink socket the table is tied to.
type Rules struct {
	mu        sync.Mutex
	conn      *nftables.Conn
	installed bool
	spec      Spec
}

// Open creates the lasting netlink connection. It also deletes a stale
// non-owned table of the same name (from a manual `nft -f` or an older
// version); an owned one belonging to another live tandem makes Apply fail,
// which is what should happen with two daemons.
func Open() (*Rules, error) {
	c, err := nftables.New(nftables.AsLasting())
	if err != nil {
		return nil, err
	}
	r := &Rules{conn: c}
	if t, _ := r.find(); t != nil && t.Flags&tableFlagOwner == 0 {
		c.DelTable(t)
		if err := c.Flush(); err != nil {
			return nil, fmt.Errorf("removing stale table: %w", err)
		}
	}
	return r, nil
}

func (r *Rules) find() (*nftables.Table, error) {
	tabs, err := r.conn.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, err
	}
	for _, t := range tabs {
		if t.Name == TableName {
			return t, nil
		}
	}
	return nil, nil
}

// Installed reports whether new connections are being redirected, and the spec.
func (r *Rules) Installed() (bool, Spec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installed && !r.spec.Off, r.spec
}

// Apply atomically swaps the chain's rules for ones built from s. The table
// and chain are created once and never deleted while the daemon lives:
// deleting and re-adding them, even inside one batch, briefly unregisters the
// NAT hook and breaks connections redirected moments earlier (see Spec.Off).
func (r *Rules) Apply(s Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := &nftables.Table{Name: TableName, Family: nftables.TableFamilyIPv4, Flags: tableFlagOwner}
	ch := &nftables.Chain{
		Name:     chainName,
		Table:    t,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATDest - 10),
	}
	if !r.installed {
		r.conn.AddTable(t)
		r.conn.AddChain(ch)
	} else {
		r.conn.FlushChain(ch)
	}
	r.conn.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: holdConntrack()})
	if !s.Off {
		for _, exprs := range build(s) {
			r.conn.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: exprs})
		}
	}
	if err := r.conn.Flush(); err != nil {
		// The kernel rolls a failed batch back as a whole.
		return fmt.Errorf("nftables: %w", err)
	}
	r.installed, r.spec = true, s
	return nil
}

// Pause stops redirecting new connections but keeps the nat chain, so
// connections redirected earlier keep working (see Spec.Off).
func (r *Rules) Pause() error {
	r.mu.Lock()
	installed, off := r.installed, r.spec.Off
	r.mu.Unlock()
	if !installed || off {
		return nil
	}
	return r.Apply(Spec{Off: true})
}

// Remove deletes the table. Only for shutdown: see Spec.Off.
func (r *Rules) Remove() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.installed {
		return nil
	}
	r.conn.DelTable(&nftables.Table{Name: TableName, Family: nftables.TableFamilyIPv4})
	if err := r.conn.Flush(); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	r.installed = false
	return nil
}

// Close removes the table and closes the socket (closing alone would also
// make the kernel drop the table).
func (r *Rules) Close() error {
	err := r.Remove()
	r.conn.CloseLasting()
	return err
}

// holdConntrack is `ct state invalid return`. Its job is not the match: any
// ct expression takes a conntrack reference for the netns, and so does the
// redirect. If flushing the redirect rule dropped the last reference, the
// kernel would unregister conntrack's hooks and every connection redirected
// earlier would stop being translated. Keeping this rule in place for the
// daemon's lifetime makes pausing safe for in-flight connections.
func holdConntrack() []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(expr.CtStateBitINVALID), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)},
		ret(),
	}
}

func ret() expr.Any { return &expr.Verdict{Kind: expr.VerdictReturn} }

func build(s Spec) [][]expr.Any {
	var rules [][]expr.Any
	// Not TCP: leave it.
	rules = append(rules, []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		ret(),
	})
	if s.Uplink != "" {
		rules = append(rules, []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifname(s.Uplink)},
			ret(),
		})
	}
	for _, m := range s.SkipMarks {
		rules = append(rules, []expr.Any{
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(m[1]), Xor: le32(0)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: le32(m[0])},
			ret(),
		})
	}
	for _, p := range s.Bypass {
		if !p.Addr().Is4() {
			continue
		}
		mask := net4mask(p.Bits())
		a := p.Masked().Addr().As4()
		rules = append(rules, []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: a[:]},
			ret(),
		})
	}
	for _, port := range s.BypassPorts {
		rules = append(rules, []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(port)},
			ret(),
		})
	}
	rules = append(rules, []expr.Any{
		&expr.Immediate{Register: 1, Data: be16(s.Port)},
		&expr.Redir{RegisterProtoMin: 1},
	})
	return rules
}

// Text renders s as the equivalent nft script, for --dry-run and docs.
func Text(s Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table ip %s {\n\tflags owner\n\tchain %s {\n", TableName, chainName)
	fmt.Fprintf(&b, "\t\ttype nat hook output priority dstnat - 10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tct state invalid return  # holds a conntrack reference, see holdConntrack\n")
	if s.Off {
		b.WriteString("\t\t# paused: nothing is redirected\n\t}\n}\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\t\tmeta l4proto != tcp return\n")
	if s.Uplink != "" {
		fmt.Fprintf(&b, "\t\toifname != %q return\n", s.Uplink)
	}
	for _, m := range s.SkipMarks {
		fmt.Fprintf(&b, "\t\tmeta mark & 0x%08x == 0x%08x return\n", m[1], m[0])
	}
	for _, p := range s.Bypass {
		fmt.Fprintf(&b, "\t\tip daddr %s return\n", p)
	}
	for _, p := range s.BypassPorts {
		fmt.Fprintf(&b, "\t\ttcp dport %d return\n", p)
	}
	fmt.Fprintf(&b, "\t\tredirect to :%d\n\t}\n}\n", s.Port)
	return b.String()
}

func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n)
	return b
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// Meta mark is host byte order in the register.
func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

func net4mask(bits int) []byte {
	m := uint32(0)
	if bits > 0 {
		m = ^uint32(0) << (32 - bits)
	}
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, m)
	return b
}
