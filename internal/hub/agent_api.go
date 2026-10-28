package hub

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// Phase 3 AgentService methods: lifecycle and budgets.

// loadAgentFor loads an agent and checks action on it for u. Unknown ids
// answer like a denial for callers the policy would refuse anyway.
func (s *server) loadAgentFor(ctx context.Context, u *userRow, action policy.Action, id string, audit bool) (*agentRow, error) {
	a, err := s.st.getAgent(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, action, "agent", id)
	}
	if err != nil {
		return nil, s.internalError("get agent", err)
	}
	if err := s.authorize(ctx, u, action, policy.Resource{Kind: "agent", ID: id, OwnerID: a.OwnerID}, audit); err != nil {
		return nil, err
	}
	return a, nil
}

// lockedAgent re-reads an agent under s.life; revoked agents are refused.
func (s *server) lockedAgent(ctx context.Context, id string) (*agentRow, error) {
	a, err := s.st.getAgent(ctx, id)
	if err != nil {
		return nil, s.internalError("get agent", err)
	}
	if a.RevokedAt != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("agent revoked"))
	}
	return a, nil
}

// reloadAgent returns the stored agent after a change.
func (s *server) reloadAgent(ctx context.Context, id string) (*mycelv1.Agent, error) {
	a, err := s.st.getAgent(ctx, id)
	if err != nil {
		return nil, s.internalError("get agent", err)
	}
	return agentToProto(a), nil
}

func (as *agentService) GetAgent(ctx context.Context, req *connect.Request[mycelv1.GetAgentRequest]) (*connect.Response[mycelv1.GetAgentResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.loadAgentFor(ctx, u, policy.ActionAgentGet, req.Msg.GetId(), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetAgentResponse{Agent: agentToProto(a)}), nil
}

// SetAgentBudget replaces an agent's budget. An agent paused for its own
// budget is resumed when the new budget exceeds its usage; a running agent
// whose usage already reaches a lowered budget is paused.
func (as *agentService) SetAgentBudget(ctx context.Context, req *connect.Request[mycelv1.SetAgentBudgetRequest]) (*connect.Response[mycelv1.SetAgentBudgetResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.loadAgentFor(ctx, u, policy.ActionAgentSetBudget, req.Msg.GetId(), true); err != nil {
		return nil, err
	}
	b := req.Msg.GetBudget()
	if err := validBudget(b); err != nil {
		return nil, err
	}
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		a, err := s.lockedAgent(ctx, req.Msg.GetId())
		if err != nil {
			return err
		}
		if err := s.st.setAgentBudget(ctx, a.ID, b.GetTokens(), b.GetUsd()); err != nil {
			return s.internalError("set budget", err)
		}
		a.BudgetTokens, a.BudgetUSD = b.GetTokens(), b.GetUsd()
		s.audit(ctx, u.ID, string(policy.ActionAgentSetBudget), a.ID, outcomeOK,
			map[string]any{"budget_tokens": a.BudgetTokens, "budget_usd": a.BudgetUSD})
		if err := s.autoResumeLocked(ctx, fx, u.ID, a, reasonAgentBudget); err != nil && !errors.Is(err, errBudgetExhausted) {
			return err
		}
		if a.State == stateRunning && a.exhausted() {
			return s.pauseLocked(ctx, fx, u.ID, a, reasonAgentBudget)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pa, err := s.reloadAgent(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.SetAgentBudgetResponse{Agent: pa}), nil
}

// PauseAgent is kill-switch level one. The reason is stored as "operator"
// or "operator: <text>"; it replaces a budget pause and is kept through
// exits, stops and re-registration until ResumeAgent.
func (as *agentService) PauseAgent(ctx context.Context, req *connect.Request[mycelv1.PauseAgentRequest]) (*connect.Response[mycelv1.PauseAgentResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.loadAgentFor(ctx, u, policy.ActionAgentPause, req.Msg.GetId(), true); err != nil {
		return nil, err
	}
	reason := operatorReason(req.Msg.GetReason())
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		a, err := s.lockedAgent(ctx, req.Msg.GetId())
		if err != nil {
			return err
		}
		return s.pauseLocked(ctx, fx, u.ID, a, reason)
	})
	if err != nil {
		return nil, err
	}
	pa, err := s.reloadAgent(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.PauseAgentResponse{Agent: pa}), nil
}

// ResumeAgent clears any pause, unless a budget of the agent is still
// exhausted.
func (as *agentService) ResumeAgent(ctx context.Context, req *connect.Request[mycelv1.ResumeAgentRequest]) (*connect.Response[mycelv1.ResumeAgentResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.loadAgentFor(ctx, u, policy.ActionAgentResume, req.Msg.GetId(), true); err != nil {
		return nil, err
	}
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		a, err := s.lockedAgent(ctx, req.Msg.GetId())
		if err != nil {
			return err
		}
		return s.resumeLocked(ctx, fx, u.ID, a)
	})
	if err != nil {
		return nil, err
	}
	pa, err := s.reloadAgent(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ResumeAgentResponse{Agent: pa}), nil
}

// StopAgent ends the agent's run: the node stops the process, and the hub
// refuses the agent's token refreshes and RPCs until it registers again.
func (as *agentService) StopAgent(ctx context.Context, req *connect.Request[mycelv1.StopAgentRequest]) (*connect.Response[mycelv1.StopAgentResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.loadAgentFor(ctx, u, policy.ActionAgentStop, req.Msg.GetId(), true); err != nil {
		return nil, err
	}
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		a, err := s.lockedAgent(ctx, req.Msg.GetId())
		if err != nil {
			return err
		}
		return s.stopLocked(ctx, fx, u.ID, a)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.StopAgentResponse{}), nil
}

// usageFor loads a channel and checks a usage action on it for u.
func (s *server) usageFor(ctx context.Context, u *userRow, action policy.Action, name string) (*channelRow, error) {
	ch, err := s.loadChannel(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, action, channelResource(ch), action != policy.ActionChannelUsageGet); err != nil {
		return nil, err
	}
	return ch, nil
}

func (s *server) channelUsageProto(ctx context.Context, name string) (*mycelv1.ChannelUsage, error) {
	b, used, ex, err := s.channelBudget(ctx, name)
	if err != nil {
		return nil, s.internalError("channel usage", err)
	}
	return &mycelv1.ChannelUsage{
		Channel: name,
		Budget:  &mycelv1.Budget{Tokens: b.GetTokens(), Usd: b.GetUsd()},
		Used:    &mycelv1.Budget{Tokens: used.UsedTokens, Usd: used.UsedUSD},
		Paused:  ex,
	}, nil
}

func (as *agentService) GetChannelUsage(ctx context.Context, req *connect.Request[mycelv1.GetChannelUsageRequest]) (*connect.Response[mycelv1.GetChannelUsageResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.usageFor(ctx, u, policy.ActionChannelUsageGet, req.Msg.GetChannel())
	if err != nil {
		return nil, err
	}
	cu, err := s.channelUsageProto(ctx, ch.Name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetChannelUsageResponse{Usage: cu}), nil
}

// ResetChannelUsage zeroes a channel's counters and resumes the agents
// paused for its budget.
func (as *agentService) ResetChannelUsage(ctx context.Context, req *connect.Request[mycelv1.ResetChannelUsageRequest]) (*connect.Response[mycelv1.ResetChannelUsageResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.usageFor(ctx, u, policy.ActionChannelUsageReset, req.Msg.GetChannel())
	if err != nil {
		return nil, err
	}
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		before, err := s.st.channelUsage(ctx, ch.Name)
		if err != nil {
			return s.internalError("channel usage", err)
		}
		if err := s.st.resetChannelUsage(ctx, ch.Name); err != nil {
			return s.internalError("reset usage", err)
		}
		s.audit(ctx, u.ID, string(policy.ActionChannelUsageReset), "channel:"+ch.Name, outcomeOK,
			map[string]any{"used_tokens": before.UsedTokens, "used_usd": before.UsedUSD})
		return s.evaluateChannelLocked(ctx, fx, u.ID, ch.Name)
	})
	if err != nil {
		return nil, err
	}
	cu, err := s.channelUsageProto(ctx, ch.Name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ResetChannelUsageResponse{Usage: cu}), nil
}

// applyChannelBudget re-evaluates a channel's agents after its policy
// changed: a raised budget resumes the agents paused for it, a lowered one
// pauses them.
func (s *server) applyChannelBudget(ctx context.Context, actorID, channel string) {
	fx := &lifeFx{}
	if err := s.withLife(ctx, fx, func() error { return s.evaluateChannelLocked(ctx, fx, actorID, channel) }); err != nil {
		s.log.Warn("apply channel budget", "channel", channel, "err", err)
	}
}
