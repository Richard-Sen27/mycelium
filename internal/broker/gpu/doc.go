// Package gpu is the GPU lease broker (spec: "GPU"): grants name a device (or
// MIG slice), exclusive or time-sliced, with a maximum duration. Attach
// reserves the device for the subject and returns the device identifiers the
// sandbox backend or the docker broker needs (e.g. NVIDIA_VISIBLE_DEVICES);
// Detach releases it and kills processes still using it (nvidia-smi
// --query-compute-apps). NVML metrics (utilisation, VRAM, power) are sampled
// every 10 s and reported with Usage() as gpu_seconds plus a metrics map the
// daemon forwards in telemetry.
//
// Contract (owned by the orchestrator): an exclusive lease refuses a second
// Attach for the same device; time-sliced leases share it; a device the node
// does not offer (ceiling) refuses Attach. On macOS there is no device
// passthrough, so the broker reports Available false with that reason and
// GPU work goes through the ollama broker.
//
// Implementation notes (v1):
//   - Detach kills the compute processes left on the device except the
//     daemon's own process tree. For a sandboxed subject the daemon's Detach
//     destroys the sandbox, which kills its processes; the broker kill is the
//     safety net for trusted-host subjects. When a time-sliced lease is
//     released while a neighbour still uses the device, nothing is killed,
//     because the neighbours' processes cannot be told apart. A failed
//     process listing is audited and returned as Detach's error.
//   - MIG slices are queried with nvidia-smi --id=<MIG UUID>; whether every
//     driver accepts a MIG UUID there is unverified on hardware.
//   - Freeze refuses new leases; existing leases stay until detached. The
//     daemon pauses or detaches the subjects that hold them.
package gpu
