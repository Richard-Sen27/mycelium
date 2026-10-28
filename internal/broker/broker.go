// Package broker holds what every resource broker shares (spec: "Resource
// brokers"): a broker is a host-side proxy in myceld that exposes one
// resource to sandboxes and trusted-host agents, checks every request against
// the live grants attached to the requesting subject and the node's local
// ceiling, and audits every request. Brokers never give a sandbox the
// underlying socket or device.
//
// Contract (owned by the orchestrator): exported names are fixed; the
// subpackages ollama, gpu and docker implement Broker.
package broker

import (
	"context"
	"errors"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("broker: not implemented")

// Kind names, equal to the grant spec kinds and the profile broker names.
const (
	KindOllama     = "ollama"
	KindGPU        = "gpu"
	KindDockerHost = "docker-host"
)

// Attachment is one active grant on this node, as the broker sees it.
type Attachment struct {
	Grant     *mycelv1.Grant
	SandboxID string // "" for a trusted-host agent
	ExpiresAt time.Time
}

// Usage is what a broker reports to the hub through broker.usage.
type Usage struct {
	GrantID    string
	Subject    string
	Broker     string
	Tokens     int64
	Requests   int64
	GPUSeconds int64
	Model      string
}

// Audit receives one record per brokered request.
type Audit func(rec Record)

// Record is an audited broker request.
type Record struct {
	Broker    string
	GrantID   string
	Subject   string
	SandboxID string
	Op        string // "generate", "chat", "pull", "containers.create", "lease", …
	Target    string // model, image, device
	Allowed   bool
	Reason    string
	Bytes     int64
	Tokens    int64
}

// Broker is one resource proxy. Implementations are safe for concurrent use.
type Broker interface {
	Kind() string
	// Available reports whether the resource exists on this node.
	Available(ctx context.Context) (bool, string)
	// Attach makes the grant usable for its subject; it returns the address
	// the sandbox or agent uses (an http URL for ollama and docker-host, a
	// device list for gpu). Attaching a grant again replaces its terms.
	Attach(ctx context.Context, a Attachment) (endpoint string, err error)
	// Detach removes the grant: open requests are refused, leases are
	// released and processes using them are killed (gpu).
	Detach(ctx context.Context, grantID string, reason string) error
	// Freeze makes the broker deny every request until Unfreeze.
	Freeze(reason string)
	Unfreeze()
	// Usage returns and resets the increments since the last call.
	Usage() []Usage
	Close() error
}

// Ceiling is the node's local sharing ceiling as the brokers see it: an
// offer that is switched off locally makes Attach refuse with ErrCeiling
// and makes every request of attached grants of that kind fail, even if the
// hub granted it (spec: "Node owner has the final say").
type Ceiling interface {
	Offers(kind string) bool
	// Active reports whether sharing is active right now (schedule, idle).
	Active() bool
}

// Errors.
var (
	ErrCeiling    = errors.New("broker: refused by the node's local sharing settings")
	ErrFrozen     = errors.New("broker: frozen")
	ErrNoGrant    = errors.New("broker: no active grant")
	ErrNotAllowed = errors.New("broker: not allowed by the grant")
	ErrRateLimit  = errors.New("broker: rate limit")
)
