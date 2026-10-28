// Package egress is the node's egress proxy (spec: "Filesystem, network,
// secrets → Egress"): every sandbox's outbound traffic goes through it, it
// allows only the sandbox's allowlisted hosts, and it logs domain and bytes
// per sandbox to the audit stream.
//
// Contract (owned by the orchestrator): exported names are fixed.
//
// Protocol: HTTP forward proxy with CONNECT for TLS (clients set
// HTTP_PROXY/HTTPS_PROXY). Plain HTTP requests are forwarded with the Host
// header checked against the allowlist; CONNECT targets are checked by
// host:port. No TLS interception. Each sandbox authenticates to the proxy
// with a per-sandbox token in the Proxy-Authorization header (Basic, user =
// sandbox id, password = token), so sandboxes cannot use each other's
// allowlists; backends that give each sandbox its own network may instead
// bind one listener per sandbox and skip the token.
package egress

import (
	"context"
	"errors"
	"log/slog"
	"net"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("egress: not implemented")

// Rule is one allowlist entry: "host", "*.suffix", "host:port"; ports default
// to 80 and 443; no CIDRs in v1; IP literals are allowed when listed.
type Rule string

// Audit receives one record per connection or request.
type Audit func(rec Record)

// Record is what gets audited.
type Record struct {
	SandboxID string
	Host      string
	Port      int
	Allowed   bool
	BytesIn   int64
	BytesOut  int64
	Method    string // "CONNECT" or the HTTP method
}

// Proxy is the running egress proxy for one node.
type Proxy struct {
	// unexported
}

// Options configures the proxy.
type Options struct {
	// Listen is the address sandboxes reach, e.g. "127.0.0.1:0" for the fake
	// backend or a vmnet/bridge address for VM backends.
	Listen string
	Audit  Audit
	Logger *slog.Logger
	// MaxConnsPerSandbox caps concurrent connections; default 64.
	MaxConnsPerSandbox int
}

// Start listens and serves until Close.
func Start(ctx context.Context, opts Options) (*Proxy, error) { panic(ErrNotImplemented) }

// Addr is the bound listener address.
func (p *Proxy) Addr() net.Addr { panic(ErrNotImplemented) }

// Register adds a sandbox with its allowlist and returns the proxy URL to put
// into the sandbox environment (http://<sandbox id>:<token>@host:port).
func (p *Proxy) Register(sandboxID string, allow []Rule) (proxyURL string, err error) {
	panic(ErrNotImplemented)
}

// Update replaces a sandbox's allowlist (live grants).
func (p *Proxy) Update(sandboxID string, allow []Rule) error { panic(ErrNotImplemented) }

// Unregister removes a sandbox and closes its connections.
func (p *Proxy) Unregister(sandboxID string) { panic(ErrNotImplemented) }

// Close stops the proxy.
func (p *Proxy) Close() error { panic(ErrNotImplemented) }

// Match reports whether host:port is allowed by the rules.
func Match(rules []Rule, host string, port int) bool { panic(ErrNotImplemented) }
