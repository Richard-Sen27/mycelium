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
package docker
