// Package hub is the control-plane role: identity (CA, tokens), the registry
// of users, nodes and agents, policy checks, the embedded bus, the audit log
// and the Connect-RPC API.
//
// Contract (owned by the orchestrator): Init, Run and their option types are
// what the CLI calls. Everything else in this package is the hub work
// package's to design. State lives under Options.DataDir (the hub dir):
//
//	ca.crt, ca.key          root CA
//	server.crt, server.key  TLS server cert for API and bus
//	agent-token.key         HMAC key for agent tokens
//	hub.yaml                trust domain, advertise URL, listen addresses
//	hub.db                  SQLite
//	jetstream/              bus storage (phase 2)
package hub

import (
	"context"
	"errors"
	"log/slog"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("hub: not implemented")

// InitOptions configures `mycel hub init`.
type InitOptions struct {
	HubDir      string // config.Dirs.HubDir()
	NodeDir     string // config.Dirs.NodeDir(); the hub machine is enrolled as a node here
	TrustDomain string // e.g. "my-team"; default: host of AdvertiseURL
	// AdvertiseURL is how nodes reach the API, e.g. https://hub.example.com:7777.
	// Its host is added to the server certificate together with localhost,
	// 127.0.0.1 and the machine hostname.
	AdvertiseURL string
	// BusAdvertise is the bus address given to nodes, e.g. tls://hub.example.com:7778.
	// Default: AdvertiseURL's host with BusPort.
	BusAdvertise string
	APIListen    string // default ":7777"
	BusListen    string // default ":7778"
	AdminName    string // first admin user, default "admin"
	LocalNode    string // name for this machine's node, default hostname
	Logger       *slog.Logger
}

// InitResult is returned by Init.
type InitResult struct {
	TrustDomain   string
	AdvertiseURL  string
	CAFingerprint string
	CACertPEM     []byte
	AdminUserID   string
	AdminAPIKey   string // shown once
	LocalNodeName string
}

// Init creates the hub state. It fails if HubDir already holds a hub. It also
// enrols the local machine as a node owned by the admin, writing the node
// state into NodeDir (docs/decisions.md D006).
func Init(ctx context.Context, opts InitOptions) (*InitResult, error) { return initHub(ctx, opts) }

// Options configures the running hub role.
type Options struct {
	HubDir string
	// APIListen and BusListen override hub.yaml when non-empty (tests use
	// 127.0.0.1:0 and read the bound ports back from Ready).
	APIListen string
	BusListen string
	Logger    *slog.Logger
	// AllowFakeProfile loads the CI-only "fake" sandbox profile (phase 4).
	// Never set outside tests; the CLI exposes it as --allow-fake-profile.
	AllowFakeProfile bool
}

// Ready describes the running hub's bound addresses.
type Ready struct {
	APIAddr string // host:port actually bound
	BusAddr string
	HubURL  string // https://<api addr> for local callers
}

// Run starts the hub role and blocks until ctx is done or a fatal error
// occurs. If ready is non-nil it receives one Ready once the listeners are up.
func Run(ctx context.Context, opts Options, ready chan<- Ready) error {
	return runHub(ctx, opts, ready)
}
