package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// fakeSource stands in for autocert.Manager.
type fakeSource struct {
	mu    sync.Mutex
	cert  *tls.Certificate
	err   error
	calls []time.Time
}

func (f *fakeSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, time.Now())
	return f.cert, f.err
}

func (f *fakeSource) set(c *tls.Certificate, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cert, f.err = c, err
}

func (f *fakeSource) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func fastACME(src certSource, hold time.Duration) *ACME {
	a := newACME("hub.example.com", src, quietLog())
	a.minRetry, a.maxRetry, a.failHold = 5*time.Millisecond, 20*time.Millisecond, hold
	return a
}

func TestACMECertificateSelection(t *testing.T) {
	ca := newTestCA(t, "letsencrypt-ish")
	acmeCert := ca.issue(t, "hub.example.com")
	src := &fakeSource{cert: &acmeCert}
	a := fastACME(src, 50*time.Millisecond)
	browser := &tls.ClientHelloInfo{ServerName: "hub.example.com", SupportedProtos: []string{"h2", "http/1.1"}}
	node := &tls.ClientHelloInfo{ServerName: "hub.example.com", SupportedProtos: identity.HubClientNextProtos()}
	challenge := &tls.ClientHelloInfo{ServerName: "hub.example.com", SupportedProtos: []string{acme.ALPNProto}}

	// Before the issuer ran: the CA certificate, and no hello issues.
	if got, err := a.GetCertificate(browser); got != nil || err != nil {
		t.Fatalf("before ready: %v %v", got, err)
	}
	if src.n() != 0 {
		t.Fatal("a normal hello reached the ACME manager before the certificate was ready")
	}
	// The challenge always reaches the manager.
	if got, _ := a.GetCertificate(challenge); got != &acmeCert || src.n() != 1 {
		t.Fatalf("challenge: %v, calls %d", got, src.n())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	a.Start(ctx) // once
	eventually(t, 5*time.Second, "certificate ready", a.Ready)

	if got, err := a.GetCertificate(browser); err != nil || got != &acmeCert {
		t.Fatalf("browser: %v %v", got, err)
	}
	if got, err := a.GetCertificate(&tls.ClientHelloInfo{ServerName: "HUB.example.com."}); err != nil || got != &acmeCert {
		t.Fatalf("case/trailing dot: %v %v", got, err)
	}
	// Nodes and CLIs (the marker): the CA certificate, always.
	if got, err := a.GetCertificate(node); got != nil || err != nil {
		t.Fatalf("marker hello: got %v %v, want the CA certificate", got, err)
	}
	// Another name or no SNI: the CA certificate.
	for _, sni := range []string{"localhost", "", "10.0.0.1", "other.example.com"} {
		if got, err := a.GetCertificate(&tls.ClientHelloInfo{ServerName: sni}); got != nil || err != nil {
			t.Fatalf("%q: got %v %v, want fallback", sni, got, err)
		}
	}

	// A failure in a ready-state hello: the CA certificate, not ready, and
	// no hello asks the CA again until the issuer's hold is over.
	src.set(nil, errors.New("rate limited"))
	if got, err := a.GetCertificate(browser); got != nil || err != nil {
		t.Fatalf("failing manager: got %v %v, want fallback", got, err)
	}
	if a.Ready() {
		t.Fatal("still ready after a failure")
	}
	before := src.n()
	for range 20 {
		_, _ = a.GetCertificate(browser)
	}
	if src.n() != before {
		t.Fatal("hellos reached the manager after a failure")
	}
	if _, err := a.GetCertificate(challenge); err == nil {
		t.Fatal("challenge error swallowed")
	}
	src.set(&acmeCert, nil)
	eventually(t, 5*time.Second, "ready again after the hold", a.Ready)

	var nilACME *ACME
	if c, err := nilACME.GetCertificate(&tls.ClientHelloInfo{ServerName: "x"}); c != nil || err != nil {
		t.Fatal("nil ACME must fall back")
	}
	nilACME.Start(ctx)
	cfg := &tls.Config{}
	nilACME.Configure(cfg, true)
	if cfg.GetCertificate != nil {
		t.Fatal("nil ACME configured something")
	}
	a.Configure(cfg, true)
	protos := strings.Join(cfg.NextProtos, ",")
	if cfg.GetCertificate == nil || !strings.Contains(protos, acme.ALPNProto) || strings.Contains(protos, identity.HubCAALPN) {
		t.Fatalf("Configure: %v", cfg.NextProtos)
	}
}

// TestACMEIssuerBackoff: after a failure the issuer holds off, then retries
// with a doubling backoff.
func TestACMEIssuerBackoff(t *testing.T) {
	src := &fakeSource{err: errors.New("no")}
	a := fastACME(src, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	time.Sleep(450 * time.Millisecond)
	cancel()
	// One attempt at once, then at most one per 100 ms hold.
	if n := src.n(); n < 2 || n > 5 {
		t.Fatalf("%d attempts in 450 ms with a 100 ms hold", n)
	}

	src = &fakeSource{err: errors.New("no")}
	a = fastACME(src, 0)
	ctx, cancel = context.WithCancel(context.Background())
	a.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()
	src.mu.Lock()
	calls := append([]time.Time(nil), src.calls...)
	src.mu.Unlock()
	if len(calls) < 4 {
		t.Fatalf("%d attempts", len(calls))
	}
	// Gaps grow to the 20 ms cap: never closer than the 5 ms start.
	for i := 1; i < len(calls); i++ {
		if d := calls[i].Sub(calls[i-1]); d < 4*time.Millisecond {
			t.Fatalf("attempt %d after %v", i, d)
		}
	}
	if len(calls) > 300/5 {
		t.Fatalf("%d attempts: no backoff", len(calls))
	}
}

// TestACMEMarkerTLS runs real handshakes: nodes and CLIs (the marker,
// pinned hub CA only) get the CA-issued certificate, browsers the ACME one;
// the marker is never the negotiated protocol, and a pinned client refuses
// a hub that only has a publicly trusted certificate.
func TestACMEMarkerTLS(t *testing.T) {
	hubCA := newTestCA(t, "hub ca")
	pub := newTestCA(t, "public ca")
	caCert := hubCA.issue(t, "hub.example.com", "localhost", "127.0.0.1")
	acmeCert := pub.issue(t, "hub.example.com")
	a := fastACME(&fakeSource{cert: &acmeCert}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	eventually(t, 5*time.Second, "certificate ready", a.Ready)

	serve := func(cfg *tls.Config) string {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
			}
		}()
		return ln.Addr().String()
	}
	hubCfg := &tls.Config{Certificates: []tls.Certificate{caCert}, MinVersion: tls.VersionTLS12}
	a.Configure(hubCfg, true)
	hub := serve(hubCfg)
	// An impersonator: a publicly trusted certificate for the hub's name.
	fake := serve(&tls.Config{Certificates: []tls.Certificate{acmeCert}, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12})

	nodeCfg := &tls.Config{RootCAs: hubCA.pool, ServerName: "hub.example.com", NextProtos: identity.HubClientNextProtos(), MinVersion: tls.VersionTLS12}
	browserCfg := &tls.Config{RootCAs: pub.pool, ServerName: "hub.example.com", NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12}

	c, err := tls.Dial("tcp", hub, nodeCfg)
	if err != nil {
		t.Fatalf("node with the marker: %v", err)
	}
	st := c.ConnectionState()
	_ = c.Close()
	if iss := st.PeerCertificates[0].Issuer.CommonName; iss != "hub ca" {
		t.Fatalf("node served a certificate from %q", iss)
	}
	if st.NegotiatedProtocol != "h2" {
		t.Fatalf("negotiated %q, want h2 (the marker is not a protocol)", st.NegotiatedProtocol)
	}
	c, err = tls.Dial("tcp", hub, browserCfg)
	if err != nil {
		t.Fatalf("browser: %v", err)
	}
	if iss := c.ConnectionState().PeerCertificates[0].Issuer.CommonName; iss != "public ca" {
		t.Fatalf("browser served a certificate from %q", iss)
	}
	_ = c.Close()
	if c, err := tls.Dial("tcp", fake, nodeCfg); err == nil {
		_ = c.Close()
		t.Fatal("a node accepted a publicly trusted certificate for the hub's name")
	}
	// The bus listener (no ALPN protocols of its own): same selection.
	busCfg := &tls.Config{Certificates: []tls.Certificate{caCert}, GetCertificate: a.GetCertificate, MinVersion: tls.VersionTLS13}
	bus := serve(busCfg)
	c, err = tls.Dial("tcp", bus, &tls.Config{RootCAs: hubCA.pool, ServerName: "hub.example.com", NextProtos: []string{identity.HubCAALPN}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatalf("bus client with the marker: %v", err)
	}
	if p := c.ConnectionState().NegotiatedProtocol; p != "" {
		t.Fatalf("bus negotiated %q", p)
	}
	_ = c.Close()
}
