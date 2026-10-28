package hub

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

const testRelayFP = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSetTunnelAndACMEConfig(t *testing.T) {
	opts, _ := initTestHub(t)
	dir := opts.HubDir
	secret := strings.Repeat("A", 43)
	url := "relay://relay.example.com:7790/hub1?fp=" + testRelayFP + "&token=" + secret

	if _, err := SetTunnel(dir, "relay://relay.example.com/hub1", nil); err == nil {
		t.Fatal("URL without fp/token accepted")
	}
	res, err := SetTunnel(dir, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CertReissued || res.AdvertiseURL != "https://relay.example.com" || res.BusAdvertise != "tls://relay.example.com:7778" {
		t.Fatalf("SetTunnel = %+v", res)
	}
	if strings.Contains(res.Relay, secret) {
		t.Fatal("result leaks the token")
	}
	b, err := os.ReadFile(filepath.Join(dir, tunnel.RelayURLFile))
	if err != nil || !strings.Contains(string(b), secret) {
		t.Fatalf("relay.url: %q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, tunnel.RelayURLFile))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("relay.url mode %v", st.Mode().Perm())
		}
	}
	yml, _ := os.ReadFile(filepath.Join(dir, hubConfigFile))
	if strings.Contains(string(yml), secret) {
		t.Fatal("hub.yaml holds the relay secret")
	}
	cfg, err := ReadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tunnel.Enabled || cfg.Tunnel.PortMap()[443] != tunnel.RoleAPI || cfg.AdvertiseURL != "https://relay.example.com" {
		t.Fatalf("config = %+v", cfg)
	}
	certPEM, _ := os.ReadFile(filepath.Join(dir, serverCertFile))
	crt, err := parseCertPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"relay.example.com", "localhost", "127.0.0.1"} {
		if err := crt.VerifyHostname(h); err != nil {
			t.Errorf("re-issued certificate does not cover %s: %v", h, err)
		}
	}
	// Second time: nothing to re-issue.
	if res, err := SetTunnel(dir, url, map[int]string{8443: tunnel.RoleAPI, 7778: tunnel.RoleBus}); err != nil || res.CertReissued || res.AdvertiseURL != "https://relay.example.com:8443" {
		t.Fatalf("second SetTunnel = %+v, %v", res, err)
	}
	// ACME through the relay needs 443 → api.
	if err := SetACME(dir, tunnel.ACMEConfig{Enabled: true}); err == nil {
		t.Fatal("ACME accepted without the relay's 443 forwarded to the API")
	}
	if _, err := SetTunnel(dir, url, nil); err != nil {
		t.Fatal(err)
	}
	if err := SetACME(dir, tunnel.ACMEConfig{Enabled: true, Email: "ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := UnsetTunnel(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tunnel.RelayURLFile)); !os.IsNotExist(err) {
		t.Fatalf("relay.url still there: %v", err)
	}
	cfg, _ = ReadConfig(dir)
	if cfg.Tunnel.Enabled || !cfg.ACME.Enabled {
		t.Fatalf("after unset: %+v", cfg)
	}

	// A hub on localhost cannot use ACME.
	opts2, _ := initTestHub(t)
	if err := SetACME(opts2.HubDir, tunnel.ACMEConfig{Enabled: true}); err == nil {
		t.Fatal("ACME accepted for localhost")
	}
}

func TestHubThroughRelay(t *testing.T) {
	relayDir := filepath.Join(t.TempDir(), "relay")
	r, err := tunnel.NewRelay(tunnel.RelayOptions{
		DataDir: relayDir, Listen: "127.0.0.1:0",
		Public: map[int]string{443: "127.0.0.1:0", 7778: "127.0.0.1:0"}, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	rdone := make(chan struct{})
	go func() { _ = r.Serve(rctx); close(rdone) }()
	t.Cleanup(func() { rcancel(); <-rdone })
	secret, err := tunnel.CreateToken(relayDir, "hub1", false)
	if err != nil {
		t.Fatal(err)
	}
	_, cport, _ := net.SplitHostPort(r.ControlAddr())
	p, _ := strconv.Atoi(cport)
	u := &tunnel.RelayURL{Host: "127.0.0.1", Port: p, Name: "hub1", Fingerprint: r.Fingerprint(), Token: secret}

	opts, res := initTestHub(t)
	if _, err := SetTunnel(opts.HubDir, u.String(), nil); err != nil {
		t.Fatal(err)
	}
	h, _ := runTestHub(t, opts, res, false)
	hc := h.httpClient(nil)

	status := func(key string) (int, tunnelStatusJSON) {
		req, _ := http.NewRequest(http.MethodGet, h.ready.HubURL+"/api/hub/tunnel", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out tunnelStatusJSON
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), secret) {
			t.Fatal("status leaks the relay secret")
		}
		_ = json.Unmarshal(body, &out)
		return resp.StatusCode, out
	}
	if code, _ := status(""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous status: %d", code)
	}
	_, memberKey := createUser(t, h.clients(res.AdminAPIKey, nil), "bob", mycelv1.Role_ROLE_MEMBER)
	if code, _ := status(memberKey); code != http.StatusForbidden {
		t.Fatalf("member status: %d", code)
	}
	eventually(t, 10*time.Second, "tunnel connected", func() bool {
		code, st := status(res.AdminAPIKey)
		return code == http.StatusOK && st.Enabled && st.Status != nil && st.Connected
	})

	// The API through the relay, verified against the hub CA, as a node or
	// CLI does it (with the CA marker; h2 is still negotiated).
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: h.pool, ServerName: "localhost", MinVersion: tls.VersionTLS12,
			NextProtos: identity.HubClientNextProtos(),
		},
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", r.PublicAddr(443))
		},
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get("https://localhost/healthz")
	if err != nil {
		t.Fatalf("healthz through relay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" || resp.ProtoMajor != 2 {
		t.Fatalf("healthz through relay: %d %q %s", resp.StatusCode, body, resp.Proto)
	}

	// The bus through the relay: NATS greets with INFO before TLS.
	c, err := net.Dial("tcp", r.PublicAddr(7778))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "INFO ") {
		t.Fatalf("bus through relay: %q %v", line, err)
	}
	_, st := status(res.AdminAPIKey)
	if st.Total < 2 || st.ACME.Enabled {
		t.Fatalf("status = %+v", st)
	}
}

// TestTunnelAuditMarksRelayedRemote: audit details with a remote address
// from a relayed connection carry remote_via_relay; others are unchanged.
func TestTunnelAuditMarksRelayedRemote(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	plain := relayConnContext(context.Background(), a)
	relayed := context.WithValue(context.Background(), viaRelayKey{}, true)

	in := map[string]any{"remote": "192.0.2.1"}
	if out := markRelayedRemote(plain, in); out["remote_via_relay"] != nil {
		t.Fatalf("direct connection marked: %v", out)
	}
	out := markRelayedRemote(relayed, in)
	if out["remote_via_relay"] != true || out["remote"] != "192.0.2.1" {
		t.Fatalf("relayed remote: %v", out)
	}
	if _, ok := in["remote_via_relay"]; ok {
		t.Fatal("caller's details modified")
	}
	if out := markRelayedRemote(relayed, map[string]any{"peer": "192.0.2.1"}); out["remote_via_relay"] != true {
		t.Fatalf("relayed peer: %v", out)
	}
	if out := markRelayedRemote(relayed, map[string]any{"reason": "x"}); out["remote_via_relay"] != nil {
		t.Fatalf("details without an address marked: %v", out)
	}
}
