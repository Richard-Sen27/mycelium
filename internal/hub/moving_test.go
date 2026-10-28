package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/discovery"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

// Hub tests never advertise on the real network.
var advertised = struct {
	sync.Mutex
	hubs map[string]discovery.Hub // by CA fingerprint
}{hubs: map[string]discovery.Hub{}}

type fakeAdvertiser struct{ fp string }

func (f fakeAdvertiser) Close() error {
	advertised.Lock()
	delete(advertised.hubs, f.fp)
	advertised.Unlock()
	return nil
}

func init() {
	advertise = func(_ context.Context, h discovery.Hub, _ discovery.AdvertiseOptions) (discovery.Advertiser, error) {
		advertised.Lock()
		advertised.hubs[h.CAFingerprint] = h
		advertised.Unlock()
		return fakeAdvertiser{fp: h.CAFingerprint}, nil
	}
}

func isAdvertised(fp string) (discovery.Hub, bool) {
	advertised.Lock()
	defer advertised.Unlock()
	h, ok := advertised.hubs[fp]
	return h, ok
}

func TestHubAdvertisesUnlessDisabled(t *testing.T) {
	h := startTestHub(t)
	ah, ok := isAdvertised(h.init.CAFingerprint)
	if !ok || ah.TrustDomain != h.init.TrustDomain || ah.URL != h.opts.AdvertiseURL || !strings.HasPrefix(ah.BusURL, "tls://") {
		t.Fatalf("advertised %+v ok=%v", ah, ok)
	}

	root := t.TempDir()
	opts := InitOptions{
		HubDir: filepath.Join(root, "hub"), NodeDir: filepath.Join(root, "node"),
		AdvertiseURL: "https://localhost:7777", LocalNode: "quiet", Logger: quietLogger(), NoMDNS: true,
	}
	res, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(opts.HubDir)
	if err != nil || cfg.MDNSEnabled() {
		t.Fatalf("--no-mdns not written: %+v %v", cfg.Discovery, err)
	}
	runTestHub(t, opts, res, false)
	if _, ok := isAdvertised(res.CAFingerprint); ok {
		t.Fatal("hub with mdns: false advertised")
	}
}

// adminPostTest posts JSON to a hub route with key.
func adminPostTest(t *testing.T, h *testHub, path, key string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, h.ready.HubURL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.httpClient(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRedirectMovedMode(t *testing.T) {
	ctx := context.Background()
	opts, res := initTestHub(t)
	h, stop := runTestHub(t, opts, res, false)
	admin := h.clients(res.AdminAPIKey, nil)
	_, memberKey := createUser(t, admin, "bob", mycelv1.Role_ROLE_MEMBER)
	e := enrol(t, admin, createToken(t, admin, "n1", nil, ""), "n1")
	sn := h.connectNode(t, e)

	// Only an admin API key may redirect.
	if r := adminPostTest(t, h, "/api/admin/redirect", "", map[string]string{"hub_url": "https://new.example.com:7777"}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", r.StatusCode)
	}
	if r := adminPostTest(t, h, "/api/admin/redirect", memberKey, map[string]string{"hub_url": "https://new.example.com:7777"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("member: %d", r.StatusCode)
	}
	for _, bad := range []string{"http://new.example.com", "https://localhost:7777", "https://x/path", "nope"} {
		if r := adminPostTest(t, h, "/api/admin/redirect", res.AdminAPIKey, map[string]string{"hub_url": bad}); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad target %q: %d", bad, r.StatusCode)
		}
	}
	if _, err := admin.nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{})); err != nil {
		t.Fatalf("not moved yet: %v", err)
	}

	resp := adminPostTest(t, h, "/api/admin/redirect", res.AdminAPIKey, map[string]string{"hub_url": "https://new.example.com:7777/"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redirect: %d", resp.StatusCode)
	}
	var rr RedirectResult
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatal(err)
	}
	if rr.HubURL != "https://new.example.com:7777" || rr.BusURL != "tls://new.example.com:7778" || rr.Nodes != 2 {
		t.Fatalf("result %+v", rr)
	}
	if _, ok := isAdvertised(res.CAFingerprint); ok {
		t.Fatal("a moved hub still advertises")
	}

	// The connected node gets the signed Redirect.
	var got *mycelv1.Redirect
	eventually(t, 5*time.Second, "redirect ctl", func() bool {
		select {
		case req := <-sn.ctl:
			got = req.GetRedirect()
		default:
		}
		return got != nil
	})
	if err := daemon.VerifyRedirect(res.CACertPEM, got, time.Now()); err != nil || got.GetHubUrl() != rr.HubURL {
		t.Fatalf("ctl redirect %+v: %v", got, err)
	}

	// Every Connect call answers FailedPrecondition with the header.
	_, err := admin.nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	r := daemon.RedirectFromError(err)
	if r == nil || daemon.VerifyRedirect(res.CACertPEM, r, time.Now()) != nil || !strings.Contains(err.Error(), "hub moved to https://new.example.com:7777") {
		t.Fatalf("moved error %v, redirect %+v", err, r)
	}
	_, err = admin.enrol.Enrol(ctx, connect.NewRequest(&mycelv1.EnrolRequest{Token: "x"}))
	if daemon.RedirectFromError(err) == nil {
		t.Fatalf("enrol on a moved hub: %v", err)
	}
	// HTTP routes: 412 with the header; health and the CA still answer.
	hc := h.httpClient(nil)
	ar, err := hc.Get(h.ready.HubURL + "/api/artifacts/abc")
	if err != nil {
		t.Fatal(err)
	}
	if ar.StatusCode != http.StatusPreconditionFailed || ar.Header.Get(daemon.RedirectHeader) == "" {
		t.Fatalf("artifact route: %d %v", ar.StatusCode, ar.Header)
	}
	for _, p := range []string{"/healthz", "/ca.pem"} {
		if r, err := hc.Get(h.ready.HubURL + p); err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("%s in moved mode: %v %v", p, r, err)
		}
	}
	// Node RPCs over the bus are refused.
	if _, err := sn.rpc(t, "n1", "agent.list", &mycelv1.CtlRequest{}, &mycelv1.CtlResponse{}); err == nil || !strings.Contains(err.Error(), "failed_precondition: hub moved") {
		t.Fatalf("bus rpc in moved mode: %v", err)
	}

	cfg, err := ReadConfig(opts.HubDir)
	if err != nil || cfg.MovedTo != rr.HubURL {
		t.Fatalf("moved_to: %+v %v", cfg, err)
	}

	// Moved mode survives a restart; a reconnecting node is sent the
	// redirect first thing.
	stop()
	h2, _ := runTestHub(t, opts, res, false)
	_, err = h2.clients(res.AdminAPIKey, nil).nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	sn2 := h2.connectNode(t, e)
	b, _ := marshalBeat("n1")
	if err := sn2.nc.Publish(bus.NodeTelemetry("n1"), b); err != nil {
		t.Fatal(err)
	}
	got = nil
	eventually(t, 5*time.Second, "redirect after reconnect", func() bool {
		select {
		case req := <-sn2.ctl:
			if req.GetRedirect() == nil {
				t.Fatalf("moved hub sent %v", req)
			}
			got = req.GetRedirect()
		default:
		}
		return got != nil
	})
	if daemon.VerifyRedirect(res.CACertPEM, got, time.Now()) != nil || got.GetHubUrl() != rr.HubURL {
		t.Fatalf("redirect after restart: %+v", got)
	}

	// The redirect was audited.
	st, err := openStore(filepath.Join(opts.HubDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	entries, err := st.listAudit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range entries {
		found = found || (a.Action == "hub.redirect" && a.Outcome == outcomeOK)
	}
	if !found {
		t.Fatal("hub.redirect not audited")
	}
}

func marshalBeat(node string) ([]byte, error) {
	return proto.Marshal(&mycelv1.Heartbeat{NodeName: node, Inventory: &mycelv1.Inventory{Os: "linux"}})
}

func TestOfflineRedirect(t *testing.T) {
	opts, res := initTestHub(t)
	rr, err := Redirect(context.Background(), RedirectOptions{HubDir: opts.HubDir, HubURL: "https://moved.example.com:9000", BusURL: "moved.example.com:9001"})
	if err != nil {
		t.Fatal(err)
	}
	if rr.BusURL != "tls://moved.example.com:9001" || rr.Nodes != 1 {
		t.Fatalf("%+v", rr)
	}
	if _, err := Redirect(context.Background(), RedirectOptions{HubDir: opts.HubDir, HubURL: "https://localhost:7777"}); err == nil {
		t.Fatal("redirect to itself accepted")
	}
	h, _ := runTestHub(t, opts, res, false)
	_, err = h.clients(res.AdminAPIKey, nil).users.Whoami(context.Background(), connect.NewRequest(&mycelv1.WhoamiRequest{}))
	r := daemon.RedirectFromError(err)
	if r == nil || r.GetHubUrl() != "https://moved.example.com:9000" || r.GetBusUrl() != "tls://moved.example.com:9001" {
		t.Fatalf("offline redirect not served: %v", err)
	}
	if err := daemon.VerifyRedirect(res.CACertPEM, r, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestImportRefusesEvilArchive(t *testing.T) {
	for _, name := range []string{"../evil", "/abs", "acme/../../evil", "unknown.txt", "jetstream-snap/a/b"} {
		var tgz bytes.Buffer
		gz := gzip.NewWriter(&tgz)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		_ = gz.Close()
		pass := []byte("correct horse battery")
		file := sealBytes(t, pass, tgz.Bytes())
		root := t.TempDir()
		dir := filepath.Join(root, "hub")
		_, err := Import(context.Background(), ImportOptions{HubDir: dir, Source: bytes.NewReader(file), Passphrase: pass, AdvertiseURL: "https://new:7777", Logger: quietLogger()})
		if err == nil || !strings.Contains(err.Error(), "refusing entry") {
			t.Fatalf("%q: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(root, "evil")); err == nil {
			t.Fatalf("%q escaped", name)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Fatalf("%q: half-imported hub dir left behind", name)
		}
		if left, _ := filepath.Glob(filepath.Join(root, ".import-*")); len(left) > 0 {
			t.Fatalf("%q: temporary import directory left behind: %v", name, left)
		}
	}
}

// seedHub fills a running hub with a user, a channel and messages.
func seedHub(t *testing.T, h *testHub, key string) (userID string, msgs int) {
	t.Helper()
	ctx := context.Background()
	admin := h.clients(key, nil)
	userID, _ = createUser(t, admin, "carol", mycelv1.Role_ROLE_MEMBER)
	ch := mycelv1connect.NewChannelServiceClient(h.httpClient(nil), h.ready.HubURL, bearer(key))
	if _, err := ch.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "#move", Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC})); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := ch.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#move", Text: "message " + string(rune('a'+i))})); err != nil {
			t.Fatal(err)
		}
	}
	return userID, 5
}

func streamMsgs(t *testing.T, srv *server, name string) uint64 {
	t.Helper()
	s, err := srv.ch.js.Stream(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

func TestExportImportRoundTrip(t *testing.T) {
	for _, live := range []bool{true, false} {
		name := "stopped"
		if live {
			name = "running"
		}
		t.Run(name, func(t *testing.T) { exportImportRoundTrip(t, live) })
	}
}

func exportImportRoundTrip(t *testing.T, live bool) {
	ctx := context.Background()
	var srv *server
	setVar(t, &testHookServer, func(s *server) { srv = s })
	opts, res := initTestHub(t)
	h, stop := runTestHub(t, opts, res, false)
	admin := h.clients(res.AdminAPIKey, nil)
	e := enrol(t, admin, createToken(t, admin, "n1", nil, ""), "n1")
	userID, _ := seedHub(t, h, res.AdminAPIKey)
	chBefore := streamMsgs(t, srv, bus.StreamChannels)
	if chBefore == 0 {
		t.Fatal("no stream messages")
	}
	pass := []byte("a long export passphrase")
	var file bytes.Buffer
	if err := os.WriteFile(filepath.Join(opts.HubDir, tunnel.RelayURLFile), []byte("https://relay.example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eopts := ExportOptions{HubDir: opts.HubDir, Passphrase: pass, Logger: quietLogger()}
	if live {
		// The route is admin-only.
		if r := adminPostTest(t, h, "/api/admin/export", "", struct{}{}); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous export: %d", r.StatusCode)
		}
		resp := adminPostTest(t, h, "/api/admin/export", res.AdminAPIKey, struct{}{})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("export route: %d", resp.StatusCode)
		}
		defer func() { _ = resp.Body.Close() }()
		eopts.Live = resp.Body
	} else {
		stop()
	}
	if err := Export(ctx, eopts, &file); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if bytes.Contains(file.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("key material visible in the export")
	}
	stop()

	newDir := filepath.Join(t.TempDir(), "hub")
	if err := os.Mkdir(newDir, 0o700); err != nil { // an empty directory is accepted
		t.Fatal(err)
	}
	src := func() io.Reader { return bytes.NewReader(file.Bytes()) }
	if _, err := Import(ctx, ImportOptions{HubDir: newDir, Source: src(), Passphrase: []byte("not the passphrase"), AdvertiseURL: "https://127.0.0.1:7777"}); !errors.Is(err, ErrExportDecrypt) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Import(ctx, ImportOptions{HubDir: opts.HubDir, Source: src(), Passphrase: pass, AdvertiseURL: "https://127.0.0.1:7777"}); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("import over an existing hub: %v", err)
	}
	ir, err := Import(ctx, ImportOptions{HubDir: newDir, Source: src(), Passphrase: pass, AdvertiseURL: "https://127.0.0.1:7777", Logger: quietLogger()})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if ir.TrustDomain != res.TrustDomain || ir.CAFingerprint != res.CAFingerprint || ir.Nodes != 2 || ir.OldURL != opts.AdvertiseURL || !ir.RelayDropped {
		t.Fatalf("import result %+v", ir)
	}
	if _, err := os.Stat(filepath.Join(newDir, tunnel.RelayURLFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("relay.url restored")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(newDir), ".import-*")); len(left) > 0 {
		t.Fatalf("temporary import directory left: %v", left)
	}
	cfg, err := ReadConfig(newDir)
	if err != nil || cfg.AdvertiseURL != "https://127.0.0.1:7777" || cfg.BusAdvertise != "tls://127.0.0.1:7778" || cfg.MovedTo != "" {
		t.Fatalf("imported config %+v %v", cfg, err)
	}
	if _, err := os.Stat(filepath.Join(newDir, snapDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("snapshots left behind")
	}

	srv = nil
	nopts := opts
	nopts.HubDir = newDir
	h2, _ := runTestHub(t, nopts, res, false)
	c2 := h2.clients(res.AdminAPIKey, nil)
	users, err := c2.users.ListUsers(ctx, connect.NewRequest(&mycelv1.ListUsersRequest{}))
	if err != nil {
		t.Fatalf("same API key on the imported hub: %v", err)
	}
	if !containsUser(users.Msg.GetUsers(), userID) {
		t.Fatal("user missing after import")
	}
	nodes, err := c2.nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	if err != nil || len(nodes.Msg.GetNodes()) != 2 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	ch := mycelv1connect.NewChannelServiceClient(h2.httpClient(nil), h2.ready.HubURL, bearer(res.AdminAPIKey))
	lm, err := ch.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: "#move"}))
	if err != nil || len(lm.Msg.GetMessages()) != 5 {
		t.Fatalf("messages: %v %v", lm, err)
	}
	if got := streamMsgs(t, srv, bus.StreamChannels); got != chBefore {
		t.Fatalf("CH stream has %d messages, want %d", got, chBefore)
	}
	// The same node certificate connects to the imported hub's bus.
	h2.connectNode(t, e)
}

func containsUser(us []*mycelv1.User, id string) bool {
	for _, u := range us {
		if u.GetId() == id {
			return true
		}
	}
	return false
}

func TestExportPeerAllowed(t *testing.T) {
	addr := func(s string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(s), Port: 7777} }
	cases := []struct {
		remote string
		local  net.Addr
		ok     bool
	}{
		{"127.0.0.1:5555", addr("127.0.0.1"), true},
		{"[::1]:5555", addr("::1"), true},
		{"192.0.2.1:5555", addr("192.0.2.1"), true},   // non-loopback api_listen, from the machine itself
		{"192.0.2.10:5555", addr("192.0.2.1"), false}, // another machine
		{"127.0.0.1:5555", addr("192.0.2.1"), false},  // a tunnel or port forward
		{"192.0.2.1:5555", addr("127.0.0.1"), false},
		{"0.0.0.0:5555", addr("0.0.0.0"), false},
		{"garbage", addr("127.0.0.1"), false},
		{"127.0.0.1:5555", nil, false},
	}
	for _, c := range cases {
		if got := exportPeerAllowed(c.remote, c.local); got != c.ok {
			t.Errorf("%s -> %v: %v, want %v", c.remote, c.local, got, c.ok)
		}
	}
}

func TestExportRouteLoopbackOnly(t *testing.T) {
	s := &server{}
	for _, remote := range []string{"192.0.2.10:5555", "[2001:db8::1]:5555"} {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/export", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.handleAdminExport(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", remote, rec.Code)
		}
	}
	// Loopback peer on a non-loopback local address (e.g. a tunnel): refused.
	req := httptest.NewRequest(http.MethodPost, "/api/admin/export", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 7777}))
	rec := httptest.NewRecorder()
	s.handleAdminExport(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback local address: %d", rec.Code)
	}
}
