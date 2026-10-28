// Package docker is the brokered host Docker broker (spec: "Docker →
// Brokered host Docker"): a filtered Docker Engine API proxy. The host
// docker.sock is never exposed; the broker listens on its own unix socket
// (or loopback TCP on Windows) per attached grant and forwards only what the
// grant allows.
//
// Contract (owned by the orchestrator):
//   - Allowed: image list/inspect/pull only for images or registries in the
//     grant's allowlist; container create/start/stop/kill/remove/logs/wait/
//     attach/exec for containers labelled with this grant's sandbox id;
//     list filtered to those labels. Everything else -> 403.
//   - Container create is rewritten: labels mycel.sandbox and mycel.grant
//     added; HostConfig.Privileged, CapAdd, NetworkMode host, PidMode host,
//     IpcMode host, UsernsMode host, Binds/Mounts with host paths, Devices
//     (except the GPU devices of an active gpu grant when the grant has gpu
//     true), SecurityOpt changes and sysctls are refused (400); CPU and memory
//     limits capped to the grant's values; DeviceRequests only for the leased
//     GPUs.
//   - Every request is audited with the operation and image or container id.
//
// Implementation notes (v1):
//   - The create body is decoded into local copies of the Engine's types
//     (the stdlib decoder reads keys exactly as the Engine does) and only a
//     freshly built request of allowlisted fields is forwarded; the labels
//     mycel.grant and mycel.sandbox are always set by the broker.
//   - Brokered host containers have no network: NetworkMode is always none,
//     other networks and port publishing are refused. Models are reached
//     through the ollama broker; image pulls are done by the host engine on
//     its own network.
//   - Every container gets CPU and memory caps (the grant's, else 2 CPUs and
//     4 GiB), MemorySwap equal to its memory, PidsLimit 1024, the local log
//     driver (10 MiB x 3), clamped ulimits, ShmSize and tmpfs sizes, restart
//     policy no or on-failure (at most 3 retries), and the cgroup parent
//     mycel-<grant> (mycel-<grant>.slice under the systemd driver). A grant
//     holds at most 8 containers. Limits are per container; aggregate
//     per-grant limits through the cgroup parent come with phase 7
//     hardening.
//   - DeviceRequests fail closed: they are accepted only for grants with gpu
//     true when Options.GPUDevices returns a non-empty lease list, and are
//     rewritten to {Driver: nvidia, DeviceIDs: <leased>}.
//   - Image pulls are limited to 5 per minute per grant. Registry-wide
//     allowlist entries ("ghcr.io", "myorg/") are broad: they allow every
//     image under that prefix.
//   - Detach cuts open streams, waits for in-flight requests, closes the
//     listener and force-removes the grant's containers; Sweep removes
//     containers of grants that are not attached (the daemon calls it at
//     start). Freeze cuts open streams and pauses the grant containers that
//     run; Unfreeze resumes those.
package docker
