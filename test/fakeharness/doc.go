// Package fakeharness is the scripted stand-in for a real agent in CI: an MCP
// client that starts `mycel mcp --agent <name>` as a subprocess (stdio) and
// runs a script of tool calls with simple assertions.
//
// Contract (owned by the orchestrator):
//
//	type Step struct {
//	    Tool   string            // MCP tool name
//	    Args   map[string]any    // tool arguments
//	    Expect string            // substring that must appear in the tool result text ("" = any)
//	    Store  string            // store the result text under this name for later {{name}} substitution
//	    Repeat int               // run this step up to Repeat times until Expect matches (for waits)
//	    Usage  *StepUsage        // phase 3, optional: {"input":N,"output":M,"usd":X} reported by
//	                             // `mycel fakeharness` as stream-json when the script runs as a worker
//	}
//	func Run(ctx context.Context, bin string, env []string, agent, harness string, script []Step) (Transcript, error)
//
// Transcript records every call and result so a test can assert on the
// whole conversation. Scripts can also be loaded from JSON files.
package fakeharness
