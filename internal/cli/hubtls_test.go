package cli

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// hubTLSServer serves HTTPS with a certificate for 127.0.0.1 from ca and
// reports the ALPN list of client hellos.
func hubTLSServer(t *testing.T, ca *identity.CA) (*httptest.Server, chan []string) {
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
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12,
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

// TestPinnedHTTPClientCAOnly: the CLI trusts the pinned hub CA only, never
// a publicly trusted certificate for the hub's name (D026: the API key
// must not reach a relay operator), and its hellos carry the CA marker.
func TestPinnedHTTPClientCAOnly(t *testing.T) {
	hubCA, err := identity.NewCA("hub.test")
	if err != nil {
		t.Fatal(err)
	}
	// Stands in for a public CA (Let's Encrypt via the relay operator).
	public, err := identity.NewCA("public.test")
	if err != nil {
		t.Fatal(err)
	}
	hc, err := pinnedHTTPClient(hubCA.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	srv, protos := hubTLSServer(t, hubCA)
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("pinned hub CA: %v", err)
	}
	_ = resp.Body.Close()
	if p := <-protos; !slices.Contains(p, identity.HubCAALPN) {
		t.Fatalf("hello ALPN %v lacks the %q marker", p, identity.HubCAALPN)
	}
	srv, _ = hubTLSServer(t, public)
	if resp, err := hc.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a publicly trusted certificate for the hub's name was accepted")
	}
	if _, err := pinnedHTTPClient([]byte("not a certificate")); err == nil {
		t.Fatal("invalid CA PEM accepted")
	}
}
