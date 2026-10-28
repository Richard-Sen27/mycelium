package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// ---- conversions ----

func roleToProto(r string) mycelv1.Role {
	switch r {
	case roleAdmin:
		return mycelv1.Role_ROLE_ADMIN
	case roleMember:
		return mycelv1.Role_ROLE_MEMBER
	default:
		return mycelv1.Role_ROLE_UNSPECIFIED
	}
}

func userToProto(u *userRow) *mycelv1.User {
	return &mycelv1.User{Id: u.ID, Name: u.Name, Role: roleToProto(u.Role), CreatedAt: u.CreatedAt}
}

func tokenToProto(t *tokenRow) *mycelv1.JoinToken {
	return &mycelv1.JoinToken{
		Id: t.ID, NodeName: t.NodeName, Caps: &mycelv1.Capabilities{Names: t.Caps}, OwnerId: t.OwnerID,
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, UsedAt: t.UsedAt, RevokedAt: t.RevokedAt,
	}
}

func agentToProto(a *agentRow) *mycelv1.Agent {
	return &mycelv1.Agent{
		Id: a.ID, Name: a.Name, NodeName: a.NodeName, SandboxId: a.SandboxID, OwnerId: a.OwnerID,
		Harness: a.Harness, State: a.State, CreatedAt: a.CreatedAt, TokenExpiresAt: a.TokenExpiresAt,
		Parent: a.Parent, HomeChannel: a.HomeChannel,
		Budget:       &mycelv1.Budget{Tokens: a.BudgetTokens, Usd: a.BudgetUSD},
		Used:         &mycelv1.Budget{Tokens: a.UsedTokens, Usd: a.UsedUSD},
		Turns:        clampInt32(a.Turns),
		PausedReason: a.PausedReason, ExitedAt: a.ExitedAt, ExitCode: clampInt32(a.ExitCode),
	}
}

func clampInt32(v int64) int32 {
	return int32(max(min(v, math.MaxInt32), math.MinInt32))
}

func (s *server) nodeToProto(n *nodeRow, online bool) *mycelv1.Node {
	out := &mycelv1.Node{
		Id: "node:" + n.Name, Name: n.Name, SpiffeId: n.SPIFFEID, OwnerId: n.OwnerID,
		CapsCeiling: &mycelv1.Capabilities{Names: n.CapsCeiling},
		CapsEnabled: &mycelv1.Capabilities{Names: n.CapsEnabled},
		Online:      online, LastSeenAt: n.LastSeenAt, CertSerial: n.CertSerial,
		CertNotAfter: n.CertNotAfter, CreatedAt: n.CreatedAt,
	}
	if len(n.Inventory) > 0 {
		var inv mycelv1.Inventory
		if err := proto.Unmarshal(n.Inventory, &inv); err == nil {
			out.Inventory = &inv
		} else {
			s.log.Warn("stored inventory does not decode", "node", n.Name, "err", err)
		}
	}
	return out
}

func (s *server) onlineSet() map[string]bool {
	set := map[string]bool{}
	for _, n := range s.bus.ConnectedNodes() {
		set[n] = true
	}
	return set
}

// validateCaps checks caps against the known set and returns them sorted
// and de-duplicated.
func validateCaps(caps []string) ([]string, error) {
	out := []string{}
	for _, c := range caps {
		c = strings.TrimSpace(c)
		if !slices.Contains(knownCaps, c) {
			return nil, fmt.Errorf("unknown capability %q (known: %s)", c, strings.Join(knownCaps, ", "))
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out, nil
}

func invalidArg(format string, a ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, a...))
}

func notFound(what string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(what+" not found"))
}

// notFoundAfterAuth answers a lookup miss on an owner-scoped action: callers
// the policy would deny without knowing the owner get PermissionDenied, so
// they cannot probe which ids exist.
func (s *server) notFoundAfterAuth(ctx context.Context, u *userRow, a policy.Action, kind, id string) error {
	if err := s.authorize(ctx, u, a, policy.Resource{Kind: kind, ID: id}, false); err != nil {
		return err
	}
	return notFound(kind)
}

// ---- TokenService ----

type tokenService struct{ s *server }

func (t *tokenService) CreateJoinToken(ctx context.Context, req *connect.Request[mycelv1.CreateJoinTokenRequest]) (*connect.Response[mycelv1.CreateJoinTokenResponse], error) {
	s := t.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetNodeName()
	if err := s.authorize(ctx, u, policy.ActionTokenCreate, policy.Resource{Kind: "token", OwnerID: u.ID}, true); err != nil {
		return nil, err
	}
	if !identity.ValidNodeName(name) {
		return nil, invalidArg("invalid node name %q: 1-63 chars of a-z, 0-9 and '-', not starting or ending with '-'", name)
	}
	caps, err := validateCaps(req.Msg.GetCaps().GetNames())
	if err != nil {
		return nil, invalidArg("%v", err)
	}
	ttl := identity.JoinTokenDefault
	if v := req.Msg.GetTtl(); v != "" {
		ttl, err = time.ParseDuration(v)
		if err != nil || ttl <= 0 {
			return nil, invalidArg("invalid ttl %q", v)
		}
		if ttl > identity.JoinTokenMax {
			return nil, invalidArg("ttl %s exceeds the maximum of %s", ttl, identity.JoinTokenMax)
		}
	}
	exists, err := s.st.nodeExists(ctx, name)
	if err != nil {
		return nil, s.internalError("node lookup", err)
	}
	if exists {
		s.audit(ctx, u.ID, string(policy.ActionTokenCreate), "node:"+name, outcomeError, map[string]any{"reason": "node exists"})
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("a node named %q already exists", name))
	}
	jt, err := identity.NewJoinToken(s.cfg.AdvertiseURL, s.ca.Fingerprint())
	if err != nil {
		return nil, s.internalError("new join token", err)
	}
	now := time.Now()
	row := &tokenRow{
		ID: jt.ID, SecretHash: jt.SecretHash(), NodeName: name, Caps: caps, OwnerID: u.ID,
		CreatedAt: formatTime(now), ExpiresAt: formatTime(now.Add(ttl)),
	}
	if err := s.st.createJoinToken(ctx, row); err != nil {
		return nil, s.internalError("store join token", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionTokenCreate), "token:"+row.ID, outcomeOK,
		map[string]any{"node_name": name, "caps": caps, "expires_at": row.ExpiresAt})
	return connect.NewResponse(&mycelv1.CreateJoinTokenResponse{Token: jt.String(), JoinToken: tokenToProto(row)}), nil
}

func (t *tokenService) ListJoinTokens(ctx context.Context, _ *connect.Request[mycelv1.ListJoinTokensRequest]) (*connect.Response[mycelv1.ListJoinTokensResponse], error) {
	s := t.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionTokenList, policy.Resource{}, false); err != nil {
		return nil, err
	}
	rows, err := s.st.listJoinTokens(ctx)
	if err != nil {
		return nil, s.internalError("list tokens", err)
	}
	out := &mycelv1.ListJoinTokensResponse{}
	for _, r := range rows {
		if u.Role != roleAdmin && r.OwnerID != u.ID {
			continue
		}
		out.Tokens = append(out.Tokens, tokenToProto(r))
	}
	return connect.NewResponse(out), nil
}

func (t *tokenService) RevokeJoinToken(ctx context.Context, req *connect.Request[mycelv1.RevokeJoinTokenRequest]) (*connect.Response[mycelv1.RevokeJoinTokenResponse], error) {
	s := t.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetId()
	row, err := s.st.getJoinToken(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, policy.ActionTokenRevoke, "token", id)
	}
	if err != nil {
		return nil, s.internalError("get token", err)
	}
	if err := s.authorize(ctx, u, policy.ActionTokenRevoke, policy.Resource{Kind: "token", ID: id, OwnerID: row.OwnerID}, true); err != nil {
		return nil, err
	}
	if err := s.st.revokeJoinToken(ctx, id); err != nil {
		return nil, s.internalError("revoke token", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionTokenRevoke), "token:"+id, outcomeOK, map[string]any{"node_name": row.NodeName})
	return connect.NewResponse(&mycelv1.RevokeJoinTokenResponse{}), nil
}

// ---- NodeService ----

type nodeService struct{ s *server }

func (n *nodeService) ListNodes(ctx context.Context, _ *connect.Request[mycelv1.ListNodesRequest]) (*connect.Response[mycelv1.ListNodesResponse], error) {
	s := n.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionNodeList, policy.Resource{}, false); err != nil {
		return nil, err
	}
	rows, err := s.st.listNodes(ctx)
	if err != nil {
		return nil, s.internalError("list nodes", err)
	}
	online := s.onlineSet()
	out := &mycelv1.ListNodesResponse{}
	for _, r := range rows {
		out.Nodes = append(out.Nodes, s.nodeToProto(r, online[r.Name]))
	}
	return connect.NewResponse(out), nil
}

func (n *nodeService) GetNode(ctx context.Context, req *connect.Request[mycelv1.GetNodeRequest]) (*connect.Response[mycelv1.GetNodeResponse], error) {
	s := n.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.st.getNode(ctx, req.Msg.GetName())
	if errors.Is(err, errNotFound) {
		return nil, notFound("node")
	}
	if err != nil {
		return nil, s.internalError("get node", err)
	}
	if err := s.authorize(ctx, u, policy.ActionNodeGet, policy.Resource{Kind: "node", ID: row.Name, OwnerID: row.OwnerID}, false); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetNodeResponse{Node: s.nodeToProto(row, s.onlineSet()[row.Name])}), nil
}

func (n *nodeService) RemoveNode(ctx context.Context, req *connect.Request[mycelv1.RemoveNodeRequest]) (*connect.Response[mycelv1.RemoveNodeResponse], error) {
	s := n.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	row, err := s.st.getNode(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, policy.ActionNodeRemove, "node", name)
	}
	if err != nil {
		return nil, s.internalError("get node", err)
	}
	if err := s.authorize(ctx, u, policy.ActionNodeRemove, policy.Resource{Kind: "node", ID: name, OwnerID: row.OwnerID}, true); err != nil {
		return nil, err
	}
	// Under the lifecycle lock, so no pause or resume races the revocation
	// of the node's agents.
	s.life.Lock()
	_, err = s.st.removeNode(ctx, name)
	s.life.Unlock()
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, notFound("node")
		}
		return nil, s.internalError("remove node", err)
	}
	// The certificate is revoked in the store now; tell the node to forget
	// its identity (best effort) and drop its connections.
	s.sendCtl(ctx, name, &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Revoked{Revoked: &mycelv1.Revoked{Reason: "node removed"}}}, true)
	dropped := s.bus.Disconnect(name)
	s.beats.forget(name)
	s.audit(ctx, u.ID, string(policy.ActionNodeRemove), "node:"+name, outcomeOK,
		map[string]any{"owner_id": row.OwnerID, "connections_dropped": dropped})
	s.alert(ctx, name, "node %s removed by %s", name, u.ID)
	return connect.NewResponse(&mycelv1.RemoveNodeResponse{}), nil
}

func (n *nodeService) SetNodeCaps(ctx context.Context, req *connect.Request[mycelv1.SetNodeCapsRequest]) (*connect.Response[mycelv1.SetNodeCapsResponse], error) {
	s := n.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	row, err := s.st.getNode(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, policy.ActionNodeSetCaps, "node", name)
	}
	if err != nil {
		return nil, s.internalError("get node", err)
	}
	if err := s.authorize(ctx, u, policy.ActionNodeSetCaps, policy.Resource{Kind: "node", ID: name, OwnerID: row.OwnerID}, true); err != nil {
		return nil, err
	}
	caps, err := validateCaps(req.Msg.GetCapsEnabled().GetNames())
	if err != nil {
		return nil, invalidArg("%v", err)
	}
	for _, c := range caps {
		if !slices.Contains(row.CapsCeiling, c) {
			return nil, invalidArg("capability %q is outside the node's ceiling (%s)", c, strings.Join(row.CapsCeiling, ", "))
		}
	}
	if err := s.st.setNodeCaps(ctx, name, caps); err != nil {
		return nil, s.internalError("set caps", err)
	}
	row.CapsEnabled = caps
	s.sendCtl(ctx, name, &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_SetCaps{SetCaps: &mycelv1.SetCaps{CapsEnabled: &mycelv1.Capabilities{Names: caps}}}}, false)
	s.audit(ctx, u.ID, string(policy.ActionNodeSetCaps), "node:"+name, outcomeOK, map[string]any{"caps_enabled": caps})
	return connect.NewResponse(&mycelv1.SetNodeCapsResponse{Node: s.nodeToProto(row, s.onlineSet()[name])}), nil
}

// sendCtl sends a control command to a node. With wait it is a request with
// a 1 s timeout; otherwise a fire-and-forget publish. Failures are logged only.
func (s *server) sendCtl(_ context.Context, node string, req *mycelv1.CtlRequest, wait bool) {
	b, err := proto.Marshal(req)
	if err != nil {
		s.log.Error("marshal ctl", "err", err)
		return
	}
	if !wait {
		if err := s.nc.Publish(bus.NodeCtl(node), b); err != nil {
			s.log.Debug("ctl publish failed", "node", node, "err", err)
		}
		return
	}
	if _, err := s.nc.Request(bus.NodeCtl(node), b, time.Second); err != nil {
		s.log.Debug("ctl request not answered", "node", node, "err", err)
	}
}

// ---- UserService ----

type userService struct{ s *server }

func roleFromProto(r mycelv1.Role) (string, bool) {
	switch r {
	case mycelv1.Role_ROLE_ADMIN:
		return roleAdmin, true
	case mycelv1.Role_ROLE_MEMBER:
		return roleMember, true
	default:
		return "", false
	}
}

func (us *userService) CreateUser(ctx context.Context, req *connect.Request[mycelv1.CreateUserRequest]) (*connect.Response[mycelv1.CreateUserResponse], error) {
	s := us.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := s.authorize(ctx, u, policy.ActionUserCreate, policy.Resource{Kind: "user"}, true); err != nil {
		return nil, err
	}
	if !validUserName(name) {
		return nil, invalidArg("invalid user name %q: 1-63 chars of a-z, 0-9 and '-'", name)
	}
	role, ok := roleFromProto(req.Msg.GetRole())
	if !ok {
		return nil, invalidArg("role must be admin or member")
	}
	nu, k, secret, err := s.st.createUser(ctx, name, role)
	if errors.Is(err, errConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("user %q already exists", name))
	}
	if err != nil {
		return nil, s.internalError("create user", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionUserCreate), "user:"+nu.ID, outcomeOK,
		map[string]any{"name": name, "role": role, "api_key_id": k.ID})
	return connect.NewResponse(&mycelv1.CreateUserResponse{User: userToProto(nu), ApiKey: secret}), nil
}

func (us *userService) ListUsers(ctx context.Context, _ *connect.Request[mycelv1.ListUsersRequest]) (*connect.Response[mycelv1.ListUsersResponse], error) {
	s := us.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionUserList, policy.Resource{}, false); err != nil {
		return nil, err
	}
	rows, err := s.st.listUsers(ctx)
	if err != nil {
		return nil, s.internalError("list users", err)
	}
	out := &mycelv1.ListUsersResponse{}
	for _, r := range rows {
		out.Users = append(out.Users, userToProto(r))
	}
	return connect.NewResponse(out), nil
}

func (us *userService) RemoveUser(ctx context.Context, req *connect.Request[mycelv1.RemoveUserRequest]) (*connect.Response[mycelv1.RemoveUserResponse], error) {
	s := us.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetId()
	if err := s.authorize(ctx, u, policy.ActionUserRemove, policy.Resource{Kind: "user", ID: id, OwnerID: id}, true); err != nil {
		return nil, err
	}
	err = s.st.removeUser(ctx, id)
	switch {
	case errors.Is(err, errNotFound):
		return nil, notFound("user")
	case errors.Is(err, errLastAdmin), errors.Is(err, errOwnsNodes):
		s.audit(ctx, u.ID, string(policy.ActionUserRemove), "user:"+id, outcomeError, map[string]any{"reason": err.Error()})
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, s.internalError("remove user", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionUserRemove), "user:"+id, outcomeOK, nil)
	return connect.NewResponse(&mycelv1.RemoveUserResponse{}), nil
}

func (us *userService) CreateApiKey(ctx context.Context, req *connect.Request[mycelv1.CreateApiKeyRequest]) (*connect.Response[mycelv1.CreateApiKeyResponse], error) {
	s := us.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	target := req.Msg.GetUserId()
	if target == "" {
		target = u.ID
	}
	if err := s.authorize(ctx, u, policy.ActionAPIKeyCreate, policy.Resource{Kind: "user", ID: target, OwnerID: target}, true); err != nil {
		return nil, err
	}
	if _, err := s.st.getUser(ctx, target); errors.Is(err, errNotFound) {
		return nil, notFound("user")
	} else if err != nil {
		return nil, s.internalError("get user", err)
	}
	k, secret, err := s.st.createAPIKey(ctx, target)
	if err != nil {
		return nil, s.internalError("create api key", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionAPIKeyCreate), "user:"+target, outcomeOK, map[string]any{"api_key_id": k.ID})
	return connect.NewResponse(&mycelv1.CreateApiKeyResponse{
		Key:    &mycelv1.ApiKey{Id: k.ID, UserId: k.UserID, Scopes: k.Scopes, CreatedAt: k.CreatedAt},
		ApiKey: secret,
	}), nil
}

func (us *userService) Whoami(ctx context.Context, _ *connect.Request[mycelv1.WhoamiRequest]) (*connect.Response[mycelv1.WhoamiResponse], error) {
	s := us.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.WhoamiResponse{
		User: userToProto(u), HubUrl: s.cfg.AdvertiseURL, TrustDomain: string(s.td), CaFingerprint: s.ca.Fingerprint(),
	}), nil
}

// ---- AgentService ----

// agentService implements mycelv1connect.AgentServiceHandler; the phase 3
// methods are in agent_api.go.
type agentService struct {
	s *server
}

var _ mycelv1connect.AgentServiceHandler = (*agentService)(nil)

func (as *agentService) ListAgents(ctx context.Context, _ *connect.Request[mycelv1.ListAgentsRequest]) (*connect.Response[mycelv1.ListAgentsResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionAgentList, policy.Resource{}, false); err != nil {
		return nil, err
	}
	rows, err := s.st.listAgents(ctx)
	if err != nil {
		return nil, s.internalError("list agents", err)
	}
	out := &mycelv1.ListAgentsResponse{}
	for _, r := range rows {
		out.Agents = append(out.Agents, agentToProto(r))
	}
	return connect.NewResponse(out), nil
}

func (as *agentService) RevokeAgent(ctx context.Context, req *connect.Request[mycelv1.RevokeAgentRequest]) (*connect.Response[mycelv1.RevokeAgentResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetId()
	a, err := s.st.getAgent(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, policy.ActionAgentRevoke, "agent", id)
	}
	if err != nil {
		return nil, s.internalError("get agent", err)
	}
	if err := s.authorize(ctx, u, policy.ActionAgentRevoke, policy.Resource{Kind: "agent", ID: id, OwnerID: a.OwnerID}, true); err != nil {
		return nil, err
	}
	// Later phases also tear down the agent's sandbox here.
	s.life.Lock()
	err = s.st.revokeAgent(ctx, id)
	s.life.Unlock()
	if err != nil {
		return nil, s.internalError("revoke agent", err)
	}
	res := id
	if !strings.HasPrefix(res, "agent:") {
		res = "agent:" + res
	}
	s.audit(ctx, u.ID, string(policy.ActionAgentRevoke), res, outcomeOK, map[string]any{"node_name": a.NodeName})
	return connect.NewResponse(&mycelv1.RevokeAgentResponse{}), nil
}
