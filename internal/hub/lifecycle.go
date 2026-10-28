package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// Agent lifecycle and budgets (spec: "Agent lifecycle", "Channel policy →
// Budget", "Kill switch" level one).
//
// Every state change runs under server.life (see withLife): usage
// accounting and the budget checks that follow it, pause, resume, stop,
// exit, registration and revocation. Control messages are queued per node
// in decision order and sent outside the lock.
//
// A pause reason is sticky: it survives exits, stops and re-registration
// (a re-registered agent with a reason is paused again). Only ResumeAgent,
// or an automatic resume of a budget pause once the budget allows it,
// clears it. An operator pause replaces a budget pause, so budget changes
// no longer resume that agent.

// Pause reasons.
const (
	reasonAgentBudget   = "budget: agent"
	reasonChannelBudget = "budget: channel"
	reasonOperator      = "operator" // operator reasons are "operator" or "operator: <text>"
)

// Limits on node reports and operator input.
const (
	maxUsageTokens = 10_000_000 // per usage report; larger reports are refused
	maxUsageUSD    = 1000.0
	maxUsageTurns  = 10_000
	maxReasonLen   = 256
	maxWorkdirLen  = 1024

	// Usage and exit reports per node: 10 per second, burst 20.
	lifeReportsPerMinute = 600
	lifeReportsBurst     = 20
)

var errBudgetExhausted = connect.NewError(connect.CodeFailedPrecondition,
	errors.New("budget still exhausted; raise it or reset usage"))

func isBudgetReason(r string) bool { return r == reasonAgentBudget || r == reasonChannelBudget }

// budgetExhausted reports whether usage has reached a budget. A zero
// component means no ceiling for that component.
func budgetExhausted(budgetTokens int64, budgetUSD float64, usedTokens int64, usedUSD float64) bool {
	return (budgetTokens > 0 && usedTokens >= budgetTokens) || (budgetUSD > 0 && usedUSD >= budgetUSD)
}

func (a *agentRow) exhausted() bool {
	return budgetExhausted(a.BudgetTokens, a.BudgetUSD, a.UsedTokens, a.UsedUSD)
}

// remaining is budget minus usage per component; 0 for a component without
// a ceiling. An exhausted component is always negative (exactly reaching
// the ceiling reports -1 token or -0.000001 USD), so "negative means
// exhausted" holds.
func remaining(budgetTokens int64, budgetUSD float64, usedTokens int64, usedUSD float64) *mycelv1.Budget {
	out := &mycelv1.Budget{}
	if budgetTokens > 0 {
		out.Tokens = budgetTokens - usedTokens
		if out.Tokens == 0 {
			out.Tokens = -1
		}
	}
	if budgetUSD > 0 {
		out.Usd = budgetUSD - usedUSD
		if out.Usd >= 0 && usedUSD >= budgetUSD {
			out.Usd = -0.000001
		}
	}
	return out
}

// validBudget checks an operator- or node-supplied budget.
func validBudget(b *mycelv1.Budget) error {
	if b.GetTokens() < 0 || b.GetUsd() < 0 || math.IsNaN(b.GetUsd()) || math.IsInf(b.GetUsd(), 0) {
		return invalidArg("budget must be finite and not negative")
	}
	return nil
}

// cleanReason makes a reason safe for a one-line event and caps its length.
func cleanReason(r string) string {
	r = strings.TrimSpace(channels.SanitizeLine(r))
	if len(r) > maxReasonLen {
		r = r[:maxReasonLen]
		for !utf8.ValidString(r) {
			r = r[:len(r)-1]
		}
	}
	return r
}

// operatorReason is the stored reason of an operator pause.
func operatorReason(text string) string {
	text = cleanReason(text)
	if text == "" || text == reasonOperator {
		return reasonOperator
	}
	return reasonOperator + ": " + text
}

// channelBudget returns a channel's budget (nil when the channel is unknown),
// its usage and whether the budget is exhausted.
func (s *server) channelBudget(ctx context.Context, name string) (*mycelv1.Budget, *usageRow, bool, error) {
	u, err := s.st.channelUsage(ctx, name)
	if err != nil {
		return nil, nil, false, err
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, u, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	b := ch.Policy.GetBudget()
	return b, u, budgetExhausted(b.GetTokens(), b.GetUsd(), u.UsedTokens, u.UsedUSD), nil
}

// budgetBlock returns the reason a's budgets keep it from running, or "".
func (s *server) budgetBlock(ctx context.Context, a *agentRow) (string, error) {
	if a.exhausted() {
		return reasonAgentBudget, nil
	}
	if a.HomeChannel == "" {
		return "", nil
	}
	_, _, ex, err := s.channelBudget(ctx, a.HomeChannel)
	if err != nil {
		return "", err
	}
	if ex {
		return reasonChannelBudget, nil
	}
	return "", nil
}

// agentEvent posts an EVENT to the agent's home channel, or to #alerts
// when it has none (or the home channel is gone or archived). Rate limited
// like the other hub events.
func (s *server) agentEvent(ctx context.Context, home, node, text string) {
	if home != "" {
		ch, err := s.st.getChannel(ctx, home)
		if err == nil && ch.ArchivedAt == "" {
			if !s.ch.limiter.Allow(rateKeyHub, ch.Name, hubEventsPerMinute) {
				s.log.Debug("agent event rate limited", "channel", ch.Name, "text", text)
				return
			}
			s.hubEvent(ctx, ch.Name, "", text, 0)
			return
		}
	}
	s.alert(ctx, node, "%s", text)
}

// event records an EVENT about a, posted once the lock is released.
func (s *server) event(fx *lifeFx, a *agentRow, text string) {
	home, node := a.HomeChannel, a.NodeName
	fx.post(func(ctx context.Context) { s.agentEvent(ctx, home, node, text) })
}

// setState stores a state change; false when the agent was revoked meanwhile.
func (s *server) setState(ctx context.Context, a *agentRow, state, reason string) (bool, error) {
	ok, err := s.st.setAgentState(ctx, a.ID, state, reason)
	if err != nil {
		return false, s.internalError("set agent state", err)
	}
	if ok {
		a.State, a.PausedReason = state, reason
	}
	return ok, nil
}

// pauseLocked pauses an agent for reason.
//   - running: paused, AgentCtl pause, EVENT, audit;
//   - paused for a budget, operator reason: the operator reason replaces it;
//   - stopped, operator reason: the reason is stored so that the next
//     registration starts paused;
//   - otherwise nothing changes (the first pause reason stays).
func (s *server) pauseLocked(ctx context.Context, fx *lifeFx, actorID string, a *agentRow, reason string) error {
	if a.RevokedAt != "" {
		return nil
	}
	// operator: an operator or kill-switch reason. An operator pause
	// replaces a budget or freeze pause, so that lifting the budget or the
	// freeze never resumes an agent an operator paused; a freeze replaces
	// a budget pause.
	operator := !isBudgetReason(reason)
	byOperator := strings.HasPrefix(reason, reasonOperator)
	weaker := func(prev string) bool { return isBudgetReason(prev) || (byOperator && isFreezeReason(prev)) }
	switch {
	case a.State == stateRunning:
		ok, err := s.setState(ctx, a, statePaused, reason)
		if err != nil || !ok {
			return err
		}
		fx.ctl(a, "pause", reason)
		s.event(fx, a, fmt.Sprintf("agent %s paused: %s", a.ID, reason))
	case a.State == statePaused && operator && weaker(a.PausedReason):
		was := a.PausedReason
		ok, err := s.setState(ctx, a, statePaused, reason)
		if err != nil || !ok {
			return err
		}
		fx.ctl(a, "pause", reason)
		s.event(fx, a, fmt.Sprintf("agent %s paused: %s (was %s)", a.ID, reason, was))
	case a.State == stateStopped && operator && (a.PausedReason == "" || weaker(a.PausedReason)):
		if ok, err := s.setState(ctx, a, stateStopped, reason); err != nil || !ok {
			return err
		}
	default:
		return nil
	}
	s.audit(ctx, actorID, string(policy.ActionAgentPause), a.ID, outcomeOK, map[string]any{"reason": reason, "state": a.State})
	return nil
}

// resumeLocked clears an agent's pause unless one of its budgets is still
// exhausted. A running agent is left alone; a stopped agent only loses its
// stored reason.
func (s *server) resumeLocked(ctx context.Context, fx *lifeFx, actorID string, a *agentRow) error {
	if a.PausedReason == "" && a.State != statePaused {
		if a.State == stateStopped {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("agent stopped"))
		}
		return nil
	}
	if block := s.freezeBlock(a); block != "" {
		s.audit(ctx, actorID, string(policy.ActionAgentResume), a.ID, outcomeError, map[string]any{"reason": block})
		return errFrozen(block)
	}
	block, err := s.budgetBlock(ctx, a)
	if err != nil {
		return s.internalError("budget check", err)
	}
	if block != "" {
		s.audit(ctx, actorID, string(policy.ActionAgentResume), a.ID, outcomeError, map[string]any{"reason": "budget exhausted", "budget": block})
		return errBudgetExhausted
	}
	prev := a.PausedReason
	if a.State == stateStopped {
		if _, err := s.setState(ctx, a, stateStopped, ""); err != nil {
			return err
		}
		s.audit(ctx, actorID, string(policy.ActionAgentResume), a.ID, outcomeOK, map[string]any{"was": prev, "state": a.State})
		return nil
	}
	ok, err := s.setState(ctx, a, stateRunning, "")
	if err != nil || !ok {
		return err
	}
	fx.ctl(a, "resume", "")
	s.event(fx, a, fmt.Sprintf("agent %s resumed (was paused: %s)", a.ID, prev))
	s.audit(ctx, actorID, string(policy.ActionAgentResume), a.ID, outcomeOK, map[string]any{"was": prev})
	return nil
}

// autoResumeLocked resumes an agent whose pause reason is the budget reason
// once its budgets allow it. If the other budget is now the exhausted one,
// the pause stays and its reason flips to that budget.
func (s *server) autoResumeLocked(ctx context.Context, fx *lifeFx, actorID string, a *agentRow, reason string) error {
	if a.PausedReason != reason || a.RevokedAt != "" {
		return nil
	}
	if s.freezeBlock(a) != "" {
		return nil // a freeze keeps it paused; unfreezing re-evaluates
	}
	block, err := s.budgetBlock(ctx, a)
	if err != nil {
		return s.internalError("budget check", err)
	}
	if block == "" {
		return s.resumeLocked(ctx, fx, actorID, a)
	}
	if block != reason {
		if ok, err := s.setState(ctx, a, a.State, block); err != nil || !ok {
			return err
		}
		s.audit(ctx, actorID, string(policy.ActionAgentPause), a.ID, outcomeOK, map[string]any{"reason": block, "was": reason})
	}
	return nil
}

// enforceLocked pauses a running agent whose budgets are exhausted.
func (s *server) enforceLocked(ctx context.Context, fx *lifeFx, actorID string, a *agentRow) error {
	if a.State != stateRunning {
		return nil
	}
	if block := s.freezeBlock(a); block != "" {
		return s.pauseLocked(ctx, fx, actorID, a, block)
	}
	block, err := s.budgetBlock(ctx, a)
	if err != nil {
		return s.internalError("budget check", err)
	}
	if block == "" {
		return nil
	}
	return s.pauseLocked(ctx, fx, actorID, a, block)
}

// evaluateChannelLocked applies a channel's budget to every agent whose
// home channel it is: running ones are paused while it is exhausted, and
// those paused for it are resumed once it is not.
func (s *server) evaluateChannelLocked(ctx context.Context, fx *lifeFx, actorID, channel string) error {
	_, _, ex, err := s.channelBudget(ctx, channel)
	if err != nil {
		return s.internalError("channel budget", err)
	}
	agents, err := s.st.agentsByHome(ctx, channel)
	if err != nil {
		return s.internalError("agents by home channel", err)
	}
	for _, a := range agents {
		if ex {
			if a.State == stateRunning {
				if err := s.pauseLocked(ctx, fx, actorID, a, reasonChannelBudget); err != nil {
					return err
				}
			}
			continue
		}
		if err := s.autoResumeLocked(ctx, fx, actorID, a, reasonChannelBudget); err != nil && !errors.Is(err, errBudgetExhausted) {
			return err
		}
	}
	return nil
}

// stopLocked stops an agent: no more token refreshes or RPCs for this run.
// A pause reason is kept for the next registration.
func (s *server) stopLocked(ctx context.Context, fx *lifeFx, actorID string, a *agentRow) error {
	if a.State == stateStopped || a.RevokedAt != "" {
		return nil
	}
	ok, err := s.setState(ctx, a, stateStopped, a.PausedReason)
	if err != nil || !ok {
		return err
	}
	fx.ctl(a, "stop", "stopped by "+actorID)
	s.event(fx, a, fmt.Sprintf("agent %s stopped by %s", a.ID, actorID))
	s.audit(ctx, actorID, string(policy.ActionAgentStop), a.ID, outcomeOK, map[string]any{"node_name": a.NodeName})
	return nil
}

// ---- node RPCs ----

// nodeAgent loads an agent the node in the subject registered. Agents of
// other nodes and unknown ids get the same answer. Revoked agents are
// returned with errAgentNotLive.
func (s *server) nodeAgent(ctx context.Context, node, method, id string) (*agentRow, error) {
	a, err := s.st.getAgent(ctx, id)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, s.internalError("get agent", err)
	}
	if err != nil || a.NodeName != node {
		s.auditDenied(ctx, "node:"+node, method, "agent", map[string]any{"reason": "agent not registered by this node", "agent_id": id})
		return nil, rpcErr(connect.CodePermissionDenied, "agent not registered by this node")
	}
	if a.RevokedAt != "" {
		return a, errAgentNotLive
	}
	return a, nil
}

// lifeReport admits a node's usage or exit report: per-node rate limit and
// the per-node RPC slots. The returned function frees the slot.
func (s *server) lifeReport(node string) (func(), error) {
	if !s.lifeLimit.allow(node) {
		return nil, rpcErr(connect.CodeResourceExhausted, "too many usage or exit reports from this node; slow down")
	}
	c := s.ch
	if !c.acquire(c.nodeRPCs, node, maxNodeRPCs) {
		return nil, rpcErr(connect.CodeResourceExhausted, "node busy: too many concurrent requests (max %d)", maxNodeRPCs)
	}
	return func() { c.release(c.nodeRPCs, node) }, nil
}

// agentUsage serves agent.usage: the node reports usage increments for one
// of its agents. Budgets are checked afterwards; the reply carries what is
// left and whether the agent is paused.
func (s *server) agentUsage(ctx context.Context, node string, data []byte) (proto.Message, error) {
	var req mycelv1.AgentUsageRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
	}
	tokens, usd, turns := req.GetTokens(), req.GetUsd(), int64(req.GetTurns())
	if tokens < 0 || turns < 0 || usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return nil, rpcErr(connect.CodeInvalidArgument, "usage increments must be finite and not negative")
	}
	if tokens > maxUsageTokens || usd > maxUsageUSD || turns > maxUsageTurns {
		s.auditDenied(ctx, "node:"+node, "agent.usage", req.GetAgentId(),
			map[string]any{"reason": "report too large", "tokens": tokens, "usd": usd, "turns": turns})
		return nil, rpcErr(connect.CodeInvalidArgument, "usage report too large: at most %d tokens, %g USD and %d turns per report",
			maxUsageTokens, maxUsageUSD, maxUsageTurns)
	}
	if _, err := s.nodeAgent(ctx, node, "agent.usage", req.GetAgentId()); errors.Is(err, errAgentNotLive) {
		return nil, rpcErr(connect.CodePermissionDenied, "agent revoked")
	} else if err != nil {
		return nil, err
	}
	resp := &mycelv1.AgentUsageResponse{ChannelRemaining: &mycelv1.Budget{}}
	fx := &lifeFx{}
	err := s.withLife(ctx, fx, func() error {
		return s.usageLocked(ctx, fx, node, req.GetAgentId(), tokens, usd, turns, resp)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *server) usageLocked(ctx context.Context, fx *lifeFx, node, id string, tokens int64, usd float64, turns int64,
	resp *mycelv1.AgentUsageResponse) error {
	a, _, err := s.st.addUsage(ctx, node, id, tokens, usd, turns)
	switch {
	case errors.Is(err, errNotFound):
		return rpcErr(connect.CodePermissionDenied, "agent not registered by this node")
	case errors.Is(err, errAgentNotLive):
		return rpcErr(connect.CodePermissionDenied, "agent revoked")
	case err != nil:
		return s.internalError("add usage", err)
	}
	if a.State == stateRunning && a.exhausted() {
		if err := s.pauseLocked(ctx, fx, hubActor.id, a, reasonAgentBudget); err != nil {
			return err
		}
	}
	resp.AgentRemaining = remaining(a.BudgetTokens, a.BudgetUSD, a.UsedTokens, a.UsedUSD)
	if a.HomeChannel != "" {
		b, u, ex, err := s.channelBudget(ctx, a.HomeChannel)
		if err != nil {
			return s.internalError("channel budget", err)
		}
		resp.ChannelRemaining = remaining(b.GetTokens(), b.GetUsd(), u.UsedTokens, u.UsedUSD)
		if ex {
			if err := s.evaluateChannelLocked(ctx, fx, hubActor.id, a.HomeChannel); err != nil {
				return err
			}
		}
		if a, err = s.st.getAgent(ctx, a.ID); err != nil {
			return s.internalError("get agent", err)
		}
	}
	resp.Paused = a.State == statePaused
	return nil
}

// agentExited serves agent.exited: the agent's process ended. A pause
// reason is kept. The exit of a revoked agent is audited and changes
// nothing; a second report for the same run is a no-op.
func (s *server) agentExited(ctx context.Context, node string, data []byte) (proto.Message, error) {
	var req mycelv1.AgentExitedRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
	}
	reason := cleanReason(req.GetReason())
	if reason == "" {
		reason = fmt.Sprintf("exit code %d", req.GetExitCode())
	}
	fx := &lifeFx{}
	err := s.withLife(ctx, fx, func() error {
		a, err := s.nodeAgent(ctx, node, "agent.exited", req.GetAgentId())
		if errors.Is(err, errAgentNotLive) {
			s.audit(ctx, "node:"+node, "agent.exited", a.ID, outcomeOK,
				map[string]any{"exit_code": req.GetExitCode(), "reason": reason, "revoked": true})
			return nil
		}
		if err != nil {
			return err
		}
		if a.ExitedAt != "" {
			return nil // already recorded for this run
		}
		if err := s.st.setAgentExited(ctx, a.ID, req.GetExitCode()); err != nil {
			return s.internalError("agent exited", err)
		}
		s.event(fx, a, fmt.Sprintf("agent %s exited (%s)", a.ID, reason))
		s.audit(ctx, "node:"+node, "agent.exited", a.ID, outcomeOK, map[string]any{"exit_code": req.GetExitCode(), "reason": reason})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &mycelv1.AgentExitedResponse{}, nil
}
