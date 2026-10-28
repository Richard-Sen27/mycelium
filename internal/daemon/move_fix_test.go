package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

func pingOK(t *testing.T, hc *nats.Conn) bool {
	t.Helper()
	return ctl(t, hc, "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}).GetOk()
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func fastSwitchConfig() runConfig {
	cfg := testRunConfig(newFakeProbe(), &stateLog{})
	cfg.move.switchRetryMin = 50 * time.Millisecond
	cfg.move.switchRetryMax = 200 * time.Millisecond
	return cfg
}

// TestRedirectWaitsForTheNewHub: a redirect to a hub that is not up yet
// leaves the node on its old hub and address until the new bus answers.
func TestRedirectWaitsForTheNewHub(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	cfg := fastSwitchConfig()
	r := startRunWith(t, dir, cfg, newFakeProbe(), &stateLog{})
	waitConnected(t, r.addr)
	before, _ := ReadState(dir)
	port := freeTCPPort(t)
	busURL := "tls://127.0.0.1:" + strconv.Itoa(port)
	now := time.Now().Unix()
	if resp := ctl(t, h.hubConn(), "n1", redirectCtl(signed(t, h.ca, "https://new.test:7777", busURL, now))); !resp.GetOk() {
		t.Fatalf("redirect: %v", resp)
	}
	time.Sleep(1500 * time.Millisecond)
	if st, _ := ReadState(dir); st.HubURL != before.HubURL || st.BusURL != before.BusURL || st.RedirectIssuedAt != 0 {
		t.Fatalf("address stored before the new hub answered: %+v", st)
	}
	if !getStatus(t, r.addr).Connected || !pingOK(t, h.hubConn()) {
		t.Fatal("node left its old hub before the new one answered")
	}
	// The same redirect again (a resend by the old hub) is a no-op.
	if resp := ctl(t, h.hubConn(), "n1", redirectCtl(signed(t, h.ca, "https://new.test:7777", busURL, now))); !resp.GetOk() {
		t.Fatalf("resend: %v", resp)
	}
	// The new hub comes up on that port.
	nb, err := bus.StartServer(bus.ServerConfig{
		Host: "127.0.0.1", Port: port, TLSCert: h.serverCert, ClientCAs: h.ca.Pool(),
		TrustDomain: testTD, StoreDir: t.TempDir(), Revocation: h.rev, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = nb.Shutdown(ctx)
	})
	waitFor(t, 10*time.Second, "node on the new hub", func() bool {
		st, _ := ReadState(dir)
		return containsString(nb.ConnectedNodes(), "n1") && st.BusURL == busURL && st.RedirectIssuedAt == now
	})
}

// TestRedirectToHubRefusingTheNodeIsNotRevocation: a new bus that refuses
// the node's certificate must neither wipe its identity nor move it.
func TestRedirectToHubRefusingTheNodeIsNotRevocation(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	cfg := fastSwitchConfig()
	sl := &stateLog{}
	r := startRunWith(t, dir, cfg, newFakeProbe(), sl)
	waitConnected(t, r.addr)
	before, _ := ReadState(dir)
	refusing := &fakeRevocation{}
	refusing.revoke("spiffe://" + string(testTD) + "/node/n1")
	nb, err := bus.StartServer(bus.ServerConfig{
		Host: "127.0.0.1", TLSCert: h.serverCert, ClientCAs: h.ca.Pool(),
		TrustDomain: testTD, StoreDir: t.TempDir(), Revocation: refusing, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = nb.Shutdown(ctx)
	})
	if resp := ctl(t, h.hubConn(), "n1", redirectCtl(signed(t, h.ca, "https://new.test:7777", nb.ClientURL(), time.Now().Unix()))); !resp.GetOk() {
		t.Fatalf("redirect: %v", resp)
	}
	time.Sleep(1500 * time.Millisecond)
	if sl.has(StateRevoked) {
		t.Fatal("a refusing redirect target revoked the node")
	}
	if st, err := ReadState(dir); err != nil || st.BusURL != before.BusURL {
		t.Fatalf("state %+v %v", st, err)
	}
	if !pingOK(t, h.hubConn()) {
		t.Fatal("node no longer answers on its hub")
	}
}

// TestRedirectFromRefusedRPC: a bus RPC refused with failed_precondition
// and the Mycel-Redirect header hands the redirect to acceptRedirect.
func TestRedirectFromRefusedRPC(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	n, err := loadNode(dir, discardLogger(), testRunConfig(newFakeProbe(), &stateLog{}))
	if err != nil {
		t.Fatal(err)
	}
	v, err := EncodeRedirect(signed(t, h.ca, "https://new.test:7777", "tls://new.test:7778", time.Now().Unix()))
	if err != nil {
		t.Fatal(err)
	}
	hdr := nats.Header{}
	hdr.Set(RedirectHeader, v)
	n.redirectFromReply(hdr, "invalid_argument: nope")
	select {
	case r := <-n.mv.ch:
		t.Fatalf("redirect taken from a non-moved error: %v", r)
	default:
	}
	n.redirectFromReply(hdr, "failed_precondition: hub moved to https://new.test:7777")
	select {
	case r := <-n.mv.ch:
		if r.GetHubUrl() != "https://new.test:7777" {
			t.Fatalf("redirect %v", r)
		}
	default:
		t.Fatal("redirect not handed on")
	}
	other, _ := identity.NewCA(testTD)
	v2, _ := EncodeRedirect(signed(t, other, "https://evil.test:7777", "tls://evil.test:7778", time.Now().Unix()))
	hdr.Set(RedirectHeader, v2)
	n.redirectFromReply(hdr, "failed_precondition: hub moved to https://evil.test:7777")
	select {
	case r := <-n.mv.ch:
		t.Fatalf("unverified redirect handed on: %v", r)
	default:
	}
}

func TestSameHubNormalisesBusURLs(t *testing.T) {
	if !sameHub("https://H:7777/", "h:7778", "https://h:7777", "tls://H:7778") {
		t.Fatal("same hub not recognised")
	}
	if sameHub("https://h:7777", "tls://h:7778", "https://h:7777", "tls://h:7779") {
		t.Fatal("different bus ports equal")
	}
	for _, bad := range []string{"", "nats://h:1", "tls://h:x", "tls://h:1\n", "h"} {
		if _, err := NormalizeBusURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ---- pinned CA only ----

// A "system" root injected into this process: Go then trusts it wherever
// a TLS client leaves RootCAs nil (x509.SetFallbackRoots with
// GODEBUG=x509usefallbackroots=1 replaces the platform roots). Every
// connection to a hub must verify against the pinned hub CA only, so a
// certificate chained to this root must be refused.
var sysRoot struct {
	once sync.Once
	ca   *identity.CA
}

func systemRoot(t *testing.T) *identity.CA {
	t.Helper()
	sysRoot.once.Do(func() {
		ca, err := identity.NewCA("system-root")
		if err != nil {
			t.Fatal(err)
		}
		gd := os.Getenv("GODEBUG")
		if gd != "" {
			gd += ","
		}
		_ = os.Setenv("GODEBUG", gd+"x509usefallbackroots=1") //nolint:usetesting // process-wide on purpose: the roots cannot be restored
		x509.SetFallbackRoots(ca.Pool())
		sysRoot.ca = ca
	})
	// The injection must work, or the tests below would prove nothing.
	srv := systemServer(t, sysRoot.ca, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }))
	resp, err := (&http.Client{Transport: &http.Transport{}}).Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("injected system root not trusted by a default client: %v", err)
	}
	_ = resp.Body.Close()
	return sysRoot.ca
}

func systemCert(t *testing.T, ca *identity.CA) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := ca.IssueServerCert([]string{"127.0.0.1", "localhost"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func systemServer(t *testing.T, ca *identity.CA, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{systemCert(t, ca)}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func isUnknownAuthority(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "unknown authority") || strings.Contains(err.Error(), "certificate signed by unknown"))
}

func TestPinnedCAOnly(t *testing.T) {
	sys := systemRoot(t)
	h := newFakeHub(t)

	t.Run("discovery probe", func(t *testing.T) {
		dir := joinNode(t, h, "n1")
		n, err := loadNode(dir, discardLogger(), testRunConfig(newFakeProbe(), &stateLog{}))
		if err != nil {
			t.Fatal(err)
		}
		srv := systemServer(t, sys, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }))
		if err := n.probeHub(context.Background(), srv.URL); !isUnknownAuthority(err) {
			t.Fatalf("probe of a system-root hub: %v", err)
		}
		if err := n.probeHub(context.Background(), h.http.URL); err != nil {
			t.Fatalf("probe of the real hub: %v", err)
		}
	})

	t.Run("enrol", func(t *testing.T) {
		var enrolCalls atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(h.ca.CertPEM()) })
		mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { enrolCalls.Add(1) })
		srv := systemServer(t, sys, mux)
		tok := registerToken(t, h, "victim", srv.URL)
		_, err := Join(context.Background(), JoinOptions{NodeDir: filepath.Join(t.TempDir(), "node"), Token: tok, Logger: discardLogger(), Browser: fakeBrowser{}})
		if !isUnknownAuthority(err) || enrolCalls.Load() != 0 {
			t.Fatalf("enrol at a system-root hub: %v (requests: %d)", err, enrolCalls.Load())
		}
	})

	t.Run("bus", func(t *testing.T) {
		dir := joinNode(t, h, "n2")
		n, err := loadNode(dir, discardLogger(), testRunConfig(newFakeProbe(), &stateLog{}))
		if err != nil {
			t.Fatal(err)
		}
		sb, err := bus.StartServer(bus.ServerConfig{
			Host: "127.0.0.1", TLSCert: systemCert(t, sys), ClientCAs: h.ca.Pool(),
			TrustDomain: testTD, StoreDir: t.TempDir(), Revocation: h.rev, Logger: discardLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = sb.Shutdown(ctx)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if nc, _, err := n.connectBusTo(ctx, sb.ClientURL(), true); err == nil {
			nc.Close()
			t.Fatal("node connected to a bus with a system-root certificate")
		}
		if containsString(sb.ConnectedNodes(), "n2") {
			t.Fatal("node reached the system-root bus")
		}
	})

	t.Run("redirect target", func(t *testing.T) {
		dir := joinNode(t, h, "n1")
		r := startRunWith(t, dir, fastSwitchConfig(), newFakeProbe(), &stateLog{})
		waitConnected(t, r.addr)
		before, _ := ReadState(dir)
		sb, err := bus.StartServer(bus.ServerConfig{
			Host: "127.0.0.1", TLSCert: systemCert(t, sys), ClientCAs: h.ca.Pool(),
			TrustDomain: testTD, StoreDir: t.TempDir(), Revocation: h.rev, Logger: discardLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = sb.Shutdown(ctx)
		})
		if resp := ctl(t, h.hubConn(), "n1", redirectCtl(signed(t, h.ca, "https://localhost:7777", sb.ClientURL(), time.Now().Unix()))); !resp.GetOk() {
			t.Fatalf("redirect: %v", resp)
		}
		time.Sleep(1500 * time.Millisecond)
		if st, _ := ReadState(dir); st.BusURL != before.BusURL || containsString(sb.ConnectedNodes(), "n1") {
			t.Fatalf("followed a redirect to a system-root bus: %+v", st)
		}
	})

	t.Run("enrol client never uses system roots", func(t *testing.T) {
		// The pinned client refuses the system-root server even for a
		// plain Connect call.
		srv := systemServer(t, sys, http.NotFoundHandler())
		c := mycelv1connect.NewEnrolServiceClient(pinnedClient(h.ca.Pool(), nil), srv.URL)
		_, err := c.Enrol(context.Background(), connect.NewRequest(&mycelv1.EnrolRequest{}))
		if !isUnknownAuthority(err) {
			t.Fatalf("pinned client and a system-root server: %v", err)
		}
	})
}
