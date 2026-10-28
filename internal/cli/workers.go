package cli

// Phase 3: agent lifecycle, budgets and local workers.
//
//	mycel agents get|pause|resume|stop <id>
//	mycel agents budget <id> [--tokens N] [--usd X]
//	mycel channels usage|reset-usage <name>
//	mycel channels policy <name> [--budget-tokens N] [--budget-usd X] [--max-hops N] [--rate N] [--open]
//	mycel agent spawn <harness> --task ... --channel ... [--name] [--workdir] [--max-turns] [--budget-tokens] [--budget-usd]
//	mycel workers ls | stop <agent>

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	mcpadapter "github.com/richard-sen27/mycelium/internal/adapters/mcp"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// normAgentID accepts "agent:<name>@<node>" or "<name>@<node>".
func normAgentID(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "agent:") && strings.Contains(s, "@") {
		return "agent:" + s
	}
	return s
}

// budgetText renders a budget: "unlimited", "1000 tokens", "$2.5000" or both.
func budgetText(b *mycelv1.Budget) string {
	var parts []string
	if b.GetTokens() > 0 {
		parts = append(parts, strconv.FormatInt(b.GetTokens(), 10)+" tokens")
	}
	if b.GetUsd() > 0 {
		parts = append(parts, "$"+strconv.FormatFloat(b.GetUsd(), 'f', 4, 64))
	}
	if len(parts) == 0 {
		return "unlimited"
	}
	return strings.Join(parts, ", ")
}

// usedCell renders usage as "<tokens>/$<usd>", "-" when nothing was used.
func usedCell(b *mycelv1.Budget) string {
	if b.GetTokens() == 0 && b.GetUsd() == 0 {
		return "-"
	}
	return strconv.FormatInt(b.GetTokens(), 10) + "/$" + strconv.FormatFloat(b.GetUsd(), 'f', 4, 64)
}

func agentState(ag *mycelv1.Agent) string {
	s := dash(ag.GetState())
	if r := ag.GetPausedReason(); r != "" {
		s += " (" + r + ")"
	}
	return s
}

// ---- agents (hub API) ----

func (a *app) printAgent(ag *mycelv1.Agent) error {
	tw := newTable(a.stdout)
	kv := func(k, v string) { row(tw, k+":", v) }
	kv("ID", ag.GetId())
	kv("Node", dash(ag.GetNodeName()))
	kv("Owner", dash(ag.GetOwnerId()))
	kv("Harness", dash(ag.GetHarness()))
	kv("State", agentState(ag))
	kv("Parent", dash(ag.GetParent()))
	kv("Home channel", dash(ag.GetHomeChannel()))
	kv("Budget", budgetText(ag.GetBudget()))
	kv("Used", budgetText(ag.GetUsed()))
	kv("Turns", strconv.Itoa(int(ag.GetTurns())))
	kv("Sandbox", dash(ag.GetSandboxId()))
	kv("Created", dash(ag.GetCreatedAt()))
	if ag.GetExitedAt() != "" {
		kv("Exited", ag.GetExitedAt())
		kv("Exit code", strconv.Itoa(int(ag.GetExitCode())))
	}
	return tw.Flush()
}

func (a *app) agentsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show an agent with its parent, budget and usage",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.agents.GetAgent(cmd.Context(), connect.NewRequest(&mycelv1.GetAgentRequest{Id: normAgentID(args[0])}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			return a.printAgent(resp.Msg.GetAgent())
		},
	}
}

func (a *app) agentsBudgetCmd() *cobra.Command {
	var tokens int64
	var usd float64
	cmd := &cobra.Command{
		Use:   "budget <id> [--tokens N] [--usd X]",
		Short: "Set an agent's budget (0 = unlimited); flags not given keep their value",
		Long: "Set an agent's budget (0 = unlimited). The command reads the current budget (GetAgent), changes only\n" +
			"the flags given and writes the whole budget back (SetAgentBudget); this read-modify-write is not atomic,\n" +
			"so a concurrent change between the two calls is overwritten.",
		Args: exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			if !f.Changed("tokens") && !f.Changed("usd") {
				return usagef("give --tokens, --usd or both")
			}
			if tokens < 0 || usd < 0 {
				return usagef("--tokens and --usd must not be negative")
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			id := normAgentID(args[0])
			cur, err := hc.agents.GetAgent(cmd.Context(), connect.NewRequest(&mycelv1.GetAgentRequest{Id: id}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			b := &mycelv1.Budget{}
			if old := cur.Msg.GetAgent().GetBudget(); old != nil {
				b = proto.CloneOf(old)
			}
			if f.Changed("tokens") {
				b.Tokens = tokens
			}
			if f.Changed("usd") {
				b.Usd = usd
			}
			resp, err := hc.agents.SetAgentBudget(cmd.Context(), connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: id, Budget: b}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Budget of %s: %s (used %s).\n", cellText(id), budgetText(resp.Msg.GetAgent().GetBudget()), budgetText(resp.Msg.GetAgent().GetUsed()))
			return err
		},
	}
	cmd.Flags().Int64Var(&tokens, "tokens", 0, "token budget (0 = unlimited)")
	cmd.Flags().Float64Var(&usd, "usd", 0, "budget in US dollars (0 = unlimited)")
	return cmd
}

func (a *app) agentsPauseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "pause <id> [--reason text]",
		Short: "Pause an agent (kill switch level one)",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.agents.PauseAgent(cmd.Context(), connect.NewRequest(&mycelv1.PauseAgentRequest{Id: normAgentID(args[0]), Reason: reason}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Paused %s.\n", cellText(normAgentID(args[0])))
			return err
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (shown with the agent's state)")
	return cmd
}

func (a *app) agentsResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume an agent paused by an operator or a budget",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.agents.ResumeAgent(cmd.Context(), connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: normAgentID(args[0])}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Resumed %s.\n", cellText(normAgentID(args[0])))
			return err
		},
	}
}

func (a *app) agentsStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop an agent (a worker's harness process is killed)",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.agents.StopAgent(cmd.Context(), connect.NewRequest(&mycelv1.StopAgentRequest{Id: normAgentID(args[0])}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Stopped %s.\n", cellText(normAgentID(args[0])))
			return err
		},
	}
}

// ---- channel budgets and policy ----

func (a *app) printChannelUsage(u *mycelv1.ChannelUsage) error {
	tw := newTable(a.stdout)
	kv := func(k, v string) { row(tw, k+":", v) }
	kv("Channel", u.GetChannel())
	kv("Budget", budgetText(u.GetBudget()))
	kv("Used", budgetText(u.GetUsed()))
	kv("Paused", yesNo(u.GetPaused()))
	return tw.Flush()
}

func (a *app) channelsUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage <name>",
		Short: "Show a channel's budget and what its agents used",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.agents.GetChannelUsage(cmd.Context(), connect.NewRequest(&mycelv1.GetChannelUsageRequest{Channel: normChannel(args[0])}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			return a.printChannelUsage(resp.Msg.GetUsage())
		},
	}
}

func (a *app) channelsResetUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-usage <name>",
		Short: "Reset a channel's usage to zero (agents paused by its budget may resume)",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ch := normChannel(args[0])
			resp, err := hc.agents.ResetChannelUsage(cmd.Context(), connect.NewRequest(&mycelv1.ResetChannelUsageRequest{Channel: ch}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Reset the usage of %s.\n", cellText(ch))
			return err
		},
	}
}

func (a *app) channelsPolicyCmd() *cobra.Command {
	var budgetTokens int64
	var budgetUSD float64
	var maxHops, rate int32
	var open bool
	cmd := &cobra.Command{
		Use:   "policy <name> [--budget-tokens N] [--budget-usd X] [--max-hops N] [--rate N] [--open]",
		Short: "Change a channel's policy; flags not given keep their value",
		Long: "Change a channel's policy. The command reads the current policy (GetChannel), changes only the flags\n" +
			"given and writes the whole policy back (UpdateChannelPolicy); this read-modify-write is not atomic, so a\n" +
			"concurrent change between the two calls is overwritten.",
		Args: exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			changed := false
			for _, n := range []string{"budget-tokens", "budget-usd", "max-hops", "rate", "open"} {
				changed = changed || f.Changed(n)
			}
			if !changed {
				return usagef("give at least one of --budget-tokens, --budget-usd, --max-hops, --rate, --open")
			}
			if budgetTokens < 0 || budgetUSD < 0 || maxHops < 0 || rate < 0 {
				return usagef("--budget-tokens, --budget-usd, --max-hops and --rate must not be negative")
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ch := normChannel(args[0])
			cur, err := hc.chans.GetChannel(cmd.Context(), connect.NewRequest(&mycelv1.GetChannelRequest{Name: ch}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			p := &mycelv1.ChannelPolicy{}
			if old := cur.Msg.GetChannel().GetPolicy(); old != nil {
				p = proto.CloneOf(old)
			}
			if f.Changed("max-hops") {
				p.MaxHops = maxHops
			}
			if f.Changed("rate") {
				p.RatePerMinute = rate
			}
			if f.Changed("open") {
				p.OpenToMembers = open
			}
			if f.Changed("budget-tokens") || f.Changed("budget-usd") {
				if p.Budget == nil {
					p.Budget = &mycelv1.Budget{}
				}
				if f.Changed("budget-tokens") {
					p.Budget.Tokens = budgetTokens
				}
				if f.Changed("budget-usd") {
					p.Budget.Usd = budgetUSD
				}
			}
			resp, err := hc.chans.UpdateChannelPolicy(cmd.Context(), connect.NewRequest(&mycelv1.UpdateChannelPolicyRequest{Name: ch, Policy: p}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			np := resp.Msg.GetChannel().GetPolicy()
			_, err = fmt.Fprintf(a.stdout, "Updated the policy of %s: max hops %d, rate %d/min, open %s, budget %s.\n",
				cellText(ch), np.GetMaxHops(), np.GetRatePerMinute(), yesNo(np.GetOpenToMembers()), budgetText(np.GetBudget()))
			return err
		},
	}
	fl := cmd.Flags()
	fl.Int64Var(&budgetTokens, "budget-tokens", 0, "channel token budget (0 = unlimited)")
	fl.Float64Var(&budgetUSD, "budget-usd", 0, "channel budget in US dollars (0 = unlimited)")
	fl.Int32Var(&maxHops, "max-hops", 0, "longest reply chain allowed (0 = hub default, 8)")
	fl.Int32Var(&rate, "rate", 0, "messages per minute per agent (0 = hub default, 60)")
	fl.BoolVar(&open, "open", false, "any member may join without an invite (--open=false to close)")
	return cmd
}

// ---- local workers (node daemon local API) ----

// spawnSessionPrefix names the agent the CLI registers to spawn workers
// through ("cli-<pid>", unique per run); it becomes the workers' parent and
// is also the session's harness name.
const spawnSessionPrefix = "cli"

func spawnSessionName() string { return spawnSessionPrefix + "-" + strconv.Itoa(os.Getpid()) }

func (a *app) agentSpawnCmd() *cobra.Command {
	var task, channel, name, workdir, localAddr string
	var maxTurns int
	var budgetTokens int64
	var budgetUSD float64
	cmd := &cobra.Command{
		Use:   "spawn <harness> --task <text> --channel <name> [--name n] [--workdir dir] [--max-turns N] [--budget-tokens N] [--budget-usd X]",
		Short: "Start a worker agent on this machine under your own login (profile trusted-host)",
		Long: "Starts a headless harness session as a worker through the local node daemon. The worker joins the\n" +
			"channel, works on the task and reports there. Its parent is a short-lived agent \"" + spawnSessionPrefix + "-<pid>\" on this\n" +
			"node, which joins the channel first and is removed when the command ends. A codex worker needs a git\n" +
			"repository as its workdir.",
		Args: exactArgs(1, "harness"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(task) == "" || strings.TrimSpace(channel) == "" {
				return usagef("--task and --channel are required")
			}
			if err := mcpadapter.CheckTask(task); err != nil {
				return usageError{fmt.Errorf("--task: %w", err)}
			}
			if name != "" && !agentNameRe.MatchString(name) {
				return usagef("invalid --name %q: use lowercase letters, digits and '-' (at most 63)", name)
			}
			if maxTurns < 0 || budgetTokens < 0 || budgetUSD < 0 {
				return usagef("--max-turns, --budget-tokens and --budget-usd must not be negative")
			}
			wd, err := mcpadapter.ResolveWorkdir(workdir)
			if err != nil {
				return usageError{fmt.Errorf("--workdir: %w", err)}
			}
			if err := mcpadapter.CheckSpawnBinary(args[0]); err != nil {
				return err
			}
			u, tok, err := a.localTarget(cmd, localAddr)
			if err != nil {
				return err
			}
			c, err := mcpadapter.NewClient(u, tok)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			sessName := spawnSessionName()
			sess, err := c.Register(ctx, sessName, spawnSessionPrefix)
			if err != nil {
				return fmt.Errorf("register the %q session with the node daemon: %w", sessName, err)
			}
			defer func() {
				// Detached from ctx so an interrupt does not leak the session.
				dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := c.Delete(dctx, sess.Session); err != nil {
					a.log.Warn("delete the spawning session", "err", err)
				}
			}()
			ch := normChannel(channel)
			// The hub requires the parent to be a member of the worker's home channel.
			var joined mycelv1.AgentJoinChannelResponse
			if err := c.Call(ctx, sess.Session, bus.RPCAgentJoinChannel, &mycelv1.AgentJoinChannelRequest{Channel: ch}, &joined); err != nil {
				return fmt.Errorf("join %s as %s (the parent must be a member of the worker's channel): %w", ch, sess.AgentID, err)
			}
			req := mcpadapter.SpawnRequest{
				Harness: args[0], Name: name, Task: task, Channel: ch,
				Workdir: wd, MaxTurns: maxTurns,
			}
			if budgetTokens > 0 || budgetUSD > 0 {
				req.Budget = &mcpadapter.SpawnBudget{Tokens: budgetTokens, USD: budgetUSD}
			}
			res, err := c.Spawn(ctx, sess.Session, req)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"agent_id": res.AgentID, "parent": sess.AgentID, "channel": req.Channel})
			}
			_, err = fmt.Fprintln(a.stdout, cellText(res.AgentID))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&task, "task", "", "what the worker should do (required)")
	f.StringVar(&channel, "channel", "", "channel the worker joins and reports in (required)")
	f.StringVar(&name, "name", "", "worker name (default "+spawnSessionPrefix+"-<pid>-w<N>)")
	f.StringVar(&workdir, "workdir", "", "worker working directory (default: the current directory)")
	f.IntVar(&maxTurns, "max-turns", 0, "maximum harness turns (0 = the harness manifest's default)")
	f.Int64Var(&budgetTokens, "budget-tokens", 0, "token budget (0 = unlimited)")
	f.Float64Var(&budgetUSD, "budget-usd", 0, "budget in US dollars (0 = unlimited)")
	localAddrFlag(cmd, &localAddr)
	return cmd
}

func (a *app) workersCmd() *cobra.Command {
	cmd := groupCmd("workers", "List and stop the worker agents running on this machine", "worker")
	cmd.AddCommand(a.workersLsCmd(), a.workersStopCmd())
	return cmd
}

func (a *app) localClient(cmd *cobra.Command, localAddr string) (*mcpadapter.Client, error) {
	u, tok, err := a.localTarget(cmd, localAddr)
	if err != nil {
		return nil, err
	}
	return mcpadapter.NewClient(u, tok)
}

func (a *app) workersLsCmd() *cobra.Command {
	var localAddr string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List this machine's workers with state and usage",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.localClient(cmd, localAddr)
			if err != nil {
				return err
			}
			ws, err := c.Workers(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				if ws == nil {
					ws = []mcpadapter.Worker{}
				}
				return a.printJSONValue(map[string]any{"workers": ws})
			}
			tw := newTable(a.stdout)
			row(tw, "AGENT", "PARENT", "HARNESS", "STATE", "PID", "USED", "TURNS", "STARTED", "EXITED")
			for _, w := range ws {
				pid := "-"
				if w.PID > 0 {
					pid = strconv.Itoa(w.PID)
				}
				exited := "-"
				if w.ExitedAt != "" {
					exited = ago(w.ExitedAt, a.now()) + " (code " + strconv.Itoa(w.ExitCode) + ")"
				}
				row(tw, w.AgentID, dash(w.Parent), dash(w.Harness), dash(w.State), pid,
					usedCell(&mycelv1.Budget{Tokens: w.Usage.Tokens, Usd: w.Usage.USD}), strconv.Itoa(w.Usage.Turns),
					ago(w.StartedAt, a.now()), exited)
			}
			return tw.Flush()
		},
	}
	localAddrFlag(cmd, &localAddr)
	return cmd
}

func (a *app) workersStopCmd() *cobra.Command {
	var localAddr string
	cmd := &cobra.Command{
		Use:   "stop <agent>",
		Short: "Stop a worker on this machine",
		Args:  exactArgs(1, "agent"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.localClient(cmd, localAddr)
			if err != nil {
				return err
			}
			id := normAgentID(args[0])
			if err := c.StopWorker(cmd.Context(), "", id); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"stopped": id})
			}
			_, err = fmt.Fprintf(a.stdout, "Stopped %s.\n", cellText(id))
			return err
		},
	}
	localAddrFlag(cmd, &localAddr)
	return cmd
}
