//go:build fakeharness

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/test/fakeharness"
)

// The hidden `mycel fakeharness` command (build tag fakeharness) is the
// harness the node daemon starts for harnesses/fake.yaml: it runs a scripted
// MCP session against this binary's `mycel mcp` and prints a Claude Code
// stream-json subset, so the daemon's usage parser is exercised in CI.

func init() {
	extraCommands = append(extraCommands, (*app).fakeHarnessCmd)
}

// fakeHarnessName is the --harness the scripted session registers with.
const fakeHarnessName = "fake"

// runFakeScript runs the script; tests replace it.
var runFakeScript = fakeharness.RunObserved

func (a *app) fakeHarnessCmd() *cobra.Command {
	var script, agent, prompt string
	cmd := &cobra.Command{
		Use:    "fakeharness --script <file> --agent <name> [--prompt <text>]",
		Short:  "Run a scripted fake harness (CI only)",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if script == "" || agent == "" {
				return usagef("--script and --agent are required")
			}
			steps, err := fakeharness.LoadScript(script)
			if err != nil {
				return err
			}
			if prompt != "" {
				_, _ = fmt.Fprintf(a.stderr, "fakeharness %s: prompt (ignored):\n%s\n", agent, prompt)
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			return a.runFakeHarness(cmd.Context(), bin, agent, steps)
		},
	}
	f := cmd.Flags()
	f.StringVar(&script, "script", "", "JSON script of tool calls (required)")
	f.StringVar(&agent, "agent", "", "agent name (required)")
	f.StringVar(&prompt, "prompt", "", "the rendered worker prompt; only logged")
	return cmd
}

// runFakeHarness runs steps as agent and prints stream-json to stdout. A
// failed script prints its transcript to stderr and exits 1.
func (a *app) runFakeHarness(ctx context.Context, bin, agent string, steps []fakeharness.Step) error {
	em := &streamEmitter{w: a.stdout, steps: steps, last: -1}
	tr, runErr := runFakeScript(ctx, bin, mycelEnv(os.Environ()), agent, fakeHarnessName, steps, em.observe)
	if runErr != nil {
		// Transcript first, so it is complete before the result line ends the run.
		_, _ = fmt.Fprintf(a.stderr, "%s\n", tr.String())
	}
	if err := em.result(runErr != nil); err != nil {
		return err
	}
	if runErr != nil {
		return exitCodeError{code: exitError, err: runErr}
	}
	return nil
}

// mycelEnv returns the MYCEL_* entries of env: the scripted session's `mycel
// mcp` must reach the same daemon (MYCEL_HOME, MYCEL_LOCAL_ADDR).
// fakeharness.Run adds them to the rest of the inherited environment.
func mycelEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "MYCEL_") {
			out = append(out, kv)
		}
	}
	return out
}

// streamEmitter prints one assistant line per executed tool step and a final
// result line, in Claude Code's stream-json shape. Sleep steps (Tool "")
// make no tool call and therefore emit no assistant line and no usage.
type streamEmitter struct {
	w     io.Writer
	steps []fakeharness.Step
	last  int // index of the last step printed
	turns int // assistant lines printed
	in    int
	out   int
	usd   float64
	err   error
}

type streamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type streamAssistant struct {
	Type    string `json:"type"`
	Message struct {
		Usage streamUsage `json:"usage"`
	} `json:"message"`
}

type streamResult struct {
	Type         string      `json:"type"`
	Subtype      string      `json:"subtype"` // "success" or "error"
	IsError      bool        `json:"is_error"`
	NumTurns     int         `json:"num_turns"`
	TotalCostUSD float64     `json:"total_cost_usd"`
	Usage        streamUsage `json:"usage"`
}

// observe is called after every tool call; repeated calls of one step print
// once.
func (e *streamEmitter) observe(c fakeharness.Call) {
	if c.Step == e.last || c.Step < 0 || c.Step >= len(e.steps) {
		return
	}
	e.last = c.Step
	var u fakeharness.StepUsage
	if p := e.steps[c.Step].Usage; p != nil {
		u = *p
	}
	e.in += u.Input
	e.out += u.Output
	e.usd += u.USD
	e.turns++
	line := streamAssistant{Type: "assistant"}
	line.Message.Usage = streamUsage{InputTokens: u.Input, OutputTokens: u.Output}
	e.write(line)
}

func (e *streamEmitter) result(failed bool) error {
	sub := "success"
	if failed {
		sub = "error"
	}
	e.write(streamResult{Type: "result", Subtype: sub, IsError: failed, NumTurns: e.turns,
		TotalCostUSD: e.usd, Usage: streamUsage{InputTokens: e.in, OutputTokens: e.out}})
	return e.err
}

func (e *streamEmitter) write(v any) {
	if e.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err == nil {
		_, err = fmt.Fprintf(e.w, "%s\n", b)
	}
	e.err = err
}
