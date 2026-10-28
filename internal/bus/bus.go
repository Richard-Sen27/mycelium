// Package bus embeds the NATS server in the hub and connects nodes to it over
// mTLS. It owns the subject layout and the per-identity permissions
// (docs/decisions.md D002).
//
// Contract (owned by the orchestrator): exported names and signatures here are
// fixed; the implementing agent fills in bodies and may add helpers.
//
// Permissions per node identity (spiffe://<td>/node/<name>):
//   - publish:   node.<name>.telemetry    (heartbeats, inventory)
//   - publish:   node.<name>.rpc.>        (rpc requests to the hub)
//   - subscribe: node.<name>.ctl          (ctl requests addressed to it)
//   - subscribe: _INBOX.<name>.>          (replies to its own requests)
//   - responses: may reply to requests it received (NATS allow_responses)
//
// Rule: a node identity never gets publish and subscribe on the same subject.
// Otherwise a node could publish a message with an arbitrary reply subject,
// receive its own echo, and use the one-shot response permission that NATS
// grants for it to publish anywhere. The hub must also refuse node RPCs whose
// reply subject fails ValidReply.
//
// The hub's in-process connection has no restrictions. Nothing else may
// connect. A node whose certificate is revoked is refused at authentication
// and, if connected, dropped by Disconnect.
package bus

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("bus: not implemented")

// ---- Subjects ----

// NodeCtl is the subject the hub sends CtlRequest on (request-reply).
func NodeCtl(node string) string { return "node." + node + ".ctl" }

// NodeTelemetry is the subject a node publishes Heartbeat on.
func NodeTelemetry(node string) string { return "node." + node + ".telemetry" }

// NodeRPC is the subject a node publishes a request on; the hub replies.
func NodeRPC(node, method string) string { return "node." + node + ".rpc." + method }

// Wildcards the hub subscribes to.
const (
	AllTelemetry = "node.*.telemetry"
	AllRPC       = "node.*.rpc.>"
)

// Node RPC method names (the last token of NodeRPC subjects).
const (
	RPCRegisterAgent     = "agent.register"
	RPCRefreshAgentToken = "agent.refresh"

	// Phase 2: agent-scoped RPCs. Each request carries an AgentCall token
	// that the hub verifies and binds to the node named in the subject.
	RPCAgentPost         = "agent.post"          // AgentPostRequest -> PostResponse
	RPCAgentJoinChannel  = "agent.channel.join"  // AgentJoinChannelRequest -> AgentJoinChannelResponse
	RPCAgentLeaveChannel = "agent.channel.leave" // AgentLeaveChannelRequest -> AgentLeaveChannelResponse
	RPCAgentListChannels = "agent.channel.list"  // AgentListChannelsRequest -> ListChannelsResponse
	RPCAgentListAgents   = "agent.list"          // AgentListAgentsRequest -> AgentListAgentsResponse
	RPCAgentWhoami       = "agent.whoami"        // AgentWhoamiRequest -> AgentWhoamiResponse
	RPCAgentInboxPull    = "agent.inbox.pull"    // InboxPullRequest -> InboxPullResponse (long poll)
	RPCAgentBBGet        = "agent.bb.get"        // AgentBlackboardGetRequest -> BlackboardGetResponse
	RPCAgentBBPut        = "agent.bb.put"        // AgentBlackboardPutRequest -> BlackboardPutResponse
	RPCAgentListMessages = "agent.messages.list" // AgentListMessagesRequest -> ListMessagesResponse
)

// StatusHeader is set on every RPC reply from the hub: "ok" or "error". On
// "error" the payload is a mycelv1.CtlResponse{ok:false, error}. Clients check
// the header before decoding the payload (docs/decisions.md D014).
const (
	StatusHeader = "Mycel-Status"
	StatusOK     = "ok"
	StatusError  = "error"
)

// JetStream streams owned by the hub (phase 2). Only the hub publishes to them.
const (
	StreamChannels        = "CH" // subjects ch.>; one Message per entry
	StreamChannelsSubject = "ch.>"
	StreamAudit           = "AUDIT" // subjects audit.>; JSON audit entries
	StreamAuditSubject    = "audit.>"
)

// ChannelSubject is the persisted subject for a channel's messages:
// "ch.<name without the leading '#'>.msg". Channel names never contain '.',
// '*', '>' or spaces (see internal/channels), so the token is safe as is.
func ChannelSubject(channel string) string {
	return "ch." + strings.TrimPrefix(channel, "#") + ".msg"
}

// AuditSubject is "audit.<action>" with '.' in the action replaced by '_'.
func AuditSubject(action string) string {
	return "audit." + strings.ReplaceAll(action, ".", "_")
}

// NodeFromSubject returns the node name in a "node.<name>.<rest>" subject.
func NodeFromSubject(subject string) (string, bool) {
	parts := strings.SplitN(subject, ".", 3)
	if len(parts) < 3 || parts[0] != "node" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// RPCMethodFromSubject returns the method in a "node.<name>.rpc.<method>" subject.
func RPCMethodFromSubject(subject string) (string, bool) {
	parts := strings.SplitN(subject, ".", 4)
	if len(parts) != 4 || parts[0] != "node" || parts[2] != "rpc" || parts[3] == "" {
		return "", false
	}
	return parts[3], true
}

// InboxPrefix is the custom inbox prefix for a node connection.
func InboxPrefix(node string) string { return "_INBOX." + node }

// ValidReply reports whether reply is an acceptable reply subject for a
// request from node: it must lie under InboxPrefix(node)+"." and contain no
// wildcards. The hub refuses node RPCs whose reply fails this check.
func ValidReply(node, reply string) bool {
	prefix := InboxPrefix(node) + "."
	if node == "" || len(reply) <= len(prefix) || !strings.HasPrefix(reply, prefix) {
		return false
	}
	for _, tok := range strings.Split(reply[len(prefix):], ".") {
		if tok == "" || tok == "*" || tok == ">" || strings.ContainsAny(tok, "*> \t\r\n") {
			return false
		}
	}
	return true
}

// ---- Server (hub side) ----

// Revocation answers whether a node certificate is revoked. Implemented by the
// hub's store. Both the serial and the SPIFFE ID are provided so either can be
// the key.
type Revocation interface {
	IsRevoked(serialHex, spiffeID string) bool
}

// ServerConfig configures the embedded NATS server.
type ServerConfig struct {
	Host        string // listen host, default "0.0.0.0"
	Port        int    // listen port; 0 = random (tests)
	TLSCert     tls.Certificate
	ClientCAs   *x509.CertPool
	TrustDomain identity.TrustDomain
	StoreDir    string // JetStream directory (used from phase 2; may be created now)
	Revocation  Revocation
	Logger      *slog.Logger
	// JetStream resource limits in bytes; zero selects the defaults
	// (DefaultJetStreamMaxMemory, DefaultJetStreamMaxStore).
	JetStreamMaxMemory int64
	JetStreamMaxStore  int64
}

// Server limits.
const (
	DefaultJetStreamMaxMemory int64 = 256 << 20 // 256 MiB
	DefaultJetStreamMaxStore  int64 = 8 << 30   // 8 GiB

	maxConn    = 4096
	maxSubs    = 512
	maxPending = 8 << 20 // 8 MiB per client
	maxPayload = 1 << 20 // 1 MiB
)

// errShutdown is returned by HubConn after Shutdown.
var errShutdown = errors.New("bus: server is shut down")

// Server is the embedded NATS server.
type Server struct {
	ns    *server.Server
	host  string
	td    identity.TrustDomain
	token string // in-process hub secret; never logged
	log   *slog.Logger

	opts    *server.Options // as passed to the server (tests inspect it)
	shut    atomic.Bool
	hubOnce sync.Once
	hubConn *nats.Conn
	hubErr  error
}

// StartServer starts the embedded server and waits until it accepts connections.
func StartServer(cfg ServerConfig) (*Server, error) {
	if len(cfg.TLSCert.Certificate) == 0 {
		return nil, errors.New("bus: server TLS certificate is required")
	}
	if cfg.ClientCAs == nil {
		return nil, errors.New("bus: client CA pool is required")
	}
	if cfg.TrustDomain == "" {
		return nil, errors.New("bus: trust domain is required")
	}
	if cfg.StoreDir == "" {
		return nil, errors.New("bus: JetStream store directory is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "bus")
	host := cfg.Host
	if host == "" {
		host = "0.0.0.0"
	}

	token, err := randomToken()
	if err != nil {
		return nil, err
	}

	s := &Server{host: host, td: cfg.TrustDomain, token: token, log: logger}

	jsMem := cfg.JetStreamMaxMemory
	if jsMem == 0 {
		jsMem = DefaultJetStreamMaxMemory
	}
	jsStore := cfg.JetStreamMaxStore
	if jsStore == 0 {
		jsStore = DefaultJetStreamMaxStore
	}

	opts := &server.Options{
		// A fixed name keeps the JetStream identity stable across restarts.
		ServerName: "hub",
		Host:       host,
		Port:       cfg.Port,
		MaxConn:    maxConn,
		MaxSubs:    maxSubs,
		MaxPending: maxPending,
		MaxPayload: maxPayload,
		// AllowNonTLS must stay false: the TCP listener then requires a TLS
		// handshake (and a verified client cert) before any protocol is
		// parsed, so only the in-process hub connection can reach Check
		// without TLS. TestAllowNonTLSOff pins this.
		AllowNonTLS: false,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cfg.TLSCert},
			ClientCAs:    cfg.ClientCAs,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS13,
		},
		TLSVerify:          true,
		TLSTimeout:         5,
		JetStream:          true,
		JetStreamMaxMemory: jsMem,
		JetStreamMaxStore:  jsStore,
		StoreDir:           cfg.StoreDir,
		NoSigs:             true,
		CustomClientAuthentication: &authenticator{
			td:         cfg.TrustDomain,
			revocation: cfg.Revocation,
			token:      token,
			log:        logger,
		},
	}
	if cfg.Port == 0 {
		// server.Options treats 0 as "default port 4222"; RANDOM_PORT asks for
		// an ephemeral one.
		opts.Port = server.RANDOM_PORT
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("bus: create server: %w", err)
	}
	s.opts = opts
	ns.SetLogger(&natsLogger{log: logger.With("source", "nats-server")}, false, false)
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		return nil, errors.New("bus: server not ready within 10s")
	}
	s.ns = ns
	logger.Info("bus listening", "url", s.ClientURL())
	return s, nil
}

// ClientURL returns "tls://host:port" of the listener (for enrol responses).
// It is the local listener address (127.0.0.1 when listening on all
// interfaces); the hub advertises its configured bus_advertise address to
// nodes instead.
func (s *Server) ClientURL() string {
	host := s.host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "tls://" + net.JoinHostPort(host, strconv.Itoa(s.Port()))
}

// Port returns the bound port.
func (s *Server) Port() int {
	if addr, ok := s.ns.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// HubConn returns an in-process connection with full permissions. Each call
// returns the same shared connection.
func (s *Server) HubConn() (*nats.Conn, error) {
	if s.shut.Load() {
		return nil, errShutdown
	}
	s.hubOnce.Do(func() {
		if s.shut.Load() {
			s.hubErr = errShutdown
			return
		}
		s.hubConn, s.hubErr = nats.Connect("",
			nats.InProcessServer(s.ns),
			nats.Token(s.token),
			nats.Name("hub"),
			// "_hub" can never be a node name, so no node can subscribe to the
			// hub's reply inboxes.
			nats.CustomInboxPrefix("_INBOX._hub"),
			nats.MaxReconnects(-1),
		)
		if s.hubErr != nil {
			s.hubErr = fmt.Errorf("bus: hub connection: %w", s.hubErr)
		}
	})
	if s.shut.Load() {
		return nil, errShutdown
	}
	return s.hubConn, s.hubErr
}

// nodeConns returns the connection IDs of all open node connections, keyed by
// node name.
func (s *Server) nodeConns() map[string][]uint64 {
	out := map[string][]uint64{}
	const page = 1024
	for offset := 0; ; offset += page {
		cz, err := s.ns.Connz(&server.ConnzOptions{Username: true, Offset: offset, Limit: page})
		if err != nil {
			s.log.Warn("bus: list connections", "err", err)
			return out
		}
		for _, ci := range cz.Conns {
			name, err := parseNodeSPIFFEID(s.td, ci.AuthorizedUser)
			if err != nil {
				continue
			}
			out[name] = append(out[name], ci.Cid)
		}
		if len(cz.Conns) < page {
			return out
		}
	}
}

// ConnectedNodes lists node names with at least one live connection.
func (s *Server) ConnectedNodes() []string {
	conns := s.nodeConns()
	names := make([]string, 0, len(conns))
	for name := range conns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Disconnect drops every connection authenticated as node and returns the
// number dropped. After Disconnect, reconnects are refused as long as
// Revocation reports the certificate revoked.
func (s *Server) Disconnect(node string) int {
	// A connection may pass Check just before the caller marked the
	// certificate revoked and only show up in Connz a moment later, so scan
	// twice more over ~300 ms.
	const passes = 3
	const gap = 150 * time.Millisecond
	n := 0
	for pass := 0; pass < passes; pass++ {
		if pass > 0 {
			time.Sleep(gap)
		}
		for _, cid := range s.nodeConns()[node] {
			if err := s.ns.DisconnectClientByID(cid); err != nil {
				s.log.Warn("bus: disconnect client", "node", node, "cid", cid, "err", err)
				continue
			}
			n++
		}
	}
	if n > 0 {
		s.log.Info("bus: node disconnected", "node", node, "connections", n)
	}
	return n
}

// Shutdown stops the server; it waits for shutdown up to ctx's deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shut.Store(true)
	// Synchronise with a concurrent HubConn before reading s.hubConn.
	s.hubOnce.Do(func() {})
	if s.hubConn != nil {
		s.hubConn.Close()
	}
	done := make(chan struct{})
	go func() {
		s.ns.Shutdown()
		s.ns.WaitForShutdown()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("bus: shutdown: %w", ctx.Err())
	}
}

// ---- Authentication ----

// authenticator implements server.Authentication: node certificates on TLS
// connections, a shared secret token on the hub's in-process connection.
type authenticator struct {
	td         identity.TrustDomain
	revocation Revocation
	token      string
	log        *slog.Logger
}

// Check authenticates a client connection and registers its permissions.
func (a *authenticator) Check(c server.ClientAuthentication) bool {
	if c.Kind() != server.CLIENT {
		return false
	}
	opts := c.GetOpts()
	if opts == nil {
		return false
	}
	// Other credential fields would make Connz report them instead of the
	// registered username, hiding the connection from ConnectedNodes and
	// Disconnect. Nobody legitimately sends them.
	if opts.Nkey != "" || opts.JWT != "" || opts.Sig != "" || opts.Username != "" || opts.Password != "" {
		a.log.Warn("bus: refused connection with unsupported credentials", "remote", remoteAddr(c))
		return false
	}
	if st := c.GetTLSConnectionState(); st != nil {
		if opts.Token != "" {
			a.log.Warn("bus: refused TLS connection carrying a token", "remote", remoteAddr(c))
			return false
		}
		return a.checkNode(c, st)
	}
	// No TLS: only the in-process connection can get here, because the TCP
	// listener requires TLS (AllowNonTLS is false). Defence in depth: refuse
	// anything with a TCP peer address, and require the secret token.
	if _, isTCP := c.RemoteAddress().(*net.TCPAddr); isTCP {
		a.log.Warn("bus: refused non-TLS TCP connection", "remote", remoteAddr(c))
		return false
	}
	if opts.Token == "" || subtle.ConstantTimeCompare([]byte(opts.Token), []byte(a.token)) != 1 {
		a.log.Warn("bus: refused connection without TLS", "remote", remoteAddr(c))
		return false
	}
	c.RegisterUser(&server.User{Username: "hub"})
	return true
}

func (a *authenticator) checkNode(c server.ClientAuthentication, st *tls.ConnectionState) bool {
	// The TLS layer has verified the chain against ClientCAs; require that it
	// actually did.
	if len(st.PeerCertificates) == 0 || len(st.VerifiedChains) == 0 {
		a.log.Warn("bus: refused TLS connection without verified client certificate", "remote", remoteAddr(c))
		return false
	}
	cert := st.PeerCertificates[0]
	if len(cert.URIs) != 1 {
		a.log.Warn("bus: refused certificate without exactly one URI SAN", "remote", remoteAddr(c))
		return false
	}
	id := cert.URIs[0].String()
	name, err := parseNodeSPIFFEID(a.td, id)
	if err != nil {
		a.log.Warn("bus: refused certificate", "remote", remoteAddr(c), "err", err)
		return false
	}
	// The serial format is SerialNumber.Text(16) (lowercase hex, no leading
	// zeros), identical to identity.SerialHex, which the revocation store uses.
	serial := cert.SerialNumber.Text(16)
	if a.revocation != nil && a.revocation.IsRevoked(serial, id) {
		a.log.Warn("bus: refused revoked node", "node", name, "serial", serial)
		return false
	}
	c.RegisterUser(&server.User{
		Username:    id,
		Permissions: nodePermissions(name),
		// The connection must not outlive the certificate.
		ConnectionDeadline: cert.NotAfter,
	})
	return true
}

// nodePermissions are the subject permissions of a node identity. No subject
// is both publishable and subscribable: otherwise a node could receive its
// own message carrying a forged reply subject and use the response
// permission to publish anywhere.
func nodePermissions(name string) *server.Permissions {
	return &server.Permissions{
		Publish: &server.SubjectPermission{Allow: []string{
			NodeTelemetry(name),
			"node." + name + ".rpc.>",
		}},
		Subscribe: &server.SubjectPermission{Allow: []string{
			NodeCtl(name),
			InboxPrefix(name) + ".>",
		}},
		Response: &server.ResponsePermission{MaxMsgs: 1, Expires: 2 * time.Minute},
	}
}

// parseNodeSPIFFEID returns the node name of "spiffe://<td>/node/<name>" and
// rejects anything else, including other trust domains.
func parseNodeSPIFFEID(td identity.TrustDomain, id string) (string, error) {
	prefix := "spiffe://" + string(td) + "/node/"
	if td == "" || !strings.HasPrefix(id, prefix) {
		return "", fmt.Errorf("not a node SPIFFE ID in trust domain %q", td)
	}
	name := id[len(prefix):]
	if !validNodeName(name) {
		return "", errors.New("invalid node name in SPIFFE ID")
	}
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "spiffe" || u.Host != string(td) || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Path != "/node/"+name {
		return "", errors.New("malformed SPIFFE ID")
	}
	return name, nil
}

// validNodeName mirrors identity.ValidNodeName: 1–63 chars of [a-z0-9-], not
// starting or ending with "-". It also guarantees the name is a single NATS
// subject token without wildcards.
func validNodeName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}

func remoteAddr(c server.ClientAuthentication) string {
	if a := c.RemoteAddress(); a != nil {
		return a.String()
	}
	return ""
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("bus: generate hub token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// natsLogger routes nats-server logs into slog.
type natsLogger struct{ log *slog.Logger }

func (l *natsLogger) Noticef(format string, v ...any) { l.log.Info(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Warnf(format string, v ...any)   { l.log.Warn(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Fatalf(format string, v ...any)  { l.log.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Errorf(format string, v ...any)  { l.log.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Debugf(format string, v ...any)  { l.log.Debug(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Tracef(format string, v ...any)  { l.log.Debug(fmt.Sprintf(format, v...)) }

// ---- Client (node side) ----

// ClientConfig configures a node's connection to the hub bus.
type ClientConfig struct {
	URL      string // from EnrolResponse.bus_url
	NodeName string
	Cert     tls.Certificate // node cert + key
	RootCAs  *x509.CertPool  // hub CA
	Logger   *slog.Logger
	// OnDisconnect is called with the error that caused a disconnect (may be
	// nil). OnReconnect is called after a successful reconnect.
	OnDisconnect func(error)
	OnReconnect  func()
	// OnAuthRevoked is called when the server refuses the connection with an
	// authorization error: the node has been revoked. The client stops
	// reconnecting after this.
	OnAuthRevoked func()
}

// Connect dials the hub bus with mTLS. It retries until ctx is done; after a
// successful connect, reconnection is automatic and unbounded. The returned
// connection uses InboxPrefix(cfg.NodeName) and the name cfg.NodeName.
func Connect(ctx context.Context, cfg ClientConfig) (*nats.Conn, error) {
	if cfg.URL == "" {
		return nil, errors.New("bus: URL is required")
	}
	if !validNodeName(cfg.NodeName) {
		return nil, fmt.Errorf("bus: invalid node name %q", cfg.NodeName)
	}
	if len(cfg.Cert.Certificate) == 0 {
		return nil, errors.New("bus: node certificate is required")
	}
	if cfg.RootCAs == nil {
		return nil, errors.New("bus: root CA pool is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "bus", "node", cfg.NodeName)

	var revokedOnce sync.Once
	revoked := func(nc *nats.Conn) {
		revokedOnce.Do(func() {
			logger.Warn("bus: hub refused authorization; node revoked, not reconnecting")
			if nc != nil {
				// Stop the reconnect loop right away instead of waiting for
				// nats.go to give up after the second refusal.
				go nc.Close()
			}
			if cfg.OnAuthRevoked != nil {
				cfg.OnAuthRevoked()
			}
		})
	}

	opts := []nats.Option{
		nats.Secure(&tls.Config{
			Certificates: []tls.Certificate{cfg.Cert},
			RootCAs:      cfg.RootCAs,
			MinVersion:   tls.VersionTLS13,
		}),
		nats.CustomInboxPrefix(InboxPrefix(cfg.NodeName)),
		nats.Name(cfg.NodeName),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ReconnectJitter(100*time.Millisecond, 500*time.Millisecond),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("bus: disconnected", "err", err)
			if cfg.OnDisconnect != nil {
				cfg.OnDisconnect(err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("bus: reconnected", "url", nc.ConnectedUrlRedacted())
			if cfg.OnReconnect != nil {
				cfg.OnReconnect()
			}
		}),
		nats.ErrorHandler(func(nc *nats.Conn, _ *nats.Subscription, err error) {
			if isAuthError(err) {
				revoked(nc)
				return
			}
			logger.Warn("bus: async error", "err", err)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			if isAuthError(nc.LastError()) {
				revoked(nc)
				return
			}
			logger.Info("bus: connection closed")
		}),
	}

	backoff := 500 * time.Millisecond
	for {
		nc, err := nats.Connect(cfg.URL, opts...)
		if err == nil {
			logger.Info("bus: connected", "url", nc.ConnectedUrlRedacted())
			return nc, nil
		}
		if isAuthError(err) {
			// A refused certificate is not transient; retrying cannot help.
			revoked(nil)
			return nil, fmt.Errorf("bus: connect: %w", err)
		}
		logger.Warn("bus: connect failed, retrying", "err", err, "in", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// isAuthError reports whether err is the server refusing authorization.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, nats.ErrAuthorization) || errors.Is(err, nats.ErrAuthRevoked) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), nats.AUTHORIZATION_ERR)
}
