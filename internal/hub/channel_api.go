package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// channelService is the human ChannelService (Connect, API key).
type channelService struct{ s *server }

// ctxStream carries the http.ResponseController of a Watch request.
const ctxStream ctxKey = 100

// streamDeadlines hands Watch handlers their response controller so they
// can lift the server's read and write timeouts once the request has been
// read and authorised; until then the normal timeouts apply. Watch streams
// live until the client goes away (capped at maxWatch), and over HTTP/1.1
// an expired read deadline would otherwise cancel the stream.
func streamDeadlines(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mycelv1connect.ChannelServiceWatchProcedure {
			r = r.WithContext(context.WithValue(r.Context(), ctxStream, http.NewResponseController(w)))
		}
		h.ServeHTTP(w, r)
	})
}

// liftStreamDeadlines clears the read and write deadlines of the current
// Watch request.
func liftStreamDeadlines(ctx context.Context) {
	if rc, ok := ctx.Value(ctxStream).(*http.ResponseController); ok {
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
	}
}

func validatePolicy(p *mycelv1.ChannelPolicy) error {
	if p == nil {
		return nil
	}
	if p.GetMaxHops() < 0 || p.GetMaxHops() > maxPolicyHops {
		return invalidArg("max_hops must be 0 (default) to %d", maxPolicyHops)
	}
	if p.GetRatePerMinute() < 0 || p.GetRatePerMinute() > maxPolicyRate {
		return invalidArg("rate_per_minute must be 0 (default) to %d", maxPolicyRate)
	}
	if len(p.GetAllowedProfiles()) > maxProfiles {
		return invalidArg("at most %d allowed profiles", maxProfiles)
	}
	for _, pr := range p.GetAllowedProfiles() {
		if !validTag(pr) {
			return invalidArg("invalid profile name %q", pr)
		}
	}
	if r := p.GetRetention(); r != "" {
		if d, err := time.ParseDuration(r); err != nil || d <= 0 {
			return invalidArg("invalid retention %q: a positive Go duration", r)
		}
	}
	if p.GetBudget() != nil {
		if err := validBudget(p.GetBudget()); err != nil {
			return err
		}
	}
	return nil
}

func memberRole(member, role string) (string, error) {
	if role == "" {
		return roleFor(member), nil
	}
	agent := roleFor(member) == memberAgent
	switch role {
	case memberObserver:
		return role, nil
	case memberAgent:
		if agent {
			return role, nil
		}
	case memberOwner, memberHuman:
		if !agent {
			return role, nil
		}
	}
	return "", invalidArg("invalid role %q for %s", role, member)
}

func (c *channelService) CreateChannel(ctx context.Context, req *connect.Request[mycelv1.CreateChannelRequest]) (*connect.Response[mycelv1.CreateChannelResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if !channels.ValidName(name) {
		return nil, invalidArg("invalid channel name %q: '#' + 1-63 chars of a-z, 0-9 and '-'", name)
	}
	if channels.IsSystem(name) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("system channels are created by the hub"))
	}
	typ := req.Msg.GetType()
	switch typ {
	case mycelv1.ChannelType_CHANNEL_TYPE_UNSPECIFIED:
		typ = mycelv1.ChannelType_CHANNEL_TYPE_TOPIC
		if channels.IsTask(name) {
			typ = mycelv1.ChannelType_CHANNEL_TYPE_TASK
		}
	case mycelv1.ChannelType_CHANNEL_TYPE_TOPIC:
		if channels.IsTask(name) {
			return nil, invalidArg("#task-... names are reserved for task channels")
		}
	case mycelv1.ChannelType_CHANNEL_TYPE_TASK:
		if !channels.IsTask(name) {
			return nil, invalidArg("task channel names start with #task-")
		}
	default:
		return nil, invalidArg("type must be topic or task")
	}
	if err := validatePolicy(req.Msg.GetPolicy()); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionChannelCreate, policy.Resource{Kind: "channel", ID: name, OwnerID: u.ID}, true); err != nil {
		return nil, err
	}
	self := userMemberID(u.ID)
	members := []memberRow{{Member: self, Role: memberOwner, AddedBy: u.ID}}
	for _, m := range req.Msg.GetMembers() {
		id, err := s.resolveParticipant(ctx, m)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(members, func(r memberRow) bool { return r.Member == id }) {
			members = append(members, memberRow{Member: id, Role: roleFor(id), AddedBy: u.ID})
		}
	}
	pol := req.Msg.GetPolicy()
	if pol == nil {
		pol = &mycelv1.ChannelPolicy{}
	}
	row := &channelRow{Name: name, Type: typ, OwnerID: u.ID, Creator: self, CreatorOwner: u.ID, Policy: pol, CreatedAt: nowString()}
	if err := s.st.createChannel(ctx, row, members); errors.Is(err, errConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("channel "+name+" already exists"))
	} else if errors.Is(err, errChannelCap) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many task and direct channels for this owner"))
	} else if err != nil {
		return nil, s.internalError("create channel", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionChannelCreate), "channel:"+name, outcomeOK, map[string]any{"type": typ.String(), "members": len(members)})
	pc, err := s.channelToProto(ctx, row)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.CreateChannelResponse{Channel: pc}), nil
}

func (c *channelService) ListChannels(ctx context.Context, req *connect.Request[mycelv1.ListChannelsRequest]) (*connect.Response[mycelv1.ListChannelsResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionChannelList, policy.Resource{}, false); err != nil {
		return nil, err
	}
	chs, err := s.visibleChannels(ctx, userActor(u), req.Msg.GetIncludeArchived())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListChannelsResponse{Channels: chs}), nil
}

func (c *channelService) GetChannel(ctx context.Context, req *connect.Request[mycelv1.GetChannelRequest]) (*connect.Response[mycelv1.GetChannelResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.loadAuthorized(ctx, userActor(u), req.Msg.GetName(), policy.ActionChannelGet, false)
	if err != nil {
		return nil, err
	}
	pc, err := s.channelToProto(ctx, ch)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetChannelResponse{Channel: pc}), nil
}

func (c *channelService) UpdateChannelPolicy(ctx context.Context, req *connect.Request[mycelv1.UpdateChannelPolicyRequest]) (*connect.Response[mycelv1.UpdateChannelPolicyResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.loadAuthorized(ctx, userActor(u), req.Msg.GetName(), policy.ActionChannelPolicyUpdate, true)
	if err != nil {
		return nil, err
	}
	if err := validatePolicy(req.Msg.GetPolicy()); err != nil {
		return nil, err
	}
	pol := req.Msg.GetPolicy()
	if pol == nil {
		pol = &mycelv1.ChannelPolicy{}
	}
	if err := s.st.setChannelPolicy(ctx, ch.Name, pol); err != nil {
		return nil, s.internalError("set policy", err)
	}
	ch.Policy = pol
	eff := channels.WithDefaults(pol, ch.Type)
	s.audit(ctx, u.ID, string(policy.ActionChannelPolicyUpdate), "channel:"+ch.Name, outcomeOK, map[string]any{
		"max_hops": eff.GetMaxHops(), "rate_per_minute": eff.GetRatePerMinute(), "open_to_members": eff.GetOpenToMembers(),
		"agents_may_post": eff.GetAgentsMayPost(), "humans_may_post": eff.GetHumansMayPost(),
		"budget_tokens": eff.GetBudget().GetTokens(), "budget_usd": eff.GetBudget().GetUsd(),
	})
	s.applyChannelBudget(ctx, u.ID, ch.Name)
	pc, err := s.channelToProto(ctx, ch)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.UpdateChannelPolicyResponse{Channel: pc}), nil
}

func (c *channelService) AddMember(ctx context.Context, req *connect.Request[mycelv1.AddMemberRequest]) (*connect.Response[mycelv1.AddMemberResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.loadAuthorized(ctx, userActor(u), req.Msg.GetChannel(), policy.ActionChannelMemberAdd, true)
	if err != nil {
		return nil, err
	}
	if ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_DIRECT {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("direct channels have exactly two members"))
	}
	if ch.ArchivedAt != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("channel is archived"))
	}
	id, err := s.resolveParticipant(ctx, req.Msg.GetMember())
	if err != nil {
		return nil, err
	}
	if ch.Name == channels.Approvals && strings.HasPrefix(id, "agent:") {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("agents may not be members of "+channels.Approvals))
	}
	role, err := memberRole(id, req.Msg.GetRole())
	if err != nil {
		return nil, err
	}
	if err := s.st.addMember(ctx, ch.Name, id, role, u.ID); err != nil {
		return nil, s.internalError("add member", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionChannelMemberAdd), "channel:"+ch.Name, outcomeOK, map[string]any{"member": id, "role": role})
	pc, err := s.channelToProto(ctx, ch)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.AddMemberResponse{Channel: pc}), nil
}

func (c *channelService) RemoveMember(ctx context.Context, req *connect.Request[mycelv1.RemoveMemberRequest]) (*connect.Response[mycelv1.RemoveMemberResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	a := userActor(u)
	ch, err := s.loadChannel(ctx, req.Msg.GetChannel())
	if err != nil {
		return nil, err
	}
	member := req.Msg.GetMember()
	if member == u.ID {
		member = a.id
	}
	// Anyone may leave; removing someone else needs the owner or an admin.
	if member != a.id {
		if err := s.authorizeChannel(ctx, a, policy.ActionChannelMemberRemove, ch, true); err != nil {
			return nil, err
		}
	}
	if ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_DIRECT {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("direct channels have exactly two members"))
	}
	if err := s.st.removeMember(ctx, ch.Name, member); errors.Is(err, errNotFound) {
		return nil, notFound("member")
	} else if err != nil {
		return nil, s.internalError("remove member", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionChannelMemberRemove), "channel:"+ch.Name, outcomeOK, map[string]any{"member": member})
	return connect.NewResponse(&mycelv1.RemoveMemberResponse{}), nil
}

func (c *channelService) ArchiveChannel(ctx context.Context, req *connect.Request[mycelv1.ArchiveChannelRequest]) (*connect.Response[mycelv1.ArchiveChannelResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.loadAuthorized(ctx, userActor(u), req.Msg.GetName(), policy.ActionChannelArchive, true)
	if err != nil {
		return nil, err
	}
	if ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM || ch.Name == channels.General {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(ch.Name+" cannot be archived"))
	}
	if err := s.st.archiveChannel(ctx, ch.Name); err != nil {
		return nil, s.internalError("archive channel", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionChannelArchive), "channel:"+ch.Name, outcomeOK, nil)
	return connect.NewResponse(&mycelv1.ArchiveChannelResponse{}), nil
}

func (c *channelService) Post(ctx context.Context, req *connect.Request[mycelv1.PostRequest]) (*connect.Response[mycelv1.PostResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	msg, err := c.s.post(ctx, userActor(u), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.PostResponse{Message: msg}), nil
}

func (c *channelService) ListMessages(ctx context.Context, req *connect.Request[mycelv1.ListMessagesRequest]) (*connect.Response[mycelv1.ListMessagesResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.s.listMessagesFor(ctx, userActor(u), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// Watch replays messages after after_seq from the requested (or all
// readable) channels, then streams new ones until the client goes away,
// the hub stops or maxWatch passes (the client resumes with after_seq).
// Read access is re-checked on every wake-up.
func (c *channelService) Watch(ctx context.Context, req *connect.Request[mycelv1.WatchRequest], stream *connect.ServerStream[mycelv1.WatchResponse]) error {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return err
	}
	a := userActor(u)
	var only map[string]bool
	if names := req.Msg.GetChannels(); len(names) > 0 {
		if len(names) > maxWatchChannels {
			return invalidArg("at most %d channels", maxWatchChannels)
		}
		only = map[string]bool{}
		for _, n := range names {
			if _, err := s.loadAuthorized(ctx, a, n, policy.ActionChannelRead, false); err != nil {
				return err
			}
			only[n] = true
		}
	}
	if !s.ch.acquireWatch(u.ID) {
		return connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("too many watch streams (at most %d per user, %d in total)", maxWatchesPerUser, maxWatchesTotal))
	}
	defer s.ch.releaseWatch(u.ID)
	// The request is read and authorised: the stream may now outlive the
	// server's read and write timeouts.
	liftStreamDeadlines(ctx)
	wake, cancel := s.ch.notify.subscribe(watchAll)
	defer cancel()
	// Send the response headers now: clients wait for them before the first
	// message, which may be a long way off.
	if err := stream.Send(nil); err != nil {
		return nil
	}
	auditedDirect := map[string]bool{}
	// canRead re-checks read access; an admin's first read of a direct
	// channel it is not part of is audited once per stream.
	canRead := func(name string) bool {
		ch, err := s.st.getChannel(ctx, name)
		if err != nil {
			return false
		}
		member, d, err := s.checkChannel(ctx, a, policy.ActionChannelRead, ch)
		if err != nil || !d.Allow {
			return false
		}
		if !member && isDirect(ch) && !auditedDirect[name] {
			auditedDirect[name] = true
			s.auditAdminDirectRead(ctx, a, ch, "watch")
		}
		return true
	}
	limit := time.NewTimer(maxWatch)
	defer limit.Stop()
	cursor := req.Msg.GetAfterSeq()
	for {
		upto, err := s.st.maxSeq(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return s.internalError("max seq", err)
		}
		readable := map[string]bool{}
		// A stream on named channels ends when access to one of them is lost.
		for name := range only {
			if !canRead(name) {
				return connect.NewError(connect.CodePermissionDenied, errors.New("read access to "+name+" was lost"))
			}
			readable[name] = true
		}
		for cursor < upto {
			batch, err := s.st.messagesAfter(ctx, cursor, upto, scanBatch)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return s.internalError("watch", err)
			}
			if len(batch) == 0 {
				cursor = upto
				break
			}
			for _, m := range batch {
				cursor = m.GetSeq()
				name := m.GetChannel()
				if only != nil && !only[name] {
					continue
				}
				ok, seen := readable[name]
				if !seen {
					ok = canRead(name)
					readable[name] = ok
				}
				if !ok {
					continue
				}
				if err := stream.Send(&mycelv1.WatchResponse{Message: m}); err != nil {
					return nil
				}
			}
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil
		case <-s.ch.stop:
			return nil
		case <-limit.C:
			return nil
		}
	}
}

func (c *channelService) BlackboardGet(ctx context.Context, req *connect.Request[mycelv1.BlackboardGetRequest]) (*connect.Response[mycelv1.BlackboardGetResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.s.bbGetFor(ctx, userActor(u), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *channelService) BlackboardPut(ctx context.Context, req *connect.Request[mycelv1.BlackboardPutRequest]) (*connect.Response[mycelv1.BlackboardPutResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.s.bbPutFor(ctx, userActor(u), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *channelService) BlackboardDelete(ctx context.Context, req *connect.Request[mycelv1.BlackboardDeleteRequest]) (*connect.Response[mycelv1.BlackboardDeleteResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.s.bbDeleteFor(ctx, userActor(u), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *channelService) ChannelSummary(ctx context.Context, req *connect.Request[mycelv1.ChannelSummaryRequest]) (*connect.Response[mycelv1.ChannelSummaryResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.loadAuthorized(ctx, userActor(u), req.Msg.GetChannel(), policy.ActionChannelRead, false)
	if err != nil {
		return nil, err
	}
	resp, err := s.summaryFor(ctx, ch)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
