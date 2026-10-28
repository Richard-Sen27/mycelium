// Package daemon is the node role ("myceld"): it joins a hub, keeps the bus
// connection, reports inventory, renews its certificate and serves the local
// UI and status endpoint. Later phases add sandboxes, brokers and the MCP
// socket here.
//
// Contract (owned by the orchestrator): Join, Run and their option types are
// what the CLI calls. State lives under NodeDir:
//
//	node.key, node.crt     node identity
//	ca.crt                 hub CA
//	node.yaml              name, hub URL, bus URL, owner, caps ceiling
//
// Local status endpoint (plain HTTP on LocalAddr, default 127.0.0.1:7777):
//
//	GET /api/status  -> JSON {"node":"<name>","hub_url":"...","state":"connecting|connected|disconnected|revoked|expired","connected":bool,"last_heartbeat_at":"...","cert_not_after":"...","inventory":{...}}
//	GET /            -> minimal HTML page showing the same (placeholder until phase 6)
package daemon

import (
	"context"
	"errors"
	"log/slog"

	"github.com/richard-sen27/mycelium/internal/discovery"
)

// JoinOptions configures `mycel join`.
type JoinOptions struct {
	NodeDir string
	Token   string // "mcl1_..."
	// HubURL overrides the URL inside the token (e.g. the hub is reachable
	// under a different address from this network). Usually empty.
	HubURL string
	Logger *slog.Logger
	// Browser finds the hub by mDNS when the token's URL does not answer
	// (phase 7); nil uses discovery.Default().
	Browser discovery.Browser
}

// JoinResult is what Join reports.
type JoinResult struct {
	NodeName     string
	SPIFFEID     string
	HubURL       string
	BusURL       string
	OwnerID      string
	Caps         []string
	CertNotAfter string // RFC 3339
}

// Join enrols this machine: generates a key (if none), builds a CSR, verifies
// the hub's CA against the fingerprint in the token, calls EnrolService.Enrol
// and writes the node state. It fails if NodeDir already holds a node.
func Join(ctx context.Context, opts JoinOptions) (*JoinResult, error) { return join(ctx, opts) }

// Options configures the running node role.
type Options struct {
	NodeDir   string
	LocalAddr string // default "127.0.0.1:7777"; tests use 127.0.0.1:0
	Logger    *slog.Logger
}

// Ready describes the running node's bound addresses.
type Ready struct {
	LocalAddr string
}

// Run starts the node role and blocks until ctx is done, the node is revoked
// (returns ErrRevoked) or a fatal error occurs. If ready is non-nil it receives
// one Ready once the local listener is up.
func Run(ctx context.Context, opts Options, ready chan<- Ready) error {
	return run(ctx, opts, ready, defaultRunConfig())
}

// ErrRevoked is returned by Run when the hub revoked this node.
var ErrRevoked = errors.New("daemon: node revoked by hub")
