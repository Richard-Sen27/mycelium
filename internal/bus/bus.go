// Package bus embeds the NATS server in the hub and connects nodes to it over
// mTLS. It owns the subject layout and the per-identity permissions
// (docs/decisions.md D002).
//
// Contract (owned by the orchestrator): exported names and signatures here are
// fixed; the implementing agent fills in bodies and may add helpers.
//
// Permissions per node identity (spiffe://<td>/node/<name>):
//   - publish:   node.<name>.>            (telemetry, rpc requests)
//   - subscribe: node.<name>.>            (ctl requests addressed to it)
//   - subscribe: _INBOX.<name>.>          (replies to its own requests)
//   - responses: may reply to requests it received (NATS allow_responses)
//
// The hub's in-process connection has no restrictions. Nothing else may
// connect. A node whose certificate is revoked is refused at authentication
// and, if connected, dropped by Disconnect.
package bus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"strings"

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
)

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
}

// Server is the embedded NATS server.
type Server struct {
	// unexported fields added by the implementation
}

// StartServer starts the embedded server and waits until it accepts connections.
func StartServer(cfg ServerConfig) (*Server, error) { panic(ErrNotImplemented) }

// ClientURL returns "tls://host:port" of the listener (for enrol responses).
func (s *Server) ClientURL() string { panic(ErrNotImplemented) }

// Port returns the bound port.
func (s *Server) Port() int { panic(ErrNotImplemented) }

// HubConn returns an in-process connection with full permissions. Each call
// returns the same shared connection.
func (s *Server) HubConn() (*nats.Conn, error) { panic(ErrNotImplemented) }

// ConnectedNodes lists node names with at least one live connection.
func (s *Server) ConnectedNodes() []string { panic(ErrNotImplemented) }

// Disconnect drops every connection authenticated as node and returns the
// number dropped. After Disconnect, reconnects are refused as long as
// Revocation reports the certificate revoked.
func (s *Server) Disconnect(node string) int { panic(ErrNotImplemented) }

// Shutdown stops the server; it waits for shutdown up to ctx's deadline.
func (s *Server) Shutdown(ctx context.Context) error { panic(ErrNotImplemented) }

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
func Connect(ctx context.Context, cfg ClientConfig) (*nats.Conn, error) { panic(ErrNotImplemented) }
