package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/discovery"
	"github.com/richard-sen27/mycelium/internal/identity"
)

type fakeBrowser struct{ hubs []discovery.Hub }

func (f fakeBrowser) Browse(context.Context, time.Duration) ([]discovery.Hub, error) {
	return f.hubs, nil
}

func signed(t *testing.T, ca *identity.CA, hubURL, busURL string, issued int64) *mycelv1.Redirect {
	t.Helper()
	sig, err := ca.SignRedirect(hubURL, busURL, issued)
	if err != nil {
		t.Fatal(err)
	}
	return &mycelv1.Redirect{TrustDomain: string(ca.TrustDomain()), HubUrl: hubURL, BusUrl: busURL, IssuedAt: issued, Signature: sig}
}

// secondBus starts another bus with the fake hub's CA and certificate
// (the "new hub").
func secondBus(t *testing.T, h *fakeHub) *bus.Server {
	t.Helper()
	s, err := bus.StartServer(bus.ServerConfig{
		Host: "127.0.0.1", TLSCert: h.serverCert, ClientCAs: h.ca.Pool(),
		TrustDomain: testTD, StoreDir: t.TempDir(), Revocation: h.rev, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func redirectCtl(r *mycelv1.Redirect) *mycelv1.CtlRequest {
	return &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Redirect{Redirect: r}}
}

func TestVerifyRedirectHelpers(t *testing.T) {
	ca, err := identity.NewCA(testTD)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := signed(t, ca, "https://new:7777", "tls://new:7778", now.Unix())
	if err := VerifyRedirect(ca.CertPEM(), r, now); err != nil {
		t.Fatal(err)
	}
	h, err := EncodeRedirect(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeRedirect(h)
	if err != nil || back.GetHubUrl() != r.GetHubUrl() {
		t.Fatalf("round trip: %v %v", back, err)
	}
	if _, err := DecodeRedirect("!!!"); err == nil {
		t.Fatal("garbage decoded")
	}
	other, _ := identity.NewCA("other-td")
	noBus := signed(t, ca, "https://new:7777", "tls://new:7778", now.Unix())
	noBus.BusUrl = ""
	bad := []*mycelv1.Redirect{
		signed(t, other, "https://new:7777", "tls://new:7778", now.Unix()), // other trust domain
		signed(t, ca, "http://new:7777", "tls://new:7778", now.Unix()),     // not https
		noBus, // no bus
		signed(t, ca, "https://new:7777", "tls://new:7778", now.Add(-25*time.Hour).Unix()), // too old
	}
	for i, b := range bad {
		if err := VerifyRedirect(ca.CertPEM(), b, now); err == nil {
			t.Errorf("bad redirect %d verified", i)
		}
	}
	forged := signed(t, ca, "https://new:7777", "tls://new:7778", now.Unix())
	forged.HubUrl = "https://evil:7777"
	if err := VerifyRedirect(ca.CertPEM(), forged, now); err == nil {
		t.Fatal("forged URL verified")
	}
	ce := connect.NewError(connect.CodeFailedPrecondition, errors.New("hub moved to https://new:7777"))
	ce.Meta().Set(RedirectHeader, h)
	if got := RedirectFromError(ce); got == nil || got.GetHubUrl() != "https://new:7777" {
		t.Fatalf("RedirectFromError: %v", got)
	}
	if RedirectFromError(connect.NewError(connect.CodeFailedPrecondition, errors.New("x"))) != nil {
		t.Fatal("redirect without header")
	}
}

func TestNodeFollowsRedirect(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)
	nb := secondBus(t, h)
	hc := h.hubConn()
	now := time.Now().Unix()

	// Refused: a foreign CA, an expired redirect.
	other, _ := identity.NewCA(testTD)
	if resp := ctl(t, hc, "n1", redirectCtl(signed(t, other, "https://new.test:7777", nb.ClientURL(), now))); resp.GetOk() || !strings.Contains(resp.GetError(), "redirect refused") {
		t.Fatalf("foreign CA: %v", resp)
	}
	if resp := ctl(t, hc, "n1", redirectCtl(signed(t, h.ca, "https://new.test:7777", nb.ClientURL(), now-25*3600))); resp.GetOk() {
		t.Fatalf("expired: %v", resp)
	}
	if st, _ := ReadState(dir); st.HubURL != h.http.URL {
		t.Fatalf("state changed by a refused redirect: %+v", st)
	}

	if resp := ctl(t, hc, "n1", redirectCtl(signed(t, h.ca, "https://new.test:7777", nb.ClientURL(), now))); !resp.GetOk() {
		t.Fatalf("redirect: %v", resp)
	}
	waitFor(t, 10*time.Second, "node on the new bus, state stored", func() bool {
		st, _ := ReadState(dir)
		return containsString(nb.ConnectedNodes(), "n1") && st.BusURL == nb.ClientURL()
	})
	st, err := ReadState(dir)
	if err != nil || st.HubURL != "https://new.test:7777" || st.BusURL != nb.ClientURL() || st.RedirectIssuedAt != now {
		t.Fatalf("state %+v %v", st, err)
	}
	waitConnected(t, r.addr)
	nhc, err := nb.HubConn()
	if err != nil {
		t.Fatal(err)
	}
	if resp := ctl(t, nhc, "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}); !resp.GetOk() {
		t.Fatalf("ping on the new hub: %v", resp)
	}
	// Older than the one followed: refused; the same one again: ok, no-op.
	if resp := ctl(t, nhc, "n1", redirectCtl(signed(t, h.ca, "https://old.test:7777", h.busServer().ClientURL(), now-10))); resp.GetOk() {
		t.Fatalf("older redirect accepted: %v", resp)
	}
	if resp := ctl(t, nhc, "n1", redirectCtl(signed(t, h.ca, "https://new.test:7777", nb.ClientURL(), now))); !resp.GetOk() {
		t.Fatalf("repeat: %v", resp)
	}
	if st, _ := ReadState(dir); st.BusURL != nb.ClientURL() {
		t.Fatalf("state after older redirect: %+v", st)
	}
}

type mutBrowser struct {
	mu   sync.Mutex
	hubs []discovery.Hub
}

func (b *mutBrowser) set(h ...discovery.Hub) { b.mu.Lock(); b.hubs = h; b.mu.Unlock() }

func (b *mutBrowser) Browse(context.Context, time.Duration) ([]discovery.Hub, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]discovery.Hub{}, b.hubs...), nil
}

func TestNodeDiscoversMovedHub(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	before, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	nb := secondBus(t, h)
	impostor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer impostor.Close()
	other, _ := identity.NewCA(testTD)
	fp := h.ca.Fingerprint()
	b := &mutBrowser{}
	b.set(
		// Another CA: never matched.
		discovery.Hub{URL: "https://evil.test:7777", BusURL: "tls://evil.test:7778", CAFingerprint: other.Fingerprint()},
	)
	cfg.move.discoverAfter = 300 * time.Millisecond
	cfg.move.discoverEvery = 100 * time.Millisecond
	cfg.move.browser = b
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	h.stopBus()
	waitFor(t, 5*time.Second, "disconnected", func() bool { return !getStatus(t, r.addr).Connected })
	time.Sleep(700 * time.Millisecond)
	// The right fingerprint at an address that fails the pinned-CA handshake.
	b.set(discovery.Hub{URL: impostor.URL, BusURL: nb.ClientURL(), CAFingerprint: fp})
	time.Sleep(500 * time.Millisecond)
	if st, _ := ReadState(dir); st.HubURL != before.HubURL || st.BusURL != before.BusURL {
		t.Fatalf("followed an impostor: %+v", st)
	}

	// The impostor and the real hub answer together: each is probed in
	// turn and the real one is followed.
	newURL := strings.Replace(h.http.URL, "127.0.0.1", "localhost", 1)
	b.set(discovery.Hub{URL: impostor.URL, BusURL: nb.ClientURL(), CAFingerprint: fp},
		discovery.Hub{URL: newURL, BusURL: nb.ClientURL(), CAFingerprint: fp})
	waitFor(t, 10*time.Second, "node on the discovered hub", func() bool {
		st, _ := ReadState(dir)
		return containsString(nb.ConnectedNodes(), "n1") && st.HubURL == newURL
	})
	st, err := ReadState(dir)
	if err != nil || st.HubURL != newURL || st.BusURL != nb.ClientURL() {
		t.Fatalf("state %+v %v", st, err)
	}
	waitConnected(t, r.addr)
}

// ---- join ----

// registerToken records a token naming hubURL with the fake hub.
func registerToken(t *testing.T, h *fakeHub, name, hubURL string) string {
	t.Helper()
	tok, err := identity.NewJoinToken(hubURL, h.ca.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.tokens[tok.ID] = joinRecord{name: name, secretHash: tok.SecretHash(), owner: "u-owner"}
	h.mu.Unlock()
	return tok.String()
}

func TestJoinFallsBackToMDNS(t *testing.T) {
	h := newFakeHub(t)
	dead := "https://127.0.0.1:1"
	tok := registerToken(t, h, "n1", dead)
	dir := filepath.Join(t.TempDir(), "node")
	// Nothing found: a clear error.
	_, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: tok, Logger: discardLogger(), Browser: fakeBrowser{}})
	if err == nil || !strings.Contains(err.Error(), "mDNS") {
		t.Fatalf("no hub found: %v", err)
	}
	other, _ := identity.NewCA(testTD)
	// A copycat serves the real CA certificate and advertises its
	// fingerprint, but its own TLS certificate is not the hub CA's: it is
	// probed, refused, and the next answer is tried.
	copycat := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ca.pem" {
			_, _ = w.Write(h.ca.CertPEM())
		}
	}))
	defer copycat.Close()
	res, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: tok, Logger: discardLogger(), Browser: fakeBrowser{hubs: []discovery.Hub{
		{URL: "https://evil.test:1", CAFingerprint: other.Fingerprint()},
		{URL: copycat.URL, BusURL: "tls://x:1", CAFingerprint: h.ca.Fingerprint()},
		{URL: h.http.URL, BusURL: "tls://x:1", CAFingerprint: h.ca.Fingerprint()},
	}}})
	if err != nil {
		t.Fatalf("Join via mDNS: %v", err)
	}
	if res.HubURL != h.http.URL {
		t.Fatalf("joined %s", res.HubURL)
	}
}

// movedHub is an old hub that answers Enrol with a redirect.
func movedHub(t *testing.T, h *fakeHub, r *mycelv1.Redirect) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(h.ca.CertPEM()) })
	path, handler := mycelv1connect.NewEnrolServiceHandler(movedEnrol{r: r})
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

type movedEnrol struct {
	mycelv1connect.UnimplementedEnrolServiceHandler
	r *mycelv1.Redirect
}

func (m movedEnrol) Enrol(context.Context, *connect.Request[mycelv1.EnrolRequest]) (*connect.Response[mycelv1.EnrolResponse], error) {
	e := connect.NewError(connect.CodeFailedPrecondition, errors.New("hub moved to "+m.r.GetHubUrl()))
	v, _ := EncodeRedirect(m.r)
	e.Meta().Set(RedirectHeader, v)
	return nil, e
}

func TestJoinFollowsRedirect(t *testing.T) {
	h := newFakeHub(t)
	now := time.Now().Unix()
	old := movedHub(t, h, signed(t, h.ca, h.http.URL, h.busServer().ClientURL(), now))
	tok := registerToken(t, h, "n1", old)
	dir := filepath.Join(t.TempDir(), "node")
	res, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: tok, Logger: discardLogger(), Browser: fakeBrowser{}})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if res.HubURL != h.http.URL || res.NodeName != "n1" {
		t.Fatalf("result %+v", res)
	}

	other, _ := identity.NewCA(testTD)
	forged := movedHub(t, h, signed(t, other, h.http.URL, h.busServer().ClientURL(), now))
	tok2 := registerToken(t, h, "n2", forged)
	_, err = Join(context.Background(), JoinOptions{NodeDir: filepath.Join(t.TempDir(), "node"), Token: tok2, Logger: discardLogger(), Browser: fakeBrowser{}})
	if err == nil || !strings.Contains(err.Error(), "redirect is not valid") {
		t.Fatalf("forged redirect: %v", err)
	}
}
