# Progress

Status board for the orchestrator. A fresh session reads this first, then `docs/decisions.md`.

Legend: `todo` · `contracts` · `in progress` · `review` · `merged` · `done`

## Phase 1 — Skeleton and identity

Status: contracts

| Package | Owns | Status | Notes |
| --- | --- | --- | --- |
| P1-identity | `internal/identity` | todo | CA, join tokens, agent tokens, keys, CSR, SPIFFE IDs |
| P1-bus | `internal/bus` | todo | embedded NATS, mTLS auth, per-node permissions, disconnect |
| P1-hub | `internal/hub`, `internal/policy` | todo | store, enrol, Connect API, registry, revocation |
| P1-daemon | `internal/daemon`, `internal/inventory` | todo | join, connect, inventory, heartbeat, renewal, local status |
| P1-cli | `internal/cli`, `cmd/mycel` | todo | subcommands |
| P1-e2e | `test/e2e` | todo | multi-process acceptance tests |

Acceptance (from spec): a hub and two nodes join with tokens; a reused or expired token is refused; a revoked node is disconnected within 5 seconds; `mycel nodes ls` shows both nodes with inventory.

Open issues: none.

## Phase 2 — Bus, channels and MCP adapter

Status: todo

## Phase 3 — Orchestrator and local workers

Status: todo

## Phase 4 — Sandboxes and jobs

Status: todo

## Phase 5 — Brokers, grants and approvals

Status: todo

## Phase 6 — UI

Status: todo

## Phase 7 — Team rollout

Status: todo
