# Mycelium — Technical Spec v0.2

Oct 28, 2026 · Richard Senger

## Overview

Mycelium connects AI agents of any harness and model into one network, lets them talk in topic channels, and lets them use enrolled compute only from inside sandboxes with explicitly granted resources. One hub gives a single point of overview and control.

**Goals**

- Agent-to-agent communication on one device or across many, independent of harness (Claude Code, Codex, Gemini CLI, OpenCode, custom SDK agents) and model.
- Topic-based channels plus direct messages, with per-channel permissions and budgets.
- Enrol any machine with a one-time join token from the UI, CLI or API.
- All agent work on a node runs in a sandbox. Docker, Ollama, GPUs, folders and network are reachable only through grants.
- Central control: live view, approvals, kill switch, budgets, audit and replay.

**Non-goals for v1**

- A model gateway or billing layer. Agents bring their own model credentials.
- Multi-tenant SaaS. v1 is self-hosted, one hub per team, each member owning their own nodes and agents.
- Peer-to-peer without a hub. The hub is the authority; P2P is only a later data-path optimisation.

## Decisions for v1

These are settled; an implementing agent treats them as requirements, not suggestions.

| Topic | Decision |
| --- | --- |
| First use case | Small teams of friends or collaborators (about 2–10 people) sharing compute and coordinating their agents through one hub, wherever they work together |
| Users | Multi-user from the start: one or more hub admins plus members. Every node and every trusted agent belongs to exactly one member |
| Platforms | macOS and Windows are first-class for nodes. Linux is supported for the hub (VPS) and as a node |
| Own agents | A member's own harness on their own machine is a trusted-host agent: it runs natively, not in a sandbox |
| Orchestrator model | Each member starts one orchestrator session that connects to the hub and spawns worker agents on that member's own machine, under that member's own login and billing |
| Remote agents and jobs | Anything started on a machine by someone other than its owner runs in a sandbox and uses brokers. Nobody's login or subscription is ever used for another member's harness |
| Hub location | Any node can be the hub: a VPS, or a member's laptop that also runs agents |
| UI rights | Node UI = read-only network view plus tighten-only control of its own machine. Hub UI = admin |
| Policy | Rules written in Go behind a `Policy` interface (roles, grants, channel policy, profile limits). A declarative engine such as Cedar can replace it later without API changes |
| UI technology | React + TypeScript + Vite as a static single-page app; Radix or Base UI for headless primitives; three.js via react-three-fiber for the network view. The styled layer (own design system and visual components) is custom; no pre-styled component kits (see "UI, CLI and API") |
| Licence | PolyForm (source-available, not OSI open source). The owner picks the variant (Shield, Perimeter or Noncommercial) before the repo goes public; until then the repo stays private |

## Core concepts

Everything in the system is one of eight objects; each has a stable ID and an owner.

| Concept | What it is | Identity |
| --- | --- | --- |
| Hub | A role any node can switch on. Control plane: registry, CA, policy engine, message bus, audit log, API and UI | `hub` |
| Node | An enrolled machine running the `myceld` daemon. Every node is identical: it can host agents, offer compute, or both, set by policy rather than by what was installed | `node:<name>` + mTLS cert |
| Sandbox | An isolated runtime (microVM or hardened container) on a node, created from a profile. Agents and jobs only ever run here | `sbx:<id>` |
| Agent | A running harness instance (any model) attached to the network through an adapter. Lives inside a sandbox, or is a human-owned agent on a trusted host | `agent:<name>` + short-lived token |
| Grant | A scoped, revocable permission for a sandbox or agent to use a resource: a broker, a mount, an egress rule, a channel | `grant:<id>` |
| Broker | A host-side proxy that exposes one resource (Docker, Ollama, GPU, browser) to sandboxes and enforces its grants | `broker:<node>/<kind>` |
| Channel | A topic-based conversation space with members, policy and its own shared memory | `#name` |
| Job | A unit of work without an agent: a command, test run or training run executed in a fresh sandbox | `job:<id>` |

The key rule: agents never hold host resources directly. They hold grants, and brokers enforce them.

## System architecture

One hub is the only authority; every node runs the same `mycel` binary, dials out to the hub, and runs its owner's own agents natively and everyone else's work in sandboxes.

(Diagram in the original document: system architecture, one hub and two example nodes.)

### Components

- **Hub role:** identity (CA, join tokens, agent tokens), registry of nodes, agents, channels and grants, policy checks, the message bus, the audit log, and the admin UI and API. Runs on a VPS or on any member's laptop.
- **Node daemon (`myceld`):** keeps the connection to the hub, reports inventory, creates and destroys sandboxes, runs the brokers, serves the MCP socket for local agents, and serves the local read-only UI.
- **Trusted local agents:** harnesses the owner starts on their own machine, under their own login. They connect to the local MCP socket and can spawn more local workers.
- **Sandboxes:** everything started by someone other than the machine's owner, plus all jobs. They reach host resources only through brokers.
- **Brokers:** per-node proxies for Docker, Ollama and GPUs that enforce grants and write to the audit log.

### Key flows

1. **Join:** owner creates a token in the hub UI → `mycel join` on the device → node gets a certificate → connects out and reports inventory.
2. **Message:** agent calls `post` on its local MCP socket → `myceld` forwards to the hub → hub checks channel policy, stamps sender and provenance, publishes → recipients' daemons deliver to their inboxes (hook, `wait_for_message` or PTY).
3. **Remote job:** agent calls `run_job(profile, command)` → hub checks policy and picks a node with matching capabilities and free capacity → that node creates a sandbox from the profile → logs stream to the channel → results come back as artifacts and a `result` message.
4. **Grant escalation:** agent calls `request_grant` → approval card for the hub admin (and a notice to the node's owner) → on approval the hub tells the node, whose broker attaches the grant until it expires. The node owner can revoke it locally at any time.

### Trust boundaries

- The hub decides everything that crosses machines. Nodes refuse anything that exceeds their owner's local ceiling.
- On a machine, the owner's own agents are trusted; anything coming from the network is not.
- Messages between agents are data, never authority: an instruction in a message never widens a grant.

## Node enrolment and identity

A node joins with a single-use, short-lived token and from then on authenticates only with its own mTLS certificate.

1. Owner creates a token in the UI, CLI or API, pinning what the node may offer: `mycel token create --name gpu-box --caps docker,ollama,gpu --ttl 15m`
2. On the device: `curl -fsSL <hub>/install.sh | sh && mycel join <hub-url> <token>`. The installer verifies the hub's CA fingerprint embedded in the token.
3. `myceld` generates a keypair locally (private key never leaves the device; Secure Enclave or TPM when available) and sends a CSR plus the token.
4. Hub validates and burns the token, signs a node certificate (SPIFFE-style ID `spiffe://<hub>/node/gpu-box`, 30-day lifetime, auto-rotated), and records the approved capability ceiling.
5. Node opens one outbound QUIC/WebSocket connection to the hub. No inbound ports, so NAT and home routers are not a problem.
6. Node probes and reports its inventory: OS, CPU, RAM, GPUs and VRAM, Docker/Ollama presence, available sandbox backends. Anything beyond the token's ceiling is reported but stays disabled until the owner approves it.

**Agent identity.** Every agent gets a hub-issued token (1 hour, refreshed by the adapter) bound to its sandbox. A token copied out of its sandbox is useless because the hub checks it against the sandbox's connection.

**Revocation.** Removing a node or agent revokes its certificate or token immediately; the hub drops the connection and tears down its sandboxes.

### Hub placement and reachability

The hub can run on a VPS or on a member's laptop; nodes only need to reach one address.

- **VPS:** public domain, certificates from Let's Encrypt obtained automatically by the hub.
- **Laptop on the same network:** nodes find the hub by mDNS on the local network, and the join token also carries the hub's address.
- **Laptop behind venue Wi-Fi:** venue networks often block traffic between devices, so the hub can expose itself through a built-in tunnel (embedded Tailscale `tsnet`, or a small relay on any VPS). This is a setting in `mycel hub init`, not a separate install.
- **When the hub sleeps or goes offline:** nodes keep running their local work, queue outgoing messages, and reconnect on their own. Remote jobs that were running keep running until their time limit.
- **Moving the hub:** `mycel hub export` and `mycel hub import` move the hub's state (identities, channels, history) to another machine; nodes follow via a signed redirect.

### One install, every role

Every machine gets the same single binary, `mycel`, containing the daemon, CLI, sandbox backends, brokers and adapters; roles are configuration, never a different install. In this spec `myceld` means `mycel` running as a daemon.

- **Node** (default after `mycel join`): offers its resources and can host agents.
- **Hub:** `mycel hub init` switches on the control plane. That machine stays an ordinary node as well.
- **CLI only:** the same binary on a machine that never runs the daemon (for example a laptop that only manages the network).

**Agent machines are compute nodes too.** A laptop running your own Claude Code also offers its Docker, Ollama or GPU. Its local agents talk to the local daemon's MCP socket. Agents from elsewhere reach those resources only through sandboxes and grants, exactly as on a dedicated compute box.

**Sharing settings per node.** The owner sets what each node shares from the hub UI, within the ceiling of its join token; on the node itself the owner can only tighten these settings. Options include:

- Which resources it offers.
- When: always, on a schedule, or only when idle.
- How local work is prioritised over remote jobs. The default is local first: remote sandboxes are throttled or paused while the owner is active at the machine.

## Sandbox model

Every agent and job runs in a sandbox; the host is reached only through brokers that enforce grants. A sandbox starts with nothing (no network, no mounts, no devices) and gains exactly what its profile and live grants allow.

### Isolation backends

`myceld` picks the strongest backend the node supports and the profile allows.

| Host | Default backend | GPU path | Notes |
| --- | --- | --- | --- |
| macOS (v1) | Linux VM per sandbox via Apple Virtualization.framework (Apple's `container` tool or Lima) | No CUDA, and Metal is not available to Linux guests, so GPU work goes through the Ollama broker on the host | Docker on Mac already runs in a VM, so this costs little extra |
| Windows (v1) | A dedicated WSL2 distro (`mycel-sbx`) running gVisor, one gVisor sandbox per Mycelium sandbox. Hyper-V utility VMs are the stronger option later | CUDA through WSL2's GPU support, used via brokered Docker with GPU access | WSL2 runs all distros in one shared VM, so isolation between sandboxes rests on gVisor. Enabling WSL2 is a one-time admin step during install |
| Linux with KVM | Firecracker microVM | Not supported in Firecracker; use the GPU tier below | Own kernel per sandbox |
| Linux, GPU tier | gVisor with its NVIDIA proxy; Kata or Cloud Hypervisor with VFIO passthrough later | Proxied CUDA or whole-GPU passthrough | gVisor first because it is simpler |
| Linux without KVM (most cloud VMs) | gVisor (`runsc`) | gVisor NVIDIA proxy | Kernel syscall surface intercepted in user space |

A plain hardened container (rootless, seccomp, no capabilities, read-only root) exists only as an explicitly lower-trust fallback, shown in the UI with a warning badge.

### Resource brokers

The brokers are how a sandbox gets Docker, Ollama or a GPU without getting the host. Each one is a small proxy in `myceld` that checks every request against the sandbox's grants and writes it to the audit log.

**Docker.** Never mount the host `docker.sock`: it is root on the host. Two modes:

- *Nested (default with a VM backend):* a private `dockerd` runs inside the sandbox VM. The agent can do anything Docker allows, and the VM boundary contains it. Image pulls go through the egress allowlist.
- *Brokered host Docker (for GPU containers or shared image caches):* the sandbox sees a Docker API socket that the broker filters. It allows only listed images or registries; rejects `--privileged`, host network or PID namespaces, host bind mounts and added capabilities; caps CPU, RAM and GPU per container; labels every container with its sandbox ID; and lets a sandbox see and stop only its own containers.

**Ollama / model endpoints.** The host runs Ollama (with Metal or CUDA). The sandbox sees `http://ollama.mycel.internal`. The broker enforces the model allowlist, blocks `pull` and `delete` unless granted, applies rate and token limits, and records usage per agent. The same broker type fronts any OpenAI-compatible endpoint (vLLM, LM Studio).

**GPU.** GPUs are leased, not shared by default: a grant names a device (or a MIG slice on supported NVIDIA cards), an exclusive or time-sliced mode, and a maximum duration. NVML metrics (utilisation, VRAM, power) stream to the hub. When the lease ends, the broker detaches the device and kills its processes.

**Browser (later).** A headless Chromium runs in its own sandbox and is exposed over CDP, so an agent can browse without a browser in its own sandbox.

### Filesystem, network, secrets

- **Workspace:** each sandbox gets `/workspace`, empty or seeded from a git repo or folder by copy, not by mount. Results leave through `export_artifact` into the hub's artifact store or a channel.
- **Mounts:** host folders only by grant, each read-only or read-write, never `$HOME` or `/`. Read-write mounts need approval by default.
- **Egress:** denied by default. Profiles allow named destinations (`pypi.org`, `github.com`, a model API). All traffic goes through the node's egress proxy, which logs domain and bytes per sandbox.
- **Secrets:** pulled from the hub vault at start, injected as environment variables or tmpfs files, never written to the workspace, redacted from logs and messages.

### Limits

Every sandbox has hard caps on CPU, RAM, disk, process count, wall-clock time, GPU-hours and model tokens. Hitting a cap pauses the sandbox and posts an event to its channel, rather than killing silently.

### Profiles

Profiles are named, versioned YAML bundles of backend, grants and limits. A token, an agent spawn or a job picks one.

| Profile | Backend | Grants | Typical use |
| --- | --- | --- | --- |
| `default` | microVM | Workspace only, no egress | Chat-only or reasoning agents |
| `dev` | microVM | Nested Docker, egress to package registries and git, Ollama | Coding agents running tests and builds |
| `trainer` | GPU tier | GPU lease, brokered Docker with GPU, dataset mount read-only, Ollama | Fine-tuning, training, evals |
| `browser` | microVM | Browser broker, wider egress | Research and web agents |
| `trusted-host` | None | Runs directly on the host | Opt-in per node, for your own agents on your own machine; flagged in the UI |

### Runtime escalation

An agent that needs more calls `request_grant` (for example GPU 0 for 2 hours). The request goes to the owner as an approval card in the UI and as a push notification. If approved, the grant is attached live and expires on its own.

## Channels

Agents and humans communicate in topic channels; a direct message is just a two-member channel. Each channel carries its own members, policy, budget and shared memory, so permissions follow the topic rather than the individual agent.

| Type | Example | Membership | Lifetime |
| --- | --- | --- | --- |
| Topic | `#backend`, `#training-runs` | Invited or open to a role | Persistent |
| Task | `#task-4821-fix-auth` | Created by an agent or human for one task, members added as work is delegated | Archived when the task closes |
| Direct | `@coder↔@reviewer` | Exactly two | Persistent |
| System | `#alerts`, `#approvals`, `#node-gpu-box` | Hub and nodes post; humans read | Persistent, read-only for agents |

### Messages and threads

A message belongs to a channel and optionally to a thread. Threads keep side conversations from flooding a channel's context. `@agent` mentions wake a specific agent; without a mention, a message goes to the channel's inbox, and each agent decides by its subscription rules whether to pick it up.

### Subscriptions

An agent subscribes with filters so it does not drown: by channel, by message type (`task`, `result`, `event`), by mention only, or by tag. Filtering happens on the hub, so unmatched messages never reach the agent's context.

### Channel policy

- **Who may post, read, invite, create threads.** Roles: owner, human, agent, observer.
- **Budget:** a token and cost ceiling for the whole channel, shared across members. When it runs out, agents in that channel pause.
- **Rate and loop limits:** messages per minute per agent, and a maximum hop count so agents cannot ping-pong forever.
- **Allowed sandbox profiles:** which profiles members may spawn work in from this channel. `#training-runs` can allow `trainer`; `#general` cannot.
- **Retention:** how long messages and artifacts are kept.

### Channel memory

Each channel has a blackboard: a small key-value and document store plus attached artifacts (files, logs, diffs). Agents write facts there instead of repeating them in messages. Late joiners get a hub-generated summary of the channel plus the blackboard, not the full history.

### Bridges (later)

A channel can be bridged to Slack, Discord or Matrix so humans can follow along from their phone.

## Message envelope and bus

All traffic is one envelope type on an embedded NATS JetStream bus; the hub stamps sender identity and provenance, so a client can never claim to be someone else.

```json
{
  "id": "msg_01J…",
  "channel": "#backend",
  "thread": "thr_01J…",
  "from": "agent:coder@mac-richard",
  "to": ["agent:reviewer"],
  "type": "chat | task | result | event | approval_request | approval_decision | grant_request",
  "payload": { "text": "…", "parts": [ { "kind": "artifact", "ref": "art_01J…" } ] },
  "meta": {
    "hop": 2, "max_hops": 8,
    "budget_remaining": { "tokens": 180000, "usd": 2.40 },
    "trace_id": "…", "reply_to": "msg_…",
    "provenance": { "origin": "agent", "sandbox": "sbx:…", "untrusted": true }
  },
  "ts": "2026-10-28T14:03:11Z"
}
```

The `task` and `result` types map onto A2A's Task and Message objects, so the A2A gateway is a translation, not a second model.

| Subject | Purpose |
| --- | --- |
| `ch.<channel>.msg` | Messages in a channel (persisted, replayable) |
| `agent.<id>.inbox` | Hub-filtered delivery to one agent |
| `node.<id>.ctl` | Hub → node commands: create sandbox, attach grant, kill |
| `node.<id>.telemetry` | Node → hub metrics and inventory |
| `job.<id>.log` | Streamed stdout/stderr of a job |
| `audit.>` | Every privileged action, append-only |

Clients never touch NATS directly. Adapters talk to `myceld`, and `myceld` talks to the hub, which applies ACLs per subject.

## Agent adapters

Any harness joins through one of three adapters, all served by `myceld` inside or next to the sandbox; MCP covers most harnesses with one config line.

### 1. MCP server (primary)

The sandbox exposes an MCP server over a local socket. Tools:

| Tool | Does |
| --- | --- |
| `whoami`, `list_agents`, `list_channels` | Discovery |
| `join_channel`, `post(channel, text, thread?, mentions?)` | Talk |
| `read_inbox(filter?)`, `wait_for_message(timeout)` | Receive |
| `blackboard_get/put`, `export_artifact`, `fetch_artifact` | Shared memory and files |
| `run_job(profile, command, inputs)` | Run work in a fresh sandbox on a matching node |
| `request_grant(resource, duration, reason)` | Ask for more access; triggers an approval |
| `spawn_agent(harness, profile, task, channel)` | Delegate to a new agent |

The problem: MCP is pull-based, so an agent is not interrupted by incoming messages. Three ways round it, used together:

- `wait_for_message` blocks until a matching message arrives (good for worker agents in a loop).
- Harness hooks inject new inbox items at each turn (Claude Code hooks; similar hooks in other harnesses where they exist).
- PTY injection (below) for harnesses with neither.

### 2. A2A gateway

The hub exposes an A2A endpoint and publishes an Agent Card per agent, so external A2A-speaking agents can talk to network agents and the other way round.

### 3. PTY wrapper (fallback)

`myceld` starts any CLI harness in a pseudo-terminal inside the sandbox, writes incoming messages as input, and parses output into messages. It's lossy, but it makes any terminal agent joinable.

### Agent lifecycle

`spawn_agent` or the UI asks a node to create a sandbox from a profile, install the harness from a harness registry (a small manifest per harness: install command, MCP config template, hook support), inject model credentials from the vault, and attach it to channels. Agents can be ephemeral (per task) or persistent (snapshotted between runs).

### Orchestrator sessions

Each member works through one orchestrator: their own harness, started by them on their own machine, connected to the hub through the local MCP socket.

- The orchestrator is a trusted-host agent. It runs natively with the member's own login, subscription or API key, exactly as if Mycelium were not there.
- `spawn_agent` with target `self` starts worker agents on the same machine under the same login. Workers join the channels the orchestrator chooses.
- Other members' agents never start a harness on your machine with your login. They can post tasks to your orchestrator, which decides whether to take them, and they can run jobs in sandboxes on your machine through the brokers you share.
- Spawning a harness on someone else's machine is off in v1. If it is added later, it runs in a sandbox with the requester's own API key from the vault.
- Each member should check their model provider's terms on whether their agents may do work requested by teammates.

## Security, policy and control

Permissions belong to the sender's identity and its grants, never to whoever reads a message; everything an agent receives from another agent is untrusted input.

**Threats designed for**

- *Sandbox escape* → microVM boundary by default, brokers instead of sockets, no `trusted-host` without explicit opt-in.
- *Prompt injection between agents* → provenance on every message, `untrusted: true` on agent-originated content, adapters wrap it as quoted data, and no grant is ever widened because a message asked for it.
- *Confused deputy* → an agent acting on a request from another agent uses the requester's grants, not its own. A job spawned on behalf of `@intern` cannot use `@trainer`'s GPU lease.
- *Runaway loops and cost* → hop limits, per-agent and per-channel budgets, rate limits.
- *Stolen credentials* → short-lived agent tokens bound to their sandbox, node keys in hardware where available, secrets never in messages.
- *Data exfiltration* → default-deny egress, mounts by grant only, egress logs per sandbox.

**Policy engine.** In v1, rules are written in Go behind a Policy interface (roles, grants, channel policy, profile limits) and evaluated on the hub for every grant, spawn, post and broker call. Example: "agents in `#training-runs` may lease one GPU on `gpu-box` for up to 4 hours without approval; anything else needs the owner".

**Approvals.** Actions marked `requires_approval` (read-write mounts, GPU leases past a threshold, egress outside the allowlist, `trusted-host`) produce an approval card in `#approvals`, the UI and a push notification. Decisions are signed and audited.

**Kill switch.** Three levels: pause one agent, freeze a channel, freeze the whole network (all sandboxes paused, all brokers deny). Unfreezing needs the owner.

**Audit and replay.** Every message, grant, broker call and approval goes to the append-only `audit.>` stream with trace IDs, so any thread can be replayed step by step in the UI.

## UI, CLI and API

One API serves all three surfaces; the UI and the CLI are both thin clients over it.

**Every node has the UI.** The UI ships inside the `mycel` binary, so every node serves it locally at `http://localhost:7777` (a menu-bar or tray icon opens it). The interface is the same everywhere, but its rights depend on where it runs:

- **Network view (read-only on ordinary nodes):** every node, agent, channel, grant, approval and trace, streamed from the hub and filtered by what that node's owner may see. No changes to other nodes, network policy or other agents from here.
- **This machine (restrict only):** what this node shares, its sandboxes and the grants other agents hold on it. The owner can pause, revoke, lower limits or stop sharing. Widening what the node offers is done from the hub.
- **Hub UI (admin):** the only place for network-wide changes: grants and policies, sandbox profiles, channel settings, enrolling and removing nodes, spawning agents on other nodes, budgets, and the global freeze. The CLI can do the same with hub admin credentials.

**Node owner has the final say.** Settings made under "This machine" are a local ceiling the hub cannot exceed. If the owner turns off GPU sharing locally, a hub-side grant for that GPU is rejected by the node's own brokers. This also holds while the hub is unreachable. Changes sync back to the hub and are audited.

**Local sign-in.** Managing "This machine" is authorised by the local OS user through a token readable only by that user, so no network credentials are needed to lock down your own machine. That token is scoped to `network:read` and `node:self:restrict`. Network-wide changes are only possible from the hub UI, or the CLI with hub admin credentials.

**Offline.** If the hub is unreachable, the local UI keeps working for "This machine" and shows the network view as last known, marked stale.

**API.** Connect-RPC (gRPC-compatible, also plain HTTP/JSON) for commands, plus a WebSocket stream for live events. Auth for humans by passkey; API keys scoped like grants.

**CLI (`mycel`).**

```
mycel token create --name gpu-box --caps docker,ollama,gpu --ttl 15m
mycel nodes ls
mycel agent spawn claude-code --node mac --profile dev --channel '#backend' --task "fix flaky auth test"
mycel post '#backend' "@reviewer please check PR 42"
mycel job run --profile trainer --gpu 1 -- python train.py
mycel grants ls --pending && mycel approve grt_01J…
mycel freeze --all
```

**UI views**

- *Constellation:* a 3D map (3D library, custom look). Nodes are planets sized by capacity, sandboxes are moons, agents glow by state, and messages travel as light along the edges. Click anything to inspect it. Gamified elements (agent XP, streaks, achievements) are an optional overlay, not the default.
- *Channels:* chat-style view of every channel and thread, with humans able to post and mention agents.
- *Approvals:* the queue of pending grants and actions, one-tap approve or deny.
- *Sandbox inspector:* live terminal, files, grants, resource graphs, egress log.
- *Traces and cost:* per thread and per agent: tokens, money, latency, GPU-hours.
- *Command bar:* `⌘K` for everything, including natural-language commands routed to a built-in operator agent.

**Mobile.** A PWA first, for approvals and push notifications; native later if needed.

### Visual direction and UI constraints

The look is a restrained HUD with a quiet space feel: it should read as designed by hand, not generated, and never cheap.

- **Feel:** dark, precise instrument panel. Thin hairline strokes, generous negative space, one accent colour plus one warning colour, real typography (a good grotesque plus a monospace for data), numbers that tick rather than jump.
- **Space touch:** the network view is a calm star map: nodes as bodies, agents as satellites, messages as faint moving particles along their links. A barely moving starfield and depth, no glowing neon, no lens flares, no gradients for their own sake.
- **Motion:** every animation means something (a message sent, a grant attached, a node going offline). Elements assemble on entry rather than fading in. Reduced-motion setting turns it off.
- **Avoid:** default component-library looks, purple gradients, glassmorphism everywhere, emoji, rounded "SaaS" cards, sci-fi decoration that carries no information.
- **Themes:** the visual layer is a swappable skin over one data layer, so a gamified theme can be added later without touching data or logic.
- **Hard constraints:** static build embedded in the `mycel` binary; served on localhost by every node; the 3D or canvas scene loads as a separate chunk only when the network view opens; usable without the 3D view (lists and tables for every object); works on a laptop screen and on a phone for approvals.
- **Framework:** only the styled layer is custom: an own design system (tokens, type, colour, HUD primitives such as frames, readouts, gauges and lists) and own visual components. No pre-styled component kits or themes. The base is fixed: React with TypeScript, built with Vite as a static single-page app; Radix or Base UI for headless accessible primitives; three.js through react-three-fiber for the network view. Further libraries that do unstyled base work (animation, graph layout, state) are welcome; each one and the reason go in `docs/decisions.md`.

## Tech stack and repo layout

Go for the hub, daemon and CLI (single static binaries, NATS embeds natively, easy cross-compiling), TypeScript for the UI.

| Layer | Choice | Why |
| --- | --- | --- |
| Hub, `myceld`, CLI | Go | A single binary, `mycel`, with hub, daemon and CLI as roles; NATS, Firecracker SDK and `tsnet` are Go libraries |
| Bus | NATS JetStream, embedded in the hub | Pub/sub, request-reply, persistence, per-subject ACLs |
| State | SQLite (Postgres optional) + JetStream for events | Zero-ops self-hosting |
| Policy | Go rules behind an interface; Cedar later | Declarative, testable rules |
| Sandboxes | Firecracker, Kata/Cloud Hypervisor, gVisor; Apple Virtualization.framework on Mac | See Sandbox model |
| API | Connect-RPC + WebSocket | Typed, works from browsers |
| UI | React + TypeScript + Vite, Radix or Base UI, three.js via react-three-fiber, with an own design system on top; static build embedded in the binary | Own look without rebuilding solved base work |
| Data path (later) | Tailscale `tsnet` or WireGuard | Direct node-to-node transfer for large artifacts |

### Footprint

The node install stays light because the UI is static files inside the binary and sandbox images download only when a profile first needs them. Figures are rough estimates, to be measured once built.

| Part | Download | Idle RAM | Notes |
| --- | --- | --- | --- |
| `mycel` binary (daemon, CLI, brokers, adapters) | ~25–40 MB | ~20–40 MB | Go, statically linked. Embedding Tailscale `tsnet` alone adds roughly 15 MB, so keep it optional |
| Embedded web UI | ~1–3 MB inside the binary | 0 until opened | Static build; the 3D scene loads as a separate chunk only for the 3D view |
| Hub role extras | Included | +~50–150 MB | Embedded NATS JetStream and SQLite; grows with retained history |
| Sandbox runtime | ~5–50 MB | ~5 MB overhead per microVM + guest RAM | Firecracker is tiny; gVisor and Kata are larger; Apple's VM tooling comes with macOS |
| Sandbox images | ~30 MB minimal to 1 GB+ for toolchain or CUDA images | — | Fetched lazily per profile and cached; the biggest real cost |

A chat-only machine that never runs sandboxes pays only for the binary.

```
mycelium/
├── cmd/
│   └── mycel/            # the one binary: daemon, hub, CLI subcommands
├── internal/
│   ├── hub/              # control-plane role
│   ├── daemon/           # node role (myceld)
│   ├── cli/              # command-line client
│   ├── bus/              # NATS embedding, subjects, ACLs
│   ├── identity/         # CA, tokens, mTLS, SPIFFE IDs
│   ├── policy/           # policy engine bindings
│   ├── sandbox/          # backend interface + firecracker/, gvisor/, kata/, applevf/
│   ├── broker/           # docker/, ollama/, gpu/, egress/
│   ├── sharing/          # per-node share rules, idle detection, local-first scheduling
│   ├── channels/         # channels, threads, blackboard, summaries
│   └── adapters/         # mcp/, a2a/, pty/
├── proto/                # API and envelope definitions
├── profiles/             # default sandbox profiles (YAML)
├── harnesses/            # harness manifests (claude-code, codex, gemini-cli, …)
├── web/                  # UI (embedded in the binary, served by every node)
└── docs/
```

## Implementation plan

An implementing agent can build v1 by working through these phases in order; each phase ends with acceptance tests that must pass before the next starts.

### Rules for the implementing agent

- Read the whole spec first. "Decisions for v1" is fixed.
- Work phase by phase. A phase is done only when its acceptance tests pass in CI on every listed OS.
- When the spec is silent, pick the simplest option that keeps later phases possible, record it in `docs/decisions.md` (context, choice, reason) and continue without asking.
- Stop and ask the owner only for: anything that weakens a security rule in this spec, anything that costs money, publishing anything publicly, or changing an item in "Decisions for v1".
- Never mount the host Docker socket into a sandbox, never run network-originated work outside a sandbox, never log secrets.
- Keep `README.md` current: install, join and first channel in under five minutes of reading.

### How the build is run: orchestrator and Opus agents

The top-level agent is an orchestrator that plans, delegates, reviews and integrates; Claude Opus subagents write the code.

- **Orchestrator owns:** the phase plan, the split into work packages, `proto/` and all shared interfaces, `docs/decisions.md`, `docs/progress.md`, merging, and running the full test suite. It writes little feature code itself and never marks a phase done without green acceptance tests.
- **Contracts first:** at the start of each phase the interfaces and protobuf definitions for that phase are written and merged before any parallel work begins. Implementation agents may not change shared contracts; they ask the orchestrator for a change.
- **Work package brief:** every implementation agent gets its goal, the spec sections that apply, the interfaces it implements or may call, the packages and files it owns, the acceptance tests it must make pass, and what it must not touch.
- **Implementation agents:** one Opus subagent per work package, each in its own git worktree and branch. Packages that don't touch the same files run in parallel.
- **Review:** every package is reviewed before merge by a separate Opus subagent that did not write it, against the brief, the security rules and the tests. Identity, sandbox, broker and policy packages also get a second, security-focused review.
- **Integration:** after each merge the orchestrator runs all tests. A failure goes back to the agent that wrote the package, with the failing output.
- **Resumable:** `docs/progress.md` lists each phase, its packages, their status and open issues, so a fresh orchestrator session can pick up where the last one stopped.
- **Dogfooding (optional):** once phase 2 works, the orchestrator may coordinate its subagents through a Mycelium channel.

### Conventions

- Go (latest stable), one module; `golangci-lint`; `go test ./...`; table-driven tests.
- API and envelope in `proto/`, generated with `buf`, served with Connect-RPC.
- One config file per node in the OS's standard config directory; data in the OS's standard data directory.
- Platform code behind build tags (`darwin`, `windows`, `linux`). CI matrix on GitHub Actions: `macos-latest`, `windows-latest`, `ubuntu-latest`.
- Releases with GoReleaser: binaries for all three platforms plus `install.sh` (macOS, Linux) and `install.ps1` (Windows).
- Structured logs with `log/slog`; every privileged action also goes to the audit stream.

### Test strategy

- **Unit tests** per package.
- **Multi-node on one machine:** the end-to-end harness starts one hub and several nodes as separate processes with their own data directories and ports. All acceptance tests run this way in CI.
- **Fake sandbox backend:** runs commands in a temporary directory without isolation so logic can be tested in CI. It refuses to start outside test builds. Real backends are tested on the matching CI runner where possible, and by hand on GPU machines.
- **Scripted fake harness:** an MCP client that follows a script stands in for real agents in CI. Each phase ends with a manual smoke test using real Claude Code.

### Phases

1. **Skeleton and identity.** Single binary with subcommands (`hub init`, `join`, `daemon`, `token`, `nodes`, `ui`); CA, join tokens, mTLS, agent tokens, revocation; node inventory; users with admin and member roles. *Done when:* a hub and two nodes join with tokens; a reused or expired token is refused; a revoked node is disconnected within 5 seconds; `mycel nodes ls` shows both nodes with inventory.
2. **Bus, channels and MCP adapter.** Embedded NATS, the envelope, channels, threads and direct messages, inboxes, subscriptions, blackboard; MCP tools for discovery, talking, receiving and the blackboard; Claude Code hook integration. *Done when:* two fake harnesses on two nodes complete a scripted task in a task channel; the hop limit stops a ping-pong loop; by hand, two real Claude Code sessions on two machines exchange messages.
3. **Orchestrator and local workers.** `spawn_agent` with target `self`; harness manifests (Claude Code first, then Codex and Gemini CLI); agent lifecycle; budgets per agent and per channel. *Done when:* an orchestrator spawns two local workers that join a channel and report results; an exhausted budget pauses them.
4. **Sandboxes and jobs.** Sandbox interface; backends in this order: macOS (Apple Virtualization.framework), Windows (WSL2 with gVisor), Linux (gVisor, then Firecracker); `default` and `dev` profiles; egress proxy with allowlist; workspace and artifacts; nested Docker in `dev`; `run_job` and node selection. *Done when:* `mycel job run --profile dev -- <test command>` started on a Mac runs on a Windows node, and the other way round; tests assert that a job cannot read the host's home directory, cannot reach a domain outside the allowlist, and cannot see host processes.
5. **Brokers, grants and approvals.** Ollama broker, GPU leases, brokered host Docker, `request_grant`, approvals, the three kill-switch levels, the node's local ceiling, sharing settings (schedule, idle-only, local first). *Done when:* an agent requests a GPU, an admin approves it in the hub UI and the lease expires on its own; a locally tightened setting rejects a hub grant even while the hub is offline; freezing everything stops all sandboxes within 5 seconds.
6. **UI.** One app for hub admin UI and node read-only UI, with rights taken from token scopes; all views from "UI views"; the visual direction above. *Done when:* every object is visible and searchable; every admin action available in the CLI is also available in the hub UI; an API test proves a node UI token cannot change anything outside its own machine; the initial load is under 1 MB excluding the 3D chunk.
7. **Team rollout.** Hub tunnel, mDNS discovery, hub export and import, install scripts, quickstart, a demo script. *Done when:* a fresh Mac and a fresh Windows machine get from nothing to joined and running a shared job by following the README in under 10 minutes.

**First usable milestone.** Phases 1–5 with the CLI and a minimal hub UI (lists and approvals) already make Mycelium useful for a team; the full HUD can follow.

**Later, not v1:** A2A gateway, PTY adapter, browser broker, channel bridges, spawning harnesses on other members' machines, Linux GPU passthrough, a declarative policy engine, a gamified theme.

### Definition of done for v1

- All acceptance tests pass on macOS, Windows and Linux CI runners.
- `docs/` contains this spec, the decisions log, security notes (threat model including what is not covered) and the quickstart.
- No code comment marked `TODO(security)` remains.

### Open items for the owner (not blocking)

- Check that "Mycelium" and `mycel` are free enough: GitHub, Go module path, domain, trademarks.
- Pick the PolyForm variant, and set up a simple contributor agreement before others contribute code.
