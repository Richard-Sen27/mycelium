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
// Every local request is logged at debug; agent tokens never appear in logs.

// LocalTokenFile is the local sign-in token file name under NodeDir.
const LocalTokenFile = "local.token"

// AgentsDir holds per-agent local state (inbox cursors) under NodeDir.
const AgentsDir = "agents"
