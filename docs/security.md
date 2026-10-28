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

## Phase 5

- Grants are hub records with a kind-specific spec, a subject (agent or job), one node and an expiry. Requests come from the subject's own agent or from a human; a channel named in a request is only honoured when it is the subject's home channel, a job's channel, or a membership added by the channel owner, an admin or the spawner, never a self-join. Auto rules without an explicit kind never match egress or docker-host; read-write mounts need `read_only: false` spelled out. Everything else is PENDING until an admin or the owner of the target node approves; approval re-checks freezes, the subject's state and the node's current offers inside the lock, can only shorten the duration and never changes the spec.
- `#approvals` is hub-only: agents can neither join nor read it, and the card carries id, kind, subject, node, duration and requester, never a reason text or a path. Both approval message types can only be posted by the hub; decisions and expiries reach the requester by direct message. Approvals are audited but not cryptographically signed (the spec says "signed"; open item for after v1).
- Attachment goes hub → node by `AttachGrant`; the node validates again (node name, subject, expiry, mounts via ResolveMount and once more at sandbox creation, egress rules), refuses anything its local ceiling does not offer and reports `grant.revoked`. A node refusal revokes the grant on the hub. Returning nodes are re-sent every attach and detach; nodes expire grants on their own clock, also across sleep, and remember local revocations until the hub acknowledges them.
- Brokers are per-grant credentials, not sockets. Ollama: one opaque token per grant, model allowlist, requests per minute, `max_tokens` enforced by clamping `num_predict` and cutting streams, in-flight and connection caps, slow-body timeouts, aborted streams still accounted once. GPU: leases per device, exclusive or time-sliced, detach kills processes still on the device except the daemon's own tree and never while a neighbour still holds a time-slice. Docker: every create and exec body is decoded into typed structures and rebuilt from allowed fields only (so case-variant or duplicate keys cannot smuggle `Privileged`), `NetworkMode` is always `none`, no port bindings, no host paths beyond the grant's mounts, capped CPU/memory/pids/containers under a per-grant cgroup parent, `DeviceRequests` only with a gpu grant and only the leased ids, labels for sweep, a pull rate limit, and the socket directory must be a real directory owned by the daemon user. The host `docker.sock` is never mounted anywhere.
- Sandboxes reach brokers only through the egress proxy: per-sandbox aliases (`ollama.mycel.internal:80`, `docker.mycel.internal:2375`) map to the broker's loopback listener, so the allowlist stays default-deny and no guest routing is needed.
- The node owner's ceiling: `PUT /api/local/sharing` may only tighten relative to the settings the hub last sent, is persisted locally, enforced by admission and every broker while the hub is unreachable, and reported back with `node.sharing`. Removing an offer detaches open grants of that kind at once. A detector that cannot tell whether the owner is idle reports an explicit unknown state: `when: idle` then stays inactive and `local_first` pauses nothing, so headless nodes are not stuck.
- Kill switch: an operator pause (phase 3), a channel freeze by its owner or an admin (agents homed there pause, posts are refused), and the network freeze, which is admin-only in both directions: the hub stops dispatching queued jobs, refuses posts, jobs, grant requests and approvals, pauses every agent and sends `Freeze` ahead of anything else in every node's control queue; nodes stop sandboxes and workers (SIGSTOP; a no-op on Windows), brokers answer 503, and the state is persisted so a restart with the hub down stays frozen. An operator pause survives an unfreeze. The hub re-sends the current freeze state on every full-inventory heartbeat, so a node that missed an unfreeze recovers.
- The approvals page uses HttpOnly, Secure, SameSite=Strict cookies, a per-session CSRF token, an Origin and `Sec-Fetch-Site` check, a login rate limit and at most 8 sessions per user; the API key travels only in the login POST body.
- Boundaries worth knowing: every process of the node's OS user holds the local token and can read any local broker credential (the local token is the same-user boundary); a job's wall clock keeps counting while it is paused by `local_first` or a freeze; the Docker and GPU brokers are tested against fakes only (no engine or GPU on the development machine); MIG slice handling is a best guess until run on real hardware.

