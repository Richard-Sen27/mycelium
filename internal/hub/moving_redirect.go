package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/discovery"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Moving the hub, part 1 (moving.go): mDNS advertising, signed redirects
// and moved mode.
//
// Moved mode is entered by `mycel hub redirect` (the admin route POST
// /api/admin/redirect, or Redirect when the hub is stopped) and left only
// by removing `moved_to` from hub.yaml and restarting; no API call leaves it.
// The signed target lives in <hub dir>/redirect.pb next to `moved_to`.

const (
	redirectStateFile = "redirect.pb"
	// redirectRefresh: a redirect older than this is signed again before it
	// is handed out, so nodes that come back days later (receivers accept
	// at most 24 h) still accept it.
	redirectRefresh = time.Hour
	// ctlKeyRedirect is the dedupe key of the Redirect control message.
	ctlKeyRedirect  = "redirect"
	maxRedirectBody = 4 << 10
	// redirectResend: in moved mode a node's heartbeat re-queues the
	// redirect at most this often.
	redirectResend = 30 * time.Second
	mdnsPIDFile    = "mdns.pid"
	// EnvNoMDNS=1 keeps every hub in this process from advertising (tests).
	EnvNoMDNS = "MYCEL_NO_MDNS"
)

// advertise is discovery.Advertise; hub tests replace it so they never
// touch the network.
var advertise = discovery.Advertise

// moveState is the hub's moved mode and its mDNS advertisement.
type moveState struct {
	mu   sync.Mutex
	r    *mycelv1.Redirect // nil: not moved
	adv  discovery.Advertiser
	sent map[string]time.Time // node -> last time the redirect was queued

	exporting atomic.Bool // a live export is running (one at a time)
}

// MDNSEnabled reports whether hub.yaml lets the hub advertise by mDNS.
func (c *Config) MDNSEnabled() bool { return c.Discovery.MDNS == nil || *c.Discovery.MDNS }

// loadMoveState loads moved mode from hub.yaml and redirect.pb; it runs
// before anything of the hub starts.
func loadMoveState(dir string, cfg *Config, ca *identity.CA, log *slog.Logger) *moveState {
	m := &moveState{sent: map[string]time.Time{}}
	if cfg.MovedTo == "" {
		return m
	}
	r, err := loadRedirect(dir, cfg, ca, time.Now(), log)
	if err != nil {
		log.Error("moved mode: cannot build the redirect; every call is refused without one", "err", err)
		r = &mycelv1.Redirect{TrustDomain: string(ca.TrustDomain()), HubUrl: cfg.MovedTo}
	}
	m.r = r
	log.Warn("hub moved: answering every call with a signed redirect (remove moved_to from hub.yaml to undo)", "moved_to", cfg.MovedTo)
	return m
}

// startMove starts the mDNS advertisement unless the hub moved; the
// returned function stops it.
func (s *server) startMove(ctx context.Context) func() {
	m := s.move
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	moved := m.r != nil
	m.mu.Unlock()
	if moved {
		// Moved mode entered at start: startTunnel sees isMoved and stays down.
		return func() {}
	}
	if !moved && s.cfg.MDNSEnabled() && s.cfg.AdvertiseURL != "" && os.Getenv(EnvNoMDNS) != "1" {
		a, err := advertise(ctx, discovery.Hub{
			TrustDomain: string(s.td), URL: s.cfg.AdvertiseURL, BusURL: s.cfg.BusAdvertise, CAFingerprint: s.ca.Fingerprint(),
		}, discovery.AdvertiseOptions{PIDFile: filepath.Join(s.dir, mdnsPIDFile), Logger: s.log})
		if err != nil {
			s.log.Warn("mDNS advertising unavailable", "err", err)
		} else {
			m.mu.Lock()
			if m.r != nil { // moved meanwhile
				m.mu.Unlock()
				_ = a.Close()
				return func() {}
			}
			m.adv = a
			m.mu.Unlock()
			s.log.Info("advertising the hub on the local network (mDNS)", "service", discovery.ServiceType)
		}
	}
	return func() { m.stopAdvertising() }
}

func (m *moveState) stopAdvertising() {
	m.mu.Lock()
	a := m.adv
	m.adv = nil
	m.mu.Unlock()
	if a != nil {
		_ = a.Close()
	}
}

// movedRedirect returns the current redirect in moved mode (signed again
// when it is older than redirectRefresh), or nil.
func (s *server) movedRedirect() *mycelv1.Redirect {
	m := s.move
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.r == nil {
		return nil
	}
	now := time.Now()
	if len(m.r.GetSignature()) > 0 && fresh(m.r.GetIssuedAt(), now) {
		return m.r
	}
	if r, err := signRedirect(s.ca, m.r.GetHubUrl(), m.r.GetBusUrl(), nextIssued(m.r.GetIssuedAt(), now, s.log)); err == nil {
		if err := saveRedirect(s.dir, r); err != nil {
			s.log.Warn("store refreshed redirect", "err", err)
		}
		m.r = r
	} else {
		s.log.Error("sign redirect", "err", err)
	}
	return m.r
}

// fresh reports whether a redirect issued at issued can still be handed
// out: younger than redirectRefresh and not dated beyond what receivers
// accept (a clock that jumped back would leave it in the future).
func fresh(issued int64, now time.Time) bool {
	age := now.Unix() - issued
	return age < int64(redirectRefresh/time.Second) && -age <= identity.RedirectMaxFuture
}

// nextIssued is issued_at for a signature replacing one issued at prev:
// now, or prev+1 while the clock has not passed prev; when prev lies more
// than identity.RedirectMaxFuture ahead (the clock jumped back), now, so
// receivers accept it again.
func nextIssued(prev int64, now time.Time, log *slog.Logger) int64 {
	n := now.Unix()
	if prev > n+identity.RedirectMaxFuture {
		if log != nil {
			log.Warn("clock jump: the last redirect is dated in the future; signing again with the current time", "issued_at", prev, "now", n)
		}
		return n
	}
	return max(n, prev+1)
}

func signRedirect(ca *identity.CA, hubURL, busURL string, issuedAt int64) (*mycelv1.Redirect, error) {
	if hubURL == "" || busURL == "" {
		return nil, errors.New("hub: redirect needs a hub and a bus URL")
	}
	sig, err := ca.SignRedirect(hubURL, busURL, issuedAt)
	if err != nil {
		return nil, err
	}
	return &mycelv1.Redirect{TrustDomain: string(ca.TrustDomain()), HubUrl: hubURL, BusUrl: busURL, IssuedAt: issuedAt, Signature: sig}, nil
}

// loadRedirect builds the redirect for cfg.MovedTo from redirect.pb, or
// signs a fresh one (bus: the new host with the old bus port) when the
// file is missing or names another URL.
func loadRedirect(dir string, cfg *Config, ca *identity.CA, now time.Time, log *slog.Logger) (*mycelv1.Redirect, error) {
	var r mycelv1.Redirect
	b, err := os.ReadFile(filepath.Join(dir, redirectStateFile))
	if err == nil && proto.Unmarshal(b, &r) == nil && r.GetHubUrl() == cfg.MovedTo && r.GetBusUrl() != "" {
		if fresh(r.GetIssuedAt(), now) {
			return &r, nil
		}
		nr, err := signRedirect(ca, r.GetHubUrl(), r.GetBusUrl(), nextIssued(r.GetIssuedAt(), now, log))
		if err != nil {
			return nil, err
		}
		return nr, saveRedirect(dir, nr)
	}
	bus, err := defaultRedirectBus(cfg, cfg.MovedTo)
	if err != nil {
		return nil, err
	}
	nr, err := signRedirect(ca, cfg.MovedTo, bus, nextIssued(r.GetIssuedAt(), now, log))
	if err != nil {
		return nil, err
	}
	return nr, saveRedirect(dir, nr)
}

func saveRedirect(dir string, r *mycelv1.Redirect) error {
	b, err := proto.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, redirectStateFile), b)
}

// writeFileAtomic writes a 0600 file through a temporary file and rename.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// defaultRedirectBus is tls://<new host>:<this hub's bus port>.
func defaultRedirectBus(cfg *Config, hubURL string) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("hub: invalid hub URL %q", hubURL)
	}
	port := "7778"
	for _, src := range []string{cfg.BusAdvertise, cfg.BusListen} {
		src = strings.TrimPrefix(src, "tls://")
		if _, p, err := net.SplitHostPort(src); err == nil && p != "" {
			port = p
			break
		}
	}
	return "tls://" + net.JoinHostPort(u.Hostname(), port), nil
}

// NormalizeRedirect validates a redirect target for the hub with cfg and
// fills the bus URL (default: the new host with this hub's bus port).
func NormalizeRedirect(cfg *Config, hubURL, busURL string) (string, string, error) {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	u, err := url.Parse(hubURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("hub URL must be https://host[:port], got %q", hubURL)
	}
	if strings.EqualFold(hubURL, strings.TrimRight(cfg.AdvertiseURL, "/")) {
		return "", "", errors.New("the new hub URL is this hub's own advertise URL")
	}
	busURL = strings.TrimSpace(busURL)
	if busURL == "" {
		if busURL, err = defaultRedirectBus(cfg, hubURL); err != nil {
			return "", "", err
		}
	}
	hostPort := strings.TrimPrefix(busURL, "tls://")
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil || h == "" || p == "" || strings.ContainsAny(hostPort, "/?#@ ") {
		return "", "", fmt.Errorf("bus address must be host:port or tls://host:port, got %q", busURL)
	}
	return hubURL, "tls://" + hostPort, nil
}

// errBadRedirect marks an invalid redirect target.
var errBadRedirect = errors.New("invalid redirect")

// RedirectResult reports a redirect.
type RedirectResult struct {
	HubURL   string `json:"hub_url"`
	BusURL   string `json:"bus_url"`
	IssuedAt int64  `json:"issued_at"`
	Nodes    int    `json:"nodes"` // nodes it was queued to (or will be sent to on reconnect)
}

// redirectTo signs a redirect, persists moved mode, queues the redirect to
// every node and audits it.
func (s *server) redirectTo(ctx context.Context, actor, hubURL, busURL string) (*RedirectResult, error) {
	hubURL, busURL, err := NormalizeRedirect(s.cfg, hubURL, busURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBadRedirect, err)
	}
	m := s.move
	if m == nil {
		return nil, errors.New("hub: not running")
	}
	m.mu.Lock()
	issued := time.Now().Unix()
	if m.r != nil {
		issued = nextIssued(m.r.GetIssuedAt(), time.Now(), s.log)
	}
	r, err := signRedirect(s.ca, hubURL, busURL, issued)
	if err == nil {
		err = persistMoved(s.dir, r)
	}
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.r = r
	m.mu.Unlock()
	// Moved mode entered at runtime: stop advertising and leave the relay.
	m.stopAdvertising()
	s.stopTunnel()

	nodes, err := s.st.listNodes(ctx)
	if err != nil {
		s.log.Error("redirect: list nodes", "err", err)
	}
	for _, n := range nodes {
		s.queueRedirect(n.Name, r)
	}
	res := &RedirectResult{HubURL: hubURL, BusURL: busURL, IssuedAt: issued, Nodes: len(nodes)}
	s.audit(ctx, actor, "hub.redirect", "hub", outcomeOK, map[string]any{"hub_url": hubURL, "bus_url": busURL, "issued_at": issued, "nodes": len(nodes)})
	return res, nil
}

// persistMoved writes redirect.pb, then moved_to in hub.yaml.
func persistMoved(dir string, r *mycelv1.Redirect) error {
	if err := saveRedirect(dir, r); err != nil {
		return fmt.Errorf("hub: store redirect: %w", err)
	}
	cfg, err := ReadConfig(dir)
	if err != nil {
		return err
	}
	cfg.MovedTo = r.GetHubUrl()
	if err := writeConfig(dir, cfg); err != nil {
		return fmt.Errorf("hub: write %s: %w", hubConfigFile, err)
	}
	return nil
}

// RedirectOptions configures Redirect.
type RedirectOptions struct {
	HubDir string
	HubURL string
	BusURL string // default: the new host with this hub's bus port
	Logger *slog.Logger
}

// Redirect puts a stopped hub in moved mode: it signs the redirect with the
// CA key, stores it with moved_to in hub.yaml and audits it. Nodes receive
// it when they reconnect after the hub starts again. Use the admin route of
// a running hub instead (it also queues the redirect at once).
func Redirect(ctx context.Context, opts RedirectOptions) (*RedirectResult, error) {
	cfg, err := ReadConfig(opts.HubDir)
	if err != nil {
		return nil, fmt.Errorf("hub: read config: %w", err)
	}
	hubURL, busURL, err := NormalizeRedirect(cfg, opts.HubURL, opts.BusURL)
	if err != nil {
		return nil, err
	}
	ca, err := loadHubCA(opts.HubDir)
	if err != nil {
		return nil, err
	}
	unlock, err := lockHubDir(opts.HubDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	issued := time.Now().Unix()
	var old mycelv1.Redirect
	if b, err := os.ReadFile(filepath.Join(opts.HubDir, redirectStateFile)); err == nil && proto.Unmarshal(b, &old) == nil {
		issued = nextIssued(old.GetIssuedAt(), time.Now(), opts.Logger)
	}
	r, err := signRedirect(ca, hubURL, busURL, issued)
	if err != nil {
		return nil, err
	}
	if err := persistMoved(opts.HubDir, r); err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(opts.HubDir, dbFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	nodes, err := st.listNodes(ctx)
	if err != nil {
		return nil, err
	}
	details := map[string]any{"hub_url": hubURL, "bus_url": busURL, "issued_at": issued, "nodes": len(nodes), "offline": true}
	if err := st.appendAudit(ctx, "cli", "hub.redirect", "hub", outcomeOK, details); err != nil {
		return nil, err
	}
	if opts.Logger != nil {
		opts.Logger.Info("audit", "actor", "cli", "action", "hub.redirect", "resource", "hub", "outcome", outcomeOK, "hub_url", hubURL)
	}
	return &RedirectResult{HubURL: hubURL, BusURL: busURL, IssuedAt: issued, Nodes: len(nodes)}, nil
}

func loadHubCA(dir string) (*identity.CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	return identity.LoadCA(certPEM, keyPEM)
}

// ---- control queue and node sync in moved mode ----

func isRedirectCtl(req *mycelv1.CtlRequest) bool {
	_, ok := req.GetCmd().(*mycelv1.CtlRequest_Redirect)
	return ok
}

// dropMovedCtl drops every control message except Redirect in moved mode.
func (s *server) dropMovedCtl(it ctlItem) bool {
	if isRedirectCtl(it.req) || s.movedRedirect() == nil {
		return false
	}
	if it.done != nil {
		go it.done(nil, errCtlDropped)
	}
	return true
}

// queueRedirect replaces node's queued control messages with r.
func (s *server) queueRedirect(node string, r *mycelv1.Redirect) {
	if m := s.move; m != nil {
		m.mu.Lock()
		m.sent[node] = time.Now()
		m.mu.Unlock()
	}
	cq := s.ctlq
	cq.mu.Lock()
	for _, it := range cq.q[node] {
		if it.done != nil {
			go it.done(nil, errCtlDropped)
		}
	}
	if _, ok := cq.q[node]; ok {
		cq.q[node] = nil
	}
	cq.mu.Unlock()
	s.enqueueCtl(node, ctlItem{
		req:      &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Redirect{Redirect: r}},
		priority: true, critical: true, dedupe: ctlKeyRedirect,
	})
}

// resendMovedRedirect, in moved mode, re-queues the redirect for a node
// that is still heartbeating here (it missed or refused the last one, or
// could not reach the new hub yet), at most every redirectResend, and
// reports true; the heartbeat is otherwise ignored. Not moved: false.
func (s *server) resendMovedRedirect(node string) bool {
	r := s.movedRedirect()
	if r == nil {
		return false
	}
	m := s.move
	m.mu.Lock()
	recent := time.Since(m.sent[node]) < redirectResend
	m.mu.Unlock()
	if !recent {
		s.queueRedirect(node, r)
	}
	return true
}

// syncMoved, in moved mode, sends a (re)connecting node only the redirect.
func (s *server) syncMoved(node string) bool {
	r := s.movedRedirect()
	if r == nil {
		return false
	}
	s.queueRedirect(node, r)
	return true
}

// movedError is the "hub moved" answer with the signed redirect attached.
func movedError(r *mycelv1.Redirect) *connect.Error {
	e := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("hub moved to %s", r.GetHubUrl()))
	if len(r.GetSignature()) > 0 {
		if h, err := daemon.EncodeRedirect(r); err == nil {
			e.Meta().Set(daemon.RedirectHeader, h)
		}
	}
	return e
}

// refuseMovedRPC answers a node bus RPC in moved mode.
func (s *server) refuseMovedRPC(msg *nats.Msg) bool {
	r := s.movedRedirect()
	if r == nil {
		return false
	}
	out := nats.NewMsg(msg.Reply)
	out.Header.Set(rpcStatusHeader, bus.StatusError)
	if h, err := daemon.EncodeRedirect(r); err == nil && len(r.GetSignature()) > 0 {
		out.Header.Set(daemon.RedirectHeader, h)
	}
	out.Data, _ = proto.Marshal(&mycelv1.CtlResponse{Ok: false, Error: connect.CodeFailedPrecondition.String() + ": hub moved to " + r.GetHubUrl()})
	if err := s.nc.PublishMsg(out); err != nil {
		s.log.Debug("moved rpc reply", "err", err)
	}
	return true
}

// ---- Connect and HTTP ----

// movedInterceptor answers every Connect call in moved mode.
type movedInterceptor struct{ s *server }

func (mi movedInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if r := mi.s.movedRedirect(); r != nil {
			return nil, movedError(r)
		}
		return next(ctx, req)
	}
}

func (mi movedInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (mi movedInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if r := mi.s.movedRedirect(); r != nil {
			return movedError(r)
		}
		return next(ctx, conn)
	}
}

// movedAllowed lists the routes a moved hub still serves: health, the CA
// (joins verify the redirect against it) and the admin routes.
func movedAllowed(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/ca.pem":
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case "/api/admin/redirect", "/api/admin/export":
		return r.Method == http.MethodPost
	}
	return false
}

// movedHTTP answers the plain HTTP routes in moved mode with 412, the
// redirect header and "hub moved to <url>"; Connect calls are left to the
// interceptor.
func (s *server) movedHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		red := s.movedRedirect()
		if red == nil || strings.HasPrefix(r.URL.Path, "/mycel.v1.") || movedAllowed(r) {
			next.ServeHTTP(w, r)
			return
		}
		if h, err := daemon.EncodeRedirect(red); err == nil && len(red.GetSignature()) > 0 {
			w.Header().Set(daemon.RedirectHeader, h)
		}
		writeAPIError(w, http.StatusPreconditionFailed, "hub moved to "+red.GetHubUrl())
	})
}

// moveRoutes adds the admin routes of moving.go.
func (s *server) moveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/redirect", s.handleAdminRedirect)
	mux.HandleFunc("POST /api/admin/export", s.handleAdminExport)
}

// apiKeyAdmin requires an admin API key (never a cookie session or a node
// read scope) and audits a refused member.
func (s *server) apiKeyAdmin(w http.ResponseWriter, r *http.Request, action string) (*userRow, bool) {
	ctx := r.Context()
	u := userFrom(ctx)
	if u == nil || sessionOf(ctx) != "" || readScopeOf(ctx) != "" {
		writeAPIError(w, http.StatusUnauthorized, "admin API key required")
		return nil, false
	}
	if u.Role != roleAdmin {
		s.audit(ctx, u.ID, action, "hub", outcomeDenied, map[string]any{"reason": "admin only"})
		writeAPIError(w, http.StatusForbidden, "admin only")
		return nil, false
	}
	return u, true
}

func (s *server) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	u, ok := s.apiKeyAdmin(w, r, "hub.redirect")
	if !ok {
		return
	}
	var req struct {
		HubURL string `json:"hub_url"`
		BusURL string `json:"bus_url"`
	}
	if !readAPIJSON(w, r, &req, maxRedirectBody) {
		return
	}
	res, err := s.redirectTo(r.Context(), u.ID, req.HubURL, req.BusURL)
	if errors.Is(err, errBadRedirect) {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.log.Error("redirect", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeAPIJSON(w, http.StatusOK, res)
}
