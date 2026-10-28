package bus

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// TestMarkerBusGetCertificate: ServerConfig.GetCertificate picks a
// certificate per hello (the hub's ACME certificate for its advertise
// host); (nil, nil) falls back to TLSCert. Nodes send the CA marker and
// still connect with the pinned CA.
func TestMarkerBusGetCertificate(t *testing.T) {
	ca := newTestCA(t, testTD)
	public := newTestCA(t, "public.test")
	pubCert, _ := public.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "public"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"hub.example.com"},
	})
	var mu sync.Mutex
	var hellos [][]string
	s, err := StartServer(ServerConfig{
		Host: "127.0.0.1", TLSCert: ca.serverCert(t), ClientCAs: ca.pool,
		TrustDomain: identity.TrustDomain(testTD), StoreDir: t.TempDir(), Logger: discardLogger(),
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			mu.Lock()
			hellos = append(hellos, append([]string{"sni=" + hi.ServerName}, hi.SupportedProtos...))
			mu.Unlock()
			if hi.ServerName == "hub.example.com" && !slices.Contains(hi.SupportedProtos, identity.HubCAALPN) {
				return &pubCert, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdown(t, s) })

	served := func(sni string, protos []string) string {
		t.Helper()
		addr := strings.TrimPrefix(s.ClientURL(), "tls://")
		raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		if info, err := bufio.NewReader(raw).ReadString('\n'); err != nil || !strings.HasPrefix(info, "INFO ") {
			t.Fatalf("read INFO: %q %v", info, err)
		}
		// The server asks for a client certificate; this probe offers none
		// and only looks at what the server presented.
		c := tls.Client(raw, &tls.Config{ServerName: sni, NextProtos: protos, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // test inspects the served certificate
		_ = c.Handshake()
		st := c.ConnectionState()
		if len(st.PeerCertificates) == 0 {
			t.Fatalf("%s: no server certificate", sni)
		}
		return st.PeerCertificates[0].Subject.CommonName
	}
	if cn := served("hub.example.com", nil); cn != "public" {
		t.Fatalf("advertise host without the marker: served %q", cn)
	}
	if cn := served("hub.example.com", []string{identity.HubCAALPN}); cn != "hub" {
		t.Fatalf("advertise host with the marker: served %q", cn)
	}
	if cn := served("localhost", nil); cn != "hub" {
		t.Fatalf("other name: served %q", cn)
	}

	// A node (Connect sends the marker) verifies against the pinned CA. By
	// name, so the hello carries SNI (Go only asks GetCertificate then).
	mu.Lock()
	hellos = nil
	mu.Unlock()
	cert, _ := ca.nodeCert(t, testTD, "n1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := Connect(ctx, ClientConfig{
		URL: strings.Replace(s.ClientURL(), "127.0.0.1", "localhost", 1), NodeName: "n1",
		Cert: cert, RootCAs: ca.pool, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("node connect: %v", err)
	}
	defer nc.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(hellos) == 0 || !slices.Contains(hellos[len(hellos)-1], identity.HubCAALPN) {
		t.Fatalf("node hello ALPN %v lacks the marker", hellos)
	}
}
