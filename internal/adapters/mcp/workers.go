package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Phase 3: orchestrator sessions start local workers (spawn_agent with target
// self). The daemon does the work; these tools only shape requests and render
// replies.

const workerNote = " Workers run on this machine, natively, under your user's own login and model credentials (profile trusted-host, the only profile in this phase; sandboxed workers come later)."

func (t *tools) addWorkerTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "spawn_agent",
		Description: "Start a worker agent: a new headless session of a harness (e.g. claude-code) that joins channel, works on task, posts progress there and reports a result mentioning you. " +
			"You must have joined channel yourself first (join_channel); otherwise the hub refuses the spawn and its error is returned." +
			workerNote + " A claude-code worker may only use the Mycelium tools; a codex worker can also edit files and run commands in its workdir (which must be a git repository), without network access." +
			" workdir defaults to your own working directory. Optional limits: max_turns, budget_tokens, budget_usd (exhausting a budget pauses the worker). Returns the worker's agent id."}, t.spawnAgent)
	sdk.AddTool(srv, &sdk.Tool{Name: "list_workers",
		Description: "List the workers you spawned on this machine, with state, process id and usage (tokens, USD, turns)." + workerNote}, t.listWorkers)
	sdk.AddTool(srv, &sdk.Tool{Name: "stop_agent",
		Description: "Stop one of your workers (its harness process is killed). Only workers you spawned can be stopped." + workerNote}, t.stopAgent)
}

type spawnArgs struct {
	Harness      string  `json:"harness" jsonschema:"harness to start, e.g. claude-code, codex or fake"`
	Task         string  `json:"task" jsonschema:"what the worker should do"`
	Channel      string  `json:"channel" jsonschema:"channel the worker joins and reports in, e.g. #task-4821-fix-auth"`
	Name         string  `json:"name,omitempty" jsonschema:"worker name (lowercase letters, digits, '-'); default <your name>-w<N>"`
	Profile      string  `json:"profile,omitempty" jsonschema:"worker profile; only trusted-host (the default) exists in this phase"`
	MaxTurns     int     `json:"max_turns,omitempty" jsonschema:"maximum harness turns; default from the harness manifest"`
	BudgetTokens int64   `json:"budget_tokens,omitempty" jsonschema:"token budget; 0 or omitted = unlimited"`
	BudgetUSD    float64 `json:"budget_usd,omitempty" jsonschema:"budget in US dollars; 0 or omitted = unlimited"`
	Workdir      string  `json:"workdir,omitempty" jsonschema:"existing working directory of the worker; default: your own working directory"`
}

type stopArgs struct {
	AgentID string `json:"agent_id" jsonschema:"the worker's agent id, e.g. agent:coder-w1@mac"`
}

func (t *tools) spawnAgent(ctx context.Context, _ *sdk.CallToolRequest, in spawnArgs) (*sdk.CallToolResult, any, error) {
	ch := NormalizeChannel(in.Channel)
	switch {
	case strings.TrimSpace(in.Harness) == "":
		return nil, nil, errors.New("harness is required")
	case CheckTask(in.Task) != nil:
		return nil, nil, CheckTask(in.Task)
	case ch == "":
		return nil, nil, errors.New("channel is required")
	case in.MaxTurns < 0 || in.BudgetTokens < 0 || in.BudgetUSD < 0:
		return nil, nil, errors.New("max_turns, budget_tokens and budget_usd must not be negative")
	}
	wd, err := ResolveWorkdir(in.Workdir)
	if err != nil {
		return nil, nil, err
	}
	if err := CheckSpawnBinary(in.Harness); err != nil {
		return nil, nil, err
	}
	req := SpawnRequest{
		Harness: in.Harness, Name: in.Name, Task: in.Task, Channel: ch,
		Profile: in.Profile, Workdir: wd, MaxTurns: in.MaxTurns,
	}
	if in.BudgetTokens > 0 || in.BudgetUSD > 0 {
		req.Budget = &SpawnBudget{Tokens: in.BudgetTokens, USD: in.BudgetUSD}
	}
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	res, err := t.c.Spawn(cctx, t.session, req)
	if err != nil {
		t.log.Debug("spawn failed", "err", err)
		return nil, nil, err
	}
	return text(fmt.Sprintf("spawned %s in %s (worker of %s)", dash(res.AgentID), sanitizeLine(ch), dash(t.me))), nil, nil
}

// myWorkers returns the node's workers whose parent is this session's agent.
func (t *tools) myWorkers(ctx context.Context) ([]Worker, error) {
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	all, err := t.c.Workers(cctx)
	if err != nil {
		return nil, err
	}
	var out []Worker
	for _, w := range all {
		if t.me != "" && w.Parent == t.me {
			out = append(out, w)
		}
	}
	return out, nil
}

func (t *tools) listWorkers(ctx context.Context, _ *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, any, error) {
	ws, err := t.myWorkers(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(ws) == 0 {
		return text("no workers"), nil, nil
	}
	lines := make([]string, 0, len(ws))
	for _, w := range ws {
		lines = append(lines, WorkerLine(w))
	}
	return text(strings.Join(lines, "\n")), nil, nil
}

func (t *tools) stopAgent(ctx context.Context, _ *sdk.CallToolRequest, in stopArgs) (*sdk.CallToolResult, any, error) {
	id := strings.TrimSpace(in.AgentID)
	if id == "" {
		return nil, nil, errors.New("agent_id is required")
	}
	ws, err := t.myWorkers(ctx)
	if err != nil {
		return nil, nil, err
	}
	target := ""
	for _, w := range ws {
		if w.AgentID == id || (!strings.Contains(id, "@") && AgentName(w.AgentID) == strings.TrimPrefix(id, "agent:")) {
			target = w.AgentID
			break
		}
	}
	if target == "" {
		return nil, nil, fmt.Errorf("%s is not one of your workers (see list_workers)", sanitizeLine(id))
	}
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if err := t.c.StopWorker(cctx, t.session, target); err != nil {
		return nil, nil, err
	}
	return text("stopped " + sanitizeLine(target)), nil, nil
}

// ResolveWorkdir returns dir ("" = the current directory) as an absolute
// path, or an error when it is not an existing directory.
func ResolveWorkdir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("workdir %s: %w", dir, err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("workdir %s is not an existing directory", abs)
	}
	return abs, nil
}

// executable is os.Executable; tests replace it.
var executable = os.Executable

// CheckSpawnBinary rejects spawning a codex worker when the mycel binary's
// path contains a single quote: codex.yaml passes {{mycel}} inside a TOML
// literal string ('…'), which cannot contain one. The daemon checks the same.
func CheckSpawnBinary(harness string) error {
	if harness != "codex" {
		return nil
	}
	exe, err := executable()
	if err != nil {
		return nil //nolint:nilerr // the daemon resolves its own path; nothing to check here
	}
	if strings.ContainsRune(exe, '\'') {
		return fmt.Errorf("cannot spawn a codex worker: the mycel binary path %q contains a single quote, which codex's TOML config override cannot hold; install mycel under a path without quotes", exe)
	}
	return nil
}

// WorkerLine renders a worker on one line with sanitised fields.
func WorkerLine(w Worker) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s state %s harness %s", dash(w.AgentID), dash(w.State), dash(w.Harness))
	if w.PID > 0 {
		fmt.Fprintf(&b, " pid %d", w.PID)
	}
	fmt.Fprintf(&b, " used %s turns %d", UsageText(w.Usage.Tokens, w.Usage.USD), w.Usage.Turns)
	if w.StartedAt != "" {
		fmt.Fprintf(&b, " started %s", sanitizeLine(w.StartedAt))
	}
	if w.ExitedAt != "" {
		fmt.Fprintf(&b, " exited %s code %d", sanitizeLine(w.ExitedAt), w.ExitCode)
	}
	return b.String()
}

// UsageText renders token and dollar usage as "1200 tokens $0.0123".
func UsageText(tokens int64, usd float64) string {
	return strconv.FormatInt(tokens, 10) + " tokens $" + strconv.FormatFloat(usd, 'f', 4, 64)
}
