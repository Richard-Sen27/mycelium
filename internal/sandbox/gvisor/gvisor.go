//go:build linux

// Package gvisor is the Linux sandbox backend: one gVisor (runsc) container
// per sandbox, built from an OCI bundle by package bundle, which holds all
// the logic; this package only registers it.
//
// Requirements on the node (none of this has run on a real Linux machine
// yet; the logic is unit tested with a recording fake runner):
//
//   - runsc on PATH (`runsc --version` must work), iproute2 (`ip`), and nft or
//     iptables/ip6tables;
//   - the Docker client, used only to export image root filesystems (`docker
//     create` + `docker export`, filtered and extracted once per digest under
//     /var/lib/mycel/sbx/images); the Docker socket is never mounted into a
//     sandbox;
//   - the daemon running as root: network setup needs CAP_NET_ADMIN and
//     CAP_SYS_ADMIN (namespaces, veth pairs, firewall rules) and runsc runs
//     as root. The planned narrowing is a small root helper that accepts only
//     the fixed verbs package bundle uses;
//   - cgroups available to runsc (limits apply under /mycel/<container id>);
//   - the egress proxy listening on 0.0.0.0: the daemon sets that listen
//     address on Linux nodes (from MYCEL_EGRESS_LISTEN). Each sandbox
//     reaches the proxy at the host side of its own veth pair, the proxy
//     URL's host is rewritten to that address, and host-wide rules (nft
//     table inet mycel, or iptables chain MYCEL-HOST) accept the proxy port
//     only from lo and the myh* interfaces;
//   - no host firewall that drops INPUT from the myh* interfaces before the
//     per-sandbox rules (nft table inet mycel_<id>, or iptables chain
//     MYCEL-<id>).
//
// Nested Docker is not available inside these sandboxes in v1
// (Capabilities.NestedDocker is false).
package gvisor

import (
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/gvisor/bundle"
)

func init() {
	sandbox.Register(bundle.New(bundle.Config{
		Name: sandbox.BackendGVisor,
		Mode: bundle.ModeLinux,
		Host: bundle.LocalHost{},
	}))
}
