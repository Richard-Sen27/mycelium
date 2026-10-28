package cli

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/hub"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

func TestRelayTokenCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	res := runCLI(t, "", "relay", "token", "create", "--name", "hub1", "--host", "relay.example.com", "--data-dir", dir, "--json")
	if res.code != 0 {
		t.Fatalf("create: %+v", res)
	}
	var out struct{ Name, URL, Fingerprint string }
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatal(err)
	}
	u, err := tunnel.ParseRelayURL(out.URL)
	if err != nil || u.Host != "relay.example.com" || u.Port != tunnel.DefaultControlPort || u.Name != "hub1" || u.Fingerprint != out.Fingerprint {
		t.Fatalf("URL %q: %+v %v", out.URL, u, err)
	}
	if strings.Contains(res.stderr, u.Token) {
		t.Fatal("secret logged")
	}
	// One hub per relay: a second token needs --replace, which revokes the
	// first.
	for _, name := range []string{"hub1", "hub2"} {
		if res := runCLI(t, "", "relay", "token", "create", "--name", name, "--host", "relay.example.com", "--data-dir", dir); res.code == 0 || !strings.Contains(res.stderr, "--replace") {
			t.Fatalf("second token for %s: %+v", name, res)
		}
	}
	res = runCLI(t, "", "relay", "token", "create", "--name", "hub1", "--host", "relay.example.com", "--data-dir", dir, "--replace")
	if res.code != 0 || strings.Contains(res.stdout, u.Token) {
		t.Fatalf("replace: %+v", res)
	}
	// The URL is printed on its own; the hub reads it from stdin.
	if !strings.Contains(res.stdout, "mycel hub tunnel set -") || strings.Contains(res.stdout, "tunnel set '") || strings.Contains(res.stdout, "tunnel set relay://") {
		t.Fatalf("create output:\n%s", res.stdout)
	}
	var newURL string
	for _, f := range strings.Fields(res.stdout) {
		if strings.HasPrefix(f, "relay://") {
			newURL = f
		}
	}
	nu, err := tunnel.ParseRelayURL(newURL)
	if err != nil || nu.Token == u.Token {
		t.Fatalf("replaced URL %q: %v", newURL, err)
	}
	// Without --host the hostname is used, with a warning.
	res = runCLI(t, "", "relay", "token", "create", "--name", "hub1", "--data-dir", dir, "--replace")
	if res.code == 0 && !strings.Contains(res.stderr, "--host not given") {
		t.Fatalf("default host without a warning: %+v", res)
	}
	if res := runCLI(t, "", "relay", "token", "create", "--name", "Bad", "--data-dir", dir); res.code != exitUsage {
		t.Fatalf("bad name: %+v", res)
	}
	res = runCLI(t, "", "relay", "token", "ls", "--data-dir", dir)
	if res.code != 0 || !strings.Contains(res.stdout, "hub1") || strings.Contains(res.stdout, u.Token) {
		t.Fatalf("ls: %+v", res)
	}
	if res := runCLI(t, "", "relay", "token", "revoke", "hub1", "--data-dir", dir); res.code != 0 {
		t.Fatalf("revoke: %+v", res)
	}
	if res := runCLI(t, "", "relay", "token", "revoke", "hub1", "--data-dir", dir); res.code == 0 {
		t.Fatal("second revoke succeeded")
	}
	if res := runCLI(t, "", "relay", "serve", "--public", "x", "--data-dir", dir); res.code != exitUsage {
		t.Fatalf("serve with bad ports: %+v", res)
	}
}

func TestHubInitTunnelAndACMEFlags(t *testing.T) {
	newTestEnv(t)
	url := "relay://relay.example.com:7790/hub1?fp=" + strings.Repeat("ab", 32) + "&token=" + strings.Repeat("A", 43)

	// ACME needs a public name on 443: refused before anything is created.
	for _, args := range [][]string{
		{"hub", "init", "--advertise", "https://localhost:7777", "--acme"},
		{"hub", "init", "--advertise", "https://hub.example.com:7777", "--acme"},
		{"hub", "init", "--advertise", "https://hub.example.com", "--acme-email", "a@example.com"},
		{"hub", "init", "--tunnel", "relay://bad"},
		{"hub", "init", "--tunnel", url, "--advertise", "https://other.example.com"},
		// --advertise with --tunnel must be the tunnel's advertise URL.
		{"hub", "init", "--tunnel", url, "--advertise", "https://relay.example.com:8443"},
		{"hub", "init", "--tunnel", url, "--advertise", "https://relay.example.com/x"},
		{"hub", "init", "--tunnel", "-"}, // nothing on stdin
	} {
		if res := runCLI(t, "", args...); res.code != exitUsage {
			t.Errorf("%v: %+v", args, res)
		}
	}

	// The URL comes from stdin with "-".
	ta := newTestApp(t, url+"\n")
	var got hub.InitOptions
	ta.hubInit = func(ctx context.Context, o hub.InitOptions) (*hub.InitResult, error) {
		got = o
		return hub.Init(ctx, o)
	}
	res := ta.run("hub", "init", "--tunnel", "-", "--advertise", "https://RELAY.example.com:443/", "--acme", "--acme-email", "ops@example.com", "--node-name", "hubnode")
	if res.code != 0 {
		t.Fatalf("init: %+v", res)
	}
	if got.BusAdvertise != "tls://relay.example.com:7778" || !got.LocalListeners {
		t.Fatalf("init options: %+v", got)
	}
	if strings.Contains(res.stderr, "Warning: the relay URL contains a secret") {
		t.Fatal("warned about argv although the URL came from stdin")
	}
	dirs, _ := config.Resolve()
	// This machine's node and CLI use the local listeners, not the relay.
	st, err := daemon.ReadState(dirs.NodeDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.HubURL != "https://127.0.0.1:7777" || st.BusURL != "tls://127.0.0.1:7778" {
		t.Fatalf("local node state: hub %q bus %q", st.HubURL, st.BusURL)
	}
	if f, err := config.Load(dirs); err != nil || f.HubURL != "https://127.0.0.1:7777" {
		t.Fatalf("CLI config hub URL: %v %v", f, err)
	}
	cfg, err := hub.ReadConfig(dirs.HubDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tunnel.Enabled || !cfg.ACME.Enabled || cfg.ACME.Email != "ops@example.com" {
		t.Fatalf("hub.yaml: %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(dirs.HubDir(), tunnel.RelayURLFile)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.stdout+res.stderr, strings.Repeat("A", 43)) {
		t.Fatal("relay secret printed")
	}

	// tunnel unset / set on the existing hub.
	if res := runCLI(t, "", "hub", "tunnel", "unset"); res.code != 0 {
		t.Fatalf("unset: %+v", res)
	}
	if res := runCLI(t, url+"\n", "hub", "tunnel", "set", "-", "--ports", "443=api,7778=web"); res.code != exitUsage {
		t.Fatalf("set with bad ports: %+v", res)
	}
	res = runCLI(t, url+"\n", "hub", "tunnel", "set", "-")
	if res.code != 0 || !strings.Contains(res.stdout, "relay://relay.example.com:7790/hub1") || strings.Contains(res.stdout+res.stderr, strings.Repeat("A", 43)) {
		t.Fatalf("set: %+v", res)
	}
	// On argv it still works, with a warning.
	res = runCLI(t, "", "hub", "tunnel", "set", url)
	if res.code != 0 || !strings.Contains(res.stderr, "contains a secret") {
		t.Fatalf("set on argv: %+v", res)
	}
}

func TestHubTunnelStatus(t *testing.T) {
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/tunnel" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"enabled":true,"ports":{"443":"api","7778":"bus"},"relay":"relay://r.example.com:7790/hub1","connected":true,"since":"2026-10-28T10:00:00Z","connects":1,"active":2,"total":5,"acme":{"enabled":true,"host":"r.example.com"}}`))
	}))
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	t.Setenv(config.EnvHome, t.TempDir())
	d, _ := config.Resolve()
	if err := config.Save(d, &config.File{HubURL: srv.URL, APIKey: testAPIKey, CACertPEM: string(caPEM)}); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, "", "hub", "tunnel", "status")
	if res.code != 0 {
		t.Fatalf("status: %+v", res)
	}
	for _, want := range []string{"enabled", "relay://r.example.com:7790/hub1", "443=api,7778=bus", "connected since", "2 open, 5 total, 1 sessions", "enabled for r.example.com"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("status output lacks %q:\n%s", want, res.stdout)
		}
	}
	if auth != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q", auth)
	}
	res = runCLI(t, "", "--json", "hub", "tunnel", "status")
	if res.code != 0 || !strings.Contains(res.stdout, `"connected":true`) {
		t.Fatalf("status --json: %+v", res)
	}
}
