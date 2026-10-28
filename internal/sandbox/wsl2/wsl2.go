//go:build windows

// Package wsl2 is the Windows sandbox backend: one gVisor (runsc) container
// per sandbox inside the dedicated WSL2 distro mycel-sbx. Every command runs
// in the distro as `wsl.exe -d mycel-sbx -u root --exec <argv…>`; arguments
// with control characters or double quotes are refused because they cross
// the Windows command line. The OCI bundle, network and lifecycle logic is
// package bundle's, shared with the Linux gvisor backend. Sandbox
// directories live inside the distro under /var/lib/mycel/sbx/sbx-<ULID>.
//
// Nothing here has run on a real Windows machine yet; the logic is unit
// tested with a recording fake runner.
//
// The mycel-sbx distro must contain runsc, iproute2 (`ip`), nft or
// iptables/ip6tables, sh and setsid (util-linux, with --wait), and
// coreutils, tar and findutils (mkdir, install, dd, chmod, chown, mv, rm,
// du, cat, test, find). `runsc run --detach` runs through `setsid -w` with
// stdio on /dev/null inside the distro (bundle.DetachScript) so the sandbox
// does not keep wsl.exe's pipes open. Every command runs with -u root.
//
// WSL must use the default NAT networking mode: the egress proxy runs on
// Windows and sandboxes reach it at the Windows host's address on the WSL
// interface, read from `ip route show default` inside the distro; the
// sandbox's proxy URL host is rewritten to that address and the distro
// forwards and masquerades only that address:port. This sets
// net.ipv4.ip_forward=1 in the distro, which is not reverted. The daemon's
// egress proxy must listen on that interface or on 0.0.0.0, and the Windows
// firewall must allow inbound connections from the WSL interface to the
// proxy port.
//
// Images are exported with the Docker client on Windows (`docker create` +
// `docker export`, e.g. Docker Desktop), filtered, and extracted into the
// distro once per digest; Prepare fails with a clear error when no Docker
// client is installed. Nested Docker is not available inside these
// sandboxes in v1.
package wsl2

import (
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/gvisor/bundle"
)

func init() {
	sandbox.Register(bundle.New(bundle.Config{
		Name: sandbox.BackendWSL2GVisor,
		Mode: bundle.ModeWSL,
		Host: bundle.WSLHost{Distro: bundle.DefaultDistro},
	}))
}
