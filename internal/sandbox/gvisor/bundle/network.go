package bundle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// Firewall selects the rule tool on the sandbox host.
type Firewall string

// Firewall tools.
const (
	FirewallNFT      Firewall = "nft"
	FirewallIPTables Firewall = "iptables"
)

// DefaultPool is the address pool carved into one /30 per sandbox.
var DefaultPool = netip.MustParsePrefix("10.237.0.0/16")

// Net is the network plan of one sandbox.
type Net struct {
	Short     string     // 10 hex chars derived from the sandbox id
	Namespace string     // ip netns name
	HostIf    string     // host side of the veth pair
	PeerIf    string     // namespace side
	HostAddr  netip.Addr // .1 of the /30, default gateway of the sandbox
	PeerAddr  netip.Addr // .2 of the /30
	Bits      int        // 30
	Proxy     netip.AddrPort
	DNS       netip.Addr // resolver reachable from the sandbox; invalid = none
	Firewall  Firewall
}

// NetnsPath is the bind-mounted namespace file `ip netns add` creates.
func (n Net) NetnsPath() string { return "/var/run/netns/" + n.Namespace }

func (n Net) chain() string { return "MYCEL-" + n.Short }
func (n Net) table() string { return "mycel_" + n.Short }

// ShortID derives a stable short name from a sandbox id.
func ShortID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:5])
}

// NewNet names the interfaces for id and puts it on subnet slot.
func NewNet(id string, pool netip.Prefix, slot int) (Net, error) {
	if !pool.Addr().Is4() || pool.Bits() > 30 {
		return Net{}, errors.New("bundle: address pool must be IPv4 and at least a /30")
	}
	if slot < 0 || slot >= 1<<(30-pool.Bits()) {
		return Net{}, fmt.Errorf("bundle: subnet slot %d out of range", slot)
	}
	base := pool.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(base[:]) + uint32(slot)*4 // #nosec G115 -- slot < 2^28 checked above
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], v+1)
	host := netip.AddrFrom4(a)
	binary.BigEndian.PutUint32(a[:], v+2)
	peer := netip.AddrFrom4(a)
	s := ShortID(id)
	return Net{
		Short:     s,
		Namespace: "mycel-" + s,
		HostIf:    "myh" + s,
		PeerIf:    "mys" + s,
		HostAddr:  host,
		PeerAddr:  peer,
		Bits:      30,
	}, nil
}

// SlotAllocator hands out /30 slots of a pool.
type SlotAllocator struct {
	mu   sync.Mutex
	size int
	used map[int]string
}

// NewSlotAllocator covers pool.
func NewSlotAllocator(pool netip.Prefix) *SlotAllocator {
	n := 0
	if pool.Bits() <= 30 {
		n = 1 << (30 - pool.Bits())
	}
	return &SlotAllocator{size: n, used: map[int]string{}}
}

// Get returns a free slot for id, starting at a hash of id. Slots for which
// skip returns true (their addresses are already assigned on the host) are
// passed over; skip may be nil.
func (a *SlotAllocator) Get(id string, skip func(slot int) bool) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.size == 0 {
		return 0, errors.New("bundle: empty address pool")
	}
	sum := sha256.Sum256([]byte(id))
	start := int(binary.BigEndian.Uint32(sum[:4]) % uint32(a.size)) // #nosec G115 -- size <= 2^30
	for i := 0; i < a.size; i++ {
		s := (start + i) % a.size
		if skip != nil && skip(s) {
			continue
		}
		if _, ok := a.used[s]; !ok {
			a.used[s] = id
			return s, nil
		}
	}
	return 0, errors.New("bundle: address pool exhausted")
}

// Release frees the slot held by id.
func (a *SlotAllocator) Release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for s, owner := range a.used {
		if owner == id {
			delete(a.used, s)
		}
	}
}

// local reports whether dst is the host side of the veth (delivered by
// INPUT) rather than a remote address that has to be forwarded (WSL2).
func (n Net) local(dst netip.Addr) bool { return dst == n.HostAddr }

func (n Net) needsForward() bool {
	return !n.local(n.Proxy.Addr()) || (n.DNS.IsValid() && !n.local(n.DNS))
}

// SetupArgv is the ordered list of commands that creates the namespace,
// the veth pair and the firewall rules.
func (n Net) SetupArgv() [][]string {
	cidr := func(a netip.Addr) string { return fmt.Sprintf("%s/%d", a, n.Bits) }
	cmds := [][]string{
		{"ip", "netns", "add", n.Namespace},
		{"ip", "link", "add", n.HostIf, "type", "veth", "peer", "name", n.PeerIf},
		{"ip", "link", "set", n.PeerIf, "netns", n.Namespace},
		{"ip", "addr", "add", cidr(n.HostAddr), "dev", n.HostIf},
		{"ip", "link", "set", n.HostIf, "up"},
		{"ip", "-n", n.Namespace, "addr", "add", cidr(n.PeerAddr), "dev", n.PeerIf},
		{"ip", "-n", n.Namespace, "link", "set", n.PeerIf, "up"},
		{"ip", "-n", n.Namespace, "link", "set", "lo", "up"},
		{"ip", "-n", n.Namespace, "route", "add", "default", "via", n.HostAddr.String()},
	}
	if n.needsForward() {
		cmds = append(cmds, []string{"sysctl", "-w", "net.ipv4.ip_forward=1"})
	}
	if n.Firewall == FirewallIPTables {
		return append(cmds, n.iptablesSetup()...)
	}
	return append(cmds, n.nftSetup()...)
}

// TeardownArgv removes rules, interfaces and namespace. Every command may fail
// when the object is already gone; callers ignore those errors.
func (n Net) TeardownArgv() [][]string {
	var cmds [][]string
	if n.Firewall == FirewallIPTables {
		cmds = n.iptablesTeardown()
	} else {
		cmds = [][]string{{"nft", "delete", "table", "inet", n.table()}}
	}
	return append(cmds,
		[]string{"ip", "link", "del", n.HostIf},
		[]string{"ip", "netns", "del", n.Namespace},
	)
}

type flow struct {
	proto string
	dst   netip.Addr
	port  uint16
}

func (n Net) flows() []flow {
	f := []flow{{"tcp", n.Proxy.Addr(), n.Proxy.Port()}}
	if n.DNS.IsValid() {
		f = append(f, flow{"udp", n.DNS, 53}, flow{"tcp", n.DNS, 53})
	}
	return f
}

func (n Net) nftSetup() [][]string {
	t := n.table()
	cmds := [][]string{
		{"nft", "add", "table", "inet", t},
		{"nft", "add", "chain", "inet", t, "input", "{ type filter hook input priority 0 ; policy accept ; }"},
		{"nft", "add", "chain", "inet", t, "forward", "{ type filter hook forward priority 0 ; policy accept ; }"},
	}
	var forwarded bool
	for _, f := range n.flows() {
		port := fmt.Sprint(f.port)
		if n.local(f.dst) {
			cmds = append(cmds, []string{"nft", "add", "rule", "inet", t, "input",
				"iifname", n.HostIf, "ip", "saddr", n.PeerAddr.String(), "ip", "daddr", f.dst.String(), f.proto, "dport", port, "accept"})
			continue
		}
		forwarded = true
		cmds = append(cmds, []string{"nft", "add", "rule", "inet", t, "forward",
			"iifname", n.HostIf, "ip", "saddr", n.PeerAddr.String(), "ip", "daddr", f.dst.String(), f.proto, "dport", port, "accept"})
	}
	cmds = append(cmds,
		[]string{"nft", "add", "rule", "inet", t, "input", "iifname", n.HostIf, "drop"},
	)
	if forwarded {
		cmds = append(cmds,
			[]string{"nft", "add", "rule", "inet", t, "forward", "oifname", n.HostIf, "ct", "state", "established,related", "accept"},
		)
	}
	cmds = append(cmds,
		[]string{"nft", "add", "rule", "inet", t, "forward", "iifname", n.HostIf, "drop"},
		[]string{"nft", "add", "rule", "inet", t, "forward", "oifname", n.HostIf, "drop"},
	)
	if forwarded {
		cmds = append(cmds,
			[]string{"nft", "add", "chain", "inet", t, "postrouting", "{ type nat hook postrouting priority 100 ; policy accept ; }"},
			[]string{"nft", "add", "rule", "inet", t, "postrouting", "ip", "saddr", n.PeerAddr.String(), "oifname", "!=", n.HostIf, "masquerade"},
		)
	}
	return cmds
}

func (n Net) iptablesSetup() [][]string {
	c := n.chain()
	cmds := [][]string{{"iptables", "-w", "-N", c}}
	var forwarded bool
	for _, f := range n.flows() {
		if !n.local(f.dst) {
			forwarded = true
		}
		cmds = append(cmds, []string{"iptables", "-w", "-A", c, "-s", n.PeerAddr.String(),
			"-d", f.dst.String(), "-p", f.proto, "--dport", fmt.Sprint(f.port), "-j", "ACCEPT"})
	}
	if forwarded {
		cmds = append(cmds, []string{"iptables", "-w", "-A", c, "-o", n.HostIf,
			"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"})
	}
	cmds = append(cmds,
		[]string{"iptables", "-w", "-A", c, "-j", "DROP"},
		[]string{"iptables", "-w", "-I", "INPUT", "-i", n.HostIf, "-j", c},
		[]string{"iptables", "-w", "-I", "FORWARD", "-i", n.HostIf, "-j", c},
		[]string{"iptables", "-w", "-I", "FORWARD", "-o", n.HostIf, "-j", c},
		[]string{"ip6tables", "-w", "-I", "INPUT", "-i", n.HostIf, "-j", "DROP"},
		[]string{"ip6tables", "-w", "-I", "FORWARD", "-i", n.HostIf, "-j", "DROP"},
		[]string{"ip6tables", "-w", "-I", "FORWARD", "-o", n.HostIf, "-j", "DROP"},
	)
	if forwarded {
		cmds = append(cmds, []string{"iptables", "-w", "-t", "nat", "-A", "POSTROUTING",
			"-s", n.PeerAddr.String(), "!", "-o", n.HostIf, "-j", "MASQUERADE"})
	}
	return cmds
}

func (n Net) iptablesTeardown() [][]string {
	c := n.chain()
	return [][]string{
		{"iptables", "-w", "-t", "nat", "-D", "POSTROUTING", "-s", n.PeerAddr.String(), "!", "-o", n.HostIf, "-j", "MASQUERADE"},
		{"ip6tables", "-w", "-D", "FORWARD", "-o", n.HostIf, "-j", "DROP"},
		{"ip6tables", "-w", "-D", "FORWARD", "-i", n.HostIf, "-j", "DROP"},
		{"ip6tables", "-w", "-D", "INPUT", "-i", n.HostIf, "-j", "DROP"},
		{"iptables", "-w", "-D", "FORWARD", "-o", n.HostIf, "-j", c},
		{"iptables", "-w", "-D", "FORWARD", "-i", n.HostIf, "-j", c},
		{"iptables", "-w", "-D", "INPUT", "-i", n.HostIf, "-j", c},
		{"iptables", "-w", "-F", c},
		{"iptables", "-w", "-X", c},
	}
}

// ParseDefaultGateway extracts the gateway of `ip route show default`
// ("default via 172.22.16.1 dev eth0 proto kernel").
func ParseDefaultGateway(out []byte) (netip.Addr, error) {
	fields := splitFields(string(out))
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] == "default" && fields[i+1] == "via" {
			return netip.ParseAddr(fields[i+2])
		}
	}
	return netip.Addr{}, errors.New("bundle: no default route")
}
