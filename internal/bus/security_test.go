package bus

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// TestReplyEchoEscalation runs the echo-and-reply attack: a node publishes a
// message carrying a forged reply subject, hoping to receive its own echo and
// gain NATS' one-shot response permission for that reply subject. Since no
// subject is both publishable and subscribable by a node, the echo never
// arrives and the follow-up publish is refused.
func TestReplyEchoEscalation(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	hc := hubConn(t, s)
	telem, err := hc.SubscribeSync(NodeTelemetry("alpha"))
	if err != nil {
		t.Fatal(err)
	}

	nc := connectNode(t, s, ca, "alpha", ClientConfig{})
	errs := make(chan error, 64)
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err })

	// The attacker listens on everything it is allowed to (and some it is not).
	echo := make(chan *nats.Msg, 64)
	for _, subj := range []string{NodeCtl("alpha"), InboxPrefix("alpha") + ".>", "node.alpha.>", NodeTelemetry("alpha"), "node.alpha.rpc.>"} {
		if _, err := nc.ChanSubscribe(subj, echo); err != nil {
			t.Fatal(err)
		}
	}
	flush(t, nc)

	targets := []string{NodeCtl("beta"), "$JS.API.INFO", "_INBOX._hub.x"}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			hubTarget, err := hc.SubscribeSync(target)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hubTarget.Unsubscribe() }()
			flush(t, hc)

			// Bait: own publishable subjects with the forged reply.
			for _, subj := range []string{NodeTelemetry("alpha"), NodeRPC("alpha", RPCRegisterAgent)} {
				if err := nc.PublishMsg(&nats.Msg{Subject: subj, Reply: target, Data: []byte("bait")}); err != nil {
					t.Fatal(err)
				}
			}
			flush(t, nc)
			// Exploit: answer the echo, if any, and publish straight to the target.
			for {
				select {
				case m := <-echo:
					if m.Reply != "" {
						_ = m.Respond([]byte("pwned via echo"))
					}
					continue
				default:
				}
				break
			}
			if err := nc.Publish(target, []byte("pwned")); err != nil {
				t.Fatal(err)
			}
			marker := []byte("marker " + target)
			if err := nc.Publish(NodeTelemetry("alpha"), marker); err != nil {
				t.Fatal(err)
			}
			for {
				if m := recvMsg(t, telem); string(m.Data) == string(marker) {
					break
				}
			}
			// The marker was sent after the exploit attempts on the same
			// connection; anything that got through would already be queued.
			if m, err := hubTarget.NextMsg(100 * time.Millisecond); err == nil {
				t.Fatalf("hub received %q on %q", m.Data, target)
			}
			select {
			case m := <-echo:
				t.Fatalf("node received its own echo on %q", m.Subject)
			default:
			}
		})
	}
	// The direct publishes were reported as violations.
	var violations int
	for {
		select {
		case err := <-errs:
			if errors.Is(err, nats.ErrPermissionViolation) {
				violations++
			}
			continue
		default:
		}
		break
	}
	if violations == 0 {
		t.Fatal("no permissions violations reported")
	}
}

func TestValidReply(t *testing.T) {
	tests := []struct {
		node, reply string
		want        bool
	}{
		{"alpha", "_INBOX.alpha.abc", true},
		{"alpha", "_INBOX.alpha.abc.def", true},
		{"alpha", "_INBOX.alpha", false},
		{"alpha", "_INBOX.alpha.", false},
		{"alpha", "_INBOX.alpha..x", false},
		{"alpha", "_INBOX.alpha.x.", false},
		{"alpha", "_INBOX.alpha.*", false},
		{"alpha", "_INBOX.alpha.>", false},
		{"alpha", "_INBOX.alpha.a*b", false},
		{"alpha", "_INBOX.alpha.a b", false},
		{"alpha", "_INBOX.alphabet.x", false},
		{"alpha", "_INBOX.beta.x", false},
		{"alpha", "_INBOX._hub.x", false},
		{"alpha", "node.beta.ctl", false},
		{"alpha", "$JS.API.INFO", false},
		{"alpha", "", false},
		{"", "_INBOX..x", false},
	}
	for _, tc := range tests {
		if got := ValidReply(tc.node, tc.reply); got != tc.want {
			t.Errorf("ValidReply(%q, %q) = %v, want %v", tc.node, tc.reply, got, tc.want)
		}
	}
}

// TestCredentialFieldsRefused: a node with a valid cert that also sends other
// credentials in CONNECT is refused, so it cannot hide its identity from Connz.
func TestCredentialFieldsRefused(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	cert, _ := ca.nodeCert(t, testTD, "alpha")
	sigCB := func([]byte) ([]byte, error) { return []byte("sig"), nil }

	tests := []struct {
		name string
		opt  nats.Option
	}{
		{"user and password", nats.UserInfo("someone", "secret")},
		{"user only", nats.UserInfo("someone", "")},
		{"token on TLS", nats.Token("whatever")},
		{"hub token on TLS", nats.Token(s.token)},
		{"jwt and sig", nats.UserJWT(func() (string, error) { return "eyJhbGciOiJlZDI1NTE5In0.e30.c2ln", nil }, sigCB)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: ca.pool, MinVersion: tls.VersionTLS13}
			nc, err := nats.Connect(s.ClientURL(), nats.Secure(tlsCfg), nats.NoReconnect(), tc.opt)
			if err == nil {
				nc.Close()
				t.Fatal("connection succeeded")
			}
			if !isAuthError(err) {
				t.Fatalf("error %v is not an authorization error", err)
			}
		})
	}

	// nats.go will not send an nkey without a server nonce, so these CONNECT
	// fields go over a hand-made TLS connection.
	rawFields := []struct{ name, fields string }{
		{"raw nkey and sig", `"nkey":"UDXU4RCSJNZOIQHZNWXHXORDPRTGNJAHAHFRGZNEEJCPQTT2M7NLCNF4","sig":"c2ln"`},
		{"raw sig only", `"sig":"c2ln"`},
		{"raw jwt and sig", `"jwt":"eyJhbGciOiJlZDI1NTE5In0.e30.c2ln","sig":"c2ln"`},
		{"raw pass only", `"pass":"x"`},
		{"raw token", `"auth_token":"x"`},
	}
	for _, tc := range rawFields {
		t.Run(tc.name, func(t *testing.T) {
			conn, r := rawTLSConn(t, s, ca, cert)
			if _, err := conn.Write([]byte("CONNECT {\"verbose\":false,\"pedantic\":false," + tc.fields + "}\r\nPING\r\n")); err != nil {
				return // already refused
			}
			resp, _ := r.ReadString('\n')
			if strings.HasPrefix(resp, "PONG") {
				t.Fatal("CONNECT accepted")
			}
			if !strings.Contains(strings.ToLower(resp), "authorization violation") {
				t.Fatalf("response %q is not an authorization violation", resp)
			}
		})
	}

	// Outside operator mode nats-server discards a CONNECT jwt before Check
	// runs (Check refuses one regardless). Either way the connection must
	// stay visible under its SPIFFE ID.
	t.Run("raw jwt alone", func(t *testing.T) {
		conn, r := rawTLSConn(t, s, ca, cert)
		if _, err := conn.Write([]byte("CONNECT {\"verbose\":false,\"pedantic\":false,\"jwt\":\"eyJhbGciOiJlZDI1NTE5In0.e30.c2ln\"}\r\nPING\r\n")); err != nil {
			return
		}
		resp, _ := r.ReadString('\n')
		if !strings.HasPrefix(resp, "PONG") {
			return // refused: fine
		}
		if got := s.ConnectedNodes(); len(got) != 1 || got[0] != "alpha" {
			t.Fatalf("accepted connection hidden from ConnectedNodes: %v", got)
		}
		_ = conn.Close()
		waitFor(t, 5*time.Second, "connection gone", func() bool { return len(s.ConnectedNodes()) == 0 })
	})

	// A second CONNECT on a live connection re-runs Check and is refused too.
	t.Run("second CONNECT with user", func(t *testing.T) {
		conn, r := rawTLSConn(t, s, ca, cert)
		send := func(line string) string {
			t.Helper()
			if _, err := conn.Write([]byte(line)); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp, err := r.ReadString('\n')
			if err != nil {
				return "EOF: " + err.Error()
			}
			return resp
		}
		if resp := send("CONNECT {\"verbose\":false,\"pedantic\":false}\r\nPING\r\n"); !strings.HasPrefix(resp, "PONG") {
			t.Fatalf("first CONNECT: %q", resp)
		}
		waitFor(t, 5*time.Second, "alpha listed", func() bool { return len(s.ConnectedNodes()) == 1 })
		resp := send("CONNECT {\"verbose\":false,\"pedantic\":false,\"user\":\"hidden\",\"pass\":\"x\"}\r\nPING\r\n")
		if strings.HasPrefix(resp, "PONG") {
			t.Fatal("second CONNECT with user accepted")
		}
		waitFor(t, 5*time.Second, "connection dropped", func() bool { return len(s.ConnectedNodes()) == 0 })
		for _, user := range connzUsers(t, s) {
			if user == "hidden" {
				t.Fatal("a connection is listed under a client-chosen username")
			}
		}
	})
}

// rawTLSConn speaks the NATS protocol by hand: plaintext INFO, then TLS.
func rawTLSConn(t *testing.T, s *Server, ca *testCA, cert tls.Certificate) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	addr := strings.TrimPrefix(s.ClientURL(), "tls://")
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	info, err := bufio.NewReader(raw).ReadString('\n')
	if err != nil || !strings.HasPrefix(info, "INFO ") {
		t.Fatalf("read INFO: %q %v", info, err)
	}
	conn := tls.Client(raw, &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca.pool,
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS13,
	})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

func connzUsers(t *testing.T, s *Server) []string {
	t.Helper()
	cz, err := s.ns.Connz(&server.ConnzOptions{Username: true})
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, ci := range cz.Conns {
		users = append(users, ci.AuthorizedUser)
	}
	return users
}

// TestServerOptionsPinned pins the security-relevant server options.
func TestServerOptionsPinned(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	o := s.opts
	if o.AllowNonTLS {
		t.Error("AllowNonTLS must stay false")
	}
	if o.TLSConfig == nil || o.TLSConfig.ClientAuth != tls.RequireAndVerifyClientCert || !o.TLSVerify {
		t.Error("client certificates must be required and verified")
	}
	if o.ServerName != "hub" {
		t.Errorf("ServerName = %q", o.ServerName)
	}
	checks := []struct {
		name      string
		got, want int64
	}{
		{"MaxConn", int64(o.MaxConn), 4096},
		{"MaxSubs", int64(o.MaxSubs), 512},
		{"MaxPending", o.MaxPending, 8 << 20},
		{"MaxPayload", int64(o.MaxPayload), 1 << 20},
		{"JetStreamMaxMemory", o.JetStreamMaxMemory, DefaultJetStreamMaxMemory},
		{"JetStreamMaxStore", o.JetStreamMaxStore, DefaultJetStreamMaxStore},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	s2, err := StartServer(ServerConfig{
		Host: "127.0.0.1", TLSCert: ca.serverCert(t), ClientCAs: ca.pool, TrustDomain: testTD,
		StoreDir: t.TempDir(), Logger: discardLogger(),
		JetStreamMaxMemory: 64 << 20, JetStreamMaxStore: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, s2)
	if s2.opts.JetStreamMaxMemory != 64<<20 || s2.opts.JetStreamMaxStore != 1<<30 {
		t.Errorf("JetStream limit overrides not applied: %d, %d", s2.opts.JetStreamMaxMemory, s2.opts.JetStreamMaxStore)
	}
}

func TestRevocationBySerial(t *testing.T) {
	ca := newTestCA(t, testTD)
	rev := &fakeRevocation{}
	s := startTestServer(t, ca, serverOpts{rev: rev})
	cert, x := ca.nodeCert(t, testTD, "alpha")
	// Same format as identity.SerialHex: lowercase hex, no leading zeros.
	rev.revokeSerial(fmt.Sprintf("%x", x.SerialNumber))

	var revoked int
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	nc, err := Connect(ctx, ClientConfig{
		URL: s.ClientURL(), NodeName: "alpha", Cert: cert, RootCAs: ca.pool,
		Logger: discardLogger(), OnAuthRevoked: func() { revoked++ },
	})
	if err == nil {
		nc.Close()
		t.Fatal("cert revoked by serial connected")
	}
	if !isAuthError(err) || revoked != 1 {
		t.Fatalf("err = %v, OnAuthRevoked calls = %d", err, revoked)
	}

	// Another cert for the same node with a different serial still works.
	connectNode(t, s, ca, "alpha", ClientConfig{})
}

func TestConnectionEndsWithCertificate(t *testing.T) {
	ca := newTestCA(t, testTD)
	s := startTestServer(t, ca, serverOpts{})
	cert := ca.shortNodeCert(t, testTD, "alpha", time.Now().Add(2*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nc, err := Connect(ctx, ClientConfig{
		URL: s.ClientURL(), NodeName: "alpha", Cert: cert, RootCAs: ca.pool, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	waitFor(t, 5*time.Second, "alpha listed", func() bool { return len(s.ConnectedNodes()) == 1 })
	waitFor(t, 8*time.Second, "connection closed at certificate expiry", func() bool { return len(s.ConnectedNodes()) == 0 })
}

func TestHubConnAfterShutdown(t *testing.T) {
	ca := newTestCA(t, testTD)
	s, err := StartServer(ServerConfig{
		Host: "127.0.0.1", TLSCert: ca.serverCert(t), ClientCAs: ca.pool, TrustDomain: testTD,
		StoreDir: t.TempDir(), Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	shutdown(t, s)
	if nc, err := s.HubConn(); err == nil || nc != nil {
		t.Fatalf("HubConn after Shutdown = %v, %v; want error", nc, err)
	}
}
