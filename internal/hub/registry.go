package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// rpcStatusHeader is set on every node RPC reply: "ok" or "error".
const rpcStatusHeader = bus.StatusHeader

// rpcTimeout bounds the work done for one node message.
const rpcTimeout = 10 * time.Second

func (s *server) startRegistry() ([]*nats.Subscription, error) {
	s.startBeatFlusher()
	tel, err := s.nc.Subscribe(bus.AllTelemetry, s.handleTelemetry)
	if err != nil {
		return nil, err
	}
	rpc, err := s.nc.Subscribe(bus.AllRPC, s.handleRPC)
	if err != nil {
		_ = tel.Unsubscribe()
		return nil, err
	}
	return []*nats.Subscription{tel, rpc}, nil
}

// validReply reports whether reply is an inbox owned by node: a request may
// only make the hub publish into the requesting node's own inbox space, never
// onto another subject (confused deputy). It is bus.ValidReply.
func validReply(node, reply string) bool { return bus.ValidReply(node, reply) }

// handleRPC serves node-originated requests on node.<name>.rpc.<method>.
//
// Reply convention: on success the reply payload is the method's response
// message and the header Mycel-Status is "ok". On failure the payload is a
// CtlResponse{ok: false, error: "<code>: <message>"} where <code> is the
// Connect code name (permission_denied, not_found, ...), and Mycel-Status is
// "error"; clients check the header before decoding. Requests whose reply
// subject is not in the node's own inbox space are dropped without a reply
// and audited.
//
// Agent RPCs run on their own goroutines (bounded), because inbox pulls are
// long polls and the subscription callback is serial.
func (s *server) handleRPC(msg *nats.Msg) {
	node, ok := bus.NodeFromSubject(msg.Subject)
	if !ok {
		return
	}
	method, ok := bus.RPCMethodFromSubject(msg.Subject)
	if !ok {
		return
	}
	actor := "node:" + node
	if !validReply(node, msg.Reply) {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancel()
		s.audit(ctx, actor, "rpc."+method, actor, outcomeError, map[string]any{"reason": "invalid reply subject"})
		return
	}
	if agentMethods[method] {
		data := msg.Data
		if !s.ch.goWork(func(release func()) {
			timeout := rpcTimeout
			switch method {
			case bus.RPCAgentInboxPull:
				timeout += maxInboxWait
			case bus.RPCAgentJobLogs:
				timeout += maxLogWait
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			resp, err := s.serveAgentRPC(ctx, node, method, data, release)
			s.reply(msg, resp, err)
		}) {
			s.reply(msg, nil, connect.NewError(connect.CodeUnavailable, errors.New("hub busy or stopping; retry")))
		}
		return
	}
	switch method {
	case bus.RPCJobStarted, bus.RPCJobLog, bus.RPCJobDone:
		// Job reports write files and post results; they leave the serial
		// subscription callback and share the node's RPC slots.
		if !s.ch.acquire(s.ch.nodeRPCs, node, maxNodeRPCs) {
			s.reply(msg, nil, rpcErr(connect.CodeResourceExhausted, "node busy: too many concurrent requests (max %d)", maxNodeRPCs))
			return
		}
		data := msg.Data
		if !s.ch.goWork(func(func()) {
			defer s.ch.release(s.ch.nodeRPCs, node)
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			resp, err := s.serveJobRPC(ctx, node, method, data)
			s.reply(msg, resp, err)
		}) {
			s.ch.release(s.ch.nodeRPCs, node)
			s.reply(msg, nil, connect.NewError(connect.CodeUnavailable, errors.New("hub busy or stopping; retry")))
		}
		return
	case bus.RPCAgentUsage, bus.RPCAgentExited:
		// Lifecycle reports take the lifecycle lock, so they leave the
		// serial subscription callback; they are rate limited per node and
		// share the node's RPC slots.
		done, err := s.lifeReport(node)
		if err != nil {
			s.reply(msg, nil, err)
			return
		}
		data := msg.Data
		if !s.ch.goWork(func(func()) {
			defer done()
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			var (
				resp proto.Message
				err  error
			)
			if method == bus.RPCAgentUsage {
				resp, err = s.agentUsage(ctx, node, data)
			} else {
				resp, err = s.agentExited(ctx, node, data)
			}
			s.reply(msg, resp, err)
		}) {
			done()
			s.reply(msg, nil, connect.NewError(connect.CodeUnavailable, errors.New("hub busy or stopping; retry")))
		}
		return
	case bus.RPCRegisterAgent:
		// Registration takes the lifecycle lock; a pause it decides is sent
		// to the node only after the reply (gate).
		data := msg.Data
		if !s.ch.goWork(func(func()) {
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			gate := make(chan struct{})
			defer close(gate)
			resp, err := s.registerAgent(ctx, node, data, gate)
			s.reply(msg, resp, err)
		}) {
			s.reply(msg, nil, connect.NewError(connect.CodeUnavailable, errors.New("hub busy or stopping; retry")))
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	var (
		resp proto.Message
		err  error
	)
	switch method {
	case bus.RPCRefreshAgentToken:
		resp, err = s.refreshAgent(ctx, node, msg.Data)
	default:
		err = rpcErr(connect.CodeNotFound, "unknown method %s", method)
	}
	s.reply(msg, resp, err)
}

// rpcErr builds a node RPC error; its text is "<code>: <message>".
func rpcErr(code connect.Code, format string, args ...any) error {
	return connect.NewError(code, fmt.Errorf(format, args...))
}

// rpcErrorText renders err as "<code>: <message>". Errors that are not
// Connect errors never reach a node verbatim.
func (s *server) rpcErrorText(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		if ce.Message() == "" {
			return ce.Code().String() + ": " + ce.Code().String()
		}
		return ce.Code().String() + ": " + ce.Message()
	}
	s.log.Error("rpc failed", "err", err)
	return connect.CodeInternal.String() + ": internal error"
}

func (s *server) reply(msg *nats.Msg, resp proto.Message, rpcErr error) {
	out := nats.NewMsg(msg.Reply)
	if rpcErr != nil {
		out.Header.Set(rpcStatusHeader, bus.StatusError)
		out.Data, _ = proto.Marshal(&mycelv1.CtlResponse{Ok: false, Error: s.rpcErrorText(rpcErr)})
	} else {
		out.Header.Set(rpcStatusHeader, bus.StatusOK)
		b, err := proto.Marshal(resp)
		if err != nil {
			out.Header.Set(rpcStatusHeader, bus.StatusError)
			b, _ = proto.Marshal(&mycelv1.CtlResponse{Ok: false, Error: connect.CodeInternal.String() + ": internal error"})
		}
		out.Data = b
	}
	if err := s.nc.PublishMsg(out); err != nil {
		s.log.Warn("rpc reply", "subject", msg.Subject, "err", err)
	}
}

// validSandboxID: empty, or up to 128 chars of [A-Za-z0-9._:-].
func validSandboxID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '.' && c != '_' && c != ':' && c != '-' {
			return false
		}
	}
	return true
}

// validHarness: up to 64 chars of [a-z0-9-] (empty allowed).
func validHarness(h string) bool {
	if len(h) > 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func internalRPC() error { return rpcErr(connect.CodeInternal, "internal error") }

func (s *server) registerAgent(ctx context.Context, node string, data []byte, gate <-chan struct{}) (proto.Message, error) {
	actor := "node:" + node
	var req mycelv1.RegisterAgentRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
	}
	name := req.GetAgentName()
	agentID := "agent:" + name + "@" + node
	if !identity.ValidNodeName(name) {
		s.audit(ctx, actor, "agent.register", "agent", outcomeError, map[string]any{"reason": "invalid agent name"})
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid agent name: 1-63 chars of a-z, 0-9 and '-'")
	}
	if !validSandboxID(req.GetSandboxId()) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "invalid sandbox id"})
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid sandbox id: up to 128 chars of A-Z, a-z, 0-9, '.', '_', ':' and '-'")
	}
	if !validHarness(req.GetHarness()) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "invalid harness"})
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid harness: up to 64 chars of a-z, 0-9 and '-'")
	}
	if err := validBudget(req.GetBudget()); err != nil {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "invalid budget"})
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid budget: tokens and usd must be finite and not negative")
	}
	workdir := req.GetWorkdir()
	if len(workdir) > maxWorkdirLen || !channels.IsPrintable(workdir) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "invalid workdir"})
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid workdir: up to %d printable bytes", maxWorkdirLen)
	}
	n, err := s.st.getNode(ctx, node)
	if err != nil {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "unknown node"})
		return nil, rpcErr(connect.CodeNotFound, "unknown node")
	}
	claims := identity.AgentClaims{AgentID: agentID, NodeName: node, SandboxID: req.GetSandboxId(), OwnerID: n.OwnerID}
	token, err := s.signer.Issue(claims, identity.AgentTokenLifetime)
	if err != nil {
		s.log.Error("issue agent token", "err", err)
		return nil, internalRPC()
	}
	exp := formatTime(time.Now().Add(identity.AgentTokenLifetime).Truncate(time.Second))
	if v, err := s.signer.Verify(token); err == nil {
		exp = formatTime(v.ExpiresAt)
	}
	resp := &mycelv1.RegisterAgentResponse{AgentId: agentID, Token: token, ExpiresAt: exp}
	fx := &lifeFx{gate: gate}
	err = s.withLife(ctx, fx, func() error {
		return s.registerLocked(ctx, fx, node, n.OwnerID, agentID, &req, resp)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// registerLocked checks and stores a registration and fills the reply's
// state. Caller holds s.life.
func (s *server) registerLocked(ctx context.Context, fx *lifeFx, node, ownerID, agentID string,
	req *mycelv1.RegisterAgentRequest, resp *mycelv1.RegisterAgentResponse) error {
	actor := "node:" + node
	prev, err := s.st.getAgent(ctx, agentID)
	if err != nil && !errors.Is(err, errNotFound) {
		return s.internalError("get agent", err)
	}
	if err == nil && prev.RevokedAt == "" {
		if err := s.checkReRegister(ctx, node, prev, req); err != nil {
			return err
		}
	} else if err := s.checkSpawn(ctx, node, ownerID, agentID, req); err != nil {
		return err
	}
	created, a, err := s.st.upsertAgent(ctx, &agentRow{
		ID: agentID, Name: req.GetAgentName(), NodeName: node, SandboxID: req.GetSandboxId(), OwnerID: ownerID,
		Harness: req.GetHarness(), State: stateRunning, CreatedAt: nowString(), TokenExpiresAt: resp.GetExpiresAt(),
		Parent: req.GetParent(), HomeChannel: req.GetHomeChannel(),
		BudgetTokens: req.GetBudget().GetTokens(), BudgetUSD: req.GetBudget().GetUsd(), Workdir: req.GetWorkdir(),
	})
	if errors.Is(err, errTokenSpent) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeDenied, map[string]any{"reason": "agent revoked"})
		return rpcErr(connect.CodePermissionDenied, "agent revoked; register under another name")
	}
	if errors.Is(err, errTooManyAgents) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "agent limit reached"})
		return rpcErr(connect.CodeResourceExhausted, "too many agents on this node")
	}
	if err != nil {
		s.log.Error("store agent", "err", err)
		return internalRPC()
	}
	if home := a.HomeChannel; created && home != "" {
		// The worker can read its home channel at once (first registration
		// only: a worker removed from it later is not re-added).
		if _, err := s.st.getMember(ctx, home, agentID); errors.Is(err, errNotFound) {
			if err := s.st.addMember(ctx, home, agentID, memberAgent); err != nil {
				return s.internalError("add member", err)
			}
			s.audit(ctx, actor, "channel.join", "channel:"+home, outcomeOK, map[string]any{"member": agentID, "via": "home channel"})
		} else if err != nil {
			return s.internalError("get member", err)
		}
	}
	s.audit(ctx, actor, "agent.register", agentID, outcomeOK, map[string]any{
		"sandbox_id": req.GetSandboxId(), "harness": req.GetHarness(), "owner_id": ownerID, "expires_at": resp.GetExpiresAt(),
		"parent": a.Parent, "home_channel": a.HomeChannel, "first_registration": created,
		"budget_tokens": a.BudgetTokens, "budget_usd": a.BudgetUSD, "state": a.State, "paused_reason": a.PausedReason,
	})
	if created {
		harness := req.GetHarness()
		if harness == "" {
			harness = "unknown"
		}
		fx.post(func(ctx context.Context) {
			s.alert(ctx, node, "agent %s registered on node %s (harness %s)", agentID, node, harness)
		})
	}
	// A budget pause whose budget was lifted meanwhile ends; a fresh run
	// whose budget is already exhausted is paused (the node hears it from
	// the reply and from an AgentCtl sent after the reply).
	if isBudgetReason(a.PausedReason) {
		if err := s.autoResumeLocked(ctx, fx, hubActor.id, a, a.PausedReason); err != nil && !errors.Is(err, errBudgetExhausted) {
			return err
		}
	}
	if err := s.enforceLocked(ctx, fx, hubActor.id, a); err != nil {
		return err
	}
	resp.State, resp.PausedReason, resp.FirstRegistration = a.State, a.PausedReason, created
	return nil
}

// checkReRegister enforces what is fixed after the first registration: an
// orchestrator (no parent) stays one, a worker keeps its parent, and the
// home channel cannot change (an omitted home channel means unchanged).
func (s *server) checkReRegister(ctx context.Context, node string, prev *agentRow, req *mycelv1.RegisterAgentRequest) error {
	var reason string
	switch {
	case prev.Parent == "" && req.GetParent() != "":
		reason = "agent exists as an orchestrator"
	case prev.Parent != "" && req.GetParent() == "":
		reason = "agent exists as a worker of " + prev.Parent
	case req.GetParent() != prev.Parent:
		reason = "parent is fixed after the first registration (" + prev.Parent + ")"
	case req.GetHomeChannel() != "" && req.GetHomeChannel() != prev.HomeChannel:
		reason = "home channel is fixed after the first registration"
		if prev.HomeChannel != "" {
			reason += " (" + prev.HomeChannel + ")"
		}
	default:
		return nil
	}
	s.audit(ctx, "node:"+node, "agent.register", prev.ID, outcomeError, map[string]any{"reason": reason})
	return rpcErr(connect.CodeFailedPrecondition, "%s", reason)
}

// checkSpawn validates a first registration: the parent is a live, running
// agent of the same node, the home channel exists (not system, direct or
// archived), the spawner (parent, or the node owner for an agent without
// parent) is a member of it, and its budget is not exhausted.
func (s *server) checkSpawn(ctx context.Context, node, ownerID, agentID string, req *mycelv1.RegisterAgentRequest) error {
	actor := "node:" + node
	refuse := func(code connect.Code, reason string) error {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": reason})
		return rpcErr(code, "%s", reason)
	}
	parent := req.GetParent()
	if parent != "" {
		if parent == agentID {
			return refuse(connect.CodeInvalidArgument, "an agent cannot be its own parent")
		}
		p, err := s.st.getAgent(ctx, parent)
		if err != nil && !errors.Is(err, errNotFound) {
			return s.internalError("get parent", err)
		}
		if err != nil || p.NodeName != node || p.RevokedAt != "" || p.State == stateStopped {
			return refuse(connect.CodeInvalidArgument, "parent must be a live agent on this node")
		}
		if p.State == statePaused {
			return refuse(connect.CodeFailedPrecondition, "parent agent is paused ("+p.PausedReason+")")
		}
	}
	home := req.GetHomeChannel()
	if home == "" {
		return nil
	}
	if !channels.ValidName(home) {
		return refuse(connect.CodeInvalidArgument, "invalid home channel name")
	}
	ch, err := s.st.getChannel(ctx, home)
	if errors.Is(err, errNotFound) {
		return refuse(connect.CodeFailedPrecondition, "home channel "+home+" does not exist")
	}
	if err != nil {
		return s.internalError("get channel", err)
	}
	switch {
	case ch.ArchivedAt != "":
		return refuse(connect.CodeFailedPrecondition, "home channel "+home+" is archived")
	case ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM:
		return refuse(connect.CodeFailedPrecondition, "a system channel cannot be a home channel")
	case ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_DIRECT:
		return refuse(connect.CodeFailedPrecondition, "a direct channel cannot be a home channel")
	}
	spawner := parent
	if spawner == "" {
		spawner = userMemberID(ownerID)
	}
	member, _, err := s.membership(ctx, ch, spawner)
	if err != nil {
		return s.internalError("membership", err)
	}
	if !member {
		return refuse(connect.CodeFailedPrecondition, spawner+" is not a member of home channel "+home)
	}
	_, _, ex, err := s.channelBudget(ctx, home)
	if err != nil {
		return s.internalError("channel budget", err)
	}
	if ex {
		return refuse(connect.CodeFailedPrecondition, "home channel "+home+" budget exhausted; raise it or reset usage")
	}
	return nil
}

// refreshAgent re-issues an agent token. A revoked agent's existing token
// stays cryptographically valid until it expires (at most 1 h); revocation
// takes effect here and on every agent RPC (see agentAuth).
func (s *server) refreshAgent(ctx context.Context, node string, data []byte) (proto.Message, error) {
	actor := "node:" + node
	var req mycelv1.RefreshAgentTokenRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
	}
	deny := func(resource, reason string) error {
		s.audit(ctx, actor, "agent.refresh", resource, outcomeDenied, map[string]any{"reason": reason})
		if reason == "agent revoked" {
			return rpcErr(connect.CodePermissionDenied, "agent revoked")
		}
		return rpcErr(connect.CodeUnauthenticated, "invalid agent token")
	}
	claims, err := s.signer.Verify(req.GetToken())
	if err != nil {
		return nil, deny(req.GetAgentId(), "token does not verify")
	}
	// The binding check: a token is only good over its own node's connection.
	if claims.NodeName != node {
		return nil, deny(claims.AgentID, "token bound to another node")
	}
	if s.st.agentRevoked(claims.AgentID) {
		return nil, deny(claims.AgentID, "agent revoked")
	}
	if req.GetAgentId() != "" && req.GetAgentId() != claims.AgentID {
		return nil, deny(claims.AgentID, "agent id mismatch")
	}
	a, err := s.st.getAgent(ctx, claims.AgentID)
	if err != nil {
		return nil, deny(claims.AgentID, "unknown agent")
	}
	if a.RevokedAt != "" {
		return nil, deny(claims.AgentID, "agent revoked")
	}
	if a.NodeName != node || a.SandboxID != claims.SandboxID {
		return nil, deny(claims.AgentID, "agent binding changed")
	}
	if a.State == stateStopped {
		s.audit(ctx, actor, "agent.refresh", claims.AgentID, outcomeDenied, map[string]any{"reason": "agent stopped"})
		return nil, rpcErr(connect.CodePermissionDenied, "agent stopped")
	}
	token, err := s.signer.Issue(identity.AgentClaims{
		AgentID: claims.AgentID, NodeName: node, SandboxID: claims.SandboxID, OwnerID: a.OwnerID,
	}, identity.AgentTokenLifetime)
	if err != nil {
		s.log.Error("issue agent token", "err", err)
		return nil, internalRPC()
	}
	exp := formatTime(time.Now().Add(identity.AgentTokenLifetime))
	if v, err := s.signer.Verify(token); err == nil {
		exp = formatTime(v.ExpiresAt)
	}
	if err := s.st.setAgentTokenExpiry(ctx, claims.AgentID, exp); err != nil {
		s.log.Warn("store agent expiry", "err", err)
	}
	s.audit(ctx, actor, "agent.refresh", claims.AgentID, outcomeOK, map[string]any{"expires_at": exp})
	return &mycelv1.RefreshAgentTokenResponse{Token: token, ExpiresAt: exp}, nil
}
