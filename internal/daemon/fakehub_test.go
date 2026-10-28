package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

const testTD identity.TrustDomain = "test-hub"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRevocation is the hub's revocation set; it also records every serial
// the bus authenticated.
type fakeRevocation struct {
	mu      sync.Mutex
	revoked map[string]bool // SPIFFE ID
	seen    []string        // serials, in order of authentication
	gate    chan struct{}   // if set, node authentication waits for it to close
}

func (f *fakeRevocation) IsRevoked(serialHex, spiffeID string) bool {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, serialHex)
	return f.revoked[spiffeID]
}

func (f *fakeRevocation) revoke(spiffeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked == nil {
		f.revoked = map[string]bool{}
	}
	f.revoked[spiffeID] = true
}

// hold makes node authentication wait until the returned release is called.
func (f *fakeRevocation) hold() (release func()) {
	g := make(chan struct{})
	f.mu.Lock()
	f.gate = g
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(g)
		})
	}
}

func (f *fakeRevocation) sawSerial(serial string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.seen {
		if s == serial {
			return true
		}
	}
	return false
}

type joinRecord struct {
	name, secretHash, owner string
	caps                    []string
}

// fakeHub is a minimal in-process hub: CA, bus server and EnrolService.
type fakeHub struct {
	t          *testing.T
	ca         *identity.CA
	serverCert tls.Certificate
	rev        *fakeRevocation
	storeDir   string
	http       *httptest.Server

	mu           sync.Mutex
	bus          *bus.Server
	busPort      int
	tokens       map[string]joinRecord
	denyEnrol    bool
	nodeTTL      time.Duration
	renewRevoked bool
	renewCalls   int
	renewFails   int // Renew answers Unavailable this many times first
}

type peerKey struct{}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	ca, err := identity.NewCA(testTD)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueServerCert([]string{"127.0.0.1", "localhost"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{
		t: t, ca: ca, serverCert: sc, rev: &fakeRevocation{}, storeDir: t.TempDir(),
		tokens: map[string]joinRecord{}, nodeTTL: identity.NodeCertLifetime,
	}
	h.startBus(0)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(ca.CertPEM())
	})
	path, handler := mycelv1connect.NewEnrolServiceHandler(&fakeEnrol{h: h})
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), peerKey{}, r.TLS.PeerCertificates[0]))
		}
		mux.ServeHTTP(w, r)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{sc},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    ca.Pool(),
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	h.http = srv
	t.Cleanup(func() {
		srv.Close()
		h.stopBus()
	})
	return h
}

func (h *fakeHub) startBus(port int) {
	h.t.Helper()
	s, err := bus.StartServer(bus.ServerConfig{
		Host: "127.0.0.1", Port: port, TLSCert: h.serverCert, ClientCAs: h.ca.Pool(),
		TrustDomain: testTD, StoreDir: h.storeDir, Revocation: h.rev, Logger: discardLogger(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.bus, h.busPort = s, s.Port()
	h.mu.Unlock()
}

func (h *fakeHub) stopBus() {
	h.mu.Lock()
	s := h.bus
	h.bus = nil
	h.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		h.t.Errorf("bus shutdown: %v", err)
	}
}

func (h *fakeHub) busServer() *bus.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bus
}

func (h *fakeHub) hubConn() *nats.Conn {
	h.t.Helper()
	nc, err := h.busServer().HubConn()
	if err != nil {
		h.t.Fatal(err)
	}
	return nc
}

// newToken creates a join token for name with the hub's real CA fingerprint.
func (h *fakeHub) newToken(name string, caps ...string) string {
	h.t.Helper()
	return h.newTokenWithFingerprint(name, h.ca.Fingerprint(), caps...)
}

func (h *fakeHub) newTokenWithFingerprint(name, fp string, caps ...string) string {
	h.t.Helper()
	tok, err := identity.NewJoinToken(h.http.URL, fp)
	if err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.tokens[tok.ID] = joinRecord{name: name, secretHash: tok.SecretHash(), owner: "u-owner", caps: caps}
	h.mu.Unlock()
	return tok.String()
}

type fakeEnrol struct{ h *fakeHub }

func (f *fakeEnrol) Enrol(_ context.Context, req *connect.Request[mycelv1.EnrolRequest]) (*connect.Response[mycelv1.EnrolResponse], error) {
	h := f.h
	tok, err := identity.ParseJoinToken(req.Msg.GetToken())
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("invalid token"))
	}
	h.mu.Lock()
	rec, ok := h.tokens[tok.ID]
	deny := h.denyEnrol
	ttl := h.nodeTTL
	if ok && !deny {
		delete(h.tokens, tok.ID)
	}
	busURL := ""
	if h.bus != nil {
		busURL = h.bus.ClientURL()
	}
	h.mu.Unlock()
	if deny || !ok || !identity.ConstantTimeEqualHex(rec.secretHash, tok.SecretHash()) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("invalid or used token"))
	}
	certPEM, _, err := h.ca.SignNodeCSR(req.Msg.GetCsrDer(), rec.name, ttl)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&mycelv1.EnrolResponse{
		NodeId:   "node:" + rec.name,
		SpiffeId: identity.NodeSPIFFEID(testTD, rec.name),
		CertPem:  certPEM,
		CaPem:    h.ca.CertPEM(),
		BusUrl:   busURL,
		HubUrl:   h.http.URL,
		Caps:     &mycelv1.Capabilities{Names: rec.caps},
		OwnerId:  rec.owner,
	}), nil
}

func (f *fakeEnrol) Renew(ctx context.Context, req *connect.Request[mycelv1.RenewRequest]) (*connect.Response[mycelv1.RenewResponse], error) {
	h := f.h
	peer, _ := ctx.Value(peerKey{}).(*x509.Certificate)
	if peer == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("client certificate required"))
	}
	name, err := identity.NodeNameFromCert(testTD, peer)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	h.mu.Lock()
	h.renewCalls++
	forced := h.renewRevoked
	fail := h.renewFails > 0
	if fail {
		h.renewFails--
	}
	h.mu.Unlock()
	if fail {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("hub busy"))
	}
	if forced || h.rev.IsRevoked(identity.SerialHex(peer), identity.NodeSPIFFEID(testTD, name)) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("node revoked"))
	}
	certPEM, _, err := h.ca.SignNodeCSR(req.Msg.GetCsrDer(), name, identity.NodeCertLifetime)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&mycelv1.RenewResponse{CertPem: certPEM, CaPem: h.ca.CertPEM()}), nil
}

// ---- small helpers ----

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
