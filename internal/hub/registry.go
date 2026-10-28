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
			if method == bus.RPCAgentInboxPull {
				timeout += maxInboxWait
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
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	var (
		resp proto.Message
		err  error
	)
	switch method {
	case bus.RPCRegisterAgent:
		resp, err = s.registerAgent(ctx, node, msg.Data)
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

func (s *server) registerAgent(ctx context.Context, node string, data []byte) (proto.Message, error) {
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
	created, err := s.st.upsertAgent(ctx, &agentRow{
		ID: agentID, Name: name, NodeName: node, SandboxID: req.GetSandboxId(), OwnerID: n.OwnerID,
		Harness: req.GetHarness(), State: "running", CreatedAt: nowString(), TokenExpiresAt: exp,
	})
	if errors.Is(err, errTokenSpent) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeDenied, map[string]any{"reason": "agent revoked"})
		return nil, rpcErr(connect.CodePermissionDenied, "agent revoked; register under another name")
	}
	if errors.Is(err, errTooManyAgents) {
		s.audit(ctx, actor, "agent.register", agentID, outcomeError, map[string]any{"reason": "agent limit reached"})
		return nil, rpcErr(connect.CodeResourceExhausted, "too many agents on this node")
	}
	if err != nil {
		s.log.Error("store agent", "err", err)
		return nil, internalRPC()
	}
	s.audit(ctx, actor, "agent.register", agentID, outcomeOK,
		map[string]any{"sandbox_id": req.GetSandboxId(), "harness": req.GetHarness(), "owner_id": n.OwnerID, "expires_at": exp})
	if created {
		harness := req.GetHarness()
		if harness == "" {
			harness = "unknown"
		}
		s.alert(ctx, node, "agent %s registered on node %s (harness %s)", agentID, node, harness)
	}
	return &mycelv1.RegisterAgentResponse{AgentId: agentID, Token: token, ExpiresAt: exp}, nil
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
