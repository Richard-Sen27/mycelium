package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/hub"
)

func initRoles(t *testing.T, home string, hubRole, nodeRole bool) {
	t.Helper()
	if hubRole {
		writeFile(t, filepath.Join(home, "data", "hub", "hub.yaml"), "x: 1\n")
	}
	if nodeRole {
		writeFile(t, filepath.Join(home, "data", "node", "node.yaml"), "x: 1\n")
	}
}

func TestDaemonRoles(t *testing.T) {
	type fakes struct {
		hub  func(ctx context.Context, ready chan<- hub.Ready, cancel context.CancelFunc) error
		node func(ctx context.Context, ready chan<- daemon.Ready, cancel context.CancelFunc) error
	}
	hubReadyThenWait := func(ctx context.Context, ready chan<- hub.Ready, _ context.CancelFunc) error {
		ready <- hub.Ready{APIAddr: "127.0.0.1:1111", BusAddr: "127.0.0.1:2222", HubURL: "https://127.0.0.1:1111"}
		<-ctx.Done()
		return ctx.Err()
	}
	tests := []struct {
		name          string
		hubRole       bool
		nodeRole      bool
		args          []string
		fakes         fakes
		wantCode      int
		wantLine      string // exact stdout; "" means nothing printed
		wantErr       string
		wantLocalAddr string
	}{
		{
			name: "both ready then signal", hubRole: true, nodeRole: true,
			args: []string{"daemon", "--json"},
			fakes: fakes{
				hub: hubReadyThenWait,
				node: func(ctx context.Context, ready chan<- daemon.Ready, cancel context.CancelFunc) error {
					time.Sleep(50 * time.Millisecond) // the hub is ready first; no line yet
					ready <- daemon.Ready{LocalAddr: "127.0.0.1:3333"}
					time.Sleep(20 * time.Millisecond)
					cancel() // stands in for SIGINT
					<-ctx.Done()
					return nil
				},
			},
			wantCode:      0,
			wantLine:      `{"hub":{"api_addr":"127.0.0.1:1111","bus_addr":"127.0.0.1:2222"},"node":{"local_addr":"127.0.0.1:3333"}}`,
			wantLocalAddr: hubMachineLocalAddr,
		},
		{
			name: "node only uses default local addr", nodeRole: true,
			args: []string{"daemon", "--json"},
			fakes: fakes{node: func(ctx context.Context, ready chan<- daemon.Ready, cancel context.CancelFunc) error {
				ready <- daemon.Ready{LocalAddr: "127.0.0.1:7777"}
				cancel()
				return ctx.Err()
			}},
			wantLine:      `{"node":{"local_addr":"127.0.0.1:7777"}}`,
			wantLocalAddr: defaultLocalAddr,
		},
		{
			name: "one role failing stops the other", hubRole: true, nodeRole: true,
			args: []string{"daemon", "--json"},
			fakes: fakes{
				hub: hubReadyThenWait,
				node: func(context.Context, chan<- daemon.Ready, context.CancelFunc) error {
					return errors.New("cannot reach hub")
				},
			},
			wantCode: 1,
			wantErr:  "mycel: node: cannot reach hub",
		},
		{
			name: "revoked exits 3", hubRole: true, nodeRole: true,
			args: []string{"daemon", "--json"},
			fakes: fakes{
				hub: hubReadyThenWait,
				node: func(_ context.Context, ready chan<- daemon.Ready, _ context.CancelFunc) error {
					ready <- daemon.Ready{LocalAddr: "127.0.0.1:3333"}
					return daemon.ErrRevoked
				},
			},
			wantCode: 3,
			wantLine: `{"hub":{"api_addr":"127.0.0.1:1111","bus_addr":"127.0.0.1:2222"},"node":{"local_addr":"127.0.0.1:3333"}}`,
			wantErr:  "revoked",
		},
		{
			name: "role returning nil early is an error", hubRole: true,
			args: []string{"daemon", "--local-addr", "127.0.0.1:5555"},
			fakes: fakes{hub: func(context.Context, chan<- hub.Ready, context.CancelFunc) error {
				return nil
			}},
			wantCode: 1,
			wantErr:  "mycel: hub: exited unexpectedly",
		},
		{
			name: "listen flags without hub role warn", nodeRole: true,
			args: []string{"daemon", "--api-listen", ":9"},
			fakes: fakes{node: func(ctx context.Context, ready chan<- daemon.Ready, cancel context.CancelFunc) error {
				ready <- daemon.Ready{LocalAddr: "127.0.0.1:7777"}
				cancel()
				return ctx.Err()
			}},
			wantErr: "only apply to the hub role",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			initRoles(t, env.home, tt.hubRole, tt.nodeRole)
			ta := newTestApp(t, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ta.baseCtx = ctx

			var hubStopped atomic.Bool
			var gotLocalAddr string
			if tt.fakes.hub != nil {
				ta.hubRun = func(c context.Context, o hub.Options, r chan<- hub.Ready) error {
					if o.HubDir != filepath.Join(env.home, "data", "hub") {
						t.Errorf("HubDir = %q", o.HubDir)
					}
					err := tt.fakes.hub(c, r, cancel)
					hubStopped.Store(true)
					return err
				}
			}
			if tt.fakes.node != nil {
				ta.daemonRun = func(c context.Context, o daemon.Options, r chan<- daemon.Ready) error {
					gotLocalAddr = o.LocalAddr
					return tt.fakes.node(c, r, cancel)
				}
			}
			res := ta.run(tt.args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d: %+v", res.code, tt.wantCode, res)
			}
			if got := strings.TrimSpace(res.stdout); got != tt.wantLine {
				t.Errorf("stdout = %q, want %q", got, tt.wantLine)
			}
			if tt.wantErr != "" && !strings.Contains(res.stderr, tt.wantErr) {
				t.Errorf("stderr missing %q: %s", tt.wantErr, res.stderr)
			}
			if tt.fakes.hub != nil && !hubStopped.Load() {
				t.Error("hub role still running after daemon returned")
			}
			if tt.wantLocalAddr != "" && gotLocalAddr != tt.wantLocalAddr {
				t.Errorf("LocalAddr = %q, want %q", gotLocalAddr, tt.wantLocalAddr)
			}
		})
	}
}

func fakeInitResult() *hub.InitResult {
	return &hub.InitResult{
		TrustDomain:   "team",
		AdvertiseURL:  "https://hub.example.com:7777",
		CAFingerprint: "abcd",
		CACertPEM:     []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"),
		AdminUserID:   "usr_admin",
		AdminAPIKey:   "mck_adminsecret",
		LocalNodeName: "laptop",
	}
}

func TestHubInit(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonOut], func(t *testing.T) {
			env := newTestEnv(t)
			ta := newTestApp(t, "")
			var got hub.InitOptions
			ta.hubInit = func(_ context.Context, o hub.InitOptions) (*hub.InitResult, error) {
				got = o
				return fakeInitResult(), nil
			}
			args := []string{"hub", "init", "--advertise", "https://hub.example.com:7777", "--admin", "root", "--node-name", "laptop"}
			if jsonOut {
				args = append(args, "--json")
			}
			res := ta.run(args...)
			if res.code != 0 {
				t.Fatalf("hub init: %+v", res)
			}
			if got.HubDir != filepath.Join(env.home, "data", "hub") || got.NodeDir != filepath.Join(env.home, "data", "node") ||
				got.AdminName != "root" || got.LocalNode != "laptop" || got.APIListen != ":7777" || got.BusListen != ":7778" {
				t.Errorf("InitOptions = %+v", got)
			}
			if n := strings.Count(res.stdout, "mck_adminsecret"); n != 1 {
				t.Errorf("admin key printed %d times:\n%s", n, res.stdout)
			}
			if strings.Contains(res.stderr, "mck_adminsecret") {
				t.Error("admin key on stderr")
			}
			if jsonOut {
				var m map[string]string
				if err := json.Unmarshal([]byte(res.stdout), &m); err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"trust_domain": "team", "hub_url": "https://hub.example.com:7777", "ca_fingerprint": "abcd",
					"admin_user_id": "usr_admin", "admin_api_key": "mck_adminsecret", "local_node_name": "laptop"}
				for k, v := range want {
					if m[k] != v {
						t.Errorf("%s = %q, want %q", k, m[k], v)
					}
				}
			}
			d, _ := config.Resolve()
			f, err := config.Load(d)
			if err != nil {
				t.Fatal(err)
			}
			want := config.File{HubURL: "https://hub.example.com:7777", APIKey: "mck_adminsecret", CACertPEM: string(fakeInitResult().CACertPEM), NodeName: "laptop"}
			if *f != want {
				t.Errorf("config = %+v", f)
			}
		})
	}
}

func TestHubInitPrintsKeyWhenSaveFails(t *testing.T) {
	env := newTestEnv(t)
	// A directory where config.yaml should be makes loading and saving fail.
	if err := os.Mkdir(filepath.Join(env.home, "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	ta := newTestApp(t, "")
	ta.hubInit = func(context.Context, hub.InitOptions) (*hub.InitResult, error) { return fakeInitResult(), nil }
	res := ta.run("hub", "init", "--advertise", "https://hub.example.com:7777")
	if res.code != 1 || !strings.Contains(res.stdout, "mck_adminsecret") || !strings.Contains(res.stderr, "keep the admin key") {
		t.Fatalf("result: %+v", res)
	}
}

func TestJoin(t *testing.T) {
	const caPEM = "-----BEGIN CERTIFICATE-----\njoined\n-----END CERTIFICATE-----\n"
	joined := "https://hub.example.com:7777"
	tests := []struct {
		name    string
		initial config.File
		want    config.File
	}{
		{
			name:    "empty config gets hub, ca and node",
			initial: config.File{},
			want:    config.File{HubURL: joined, CACertPEM: caPEM, NodeName: "gpu-box"},
		},
		{
			name:    "same hub updates ca",
			initial: config.File{HubURL: joined + "/", APIKey: "mck_k", CACertPEM: "old"},
			want:    config.File{HubURL: joined, APIKey: "mck_k", CACertPEM: caPEM, NodeName: "gpu-box"},
		},
		{
			name:    "different hub left untouched",
			initial: config.File{HubURL: "https://other.example.com", APIKey: "mck_k", CACertPEM: "other"},
			want:    config.File{HubURL: "https://other.example.com", APIKey: "mck_k", CACertPEM: "other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			d, _ := config.Resolve()
			initial := tt.initial
			if err := config.Save(d, &initial); err != nil {
				t.Fatal(err)
			}
			ta := newTestApp(t, "")
			var got daemon.JoinOptions
			ta.daemonJoin = func(_ context.Context, o daemon.JoinOptions) (*daemon.JoinResult, error) {
				got = o
				writeFile(t, filepath.Join(o.NodeDir, "ca.crt"), caPEM)
				return &daemon.JoinResult{NodeName: "gpu-box", SPIFFEID: "spiffe://team/node/gpu-box", HubURL: joined, Caps: []string{"gpu"}}, nil
			}
			res := ta.run("join", "mcl1_tok", "--hub-url", "https://alt.example.com:7777", "--json")
			if res.code != 0 {
				t.Fatalf("join: %+v", res)
			}
			if got.Token != "mcl1_tok" || got.HubURL != "https://alt.example.com:7777" || got.NodeDir != filepath.Join(env.home, "data", "node") {
				t.Errorf("JoinOptions = %+v", got)
			}
			if !strings.Contains(res.stdout, `"node_name":"gpu-box"`) || !strings.Contains(res.stdout, `"caps":["gpu"]`) {
				t.Errorf("stdout = %s", res.stdout)
			}
			f, err := config.Load(d)
			if err != nil {
				t.Fatal(err)
			}
			if *f != tt.want {
				t.Errorf("config = %+v, want %+v", f, tt.want)
			}
		})
	}
}

func TestLoginPrompt(t *testing.T) {
	tests := []struct {
		name      string
		stdin     string
		json      bool
		wantCode  int
		wantSaved bool
	}{
		{name: "y accepts", stdin: "y\n", wantSaved: true},
		{name: "yes accepts", stdin: "YES\n", wantSaved: true},
		{name: "n refuses", stdin: "n\n", wantCode: 1},
		{name: "empty refuses", stdin: "\n", wantCode: 1},
		{name: "eof refuses", stdin: "", wantCode: 1},
		{name: "json refuses on a tty", stdin: "y\n", json: true, wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			ta := newTestApp(t, tt.stdin)
			ta.isTTY = func() bool { return true }
			args := []string{"login", env.srv.URL, "--api-key", testAPIKey}
			if tt.json {
				args = append(args, "--json")
			}
			res := ta.run(args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit = %d, want %d: %+v", res.code, tt.wantCode, res)
			}
			if !tt.json && !strings.Contains(res.stderr, env.fp) {
				t.Errorf("prompt does not show the fingerprint: %s", res.stderr)
			}
			if tt.json && strings.Contains(res.stderr, "Trust this hub?") {
				t.Error("prompted despite --json")
			}
			d, _ := config.Resolve()
			f, _ := config.Load(d)
			if saved := f.CACertPEM != ""; saved != tt.wantSaved {
				t.Errorf("saved = %v, want %v", saved, tt.wantSaved)
			}
		})
	}
}

func TestHubURLPrecedence(t *testing.T) {
	const bad = "https://127.0.0.1:1"
	tests := []struct {
		name     string
		file     string
		env      string
		flag     string
		wantCode int
	}{
		{name: "file", file: "good", wantCode: 0},
		{name: "env over file", file: bad, env: "good", wantCode: 0},
		{name: "env over file (bad env)", file: "good", env: bad, wantCode: 1},
		{name: "flag over env", file: bad, env: bad, flag: "good", wantCode: 0},
		{name: "flag over env (bad flag)", file: "good", env: "good", flag: bad, wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			resolve := func(s string) string {
				if s == "good" {
					return env.srv.URL
				}
				return s
			}
			d, _ := config.Resolve()
			if err := config.Save(d, &config.File{HubURL: resolve(tt.file), APIKey: testAPIKey, CACertPEM: env.caPEM}); err != nil {
				t.Fatal(err)
			}
			if tt.env != "" {
				t.Setenv(EnvHubURL, resolve(tt.env))
			}
			args := []string{"whoami"}
			if tt.flag != "" {
				args = append(args, "--hub-url", resolve(tt.flag))
			}
			res := runCLI(t, "", args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit = %d, want %d: %+v", res.code, tt.wantCode, res)
			}
			if tt.wantCode == 1 && !strings.Contains(res.stderr, "127.0.0.1:1") {
				t.Errorf("error does not name the hub: %s", res.stderr)
			}
		})
	}
}
