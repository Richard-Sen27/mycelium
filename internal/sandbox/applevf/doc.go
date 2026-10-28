// Package applevf is the macOS sandbox backend: one Linux VM per sandbox on
// Apple's Virtualization.framework, driven through Lima (`limactl` 2.x,
// vmType vz). It is compiled on darwin only and registers itself from init().
//
// # Lima
//
// Every limactl call runs with a dedicated LIMA_HOME (<node data dir>/lima,
// see Backend.SetLimaHome) and a scrubbed environment (no $SSH, no *_proxy;
// see limaEnv), in its own process group. The backend refuses to work while
// _config/override.yaml or _config/default.yaml exists in that LIMA_HOME,
// because Lima merges them into every instance. Instance names are
// "sbx-" + 12 hex digits of sha256(sandbox id), short enough for Lima's unix
// socket paths.
//
// # Create
//
// Create writes <Dir>/lima.yaml (0600, never through a symlink) and runs
// `limactl start`:
//   - the guest is a pinned cloud image of the profile's distribution
//     (images.go maps OCI references such as debian:bookworm to it);
//   - plain mode, no mounts, no port forwards, no containerd, no extra
//     networks, propagateProxyEnv false, no agent forwarding: the guest sees
//     nothing of the host file system or environment;
//   - CPUs, memory and disk of the VM come from Limits; max_processes is
//     enforced in the guest by pam_limits and the user slice's TasksMax;
//   - first-boot provisioning (guest/provision.sh.tmpl) installs packages
//     (nftables, curl and, for docker-nested, rootless Docker) from the
//     distribution's signed repository with unfiltered network — before any
//     sandbox code runs — then creates /workspace, disables user namespaces
//     (unless docker-nested needs them), installs the guest helpers, makes a
//     wrapper that runs bash without profile or rc files the guest user's
//     login shell, sets PermitUserRC no, and loads the firewall.
//
// After start, guest/configure.sh runs once as root: it receives the proxy URL
// on stdin (the credential never appears in argv or in the YAML), writes
// HTTP(S)_PROXY to /etc/environment (read by pam_env for every helper call)
// and /etc/profile.d/mycel.sh, starts rootless Docker when the profile lists
// docker-nested, checks the firewall and that no host mount exists, and
// removes the sandbox user's sudo rights. Every later operation runs as that
// unprivileged user.
//
// # Network
//
// The guest reaches the proxy at 192.168.5.2:<port of Spec.Egress.ProxyURL>.
// Lima's user-mode network (gvisor-tap-vsock in the host agent) turns
// connections to 192.168.5.2 into connections to the host's 127.0.0.1, so the
// node's egress proxy should listen on 127.0.0.1 (0.0.0.0 works too but
// exposes it to the LAN). All sandboxes arrive from 127.0.0.1, so the proxy
// tells them apart only by their Basic credential.
//
// The nftables policy (table inet mycel) accepts output only to loopback, to
// established flows, to 192.168.5.2:<proxy port> and DHCP to the gateway or
// broadcast; everything else, DNS and IPv6 included, is rejected (TCP reset,
// ICMP otherwise), and nothing is forwarded.
//
// Blast radius: the firewall is the only layer that keeps the guest off the
// network, and it lives inside the guest. Whoever gets root in the guest (a
// kernel exploit, a mistake in the lockdown, a future feature that hands out
// root) can flush it and reach, through Lima's NAT, anything the host user
// can reach: the internet, the LAN, and every service listening on the
// host's 127.0.0.1. Lima offers no host-side filter for user-mode networking
// in v1. The guest user has no sudo, no user namespaces (nodocker) and only
// root-owned helpers; with docker-nested it gets user namespaces for rootless
// Docker, which widens the kernel attack surface.
//
// # Operations
//
// Commands run through `limactl shell --shell <wrapper>` and the guest helper
// (guest/helper.sh); see limaSandbox for the argument encoding. Exec requests
// (argv and environment) travel over stdin into a 0600 file in the guest,
// never in any argv. Process streams must be drained concurrently; unread
// output beyond 8 MiB per stream spills to an unlinked file in Dir. CopyOut
// returns a tar stream produced by the guest: it is untrusted.
//
// # Status
//
// Both profiles are exercised on a real VM (Debian 12 arm64):
// TestLimaDefaultProfile and TestLimaDockerNested (MYCEL_TEST_LIMA_DOCKER=1;
// rootless dockerd with the cgroupfs driver, so per-container limits are not
// enforced inside the VM; the VM's own limits still apply). Docker Hub serves
// blobs from production.cloudfront.docker.com, which the dev profile's
// allowlist must include for pulls to work.
package applevf
