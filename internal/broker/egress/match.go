package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// rule is a parsed Rule. Exactly one of ip and host is set.
type rule struct {
	ip       netip.Addr // IP-literal rule
	host     string     // lower-case hostname, or the suffix of a wildcard
	wildcard bool       // "*.suffix"
	port     int        // 0 means the default ports 80 and 443
}

// String is the rule's normalised text.
func (r rule) String() string {
	var h string
	switch {
	case r.ip.IsValid():
		h = r.ip.String()
		if r.ip.Is6() && r.port != 0 {
			h = "[" + h + "]"
		}
	case r.wildcard:
		h = "*." + r.host
	default:
		h = r.host
	}
	if r.port != 0 {
		h += ":" + strconv.Itoa(r.port)
	}
	return h
}

// ParseRule checks one allowlist entry against the rule grammar ("host",
// "*.suffix", "host:port", IP literals, "[v6]:port") and returns it
// normalised: host lower-cased, trailing dot trimmed, IP literals in
// canonical form. Wildcards need at least two suffix labels, and names may
// not contain all-numeric or hex ("0x…") labels.
func ParseRule(s string) (Rule, error) {
	r, err := parseRuleErr(s)
	if err != nil {
		return "", err
	}
	return Rule(r.String()), nil
}

// ValidateRules returns the errors of every invalid rule, joined; nil when
// all rules are valid.
func ValidateRules(rules []string) error {
	var errs []error
	for _, s := range rules {
		if _, err := parseRuleErr(s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// parseRule parses one allowlist entry; ok is false for invalid rules, which
// never match.
func parseRule(r Rule) (rule, bool) {
	p, err := parseRuleErr(string(r))
	return p, err == nil
}

func parseRuleErr(raw string) (rule, error) {
	bad := func(why string) (rule, error) {
		return rule{}, fmt.Errorf("egress: invalid rule %q: %s", raw, why)
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return bad("empty")
	}
	var h, pt string
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return bad("unclosed bracket")
		}
		h, rest := s[1:end], s[end+1:]
		if rest != "" {
			p, found := strings.CutPrefix(rest, ":")
			if !found || p == "" {
				return bad("expected :port after ]")
			}
			pt = p
		}
		ip, err := netip.ParseAddr(h)
		if err != nil || !ip.Is6() {
			return bad("brackets only hold IPv6 literals")
		}
		port, ok := parseRulePort(pt)
		if !ok {
			return bad("port must be 1-65535")
		}
		return rule{ip: ip.Unmap(), port: port}, nil
	case strings.Count(s, ":") > 1:
		// A bare IPv6 literal; a port needs brackets.
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return bad("not an IPv6 literal (use [v6]:port for a port)")
		}
		return rule{ip: ip.Unmap()}, nil
	default:
		h = s
		if i := strings.IndexByte(s, ':'); i >= 0 {
			h, pt = s[:i], s[i+1:]
			if pt == "" {
				return bad("empty port")
			}
		}
	}
	port, ok := parseRulePort(pt)
	if !ok {
		return bad("port must be 1-65535")
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return rule{ip: ip.Unmap(), port: port}, nil
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	wildcard := false
	if suffix, found := strings.CutPrefix(h, "*."); found {
		wildcard = true
		h = suffix
	}
	if err := checkHostname(h); err != nil {
		return bad(err.Error())
	}
	if wildcard && !strings.Contains(h, ".") {
		return bad("a wildcard needs at least two suffix labels")
	}
	return rule{host: h, wildcard: wildcard, port: port}, nil
}

// parseRulePort parses an optional port; "" yields 0 (default ports).
func parseRulePort(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	return parsePort(s)
}

// parsePort parses a decimal port in 1..65535.
func parsePort(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// checkHostname accepts lower-case dot-separated labels of [a-z0-9_-]. It
// refuses names that resolvers may read as IPv4 addresses ("127.1",
// "0x7f.1", "2130706433"): names whose labels are all numeric or 0x-hex, and
// names whose last label is numeric or hex. A numeric label elsewhere
// ("123.example.com") is an ordinary host name.
func checkHostname(h string) error {
	if h == "" || len(h) > 253 {
		return errors.New("host name empty or too long")
	}
	labels := strings.Split(h, ".")
	allAddrLike := true
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return errors.New("empty or over-long label")
		}
		for _, c := range label {
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
			if !ok {
				return fmt.Errorf("label %q has characters outside [a-z0-9_-]", label)
			}
		}
		if !isNumericLabel(label) && !isHexLabel(label) {
			allAddrLike = false
		}
	}
	last := labels[len(labels)-1]
	if allAddrLike || isNumericLabel(last) || isHexLabel(last) {
		return fmt.Errorf("name %q could be read as an IPv4 address", h)
	}
	return nil
}

// isNumericLabel reports a label made only of digits.
func isNumericLabel(l string) bool {
	if l == "" {
		return false
	}
	for _, c := range l {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isHexLabel reports "0x" followed only by hex digits.
func isHexLabel(l string) bool {
	rest, ok := strings.CutPrefix(l, "0x")
	if !ok {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// normalizeHost returns the canonical form of host: IP literals in their
// canonical text (brackets dropped, IPv4-mapped unmapped), names lower-cased
// without a trailing dot. Brackets around anything but an IPv6 literal yield
// "". The returned address is valid when host is an IP literal.
func normalizeHost(host string) (string, netip.Addr) {
	h := strings.TrimSpace(host)
	if strings.HasPrefix(h, "[") || strings.HasSuffix(h, "]") {
		if len(h) < 2 || h[0] != '[' || h[len(h)-1] != ']' {
			return "", netip.Addr{}
		}
		ip, err := netip.ParseAddr(h[1 : len(h)-1])
		if err != nil || !ip.Is6() {
			return "", netip.Addr{}
		}
		ip = ip.Unmap()
		return ip.String(), ip
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		ip = ip.Unmap()
		return ip.String(), ip
	}
	return strings.TrimSuffix(strings.ToLower(h), "."), netip.Addr{}
}

func (r rule) matches(host string, ip netip.Addr, port int) bool {
	if r.port != 0 {
		if port != r.port {
			return false
		}
	} else if port != 80 && port != 443 {
		return false
	}
	if ip.IsValid() || r.ip.IsValid() {
		// IP literals match only rules that list them literally.
		return ip.IsValid() && r.ip.IsValid() && ip == r.ip
	}
	if r.wildcard {
		return strings.HasSuffix(host, "."+r.host)
	}
	return host == r.host
}

// matchRules is Match on parsed rules.
func matchRules(rules []rule, host string, port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	h, ip := normalizeHost(host)
	if h == "" {
		return false
	}
	for _, r := range rules {
		if r.matches(h, ip, port) {
			return true
		}
	}
	return false
}

// parseRules parses rules, dropping invalid ones.
func parseRules(rules []Rule) []rule {
	out := make([]rule, 0, len(rules))
	for _, r := range rules {
		if p, ok := parseRule(r); ok {
			out = append(out, p)
		}
	}
	return out
}
