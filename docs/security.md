# Security notes

Threat model and what is not covered. Grows with each phase; see docs/spec.md
"Security, policy and control" for the designed-for threats.

## Phase 1

Covered:

- Node identity by mTLS. Private keys are generated on the node and never leave it.
- Join tokens are single-use and short-lived; the hub stores only a hash of the secret.
- The join token carries the hub CA fingerprint; the node verifies the CA before trusting anything.
- Revocation: a removed node is dropped from the bus within seconds and its certificate is refused on reconnect and on renewal.
- Agent tokens are short-lived, HMAC-signed, and bound to the node (and sandbox) they were issued to; a token copied elsewhere does not verify against another connection.
- API keys are stored hashed. Role checks go through the Policy interface and default to deny.
- Per-node NATS permissions: a node can only publish telemetry and RPC requests under its own name and only subscribe to its own control subject and inbox; publish and subscribe never overlap on a subject (reply-permission escalation, see D012). The hub validates the reply subject of every node RPC.

Not covered yet:

- Hardware-backed node keys (Secure Enclave, TPM): planned behind the identity API.
- On Windows, file modes are not applied; `local.token`, keys and the hub database rely on the ACL of the per-user config and data directories.
- Passkeys for humans: phase 6.
- Rate limiting of the enrol endpoint is a simple per-IP token bucket; there is no global limit yet.
- Revoking an agent takes effect at its next token refresh: an already issued agent token stays cryptographically valid until it expires (at most one hour). Removing a node drops its bus connection immediately, which is what carries agent traffic.
- Audit, revoked-serial and issued-certificate tables are not pruned beyond expired serials at startup.
- An individual API key cannot be revoked yet; removing the user revokes all of their keys (a `RevokeApiKey` RPC comes with the phase 2 contracts).
- Hub TLS certificates from Let's Encrypt: phase 7. Until then the hub serves a CA-signed server certificate and clients pin the CA.

## Phase 2

Covered:

- Sender identity, sequence, timestamp, hop and provenance on every message are stamped by the hub; agent-originated content is marked untrusted and adapters hand it to harnesses as quoted data with header fields sanitised so a message cannot close its own quote.
- Agent tokens are verified on every RPC and bound to the node connection in the subject; a token from another node is refused.
- Membership and policy checks on every read and write; direct channels are visible only to their participants and to admins (audited).
- Loop and cost controls: hop count along the reply chain with a per-thread limit, rate limits per sender and channel and per agent overall, caps on channels, blackboard size, long polls and RPC concurrency per node, Watch streams per user.

Not covered yet:

- A loop can restart by posting in the same thread without `reply_to` (hop returns to zero); only the rate limits bound it. Budgets (phase 3) add a cost ceiling.
- Human messages from other team members carry `untrusted: false`; the adapter shows the sender so a harness can tell owners apart, but there is no consent step before another member adds your agent to a channel or messages it.
- Audit rows for refused requests are sampled per actor; the audit table and streams are not pruned.

## Phase 3

Covered:

- Workers run as the member's own user on their own machine only; the hub refuses a parent on another node; nobody's login is used for another member's harness.
- A worker's only network credential is the agent token held inside the daemon; the worker process gets a local session id and the local token.
- Spawn requests carry no environment; the daemon's environment is inherited and `MYCEL_HOME`/`MYCEL_LOCAL_ADDR` are set last. Manifest `{{env:NAME}}` placeholders are limited to `MYCEL_FAKE_*`.
- Budgets pause rather than kill, usage never decreases, an operator pause survives exits, and a worker's home channel and parent cannot change after the first registration.

Not covered yet:

- A node that under-reports or never reports usage is not detected; usage is self-reported by the node that runs the harness.
- Claude Code workers are limited to the mycel tools; Codex workers run with workspace-write and no approvals inside a git repository, so they can edit files and run commands there.
- On Windows, workers are placed in a Job Object so stop and shutdown kill the whole tree, but this path has not run on a Windows machine yet.
- Child processes that detach from the worker's process group escape pause, stop and kill.
- Stop ends the current run only; a stopped worker may be registered again by its node. Revoke is the lasting removal.
- Any process of the node's OS user holds the local token and can read every broker credential of local agents; the local token is the same-user boundary, not a per-agent one.

## Phase 4

Covered:

- Every sandbox starts empty; the workspace is seeded by copy; mounts are validated against a deny list and symlink-resolved; the host Docker socket is never mounted.
- Egress only through the node's proxy with a per-sandbox credential; names are resolved once and dialled exactly; name-resolved loopback, private, carrier-NAT, unique-local, translated and node-own addresses are refused unless an IP is listed literally; every attempt is audited with bytes.
- Jobs are bound to the node identity they were assigned to; a removed or revoked node's jobs fail at once, and a re-enrolled name inherits nothing.
- A job's result message in a channel obeys the same posting rules as the submitter and is marked untrusted.
- Artifacts are owner-scoped with channel visibility, served as attachments with nosniff, quota- and concurrency-limited; nodes can only report on, upload for and fetch inputs of their own assigned jobs.

Not covered yet:

- Any member's job can run on any node with the `sandbox` capability, and that node's owner can read the job's environment, inputs and workspace: job environments are not secrets until phase 5 adds sharing settings and the vault.
- Nested Docker is unavailable inside gVisor-based sandboxes (Linux, Windows); phase 5's brokered host Docker covers those nodes.
- The Linux and Windows backends have only been compile-checked; their firewall and cgroup behaviour must be verified on real machines. The gVisor path needs root or CAP_NET_ADMIN plus CAP_SYS_ADMIN on the node; a fixed-verb root helper is the planned narrowing.
- Guest helper binaries inside gVisor sandboxes come from the image, so a workload can trojan them; CopyOut and Glob output is treated as untrusted.
- Child processes that detach from a fake-backend job's process group escape it (the fake backend has no isolation by design).
- On macOS the sandbox firewall is a single in-guest layer: a guest kernel local-root exploit could flush it and then reach the host's loopback services through the Lima gateway. Mitigations in place: unprivileged user namespaces disabled, no sudo, no DNS, pinned images; the images need periodic refreshing because they receive no upgrades at first boot.

