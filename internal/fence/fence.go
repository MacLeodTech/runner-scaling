// Package fence keeps runner networks off private address space with an nftables table of its own.
//
// Everything is spoken over netlink, so the container needs no nft or iptables binary. The table (inet gh_runners)
// is separate from Docker's, ufw's and firewalld's, so their reloads and an `iptables -F` do not touch it, and its
// base chains run before the default (priority 0) filter chains, so no accept elsewhere can undo a drop here.
// One inet table covers IPv4 and IPv6.
package fence

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

const (
	// TableName is the nftables table the fence owns.
	TableName = "gh_runners"
	chainFwd  = "forward"
	chainIn   = "input"
	// priority runs before Docker's, iptables' and ufw's chains (all 0, or -1 for Docker's nft backend raw) so that
	// our drop is decided first. Any lower chain's accept does not matter: a verdict of drop is final.
	priority = -5
)

// DefaultBlock4 is the IPv4 space a runner network must not reach: private, link-local, shared (CGNAT), loopback,
// "this network", IETF protocol and benchmarking ranges, multicast and the reserved/broadcast block.
var DefaultBlock4 = mustPrefixes("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4")

// DefaultBlock6 is the IPv6 counterpart: unspecified, loopback, unique local, link-local, multicast, IPv4-mapped and
// the NAT64 well-known prefix (which would reach IPv4 private space through a translator).
var DefaultBlock6 = mustPrefixes("::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "fc00::/7", "fe80::/10", "ff00::/8")

func mustPrefixes(s ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, v := range s {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}

// Spec is what to fence. Src* are the runner networks (every guard's subnet range must fall inside); Block* the
// destinations they may not reach, forwarded or aimed at the host itself.
type Spec struct {
	Src4, Src6     []netip.Prefix
	Block4, Block6 []netip.Prefix
}

// Normalize fills the default block lists and validates the prefixes.
func (s *Spec) Normalize() error {
	if len(s.Src4)+len(s.Src6) == 0 {
		return errors.New("no source range to fence")
	}
	if s.Block4 == nil {
		s.Block4 = DefaultBlock4
	}
	if s.Block6 == nil {
		s.Block6 = DefaultBlock6
	}
	for _, p := range s.Src4 {
		if !p.Addr().Is4() || p.Bits() < 8 || p != p.Masked() {
			return fmt.Errorf("source %s: want an IPv4 network of /8 or longer, e.g. 172.30.0.0/16", p)
		}
	}
	for _, p := range s.Src6 {
		if !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() < 16 || p != p.Masked() {
			return fmt.Errorf("source %s: want an IPv6 network of /16 or longer, e.g. fd00:a::/64", p)
		}
	}
	for _, p := range s.Block4 {
		if !p.Addr().Is4() {
			return fmt.Errorf("block %s is not IPv4", p)
		}
	}
	for _, p := range s.Block6 {
		if !p.Addr().Is6() {
			return fmt.Errorf("block %s is not IPv6", p)
		}
	}
	return nil
}

// ruleDef is one rule: the chain, a fingerprint stored in the rule's userdata (so Verify can tell our rules from
// anything else), and its expressions.
type ruleDef struct {
	chain string
	tag   string
	exprs []expr.Any
}

func (s Spec) rules() []ruleDef {
	var out []ruleDef
	for _, src := range s.Src4 {
		for _, dst := range s.Block4 {
			out = append(out, ruleDef{chainFwd, fmt.Sprintf("fwd %s>%s", src, dst), dropRule(src, &dst)})
		}
		out = append(out, ruleDef{chainIn, fmt.Sprintf("in %s", src), inputRule(src)})
	}
	for _, src := range s.Src6 {
		for _, dst := range s.Block6 {
			out = append(out, ruleDef{chainFwd, fmt.Sprintf("fwd %s>%s", src, dst), dropRule(src, &dst)})
		}
		out = append(out, ruleDef{chainIn, fmt.Sprintf("in %s", src), inputRule(src)})
	}
	return out
}

func (r ruleDef) userData() []byte { return []byte("runner-scaling " + r.tag) }

// dropRule: source in src (and destination in dst, if given) -> count and drop.
func dropRule(src netip.Prefix, dst *netip.Prefix) []expr.Any {
	e := family(src)
	e = append(e, matchAddr(src, srcOffset(src))...)
	if dst != nil {
		e = append(e, matchAddr(*dst, dstOffset(src))...)
	}
	return append(e, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop})
}

// inputRule: traffic from the fenced network to the host itself is dropped unless it answers something the host
// started (conntrack established or related).
func inputRule(src netip.Prefix) []expr.Any {
	e := family(src)
	e = append(e, matchAddr(src, srcOffset(src))...)
	state := binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED)
	e = append(e,
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: state, Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: make([]byte, 4)}, // none of the reply states set
	)
	return append(e, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop})
}

func family(p netip.Prefix) []expr.Any {
	proto := byte(nftables.TableFamilyIPv4)
	if p.Addr().Is6() {
		proto = byte(nftables.TableFamilyIPv6)
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
	}
}

func srcOffset(p netip.Prefix) uint32 {
	if p.Addr().Is6() {
		return 8
	}
	return 12
}

func dstOffset(p netip.Prefix) uint32 {
	if p.Addr().Is6() {
		return 24
	}
	return 16
}

// matchAddr loads the address at the given network-header offset and compares it with the prefix.
func matchAddr(p netip.Prefix, off uint32) []expr.Any {
	n := uint32(4)
	if p.Addr().Is6() {
		n = 16
	}
	addr := p.Masked().Addr().AsSlice()
	mask := make([]byte, n)
	for i := 0; i < p.Bits(); i++ {
		mask[i/8] |= 0x80 >> (i % 8)
	}
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: off, Len: n},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: n, Mask: mask, Xor: make([]byte, n)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: addr},
	}
}

// Apply installs the table, replacing any earlier version of it in one atomic netlink transaction: there is no
// moment with the old rules gone and the new ones not yet in place.
func (s Spec) Apply() error {
	if err := s.check(); err != nil {
		return err
	}
	c, err := nftables.New()
	if err != nil {
		return err
	}
	defer c.CloseLasting()
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: TableName}
	c.AddTable(t) // create if missing, so the delete below cannot fail on a fresh host
	c.DelTable(t)
	c.AddTable(t)
	accept := nftables.ChainPolicyAccept
	prio := nftables.ChainPriority(priority)
	chains := map[string]*nftables.Chain{}
	for _, cc := range []struct {
		name string
		hook *nftables.ChainHook
	}{{chainFwd, nftables.ChainHookForward}, {chainIn, nftables.ChainHookInput}} {
		chains[cc.name] = c.AddChain(&nftables.Chain{Name: cc.name, Table: t, Type: nftables.ChainTypeFilter,
			Hooknum: cc.hook, Priority: &prio, Policy: &accept})
	}
	for _, r := range s.rules() {
		c.AddRule(&nftables.Rule{Table: t, Chain: chains[r.chain], Exprs: r.exprs, UserData: r.userData()})
	}
	return c.Flush()
}

func (s Spec) check() error {
	if len(s.Src4)+len(s.Src6) == 0 || s.Block4 == nil && s.Block6 == nil {
		return errors.New("fence: spec not normalized")
	}
	return nil
}

// Status is what Verify found.
type Status struct {
	OK      bool
	Problem string // why not OK
	// Packets dropped so far (since the table was installed), by chain.
	DroppedForward, DroppedInput uint64
}

// Verify reads the kernel's ruleset and checks that the table, its two chains (hook, priority, policy) and exactly
// the expected rules, in order, are present. It reads, never changes.
func (s Spec) Verify() (Status, error) {
	if err := s.check(); err != nil {
		return Status{}, err
	}
	c, err := nftables.New()
	if err != nil {
		return Status{}, err
	}
	defer c.CloseLasting()
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return Status{}, err
	}
	var t *nftables.Table
	for _, x := range tables {
		if x.Name == TableName {
			t = x
		}
	}
	if t == nil {
		return Status{Problem: "table " + TableName + " is missing"}, nil
	}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		return Status{}, err
	}
	got := map[string]*nftables.Chain{}
	for _, ch := range chains {
		if ch.Table != nil && ch.Table.Name == TableName {
			got[ch.Name] = ch
		}
	}
	st := Status{}
	want := map[string]*nftables.ChainHook{chainFwd: nftables.ChainHookForward, chainIn: nftables.ChainHookInput}
	for name, hook := range want {
		ch := got[name]
		switch {
		case ch == nil:
			st.Problem = "chain " + name + " is missing"
		case ch.Hooknum == nil || *ch.Hooknum != *hook || ch.Priority == nil || int32(*ch.Priority) != priority:
			st.Problem = "chain " + name + " has the wrong hook or priority"
		case ch.Policy == nil || *ch.Policy != nftables.ChainPolicyAccept || ch.Type != nftables.ChainTypeFilter:
			st.Problem = "chain " + name + " has the wrong type or policy"
		}
		if st.Problem != "" {
			return st, nil
		}
	}
	for name, ch := range got {
		rules, err := c.GetRules(t, ch)
		if err != nil {
			return Status{}, err
		}
		var wantTags []string
		for _, r := range s.rules() {
			if r.chain == name {
				wantTags = append(wantTags, string(r.userData()))
			}
		}
		var gotTags []string
		for _, r := range rules {
			gotTags = append(gotTags, string(r.UserData))
			for _, e := range r.Exprs {
				if cn, ok := e.(*expr.Counter); ok {
					if name == chainFwd {
						st.DroppedForward += cn.Packets
					} else {
						st.DroppedInput += cn.Packets
					}
				}
			}
		}
		if !bytes.Equal([]byte(strings.Join(gotTags, "\n")), []byte(strings.Join(wantTags, "\n"))) {
			st.Problem = fmt.Sprintf("chain %s has %d rules, expected %d (or they differ)", name, len(gotTags), len(wantTags))
			return st, nil
		}
	}
	st.OK = true
	return st, nil
}

// Remove deletes the table (the fence). A missing table is not an error.
func Remove() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	defer c.CloseLasting()
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: TableName}
	c.AddTable(t)
	c.DelTable(t)
	return c.Flush()
}
