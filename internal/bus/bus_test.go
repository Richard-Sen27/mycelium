package bus

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/richard-sen27/mycelium/internal/identity"
)

const testTD = "test.local"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeRevocation revokes by SPIFFE ID or by serial.
type fakeRevocation struct {
	mu      sync.Mutex
	revoked map[string]bool
	serials map[string]bool
}

func (f *fakeRevocation) IsRevoked(serialHex, spiffeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked[spiffeID] || f.serials[serialHex]
}

func (f *fakeRevocation) revokeSerial(serialHex string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serials == nil {
		f.serials = map[string]bool{}
	}
	f.serials[serialHex] = true
}

func (f *fakeRevocation) revoke(spiffeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked == nil {
		f.revoked = map[string]bool{}
	}
	f.revoked[spiffeID] = true
}

type serverOpts struct {
	port     int
	storeDir string
	rev      Revocation
	logger   *slog.Logger
}

func startTestServer(t *testing.T, ca *testCA, o serverOpts) *Server {
	t.Helper()
	if o.storeDir == "" {
		o.storeDir = t.TempDir()
	}
	if o.logger == nil {
		o.logger = discardLogger()
	}
	s, err := StartServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        o.port,
		TLSCert:     ca.serverCert(t),
		ClientCAs:   ca.pool,
		TrustDomain: identity.TrustDomain(testTD),
		StoreDir:    o.storeDir,
		Revocation:  o.rev,
		Logger:      o.logger,
	})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	t.Cleanup(func() { shutdown(t, s) })
	return s
}

func shutdown(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func hubConn(t *testing.T, s *Server) *nats.Conn {
	t.Helper()
	hc, err := s.HubConn()
	if err != nil {
		t.Fatalf("HubConn: %v", err)
	}
	return hc
}

// connectNode connects a node with a fresh certificate for name.
func connectNode(t *testing.T, s *Server, ca *testCA, name string, cfg ClientConfig) *nats.Conn {
	t.Helper()
	cert, _ := ca.nodeCert(t, testTD, name)
	cfg.URL = s.ClientURL()
	cfg.NodeName = name
	cfg.Cert = cert
	cfg.RootCAs = ca.pool
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect(%s): %v", name, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func recvMsg(t *testing.T, sub *nats.Subscription) *nats.Msg {
	t.Helper()
	m, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("NextMsg on %s: %v", sub.Subject, err)
	}
	return m
}

func flush(t *testing.T, ncs ...*nats.Conn) {
	t.Helper()
	for _, nc := range ncs {
		if err := nc.FlushTimeout(5 * time.Second); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
}

func TestHubConnRoundTrip(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})

	hc := hubConn(t, s)
	hc2 := hubConn(t, s)
	if hc != hc2 {
		t.Fatal("HubConn returned different connections")
	}

	sub, err := hc.SubscribeSync("anything.at.all")
	if err != nil {
		t.Fatal(err)
	}
	if err := hc.Publish("anything.at.all", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if m := recvMsg(t, sub); string(m.Data) != "hi" {
		t.Fatalf("got %q", m.Data)
	}

	// JetStream is enabled for the hub's account (used from phase 2).
	js, err := hc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AccountInfo(); err != nil {
		t.Fatalf("JetStream AccountInfo: %v", err)
	}

	if got := s.ConnectedNodes(); len(got) != 0 {
		t.Fatalf("ConnectedNodes with only the hub = %v, want none", got)
	}
}

func TestClientURL(t *testing.T) {
	ca := newTestCA(t, testTD)
	s, err := StartServer(ServerConfig{
		Host:        "0.0.0.0",
		TLSCert:     ca.serverCert(t),
		ClientCAs:   ca.pool,
		TrustDomain: testTD,
		StoreDir:    t.TempDir(),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, s)
	if s.Port() == 0 {
		t.Fatal("Port() = 0")
	}
	want := fmt.Sprintf("tls://127.0.0.1:%d", s.Port())
	if got := s.ClientURL(); got != want {
		t.Fatalf("ClientURL = %q, want %q", got, want)
	}
}

func TestNodeTelemetryAndCtl(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	hc := hubConn(t, s)

	telem, err := hc.SubscribeSync(AllTelemetry)
	if err != nil {
		t.Fatal(err)
	}
	flush(t, hc)

	nc := connectNode(t, s, ca, "alpha", ClientConfig{})

	// Node → hub telemetry.
	if err := nc.Publish(NodeTelemetry("alpha"), []byte(`{"heartbeat":1}`)); err != nil {
		t.Fatal(err)
	}
	m := recvMsg(t, telem)
	if node, ok := NodeFromSubject(m.Subject); !ok || node != "alpha" || string(m.Data) != `{"heartbeat":1}` {
		t.Fatalf("telemetry: subject %q data %q", m.Subject, m.Data)
	}

	// Hub → node request, node replies (response permission).
	if _, err := nc.Subscribe(NodeCtl("alpha"), func(m *nats.Msg) {
		_ = m.Respond(append([]byte("ack:"), m.Data...))
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, nc)
	reply, err := hc.Request(NodeCtl("alpha"), []byte("ping"), 5*time.Second)
	if err != nil {
		t.Fatalf("ctl request: %v", err)
	}
	if string(reply.Data) != "ack:ping" {
		t.Fatalf("ctl reply %q", reply.Data)
	}

	// Node → hub RPC: the reply comes back on the node's own inbox prefix.
	if _, err := hc.Subscribe(AllRPC, func(m *nats.Msg) {
		method, _ := RPCMethodFromSubject(m.Subject)
		_ = m.Respond([]byte("ok:" + method))
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, hc)
	reply, err = nc.Request(NodeRPC("alpha", RPCRegisterAgent), []byte("x"), 5*time.Second)
	if err != nil {
		t.Fatalf("rpc request: %v", err)
	}
	if string(reply.Data) != "ok:"+RPCRegisterAgent {
		t.Fatalf("rpc reply %q", reply.Data)
	}
}

func TestNodePermissions(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	hc := hubConn(t, s)
	hubSeen, err := hc.SubscribeSync(">")
	if err != nil {
		t.Fatal(err)
	}
	flush(t, hc)

	nc := connectNode(t, s, ca, "alpha", ClientConfig{})
	errs := make(chan error, 16)
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err })

	expectViolation := func(t *testing.T, kind, subject string) {
		t.Helper()
		select {
		case err := <-errs:
			if !errors.Is(err, nats.ErrPermissionViolation) {
				t.Fatalf("error %v is not a permissions violation", err)
			}
			if !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), subject) {
				t.Fatalf("violation %q does not mention %s %q", err, kind, subject)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no permissions violation for %s %q", kind, subject)
		}
	}

	publishDenied := []string{
		NodeTelemetry("beta"),
		NodeCtl("beta"),
		NodeCtl("alpha"), // own ctl: subscribable, so never publishable
		"node.alpha.other",
		NodeRPC("beta", RPCRegisterAgent),
		InboxPrefix("alpha") + ".x",
		"ch.general.msg",
		"audit.x",
		"_INBOX._hub.abc",
		"$JS.API.INFO",
	}
	for _, subj := range publishDenied {
		t.Run("publish "+subj, func(t *testing.T) {
			if err := nc.Publish(subj, []byte("forbidden")); err != nil {
				t.Fatal(err)
			}
			expectViolation(t, "Publish", subj)
			// A permitted message sent afterwards arrives; the forbidden one,
			// sent before it on the same connection, must not have.
			marker := []byte("marker " + subj)
			if err := nc.Publish(NodeTelemetry("alpha"), marker); err != nil {
				t.Fatal(err)
			}
			for {
				m := recvMsg(t, hubSeen)
				if m.Subject == subj {
					t.Fatalf("hub received forbidden publish on %q", subj)
				}
				if bytes.Equal(m.Data, marker) {
					break
				}
			}
		})
	}

	subscribeDenied := []string{
		"node.alpha.>",
		NodeTelemetry("alpha"), // own telemetry: publishable, so never subscribable
		"node.alpha.rpc.>",
		"node.beta.>",
		NodeTelemetry("beta"),
		">",
		"node.*.telemetry",
		"_INBOX.>",
		"_INBOX.beta.>",
	}
	for _, subj := range subscribeDenied {
		t.Run("subscribe "+subj, func(t *testing.T) {
			sub, err := nc.SubscribeSync(subj)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			expectViolation(t, "Subscription", subj)
			// Nothing published by the hub on beta's subjects reaches alpha.
			if err := hc.Publish(NodeTelemetry("beta"), []byte("secret")); err != nil {
				t.Fatal(err)
			}
			flush(t, hc, nc)
			if m, err := sub.NextMsg(200 * time.Millisecond); err == nil {
				t.Fatalf("node received %q on %q", m.Data, m.Subject)
			}
		})
	}
}

func TestRefusedConnections(t *testing.T) {
	ca := newTestCA(t, testTD)
	otherCA := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})

	otherCACert, _ := otherCA.nodeCert(t, testTD, "alpha")
	noSAN, _ := ca.clientCert(t, "alpha")
	wrongTD, _ := ca.nodeCert(t, "other.domain", "alpha")
	agentID, _ := ca.clientCert(t, "alpha", "spiffe://"+testTD+"/agent/alpha")
	badName, _ := ca.clientCert(t, "x", "spiffe://"+testTD+"/node/Bad.Name")
	wildcard, _ := ca.clientCert(t, "x", "spiffe://"+testTD+"/node/*")
	twoSANs, _ := ca.clientCert(t, "alpha", "spiffe://"+testTD+"/node/alpha", "spiffe://"+testTD+"/node/beta")
	httpsURI, _ := ca.clientCert(t, "alpha", "https://"+testTD+"/node/alpha")

	tests := []struct {
		name     string
		cert     *tls.Certificate
		wantAuth bool // refused by Check (authorization error) rather than TLS
	}{
		{"cert from other CA", &otherCACert, false},
		{"no client cert", nil, false},
		{"no SPIFFE SAN", &noSAN, true},
		{"wrong trust domain", &wrongTD, true},
		{"agent SPIFFE ID", &agentID, true},
		{"invalid node name", &badName, true},
		{"wildcard node name", &wildcard, true},
		{"two URI SANs", &twoSANs, true},
		{"non-spiffe URI", &httpsURI, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg := &tls.Config{RootCAs: ca.pool, MinVersion: tls.VersionTLS13}
			if tc.cert != nil {
				tlsCfg.Certificates = []tls.Certificate{*tc.cert}
			}
			nc, err := nats.Connect(s.ClientURL(), nats.Secure(tlsCfg), nats.NoReconnect())
			if err == nil {
				nc.Close()
				t.Fatal("connection succeeded")
			}
			if got := isAuthError(err); got != tc.wantAuth {
				t.Fatalf("isAuthError(%v) = %v, want %v", err, got, tc.wantAuth)
			}
		})
	}
	if got := s.ConnectedNodes(); len(got) != 0 {
		t.Fatalf("ConnectedNodes = %v, want none", got)
	}
}

func TestPlainConnectionRefused(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	addr := strings.TrimPrefix(s.ClientURL(), "tls://")

	t.Run("nats client without TLS config", func(t *testing.T) {
		nc, err := nats.Connect("nats://"+addr, nats.NoReconnect(), nats.Token(s.token))
		if err == nil {
			nc.Close()
			t.Fatal("connection succeeded")
		}
	})

	// Even the hub's secret token does not work over TCP: the listener
	// requires a TLS handshake before any protocol is parsed.
	t.Run("raw TCP with hub token", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		info, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read INFO: %v", err)
		}
		if !strings.HasPrefix(info, "INFO ") || !strings.Contains(info, `"tls_required":true`) {
			t.Fatalf("unexpected INFO %q", info)
		}
		connect := fmt.Sprintf("CONNECT {\"verbose\":false,\"pedantic\":false,\"auth_token\":%q}\r\nPING\r\n", s.token)
		if _, err := conn.Write([]byte(connect)); err != nil {
			return // server already closed the connection
		}
		rest, _ := io.ReadAll(r)
		if strings.Contains(string(rest), "PONG") {
			t.Fatalf("plain connection was accepted: %q", rest)
		}
	})
}

func TestRevocation(t *testing.T) {
	ca := newTestCA(t, testTD)
	rev := &fakeRevocation{}
	logs := &syncBuffer{}
	s := startTestServer(t, ca, serverOpts{rev: rev, logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	hubConn(t, s)

	cert, _ := ca.nodeCert(t, testTD, "alpha")
	var revoked, reconnected atomic.Int32
	cfg := ClientConfig{
		URL:           s.ClientURL(),
		NodeName:      "alpha",
		Cert:          cert,
		RootCAs:       ca.pool,
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
		OnAuthRevoked: func() { revoked.Add(1) },
		OnReconnect:   func() { reconnected.Add(1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	waitFor(t, 5*time.Second, "alpha listed", func() bool {
		nodes := s.ConnectedNodes()
		return len(nodes) == 1 && nodes[0] == "alpha"
	})

	rev.revoke("spiffe://" + testTD + "/node/alpha")
	if n := s.Disconnect("alpha"); n != 1 {
		t.Fatalf("Disconnect = %d, want 1", n)
	}
	waitFor(t, 5*time.Second, "OnAuthRevoked", func() bool { return revoked.Load() == 1 })
	waitFor(t, 5*time.Second, "node connection closed", nc.IsClosed)
	if got := s.ConnectedNodes(); len(got) != 0 {
		t.Fatalf("ConnectedNodes after revoke = %v", got)
	}
	if reconnected.Load() != 0 {
		t.Fatal("revoked node reconnected")
	}
	if n := s.Disconnect("alpha"); n != 0 {
		t.Fatalf("second Disconnect = %d, want 0", n)
	}

	// A fresh connect with the revoked cert is refused.
	revoked.Store(0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	nc2, err := Connect(ctx2, cfg)
	if err == nil {
		nc2.Close()
		t.Fatal("revoked cert connected")
	}
	if !isAuthError(err) {
		t.Fatalf("fresh connect error %v is not an authorization error", err)
	}
	if revoked.Load() != 1 {
		t.Fatalf("OnAuthRevoked calls on fresh connect = %d, want 1", revoked.Load())
	}

	if strings.Contains(logs.String(), s.token) {
		t.Fatal("hub token appears in logs")
	}
}

func TestConnectedNodesTwo(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	hubConn(t, s)
	connectNode(t, s, ca, "alpha", ClientConfig{})
	connectNode(t, s, ca, "beta", ClientConfig{})
	waitFor(t, 5*time.Second, "two nodes listed", func() bool {
		nodes := s.ConnectedNodes()
		return len(nodes) == 2 && nodes[0] == "alpha" && nodes[1] == "beta"
	})
	if n := s.Disconnect("gamma"); n != 0 {
		t.Fatalf("Disconnect(unknown) = %d", n)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestServerRestart(t *testing.T) {
	ca := newTestCA(t, testTD)
	storeDir := t.TempDir()

	s1, err := StartServer(ServerConfig{
		Host: "127.0.0.1", Port: 0, TLSCert: ca.serverCert(t), ClientCAs: ca.pool,
		TrustDomain: testTD, StoreDir: storeDir, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var disconnected, reconnected, revoked atomic.Int32
	nc := connectNode(t, s1, ca, "alpha", ClientConfig{
		OnDisconnect:  func(error) { disconnected.Add(1) },
		OnReconnect:   func() { reconnected.Add(1) },
		OnAuthRevoked: func() { revoked.Add(1) },
	})

	port := s1.Port()
	shutdown(t, s1)
	waitFor(t, 5*time.Second, "OnDisconnect", func() bool { return disconnected.Load() >= 1 })

	s2 := startTestServer(t, ca, serverOpts{port: port, storeDir: storeDir})
	hc := hubConn(t, s2)
	telem, err := hc.SubscribeSync(NodeTelemetry("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	flush(t, hc)

	waitFor(t, 15*time.Second, "OnReconnect", func() bool { return reconnected.Load() >= 1 })
	if err := nc.Publish(NodeTelemetry("alpha"), []byte("back")); err != nil {
		t.Fatal(err)
	}
	if m := recvMsg(t, telem); string(m.Data) != "back" {
		t.Fatalf("got %q", m.Data)
	}
	if revoked.Load() != 0 {
		t.Fatal("OnAuthRevoked fired on restart")
	}
	waitFor(t, 5*time.Second, "alpha listed on new server", func() bool {
		nodes := s2.ConnectedNodes()
		return len(nodes) == 1 && nodes[0] == "alpha"
	})
}

func TestConnectRetriesUntilContextDone(t *testing.T) {
	ca := newTestCA(t, testTD)
	cert, _ := ca.nodeCert(t, testTD, "alpha")
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	_, err := Connect(ctx, ClientConfig{
		URL: fmt.Sprintf("tls://127.0.0.1:%d", freePort(t)), NodeName: "alpha",
		Cert: cert, RootCAs: ca.pool, Logger: discardLogger(),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
}

func TestParseNodeSPIFFEID(t *testing.T) {
	td := identity.TrustDomain(testTD)
	tests := []struct {
		id   string
		want string
		ok   bool
	}{
		{"spiffe://test.local/node/alpha", "alpha", true},
		{"spiffe://test.local/node/gpu-box-2", "gpu-box-2", true},
		{"spiffe://other/node/alpha", "", false},
		{"spiffe://test.local/agent/alpha", "", false},
		{"spiffe://test.local/node/", "", false},
		{"spiffe://test.local/node/a/b", "", false},
		{"spiffe://test.local/node/a.b", "", false},
		{"spiffe://test.local/node/-a", "", false},
		{"spiffe://test.local/node/a-", "", false},
		{"spiffe://test.local/node/Alpha", "", false},
		{"spiffe://test.local/node/a?x=1", "", false},
		{"spiffe://test.local/node/a#f", "", false},
		{"spiffe://test.local/node/" + strings.Repeat("a", 64), "", false},
		{"https://test.local/node/alpha", "", false},
		{"spiffe://test.local.evil/node/alpha", "", false},
		{"hub", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, err := parseNodeSPIFFEID(td, tc.id)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseNodeSPIFFEID(%q) = %q, %v; want %q, ok=%v", tc.id, got, err, tc.want, tc.ok)
		}
	}
	if _, err := parseNodeSPIFFEID("", "spiffe:///node/alpha"); err == nil {
		t.Error("empty trust domain accepted")
	}
}
