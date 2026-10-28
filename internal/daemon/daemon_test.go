package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// ---- Join ----

func joinNode(t *testing.T, h *fakeHub, name string, caps ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: h.newToken(name, caps...), Logger: discardLogger()}); err != nil {
		t.Fatalf("Join: %v", err)
	}
	return dir
}

func TestJoinSuccess(t *testing.T) {
	h := newFakeHub(t)
	dir := filepath.Join(t.TempDir(), "node")
	token := h.newToken("gpu-box", "docker", "gpu")
	var logBuf bytes.Buffer
	res, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: token, Logger: slog.New(slog.NewTextHandler(&logBuf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeName != "gpu-box" || res.SPIFFEID != "spiffe://test-hub/node/gpu-box" || res.OwnerID != "u-owner" ||
		res.HubURL != h.http.URL || res.BusURL != h.busServer().ClientURL() || len(res.Caps) != 2 || res.CertNotAfter == "" {
		t.Fatalf("result %+v", res)
	}
	for _, f := range []string{StateFile, KeyFile, CertFile, CAFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	st, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := State{Name: "gpu-box", SPIFFEID: "spiffe://test-hub/node/gpu-box", TrustDomain: "test-hub",
		HubURL: h.http.URL, BusURL: h.busServer().ClientURL(), OwnerID: "u-owner", Caps: []string{"docker", "gpu"}}
	if st.Name != want.Name || st.SPIFFEID != want.SPIFFEID || st.TrustDomain != want.TrustDomain ||
		st.HubURL != want.HubURL || st.BusURL != want.BusURL || st.OwnerID != want.OwnerID ||
		strings.Join(st.Caps, ",") != "docker,gpu" {
		t.Fatalf("state %+v", st)
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if !bytes.Equal(caPEM, h.ca.CertPEM()) {
		t.Fatal("ca.crt differs from hub CA")
	}
	if !strings.Contains(logBuf.String(), "joined hub as node gpu-box") {
		t.Fatalf("missing join log line: %q", logBuf.String())
	}
	if strings.Contains(logBuf.String(), token) || strings.Contains(logBuf.String(), "mcl1_") {
		t.Fatal("token appears in the log")
	}
}

func TestJoinWrongFingerprint(t *testing.T) {
	h := newFakeHub(t)
	other, err := identity.NewCA(testTD)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "node")
	token := h.newTokenWithFingerprint("n1", other.Fingerprint())
	_, err = Join(context.Background(), JoinOptions{NodeDir: dir, Token: token, Logger: discardLogger()})
	if err == nil || !strings.Contains(err.Error(), "hub CA fingerprint does not match the token") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("token in error")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("node dir written: %v", err)
	}
}

func TestJoinTwiceRefused(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	_, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: h.newToken("n2"), Logger: discardLogger()})
	if !errors.Is(err, ErrNodeExists) {
		t.Fatalf("got %v", err)
	}
	st, _ := ReadState(dir)
	if st.Name != "n1" {
		t.Fatalf("state overwritten: %+v", st)
	}
}

func TestJoinPermissionDenied(t *testing.T) {
	h := newFakeHub(t)
	h.mu.Lock()
	h.denyEnrol = true
	h.mu.Unlock()
	dir := filepath.Join(t.TempDir(), "node")
	token := h.newToken("n1")
	_, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: token, Logger: discardLogger()})
	if err == nil {
		t.Fatal("Join succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("token in error")
	}
	if HasState(dir) {
		t.Fatal("state written")
	}
	for _, f := range []string{KeyFile, CertFile, CAFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s written", f)
		}
	}
}

func TestJoinMalformedToken(t *testing.T) {
	_, err := Join(context.Background(), JoinOptions{NodeDir: t.TempDir(), Token: "mcl1_notatoken", Logger: discardLogger()})
	if err == nil || strings.Contains(err.Error(), "notatoken") {
		t.Fatalf("got %v", err)
	}
}

// ---- Run harness ----

type fakeProbe struct {
	inv atomic.Pointer[mycelv1.Inventory]
}

func newFakeProbe() *fakeProbe {
	p := &fakeProbe{}
	p.inv.Store(&mycelv1.Inventory{Os: "testos", Arch: "testarch", Hostname: "h1", CpuCount: 4})
	return p
}

func (p *fakeProbe) probe(context.Context) (*mycelv1.Inventory, error) {
	inv := proto.Clone(p.inv.Load()).(*mycelv1.Inventory)
	inv.ProbedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return inv, nil
}

type stateLog struct {
	mu     sync.Mutex
	states []string
}

func (s *stateLog) add(st string) {
	s.mu.Lock()
	s.states = append(s.states, st)
	s.mu.Unlock()
}

func (s *stateLog) has(st string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return containsString(s.states, st)
}

type running struct {
	addr   string
	done   chan error
	cancel context.CancelFunc
	states *stateLog
	probe  *fakeProbe
}

func testRunConfig(p *fakeProbe, sl *stateLog) runConfig {
	cfg := defaultRunConfig()
	cfg.heartbeatEvery = 100 * time.Millisecond
	cfg.probeEvery = 50 * time.Millisecond
	cfg.renewCheckEvery = time.Hour
	cfg.renewRetryEvery = time.Hour
	cfg.probe = p.probe
	cfg.onState = sl.add
	return cfg
}

func startRun(t *testing.T, dir string) *running {
	t.Helper()
	p := newFakeProbe()
	sl := &stateLog{}
	return startRunWith(t, dir, testRunConfig(p, sl), p, sl)
}

func startRunWith(t *testing.T, dir string, cfg runConfig, p *fakeProbe, sl *stateLog) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan Ready, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Options{NodeDir: dir, LocalAddr: "127.0.0.1:0", Logger: discardLogger()}, ready, cfg)
	}()
	r := &running{done: done, cancel: cancel, states: sl, probe: p}
	select {
	case rd := <-ready:
		r.addr = rd.LocalAddr
	case err := <-done:
		cancel()
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no Ready within 5s")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			_ = err // the test body checks the result where it matters
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return r
}

func (r *running) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // keep it for Cleanup
		return err
	case <-time.After(d):
		t.Fatalf("Run did not return within %v", d)
		return nil
	}
}

func getStatus(t *testing.T, addr string) Status {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func waitConnected(t *testing.T, addr string) {
	t.Helper()
	waitFor(t, 5*time.Second, "status connected", func() bool {
		s := getStatus(t, addr)
		return s.Connected && s.State == StateConnected
	})
}

func ctl(t *testing.T, hc *nats.Conn, name string, req *mycelv1.CtlRequest) *mycelv1.CtlResponse {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var resp *mycelv1.CtlResponse
	waitFor(t, 5*time.Second, "ctl reply", func() bool {
		m, err := hc.Request(bus.NodeCtl(name), b, time.Second)
		if err != nil {
			return false
		}
		resp = &mycelv1.CtlResponse{}
		if err := proto.Unmarshal(m.Data, resp); err != nil {
			t.Fatal(err)
		}
		return true
	})
	return resp
}

func nextBeat(t *testing.T, sub *nats.Subscription) *mycelv1.Heartbeat {
	t.Helper()
	m, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no heartbeat: %v", err)
	}
	hb := &mycelv1.Heartbeat{}
	if err := proto.Unmarshal(m.Data, hb); err != nil {
		t.Fatal(err)
	}
	return hb
}

// ---- Run ----

func TestRunConnectHeartbeatAndCtl(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1", "docker")
	hc := h.hubConn()
	sub, err := hc.SubscribeSync(bus.NodeTelemetry("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := hc.Flush(); err != nil {
		t.Fatal(err)
	}

	r := startRun(t, dir)
	waitConnected(t, r.addr)

	// A full beat arrives, then (without a change) beats without inventory.
	var full *mycelv1.Heartbeat
	for full == nil {
		hb := nextBeat(t, sub)
		if hb.GetInventory() != nil {
			full = hb
		}
	}
	if full.GetNodeName() != "n1" || full.GetInventory().GetHostname() != "h1" || full.GetInventoryHash() == "" || full.GetSentAt() == "" {
		t.Fatalf("full beat %v", full)
	}
	next := nextBeat(t, sub)
	if next.GetInventory() != nil {
		t.Fatal("unchanged inventory sent again")
	}
	if next.GetInventoryHash() != full.GetInventoryHash() {
		t.Fatal("hash changed without a change")
	}

	// A changed inventory is sent in full again.
	r.probe.inv.Store(&mycelv1.Inventory{Os: "testos", Arch: "testarch", Hostname: "h2", CpuCount: 8})
	waitFor(t, 5*time.Second, "changed inventory", func() bool {
		hb := nextBeat(t, sub)
		return hb.GetInventory().GetHostname() == "h2"
	})

	s := getStatus(t, r.addr)
	if s.Node != "n1" || s.HubURL != h.http.URL || s.LastHeartbeatAt == nil || s.CertNotAfter == "" ||
		len(s.Inventory) == 0 || string(s.Inventory) == "null" || strings.Join(s.Caps, ",") != "docker" {
		t.Fatalf("status %+v", s)
	}
	var inv map[string]any
	if err := json.Unmarshal(s.Inventory, &inv); err != nil {
		t.Fatal(err)
	}
	if _, ok := inv["cpu_count"]; !ok {
		t.Fatalf("inventory not in proto names: %s", s.Inventory)
	}

	// Ping.
	if resp := ctl(t, hc, "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}); !resp.GetOk() {
		t.Fatalf("ping: %v", resp)
	}

	// SetCaps.
	resp := ctl(t, hc, "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_SetCaps{SetCaps: &mycelv1.SetCaps{
		CapsEnabled: &mycelv1.Capabilities{Names: []string{"ollama", "gpu"}}}}})
	if !resp.GetOk() {
		t.Fatalf("set caps: %v", resp)
	}
	if got := strings.Join(getStatus(t, r.addr).Caps, ","); got != "ollama,gpu" {
		t.Fatalf("status caps %q", got)
	}
	st, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Caps, ",") != "ollama,gpu" || st.Name != "n1" || st.BusURL == "" {
		t.Fatalf("node.yaml %+v", st)
	}

	// HTML page.
	res, err := http.Get("http://" + r.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "node n1") {
		t.Fatalf("index: %d %s", res.StatusCode, body)
	}

	// Shutdown ctl stops Run with nil.
	if resp := ctl(t, hc, "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Shutdown{Shutdown: &mycelv1.Shutdown{Reason: "test"}}}); !resp.GetOk() {
		t.Fatalf("shutdown: %v", resp)
	}
	if err := r.wait(t, 5*time.Second); err != nil {
		t.Fatalf("Run after Shutdown: %v", err)
	}
	if !HasState(dir) {
		t.Fatal("shutdown removed the node state")
	}
}

func TestRunContextCancel(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)
	r.cancel()
	if err := r.wait(t, 5*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitFor(t, 5*time.Second, "node gone from bus", func() bool { return len(h.busServer().ConnectedNodes()) == 0 })
}

func TestRevokedViaCtl(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)

	resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Revoked{Revoked: &mycelv1.Revoked{Reason: "removed"}}})
	if !resp.GetOk() {
		t.Fatalf("revoked reply: %v", resp)
	}
	if err := r.wait(t, 5*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	if !r.states.has(StateRevoked) {
		t.Fatal("state never went to revoked")
	}
	assertForgotten(t, dir)
}

func assertForgotten(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{KeyFile, CertFile, StateFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still present: %v", f, err)
		}
	}
	m, _ := filepath.Glob(filepath.Join(dir, StateFile+".revoked-*"))
	if len(m) != 1 {
		t.Fatalf("revoked state file: %v", m)
	}
	if HasState(dir) {
		t.Fatal("HasState after revocation")
	}
}

func TestRevokedViaAuth(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)

	h.rev.revoke(identity.NodeSPIFFEID(testTD, "n1"))
	if n := h.busServer().Disconnect("n1"); n == 0 {
		t.Fatal("nothing disconnected")
	}
	if err := r.wait(t, 5*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	if !r.states.has(StateRevoked) {
		t.Fatal("state never went to revoked")
	}
	assertForgotten(t, dir)
}

func TestRenewal(t *testing.T) {
	h := newFakeHub(t)
	// The CA backdates NotBefore by a minute, so a 20 s certificate has less
	// than a third of its lifetime left right away.
	h.mu.Lock()
	h.nodeTTL = 20 * time.Second
	h.mu.Unlock()
	dir := joinNode(t, h, "n1")
	oldPEM, _ := os.ReadFile(filepath.Join(dir, CertFile))
	oldCert, _, err := identity.VerifyNodeCert(h.ca.Pool(), testTD, oldPEM)
	if err != nil {
		t.Fatal(err)
	}

	r := startRun(t, dir)
	var newPEM []byte
	waitFor(t, 10*time.Second, "node.crt replaced", func() bool {
		newPEM, _ = os.ReadFile(filepath.Join(dir, CertFile))
		return len(newPEM) > 0 && !bytes.Equal(newPEM, oldPEM)
	})
	newCert, _, err := identity.VerifyNodeCert(h.ca.Pool(), testTD, newPEM)
	if err != nil {
		t.Fatal(err)
	}
	if identity.SerialHex(newCert) == identity.SerialHex(oldCert) {
		t.Fatal("same serial")
	}
	waitFor(t, 10*time.Second, "bus connection with the new serial", func() bool {
		return h.rev.sawSerial(identity.SerialHex(newCert))
	})
	waitFor(t, 5*time.Second, "status shows the new certificate", func() bool {
		s := getStatus(t, r.addr)
		return s.Connected && s.CertNotAfter == newCert.NotAfter.UTC().Format(time.RFC3339)
	})
	// The ctl subscription survives the reconnect.
	if resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}); !resp.GetOk() {
		t.Fatalf("ping after renewal: %v", resp)
	}
	waitFor(t, 5*time.Second, "single bus connection", func() bool {
		return len(h.busServer().ConnectedNodes()) == 1
	})
	// Heartbeats keep flowing on the new connection.
	sub, err := h.hubConn().SubscribeSync(bus.NodeTelemetry("n1"))
	if err != nil {
		t.Fatal(err)
	}
	nextBeat(t, sub)
}

func TestRenewalRefusedAsRevoked(t *testing.T) {
	h := newFakeHub(t)
	h.mu.Lock()
	h.nodeTTL = 20 * time.Second
	h.renewRevoked = true
	h.mu.Unlock()
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	if err := r.wait(t, 5*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	assertForgotten(t, dir)
}

func TestHubRestart(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)

	port := h.busServer().Port()
	h.stopBus()
	waitFor(t, 5*time.Second, "status disconnected", func() bool {
		s := getStatus(t, r.addr)
		return s.State == StateDisconnected && !s.Connected
	})
	// Hold node authentication until the hub subscribed to telemetry, so the
	// first beat after the reconnect cannot be missed.
	release := h.rev.hold()
	defer release()
	h.startBus(port)
	sub, err := h.hubConn().SubscribeSync(bus.NodeTelemetry("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hubConn().Flush(); err != nil {
		t.Fatal(err)
	}
	release()
	if hb := nextBeat(t, sub); hb.GetInventory() == nil {
		t.Fatal("first beat after the hub restart has no inventory")
	}
	waitFor(t, 15*time.Second, "status connected again", func() bool {
		s := getStatus(t, r.addr)
		return s.State == StateConnected && s.Connected
	})
	if r.states.has(StateRevoked) {
		t.Fatal("restart treated as revocation")
	}
	if resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}); !resp.GetOk() {
		t.Fatalf("ping after restart: %v", resp)
	}
}

func TestRunRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:7777", "[::]:0", "example.com:7777", "nonsense"} {
		t.Run(addr, func(t *testing.T) {
			start := time.Now()
			err := Run(context.Background(), Options{NodeDir: t.TempDir(), LocalAddr: addr, Logger: discardLogger()}, nil)
			if err == nil || errors.Is(err, ErrNoNode) {
				t.Fatalf("got %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("not fast")
			}
		})
	}
}

func TestRunWithoutNode(t *testing.T) {
	err := Run(context.Background(), Options{NodeDir: t.TempDir(), LocalAddr: "127.0.0.1:0", Logger: discardLogger()}, nil)
	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("got %v", err)
	}
}

func TestLoopbackHelpers(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:7777", true}, {"127.0.0.1:0", true}, {"localhost:7777", true}, {"[::1]:7777", true},
		{"0.0.0.0:7777", false}, {":7777", false}, {"10.0.0.1:7777", false},
	} {
		if err := checkLoopback(tc.addr); (err == nil) != tc.ok {
			t.Errorf("checkLoopback(%q) = %v", tc.addr, err)
		}
	}
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:7777", true}, {"localhost", true}, {"LOCALHOST:1", true}, {"[::1]:7777", true},
		{"evil.example:7777", false}, {"192.168.1.2", false}, {"", false},
	} {
		if got := loopbackHost(tc.host); got != tc.ok {
			t.Errorf("loopbackHost(%q) = %v", tc.host, got)
		}
	}
}

func TestStatusRejectsForeignHost(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	req, _ := http.NewRequest(http.MethodGet, "http://"+r.addr+"/api/status", nil)
	req.Host = "rebind.example:7777"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStatusBeforeConnect(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	h.stopBus() // nothing to connect to
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	cfg.probe = func(ctx context.Context) (*mycelv1.Inventory, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := startRunWith(t, dir, cfg, p, sl)
	s := getStatus(t, r.addr)
	if s.State != StateConnecting || s.Connected || s.LastHeartbeatAt != nil || string(s.Inventory) != "null" {
		t.Fatalf("status %+v", s)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := writeFileAtomic(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "two" {
		t.Fatalf("got %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestHubDownAtStart(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	port := h.busServer().Port()
	h.stopBus()
	r := startRun(t, dir)
	if s := getStatus(t, r.addr); s.State != StateConnecting || s.Connected {
		t.Fatalf("status %+v", s)
	}
	h.startBus(port)
	waitFor(t, 15*time.Second, "status connected", func() bool {
		s := getStatus(t, r.addr)
		return s.Connected && s.State == StateConnected
	})
	if resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}); !resp.GetOk() {
		t.Fatalf("ping: %v", resp)
	}
}

func TestProbeFailureKeepsRunning(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	sub, err := h.hubConn().SubscribeSync(bus.NodeTelemetry("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hubConn().Flush(); err != nil {
		t.Fatal(err)
	}
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	cfg.probe = func(context.Context) (*mycelv1.Inventory, error) { return nil, errors.New("probe broken") }
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	for range 3 {
		if hb := nextBeat(t, sub); hb.GetInventory() != nil || hb.GetInventoryHash() != "" || hb.GetNodeName() != "n1" {
			t.Fatalf("beat %v", hb)
		}
	}
	s := getStatus(t, r.addr)
	if string(s.Inventory) != "null" || s.LastHeartbeatAt == nil {
		t.Fatalf("status %+v", s)
	}
	select {
	case err := <-r.done:
		t.Fatalf("Run stopped: %v", err)
	default:
	}
}

func TestRenewalRetriesWhenUnavailable(t *testing.T) {
	h := newFakeHub(t)
	h.mu.Lock()
	h.nodeTTL = 20 * time.Second
	h.renewFails = 3
	h.mu.Unlock()
	dir := joinNode(t, h, "n1")
	oldPEM, _ := os.ReadFile(filepath.Join(dir, CertFile))
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	cfg.renewRetryEvery = 50 * time.Millisecond
	r := startRunWith(t, dir, cfg, p, sl)
	waitFor(t, 10*time.Second, "node.crt replaced", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, CertFile))
		return len(b) > 0 && !bytes.Equal(b, oldPEM)
	})
	h.mu.Lock()
	calls := h.renewCalls
	h.mu.Unlock()
	if calls != 4 {
		t.Fatalf("renew calls %d, want 4", calls)
	}
	waitConnected(t, r.addr)
	select {
	case err := <-r.done:
		t.Fatalf("Run stopped: %v", err)
	default:
	}
}

func TestRevokedWhileOffline(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)
	port := h.busServer().Port()
	h.stopBus()
	waitFor(t, 5*time.Second, "status disconnected", func() bool {
		return getStatus(t, r.addr).State == StateDisconnected
	})
	h.rev.revoke(identity.NodeSPIFFEID(testTD, "n1"))
	h.startBus(port)
	if err := r.wait(t, 15*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	assertForgotten(t, dir)
}

func TestRevokedBeforeStart(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	h.rev.revoke(identity.NodeSPIFFEID(testTD, "n1"))
	r := startRun(t, dir)
	if err := r.wait(t, 5*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	if !r.states.has(StateRevoked) {
		t.Fatal("state never went to revoked")
	}
	assertForgotten(t, dir)
}

func TestExpiredCertificate(t *testing.T) {
	h := newFakeHub(t)
	h.mu.Lock()
	h.nodeTTL = time.Second
	h.mu.Unlock()
	dir := joinNode(t, h, "n1")
	certPEM, _ := os.ReadFile(filepath.Join(dir, CertFile))
	_, leaf, err := loadKeyPair(certPEM, mustRead(t, filepath.Join(dir, KeyFile)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "certificate expired", func() bool { return time.Now().After(leaf.NotAfter) })

	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	cfg.renewRetryEvery = 50 * time.Millisecond
	r := startRunWith(t, dir, cfg, p, sl)
	waitFor(t, 5*time.Second, "status expired", func() bool {
		s := getStatus(t, r.addr)
		return s.State == StateExpired && !s.Connected
	})
	select {
	case err := <-r.done:
		t.Fatalf("Run stopped: %v", err)
	default:
	}
	for _, f := range []string{StateFile, KeyFile, CertFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHubURLMustBeHTTPS(t *testing.T) {
	h := newFakeHub(t)
	dir := filepath.Join(t.TempDir(), "node")
	for _, u := range []string{"http://127.0.0.1:1", "https://", "ftp://x", "https:///path"} {
		_, err := Join(context.Background(), JoinOptions{NodeDir: dir, Token: h.newToken("n1"), HubURL: u, Logger: discardLogger()})
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("%q: got %v", u, err)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("node dir written")
	}

	// A node.yaml with a plain-HTTP hub URL is refused by Run.
	dir = joinNode(t, h, "n2")
	st, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.HubURL = "http://127.0.0.1:1"
	if err := writeStateFile(dir, *st); err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Options{NodeDir: dir, LocalAddr: "127.0.0.1:0", Logger: discardLogger()}, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("Run: %v", err)
	}
}

func TestSetCapsRefusedAfterRevocation(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	n, err := loadNode(dir, discardLogger(), testRunConfig(newFakeProbe(), &stateLog{}))
	if err != nil {
		t.Fatal(err)
	}
	n.setState(StateRevoked)
	if err := n.setCaps([]string{"gpu"}); err == nil {
		t.Fatal("setCaps accepted after revocation")
	}
	n.forgetIdentity()
	if HasState(dir) {
		t.Fatal("node.yaml recreated")
	}
}
