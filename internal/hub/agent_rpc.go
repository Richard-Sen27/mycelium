package hub

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// agentMethods are the agent-scoped RPCs (each carries an AgentCall token).
var agentMethods = map[string]bool{
	bus.RPCAgentPost: true, bus.RPCAgentJoinChannel: true, bus.RPCAgentLeaveChannel: true,
	bus.RPCAgentListChannels: true, bus.RPCAgentListAgents: true, bus.RPCAgentWhoami: true,
	bus.RPCAgentInboxPull: true, bus.RPCAgentBBGet: true, bus.RPCAgentBBPut: true,
	bus.RPCAgentListMessages: true,
	bus.RPCAgentJobRun:       true, bus.RPCAgentJobGet: true, bus.RPCAgentJobCancel: true, bus.RPCAgentJobLogs: true,
	bus.RPCAgentArtifactList: true,
}

// maxSubTags bounds the tags of a subscription filter.
const maxSubTags = 32

// agentRequest is any agent RPC request.
type agentRequest interface {
	proto.Message
	GetCall() *mycelv1.AgentCall
}

// agentAuth verifies an agent token for a request that arrived over node's
// connection: the token must verify with the hub's key, be bound to that
// node, and name a live (registered, unrevoked) agent with the same
// sandbox binding.
func (s *server) agentAuth(ctx context.Context, node, method string, call *mycelv1.AgentCall) (actor, error) {
	deny := func(resource, reason string) (actor, error) {
		s.auditDenied(ctx, "node:"+node, "agent.auth", resource, map[string]any{"reason": reason, "method": method})
		if reason == "agent revoked" {
			return actor{}, rpcErr(connect.CodePermissionDenied, "agent revoked")
		}
		return actor{}, rpcErr(connect.CodeUnauthenticated, "invalid agent token")
	}
	claims, err := s.signer.Verify(call.GetToken())
	if err != nil {
		return deny("agent", "token does not verify")
	}
	if claims.NodeName != node {
		return deny(claims.AgentID, "token bound to another node")
	}
	if s.st.agentRevoked(claims.AgentID) {
		return deny(claims.AgentID, "agent revoked")
	}
	a, err := s.st.getAgent(ctx, claims.AgentID)
	if errors.Is(err, errNotFound) {
		return deny(claims.AgentID, "unknown agent")
	}
	if err != nil {
		return actor{}, s.internalError("get agent", err)
	}
	if a.RevokedAt != "" {
		return deny(claims.AgentID, "agent revoked")
	}
	if a.NodeName != node || a.SandboxID != claims.SandboxID {
		return deny(claims.AgentID, "agent binding changed")
	}
	if a.State == stateStopped {
		s.auditDenied(ctx, "node:"+node, "agent.auth", claims.AgentID, map[string]any{"reason": "agent stopped", "method": method})
		return actor{}, rpcErr(connect.CodePermissionDenied, "agent stopped")
	}
	out := actor{
		id:     claims.AgentID,
		p:      policy.Principal{Kind: policy.KindAgent, ID: claims.AgentID, OwnerID: claims.OwnerID},
		origin: originAgent, node: node, sandbox: claims.SandboxID,
	}
	if a.State == statePaused {
		out.paused = a.PausedReason
		if out.paused == "" {
			out.paused = "paused"
		}
	}
	return out, nil
}

// agentAllowed asks the policy about a non-channel-specific agent action.
func (s *server) agentAllowed(ctx context.Context, a actor, action policy.Action, r policy.Resource) error {
	if d := s.policy.Authorize(ctx, a.p, action, r); !d.Allow {
		s.auditDenied(ctx, a.id, string(action), resourceName(r), map[string]any{"reason": d.Reason})
		return connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
	}
	return nil
}

// reserve takes the per-node (and for long polls per-agent and hub-wide
// long-poll) slots for an authenticated agent RPC. A long poll gives its
// general RPC slot back (release) so waiting polls never exhaust it. The
// returned function frees what was taken.
func (s *server) reserve(node string, a actor, req agentRequest, release func()) (func(), error) {
	c := s.ch
	if isLongPoll(req) {
		if !c.acquire(c.agentPolls, a.id, maxPullsPerAgent) {
			return nil, rpcErr(connect.CodeResourceExhausted, "too many concurrent inbox pulls for this agent (max %d)", maxPullsPerAgent)
		}
		if !c.acquire(c.nodePolls, node, maxNodePolls) {
			c.release(c.agentPolls, a.id)
			return nil, rpcErr(connect.CodeResourceExhausted, "node busy: too many concurrent inbox pulls (max %d)", maxNodePolls)
		}
		select {
		case c.polls <- struct{}{}:
		default:
			c.release(c.agentPolls, a.id)
			c.release(c.nodePolls, node)
			return nil, rpcErr(connect.CodeResourceExhausted, "hub busy: too many concurrent inbox pulls")
		}
		release()
		return func() {
			<-c.polls
			c.release(c.nodePolls, node)
			c.release(c.agentPolls, a.id)
		}, nil
	}
	if !c.acquire(c.nodeRPCs, node, maxNodeRPCs) {
		return nil, rpcErr(connect.CodeResourceExhausted, "node busy: too many concurrent agent requests (max %d)", maxNodeRPCs)
	}
	return func() { c.release(c.nodeRPCs, node) }, nil
}

// isLongPoll reports whether req waits (inbox pull or job log poll with a
// wait).
func isLongPoll(req agentRequest) bool {
	switch r := req.(type) {
	case *mycelv1.InboxPullRequest:
		return inboxWait(r) > 0
	case *mycelv1.AgentJobLogsRequest:
		return r.GetWaitMs() > 0
	}
	return false
}

// serveAgentRPC decodes, authenticates and serves one agent RPC. release
// gives back the hub-wide RPC slot the call runs in.
func (s *server) serveAgentRPC(ctx context.Context, node, method string, data []byte, release func()) (proto.Message, error) {
	var req agentRequest
	switch method {
	case bus.RPCAgentPost:
		req = &mycelv1.AgentPostRequest{}
	case bus.RPCAgentJoinChannel:
		req = &mycelv1.AgentJoinChannelRequest{}
	case bus.RPCAgentLeaveChannel:
		req = &mycelv1.AgentLeaveChannelRequest{}
	case bus.RPCAgentListChannels:
		req = &mycelv1.AgentListChannelsRequest{}
	case bus.RPCAgentListAgents:
		req = &mycelv1.AgentListAgentsRequest{}
	case bus.RPCAgentWhoami:
		req = &mycelv1.AgentWhoamiRequest{}
	case bus.RPCAgentInboxPull:
		req = &mycelv1.InboxPullRequest{}
	case bus.RPCAgentBBGet:
		req = &mycelv1.AgentBlackboardGetRequest{}
	case bus.RPCAgentBBPut:
		req = &mycelv1.AgentBlackboardPutRequest{}
	case bus.RPCAgentListMessages:
		req = &mycelv1.AgentListMessagesRequest{}
	case bus.RPCAgentJobRun:
		req = &mycelv1.AgentRunJobRequest{}
	case bus.RPCAgentJobGet:
		req = &mycelv1.AgentGetJobRequest{}
	case bus.RPCAgentJobCancel:
		req = &mycelv1.AgentCancelJobRequest{}
	case bus.RPCAgentJobLogs:
		req = &mycelv1.AgentJobLogsRequest{}
	case bus.RPCAgentArtifactList:
		req = &mycelv1.AgentListArtifactsRequest{}
	default:
		return nil, rpcErr(connect.CodeNotFound, "unknown method %s", method)
	}
	if err := proto.Unmarshal(data, req); err != nil {
		return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
	}
	a, err := s.agentAuth(ctx, node, method, req.GetCall())
	if err != nil {
		return nil, err
	}
	// A paused agent may read but not act. Its inbox pull answers at once
	// with no messages and the cursor unchanged, so a polling loop neither
	// spins on the hub nor skips messages that arrive while it is paused.
	if a.paused != "" {
		switch r := req.(type) {
		case *mycelv1.AgentPostRequest, *mycelv1.AgentBlackboardPutRequest, *mycelv1.AgentJoinChannelRequest,
			*mycelv1.AgentRunJobRequest:
			s.auditDenied(ctx, a.id, method, a.id, map[string]any{"reason": "agent paused", "paused_reason": a.paused})
			return nil, rpcErr(connect.CodeFailedPrecondition, "agent paused (%s)", a.paused)
		case *mycelv1.InboxPullRequest:
			return &mycelv1.InboxPullResponse{NextSeq: r.GetAfterSeq()}, nil
		}
	}
	done, err := s.reserve(node, a, req, release)
	if err != nil {
		return nil, err
	}
	defer done()
	switch r := req.(type) {
	case *mycelv1.AgentPostRequest:
		msg, err := s.post(ctx, a, r.GetPost())
		if err != nil {
			return nil, err
		}
		return &mycelv1.PostResponse{Message: msg}, nil
	case *mycelv1.AgentJoinChannelRequest:
		return s.agentJoin(ctx, a, r.GetChannel(), r.GetSubscription())
	case *mycelv1.AgentLeaveChannelRequest:
		return s.agentLeave(ctx, a, r.GetChannel())
	case *mycelv1.AgentListChannelsRequest:
		if err := s.agentAllowed(ctx, a, policy.ActionChannelList, policy.Resource{}); err != nil {
			return nil, err
		}
		chs, err := s.visibleChannels(ctx, a, false)
		if err != nil {
			return nil, err
		}
		return &mycelv1.ListChannelsResponse{Channels: chs}, nil
	case *mycelv1.AgentListAgentsRequest:
		return s.agentListAgents(ctx, a, r.GetChannel())
	case *mycelv1.AgentWhoamiRequest:
		info, err := s.agentInfo(ctx, a.id)
		if err != nil {
			return nil, err
		}
		return &mycelv1.AgentWhoamiResponse{Agent: info, HubUrl: s.cfg.AdvertiseURL, TrustDomain: string(s.td)}, nil
	case *mycelv1.InboxPullRequest:
		return s.inboxPull(ctx, a, r)
	case *mycelv1.AgentBlackboardGetRequest:
		return s.bbGetFor(ctx, a, r.GetGet())
	case *mycelv1.AgentBlackboardPutRequest:
		return s.bbPutFor(ctx, a, r.GetPut())
	case *mycelv1.AgentListMessagesRequest:
		return s.listMessagesFor(ctx, a, r.GetList())
	case *mycelv1.AgentRunJobRequest:
		job, err := s.submitJob(ctx, a, r.GetSpec())
		if err != nil {
			return nil, err
		}
		return &mycelv1.RunJobResponse{Job: job}, nil
	case *mycelv1.AgentGetJobRequest:
		j, err := s.loadJob(ctx, a, policy.ActionJobGet, r.GetId())
		if err != nil {
			return nil, err
		}
		job, err := s.jobProto(ctx, j)
		if err != nil {
			return nil, err
		}
		return &mycelv1.GetJobResponse{Job: job}, nil
	case *mycelv1.AgentCancelJobRequest:
		job, err := s.cancelJob(ctx, a, r.GetId())
		if err != nil {
			return nil, err
		}
		return &mycelv1.CancelJobResponse{Job: job}, nil
	case *mycelv1.AgentJobLogsRequest:
		return s.agentJobLogs(ctx, a, r)
	case *mycelv1.AgentListArtifactsRequest:
		arts, err := s.visibleArtifacts(ctx, a, r.GetList())
		if err != nil {
			return nil, err
		}
		return &mycelv1.ListArtifactsResponse{Artifacts: arts}, nil
	}
	return nil, internalRPC()
}

func validateSubscription(name string, sub *mycelv1.Subscription) (*mycelv1.Subscription, error) {
	if sub == nil {
		return nil, nil
	}
	out := &mycelv1.Subscription{Channel: name, MentionsOnly: sub.GetMentionsOnly()}
	for _, t := range sub.GetTypes() {
		if !validMessageType(t) {
			return nil, invalidArg("unknown message type %d in subscription", t)
		}
		out.Types = append(out.Types, t)
	}
	if len(sub.GetTags()) > maxSubTags {
		return nil, invalidArg("at most %d subscription tags", maxSubTags)
	}
	for _, t := range sub.GetTags() {
		if !validTag(t) {
			return nil, invalidArg("invalid subscription tag %q", t)
		}
		out.Tags = append(out.Tags, t)
	}
	return out, nil
}

// agentJoin joins an agent to a channel, creating a task channel on first
// join. Task channels created by agents are open to members so other agents
// can join them by name.
func (s *server) agentJoin(ctx context.Context, a actor, name string, sub *mycelv1.Subscription) (*mycelv1.AgentJoinChannelResponse, error) {
	if !channels.ValidName(name) {
		return nil, invalidArg("invalid channel name %q (direct channels are created by posting to @<id>)", name)
	}
	sub, err := validateSubscription(name, sub)
	if err != nil {
		return nil, err
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		if !channels.IsTask(name) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("channel "+name+" not found; agents may create #task-... channels by joining them"))
		}
		if err := s.agentAllowed(ctx, a, policy.ActionChannelCreate, policy.Resource{Kind: "channel", ID: name, OwnerID: a.p.OwnerID}); err != nil {
			return nil, err
		}
		row := &channelRow{
			Name: name, Type: mycelv1.ChannelType_CHANNEL_TYPE_TASK, OwnerID: a.p.OwnerID, Creator: a.id, CreatorOwner: a.p.OwnerID,
			Policy: &mycelv1.ChannelPolicy{OpenToMembers: true}, CreatedAt: nowString(),
		}
		created, err := s.st.ensureChannel(ctx, row, []memberRow{{Member: a.id, Role: memberOwner}})
		if errors.Is(err, errChannelCap) {
			return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many channels created by this agent or owner"))
		}
		if err != nil {
			return nil, s.internalError("create channel", err)
		}
		if created {
			s.audit(ctx, a.id, string(policy.ActionChannelCreate), "channel:"+name, outcomeOK, map[string]any{"type": "task", "owner_id": a.p.OwnerID})
		}
		ch, err = s.st.getChannel(ctx, name)
		if err != nil {
			return nil, s.internalError("get channel", err)
		}
	} else if err != nil {
		return nil, s.internalError("get channel", err)
	}
	if ch.ArchivedAt != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("channel is archived"))
	}
	_, err = s.st.getMember(ctx, name, a.id)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, s.internalError("get member", err)
	}
	isMember := err == nil
	// Joining is reading the channel: allowed for members and, on channels
	// open to members, for any agent.
	p := a.p
	p.Member = isMember || channels.WithDefaults(ch.Policy, ch.Type).GetOpenToMembers()
	p.Creator = ch.Creator == a.id
	if d := s.policy.Authorize(ctx, p, policy.ActionChannelGet, channelResource(ch)); !d.Allow {
		s.auditDenied(ctx, a.id, "channel.join", "channel:"+name, map[string]any{"reason": "closed channel"})
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("channel "+name+" is closed; its owner or an admin must add this agent"))
	}
	if !isMember {
		if err := s.st.addMember(ctx, name, a.id, memberAgent); err != nil {
			return nil, s.internalError("add member", err)
		}
		s.audit(ctx, a.id, "channel.join", "channel:"+name, outcomeOK, nil)
	}
	if err := s.st.setSubscription(ctx, name, a.id, sub); err != nil {
		return nil, s.internalError("subscription", err)
	}
	pc, err := s.channelToProto(ctx, ch)
	if err != nil {
		return nil, err
	}
	sum, err := s.summaryFor(ctx, ch)
	if err != nil {
		return nil, err
	}
	return &mycelv1.AgentJoinChannelResponse{Channel: pc, Summary: sum}, nil
}

// agentLeave removes the agent from a channel; leaving a channel it is not
// in succeeds. Direct channels cannot be left.
func (s *server) agentLeave(ctx context.Context, a actor, name string) (*mycelv1.AgentLeaveChannelResponse, error) {
	if channels.IsDirect(name) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("direct channels cannot be left"))
	}
	if !channels.ValidName(name) {
		return nil, invalidArg("invalid channel name %q", name)
	}
	err := s.st.removeMember(ctx, name, a.id)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, s.internalError("remove member", err)
	}
	if err == nil {
		s.audit(ctx, a.id, "channel.leave", "channel:"+name, outcomeOK, nil)
	}
	return &mycelv1.AgentLeaveChannelResponse{}, nil
}

// agentInfo describes an agent to itself (whoami): all of its channels.
func (s *server) agentInfo(ctx context.Context, id string) (*mycelv1.AgentInfo, error) {
	ag, err := s.st.getAgent(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, notFound("agent")
	}
	if err != nil {
		return nil, s.internalError("get agent", err)
	}
	return s.agentRowInfo(ctx, ag, nil)
}

// agentRowInfo describes an agent; when readable is non-nil its channels are
// filtered to those, so a listing never reveals channels the caller cannot
// read.
func (s *server) agentRowInfo(ctx context.Context, ag *agentRow, readable map[string]bool) (*mycelv1.AgentInfo, error) {
	ms, err := s.st.memberships(ctx, ag.ID)
	if err != nil {
		return nil, s.internalError("memberships", err)
	}
	info := &mycelv1.AgentInfo{Id: ag.ID, NodeName: ag.NodeName, OwnerId: ag.OwnerID, Harness: ag.Harness, State: ag.State}
	for _, m := range ms {
		if readable != nil && !readable[m.Channel] {
			continue
		}
		info.Channels = append(info.Channels, m.Channel)
	}
	return info, nil
}

// agentListAgents lists the agents of a channel the caller may read, or all
// live agents that share a readable channel with the caller (its own
// channels or open ones). Each agent's channels are filtered the same way.
func (s *server) agentListAgents(ctx context.Context, a actor, channel string) (*mycelv1.AgentListAgentsResponse, error) {
	if err := s.agentAllowed(ctx, a, policy.ActionAgentList, policy.Resource{Kind: "channel", ID: channel}); err != nil {
		return nil, err
	}
	readable, err := s.readableSet(ctx, a)
	if err != nil {
		return nil, err
	}
	out := &mycelv1.AgentListAgentsResponse{}
	if channel != "" {
		ch, err := s.loadAuthorized(ctx, a, channel, policy.ActionChannelRead, false)
		if err != nil {
			return nil, err
		}
		members, err := s.st.channelMembers(ctx, ch.Name)
		if err != nil {
			return nil, s.internalError("channel members", err)
		}
		for _, m := range members {
			if !strings.HasPrefix(m.Member, "agent:") {
				continue
			}
			ag, err := s.st.getAgent(ctx, m.Member)
			if err != nil || ag.RevokedAt != "" {
				continue
			}
			info, err := s.agentRowInfo(ctx, ag, readable)
			if err != nil {
				return nil, err
			}
			out.Agents = append(out.Agents, info)
		}
		return out, nil
	}
	rows, err := s.st.listAgents(ctx)
	if err != nil {
		return nil, s.internalError("list agents", err)
	}
	for _, ag := range rows {
		if ag.RevokedAt != "" {
			continue
		}
		info, err := s.agentRowInfo(ctx, ag, readable)
		if err != nil {
			return nil, err
		}
		if len(info.GetChannels()) == 0 {
			continue
		}
		out.Agents = append(out.Agents, info)
	}
	return out, nil
}
