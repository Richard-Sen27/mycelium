package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/hub"
	"github.com/richard-sen27/mycelium/internal/version"
)

const testAPIKey = "mck_testkey"

// fakeHub implements the hub services the CLI calls and records requests.
type fakeHub struct {
	mycelv1connect.UnimplementedTokenServiceHandler
	mycelv1connect.UnimplementedNodeServiceHandler
	mycelv1connect.UnimplementedUserServiceHandler

	mu       sync.Mutex
	authz    []string
	lastReq  proto.Message
	validKey string
	denyAll  bool
}

func (f *fakeHub) check(h http.Header, req proto.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authz = append(f.authz, h.Get("Authorization"))
	f.lastReq = req
	if h.Get("Authorization") != "Bearer "+f.validKey {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid api key"))
	}
	if f.denyAll {
		return connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}
	return nil
}

func (f *fakeHub) CreateJoinToken(_ context.Context, r *connect.Request[mycelv1.CreateJoinTokenRequest]) (*connect.Response[mycelv1.CreateJoinTokenResponse], error) {
	if err := f.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.CreateJoinTokenResponse{
		Token: "mcl1_secrettoken",
		JoinToken: &mycelv1.JoinToken{
			Id: "jt_1", NodeName: r.Msg.GetNodeName(), Caps: r.Msg.GetCaps(),
			ExpiresAt: "2026-10-28T15:00:00Z",
		},
	}), nil
}

func (f *fakeHub) ListJoinTokens(_ context.Context, r *connect.Request[mycelv1.ListJoinTokensRequest]) (*connect.Response[mycelv1.ListJoinTokensResponse], error) {
	if err := f.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListJoinTokensResponse{Tokens: []*mycelv1.JoinToken{
		{Id: "jt_1", NodeName: "gpu-box", Caps: &mycelv1.Capabilities{Names: []string{"gpu"}}, ExpiresAt: "2026-10-28T15:00:00Z", UsedAt: "2026-10-28T14:10:00Z"},
	}}), nil
}

func (f *fakeHub) ListNodes(_ context.Context, r *connect.Request[mycelv1.ListNodesRequest]) (*connect.Response[mycelv1.ListNodesResponse], error) {
	if err := f.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListNodesResponse{Nodes: []*mycelv1.Node{
		{
			Name: "gpu-box", OwnerId: "usr_1", Online: true, LastSeenAt: "2026-10-28T14:59:50Z",
			CapsEnabled: &mycelv1.Capabilities{Names: []string{"docker", "gpu"}},
			Inventory: &mycelv1.Inventory{
				Os: "linux", Arch: "amd64", CpuCount: 16, MemBytes: 64 << 30,
				Gpus: []*mycelv1.Gpu{{Index: 0, Vendor: "nvidia", Model: "RTX 4090"}},
			},
		},
		{Name: "mac", OwnerId: "usr_2"},
	}}), nil
}

func (f *fakeHub) Whoami(_ context.Context, r *connect.Request[mycelv1.WhoamiRequest]) (*connect.Response[mycelv1.WhoamiResponse], error) {
	if err := f.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.WhoamiResponse{
		User:   &mycelv1.User{Id: "usr_1", Name: "alice", Role: mycelv1.Role_ROLE_ADMIN},
		HubUrl: "https://hub.test",
	}), nil
}

func (f *fakeHub) CreateUser(_ context.Context, r *connect.Request[mycelv1.CreateUserRequest]) (*connect.Response[mycelv1.CreateUserResponse], error) {
	if err := f.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.CreateUserResponse{ //nolint:gosec // fake key for tests
		User:   &mycelv1.User{Id: "usr_9", Name: r.Msg.GetName(), Role: r.Msg.GetRole()},
		ApiKey: "mck_newkey",
	}), nil
}

func (f *fakeHub) lastRequest() proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func (f *fakeHub) headers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authz...)
}

type testEnv struct {
	srv   *httptest.Server
	hub   *fakeHub
	ch    *fakeChannels
	jobs  *fakeJobs
	home  string
	caPEM string
	fp    string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	fh := &fakeHub{validKey: testAPIKey}
	mux := http.NewServeMux()
	mux.Handle(mycelv1connect.NewTokenServiceHandler(fh))
	mux.Handle(mycelv1connect.NewNodeServiceHandler(fh))
	mux.Handle(mycelv1connect.NewUserServiceHandler(fh))
	fc := &fakeChannels{hub: fh}
	mux.Handle(mycelv1connect.NewChannelServiceHandler(fc))
	mux.Handle(mycelv1connect.NewAgentServiceHandler(fc))
	fj := newFakeJobs(fh)
	mux.Handle(mycelv1connect.NewJobServiceHandler(fj))
	mux.Handle(mycelv1connect.NewArtifactServiceHandler(fj))
	mux.HandleFunc("PUT /api/artifacts", fj.putArtifact)
	mux.HandleFunc("GET /api/artifacts/{id}", fj.getArtifactBytes)
	var caPEM []byte
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(caPEM) })
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	sum := sha256.Sum256(srv.Certificate().Raw)

	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	t.Setenv(EnvHubURL, "")
	t.Setenv(EnvAPIKey, "")
	return &testEnv{srv: srv, hub: fh, ch: fc, jobs: fj, home: home, caPEM: string(caPEM), fp: hex.EncodeToString(sum[:])}
}

// writeConfig points the CLI at the test server.
func (e *testEnv) writeConfig(t *testing.T) {
	t.Helper()
	d, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(d, &config.File{HubURL: e.srv.URL, APIKey: testAPIKey, CACertPEM: e.caPEM}); err != nil {
		t.Fatal(err)
	}
}

type result struct {
	code           int
	stdout, stderr string
}

// testApp is an app with captured output, no TTY, no browser and role entry
// points that fail the test unless a test replaces them.
type testApp struct {
	*app
	out, errb bytes.Buffer
}

func newTestApp(t *testing.T, stdin string) *testApp {
	t.Helper()
	ta := &testApp{}
	ta.app = newApp(&ta.out, &ta.errb, strings.NewReader(stdin))
	ta.isTTY = func() bool { return false }
	ta.openURL = func(u string) { t.Errorf("unexpected browser open of %s", u) }
	ta.now = func() time.Time { return time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC) }
	ta.hubInit = func(context.Context, hub.InitOptions) (*hub.InitResult, error) {
		t.Error("unexpected hub.Init")
		return nil, errors.New("unexpected")
	}
	ta.hubRun = func(context.Context, hub.Options, chan<- hub.Ready) error {
		t.Error("unexpected hub.Run")
		return errors.New("unexpected")
	}
	ta.daemonJoin = func(context.Context, daemon.JoinOptions) (*daemon.JoinResult, error) {
		t.Error("unexpected daemon.Join")
		return nil, errors.New("unexpected")
	}
	ta.daemonRun = func(context.Context, daemon.Options, chan<- daemon.Ready) error {
		t.Error("unexpected daemon.Run")
		return errors.New("unexpected")
	}
	return ta
}

func (ta *testApp) run(args ...string) result {
	code := ta.execute(args)
	return result{code, ta.out.String(), ta.errb.String()}
}

func runCLI(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return newTestApp(t, stdin).run(args...)
}

func TestCommands(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		noConfig  bool
		denyAll   bool
		wantCode  int
		wantOut   []string
		wantErr   []string
		notOut    []string
		checkReq  func(t *testing.T, m proto.Message)
		checkJSON bool
	}{
		{
			name:     "token create sends request and prints token",
			args:     []string{"token", "create", "--name", "gpu-box", "--caps", "docker,ollama", "--caps", "gpu", "--ttl", "30m"},
			wantCode: 0,
			wantOut:  []string{"mycel join mcl1_secrettoken", "2026-10-28T15:00:00Z", `"gpu-box"`},
			checkReq: func(t *testing.T, m proto.Message) {
				want := &mycelv1.CreateJoinTokenRequest{NodeName: "gpu-box", Caps: &mycelv1.Capabilities{Names: []string{"docker", "ollama", "gpu"}}, Ttl: "30m"}
				if !proto.Equal(m, want) {
					t.Errorf("request = %v, want %v", m, want)
				}
			},
		},
		{
			name:      "token create json",
			args:      []string{"token", "create", "--name", "gpu-box", "--json"},
			wantOut:   []string{`"token":"mcl1_secrettoken"`, `"join_token":{`, `"node_name":"gpu-box"`},
			checkJSON: true,
			checkReq: func(t *testing.T, m proto.Message) {
				if got := m.(*mycelv1.CreateJoinTokenRequest).GetTtl(); got != "15m" {
					t.Errorf("default ttl = %q, want 15m", got)
				}
			},
		},
		{
			name:     "token create without name is a usage error",
			args:     []string{"token", "create"},
			wantCode: 2,
			wantErr:  []string{"mycel: --name is required"},
		},
		{
			name:    "token ls table",
			args:    []string{"token", "ls"},
			wantOut: []string{"ID", "NODE", "CAPS", "EXPIRES", "USED", "jt_1", "gpu-box", "2026-10-28T14:10:00Z"},
		},
		{
			name: "nodes ls human",
			args: []string{"nodes", "ls"},
			wantOut: []string{
				"NAME", "OWNER", "ONLINE", "OS/ARCH", "CPU", "RAM", "GPUS", "CAPS", "LAST SEEN",
				"gpu-box", "linux/amd64", "64.0 GiB", "docker,gpu", "10s ago", "yes",
				"mac",
			},
		},
		{
			name:      "nodes ls json",
			args:      []string{"--json", "nodes", "ls"},
			wantOut:   []string{`"nodes":[`, `"name":"gpu-box"`, `"cpu_count":16`, `"last_seen_at":"2026-10-28T14:59:50Z"`, `"online":false`},
			notOut:    []string{"NAME"},
			checkJSON: true,
		},
		{
			name:    "user add prints key once",
			args:    []string{"user", "add", "bob", "--role", "admin"},
			wantOut: []string{"mck_newkey", "admin bob"},
			checkReq: func(t *testing.T, m proto.Message) {
				if r := m.(*mycelv1.CreateUserRequest); r.GetRole() != mycelv1.Role_ROLE_ADMIN || r.GetName() != "bob" {
					t.Errorf("request = %v", r)
				}
			},
		},
		{
			name:     "user add bad role",
			args:     []string{"user", "add", "bob", "--role", "root"},
			wantCode: 2,
			wantErr:  []string{"invalid role"},
		},
		{
			name:    "whoami",
			args:    []string{"whoami"},
			wantOut: []string{"alice", "admin"},
		},
		{
			name:     "permission denied is a clear error",
			args:     []string{"token", "ls"},
			denyAll:  true,
			wantCode: 1,
			wantErr:  []string{"mycel: permission denied: admin role required"},
		},
		{
			name:     "bad api key flag is unauthenticated",
			args:     []string{"--api-key", "mck_wrong", "nodes", "ls"},
			wantCode: 1,
			wantErr:  []string{"mycel: authentication failed: invalid api key", "mycel login"},
		},
		{
			name:     "missing config",
			args:     []string{"nodes", "ls"},
			noConfig: true,
			wantCode: 1,
			wantErr:  []string{"not connected to a hub", "hub_url", "mycel login", "mycel hub init"},
		},
		{
			name:     "unknown command is a usage error",
			args:     []string{"frobnicate"},
			wantCode: 2,
			wantErr:  []string{"unknown command"},
		},
		{
			name:     "missing positional is a usage error",
			args:     []string{"nodes", "get"},
			wantCode: 2,
		},
		{
			name:     "unknown subcommand of a group is a usage error",
			args:     []string{"nodes", "foo"},
			wantCode: 2,
			wantErr:  []string{`unknown command "foo"`},
		},
		{
			name:    "group without subcommand prints help",
			args:    []string{"token"},
			wantOut: []string{"Available Commands", "create"},
		},
		{
			name:     "unknown flag is a usage error",
			args:     []string{"nodes", "ls", "--bogus"},
			wantCode: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			if !tt.noConfig {
				env.writeConfig(t)
			}
			env.hub.denyAll = tt.denyAll
			res := runCLI(t, "", tt.args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			for _, s := range tt.wantOut {
				if !strings.Contains(res.stdout, s) {
					t.Errorf("stdout missing %q:\n%s", s, res.stdout)
				}
			}
			for _, s := range tt.notOut {
				if strings.Contains(res.stdout, s) {
					t.Errorf("stdout contains %q:\n%s", s, res.stdout)
				}
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(res.stderr, s) {
					t.Errorf("stderr missing %q:\n%s", s, res.stderr)
				}
			}
			if tt.checkJSON && !json.Valid([]byte(res.stdout)) {
				t.Errorf("stdout is not JSON: %s", res.stdout)
			}
			if tt.checkReq != nil {
				tt.checkReq(t, env.hub.lastRequest())
			}
		})
	}
}

func TestBearerHeaderSent(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	if res := runCLI(t, "", "whoami"); res.code != 0 {
		t.Fatalf("whoami failed: %+v", res)
	}
	h := env.hub.headers()
	if len(h) != 1 || h[0] != "Bearer "+testAPIKey {
		t.Fatalf("Authorization headers = %q", h)
	}
}

func TestOverridePrecedence(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)

	// Env overrides the file.
	t.Setenv(EnvAPIKey, "mck_fromenv")
	if res := runCLI(t, "", "whoami"); res.code != 1 {
		t.Fatalf("env key should be used and rejected, got %+v", res)
	}
	// The flag overrides env.
	if res := runCLI(t, "", "--api-key", testAPIKey, "whoami"); res.code != 0 {
		t.Fatalf("flag key should win: %+v", res)
	}
	h := env.hub.headers()
	if h[0] != "Bearer mck_fromenv" || h[1] != "Bearer "+testAPIKey {
		t.Fatalf("headers = %q", h)
	}
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name      string
		fp        func(e *testEnv) string
		args      []string
		wantCode  int
		wantSaved bool
		wantErr   string
	}{
		{name: "matching fingerprint", fp: func(e *testEnv) string { return e.fp }, wantSaved: true},
		{name: "matching fingerprint with colons upper case", fp: func(e *testEnv) string { return colonHex(strings.ToUpper(e.fp)) }, wantSaved: true},
		{name: "mismatching fingerprint", fp: func(*testEnv) string { return strings.Repeat("ab", 32) }, wantCode: 1, wantErr: "fingerprint mismatch"},
		{name: "no fingerprint and no tty refuses", fp: func(*testEnv) string { return "" }, wantCode: 1, wantErr: "--ca-fingerprint"},
		{name: "wrong key is refused", fp: func(e *testEnv) string { return e.fp }, args: []string{"--api-key", "mck_bad"}, wantCode: 1, wantErr: "authentication failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			args := []string{"login", env.srv.URL + "/"}
			if len(tt.args) > 0 {
				args = append(args, tt.args...)
			} else {
				args = append(args, "--api-key", testAPIKey)
			}
			if fp := tt.fp(env); fp != "" {
				args = append(args, "--ca-fingerprint", fp)
			}
			res := runCLI(t, "", args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d: %+v", res.code, tt.wantCode, res)
			}
			if tt.wantErr != "" && !strings.Contains(res.stderr, tt.wantErr) {
				t.Errorf("stderr missing %q: %s", tt.wantErr, res.stderr)
			}
			d, _ := config.Resolve()
			f, err := config.Load(d)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.wantSaved {
				if f.APIKey != "" || f.CACertPEM != "" {
					t.Fatalf("config written on refusal: %+v", f)
				}
				return
			}
			if f.HubURL != env.srv.URL || f.APIKey != testAPIKey || f.CACertPEM != env.caPEM {
				t.Fatalf("saved config = %+v", f)
			}
			if !strings.Contains(res.stdout, "alice") {
				t.Errorf("stdout: %s", res.stdout)
			}
			// The saved config now works for other commands.
			if res := runCLI(t, "", "nodes", "ls"); res.code != 0 {
				t.Fatalf("nodes ls after login: %+v", res)
			}
		})
	}
}

func TestPinnedCARejectsOtherServer(t *testing.T) {
	env := newTestEnv(t)
	// Pin an unrelated CA: the test server's certificate must be rejected.
	d, _ := config.Resolve()
	if err := config.Save(d, &config.File{HubURL: env.srv.URL, APIKey: testAPIKey, CACertPEM: newCAPEM(t)}); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, "", "nodes", "ls")
	if res.code != 1 || !strings.Contains(res.stderr, "certificate") {
		t.Fatalf("expected TLS verification failure, got %+v", res)
	}
}

func TestVersion(t *testing.T) {
	newTestEnv(t)
	res := runCLI(t, "", "version")
	if res.code != 0 || strings.TrimSpace(res.stdout) != version.Version {
		t.Fatalf("version: %+v", res)
	}
	res = runCLI(t, "", "version", "--json")
	if res.code != 0 || strings.TrimSpace(res.stdout) != `{"version":"`+version.Version+`"}` {
		t.Fatalf("version --json: %+v", res)
	}
}

func TestDaemonNothingToRun(t *testing.T) {
	newTestEnv(t)
	res := runCLI(t, "", "daemon")
	if res.code != 1 || !strings.Contains(res.stderr, "nothing to run") {
		t.Fatalf("daemon: %+v", res)
	}
}

func TestUI(t *testing.T) {
	tests := []struct {
		name     string
		hubRole  bool
		args     []string
		wantURL  string
		wantOpen bool
	}{
		{name: "node default", args: []string{"ui"}, wantURL: "http://127.0.0.1:7777", wantOpen: true},
		{name: "local addr flag", args: []string{"ui", "--local-addr", "127.0.0.1:9999"}, wantURL: "http://127.0.0.1:9999", wantOpen: true},
		{name: "hub machine", hubRole: true, args: []string{"ui"}, wantURL: "https://localhost:7777", wantOpen: true},
		{name: "json never opens", args: []string{"ui", "--json"}, wantURL: `{"url":"http://127.0.0.1:7777"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tt.hubRole {
				writeFile(t, filepath.Join(env.home, "data", "hub", "hub.yaml"), "x: 1\n")
			}
			ta := newTestApp(t, "")
			var opened string
			ta.openURL = func(u string) { opened = u }
			res := ta.run(tt.args...)
			if res.code != 0 || strings.TrimSpace(res.stdout) != tt.wantURL {
				t.Fatalf("ui: %+v", res)
			}
			if tt.wantOpen && opened != tt.wantURL || !tt.wantOpen && opened != "" {
				t.Fatalf("opened %q", opened)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonExit(t *testing.T) {
	tests := []struct {
		name    string
		results []roleResult
		want    int
	}{
		{"clean", []roleResult{{"hub", nil}, {"node", context.Canceled}}, 0},
		{"revoked", []roleResult{{"hub", nil}, {"node", fmt.Errorf("conn: %w", daemon.ErrRevoked)}}, 3},
		{"failure", []roleResult{{"hub", errors.New("bind: address in use")}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := daemonExit(tt.results)
			code := 0
			var ec exitCodeError
			switch {
			case err == nil:
			case errors.As(err, &ec):
				code = ec.code
			default:
				code = 1
			}
			if code != tt.want {
				t.Fatalf("code = %d, want %d (%v)", code, tt.want, err)
			}
		})
	}
}

func TestConfigFilePermissions(t *testing.T) {
	env := newTestEnv(t)
	res := runCLI(t, "", "login", env.srv.URL, "--api-key", testAPIKey, "--ca-fingerprint", env.fp)
	if res.code != 0 {
		t.Fatalf("login: %+v", res)
	}
	if strings.Contains(res.stdout+res.stderr, testAPIKey) {
		t.Errorf("login printed the API key")
	}
	st, err := os.Stat(filepath.Join(env.home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("config.yaml mode = %v", st.Mode())
	}
}

func newCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func colonHex(s string) string {
	var parts []string
	for i := 0; i < len(s); i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, ":")
}
