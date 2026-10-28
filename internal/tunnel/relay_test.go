package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// testRelay is a relay on loopback with public ports 443 and 7778.
type testRelay struct {
	*Relay
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
}

func startRelay(t *testing.T, dir string, mod func(*RelayOptions)) *testRelay {
	t.Helper()
	opts := RelayOptions{
		DataDir: dir, Listen: "127.0.0.1:0",
		Public: map[int]string{443: "127.0.0.1:0", 7778: "127.0.0.1:0"},
		Logger: quietLog(), TokenCheck: 50 * time.Millisecond,
	}
	if mod != nil {
		mod(&opts)
	}
	r, err := NewRelay(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tr := &testRelay{Relay: r, dir: dir, cancel: cancel, done: make(chan struct{})}
	go func() { _ = r.Serve(ctx); close(tr.done) }()
	t.Cleanup(tr.stop)
	return tr
}

func (r *testRelay) stop() {
	r.cancel()
	<-r.done
}

// relayURL creates a token and returns the hub's relay URL.
func (r *testRelay) relayURL(t *testing.T, name string) *RelayURL {
	t.Helper()
	secret, err := CreateToken(r.dir, name, false)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(r.ControlAddr())
	p, _ := strconv.Atoi(port)
	return &RelayURL{Host: "127.0.0.1", Port: p, Name: name, Fingerprint: r.Fingerprint(), Token: secret}
}

// testHub is a hub stand-in: an HTTPS server fed by the tunnel client and a
// TCP echo server as the "bus".
type testHub struct {
	client *Client
	ca     *testCA
	srv    *http.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
	remote chan string
}

func startHub(t *testing.T, u *RelayURL, ports map[int]string) *testHub {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.(*net.TCPConn).CloseWrite()
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	if ports == nil {
		ports = DefaultPorts()
	}
	c, err := NewClient(ClientOptions{
		URL: u, Ports: ports, BusAddr: echo.Addr().String(), Logger: quietLog(),
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, IdleTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ca := newTestCA(t, "hub ca")
	h := &testHub{client: c, ca: ca, remote: make(chan string, 16)}
	h.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case h.remote <- r.RemoteAddr:
			default:
			}
			_, _ = io.WriteString(w, "hello from the hub")
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{ca.issue(t, "hub.example.com")}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.wg.Add(2)
	go func() { defer h.wg.Done(); c.Run(ctx) }()
	go func() { defer h.wg.Done(); _ = h.srv.ServeTLS(c.APIListener(), "", "") }()
	t.Cleanup(h.stop)
	return h
}

func (h *testHub) stop() {
	h.cancel()
	_ = h.srv.Close()
	h.wg.Wait()
}

func (h *testHub) waitConnected(t *testing.T) {
	t.Helper()
	eventually(t, 10*time.Second, "tunnel connected", func() bool { return h.client.Status().Connected })
}

// get fetches https://hub.example.com/ through the relay's public port.
func (h *testHub) get(r *testRelay) (string, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: h.ca.pool, ServerName: "hub.example.com", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", r.PublicAddr(443))
		},
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	res, err := hc.Get("https://hub.example.com/")
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

// closedQuickly reports whether a connection to addr is closed by the peer
// without any data.
func closedQuickly(t *testing.T, addr string) bool {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return true
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Read(make([]byte, 1))
	return err != nil && !isTimeout(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func echoOnce(t *testing.T, addr, msg string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	// Half-close: the echo answers, then closes its side.
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return string(b)
}

func TestRelayEndToEnd(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), nil)
	// No hub attached: public connections are closed at once.
	if !closedQuickly(t, r.PublicAddr(443)) {
		t.Fatal("public connection without a hub was not closed")
	}
	h := startHub(t, r.relayURL(t, "hub1"), nil)
	h.waitConnected(t)

	body, err := h.get(r)
	if err != nil || body != "hello from the hub" {
		t.Fatalf("GET through relay: %q %v", body, err)
	}
	// The hub sees the original source address, not the relay session.
	if ra := <-h.remote; !strings.HasPrefix(ra, "127.0.0.1:") {
		t.Fatalf("RemoteAddr = %q", ra)
	}
	// Bus: raw bytes both ways with half-close.
	big := strings.Repeat("0123456789", 20000)
	if got := echoOnce(t, r.PublicAddr(7778), big); got != big {
		t.Fatalf("bus echo: %d bytes back, want %d", len(got), len(big))
	}
	st := h.client.Status()
	if !st.Connected || st.Total < 2 || st.Connects != 1 || strings.Contains(st.Relay, "token") {
		t.Fatalf("status = %+v", st)
	}
	if rs := r.Stats(); rs.Hub != "hub1" || rs.Total < 2 {
		t.Fatalf("relay stats = %+v", rs)
	}
	eventually(t, 5*time.Second, "connections closed", func() bool { return r.Stats().Active == 0 })
}

func TestRelayHelloValidation(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), func(o *RelayOptions) {
		o.Public = map[int]string{443: "127.0.0.1:0"}
	})
	u := r.relayURL(t, "hub1")
	line := func(h hello) []byte { b, _ := json.Marshal(h); return b }
	ports := map[string]string{"443": RoleAPI}
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"ok", line(hello{V: 1, Name: "hub1", Token: u.Token, Ports: ports}), ""},
		{"malformed", []byte("{"), "malformed hello"},
		{"version", line(hello{V: 2, Name: "hub1", Token: u.Token, Ports: ports}), "unsupported protocol version"},
		{"bad token", line(hello{V: 1, Name: "hub1", Token: u.Token + "x", Ports: ports}), "unauthorized"},
		{"unknown hub", line(hello{V: 1, Name: "hub2", Token: u.Token, Ports: ports}), "unauthorized"},
		{"empty token", line(hello{V: 1, Name: "hub1", Ports: ports}), "unauthorized"},
		{"no ports", line(hello{V: 1, Name: "hub1", Token: u.Token}), "no ports requested"},
		{"port not public", line(hello{V: 1, Name: "hub1", Token: u.Token, Ports: map[string]string{"7778": RoleBus}}), "port 7778 is not public on this relay"},
		{"bad role", line(hello{V: 1, Name: "hub1", Token: u.Token, Ports: map[string]string{"443": "ssh"}}), "port 443: unknown role"},
		{"bad port", line(hello{V: 1, Name: "hub1", Token: u.Token, Ports: map[string]string{"x": RoleAPI}}), `invalid port "x"`},
	}
	for _, c := range cases {
		s, reason := r.checkHello(c.in)
		if (s != nil) != (c.want == "") || reason != c.want {
			t.Errorf("%s: session %v, reason %q, want %q", c.name, s != nil, reason, c.want)
		}
	}

	// Through the client: a port the relay does not expose is refused and
	// reported in the status.
	h := startHub(t, u, map[int]string{443: RoleAPI, 7778: RoleBus})
	eventually(t, 5*time.Second, "refusal", func() bool {
		return strings.Contains(h.client.Status().LastError, "port 7778 is not public")
	})
	if h.client.Status().Connected {
		t.Fatal("connected despite refusal")
	}
}

func TestRelayWrongFingerprint(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), nil)
	u := r.relayURL(t, "hub1")
	u.Fingerprint = testFP
	h := startHub(t, u, nil)
	eventually(t, 5*time.Second, "pin failure", func() bool {
		return strings.Contains(h.client.Status().LastError, "fingerprint")
	})
	if r.Stats().Sessions != 0 {
		t.Fatal("relay accepted a session")
	}
}

func TestRelayLimits(t *testing.T) {
	t.Run("connection cap", func(t *testing.T) {
		r := startRelay(t, filepath.Join(t.TempDir(), "relay"), func(o *RelayOptions) { o.MaxConns = 1 })
		h := startHub(t, r.relayURL(t, "hub1"), nil)
		h.waitConnected(t)
		held, err := net.Dial("tcp", r.PublicAddr(7778))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Close() }()
		eventually(t, 5*time.Second, "first connection forwarded", func() bool { return r.Stats().Active == 1 })
		if !closedQuickly(t, r.PublicAddr(7778)) {
			t.Fatal("second connection over the cap was not closed")
		}
		if r.Stats().Refused == 0 {
			t.Fatal("refusal not counted")
		}
	})
	t.Run("idle timeout", func(t *testing.T) {
		r := startRelay(t, filepath.Join(t.TempDir(), "relay"), func(o *RelayOptions) { o.IdleTimeout = 300 * time.Millisecond })
		h := startHub(t, r.relayURL(t, "hub1"), nil)
		h.waitConnected(t)
		c, err := net.Dial("tcp", r.PublicAddr(7778))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		start := time.Now()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
			t.Fatalf("idle connection not closed: %v", err)
		}
		if d := time.Since(start); d < 250*time.Millisecond {
			t.Fatalf("closed after %v, before the idle timeout", d)
		}
	})
	t.Run("rate per source", func(t *testing.T) {
		r := startRelay(t, filepath.Join(t.TempDir(), "relay"), func(o *RelayOptions) { o.RatePerSource = 2 })
		h := startHub(t, r.relayURL(t, "hub1"), nil)
		h.waitConnected(t)
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				_ = c.Close()
			}
		}()
		// 20 connections in a burst: at most a few fit the bucket (2 plus
		// refill while dialling); the rest are closed without data.
		for i := 0; i < 20; i++ {
			c, err := net.Dial("tcp", r.PublicAddr(7778))
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		closed := 0
		for _, c := range conns {
			_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err := c.Read(make([]byte, 1)); err != nil && !isTimeout(err) {
				closed++
			}
		}
		if closed < 10 || r.Stats().Refused < 10 {
			t.Fatalf("only %d of 20 connections refused (stats %+v)", closed, r.Stats())
		}
	})
}

func TestRelayRestartReconnects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	r1 := startRelay(t, dir, nil)
	ctl := r1.ControlAddr()
	h := startHub(t, r1.relayURL(t, "hub1"), nil)
	h.waitConnected(t)
	r1.stop()
	eventually(t, 5*time.Second, "disconnect", func() bool { return !h.client.Status().Connected })

	// Same data dir (same certificate and tokens), same control address.
	r2 := startRelay(t, dir, func(o *RelayOptions) { o.Listen = ctl })
	h.waitConnected(t)
	if body, err := h.get(r2); err != nil || body != "hello from the hub" {
		t.Fatalf("GET after restart: %q %v", body, err)
	}
	if n := h.client.Status().Connects; n != 2 {
		t.Fatalf("connects = %d, want 2", n)
	}
}

func TestRelaySecondHelloReplacesSession(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), nil)
	u := r.relayURL(t, "hub1")
	a := startHub(t, u, nil)
	a.waitConnected(t)
	b := startHub(t, u, nil)
	b.waitConnected(t)
	// a really lost the relay: the relay told it so and it disconnected.
	eventually(t, 5*time.Second, "first session told it was replaced", func() bool { return a.client.Status().Replaced >= 1 })
	// Stop a from reconnecting, so the two do not flap.
	a.stop()
	eventually(t, 5*time.Second, "b attached", func() bool { return b.client.Status().Connected })
	if r.Stats().Sessions < 2 {
		t.Fatalf("sessions = %d", r.Stats().Sessions)
	}
	if body, err := b.get(r); err != nil || body != "hello from the hub" {
		t.Fatalf("GET through the replacing hub: %q %v", body, err)
	}
}

func TestRelayRevokedTokenDropsSession(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), nil)
	h := startHub(t, r.relayURL(t, "hub1"), nil)
	h.waitConnected(t)
	if err := RevokeToken(r.dir, "hub1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "session dropped and refused", func() bool {
		st := h.client.Status()
		return !st.Connected && strings.Contains(st.LastError, "unauthorized")
	})
	if !closedQuickly(t, r.PublicAddr(443)) {
		t.Fatal("public port still forwards after revoke")
	}
}

// TestRelayOneHubPerRelay: while a hub is attached, a hello with another
// (valid) name is refused; the attached hub's own name may replace it.
func TestRelayOneHubPerRelay(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), nil)
	u1 := r.relayURL(t, "hub1")
	// A second token only exists in an older tokens.json; write it directly.
	tf, err := readTokens(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	tf.Tokens = append(tf.Tokens, TokenInfo{Name: "hub2", Hash: hashSecret("second-secret")})
	if err := writeTokens(r.dir, tf); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, u1, nil)
	h.waitConnected(t)
	line := func(name, token string) []byte {
		b, _ := json.Marshal(hello{V: 1, Name: name, Token: token, Ports: map[string]string{"443": RoleAPI}})
		return b
	}
	if s, reason := r.checkHello(line("hub2", "second-secret")); s != nil || reason != errOtherHub {
		t.Fatalf("other hub while attached: %v %q", s != nil, reason)
	}
	// A wrong token for the other name still says only "unauthorized".
	if _, reason := r.checkHello(line("hub2", "wrong")); reason != "unauthorized" {
		t.Fatalf("reason %q", reason)
	}
	if s, reason := r.checkHello(line("hub1", u1.Token)); s == nil {
		t.Fatalf("same hub refused: %q", reason)
	}
	u2 := *u1
	u2.Name, u2.Token = "hub2", "second-secret"
	h2 := startHub(t, &u2, nil)
	eventually(t, 5*time.Second, "second hub refused", func() bool {
		return strings.Contains(h2.client.Status().LastError, errOtherHub)
	})
	if st := r.Stats(); st.Hub != "hub1" || !h.client.Status().Connected {
		t.Fatalf("attached hub lost the relay: %+v", st)
	}
}

// TestRelayConcurrentAttach: hellos of the same hub racing each other leave
// exactly one session; every replaced one is closed.
func TestRelayConcurrentAttach(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), nil)
	// Housekeeping keeps only sessions with a valid token.
	hash := hashSecret(r.relayURL(t, "hub1").Token)
	const n = 8
	var sessions []*relaySession
	for range n {
		a, b := net.Pipe()
		srv, err := yamux.Server(a, yamuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		cl, err := yamux.Client(b, yamuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.Close(); _ = srv.Close() })
		sessions = append(sessions, &relaySession{ys: srv, name: "hub1", hash: hash})
	}
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() { defer wg.Done(); r.attach(s, "test") }()
	}
	wg.Wait()
	cur := r.current()
	for _, s := range sessions {
		if s == cur {
			continue
		}
		select {
		case <-s.ys.CloseChan():
		case <-time.After(5 * time.Second):
			t.Fatal("a replaced session was left open")
		}
	}
	if cur == nil || cur.ys.IsClosed() {
		t.Fatal("no live session left")
	}
	if r.attach(&relaySession{name: "hub2"}, "test") {
		t.Fatal("another hub attached over a live session")
	}
}

// TestRelayPerSourceCap: one source cannot take every forwarded slot.
func TestRelayPerSourceCap(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), func(o *RelayOptions) {
		o.MaxConns, o.MaxConnsPerSource = 8, 3
	})
	h := startHub(t, r.relayURL(t, "hub1"), nil)
	h.waitConnected(t)
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for range 8 {
		c, err := net.Dial("tcp", r.PublicAddr(7778))
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	eventually(t, 5*time.Second, "the rest refused", func() bool { return r.Stats().Refused >= 5 })
	time.Sleep(100 * time.Millisecond)
	if st := r.Stats(); st.Active != 3 {
		t.Fatalf("one source holds %d of 8 slots, want 3 (%+v)", st.Active, st)
	}
	// Freed slots are usable again.
	for _, c := range conns[:3] {
		_ = c.Close()
	}
	eventually(t, 5*time.Second, "slots freed", func() bool { return r.Stats().Active == 0 })
	if got := echoOnce(t, r.PublicAddr(7778), "again"); got != "again" {
		t.Fatalf("echo %q", got)
	}
}

// TestRelayPendingHandshakeCap: connections that never finish their hello
// hold a bounded number of slots, time out, and the hub still gets in.
func TestRelayPendingHandshakeCap(t *testing.T) {
	r := startRelay(t, filepath.Join(t.TempDir(), "relay"), func(o *RelayOptions) {
		o.MaxPending, o.HandshakeTimeout = 2, 500*time.Millisecond
	})
	u := r.relayURL(t, "hub1")
	for range 2 {
		c, err := net.Dial("tcp", r.ControlAddr())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
	}
	time.Sleep(50 * time.Millisecond)
	c, err := net.Dial("tcp", r.ControlAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("connection over the pending cap not closed at once: %v", err)
	}
	h := startHub(t, u, nil)
	h.waitConnected(t)
}
