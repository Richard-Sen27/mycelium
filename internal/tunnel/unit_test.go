package tunnel

import (
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testFP = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRelayURLRoundTrip(t *testing.T) {
	secret, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	u := &RelayURL{Host: "relay.example.com", Port: 7790, Name: "hub1", Fingerprint: testFP, Token: secret}
	got, err := ParseRelayURL(u.String())
	if err != nil {
		t.Fatalf("parse %q: %v", u.String(), err)
	}
	if *got != *u {
		t.Fatalf("round trip: got %+v, want %+v", got, u)
	}
	if strings.Contains(u.Redacted(), secret) || u.Redacted() != "relay://relay.example.com:7790/hub1" {
		t.Fatalf("Redacted = %q", u.Redacted())
	}
	// Default port.
	d, err := ParseRelayURL("relay://r.example/hub1?fp=" + testFP + "&token=" + secret)
	if err != nil || d.Port != DefaultControlPort {
		t.Fatalf("default port: %+v %v", d, err)
	}
	for _, bad := range []string{
		"https://r.example:7790/hub1?fp=" + testFP + "&token=" + secret,
		"relay://:7790/hub1?fp=" + testFP + "&token=" + secret,
		"relay://r.example:0/hub1?fp=" + testFP + "&token=" + secret,
		"relay://r.example:7790/?fp=" + testFP + "&token=" + secret,
		"relay://r.example:7790/Hub_1?fp=" + testFP + "&token=" + secret,
		"relay://r.example:7790/hub1?fp=abc&token=" + secret,
		"relay://r.example:7790/hub1?fp=" + testFP + "&token=short",
		"relay://r.example:7790/hub1?fp=" + testFP,
	} {
		if _, err := ParseRelayURL(bad); err == nil {
			t.Errorf("ParseRelayURL(%q) accepted", bad)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks the secret: %v", err)
		}
	}
}

func TestRelayCertAndTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	c1, fp1, err := LoadOrCreateRelayCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf := c1.Leaf
	if leaf == nil {
		t.Fatal("no leaf")
	}
	if d := time.Until(leaf.NotAfter); d < 9*365*24*time.Hour {
		t.Fatalf("certificate valid for %v, want ~10 years", d)
	}
	if _, fp2, err := LoadOrCreateRelayCert(dir); err != nil || fp2 != fp1 {
		t.Fatalf("second load: fp %s vs %s, %v", fp2, fp1, err)
	}
	old, err := CreateToken(dir, "hub0", false)
	if err != nil {
		t.Fatal(err)
	}
	// One hub per relay: any existing token blocks a new one ...
	for _, name := range []string{"hub0", "hub1"} {
		if _, err := CreateToken(dir, name, false); !errors.Is(err, ErrTokenExists) {
			t.Fatalf("%s: second token: %v, want ErrTokenExists", name, err)
		}
	}
	// ... unless replaced, which revokes it.
	secret, err := CreateToken(dir, "hub1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateToken(dir, "Bad Name", true); err == nil {
		t.Fatal("invalid name accepted")
	}
	raw, err := os.ReadFile(filepath.Join(dir, relayTokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("tokens.json holds the secret")
	}
	if strings.Contains(string(raw), hashSecret(old)) || strings.Contains(string(raw), "hub0") {
		t.Fatal("--replace kept the old token")
	}
	if !strings.Contains(string(raw), hashSecret(secret)) {
		t.Fatal("tokens.json lacks the hash")
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{relayCertFile, relayKeyFile, relayTokensFile} {
			st, err := os.Stat(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
			}
		}
	}
	toks, err := ListTokens(dir)
	if err != nil || len(toks) != 1 || toks[0].Name != "hub1" {
		t.Fatalf("ListTokens = %+v, %v", toks, err)
	}
	if err := RevokeToken(dir, "hub1"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeToken(dir, "hub1"); err == nil {
		t.Fatal("second revoke succeeded")
	}
}

func TestSourceLimiter(t *testing.T) {
	l := newSourceLimiter(4, 100)
	now := time.Now()
	for i := 0; i < 4; i++ {
		if !l.allow("a", now) {
			t.Fatalf("connection %d refused", i)
		}
	}
	if l.allow("a", now) {
		t.Fatal("burst exceeded")
	}
	if !l.allow("b", now) {
		t.Fatal("other source refused")
	}
	if !l.allow("a", now.Add(300*time.Millisecond)) {
		t.Fatal("no refill")
	}
	l.prune(now.Add(time.Minute))
	if len(l.m) != 0 || l.lru.Len() != 0 {
		t.Fatalf("prune left %d buckets", len(l.m))
	}

	// A flood of new sources evicts the least recently seen bucket instead
	// of refusing every new source (which would lock out the real hub).
	l = newSourceLimiter(1, 3)
	if !l.allow("hub", now) {
		t.Fatal("hub refused")
	}
	for i := range 100 {
		if !l.allow("flood"+strconv.Itoa(i), now) {
			t.Fatalf("new source %d refused with a full table", i)
		}
		if len(l.m) > 3 || l.lru.Len() != len(l.m) {
			t.Fatalf("table size %d/%d", len(l.m), l.lru.Len())
		}
	}
	if l.allow("flood99", now) {
		t.Fatal("a source in the table got a second token")
	}
}

func TestSourceKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7:443":                  "192.0.2.7",
		"[::ffff:192.0.2.7]:1":           "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:443":     "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff:ffff::9]:80": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:80":           "2001:db8:1:3::/64",
	}
	for in, want := range cases {
		a, err := net.ResolveTCPAddr("tcp", in)
		if err != nil {
			t.Fatal(err)
		}
		if got := sourceKey(a); got != want {
			t.Errorf("sourceKey(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanRemoteText(t *testing.T) {
	if got := cleanRemoteText("bad\x1b[31m\nline\u202eend\x00"); strings.ContainsAny(got, "\x1b\n\u202e\x00") {
		t.Fatalf("control characters kept: %q", got)
	}
	long := cleanRemoteText(strings.Repeat("é", 1000))
	if n := len([]rune(long)); n != maxRemoteText+3 {
		t.Fatalf("length %d runes", n)
	}
	if got := cleanRemoteText("port 7778 is not public"); got != "port 7778 is not public" {
		t.Fatalf("plain text changed: %q", got)
	}
}

func TestViaRelay(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if ViaRelay(a) || ViaRelay(tls.Client(a, &tls.Config{MinVersion: tls.VersionTLS12})) {
		t.Fatal("a plain connection reported as relayed")
	}
	sc := &streamConn{remote: srcAddr("x")}
	if !ViaRelay(sc) || !ViaRelay(tls.Server(sc, &tls.Config{MinVersion: tls.VersionTLS12})) {
		t.Fatal("a tunnel stream not reported as relayed")
	}
}

func TestConfigPorts(t *testing.T) {
	var c Config
	if c.PortMap()[443] != RoleAPI || c.PortMap()[7778] != RoleBus {
		t.Fatalf("defaults: %v", c.PortMap())
	}
	if p, ok := c.PublicPort(RoleBus); !ok || p != 7778 {
		t.Fatalf("PublicPort(bus) = %d %v", p, ok)
	}
	if err := (Config{Ports: map[int]string{443: "web"}}).Validate(); err == nil {
		t.Fatal("unknown role accepted")
	}
	if err := (Config{Ports: map[int]string{70000: RoleAPI}}).Validate(); err == nil {
		t.Fatal("bad port accepted")
	}
}

func TestValidateACME(t *testing.T) {
	ok := []struct {
		url string
		tun *Config
	}{
		{"https://hub.example.com", nil},
		{"https://hub.example.com:443", nil},
		{"https://relay.example.org", &Config{Enabled: true}},
	}
	for _, c := range ok {
		if err := ValidateACME(c.url, "ops@example.com", c.tun); err != nil {
			t.Errorf("%s: %v", c.url, err)
		}
	}
	bad := []struct {
		url, email string
		tun        *Config
	}{
		{"https://hub.example.com:7777", "", nil},
		{"https://10.1.2.3", "", nil},
		{"https://localhost", "", nil},
		{"https://hub.local", "", nil},
		{"https://hub", "", nil},
		{"http://hub.example.com", "", nil},
		{"https://hub.example.com", "not-an-email", nil},
		{"https://relay.example.org", "", &Config{Enabled: true, Ports: map[int]string{8443: RoleAPI, 443: RoleBus}}},
	}
	for _, c := range bad {
		if err := ValidateACME(c.url, c.email, c.tun); err == nil {
			t.Errorf("%s %q accepted", c.url, c.email)
		}
	}
}
