package daemon

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// hubServer serves HTTPS for 127.0.0.1 with a certificate from ca and
// reports the ALPN list of every client hello.
func hubServer(t *testing.T, ca *identity.CA, h http.Handler) (*httptest.Server, chan []string) {
	t.Helper()
	crt, key, err := ca.IssueServerCert([]string{"127.0.0.1"}, identity.ServerCertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	protos := make(chan []string, 16)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case protos <- slices.Clone(hi.SupportedProtos):
			default:
			}
			return nil, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, protos
}

// TestPinnedClientCAOnly: the node trusts the pinned hub CA only, never a
// publicly trusted certificate for the hub's name (D026: a relay operator
// could get one); its hellos carry the CA marker and its client
// certificate is still offered.
func TestPinnedClientCAOnly(t *testing.T) {
	hubCA, err := identity.NewCA("hub.test")
	if err != nil {
		t.Fatal(err)
	}
	// Stands in for a public CA (Let's Encrypt via the relay operator).
	public, err := identity.NewCA("public.test")
	if err != nil {
		t.Fatal(err)
	}
	cc, ck, err := hubCA.IssueServerCert([]string{"node.test"}, identity.ServerCertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(cc, ck)
	if err != nil {
		t.Fatal(err)
	}
	hc := pinnedClient(hubCA.Pool(), func() *tls.Certificate { return &clientCert })
	certs := make(chan int, 4)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		certs <- len(r.TLS.PeerCertificates)
		_, _ = w.Write([]byte("ok"))
	})

	srv, protos := hubServer(t, hubCA, ok)
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("pinned hub CA: %v", err)
	}
	_ = resp.Body.Close()
	if n := <-certs; n == 0 {
		t.Error("no client certificate sent")
	}
	if p := <-protos; !slices.Contains(p, identity.HubCAALPN) {
		t.Errorf("hello ALPN %v lacks the %q marker", p, identity.HubCAALPN)
	}

	srv, _ = hubServer(t, public, ok)
	if resp, err := hc.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a publicly trusted certificate for the hub's name was accepted")
	}
}

// TestPinnedJoinRefusesPublicCert: `mycel join` against a hub that presents
// only a publicly trusted certificate (it can serve the real, public CA
// file) fails before the join token is sent.
func TestPinnedJoinRefusesPublicCert(t *testing.T) {
	hubCA, err := identity.NewCA("hub.test")
	if err != nil {
		t.Fatal(err)
	}
	public, err := identity.NewCA("public.test")
	if err != nil {
		t.Fatal(err)
	}
	var enrol atomic.Int32
	srv, _ := hubServer(t, public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ca.pem" {
			_, _ = w.Write(hubCA.CertPEM())
			return
		}
		enrol.Add(1)
		http.Error(w, "no", http.StatusNotFound)
	}))
	tok, err := identity.NewJoinToken(srv.URL, hubCA.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = Join(ctx, JoinOptions{NodeDir: filepath.Join(t.TempDir(), "node"), Token: tok.String(), Logger: discardLogger()})
	if err == nil {
		t.Fatal("join completed against a hub with a public certificate")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("join error %v, want a certificate verification failure", err)
	}
	if n := enrol.Load(); n != 0 {
		t.Fatalf("the join token reached the impostor (%d enrol requests)", n)
	}
}
