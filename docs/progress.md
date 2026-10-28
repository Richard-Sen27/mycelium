# Progress

Status board for the orchestrator. A fresh session reads this first, then `docs/decisions.md`.

Legend: `todo` · `contracts` · `in progress` · `review` · `merged` · `done`

## Phase 1 — Skeleton and identity

Status: done locally (all packages merged, acceptance tests green on macOS; CI on Linux and Windows runners pending the first push, which is the owner's call)

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P1-identity | `internal/identity` | merged | CA, join tokens, agent tokens, keys, CSR, SPIFFE IDs |
| P1-bus | `internal/bus` | merged | embedded NATS, mTLS auth, per-node permissions, disconnect |
| P1-hub | `internal/hub`, `internal/policy` | merged | store, enrol, Connect API, registry, revocation |
| P1-daemon | `internal/daemon`, `internal/inventory` | merged | join, connect, inventory, heartbeat, renewal, local status |
| P1-cli | `internal/cli`, `cmd/mycel` | merged | subcommands |
| P1-e2e | `test/e2e` | merged | multi-process acceptance tests |

Acceptance (from spec): a hub and two nodes join with tokens; a reused or expired token is refused; a revoked node is disconnected within 5 seconds; `mycel nodes ls` shows both nodes with inventory.

Open issues: CI matrix not yet exercised (repo never pushed). Manual smoke test with real Claude Code on two machines not yet done (needs a second machine). e2e review items to fold into P2-e2e: build the binary with -race when the test runs with -race; assert the hub's "invalid or expired token" text in the three refusal tests; tamper the token by re-encoding a flipped secret byte; prove end to end that a removed node's certificate is refused by the hub (bus and Renew) within 5 s; allocate all free ports before releasing any.

## Phase 2 — Bus, channels and MCP adapter

Status: done locally (all packages merged, 13 acceptance tests green on macOS; CI matrix and the two-machine Claude Code exchange pending the owner)

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P2-channels | `internal/channels`, `internal/hub` (channels, messages, inbox, blackboard, ChannelService, agent RPCs, JetStream CH/AUDIT, #general/#alerts, policy actions) | merged | the largest package |
| P2-daemon-local | `internal/daemon` (local token, local API, agent sessions, RPC proxy, cursors) | merged | |
| P2-mcp | `internal/adapters/mcp`, `internal/cli` (mcp, hook, agent setup, channels, post, read, bb) | merged | official MCP Go SDK |
| P2-e2e | `test/fakeharness`, `test/e2e` phase 2 tests | merged | after the three above |

Acceptance (from spec): two fake harnesses on two nodes complete a scripted task in a task channel; the hop limit stops a ping-pong loop; by hand, two real Claude Code sessions on two machines exchange messages.

Open issues: none.

## Phase 3 — Orchestrator and local workers

Status: done locally (all packages merged, 3 phase-3 acceptance tests green on macOS plus the hardened phase 1–2 suite; CI matrix pending the owner)

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P3-hub | `internal/hub`, `internal/policy` | in progress | agent lifecycle, budgets, usage accounting, pause/resume/stop, AgentService phase 3 methods, paused-agent RPC refusals, EVENTs |
| P3-daemon-spawn | `internal/daemon` | in progress | spawn endpoint, worker processes, usage parsers, AgentCtl, workers listing |
| P3-mcp-cli | `internal/adapters/mcp`, `internal/cli`, `harnesses` | in progress | spawn_agent/list_workers/stop_agent tools, `mycel agents` lifecycle commands, spawn sections for claude-code, codex, gemini-cli, fake manifest, `mycel fakeharness` (build tag) |
| P3-e2e | `test/**` | merged | orchestrator spawns two workers; exhausted budget pauses them; also folds in the phase 2 test review fixes (bus refusal check, race reports from children, explicit ping-pong chain, #alerts reason, goroutine and port hygiene) |

Acceptance (from spec): an orchestrator spawns two local workers that join a channel and report results; an exhausted budget pauses them.

Open issues: none.

## Phase 4 — Sandboxes and jobs

Status: done locally (all packages merged; 6 fake-backend acceptance tests green on macOS, the Lima isolation suite green on this Mac; Linux and Windows backends compile-checked only; the cross-machine Mac↔Windows run needs a second machine)

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P4-sandbox-core | `internal/sandbox` (Select, limits, mounts, runner), `internal/sandbox/fake`, `profiles/` loader | merged | round 1 |
| P4-egress | `internal/broker/egress` | merged | round 1 |
| P4-jobs-hub | `internal/hub`, `internal/policy` (profiles, JobService, ArtifactService, scheduler, logs, node RPCs, agent RPCs) | merged | round 1 |
| P4-mcp-cli | `internal/adapters/mcp`, `internal/cli` (run_job, artifacts tools; job/artifacts/profiles commands) | merged | round 1 |
| P4-daemon-jobs | `internal/daemon` (CreateSandbox/KillSandbox, runner integration, egress proxy, artifact transfer, local artifact endpoints) | merged | round 2 |
| P4-applevf | `internal/sandbox/applevf` (Lima) | merged | both Lima integration tests (default profile, nested rootless Docker) pass on this Mac |
| P4-wsl2 | `internal/sandbox/wsl2` (same agent as gvisor) | merged | untested here: no Windows machine |
| P4-gvisor | `internal/sandbox/gvisor` (+ firecracker stub) | merged | untested here: no Linux machine |
| P4-e2e | `test/**` | merged | round 3; fake backend in CI, real backend isolation tests behind an env flag |

Acceptance (from spec): `mycel job run --profile dev -- <cmd>` started on a Mac runs on a Windows node and the other way round; tests assert a job cannot read the host's home directory, cannot reach a domain outside the allowlist, and cannot see host processes.

Open issues: the cross-platform acceptance needs a second machine; CI runners for real backends are not set up.

## Phase 5 — Brokers, grants and approvals

Status: in progress (round 1: hub grants, brokers, sharing, MCP/CLI)

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P5-hub-grants | `internal/hub`, `internal/policy` (grants, approvals, auto rules, sharing, freezes, approvals page, agent/node RPCs) | in progress | round 1 |
| P5-brokers | `internal/broker/ollama`, `internal/broker/gpu`, `internal/broker/docker` | in progress | round 1 |
| P5-sharing | `internal/sharing` (settings, effective computation, idle detectors per OS) | in progress | round 1 |
| P5-mcp-cli | `internal/adapters/mcp`, `internal/cli` (request_grant, grants/approve/deny/freeze/share commands) | in progress | round 1 |
| P5-daemon | `internal/daemon` (attach/detach, brokers lifecycle, ceiling enforcement, freeze, local controls, sandbox wiring of broker endpoints) | todo | round 2 |
| P5-e2e | `test/**` | todo | round 3 |

Acceptance (from spec): an agent requests a GPU, an admin approves it in the hub UI and the lease expires on its own; a locally tightened setting rejects a hub grant even while the hub is offline; freezing everything stops all sandboxes within 5 seconds.

Open issues: no GPU on this Mac; GPU broker tests use a fake NVML/nvidia-smi.

## Phase 6 — UI

Status: todo

## Phase 7 — Team rollout

Status: todo
