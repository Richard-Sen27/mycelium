# Decisions log

Each entry: context, choice, reason. Newest at the bottom. Items in the spec's
"Decisions for v1" are not repeated here.

## D001 Module path

- Context: the repo's remote is `github.com/Richard-Sen27/mycelium`.
- Choice: module path `github.com/richard-sen27/mycelium` (lowercase).
- Reason: Go module paths are case-sensitive and lowercase is conventional; GitHub resolves either case. Change it before publishing if the repo moves.

## D002 Node ↔ hub transport is NATS over mTLS from phase 1

- Context: the spec says a node opens one outbound QUIC/WebSocket connection and that the bus is embedded NATS (phase 2). Building a separate WebSocket transport in phase 1 and replacing it in phase 2 means two transports.
- Choice: the hub embeds the NATS server from phase 1. Nodes connect outbound to it over TLS (TCP) with their node certificate as the client certificate. Hub-to-node control and node-to-hub telemetry/RPC ride on NATS subjects with per-identity permissions. Phase 2 adds JetStream streams, the envelope and channels on the same connection.
- Reason: one connection, built-in reconnect, request-reply and per-subject ACLs. NATS also has a WebSocket listener that can be switched on later for hostile networks without changing any client code.

## D003 Generated code lives in `gen/` and is committed

- Context: `proto/` holds definitions; CI on three OSes should not need buf or protoc plugins to build.
- Choice: `buf generate` writes Go code to `gen/mycel/v1` (protobuf) and `gen/mycel/v1/mycelv1connect` (Connect). Generated files are committed; a lint job regenerates and fails on drift.
- Reason: `go build ./...` works from a clean checkout on every runner.

## D004 SQLite via `modernc.org/sqlite`

- Context: the hub needs zero-ops state; cross-compiling with CGO is painful.
- Choice: pure-Go SQLite driver `modernc.org/sqlite`.
- Reason: static binaries for all three platforms without a C toolchain.

## D005 Join token format

- Context: the token must carry the hub address and the CA fingerprint, be single-use and short-lived.
- Choice: `mcl1_` + base64url(protobuf `JoinTokenPayload{hub_url, token_id, secret, ca_fingerprint}`). The hub stores only `sha256(secret)`, the name, caps ceiling, owner, expiry and used-at.
- Reason: self-describing, versioned, copy-pasteable; the secret never sits in the database in clear.

## D006 One daemon command, roles from data directory

- Context: "roles are configuration, never a different install"; the hub machine is also a node.
- Choice: `mycel daemon` starts every role whose state exists under the data directory: `hub/` (created by `mycel hub init`) and `node/` (created by `mycel join`). `mycel hub init` also enrols the local machine as a node, signed in-process, so the hub machine is a node without a network round trip.
- Reason: one process per machine, one command to run it, and the hub laptop is a node for free.

## D007 Identity material and directories

- Context: one config file per node in the OS config dir; data in the OS data dir.
- Choice: config at `<UserConfigDir>/mycel/config.yaml`, data at `<UserDataDir>/mycel/` where `UserDataDir` is `~/Library/Application Support` (macOS), `%LOCALAPPDATA%` (Windows), `$XDG_DATA_HOME` or `~/.local/share` (Linux). `MYCEL_HOME=<dir>` overrides both to `<dir>/config.yaml` and `<dir>/data/`, which the multi-process tests use. Keys are ECDSA P-256 PEM files with mode 0600. Secure Enclave and TPM backing is a later addition behind the same `identity` API.
- Reason: standard locations, trivial isolation for tests, and a single override.

## D008 Ports

- Context: every node serves the UI on `localhost:7777`; the hub also serves its API and the bus.
- Choice: hub API + UI on `:7777` (HTTPS, hub CA-signed server cert), bus (NATS TLS) on `:7778`. Ordinary nodes serve the local UI on `127.0.0.1:7777` over plain HTTP. All ports are flags.
- Reason: one well-known UI port everywhere; the bus port is only ever reached by `mycel` itself and is returned by the enrol call, so nobody types it.

## D009 Human and CLI authentication in phase 1

- Context: the spec wants passkeys for humans in the UI and scoped API keys; phase 1 has only a CLI.
- Choice: API keys (`mck_` prefix, stored hashed) bound to a user; `mycel hub init` creates the first admin and an admin key; `mycel user add` creates members with their own key. Passkeys come with the UI phase. Keys carry a scope list; phase 1 keys have the single scope `user`, meaning "act as this user with their role".
- Reason: the smallest thing that gives admin and member roles real meaning before the UI exists.

## D010 Node UI port on the hub machine

- Context: the hub API and UI bind `:7777`; the node role's local UI defaults to `127.0.0.1:7777`. Both roles run in one process on the hub machine and cannot share the port on Linux.
- Choice: when the hub role is present, the node role's local UI defaults to `127.0.0.1:7779` unless `--local-addr` is given. The hub UI on `7777` is the admin UI; phase 6 makes its "This machine" panel read the node status from `7779`.
- Reason: no port juggling for ordinary nodes, and no coupling of the hub server to daemon state.

## D011 Identity package details

- Context: the spec fixes SPIFFE-style node IDs, 30-day node certs and 1-hour agent tokens but not the encodings.
- Choice: ECDSA P-256 everywhere (P-384 keys are also accepted on load); node certs carry exactly one URI SAN and no DNS/IP SANs; `VerifyNodeCert` requires ExtKeyUsage ClientAuth and rejects CA leaves; issued certificates never outlive the CA and TTLs are capped at the package constants; join tokens require an `https` hub URL without userinfo, query or fragment; agent tokens are `mca1_` + base64url(JSON claims) + `.` + base64url(HMAC-SHA256 over prefix and payload), capped at `AgentTokenLifetime`, with `AgentID` fixed to `agent:<name>@<node>` so the ID can never name another node; token strings are rejected if they contain any byte outside the base64url alphabet.
- Reason: smallest format set that is strict to parse, tamper-evident and short-lived by construction.

## D012 Bus hardening details

- Context: D002 puts NATS on the node connection; the spec leaves listener details open.
- Choice: node permissions are split so no subject is both publishable and subscribable by a node (publish `node.<n>.telemetry` and `node.<n>.rpc.>`, subscribe `node.<n>.ctl` and `_INBOX.<n>.>`), because NATS grants a client a one-shot publish right to any reply subject it receives, which an echoed self-message would turn into publish-anywhere; the hub refuses node RPCs whose reply subject fails `bus.ValidReply`; CONNECT fields `nkey`, `jwt`, `sig`, `user`, `pass` (and `token` over TLS) are refused so a connection cannot hide from `ConnectedNodes`/`Disconnect`; a connection's deadline is its certificate's NotAfter; limits are MaxConn 4096, MaxSubs 512, MaxPending 8 MiB, MaxPayload 1 MiB, JetStream 256 MiB memory and 8 GiB store by default; TLS 1.3 minimum on both ends; the hub's in-process connection authenticates with a random per-process token, is user `hub` with no restrictions, and uses inbox prefix `_INBOX._hub` (node names cannot contain `_`); JetStream always has an explicit store directory; nats-server runs with debug and trace logging off so CONNECT payloads are never logged; a node whose first connect is refused with an authorization error does not retry (a refused certificate will not become valid), while network and TLS errors retry with 0.5 s to 5 s backoff; the revocation key is the certificate serial as lowercase hex without leading zeros (`SerialNumber.Text(16)`), identical in `identity.SerialHex` and the bus.
- Reason: no plaintext path to the bus, no token in logs, and a revoked node stops hammering the hub.

## D013 CLI conventions

- Context: the spec names the commands but not the exit codes or output rules.
- Choice: exit 1 for errors, 2 for usage errors, 3 when the daemon stopped because the hub revoked this node; `--json` output is one compact line; the hub URL must be `https`; config.yaml is written with mode 0600 and never receives values that came from flags or the environment; `hub init` and `join` fill it, `login` fetches `/ca.pem` and pins the exact certificate it fingerprinted.
- Reason: scripts and the e2e harness can rely on exit codes and single-line JSON.

## D014 Hub store and API details

- Context: the spec fixes roles, tokens and revocation semantics but not ids, error codes or storage shape.
- Choice: SQLite with a single open connection (serialises writes, no SQLITE_BUSY); join token rows use the token's own 32-hex id; agent ids are `agent:<name>@<node>`; a `node_certs` table records every serial issued to a node and `RemoveNode` revokes all unexpired ones (renewal leaves old certificates valid until expiry); revocation by SPIFFE ID lasts until the same name is enrolled again, revocation by serial is permanent; the token burn is a conditional UPDATE checked by rows affected, so two enrolments racing on one token cannot both succeed; user names follow the node-name rule; `RemoveUser` also revokes the user's unused join tokens, refuses to remove the last admin or an owner of nodes (FailedPrecondition); `CreateJoinToken` for an existing node name returns AlreadyExists; unknown policy actions are denied even for admins; node RPC replies over the bus carry a NATS header `Mycel-Status: ok|error` and on error the payload is a `CtlResponse`, because protobuf does not reliably fail when decoding the wrong message type; `Init` cleans up what it created on failure.
- Reason: deterministic, race-free enrolment and revocation with the smallest schema that still answers "which certificates may this node still use".

## D015 Node daemon details

- Context: the spec fixes the join flow, 30-day certs with auto-rotation and the local UI port, not the mechanics.
- Choice: the hub URL used by a node must be `https` (override or token); renewal reuses the node key and replaces only `node.crt` atomically, then opens a new bus connection before closing the old one (connections carry a generation number so stale callbacks are ignored); heartbeats carry the full inventory after every (re)connect and on every inventory change, otherwise only the hash; a node whose certificate has expired reports `state: expired`, keeps its identity and tells the owner to join again; revocation (by control message, by refused reconnect, or by a "revoked" renewal answer) renames `node.yaml` and deletes the key and certificate; the local status server binds loopback only, rejects non-loopback `Host` headers and sets a strict CSP; `/api/status` adds `caps`.
- Reason: rotation without a gap in control delivery, and no way for a stale or forged event to wipe a healthy node.

## D016 Phase 2 design: messages, inboxes, local API and the MCP adapter

- Context: the spec fixes the envelope, the subjects, the MCP tool names and the hop/rate limits, not how messages reach an agent or how a harness reaches `myceld`.
- Choice:
  - SQLite is the system of record for channels, messages (with a hub-wide `seq`), subscriptions and blackboards; every accepted message is also published to the JetStream stream `CH` on `ch.<name>.msg` for replay, and audit entries to `AUDIT` on `audit.<action>`.
  - Inboxes are pull-based with a cursor: `agent.inbox.pull(after_seq, wait_ms)` long-polls on the hub, which filters by membership, sender and subscription. No push subject, so node permissions stay exactly as in D012. The node daemon owns the cursor per local agent.
  - Agent RPCs are the bus methods listed in `internal/bus`; each carries the agent token and the hub binds it to the node in the subject.
  - Harnesses reach `myceld` through `mycel mcp --agent <name>` (MCP on stdio, official Go SDK), which calls the daemon's loopback local API with the local sign-in token from `<NodeDir>/local.token`. The daemon registers the agent with the hub as a trusted-host agent and keeps the agent token; the MCP process only ever holds an opaque session id.
  - `mycel hook --agent <name>` prints new inbox messages for harness hooks (Claude Code `UserPromptSubmit`); `mycel agent setup <harness> --agent <name>` writes the harness config from `harnesses/<name>.yaml`.
  - hop = parent.hop + 1 along `reply_to`; a reply inherits or starts a thread; exceeding `max_hops` (default 8) refuses the post and the hub posts an EVENT to the channel. Rate limit 60 messages per minute per agent per channel by default.
  - Direct messages are channels named `dm:<id1>:<id2>` (sorted ids), created on first post. `#general` (topic, open) and `#alerts` (system) exist from init. Agents may create `#task-…` channels by joining them and may add members to task channels they created.
  - The late-joiner summary is a plain-text digest of the channel, blackboard and last 20 messages; no model is involved in v1.
  - Messages handed to a harness are wrapped as quoted untrusted data (`channels.QuoteForAgent`).
- Reason: pull with cursors is restart-safe and needs no new bus permissions; stdio MCP is the one-line harness integration the spec asks for; keeping agent tokens inside the daemon means a harness process never holds a network credential.

## D017 Local API, MCP adapter and CLI details

- Context: D016 fixes the shape; these are the small choices made while building it.
- Choice: hub RPC error texts start with the connect code name (`permission_denied: …`) so the daemon can map them to HTTP codes for local clients, and `unauthenticated` from the hub becomes 403 locally (401 is reserved for the local token); the local RPC path accepts the method with or without the `agent.` prefix; registering an agent name that already has a live session returns that session (409 if the harness differs); concurrent inbox pulls for one agent are serialised so a batch is delivered once; a hub refusal that names a revoked agent ends the local session immediately; `mycel hook` reuses the agent's live session or opens a short-lived one, and always exits 0; `agent setup` restricts agent names to `[a-z0-9-]` because the name is placed in a shell command, and refuses to rewrite a config file that is not valid JSON; the MCP adapter wraps summaries and blackboard values in the same quoted untrusted block as messages; channel names given without a prefix get `#`; `ChannelPolicy` booleans `agents_may_post`/`humans_may_post` cannot express "false" on a topic channel in v1 because zero means default (a known limitation to fix with optional fields when the proto next changes).
- Reason: keep the harness-facing surface forgiving and the trust boundary (local token, agent token inside the daemon) strict.

## D018 Channel semantics on the hub

- Context: details the spec leaves open in "Channels".
- Choice: users may read open and system channels without joining, and never direct channels they are not part of, except hub admins, who may read, list and watch every channel because audit and replay need it (each admin read of a direct channel they are not in is audited); direct channels have no owner and only admins may change their policy or archive them; agents must join; `#alerts` is a system channel open to members where only the hub posts; `#general` and `#alerts` cannot be archived; inboxes deliver only messages posted after the agent joined (`joined_seq`), history comes from `ListMessages` and the join summary; `next_seq` jumps to the hub's max seq whenever a scan runs out before `limit`; the hop-limit EVENT is posted once per thread with its hop set to `max_hops` so replying to it cannot restart a loop; the rate limit applies to humans and agents alike (the hub is exempt); the `observer` role may read but not post; a direct message channel is created on the first post and the other participant is always mentioned; `approval_decision` messages are refused from clients; mentions must resolve to an existing user or a live member agent; blackboard CAS conflicts return `aborted`, at most 1024 keys per channel; agent RPCs run on their own goroutines (at most 1024, at most 4 long polls per agent); JetStream `CH` is 4 GiB and `AUDIT` 2 GiB, oldest entries dropped, SQLite stays the full record; task channels created by agents are `open_to_members` so other agents can join by name (there is no agent-side AddMember RPC in v1); `max_hops` ≤ 100 and `rate_per_minute` ≤ 6000; per-agent caps: 64 channels created, 120 messages per minute across all channels, 16 concurrent long polls and 64 concurrent RPCs per node; per-owner caps: 256 channels, 16 MiB of blackboard; 8 Watch streams per user; the hop-limit EVENT is deduplicated by a database check and hub events are capped at 10 per channel per minute; a reply always inherits its parent's thread.
- Reason: predictable delivery for late joiners, loops that cannot be restarted from inside, and bounded hub resources per agent.
