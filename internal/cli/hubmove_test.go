package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/discovery"
	"github.com/richard-sen27/mycelium/internal/hub"
	"github.com/richard-sen27/mycelium/internal/identity"
)

type fakeBrowser struct{ hubs []discovery.Hub }

func (f fakeBrowser) Browse(context.Context, time.Duration) ([]discovery.Hub, error) {
	return f.hubs, nil
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func TestHubDiscover(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	ta := newTestApp(t, "")
	other := strings.Repeat("b", 64)
	ta.browser = fakeBrowser{hubs: []discovery.Hub{
		{TrustDomain: "team", URL: "https://hub.local:7777", BusURL: "tls://hub.local:7778", CAFingerprint: env.fp, Addresses: []string{"192.0.2.1"}, Port: 7777},
		{TrustDomain: "other", URL: "https://x:7777", CAFingerprint: other},
	}}
	r := ta.run("hub", "discover", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var hubs []discoveredHubJSON
	if err := json.Unmarshal([]byte(r.stdout), &hubs); err != nil || len(hubs) != 2 || !hubs[0].Pinned || hubs[1].Pinned || hubs[1].Addresses == nil {
		t.Fatalf("json %q: %v", r.stdout, err)
	}
	ta = newTestApp(t, "")
	ta.browser = fakeBrowser{}
	if r := ta.run("hub", "discover"); r.code != 0 || !strings.Contains(r.stdout, "No hubs answered") {
		t.Fatalf("empty: %+v", r)
	}
	if r := runCLI(t, "", "hub", "discover", "--timeout", "0s"); r.code != exitUsage {
		t.Fatalf("bad timeout: %+v", r)
	}
}

// TestHubMoveCLI walks init, export (running hub), redirect, --follow-redirect and
// import with a real hub.
func TestHubMoveCLI(t *testing.T) {
	oldHome := t.TempDir()
	t.Setenv(config.EnvHome, oldHome)
	t.Setenv(EnvHubURL, "")
	t.Setenv(EnvAPIKey, "")
	apiPort, busPort := freePort(t), freePort(t)
	hubURL := "https://127.0.0.1:" + apiPort
	newApp := func() *testApp {
		ta := newTestApp(t, "")
		ta.hubInit = hub.Init
		ta.now = time.Now // redirects are checked against the real clock
		return ta
	}
	r := newApp().run("hub", "init", "--advertise", hubURL, "--api-listen", "127.0.0.1:"+apiPort,
		"--bus-listen", "127.0.0.1:"+busPort, "--trust-domain", "move-team", "--node-name", "oldbox", "--no-mdns")
	if r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	dirs, _ := config.Resolve()
	if cfg, err := hub.ReadConfig(dirs.HubDir()); err != nil || cfg.MDNSEnabled() {
		t.Fatalf("--no-mdns: %+v %v", cfg, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan hub.Ready, 1)
	go func() {
		done <- hub.Run(ctx, hub.Options{HubDir: dirs.HubDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, ready)
	}()
	var once sync.Once
	stopHub := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("hub: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("hub did not stop")
			}
		})
	}
	defer stopHub()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("hub: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("hub not ready")
	}

	passFile := filepath.Join(t.TempDir(), "pass")
	writeFile(t, passFile, "a long export passphrase\n")
	out := filepath.Join(t.TempDir(), "hub.mycelhub")

	// Terminal passphrases must match twice.
	ta := newApp()
	n := 0
	ta.readPassword = func(string) ([]byte, error) {
		n++
		return []byte("a long export passphrase" + strconv.Itoa(n)), nil
	}
	if r := ta.run("hub", "export", "--out", out); r.code == 0 || !strings.Contains(r.stderr, "differ") {
		t.Fatalf("mismatched passphrases: %+v", r)
	}
	if r := newApp().run("hub", "export", "--out", out, "--passphrase-file", passFile); r.code != 0 || !strings.Contains(r.stdout, "running hub") {
		t.Fatalf("export: %+v", r)
	}
	if st, err := os.Stat(out); err != nil || st.Size() < 1000 {
		t.Fatalf("export file: %v %v", st, err)
	}

	if r := newApp().run("whoami"); r.code != 0 {
		t.Fatalf("whoami before the move: %+v", r)
	}
	// The target is checked first: nothing answers there.
	if r := newApp().run("hub", "redirect", "https://127.0.0.1:"+freePort(t)); r.code == 0 || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("redirect to a hub that does not answer: %+v", r)
	}
	if r := newApp().run("whoami"); r.code != 0 {
		t.Fatalf("whoami after a refused redirect: %+v", r)
	}
	if r := newApp().run("hub", "redirect", "https://new.example.com:7777", "--force"); r.code != 0 || !strings.Contains(r.stdout, "queued to 1 nodes") {
		t.Fatalf("redirect: %+v", r)
	}
	r = newApp().run("whoami")
	if r.code != exitError || !strings.Contains(r.stderr, "hub moved to https://new.example.com:7777") || !strings.Contains(r.stderr, "--follow-redirect") {
		t.Fatalf("moved answer: %+v", r)
	}
	if f, _ := config.Load(dirs); f.HubURL != hubURL {
		t.Fatalf("config changed without --follow-redirect: %s", f.HubURL)
	}
	if r := newApp().run("whoami", "--follow-redirect"); r.code != exitError || !strings.Contains(r.stderr, "now points at https://new.example.com:7777") {
		t.Fatalf("--follow-redirect: %+v", r)
	}
	f, _ := config.Load(dirs)
	if f.HubURL != "https://new.example.com:7777" || f.RedirectIssuedAt == 0 {
		t.Fatalf("config after --follow-redirect: %+v", f)
	}
	// An older redirect, validly signed, is refused afterwards.
	ca := loadTestCA(t, dirs.HubDir())
	old := signedRedirect(t, ca, "https://older.example.com:7777", "tls://older.example.com:7778", f.RedirectIssuedAt-60)
	ta2 := newApp()
	if code := ta2.followMoved(hubMovedError{r: old}); code != exitError || !strings.Contains(ta2.errb.String(), "older than one already followed") {
		t.Fatalf("older redirect: %d %s", code, ta2.errb.String())
	}
	if f2, _ := config.Load(dirs); f2.HubURL != "https://new.example.com:7777" {
		t.Fatal("config changed by an older redirect")
	}
	stopHub()

	// The new machine.
	newHome := t.TempDir()
	t.Setenv(config.EnvHome, newHome)
	newPort := freePort(t)
	r = newApp().run("hub", "import", out, "--advertise", "https://127.0.0.1:"+newPort, "--api-listen", "127.0.0.1:"+newPort,
		"--bus-listen", "127.0.0.1:"+freePort(t), "--passphrase-file", passFile, "--json")
	if r.code != 0 {
		t.Fatalf("import: %+v", r)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res["trust_domain"] != "move-team" || res["nodes"] != float64(1) || res["old_hub_url"] != hubURL {
		t.Fatalf("import json %q: %v", r.stdout, err)
	}
	ndirs, _ := config.Resolve()
	if f, _ := config.Load(ndirs); f.HubURL != "https://127.0.0.1:"+newPort || f.CACertPEM == "" {
		t.Fatalf("config on the new machine: %+v", f)
	}
	if r := newApp().run("hub", "import", out, "--advertise", "https://127.0.0.1:1", "--passphrase-file", passFile); r.code == 0 || !strings.Contains(r.stderr, "already holds") {
		t.Fatalf("second import: %+v", r)
	}
	if r := newApp().run("hub", "import", out); r.code != exitUsage {
		t.Fatalf("import without --advertise: %+v", r)
	}
}

func TestFollowRefusesUnverifiedRedirect(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	ta := newTestApp(t, "")
	// A redirect not signed by the pinned CA is never followed.
	code := ta.followMoved(hubMovedError{r: forgedRedirect()})
	if code != exitError || !strings.Contains(ta.errb.String(), "not valid for the pinned hub CA") {
		t.Fatalf("code %d: %s", code, ta.errb.String())
	}
	dirs, _ := config.Resolve()
	if f, _ := config.Load(dirs); f.HubURL != env.srv.URL {
		t.Fatal("config changed by an unverified redirect")
	}
	if !errors.As(error(hubMovedError{r: forgedRedirect()}), new(hubMovedError)) {
		t.Fatal("hubMovedError")
	}
}

func forgedRedirect() *mycelv1.Redirect {
	return &mycelv1.Redirect{TrustDomain: "x", HubUrl: "https://evil.example.com", BusUrl: "tls://evil.example.com:1", IssuedAt: time.Now().Unix(), Signature: []byte{1}}
}

func loadTestCA(t *testing.T, hubDir string) *identity.CA {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(hubDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(hubDir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := identity.LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func signedRedirect(t *testing.T, ca *identity.CA, hubURL, busURL string, issued int64) *mycelv1.Redirect {
	t.Helper()
	sig, err := ca.SignRedirect(hubURL, busURL, issued)
	if err != nil {
		t.Fatal(err)
	}
	return &mycelv1.Redirect{TrustDomain: string(ca.TrustDomain()), HubUrl: hubURL, BusUrl: busURL, IssuedAt: issued, Signature: sig}
}

// ---- redirect target check: pinned CA only ----

var sysRoot struct {
	once sync.Once
	ca   *identity.CA
}

// systemRoot injects a "system" root into this process (see the daemon
// tests): a client that left RootCAs nil would trust it.
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
	return sysRoot.ca
}

func certFrom(t *testing.T, ca *identity.CA) tls.Certificate {
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

type noRev struct{}

func (noRev) IsRevoked(string, string) bool { return false }

// targetHub starts a /healthz server and a bus, both with cert.
func targetHub(t *testing.T, cert tls.Certificate, clientCAs *identity.CA) (string, string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	b, err := bus.StartServer(bus.ServerConfig{
		Host: "127.0.0.1", TLSCert: cert, ClientCAs: clientCAs.Pool(), TrustDomain: clientCAs.TrustDomain(),
		StoreDir: t.TempDir(), Revocation: noRev{}, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = b.Shutdown(ctx)
	})
	return srv.URL, b.ClientURL()
}

func TestRedirectTargetCheckPinnedCAOnly(t *testing.T) {
	sys := systemRoot(t)
	hubCA, err := identity.NewCA("move-team")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	goodURL, goodBus := targetHub(t, certFrom(t, hubCA), hubCA)
	if err := checkRedirectTarget(ctx, hubCA.CertPEM(), goodURL, goodBus); err != nil {
		t.Fatalf("a hub with the pinned CA: %v", err)
	}
	sysURL, sysBus := targetHub(t, certFrom(t, sys), hubCA)
	// The injection works: a default client trusts the system-root server.
	if resp, err := (&http.Client{Transport: &http.Transport{}}).Get(sysURL); err != nil {
		t.Fatalf("injected system root not trusted by a default client: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	if err := checkRedirectTarget(ctx, hubCA.CertPEM(), sysURL, goodBus); err == nil || !strings.Contains(err.Error(), "API") {
		t.Fatalf("system-root API accepted: %v", err)
	}
	if err := checkRedirectTarget(ctx, hubCA.CertPEM(), goodURL, sysBus); err == nil || !strings.Contains(err.Error(), "bus") {
		t.Fatalf("system-root bus accepted: %v", err)
	}
	if err := checkRedirectTarget(ctx, hubCA.CertPEM(), goodURL, "tls://"+strings.TrimPrefix(goodURL, "https://")); err == nil {
		t.Fatal("an HTTPS server accepted as the bus")
	}
}

func TestArtifactMovedAnswerIsFollowed(t *testing.T) {
	r := signedRedirect(t, mustTestCA(t), "https://new.example.com:7777", "tls://new.example.com:7778", time.Now().Unix())
	v, err := daemon.EncodeRedirect(r)
	if err != nil {
		t.Fatal(err)
	}
	res := &http.Response{StatusCode: http.StatusPreconditionFailed, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"hub moved"}`))}
	res.Header.Set(daemon.RedirectHeader, v)
	var moved hubMovedError
	if err := artifactHTTPError("https://old:7777", res); !errors.As(err, &moved) || moved.r.GetHubUrl() != "https://new.example.com:7777" {
		t.Fatalf("artifact 412: %v", err)
	}
}

func mustTestCA(t *testing.T) *identity.CA {
	t.Helper()
	ca, err := identity.NewCA("team")
	if err != nil {
		t.Fatal(err)
	}
	return ca
}
