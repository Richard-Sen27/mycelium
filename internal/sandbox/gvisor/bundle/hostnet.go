package bundle

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Host-wide rules, one set per sandbox host (not per sandbox):
//
//   - the egress proxy port accepts connections only from lo and the
//     sandboxes' veth interfaces (myh*), so listening on 0.0.0.0 on a Linux
//     node does not expose the proxy to the network (Linux only: on WSL the
//     proxy is on Windows);
//   - nothing on the host may open a connection into a sandbox: output to
//     myh* is dropped unless it belongs to an established flow.

// HostTable is the nftables table holding the host-wide rules.
const HostTable = "mycel"

// HostChain is the iptables chain guarding the proxy port.
const HostChain = "MYCEL-HOST"

// HostRulesNFT is the `nft -f -` script that (re)creates the host-wide
// table atomically. guardPort 0 skips the proxy-port rule.
func HostRulesNFT(guardPort uint16) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\ntable inet %s {\n", HostTable, HostTable, HostTable)
	if guardPort != 0 {
		fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority 0; policy accept;\n")
		fmt.Fprintf(&b, "\t\ttcp dport %d iifname != \"lo\" iifname != \"myh*\" drop\n\t}\n", guardPort)
	}
	b.WriteString("\tchain output {\n\t\ttype filter hook output priority 0; policy accept;\n")
	b.WriteString("\t\toifname \"myh*\" ct state established,related accept\n")
	b.WriteString("\t\toifname \"myh*\" drop\n\t}\n}\n")
	return b.String()
}

// NFTScriptArgv applies an nft script from stdin.
var NFTScriptArgv = []string{"nft", "-f", "-"}

// HostRulesIPTables creates the host-wide rules with iptables; run
// RulesToDelete over `iptables -S` first to remove an older set.
func HostRulesIPTables(guardPort uint16) [][]string {
	var cmds [][]string
	if guardPort != 0 {
		cmds = append(cmds,
			[]string{"iptables", "-w", "-N", HostChain},
			[]string{"iptables", "-w", "-A", HostChain, "-i", "lo", "-j", "RETURN"},
			[]string{"iptables", "-w", "-A", HostChain, "-i", "myh+", "-j", "RETURN"},
			[]string{"iptables", "-w", "-A", HostChain, "-j", "DROP"},
			[]string{"iptables", "-w", "-I", "INPUT", "-p", "tcp", "--dport", fmt.Sprint(guardPort), "-j", HostChain},
		)
	}
	return append(cmds,
		[]string{"iptables", "-w", "-I", "OUTPUT", "-o", "myh+", "-m", "conntrack", "!", "--ctstate", "ESTABLISHED,RELATED", "-j", "DROP"},
		[]string{"ip6tables", "-w", "-I", "OUTPUT", "-o", "myh+", "-j", "DROP"},
	)
}

// IsHostRule matches `iptables -S` lines of the host-wide set.
func IsHostRule(line string) bool {
	return strings.Contains(line, HostChain) || (strings.Contains(line, "-A OUTPUT") && strings.Contains(line, "-o myh+"))
}

var shortRe = regexp.MustCompile(`^[0-9a-f]{10}$`)

// shortOf returns the short id in a name with the given prefix, or "".
func shortOf(name, prefix string) string {
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	s := strings.TrimPrefix(name, prefix)
	if !shortRe.MatchString(s) {
		return ""
	}
	return s
}

// ParseNetns returns the short ids of mycel-* namespaces in `ip netns list`.
func ParseNetns(out []byte) []string {
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) > 0 {
			if s := shortOf(f[0], "mycel-"); s != "" {
				res = append(res, s)
			}
		}
	}
	return res
}

// ParseLinks returns the short ids of myh* links in `ip -o link show`
// ("12: myh0123456789@if11: <…>").
func ParseLinks(out []byte) []string {
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		name := strings.TrimSuffix(f[1], ":")
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		if s := shortOf(name, "myh"); s != "" {
			res = append(res, s)
		}
	}
	return res
}

// ParseNFTTables returns the short ids of inet mycel_* tables in `nft list
// tables`.
func ParseNFTTables(out []byte) []string {
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "table" && f[1] == "inet" {
			if s := shortOf(f[2], "mycel_"); s != "" {
				res = append(res, s)
			}
		}
	}
	return res
}

// ParseAddrs returns the IPv4 addresses in `ip -o -4 addr show`.
func ParseAddrs(out []byte) map[netip.Addr]bool {
	res := map[netip.Addr]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" {
				if p, err := netip.ParsePrefix(f[i+1]); err == nil {
					res[p.Addr()] = true
				}
			}
		}
	}
	return res
}

// RulesToDelete turns the matching "-A" lines of `iptables [-t table] -S`
// into delete commands (tool and table prepended), and the matching "-N"
// chains into flush and delete commands after them.
func RulesToDelete(tool, table string, out []byte, match func(line string) bool) [][]string {
	prefix := []string{tool, "-w"}
	if table != "" {
		prefix = append(prefix, "-t", table)
	}
	var rules, chains [][]string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if !match(l) {
			continue
		}
		f := strings.Fields(l)
		switch {
		case len(f) > 2 && f[0] == "-A":
			rules = append(rules, append(append(append([]string(nil), prefix...), "-D"), f[1:]...))
		case len(f) == 2 && f[0] == "-N":
			chains = append(chains,
				append(append([]string(nil), prefix...), "-F", f[1]),
				append(append([]string(nil), prefix...), "-X", f[1]))
		}
	}
	return append(rules, chains...)
}

// sandboxRuleShort returns the short id an `iptables -S` line belongs to
// (it names myh<short> or MYCEL-<short>), or "".
var sandboxRuleRe = regexp.MustCompile(`(?:myh|MYCEL-)([0-9a-f]{10})\b`)

func sandboxRuleShort(line string) string {
	m := sandboxRuleRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}
