package daemon

// Local API (phase 2). Served by the node daemon on LocalAddr next to the
// status endpoints, loopback only, for processes of the same OS user: the MCP
// adapter (`mycel mcp`), the Claude Code hook (`mycel hook`) and the local
// UI. It is the "local sign-in" of the spec.
//
// Authentication: at every start the daemon writes <NodeDir>/local.token
// (0600, 32 random bytes base64url). Clients send `Authorization: Bearer
// <token>`. Phase 2 scope of the token: network:read, node:self:restrict,
// agent:local.
//
// Endpoints (JSON; proto messages use protojson with proto field names):
//
//	POST   /api/local/agents                  {"name":"coder","harness":"claude-code"}
//	       -> 201 {"agent_id":"agent:coder@<node>","session":"<opaque>"}
//	       Registers a trusted-host agent with the hub (agent.register with
//	       sandbox ""), keeps its token inside the daemon (never returned),
//	       refreshes it in the background. Registering a name that already
//	       has a live session returns the same session (idempotent).
//	GET    /api/local/agents                  -> {"agents":[{"agent_id","session","harness","since"}]}
//	DELETE /api/local/agents/{session}        -> 204 (stops the session; the hub agent row stays)
//	POST   /api/local/agents/{session}/rpc/{method}
//	       Body: the proto request for the method WITHOUT its `call` field.
//	       The daemon fills AgentCall{token}, sends it on
//	       bus.NodeRPC(node, method) and returns the reply payload as JSON.
//	       Allowed methods: the bus.RPCAgent* constants, given either in full
//	       ("agent.post") or without the "agent." prefix ("post",
//	       "channel.join", "channel.leave", "channel.list", "list", "whoami",
//	       "inbox.pull", "bb.get", "bb.put", "messages.list"). Anything else -> 404.
//	       For inbox.pull the daemon owns the cursor: it ignores a client
//	       after_seq of 0, substitutes the stored cursor for that agent
//	       (persisted in <NodeDir>/agents/<name>.json), and advances it to
//	       next_seq after a successful reply. A client may pass an explicit
//	       after_seq to re-read. wait_ms is capped at 60000 and the request
//	       timeout is wait_ms + 5 s.
//	       On Mycel-Status "error": 400/403/404/409/412/429/500 chosen from the
//	       error text's leading connect code name when present (an
//	       "unauthenticated:" hub reply maps to 403, never 401), body
//	       {"error":"..."}. 401 only when the local token is missing or wrong. 503 while the bus is
//	       not connected (except inbox.pull with wait, which waits up to
//	       wait_ms for the connection).
//
// Phase 3: local workers (spawn_agent with target self).
//
//	POST   /api/local/agents/{session}/spawn
//	       {"harness":"claude-code","name":"w1"?,"task":"...","channel":"#task-x",
//	        "profile":"trusted-host","workdir":"..."?,"max_turns":30?,
//	        "budget":{"tokens":0,"usd":0}?,"env":{...}?}
//	       -> 201 {"agent_id":"agent:w1@<node>","worker":"agent:w1@<node>"}
//	       Only profile "trusted-host" in phase 3 (anything else -> 400 "sandboxed
//	       workers arrive with phase 4"). The daemon registers the worker with
//	       the hub (RegisterAgentRequest.parent = the session's agent,
//	       home_channel, budget, workdir), writes the worker's MCP config under
//	       <NodeDir>/workers/<name>/mcp.json, renders the harness manifest's
//	       prompt template, starts the harness process from the manifest's
//	       spawn section with the worker's own `mycel mcp` session, parses its
//	       stdout for usage (manifest `usage` parser) and reports increments
//	       with bus.RPCAgentUsage, reports bus.RPCAgentExited when it ends,
//	       and applies AgentCtl pause/resume/stop from the hub (pause =
//	       SIGSTOP on unix, best effort on windows, plus refusing the worker's
//	       local RPCs; stop = kill). At most 8 running workers per node.
//	       A name is generated as <parent name>-w<N> when omitted.
//	GET    /api/local/workers -> {"workers":[{"agent_id","parent","harness","state","pid","started_at","exited_at","exit_code","usage":{"tokens","usd","turns"}}]}
//	DELETE /api/local/workers/{agent}  -> 204 (stops it); only the spawning session or the local UI token
//
// Phase 4: artifacts for trusted-host agents. Job RPCs (agent.job.*,
// agent.artifact.list) are proxied like every other agent RPC.
//
//	POST   /api/local/agents/{session}/artifacts?name=<name>[&channel=<name>]
//	       body = bytes (≤ 256 MiB) -> 201 Artifact JSON. The daemon uploads
//	       to the hub with its node certificate (PUT /api/artifacts) on behalf
//	       of the agent; the artifact's created_by is the agent id and its
//	       owner the node owner.
//	GET    /api/local/agents/{session}/artifacts/{id} -> bytes (hub GET with
//	       the node certificate; the hub applies the agent's visibility).
//
// Phase 5: the node owner's local controls ("This machine": restrict only).
//
//	GET    /api/local/sharing   -> {"hub": SharingSettings, "local": SharingSettings, "effective": SharingSettings}
//	PUT    /api/local/sharing   body SharingSettings -> the new effective settings. The
//	       local settings are a ceiling: a PUT may only tighten relative to the
//	       hub-side settings (fewer offers, narrower windows, idle-only, lower
//	       max_jobs, local_first on); anything wider -> 400. Stored in
//	       <NodeDir>/sharing.yaml and enforced by the brokers and the job
//	       runner even while the hub is unreachable; reported to the hub with
//	       node.sharing.
//	GET    /api/local/grants    -> {"grants":[Grant…]} (protojson; each Grant as attached on
//	       this node; an extra top-level "endpoints": {"<grant id>": "<endpoint>"} map
//	       gives the broker endpoint per grant)
//	DELETE /api/local/grants/{id} -> 204; revoke locally (broker detaches at once; the
//	       hub is told with grant.revoked and marks it REVOKED by "node:<name>")
//	GET    /api/local/freeze    -> {"frozen": bool, "scope": "...", "reason": "...", "since": "..."}
//	PUT    /api/local/sharing   replies with the bare effective SharingSettings (protojson).
//	GET    /api/local/agents/{session}/grants -> {"grants":[Grant…],"endpoints":{…},"credentials":{"<grant id>":"<token>"}}
//	       (the subject's own session only; the only route that reveals a broker
//	       credential, see docs/security.md: the local token is the same-user boundary)
//	GET    /api/local/sharing also carries "idle": "unknown" | "<duration>" (the detector's view).
//
// Phase 6: the node UI (spec "Every node has the UI", "Local sign-in", "Offline").
//
//	GET    /                     the embedded single-page app (internal/web.Handler) on the
//	       loopback local address; every non-/api path falls back to index.html.
//	GET    /api/ui-config        (no auth) {"mode":"node","node":"<name>","hub":"<hub url>","version":"..."}
//	ANY    /api/hub/{path...}    (local token) forwards the request to the hub at /{path}
//	       over the node's mTLS client certificate with the header
//	       `Mycel-Scope: network:read`, streaming request and response bodies
//	       (Connect unary and server-stream calls, including EventService.WatchEvents,
//	       and GET /api/artifacts/{id}); only the hub's read-only methods are
//	       forwarded (the same allowlist as internal/hub/uiapi.go, checked here
//	       too: anything else -> 403); the browser's cookies and Authorization
//	       header are never forwarded; 502 {"error":"hub unreachable"} when the
//	       hub cannot be reached, so the UI shows its last known network view
//	       marked stale.
//
//	Sign-in: `mycel ui` reads <NodeDir>/local.token and opens
//	http://<local addr>/#local=<token>; the app moves the token into
//	sessionStorage, strips the fragment and sends it as the local bearer. The
//	token is scoped to network:read (via the proxy) and node:self:restrict (the
//	phase 2–5 local routes), exactly the rights of the local API.
//
// Every local request is logged at debug; agent tokens never appear in logs.

// LocalTokenFile is the local sign-in token file name under NodeDir.
const LocalTokenFile = "local.token"

// AgentsDir holds per-agent local state (inbox cursors) under NodeDir.
const AgentsDir = "agents"
