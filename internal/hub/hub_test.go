package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/identity"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func initTestHub(t *testing.T) (InitOptions, *InitResult) {
	t.Helper()
	root := t.TempDir()
	opts := InitOptions{
		HubDir:       filepath.Join(root, "hub"),
		NodeDir:      filepath.Join(root, "node"),
		AdvertiseURL: "https://localhost:7777",
		LocalNode:    "hubnode",
		Logger:       quietLogger(),
	}
	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return opts, res
}

type testHub struct {
	opts  InitOptions
	init  *InitResult
	ready Ready
	pool  *x509.CertPool
}

func startTestHub(t *testing.T) *testHub { t.Helper(); return startTestHubWith(t, false) }

// startTestHubWith starts a hub; allowFake offers the fake sandbox profile.
func startTestHubWith(t *testing.T, allowFake bool) *testHub {
	t.Helper()
	opts, res := initTestHub(t)
	h, _ := runTestHub(t, opts, res, allowFake)
	return h
}

// runTestHub runs the hub initialised in opts until stop is called or the
// test ends (stop may be called more than once).
func runTestHub(t *testing.T, opts InitOptions, res *InitResult, allowFake bool) (*testHub, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan Ready, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			HubDir: opts.HubDir, APIListen: "127.0.0.1:0", BusListen: "127.0.0.1:0", Logger: quietLogger(),
			AllowFakeProfile: allowFake,
		}, readyCh)
	}()
	var ready Ready
	select {
	case ready = <-readyCh:
	case err := <-done:
		cancel()
		t.Fatalf("Run exited early: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("hub not ready")
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(res.CACertPEM) {
		t.Fatal("bad CA PEM")
	}
	return &testHub{opts: opts, init: res, ready: ready, pool: pool}, stop
}

func (h *testHub) httpClient(cert *tls.Certificate) *http.Client {
	cfg := &tls.Config{RootCAs: h.pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 15 * time.Second}
}

func bearer(key string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if key != "" {
				req.Header().Set("Authorization", "Bearer "+key)
			}
			return next(ctx, req)
		}
	}))
}

type clients struct {
	tokens mycelv1connect.TokenServiceClient
	nodes  mycelv1connect.NodeServiceClient
	users  mycelv1connect.UserServiceClient
	agents mycelv1connect.AgentServiceClient
	enrol  mycelv1connect.EnrolServiceClient
}

func (h *testHub) clients(key string, cert *tls.Certificate) clients {
	hc := h.httpClient(cert)
	o := bearer(key)
	u := h.ready.HubURL
	return clients{
		tokens: mycelv1connect.NewTokenServiceClient(hc, u, o),
		nodes:  mycelv1connect.NewNodeServiceClient(hc, u, o),
		users:  mycelv1connect.NewUserServiceClient(hc, u, o),
		agents: mycelv1connect.NewAgentServiceClient(hc, u, o),
		enrol:  mycelv1connect.NewEnrolServiceClient(hc, u, o),
	}
}

func createToken(t *testing.T, c clients, name string, caps []string, ttl string) string {
	t.Helper()
	resp, err := c.tokens.CreateJoinToken(context.Background(), connect.NewRequest(&mycelv1.CreateJoinTokenRequest{
		NodeName: name, Caps: &mycelv1.Capabilities{Names: caps}, Ttl: ttl,
	}))
	if err != nil {
		t.Fatalf("CreateJoinToken %s: %v", name, err)
	}
	return resp.Msg.GetToken()
}

type enrolled struct {
	name string
	cert tls.Certificate
	x509 *x509.Certificate
	resp *mycelv1.EnrolResponse
}

func tryEnrol(c clients, token, name string) (*enrolled, error) {
	key, keyPEM, err := identity.GenerateKey()
	if err != nil {
		return nil, err
	}
	csr, err := identity.NewCSR(key, name)
	if err != nil {
		return nil, err
	}
	resp, err := c.enrol.Enrol(context.Background(), connect.NewRequest(&mycelv1.EnrolRequest{Token: token, CsrDer: csr}))
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(resp.Msg.GetCertPem(), keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &enrolled{name: name, cert: cert, x509: leaf, resp: resp.Msg}, nil
}

func enrol(t *testing.T, c clients, token, name string) *enrolled {
	t.Helper()
	e, err := tryEnrol(c, token, name)
	if err != nil {
		t.Fatalf("Enrol %s: %v", name, err)
	}
	return e
}

func wantCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v, got success", code)
	}
	if got := connect.CodeOf(err); got != code {
		t.Fatalf("want %v, got %v (%v)", code, got, err)
	}
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// standIn is a node connected to the bus that answers ctl requests.
type standIn struct {
	nc      *nats.Conn
	revoked atomic.Bool
	silent  atomic.Bool  // when set, ctl requests are received but never answered
	refuse  atomic.Value // string: when non-empty, CreateSandbox is refused with it
	ctl     chan *mycelv1.CtlRequest
}

func (h *testHub) connectNode(t *testing.T, e *enrolled) *standIn {
	t.Helper()
	sn := &standIn{ctl: make(chan *mycelv1.CtlRequest, 16)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := bus.Connect(ctx, bus.ClientConfig{
		URL: "tls://" + h.ready.BusAddr, NodeName: e.name, Cert: e.cert, RootCAs: h.pool,
		Logger: quietLogger(), OnAuthRevoked: func() { sn.revoked.Store(true) },
	})
	if err != nil {
		t.Fatalf("bus.Connect %s: %v", e.name, err)
	}
	t.Cleanup(nc.Close)
	sn.nc = nc
	if _, err := nc.Subscribe(bus.NodeCtl(e.name), func(m *nats.Msg) {
		var req mycelv1.CtlRequest
		if proto.Unmarshal(m.Data, &req) == nil {
			select {
			case sn.ctl <- &req:
			default:
			}
		}
		if m.Reply != "" && !sn.silent.Load() {
			resp := &mycelv1.CtlResponse{Ok: true}
			if why, _ := sn.refuse.Load().(string); why != "" && req.GetCreateSandbox() != nil {
				resp = &mycelv1.CtlResponse{Error: why}
			}
			b, _ := proto.Marshal(resp)
			_ = m.Respond(b)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return sn
}

func (sn *standIn) rpc(t *testing.T, node, method string, req, resp proto.Message) (string, error) {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sn.nc.Request(bus.NodeRPC(node, method), b, 5*time.Second)
	if err != nil {
		t.Fatalf("rpc %s: %v", method, err)
	}
	if m.Header.Get(rpcStatusHeader) != "ok" {
		var cr mycelv1.CtlResponse
		_ = proto.Unmarshal(m.Data, &cr)
		if cr.GetOk() || cr.GetError() == "" {
			t.Fatalf("error reply without CtlResponse error: %+v", &cr)
		}
		return "", errors.New(cr.GetError())
	}
	if err := proto.Unmarshal(m.Data, resp); err != nil {
		t.Fatal(err)
	}
	return "ok", nil
}

func getNode(t *testing.T, c clients, name string) *mycelv1.Node {
	t.Helper()
	r, err := c.nodes.GetNode(context.Background(), connect.NewRequest(&mycelv1.GetNodeRequest{Name: name}))
	if err != nil {
		t.Fatalf("GetNode %s: %v", name, err)
	}
	return r.Msg.GetNode()
}

func createUser(t *testing.T, c clients, name string, role mycelv1.Role) (string, string) {
	t.Helper()
	r, err := c.users.CreateUser(context.Background(), connect.NewRequest(&mycelv1.CreateUserRequest{Name: name, Role: role}))
	if err != nil {
		t.Fatalf("CreateUser %s: %v", name, err)
	}
	return r.Msg.GetUser().GetId(), r.Msg.GetApiKey()
}

// ---- tests ----

func TestInitWritesStateAndRefusesSecondInit(t *testing.T) {
	opts, res := initTestHub(t)
	if res.TrustDomain != "localhost" || res.LocalNodeName != "hubnode" || !strings.HasPrefix(res.AdminAPIKey, "mck_") {
		t.Fatalf("unexpected result: %+v", res)
	}
	files := map[string]os.FileMode{
		caCertFile: 0, caKeyFile: 0o600, serverCertFile: 0, serverKeyFile: 0o600,
		agentKeyFile: 0o600, hubConfigFile: 0, dbFile: 0,
	}
	for f, mode := range files {
		fi, err := os.Stat(filepath.Join(opts.HubDir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if mode != 0 && runtime.GOOS != "windows" && fi.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", f, fi.Mode().Perm(), mode)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(opts.HubDir, agentKeyFile)); len(b) != 32 {
		t.Errorf("agent key is %d bytes", len(b))
	}
	if fi, err := os.Stat(filepath.Join(opts.HubDir, jetStreamSubdir)); err != nil || !fi.IsDir() {
		t.Errorf("jetstream dir: %v", err)
	}
	cfg, err := ReadConfig(opts.HubDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BusAdvertise != "tls://localhost:7778" || cfg.APIListen != ":7777" || cfg.BusListen != ":7778" || cfg.TrustDomain != "localhost" {
		t.Errorf("hub.yaml: %+v", cfg)
	}
	st, err := daemon.ReadState(opts.NodeDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "hubnode" || st.OwnerID != res.AdminUserID || st.BusURL != "tls://localhost:7778" ||
		st.SPIFFEID != "spiffe://localhost/node/hubnode" || len(st.Caps) != 4 {
		t.Errorf("node state: %+v", st)
	}
	certPEM, _ := os.ReadFile(filepath.Join(opts.NodeDir, daemon.CertFile))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(res.CACertPEM)
	if _, name, err := identity.VerifyNodeCert(pool, "localhost", certPEM); err != nil || name != "hubnode" {
		t.Errorf("local node cert: %v %q", err, name)
	}

	// Second init into the same hub dir fails, even with a fresh node dir.
	opts2 := opts
	opts2.NodeDir = filepath.Join(t.TempDir(), "node")
	if _, err := Init(context.Background(), opts2); err == nil {
		t.Fatal("second Init succeeded")
	}
}

func TestInitRefusesExistingNodeBeforeCreatingAnything(t *testing.T) {
	opts, _ := initTestHub(t)
	hubDir := filepath.Join(t.TempDir(), "hub2")
	opts.HubDir = hubDir
	if _, err := Init(context.Background(), opts); err == nil {
		t.Fatal("Init with an enrolled node dir succeeded")
	}
	if _, err := os.Stat(hubDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hub dir was created: %v", err)
	}
}

func TestInitValidation(t *testing.T) {
	root := t.TempDir()
	for _, adv := range []string{"http://hub:7777", "hub:7777", "https://[::1]:7777"} {
		_, err := Init(context.Background(), InitOptions{
			HubDir: filepath.Join(root, "h"), NodeDir: filepath.Join(root, "n"), AdvertiseURL: adv, Logger: quietLogger(),
		})
		if err == nil {
			t.Errorf("Init with advertise %q succeeded", adv)
		}
	}
}

func TestSanitizeNodeName(t *testing.T) {
	for in, want := range map[string]string{
		"Richards-MacBook-Pro.local": "richards-macbook-pro",
		"DESKTOP_AB12":               "desktop-ab12",
		"--weird__host--":            "weird-host",
		"":                           "hub",
		strings.Repeat("a", 70):      strings.Repeat("a", 63),
	} {
		if got := sanitizeNodeName(in); got != want || !identity.ValidNodeName(got) {
			t.Errorf("sanitizeNodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidReply(t *testing.T) {
	for reply, want := range map[string]bool{
		"_INBOX.alpha.abc.1": true,
		"_INBOX.alpha.":      false,
		"_INBOX.alphax.abc":  false,
		"_INBOX.beta.abc":    false,
		"node.other.ctl":     false,
		"_INBOX.alpha.*":     false,
		"_INBOX.alpha.>":     false,
		"":                   false,
	} {
		if got := validReply("alpha", reply); got != want {
			t.Errorf("validReply(%q) = %v, want %v", reply, got, want)
		}
	}
}

func TestHubEndToEnd(t *testing.T) {
	h := startTestHub(t)
	ctx := context.Background()
	admin := h.clients(h.init.AdminAPIKey, nil)
	anon := h.clients("", nil)

	// Misc HTTP endpoints.
	hc := h.httpClient(nil)
	for path, want := range map[string]string{"/ca.pem": "BEGIN CERTIFICATE", "/healthz": "ok", "/": "Mycelium hub localhost"} {
		resp, err := hc.Get(h.ready.HubURL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d %q", path, resp.StatusCode, b)
		}
	}

	_, err := anon.nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = h.clients("mck_bogus", nil).nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// Whoami.
	who, err := admin.users.Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if who.Msg.GetUser().GetId() != h.init.AdminUserID || who.Msg.GetUser().GetRole() != mycelv1.Role_ROLE_ADMIN ||
		who.Msg.GetTrustDomain() != "localhost" || who.Msg.GetCaFingerprint() != h.init.CAFingerprint ||
		who.Msg.GetHubUrl() != "https://localhost:7777" {
		t.Fatalf("whoami: %+v", who.Msg)
	}

	// Users.
	aliceID, aliceKey := createUser(t, admin, "alice", mycelv1.Role_ROLE_MEMBER)
	bobID, bobKey := createUser(t, admin, "bob", mycelv1.Role_ROLE_MEMBER)
	alice := h.clients(aliceKey, nil)
	bob := h.clients(bobKey, nil)
	_, err = alice.users.CreateUser(ctx, connect.NewRequest(&mycelv1.CreateUserRequest{Name: "mallory", Role: mycelv1.Role_ROLE_ADMIN}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = admin.users.CreateUser(ctx, connect.NewRequest(&mycelv1.CreateUserRequest{Name: "alice", Role: mycelv1.Role_ROLE_MEMBER}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = admin.users.CreateUser(ctx, connect.NewRequest(&mycelv1.CreateUserRequest{Name: "carol"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = admin.users.RemoveUser(ctx, connect.NewRequest(&mycelv1.RemoveUserRequest{Id: h.init.AdminUserID}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = alice.users.CreateApiKey(ctx, connect.NewRequest(&mycelv1.CreateApiKeyRequest{UserId: bobID}))
	wantCode(t, err, connect.CodePermissionDenied)
	k, err := alice.users.CreateApiKey(ctx, connect.NewRequest(&mycelv1.CreateApiKeyRequest{}))
	if err != nil || k.Msg.GetKey().GetUserId() != aliceID {
		t.Fatalf("CreateApiKey self: %v", err)
	}
	if w, err := h.clients(k.Msg.GetApiKey(), nil).users.Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{})); err != nil || w.Msg.GetUser().GetName() != "alice" {
		t.Fatalf("new key whoami: %v", err)
	}

	// Tokens: admin sees all, members their own.
	adminTok := createToken(t, admin, "alpha", []string{"docker", "gpu"}, "")
	createToken(t, admin, "beta", nil, "10m")
	aliceTok := createToken(t, alice, "alice-box", []string{"ollama"}, "")
	_, err = admin.tokens.CreateJoinToken(ctx, connect.NewRequest(&mycelv1.CreateJoinTokenRequest{NodeName: "x", Caps: &mycelv1.Capabilities{Names: []string{"fly"}}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = admin.tokens.CreateJoinToken(ctx, connect.NewRequest(&mycelv1.CreateJoinTokenRequest{NodeName: "x", Ttl: "25h"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = admin.tokens.CreateJoinToken(ctx, connect.NewRequest(&mycelv1.CreateJoinTokenRequest{NodeName: "Bad_Name"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = admin.tokens.CreateJoinToken(ctx, connect.NewRequest(&mycelv1.CreateJoinTokenRequest{NodeName: "hubnode"}))
	wantCode(t, err, connect.CodeAlreadyExists)

	list := func(c clients) []*mycelv1.JoinToken {
		r, err := c.tokens.ListJoinTokens(ctx, connect.NewRequest(&mycelv1.ListJoinTokensRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetTokens()
	}
	if got := list(admin); len(got) != 3 {
		t.Fatalf("admin sees %d tokens", len(got))
	}
	aliceList := list(alice)
	if len(aliceList) != 1 || aliceList[0].GetNodeName() != "alice-box" || aliceList[0].GetOwnerId() != aliceID {
		t.Fatalf("alice sees %+v", aliceList)
	}
	if got := list(bob); len(got) != 0 {
		t.Fatalf("bob sees %d tokens", len(got))
	}
	_, err = bob.tokens.RevokeJoinToken(ctx, connect.NewRequest(&mycelv1.RevokeJoinTokenRequest{Id: aliceList[0].GetId()}))
	wantCode(t, err, connect.CodePermissionDenied)

	// A revoked token cannot enrol.
	revTok := createToken(t, bob, "bob-box", nil, "")
	bobList := list(bob)
	if _, err := bob.tokens.RevokeJoinToken(ctx, connect.NewRequest(&mycelv1.RevokeJoinTokenRequest{Id: bobList[0].GetId()})); err != nil {
		t.Fatal(err)
	}
	_, err = tryEnrol(anon, revTok, "bob-box")
	wantCode(t, err, connect.CodePermissionDenied)

	// Enrol.
	alpha := enrol(t, anon, adminTok, "alpha")
	if alpha.resp.GetNodeId() != "node:alpha" || alpha.resp.GetSpiffeId() != "spiffe://localhost/node/alpha" ||
		alpha.resp.GetBusUrl() != "tls://localhost:7778" || alpha.resp.GetHubUrl() != "https://localhost:7777" ||
		alpha.resp.GetOwnerId() != h.init.AdminUserID || !slices.Equal(alpha.resp.GetCaps().GetNames(), []string{"docker", "gpu"}) {
		t.Fatalf("enrol response: %+v", alpha.resp)
	}
	n := getNode(t, admin, "alpha")
	if n.GetOnline() || n.GetOwnerId() != h.init.AdminUserID || n.GetCertSerial() != identity.SerialHex(alpha.x509) {
		t.Fatalf("alpha: %+v", n)
	}
	nodes, err := alice.nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	if err != nil || len(nodes.Msg.GetNodes()) != 2 {
		t.Fatalf("ListNodes: %v %d", err, len(nodes.Msg.GetNodes()))
	}

	// Reuse, tamper, existing name.
	_, err = tryEnrol(anon, adminTok, "alpha")
	wantCode(t, err, connect.CodePermissionDenied)
	if !strings.Contains(err.Error(), "invalid or expired token") {
		t.Fatalf("message: %v", err)
	}
	jt, err := identity.ParseJoinToken(aliceTok)
	if err != nil {
		t.Fatal(err)
	}
	jt.Secret[0] ^= 0xff
	_, err = tryEnrol(anon, jt.String(), "alice-box")
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = tryEnrol(anon, "mcl1_garbage", "alice-box")
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = admin.tokens.CreateJoinToken(ctx, connect.NewRequest(&mycelv1.CreateJoinTokenRequest{NodeName: "alpha"}))
	wantCode(t, err, connect.CodeAlreadyExists)

	aliceBox := enrol(t, anon, aliceTok, "alice-box")
	if aliceBox.resp.GetOwnerId() != aliceID {
		t.Fatalf("alice-box owner %q", aliceBox.resp.GetOwnerId())
	}
	betaTok := createToken(t, admin, "beta2", nil, "")
	beta := enrol(t, anon, betaTok, "beta2")

	// Expired token (the one fixed sleep).
	expTok := createToken(t, admin, "late", nil, "1s")
	time.Sleep(2100 * time.Millisecond)
	_, err = tryEnrol(anon, expTok, "late")
	wantCode(t, err, connect.CodePermissionDenied)

	// Stand-in nodes on the bus.
	alphaNode := h.connectNode(t, alpha)
	betaNode := h.connectNode(t, beta)
	eventually(t, 5*time.Second, "alpha online", func() bool { return getNode(t, admin, "alpha").GetOnline() })

	// Heartbeats.
	h1, h2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	beat := func(inv *mycelv1.Inventory, hash string) {
		t.Helper()
		b, _ := proto.Marshal(&mycelv1.Heartbeat{NodeName: "spoofed", Inventory: inv, InventoryHash: hash})
		if err := alphaNode.nc.Publish(bus.NodeTelemetry("alpha"), b); err != nil {
			t.Fatal(err)
		}
	}
	// advance sends inventory-less beats until last_seen_at moves past prev.
	advance := func(prev string) string {
		t.Helper()
		var seen string
		eventually(t, 10*time.Second, "last_seen_at to advance", func() bool {
			beat(nil, "")
			seen = getNode(t, admin, "alpha").GetLastSeenAt()
			return seen > prev
		})
		return seen
	}
	cpus := func() int32 { return getNode(t, admin, "alpha").GetInventory().GetCpuCount() }
	beat(&mycelv1.Inventory{Os: "linux", Arch: "amd64", CpuCount: 8, Hostname: "alpha-host"}, h1)
	eventually(t, 5*time.Second, "inventory", func() bool { return cpus() == 8 })
	lastSeen := getNode(t, admin, "alpha").GetLastSeenAt()
	if lastSeen == "" || getNode(t, admin, "alpha").GetInventory().GetHostname() != "alpha-host" {
		t.Fatalf("after heartbeat: %+v", getNode(t, admin, "alpha"))
	}
	_, err = admin.nodes.GetNode(ctx, connect.NewRequest(&mycelv1.GetNodeRequest{Name: "spoofed"}))
	wantCode(t, err, connect.CodeNotFound)
	// Without inventory: last_seen_at advances, inventory unchanged.
	lastSeen = advance(lastSeen)
	if cpus() != 8 {
		t.Fatal("inventory changed by a beat without inventory")
	}
	// Same hash, different inventory: not rewritten. Malformed hash and
	// oversized inventory: dropped.
	beat(&mycelv1.Inventory{CpuCount: 16}, h1)
	beat(&mycelv1.Inventory{CpuCount: 17}, "H1")
	beat(&mycelv1.Inventory{CpuCount: 18, CpuModel: strings.Repeat("x", maxInventoryBytes)}, h2)
	advance(lastSeen)
	if cpus() != 8 {
		t.Fatalf("inventory rewritten: %d cpus", cpus())
	}
	// New hash: rewritten.
	beat(&mycelv1.Inventory{CpuCount: 32}, h2)
	eventually(t, 5*time.Second, "new inventory", func() bool { return cpus() == 32 })

	// Agent register and refresh.
	var reg mycelv1.RegisterAgentResponse
	if _, err := alphaNode.rpc(t, "alpha", bus.RPCRegisterAgent, &mycelv1.RegisterAgentRequest{AgentName: "coder", SandboxId: "sbx:1", Harness: "claude-code"}, &reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.GetAgentId() != "agent:coder@alpha" || !strings.HasPrefix(reg.GetToken(), identity.AgentTokenPrefix) || reg.GetExpiresAt() == "" {
		t.Fatalf("register response: %+v", &reg)
	}
	for _, bad := range []*mycelv1.RegisterAgentRequest{
		{AgentName: "Bad.Name"},
		{AgentName: "ok", SandboxId: "sbx 1"},
		{AgentName: "ok", SandboxId: strings.Repeat("s", 129)},
		{AgentName: "ok", Harness: "Claude_Code"},
		{AgentName: "ok", Harness: strings.Repeat("h", 65)},
	} {
		var r mycelv1.RegisterAgentResponse
		if _, err := alphaNode.rpc(t, "alpha", bus.RPCRegisterAgent, bad, &r); err == nil {
			t.Fatalf("invalid register request accepted: %+v", bad)
		}
	}
	var ref mycelv1.RefreshAgentTokenResponse
	if _, err := alphaNode.rpc(t, "alpha", bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: reg.GetAgentId(), Token: reg.GetToken()}, &ref); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !strings.HasPrefix(ref.GetToken(), identity.AgentTokenPrefix) {
		t.Fatalf("refresh response: %+v", &ref)
	}
	// Binding: alpha's token presented over beta's connection is refused.
	if _, err := betaNode.rpc(t, "beta2", bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: reg.GetAgentId(), Token: reg.GetToken()}, &ref); err == nil {
		t.Fatal("token refreshed over another node's connection")
	}
	agents, err := bob.agents.ListAgents(ctx, connect.NewRequest(&mycelv1.ListAgentsRequest{}))
	if err != nil || len(agents.Msg.GetAgents()) != 1 || agents.Msg.GetAgents()[0].GetOwnerId() != h.init.AdminUserID ||
		agents.Msg.GetAgents()[0].GetState() != "running" {
		t.Fatalf("ListAgents: %v %+v", err, agents)
	}
	_, err = bob.agents.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: reg.GetAgentId()}))
	wantCode(t, err, connect.CodePermissionDenied)
	if _, err := admin.agents.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: reg.GetAgentId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := alphaNode.rpc(t, "alpha", bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: reg.GetAgentId(), Token: ref.GetToken()}, &ref); err == nil {
		t.Fatal("revoked agent refreshed")
	}

	// Caps: subset of the ceiling, owner or admin.
	_, err = bob.nodes.SetNodeCaps(ctx, connect.NewRequest(&mycelv1.SetNodeCapsRequest{Name: "alpha", CapsEnabled: &mycelv1.Capabilities{Names: []string{"docker"}}}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = admin.nodes.SetNodeCaps(ctx, connect.NewRequest(&mycelv1.SetNodeCapsRequest{Name: "alpha", CapsEnabled: &mycelv1.Capabilities{Names: []string{"ollama"}}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	sc, err := admin.nodes.SetNodeCaps(ctx, connect.NewRequest(&mycelv1.SetNodeCapsRequest{Name: "alpha", CapsEnabled: &mycelv1.Capabilities{Names: []string{"docker"}}}))
	if err != nil || !slices.Equal(sc.Msg.GetNode().GetCapsEnabled().GetNames(), []string{"docker"}) {
		t.Fatalf("SetNodeCaps: %v", err)
	}
	select {
	case c := <-alphaNode.ctl:
		if !slices.Equal(c.GetSetCaps().GetCapsEnabled().GetNames(), []string{"docker"}) {
			t.Fatalf("ctl: %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SetCaps ctl")
	}

	// Renew with a live cert: new serial, later NotAfter (the sleep above
	// guarantees a later second).
	key, keyPEM, _ := identity.GenerateKey()
	csr, _ := identity.NewCSR(key, "alpha")
	alphaClient := h.clients("", &alpha.cert)
	rn, err := alphaClient.enrol.Renew(ctx, connect.NewRequest(&mycelv1.RenewRequest{CsrDer: csr}))
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	newCert, err := tls.X509KeyPair(rn.Msg.GetCertPem(), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	newLeaf, _ := x509.ParseCertificate(newCert.Certificate[0])
	if identity.SerialHex(newLeaf) == identity.SerialHex(alpha.x509) || !newLeaf.NotAfter.After(alpha.x509.NotAfter) {
		t.Fatalf("renewed cert: serial %s notAfter %v (old %v)", identity.SerialHex(newLeaf), newLeaf.NotAfter, alpha.x509.NotAfter)
	}
	if getNode(t, admin, "alpha").GetCertSerial() != identity.SerialHex(newLeaf) {
		t.Fatal("node serial not updated")
	}
	_, err = anon.enrol.Renew(ctx, connect.NewRequest(&mycelv1.RenewRequest{CsrDer: csr}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// Remove: a member who does not own the node is denied; the owner succeeds.
	aliceNode := h.connectNode(t, aliceBox)
	eventually(t, 5*time.Second, "alice-box online", func() bool { return getNode(t, admin, "alice-box").GetOnline() })
	_, err = bob.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "alice-box"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = bob.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "no-such-node"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = admin.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "no-such-node"}))
	wantCode(t, err, connect.CodeNotFound)
	var aliceReg mycelv1.RegisterAgentResponse
	if _, err := aliceNode.rpc(t, "alice-box", bus.RPCRegisterAgent, &mycelv1.RegisterAgentRequest{AgentName: "helper"}, &aliceReg); err != nil {
		t.Fatalf("register on alice-box: %v", err)
	}
	_, err = admin.users.RemoveUser(ctx, connect.NewRequest(&mycelv1.RemoveUserRequest{Id: aliceID}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if _, err := alice.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "alice-box"})); err != nil {
		t.Fatalf("RemoveNode by owner: %v", err)
	}
	select {
	case c := <-aliceNode.ctl:
		if c.GetRevoked() == nil {
			t.Fatalf("ctl: %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Revoked ctl")
	}
	eventually(t, 5*time.Second, "OnAuthRevoked", aliceNode.revoked.Load)
	nodes, err = admin.nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes.Msg.GetNodes() {
		if n.GetName() == "alice-box" {
			t.Fatal("removed node still listed")
		}
	}
	agents, err = admin.agents.ListAgents(ctx, connect.NewRequest(&mycelv1.ListAgentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range agents.Msg.GetAgents() {
		if a.GetId() == aliceReg.GetAgentId() {
			found = true
			if a.GetState() != "stopped" {
				t.Fatalf("agent of removed node: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("agent of removed node was deleted")
	}
	st, err := openStore(filepath.Join(h.opts.HubDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if !st.IsRevoked(identity.SerialHex(aliceBox.x509), "") || !st.IsRevoked("00", "spiffe://localhost/node/alice-box") {
		t.Fatal("removed node not revoked")
	}
	_, err = h.clients("", &aliceBox.cert).enrol.Renew(ctx, connect.NewRequest(&mycelv1.RenewRequest{CsrDer: csr}))
	wantCode(t, err, connect.CodePermissionDenied)
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("renew after removal: %v", err)
	}
	// A reconnect with the old cert is refused.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = bus.Connect(cctx, bus.ClientConfig{URL: "tls://" + h.ready.BusAddr, NodeName: "alice-box", Cert: aliceBox.cert, RootCAs: h.pool, Logger: quietLogger()})
	cancel()
	if err == nil {
		t.Fatal("revoked node reconnected")
	}
	// Now alice owns no nodes and can be removed; her key stops working.
	if _, err := admin.users.RemoveUser(ctx, connect.NewRequest(&mycelv1.RemoveUserRequest{Id: aliceID})); err != nil {
		t.Fatal(err)
	}
	_, err = alice.users.Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// Audit trail.
	entries, err := st.listAudit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action+"/"+e.Outcome] = true
		for _, secret := range []string{h.init.AdminAPIKey, aliceKey, adminTok, reg.GetToken(), "BEGIN"} {
			if strings.Contains(e.Details, secret) {
				t.Fatalf("audit leaks a secret: %+v", e)
			}
		}
	}
	for _, want := range []string{"hub.init/ok", "user.create/ok", "user.create/denied", "token.create/ok", "enrol/ok", "enrol/denied",
		"agent.register/ok", "agent.refresh/ok", "agent.refresh/denied", "agent.revoke/ok", "node.caps.set/ok",
		"renew/ok", "renew/denied", "node.remove/denied", "node.remove/ok", "user.remove/ok", "token.revoke/ok"} {
		if !seen[want] {
			t.Errorf("no audit entry %s", want)
		}
	}
}

// TestRPCReplyGuard: a node cannot make the hub publish onto a subject outside
// its own inbox space (here another node's ctl subject).
func TestRPCReplyGuard(t *testing.T) {
	h := startTestHub(t)
	admin := h.clients(h.init.AdminAPIKey, nil)
	anon := h.clients("", nil)
	a := enrol(t, anon, createToken(t, admin, "attacker", nil, ""), "attacker")
	v := enrol(t, anon, createToken(t, admin, "other", nil, ""), "other")
	attacker := h.connectNode(t, a)
	victim := h.connectNode(t, v)

	b, _ := proto.Marshal(&mycelv1.RegisterAgentRequest{AgentName: "evil"})
	if err := attacker.nc.PublishRequest(bus.NodeRPC("attacker", bus.RPCRegisterAgent), bus.NodeCtl("other"), b); err != nil {
		t.Fatal(err)
	}
	_ = attacker.nc.Flush()
	select {
	case c := <-victim.ctl:
		t.Fatalf("victim received %+v", c)
	case <-time.After(time.Second):
	}
	st, err := openStore(filepath.Join(h.opts.HubDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	eventually(t, 5*time.Second, "audit of dropped rpc", func() bool {
		entries, _ := st.listAudit(context.Background())
		for _, e := range entries {
			if e.Actor == "node:attacker" && e.Outcome == outcomeError && strings.Contains(e.Details, "reply") {
				return true
			}
		}
		return false
	})
	agents, err := admin.agents.ListAgents(context.Background(), connect.NewRequest(&mycelv1.ListAgentsRequest{}))
	if err != nil || len(agents.Msg.GetAgents()) != 0 {
		t.Fatalf("agent registered despite bad reply: %v %d", err, len(agents.Msg.GetAgents()))
	}
}
