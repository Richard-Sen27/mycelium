package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		rules []Rule
		host  string
		port  int
		want  bool
	}{
		{[]Rule{"pypi.org"}, "pypi.org", 443, true},
		{[]Rule{"pypi.org"}, "pypi.org", 80, true},
		{[]Rule{"pypi.org"}, "pypi.org", 8080, false},
		{[]Rule{"pypi.org"}, "PyPI.ORG.", 443, true},
		{[]Rule{"PyPI.org."}, "pypi.org", 443, true},
		{[]Rule{"pypi.org"}, "files.pypi.org", 443, false},
		{[]Rule{"pypi.org"}, "evilpypi.org", 443, false},
		{[]Rule{"pypi.org"}, "[pypi.org]", 443, false},
		{[]Rule{"*.github.com"}, "api.github.com", 443, true},
		{[]Rule{"*.github.com"}, "a.b.github.com", 443, true},
		{[]Rule{"*.github.com"}, "github.com", 443, false},
		{[]Rule{"*.github.com"}, "evilgithub.com", 443, false},
		{[]Rule{"*.github.com:22"}, "ssh.github.com", 22, true},
		{[]Rule{"*.github.com:22"}, "ssh.github.com", 443, false},
		{[]Rule{"*.com"}, "example.com", 443, false},
		{[]Rule{"*.com:443"}, "example.com", 443, false},
		{[]Rule{"example.com:8443"}, "example.com", 8443, true},
		{[]Rule{"example.com:8443"}, "example.com", 443, false},
		{[]Rule{"10.0.0.5"}, "10.0.0.5", 443, true},
		{[]Rule{"10.0.0.5:9000"}, "10.0.0.5", 9000, true},
		{[]Rule{"10.0.0.5"}, "10.0.0.6", 443, false},
		{[]Rule{"10.0.0.5"}, "[10.0.0.5]", 443, false},
		{[]Rule{"[10.0.0.5]"}, "10.0.0.5", 443, false},
		{[]Rule{"[pypi.org]"}, "pypi.org", 443, false},
		{[]Rule{"*.0.5"}, "10.0.0.5", 443, false},
		{[]Rule{"example.com"}, "93.184.216.34", 443, false},
		{[]Rule{"::1"}, "::1", 443, true},
		{[]Rule{"[::1]:8080"}, "[::1]", 8080, true},
		{[]Rule{"[::1]"}, "::1", 80, true},
		{[]Rule{"::ffff:10.0.0.5"}, "10.0.0.5", 443, true},
		{[]Rule{""}, "", 443, false},
		{[]Rule{"*"}, "example.com", 443, false},
		{[]Rule{"*."}, "example.com", 443, false},
		{[]Rule{"ex*ample.com"}, "example.com", 443, false},
		{[]Rule{"example.com:"}, "example.com", 443, false},
		{[]Rule{"example.com:0"}, "example.com", 0, false},
		{[]Rule{"example.com:70000"}, "example.com", 443, false},
		{[]Rule{"example.com:abc"}, "example.com", 443, false},
		{[]Rule{"bad host"}, "bad host", 443, false},
		{[]Rule{"10.0.0.0/8"}, "10.0.0.1", 443, false},
		{[]Rule{"bad rule!", "ok.example"}, "ok.example", 443, true},
		{nil, "example.com", 443, false},
		{[]Rule{"example.com"}, "example.com", 0, false},
	}
	for _, tt := range tests {
		if got := Match(tt.rules, tt.host, tt.port); got != tt.want {
			t.Errorf("Match(%q, %q, %d) = %v, want %v", tt.rules, tt.host, tt.port, got, tt.want)
		}
	}
}

func TestRefusedAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"169.254.10.1", true},
		{"fe80::1", true},
		{"224.0.0.1", true},
		{"239.1.2.3", true},
		{"ff02::1", true},
		{"ff01::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"fd00::1", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"0.1.2.3", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"64:ff9b::808:808", true},
		{"2002:808:808::1", true},
		{"::ffff:10.0.0.1", true},
		// Teredo: server 8.8.8.8, client 10.0.0.1 (stored inverted).
		{"2001:0:808:808:0:0:f5ff:fffe", true},
		// Teredo: server 10.0.0.1, client 8.8.8.8.
		{"2001:0:a00:1:0:0:f7f7:f7f7", true},
		// Teredo with public server and client.
		{"2001:0:808:808:0:0:f7f7:f7f7", false},
		{"8.8.8.8", false},
		{"100.128.0.1", false},
		{"198.20.0.1", false},
		{"::ffff:8.8.8.8", false},
		{"2606:4700::1111", false},
	}
	for _, tt := range tests {
		if got := refusedAddr(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("refusedAddr(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestLocalAddrsRefresh(t *testing.T) {
	var mu sync.Mutex
	loads := 0
	current := []netip.Addr{netip.MustParseAddr("203.0.113.7")}
	l := &localAddrs{every: 30 * time.Second, load: func() ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		loads++
		return current, nil
	}}
	if !l.contains(netip.MustParseAddr("203.0.113.7")) || l.contains(netip.MustParseAddr("203.0.113.8")) {
		t.Fatal("initial set wrong")
	}
	mu.Lock()
	current = []netip.Addr{netip.MustParseAddr("203.0.113.8")}
	mu.Unlock()
	if !l.contains(netip.MustParseAddr("203.0.113.7")) {
		t.Fatal("refreshed before the interval")
	}
	l.mu.Lock()
	l.loaded = time.Now().Add(-31 * time.Second)
	l.mu.Unlock()
	if !l.contains(netip.MustParseAddr("203.0.113.8")) || l.contains(netip.MustParseAddr("203.0.113.7")) {
		t.Fatal("not refreshed after the interval")
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2", loads)
	}
	if !l.contains(netip.MustParseAddr("::ffff:203.0.113.8")) {
		t.Fatal("IPv4-mapped form not recognised")
	}
}

// TestNameResolutionRefused checks every refused class end to end with a
// fake resolver, and that names are looked up fully qualified.
func TestNameResolutionRefused(t *testing.T) {
	zone := map[string][]string{
		"private.test.":   {"10.0.0.1"},
		"private6.test.":  {"fd12::1"},
		"cgnat.test.":     {"100.64.1.1"},
		"zero.test.":      {"0.0.0.9"},
		"bench.test.":     {"198.18.3.4"},
		"reserved.test.":  {"241.0.0.1"},
		"bcast.test.":     {"255.255.255.255"},
		"nat64.test.":     {"64:ff9b::a00:1"},
		"sixto4.test.":    {"2002:a00:1::1"},
		"teredo.test.":    {"2001:0:808:808:0:0:f5ff:fffe"},
		"linklocal.test.": {"169.254.169.254"},
		"loop.test.":      {"127.0.0.1"},
		"mcast.test.":     {"224.0.0.251"},
		"self.test.":      {"203.0.113.7"},
		"mixed.test.":     {"8.8.8.8", "192.168.0.1"},
	}
	var mu sync.Mutex
	var asked []string
	p, al := startTuned(t, Options{}, func(tu *tuning) {
		tu.lookup = func(_ context.Context, name string) ([]netip.Addr, error) {
			mu.Lock()
			asked = append(asked, name)
			mu.Unlock()
			var out []netip.Addr
			for _, a := range zone[name] {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		}
		tu.ifaceAddrs = func() ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
		}
	})
	pu := register(t, p, "sb1", "*.test")
	user, pass := creds(pu)
	for name := range zone {
		host := strings.TrimSuffix(name, ".")
		resp, _, _ := rawConnect(t, p.Addr().String(), host+":443", user, pass)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", host, resp.StatusCode)
			continue
		}
		rec := al.wait(t, func(Record) bool { return true })
		if rec.Allowed || rec.Host != host || rec.Addr != "" {
			t.Errorf("%s: record = %+v", host, rec)
		}
	}
	mu.Lock()
	for _, n := range asked {
		if !strings.HasSuffix(n, ".") {
			t.Errorf("looked up %q without a trailing dot", n)
		}
	}
	mu.Unlock()
	if p.addrAllowed(netip.MustParseAddr("203.0.113.7")) || !p.addrAllowed(netip.MustParseAddr("203.0.113.8")) {
		t.Fatal("own-interface check wrong")
	}
}

func TestConnectTLSAndBytes(t *testing.T) {
	const body = "hello through the tunnel"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer ts.Close()
	p, al := startProxy(t, 0)
	port := portOf(t, ts.Listener.Addr().String())
	pu := register(t, p, "sb1", Rule("127.0.0.1:"+strconv.Itoa(port)))

	resp, err := clientVia(ts, pu).Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != body {
		t.Fatalf("body = %q", got)
	}
	rec := al.wait(t, func(r Record) bool { return r.Method == http.MethodConnect })
	if !rec.Allowed || rec.SandboxID != "sb1" || rec.Host != "127.0.0.1" || rec.Port != port || rec.Addr != ts.Listener.Addr().String() {
		t.Fatalf("record = %+v", rec)
	}
	if rec.BytesIn <= int64(len(body)) || rec.BytesOut <= 0 {
		t.Fatalf("bytes not counted: %+v", rec)
	}
}

func TestConnectDenied(t *testing.T) {
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", "pypi.org")
	user, pass := creds(pu)
	resp, _, _ := rawConnect(t, p.Addr().String(), "Example.Invalid.:443", user, pass)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	rec := al.wait(t, func(Record) bool { return true })
	if rec.Allowed || rec.SandboxID != "sb1" || rec.Host != "example.invalid" || rec.Port != 443 || rec.Method != http.MethodConnect {
		t.Fatalf("record = %+v", rec)
	}
}

func TestProxyAuth(t *testing.T) {
	echo := echoServer(t)
	p, al := startProxy(t, 0)
	a := register(t, p, "a", Rule(echo))
	b := register(t, p, "b")
	aUser, aPass := creds(a)
	bUser, bPass := creds(b)
	addr := p.Addr().String()

	cases := []struct {
		name, user, pass string
		want             int
	}{
		{"no credentials", "", "", http.StatusProxyAuthRequired},
		{"wrong token", aUser, aPass + "x", http.StatusProxyAuthRequired},
		{"unknown sandbox", "nobody", aPass, http.StatusProxyAuthRequired},
		{"other sandbox's token", aUser, bPass, http.StatusProxyAuthRequired},
		{"own token, own allowlist", bUser, bPass, http.StatusForbidden},
		{"valid", aUser, aPass, http.StatusOK},
	}
	for _, tc := range cases {
		resp, _, _ := rawConnect(t, addr, echo, tc.user, tc.pass)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
		if tc.want == http.StatusProxyAuthRequired {
			if got := resp.Header.Get("Proxy-Authenticate"); got != `Basic realm="mycel"` {
				t.Errorf("%s: Proxy-Authenticate = %q", tc.name, got)
			}
			rec := al.wait(t, func(Record) bool { return true })
			if rec.Allowed || rec.SandboxID != tc.user || rec.Host != "127.0.0.1" {
				t.Errorf("%s: auth failure record = %+v", tc.name, rec)
			}
		}
	}

	// Re-registering rotates the token.
	a2 := register(t, p, "a", Rule(echo))
	if _, pass2 := creds(a2); pass2 == aPass {
		t.Fatal("token not rotated")
	}
	resp, _, _ := rawConnect(t, addr, echo, aUser, aPass)
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("old token: status = %d", resp.StatusCode)
	}
}

func TestPlainHTTPForward(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, "echo:"+string(b))
	}))
	defer ts.Close()
	p, al := startProxy(t, 0)
	port := portOf(t, ts.Listener.Addr().String())
	pu := register(t, p, "sb1", Rule("127.0.0.1:"+strconv.Itoa(port)))

	resp, err := clientVia(ts, pu).Post(ts.URL+"/x", "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "echo:ping" || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("resp = %d %q %v", resp.StatusCode, got, resp.Header)
	}
	mu.Lock()
	if v := seen.Get("Proxy-Authorization"); v != "" {
		t.Fatalf("Proxy-Authorization forwarded: %q", v)
	}
	mu.Unlock()
	rec := al.wait(t, func(Record) bool { return true })
	if !rec.Allowed || rec.SandboxID != "sb1" || rec.Host != "127.0.0.1" || rec.Port != port ||
		rec.Method != http.MethodPost || rec.Addr != ts.Listener.Addr().String() ||
		rec.BytesIn <= int64(len("echo:ping")) || rec.BytesOut <= 4 {
		t.Fatalf("record = %+v", rec)
	}

	// Not allowlisted: 403 without reaching upstream.
	resp, err = clientVia(ts, pu).Get("http://example.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	rec = al.wait(t, func(Record) bool { return true })
	if rec.Allowed || rec.Host != "example.invalid" || rec.Port != 80 || rec.Method != http.MethodGet {
		t.Fatalf("record = %+v", rec)
	}
}

// TestForwardWireBytesAndHopHeaders checks exact wire byte counts and that
// hop-by-hop headers, including those named in Connection, are stripped in
// both directions.
func TestForwardWireBytesAndHopHeaders(t *testing.T) {
	const response = "HTTP/1.1 200 OK\r\n" +
		"Content-Length: 5\r\n" +
		"Connection: X-Up-Hop, close\r\n" +
		"X-Up-Hop: secret\r\n" +
		"Keep-Alive: timeout=5\r\n" +
		"Proxy-Authenticate: Basic\r\n" +
		"X-Kept: yes\r\n" +
		"\r\n" +
		"hello"
	up := newRawUpstream(t, response)
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(up.addr))
	user, pass := creds(pu)

	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	req := "POST http://" + up.addr + "/x HTTP/1.1\r\n" +
		"Host: " + up.addr + "\r\n" +
		"Proxy-Authorization: " + basic(user, pass) + "\r\n" +
		"Connection: X-Client-Hop\r\n" +
		"X-Client-Hop: 1\r\n" +
		"Keep-Alive: 5\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"X-Client-Kept: 1\r\n" +
		"Content-Length: 4\r\n\r\nping"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" || resp.Header.Get("X-Kept") != "yes" {
		t.Fatalf("resp = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	for _, h := range []string{"X-Up-Hop", "Keep-Alive", "Proxy-Authenticate"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("response header %s forwarded: %q", h, v)
		}
	}
	seen := <-up.reqs
	upRead := <-up.read
	for _, h := range []string{"X-Client-Hop", "Keep-Alive", "Proxy-Connection", "Proxy-Authorization"} {
		if v := seen.Header.Get(h); v != "" {
			t.Errorf("request header %s forwarded: %q", h, v)
		}
	}
	if seen.Header.Get("X-Client-Kept") != "1" {
		t.Error("end-to-end request header dropped")
	}
	rec := al.wait(t, func(Record) bool { return true })
	if rec.BytesIn != int64(len(response)) || rec.BytesOut != upRead {
		t.Fatalf("record = %+v, want in=%d out=%d", rec, len(response), upRead)
	}
}

func TestHTTP10Client(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "old school")
	}))
	defer ts.Close()
	p, _ := startProxy(t, 0)
	host := ts.Listener.Addr().String()
	pu := register(t, p, "sb1", Rule(host))
	user, pass := creds(pu)
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "GET http://"+host+"/ HTTP/1.0\r\nProxy-Authorization: "+basic(user, pass)+"\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	all, err := io.ReadAll(c) // HTTP/1.0: the proxy closes after the response
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(all), "HTTP/1.0 200") || !strings.HasSuffix(string(all), "old school") {
		t.Fatalf("response = %q", all)
	}
}

func TestForwardLoopbackNameRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("upstream reached")
	}))
	defer ts.Close()
	port := strconv.Itoa(portOf(t, ts.Listener.Addr().String()))
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule("localhost:"+port))
	resp, err := clientVia(nil, pu).Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	rec := al.wait(t, func(Record) bool { return true })
	if rec.Allowed || rec.Host != "localhost" || rec.Method != http.MethodGet {
		t.Fatalf("record = %+v", rec)
	}
}

func TestBadRequests(t *testing.T) {
	p, _ := startProxy(t, 0)
	resp, err := http.Get("http://" + p.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("origin-form request: status = %d, want 400", resp.StatusCode)
	}
	pu := register(t, p, "sb1", "example.com")
	user, pass := creds(pu)
	for _, target := range []string{"example.com", "[example.com]:443", "example.com:0", "example.com:99999"} {
		r, _, _ := rawConnect(t, p.Addr().String(), target, user, pass)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("CONNECT %s: status = %d, want 400", target, r.StatusCode)
		}
	}
}

func TestUpdate(t *testing.T) {
	echo := echoServer(t)
	p, _ := startProxy(t, 0)
	pu := register(t, p, "sb1")
	user, pass := creds(pu)
	resp, _, _ := rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("before update: status = %d", resp.StatusCode)
	}
	if err := p.Update("sb1", []Rule{Rule(echo)}); err != nil {
		t.Fatal(err)
	}
	resp, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after update: status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "hi")
	if err := p.Update("nope", nil); err == nil {
		t.Fatal("Update of an unknown sandbox succeeded")
	}
}

func TestUpdateRevokesLiveConnections(t *testing.T) {
	echo1, echo2 := echoServer(t), echoServer(t)
	p, _ := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo1), Rule(echo2))
	user, pass := creds(pu)
	_, c1, br1 := rawConnect(t, p.Addr().String(), echo1, user, pass)
	_, c2, br2 := rawConnect(t, p.Addr().String(), echo2, user, pass)
	echoOnce(t, c1, br1, "a")
	echoOnce(t, c2, br2, "b")

	if err := p.Update("sb1", []Rule{Rule(echo2)}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c1, br1, "revoked tunnel")
	echoOnce(t, c2, br2, "still here")
}

func TestReRegisterClosesConnections(t *testing.T) {
	echo := echoServer(t)
	p, _ := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	_, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	echoOnce(t, c, br, "a")
	pu2 := register(t, p, "sb1", Rule(echo))
	expectClosed(t, c, br, "tunnel after re-register")
	u2, p2 := creds(pu2)
	resp, c2, br2 := rawConnect(t, p.Addr().String(), echo, u2, p2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new token: status = %d", resp.StatusCode)
	}
	echoOnce(t, c2, br2, "b")
}

func TestUnregisterClosesConnections(t *testing.T) {
	echo := echoServer(t)
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	resp, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "abc")
	p.Unregister("sb1")
	expectClosed(t, c, br, "tunnel after Unregister")
	rec := al.wait(t, func(r Record) bool { return r.Method == http.MethodConnect })
	if !rec.Allowed || rec.BytesIn != 3 || rec.BytesOut != 3 {
		t.Fatalf("record = %+v", rec)
	}
	resp, _, _ = rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("after Unregister: status = %d", resp.StatusCode)
	}
}

func TestConnectionCap(t *testing.T) {
	echo := echoServer(t)
	p, al := startProxy(t, 2)
	pu := register(t, p, "sb1", Rule(echo))
	other := register(t, p, "sb2", Rule(echo))
	user, pass := creds(pu)
	addr := p.Addr().String()

	var conns []net.Conn
	for range 2 {
		resp, c, br := rawConnect(t, addr, echo, user, pass)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		echoOnce(t, c, br, "x")
		conns = append(conns, c)
	}
	resp, _, _ := rawConnect(t, addr, echo, user, pass)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over cap: status = %d, want 429", resp.StatusCode)
	}
	rec := al.wait(t, func(r Record) bool { return !r.Allowed })
	if rec.SandboxID != "sb1" || rec.Method != http.MethodConnect || rec.Host != "127.0.0.1" {
		t.Fatalf("429 record = %+v", rec)
	}
	// The cap is per sandbox.
	ou, op := creds(other)
	resp, _, _ = rawConnect(t, addr, echo, ou, op)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other sandbox: status = %d", resp.StatusCode)
	}
	// Closing one frees a slot.
	_ = conns[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, c, _ := rawConnect(t, addr, echo, user, pass)
		if resp.StatusCode == http.StatusOK {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed: status = %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDenialRateLimit(t *testing.T) {
	p, al := startTuned(t, Options{}, func(tu *tuning) {
		tu.denyRate = 0.001
		tu.denyBurst = 3
	})
	pu := register(t, p, "sb1")
	other := register(t, p, "sb2")
	user, pass := creds(pu)
	for i := range 5 {
		resp, _, _ := rawConnect(t, p.Addr().String(), "example.invalid:443", user, pass)
		want := http.StatusForbidden
		if i >= 3 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("attempt %d: status = %d, want %d", i, resp.StatusCode, want)
		}
	}
	// Other sandboxes have their own budget.
	ou, op := creds(other)
	resp, _, _ := rawConnect(t, p.Addr().String(), "example.invalid:443", ou, op)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other sandbox: status = %d", resp.StatusCode)
	}
	// Failed authentication draws from the same budget: audited only within it.
	for range 3 {
		resp, _, _ := rawConnect(t, p.Addr().String(), "example.invalid:443", user, "wrong")
		if resp.StatusCode != http.StatusProxyAuthRequired {
			t.Fatalf("bad token: status = %d", resp.StatusCode)
		}
	}
	n := 0
	for _, r := range al.all() {
		if r.SandboxID == "sb1" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("sb1 audit records = %d, want 3", n)
	}
}

func TestLoopbackRefusedUnlessListed(t *testing.T) {
	echo := echoServer(t)
	port := strconv.Itoa(portOf(t, echo))
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule("localhost:"+port))
	user, pass := creds(pu)

	resp, _, _ := rawConnect(t, p.Addr().String(), "localhost:"+port, user, pass)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("name resolving to loopback: status = %d, want 403", resp.StatusCode)
	}
	rec := al.wait(t, func(Record) bool { return true })
	if rec.Allowed || rec.Host != "localhost" {
		t.Fatalf("record = %+v", rec)
	}

	if err := p.Update("sb1", []Rule{Rule("127.0.0.1:" + port)}); err != nil {
		t.Fatal(err)
	}
	resp, c, br := rawConnect(t, p.Addr().String(), "127.0.0.1:"+port, user, pass)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listed IP literal: status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "ok")
}

func TestBracketedIPv6Connect(t *testing.T) {
	echo := echoServerOn(t, "tcp6", "[::1]:0")
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	resp, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "v6")
	_ = c.Close()
	rec := al.wait(t, func(r Record) bool { return r.Method == http.MethodConnect })
	if rec.Host != "::1" || rec.Addr != echo {
		t.Fatalf("record = %+v", rec)
	}
}

func TestPipelinedBytesAfterConnect(t *testing.T) {
	echo := echoServer(t)
	p, al := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	resp, c, br, err := tryConnect(p.Addr().String(), echo, user, pass, "early bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	readExactly(t, c, br, "early bytes")
	_ = c.Close()
	rec := al.wait(t, func(r Record) bool { return r.Method == http.MethodConnect })
	if rec.BytesOut != int64(len("early bytes")) || rec.BytesIn != int64(len("early bytes")) {
		t.Fatalf("record = %+v", rec)
	}
}

func TestHalfClose(t *testing.T) {
	echo := echoServer(t)
	p, _ := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	_, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	if _, err := io.WriteString(c, "last words"); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "last words" {
		t.Fatalf("got %q after half-close", got)
	}
}

func TestCloseStopsEverything(t *testing.T) {
	echo := echoServer(t)
	p, _ := startProxy(t, 0)
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	resp, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, br, "tunnel after Close")
	if _, err := p.Register("sb2", nil); err == nil {
		t.Fatal("Register after Close succeeded")
	}
}

func TestCloseDuringDial(t *testing.T) {
	entered := make(chan struct{}, 1)
	p, _ := startTuned(t, Options{}, func(tu *tuning) {
		tu.lookup = func(ctx context.Context, _ string) ([]netip.Addr, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	pu := register(t, p, "sb1", "slow.test")
	user, pass := creds(pu)
	result := make(chan int, 1)
	go func() {
		resp, c, _, err := tryConnect(p.Addr().String(), "slow.test:443", user, pass, "")
		if err != nil {
			result <- 0
			return
		}
		_ = c.Close()
		result <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dial never started")
	}
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an in-flight dial")
	}
	if st := <-result; st == http.StatusOK {
		t.Fatal("tunnel established after Close")
	}
}

func TestIdleTimeout(t *testing.T) {
	echo := echoServer(t)
	p, al := startTuned(t, Options{}, func(tu *tuning) { tu.idle = 200 * time.Millisecond })
	pu := register(t, p, "sb1", Rule(echo))
	user, pass := creds(pu)
	resp, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	expectClosed(t, c, br, "idle tunnel")
	al.wait(t, func(r Record) bool { return r.Method == http.MethodConnect })
}

func TestHTTPKeepAliveIdleClosed(t *testing.T) {
	p, _ := startTuned(t, Options{}, func(tu *tuning) { tu.httpIdle = 200 * time.Millisecond })
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusProxyAuthRequired || resp.Close {
		t.Fatalf("status = %d close = %v", resp.StatusCode, resp.Close)
	}
	expectClosed(t, c, br, "idle keep-alive connection")
}

// openServed opens a connection and proves the proxy serves it.
func openServed(t *testing.T, addr string) (net.Conn, bool) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return c, false
	}
	_ = resp.Body.Close()
	return c, resp.StatusCode == http.StatusBadRequest
}

func TestListenerLimits(t *testing.T) {
	for _, tc := range []struct {
		name         string
		total, perIP int
	}{
		{"per IP", 100, 2},
		{"global", 2, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := startTuned(t, Options{}, func(tu *tuning) {
				tu.maxConnsTotal = tc.total
				tu.maxConnsPerIP = tc.perIP
			})
			addr := p.Addr().String()
			first, ok := openServed(t, addr)
			if !ok {
				t.Fatal("first connection not served")
			}
			if _, ok := openServed(t, addr); !ok {
				t.Fatal("second connection not served")
			}
			if _, ok := openServed(t, addr); ok {
				t.Fatal("third connection served over the limit")
			}
			_ = first.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, ok := openServed(t, addr); ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("slot not freed after close")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestTokenNeverLogged(t *testing.T) {
	var buf syncBuffer
	echo := echoServer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()
	p, _ := startTuned(t, Options{Logger: debugLogger(&buf)}, nil)
	pu := register(t, p, "sb1", Rule(echo), Rule(ts.Listener.Addr().String()))
	user, pass := creds(pu)
	_, c, br := rawConnect(t, p.Addr().String(), echo, user, pass)
	echoOnce(t, c, br, "x")
	_ = c.Close()
	rawConnect(t, p.Addr().String(), echo, user, "wrong-secret-value")
	rawConnect(t, p.Addr().String(), "example.invalid:443", user, pass)
	resp, err := clientVia(ts, pu).Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := p.Update("sb1", nil); err != nil {
		t.Fatal(err)
	}
	p.Unregister("sb1")
	_ = p.Close()

	out := buf.String()
	if out == "" {
		t.Fatal("nothing logged")
	}
	for _, secret := range []string{pass, "wrong-secret-value", basic(user, pass)[len("Basic "):]} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q found in logs", secret)
		}
	}
}

// TestHammer runs tunnels concurrently with registry changes and Close
// under the race detector.
func TestHammer(t *testing.T) {
	echo := echoServer(t)
	p, _ := startTuned(t, Options{MaxConnsPerSandbox: 8}, nil)
	ids := []string{"h1", "h2", "h3"}
	var mu sync.Mutex
	urls := map[string][2]string{}
	reg := func(id string) {
		raw, err := p.Register(id, []Rule{Rule(echo)})
		if err != nil {
			return
		}
		pu := mustParse(raw)
		u, pw := creds(pu)
		mu.Lock()
		urls[id] = [2]string{u, pw}
		mu.Unlock()
	}
	for _, id := range ids {
		reg(id)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := ids[(w+i)%len(ids)]
				mu.Lock()
				cr := urls[id]
				mu.Unlock()
				resp, c, br, err := tryConnect(p.Addr().String(), echo, cr[0], cr[1], "")
				if err != nil {
					continue
				}
				if resp.StatusCode == http.StatusOK {
					_, _ = io.WriteString(c, "ping")
					_ = c.SetReadDeadline(time.Now().Add(time.Second))
					_, _ = io.ReadFull(br, make([]byte, 4))
				}
				_ = c.Close()
			}
		}()
	}
	for i := range 60 {
		id := ids[i%len(ids)]
		switch i % 3 {
		case 0:
			_ = p.Update(id, nil)
		case 1:
			p.Unregister(id)
		case 2:
			reg(id)
			_ = p.Update(id, []Rule{Rule(echo)})
		}
		time.Sleep(2 * time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung under load")
	}
	close(stop)
	wg.Wait()
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func TestColonIDsAndUnregisterTotals(t *testing.T) {
	const body = "payload through tunnel"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer ts.Close()
	p, al := startProxy(t, 0)
	port := portOf(t, ts.Listener.Addr().String())
	const id = "sbx:01JTESTULID000000000000000"
	pu := register(t, p, id, Rule("127.0.0.1:"+strconv.Itoa(port)))
	if strings.Contains(pu.String(), "sbx") {
		t.Fatalf("proxy URL exposes the sandbox id: %s", pu.String())
	}
	resp, err := clientVia(ts, pu).Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	rec := al.wait(t, func(r Record) bool { return r.Method == http.MethodConnect })
	if rec.SandboxID != id {
		t.Fatalf("audit record carries %q, want the real id", rec.SandboxID)
	}
	in, out := p.Unregister(id)
	if in <= int64(len(body)) || out <= 0 {
		t.Fatalf("totals in=%d out=%d", in, out)
	}
	if in2, out2 := p.Unregister(id); in2 != 0 || out2 != 0 {
		t.Fatalf("second Unregister returned %d/%d", in2, out2)
	}
}

func TestAliasesReachBrokers(t *testing.T) {
	var seenHost atomic.Value
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost.Store(r.Host)
		_, _ = io.WriteString(w, "broker ok")
	}))
	defer broker.Close()
	p, al := startProxy(t, 0)
	pu := register(t, p, "sbx:alias") // no allowlist at all
	if err := p.SetAliases("sbx:alias", map[string]string{"ollama.mycel.internal:80": broker.Listener.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetAliases("sbx:alias", map[string]string{"bad": "127.0.0.1:1"}); err == nil {
		t.Fatal("alias without a port accepted")
	}
	if err := p.SetAliases("sbx:alias", map[string]string{"x.internal:80": "ollama.example:80"}); err == nil {
		t.Fatal("non-literal alias target accepted")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := client.Get("http://ollama.mycel.internal/api/tags")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "broker ok" || seenHost.Load() != "ollama.mycel.internal" {
		t.Fatalf("alias forward: %d %q host %v", resp.StatusCode, body, seenHost.Load())
	}
	rec := al.wait(t, func(r Record) bool { return r.Host == "ollama.mycel.internal" })
	if !rec.Allowed || rec.Addr != broker.Listener.Addr().String() {
		t.Fatalf("audit = %+v", rec)
	}
	// Other destinations are still refused without an allowlist.
	resp2, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("non-alias destination: %d", resp2.StatusCode)
	}
}
