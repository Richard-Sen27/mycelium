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

Status: in progress (round 1: hub, daemon spawn, MCP/CLI)

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P3-hub | `internal/hub`, `internal/policy` | in progress | agent lifecycle, budgets, usage accounting, pause/resume/stop, AgentService phase 3 methods, paused-agent RPC refusals, EVENTs |
| P3-daemon-spawn | `internal/daemon` | in progress | spawn endpoint, worker processes, usage parsers, AgentCtl, workers listing |
| P3-mcp-cli | `internal/adapters/mcp`, `internal/cli`, `harnesses` | in progress | spawn_agent/list_workers/stop_agent tools, `mycel agents` lifecycle commands, spawn sections for claude-code, codex, gemini-cli, fake manifest, `mycel fakeharness` (build tag) |
| P3-e2e | `test/**` | todo | orchestrator spawns two workers; exhausted budget pauses them; also folds in the phase 2 test review fixes (bus refusal check, race reports from children, explicit ping-pong chain, #alerts reason, goroutine and port hygiene) |

Acceptance (from spec): an orchestrator spawns two local workers that join a channel and report results; an exhausted budget pauses them.

Open issues: none.

## Phase 4 — Sandboxes and jobs

Status: todo

## Phase 5 — Brokers, grants and approvals

Status: todo

## Phase 6 — UI

Status: todo

## Phase 7 — Team rollout

Status: todo
