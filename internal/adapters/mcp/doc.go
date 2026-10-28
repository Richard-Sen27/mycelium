// Package mcp is the primary agent adapter: an MCP server on stdio started as
// `mycel mcp --agent <name> [--harness <name>]`. It talks to the local node
// daemon's local API (internal/daemon/localapi.go) and never to the hub
// directly.
//
// Contract (owned by the orchestrator). Library: github.com/modelcontextprotocol/go-sdk
// v1.8.0. Tools, with JSON schemas derived from these Go argument structs:
//
//	whoami()                                    -> agent id, node, owner, hub, channels
//	list_agents(channel?)                       -> agents visible to the caller
//	list_channels()                             -> channels the agent may see
//	join_channel(channel, mentions_only?, types?, tags?)
//	                                            -> joins (creating "#task-…" channels when missing)
//	                                               and returns the late-joiner summary
//	leave_channel(channel)
//	post(channel, text, thread?, mentions?, reply_to?, type?, tags?)
//	                                            -> the posted message id, seq, thread, hop
//	read_inbox(limit?)                          -> new messages since the daemon's cursor, rendered
//	                                               with channels.QuoteForAgent; never blocks
//	wait_for_message(timeout_seconds?)          -> blocks up to timeout (default 30, max 60) until
//	                                               at least one message arrives; same rendering
//	read_messages(channel, limit?, thread?, before_seq?)
//	                                            -> channel history, rendered the same way
//	blackboard_get(channel, key?)               -> entries
//	blackboard_put(channel, key, value, expect_version?)
//	                                            -> the stored entry
//
// Every message handed to the harness is wrapped by channels.QuoteForAgent so
// it reads as quoted, untrusted data (spec: prompt injection between agents).
// Tool errors are returned as MCP tool errors with the hub's message; the
// adapter never retries a post. The adapter registers the agent on start and
// deletes its session on exit.
//
// `mycel hook --agent <name>` is the companion for harness hooks: it prints
// the new inbox messages (same rendering) to stdout, or nothing, and exits 0.
// For Claude Code it is wired as a UserPromptSubmit hook whose stdout is
// added as context (see harnesses/claude-code.yaml).
package mcp
