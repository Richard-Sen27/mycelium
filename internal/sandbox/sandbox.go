// Package sandbox defines the isolation backend interface every job and
// sandboxed agent runs behind (spec: "Sandbox model"). Backends live in
// subpackages: applevf (macOS, Apple Virtualization.framework through the
// `container` tool or Lima), wsl2 (Windows, gVisor inside the mycel-sbx
// distro), gvisor (Linux, runsc), firecracker (Linux with KVM) and fake (CI
// only, build tag fakesandbox).
//
// Contract (owned by the orchestrator): exported names and documented
// semantics are fixed; implementations may add helpers.
//
// Rules every backend must keep:
//   - a sandbox starts with no network, no mounts and no devices beyond what
//     Spec says; Spec comes from a resolved profile plus live grants;
//   - /workspace is seeded by copy, never by bind-mounting the host;
//   - the host Docker socket is never mounted (spec: never);
//   - egress goes only through the node's egress proxy (Spec.Egress.ProxyURL);
//     a backend that cannot enforce that reports it in Capabilities and the
//     hub refuses profiles with egress on such backends;
//   - Destroy must leave nothing running and nothing on disk but the
//     backend's image cache.
package sandbox

import (
	"context"
	"errors"
	"io"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("sandbox: not implemented")

// Backend names as reported in inventory.sandbox_backends and profile lists.
const (
	BackendAppleVF     = "applevf"
	BackendWSL2GVisor  = "wsl2-gvisor"
	BackendGVisor      = "gvisor"
	BackendFirecracker = "firecracker"
	BackendFake        = "fake"
)

// Capabilities describes what a backend on this node can enforce.
type Capabilities struct {
	Egress        bool // all guest traffic can be forced through the proxy
	NestedDocker  bool // a private dockerd can run inside the sandbox
	ProcessLimits bool // max_processes is enforced
	MemoryLimit   bool
	CPULimit      bool
	Isolation     string // "vm", "gvisor", "container", "none" (fake)
}

// Backend creates sandboxes.
type Backend interface {
	Name() string
	// Available reports whether the backend can run on this node right now
	// and, if not, why (missing tool, no KVM, WSL distro absent).
	Available(ctx context.Context) (bool, string)
	Capabilities() Capabilities
	// Prepare fetches the image for a profile if needed (may take minutes;
	// report progress through the callback). Safe to call concurrently for
	// different images.
	Prepare(ctx context.Context, image string, progress func(string)) error
	Create(ctx context.Context, spec Spec) (Sandbox, error)
}

// Spec is everything a backend needs to create one sandbox.
type Spec struct {
	ID        string // sbx:<ULID>
	JobID     string // job_… or "" for an agent sandbox
	Profile   *mycelv1.Profile
	Image     string
	Limits    Limits
	Env       map[string]string // already validated: no MYCEL_* keys
	Workspace WorkspaceSeed
	Egress    Egress
	Mounts    []Mount
	// Dir is a node-local scratch directory for this sandbox (logs, seeds);
	// the backend may use it and must remove its contents on Destroy.
	Dir string
}

// Limits are the hard caps (spec: "Limits").
type Limits struct {
	CPUs         int
	MemoryBytes  int64
	DiskBytes    int64
	MaxProcesses int
	WallClock    time.Duration
}

// WorkspaceSeed is how /workspace is filled before the command runs.
type WorkspaceSeed struct {
	// Tarball is a local path to a tar(.gz) extracted into /workspace, or "".
	Tarball string
	// Inputs are local files copied to /workspace/<Path>.
	Inputs []Input
}

// Input is one file to copy in.
type Input struct {
	LocalPath string
	Path      string // relative to /workspace
}

// Egress is the sandbox network policy. ProxyURL is the node's egress proxy
// reachable from the sandbox (backend-specific address). Allow is the host
// allowlist the proxy enforces; the backend only needs to make the proxy the
// sole route out.
type Egress struct {
	ProxyURL string
	Allow    []string
}

// Mount is a host folder by grant (never $HOME or /; the hub validates).
type Mount struct {
	HostPath  string
	GuestPath string
	ReadOnly  bool
}

// Sandbox is one running isolation unit.
type Sandbox interface {
	ID() string
	Backend() string
	// Exec runs argv inside the sandbox with cwd /workspace (or opts.Dir).
	Exec(ctx context.Context, argv []string, opts ExecOpts) (Process, error)
	// CopyIn writes a file into the sandbox at path (relative to /workspace).
	CopyIn(ctx context.Context, src io.Reader, path string, mode uint32) error
	// CopyOut reads a file or directory (as a tar stream) from the sandbox.
	CopyOut(ctx context.Context, path string) (io.ReadCloser, error)
	// Glob lists files matching patterns relative to /workspace.
	Glob(ctx context.Context, patterns []string) ([]string, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Stats(ctx context.Context) (Stats, error)
	// Destroy stops everything and removes the sandbox. Idempotent.
	Destroy(ctx context.Context) error
}

// ExecOpts tunes one execution.
type ExecOpts struct {
	Dir   string
	Env   map[string]string
	Stdin io.Reader
	TTY   bool
}

// Process is a running command.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait blocks until exit and returns the exit code; a kill by limit or
	// cancel returns an error wrapping ErrKilled or ErrTimeout.
	Wait() (int, error)
	Signal(sig string) error // "TERM", "KILL"
}

// Stats is a point-in-time resource reading.
type Stats struct {
	CPUMilli     int64
	MemoryBytes  int64
	DiskBytes    int64
	Processes    int
	EgressBytes  int64
	RunningSince time.Time
}

// Errors.
var (
	ErrKilled      = errors.New("sandbox: killed")
	ErrTimeout     = errors.New("sandbox: wall clock exceeded")
	ErrUnavailable = errors.New("sandbox: backend unavailable")
	ErrLimit       = errors.New("sandbox: resource limit hit")
)

// Registry holds the backends compiled into this binary, in preference order.
// Each backend package registers itself from an init() behind its build tag.
var registry []Backend

// Register adds a backend; called from backend packages' init().
func Register(b Backend) { registry = append(registry, b) }

// Backends returns the registered backends.
func Backends() []Backend { return registry }

// Select picks the first backend in the profile's preference list that is
// registered and available. It never picks "fake" unless the profile lists
// it explicitly, and the fake backend only exists under build tag
// fakesandbox.
func Select(ctx context.Context, profile *mycelv1.Profile) (Backend, error) {
	return selectBackend(ctx, profile)
}

// ResolveLimits merges profile limits with job overrides; job values may
// only lower them. Zero profile values take the package defaults
// (2 CPUs, 4 GiB RAM, 20 GiB disk, 512 processes, 1 h wall clock).
func ResolveLimits(profile *mycelv1.Limits, override *mycelv1.Limits) (Limits, error) {
	return resolveLimits(profile, override)
}

// ValidateMount refuses $HOME, /, system roots and paths that escape by
// symlink; GuestPath must be absolute and not under /workspace.
func ValidateMount(m Mount) error {
	_, err := resolveMount(m)
	return err
}

// ResolveMount validates m like ValidateMount and returns it with HostPath
// replaced by its symlink-resolved form and GuestPath cleaned; backends
// mount the returned HostPath, never the original.
func ResolveMount(m Mount) (Mount, error) { return resolveMount(m) }

// ValidateEnv checks a sandbox environment: keys match
// ^[A-Za-z_][A-Za-z0-9_]*$, no MYCEL_* keys, no proxy variables (*_PROXY,
// ALL_PROXY, NO_PROXY in any case; the backend sets those), no NUL in
// values. Every backend and the hub call it.
func ValidateEnv(env map[string]string) error { return validateEnv(env) }
