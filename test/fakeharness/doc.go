// Package fakeharness is the scripted stand-in for a real agent in CI: an MCP
// client that starts `mycel mcp --agent <name>` as a subprocess (stdio) and
// runs a script of tool calls with simple assertions.
//
// API:
//
//	type Step struct {
//	    Tool        string         // MCP tool name; "" with SleepMS only pauses
//	    Args        map[string]any // arguments; {{name}}, {{name|msgid}}, {{name|msgid:<substring>}},
//	                               // {{name|id:<prefix>}}
//	    Expect      string         // substring the result must contain ("" = any)
//	    ExpectAll   []string       // each must appear in some result of the step
//	    ExpectError bool           // the step must end in a tool error containing Expect
//	    Store       string         // keep the result text under this name
//	    Repeat      int            // retry up to Repeat times until the step is satisfied
//	    AllowError  bool           // errors and mismatches do not fail the script
//	    SleepMS     int            // with Tool "": pause
//	    Usage       *StepUsage     // usage `mycel fakeharness` reports for the step (stream-json)
//	}
//	func Run(ctx, bin string, env []string, agent, harness string, script []Step) (Transcript, error)
//	func RunObserved(ctx, bin, env, agent, harness, script, observe func(Call)) (Transcript, error)
//	func RunSession(ctx, cs *mcp.ClientSession, script []Step, observe func(Call)) (Transcript, error)
//	func LoadScript(path string) ([]Step, error)
//	func ChildEnv(extra ...string) []string
//
// {{name|msgid}} takes the id from the quoted message header that precedes
// the first occurrence of the storing step's Expect (or of <substring> with
// the msgid:<substring> form), else the first msg_ id in the text.
//
// {{name|id:<prefix>}} takes the first word of the stored result that starts
// with <prefix> followed by letters and digits, e.g. {{run|id:job_}} or
// {{exp|id:art_}}.
//
// The child environment is ChildEnv(env...): the parent's without MYCEL_*
// variables, GORACE with halt_on_error=1, then env. MYCEL_LOCAL_ADDR in env
// becomes --local-addr; MYCEL_FAKE_WORKDIR in env is the child's working
// directory; MYCEL_FAKE_TRANSCRIPT_DIR in env makes every run
// write its transcript to <dir>/<agent>.txt, which is how tests read the
// transcripts of workers that a node daemon started with `mycel
// fakeharness`.
//
// Transcript records every call and result so a test can assert on the
// whole conversation. Scripts can also be loaded from JSON files.
package fakeharness
