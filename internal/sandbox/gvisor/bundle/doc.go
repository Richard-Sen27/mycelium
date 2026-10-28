// Package bundle is the platform-independent core shared by the gVisor
// (Linux) and WSL2 (Windows) sandbox backends: OCI runtime bundle generation,
// rootfs preparation, network namespace and firewall setup, argv builders for
// runsc, ip, nft, iptables, docker and wsl.exe, parsing of `runsc events` and
// `runsc ps`, garbage collection, and the Backend/Sandbox/Process
// implementation itself.
//
// None of this has run on a real Linux or Windows machine yet: it is unit
// tested on any OS with a recording fake Runner and compile-checked for
// linux and windows.
//
// Every external program is started through a Runner with an explicit argv.
// The one exception to "no shell" is DetachScript on WSL, a fixed script fed
// on stdin that takes all values as positional parameters. A Host wraps argv
// for the machine the sandbox lives on: identity on Linux, `wsl.exe -d
// mycel-sbx -u root --exec …` on Windows, where arguments with control
// characters or double quotes are refused because they cross the Windows
// command line.
//
// Layout on the sandbox host (inside the mycel-sbx distro on Windows):
//
//	<state>/images/<digest hex>/        rootfs, extracted once per digest; the
//	                                    shared read-only lower layer
//	<state>/runsc/                      runsc --root state
//	<state>/sbx-<ULID>/config.json      OCI runtime config (0600)
//	<state>/sbx-<ULID>/overlay/         backing file of the root overlay
//	<state>/sbx-<ULID>/workspace/       bind-mounted at /workspace, seeded by copy
//	<state>/sbx-<ULID>/proc/            per-exec process.json (0600) and pid files
//
// Sandbox ids must be sbx:<ULID>, so a directory name can never be
// "images", "runsc", "." or ".." or contain a slash.
//
// runsc global flags (see Runsc):
//
//	--network=sandbox     gVisor's user-space netstack on the interfaces of the
//	                      sandbox's own network namespace (--network=none when
//	                      the sandbox has no egress)
//	--host-uds=none       no host Unix sockets
//	--host-fifo=none      no host FIFOs
//	--overlay2=root:dir=<sandbox>/overlay
//	                      writable overlay over the shared rootfs; its backing
//	                      file lives in the sandbox directory, so the shared
//	                      lower directory is never written and all writes go
//	                      away with the sandbox
//
// Image export: v1 obtains a rootfs with the host Docker client (`docker
// create` + `docker export`). Docker is used only to export an image to a
// tar stream; the Docker socket is never mounted into any sandbox. The stream
// is filtered in Go (FilterRootfs: no devices, FIFOs, absolute or ".." names,
// entries below symlinks or hard links outside the tree; setuid/setgid
// stripped) before it is extracted as root into a unique .part directory that
// is renamed into place.
//
// Isolation and limits: the boundary is the gVisor sentry and its host
// seccomp filter. OCI masked and read-only paths and any OCI seccomp profile
// are mostly no-ops under gVisor (/proc and /sys are the sentry's own
// emulation); they are kept so the bundle is valid OCI. Memory, CPU and pids
// limits apply through linux.resources under cgroupsPath /mycel/<container
// id>. /tmp is a tmpfs bounded to half the memory limit. Workloads run as
// uid:gid 1000:1000 by default (Config.User), with a reduced capability set
// (DefaultCaps) and no new privileges. Disk is measured, not capped. The
// sandbox wall clock kills every process (`runsc kill --all`) and running
// Execs report ErrTimeout; cancelling an Exec or sending KILL kills its
// process tree.
//
// Guest helpers: seeding, CopyIn, CopyOut, Glob and the resolv.conf write run
// binaries from the image inside the sandbox (tar, dd, mkdir, chmod, stat,
// cat, find). Scratch and distroless images without them are unsupported,
// and their output (CopyOut streams, Glob listings) is untrusted data:
// extract CopyOut tars without owners and without setuid/setgid bits.
//
// Network: a sandbox with egress gets its own network namespace and a veth
// pair on a private /30 out of a pool (default 10.237.0.0/16; slots whose
// addresses are already assigned on the host are skipped). The host side
// carries the host address (.1), the namespace side the sandbox address (.2)
// and a default route via the host address. Per-sandbox firewall rules
// (nftables when `nft` exists, iptables and ip6tables otherwise) accept from
// the sandbox only TCP to the egress proxy address:port and drop everything
// else, IPv6 included. DNS is off by default (Config.AllowDNS): the proxy
// resolves names and resolv.conf has no nameserver. Host-wide rules limit
// the proxy port to lo and the myh* interfaces (Linux; the daemon sets the
// egress proxy listen address to 0.0.0.0 on Linux nodes, from
// MYCEL_EGRESS_LISTEN) and drop host-initiated connections into sandboxes.
// When the proxy is not on the sandbox host (WSL2: it runs on Windows) the
// accepted flows are forwarded and masqueraded instead; this sets
// net.ipv4.ip_forward=1 in the distro, which is not reverted. A sandbox
// without egress gets --network=none and no namespace at all.
//
// Privileges: network setup needs CAP_NET_ADMIN and CAP_SYS_ADMIN (ip netns
// bind-mounts), and runsc runs as root. v1 expects the daemon to run as
// root for these backends; the planned narrowing is a small root helper that
// accepts only the fixed verbs this package uses.
//
// Lifecycle: the first Create runs GC, which removes runsc containers,
// namespaces, links, nft tables and iptables chains that earlier daemon runs
// left behind and no live sandbox owns. Destroy ignores only "does not
// exist" failures, confirms with `runsc state` that the container is gone,
// and otherwise reports the failure and stays retryable.
package bundle
