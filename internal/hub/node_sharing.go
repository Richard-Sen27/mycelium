package hub

import (
	"context"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/policy"
	"github.com/richard-sen27/mycelium/internal/sharing"
)

// Node sharing settings (spec: "Sharing settings per node", "Node owner has
// the final say"). The hub stores the owner's settings within the join
// token's ceiling and the node's last reported local ceiling; the
// effective settings are their conjunction (sharing.Effective), never
// beyond the ceiling.

// maxSharingBytes bounds a reported or submitted settings message.
const maxSharingBytes = 16 << 10

// normalizeOffers sorts the offers of s in place; a nil Offer (every
// offer) is left alone.
func normalizeOffers(s *mycelv1.SharingSettings) {
	if s.Offer == nil {
		return
	}
	names := slices.Clone(s.Offer.Names)
	slices.Sort(names)
	s.Offer.Names = slices.Compact(names)
}

// sharingView is a node's hub-side, local and effective settings. The
// effective offers never exceed the join token's ceiling.
func (s *server) sharingView(ctx context.Context, n *nodeRow) (*mycelv1.GetNodeSharingResponse, error) {
	hub, local, err := s.st.nodeSharing(ctx, n.Name)
	if err != nil {
		return nil, s.internalError("node sharing", err)
	}
	if hub == nil {
		hub = sharing.Default(n.CapsCeiling)
	}
	eff := sharing.Effective(hub, local)
	if eff == nil {
		eff = &mycelv1.SharingSettings{}
	}
	eff = proto.CloneOf(eff)
	var offers []string
	for _, o := range eff.GetOffer().GetNames() {
		if slices.Contains(n.CapsCeiling, o) {
			offers = append(offers, o)
		}
	}
	eff.Offer = &mycelv1.Capabilities{Names: offers}
	normalizeOffers(eff)
	return &mycelv1.GetNodeSharingResponse{Hub: hub, Local: local, Effective: eff}, nil
}

// effectiveOffers is what n offers right now as far as the hub knows.
func (s *server) effectiveOffers(ctx context.Context, n *nodeRow) ([]string, error) {
	v, err := s.sharingView(ctx, n)
	if err != nil {
		return nil, err
	}
	return v.GetEffective().GetOffer().GetNames(), nil
}

// controlService implements mycelv1connect.ControlServiceHandler; the
// freezes are in freeze.go.
type controlService struct{ s *server }

// loadNodeFor loads a node and checks action for u.
func (s *server) loadNodeFor(ctx context.Context, u *userRow, action policy.Action, name string, audit bool) (*nodeRow, error) {
	n, err := s.st.getNode(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, action, "node", name)
	}
	if err != nil {
		return nil, s.internalError("get node", err)
	}
	if err := s.authorize(ctx, u, action, policy.Resource{Kind: "node", ID: n.Name, OwnerID: n.OwnerID}, audit); err != nil {
		return nil, err
	}
	return n, nil
}

func (c *controlService) GetNodeSharing(ctx context.Context, req *connect.Request[mycelv1.GetNodeSharingRequest]) (*connect.Response[mycelv1.GetNodeSharingResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.loadNodeFor(ctx, u, policy.ActionNodeSharingGet, req.Msg.GetName(), false)
	if err != nil {
		return nil, err
	}
	v, err := s.sharingView(ctx, n)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func (c *controlService) SetNodeSharing(ctx context.Context, req *connect.Request[mycelv1.SetNodeSharingRequest]) (*connect.Response[mycelv1.SetNodeSharingResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.loadNodeFor(ctx, u, policy.ActionNodeSharingSet, req.Msg.GetName(), true)
	if err != nil {
		return nil, err
	}
	set := req.Msg.GetSettings()
	if set == nil {
		return nil, invalidArg("missing settings")
	}
	if proto.Size(set) > maxSharingBytes {
		return nil, invalidArg("settings exceed %d bytes", maxSharingBytes)
	}
	set = proto.CloneOf(set)
	if err := sharing.Validate(set); err != nil {
		return nil, invalidArg("%v", err)
	}
	if set.Offer == nil {
		// No offer list means everything: within the ceiling, that is the ceiling.
		set.Offer = &mycelv1.Capabilities{Names: sharing.Default(n.CapsCeiling).GetOffer().GetNames()}
	}
	normalizeOffers(set)
	for _, o := range set.GetOffer().GetNames() {
		if !slices.Contains(n.CapsCeiling, o) {
			s.audit(ctx, u.ID, string(policy.ActionNodeSharingSet), "node:"+n.Name, outcomeDenied,
				map[string]any{"reason": "outside the ceiling", "offer": o})
			return nil, invalidArg("offer %q is outside the node's ceiling (%s)", o, strings.Join(n.CapsCeiling, ", "))
		}
	}
	if set.GetWhen() == "" {
		set.When = sharing.WhenAlways
	}
	if err := s.st.setNodeSharing(ctx, n.Name, "hub", set); err != nil {
		return nil, s.internalError("set node sharing", err)
	}
	// Open grants of a kind the node no longer offers end now.
	offers, err := s.effectiveOffers(ctx, n)
	if err != nil {
		return nil, err
	}
	ended := s.endGrantsWhere(ctx, grantFilter{node: n.Name}, u.ID, "no longer offered by the node", true,
		func(g *mycelv1.Grant) bool { return !slices.Contains(offers, kindOffer(grantKind(g.GetSpec()))) })
	s.enqueueCtl(n.Name, ctlItem{req: &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Sharing{Sharing: proto.CloneOf(set)}}})
	s.audit(ctx, u.ID, string(policy.ActionNodeSharingSet), "node:"+n.Name, outcomeOK, map[string]any{
		"offer": set.GetOffer().GetNames(), "when": set.GetWhen(), "local_first": set.GetLocalFirst(),
		"max_jobs": set.GetMaxJobs(), "idle_after_minutes": set.GetIdleAfterMinutes(), "windows": len(set.GetSchedule()),
		"grants_ended": ended,
	})
	v, err := s.sharingView(ctx, n)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.SetNodeSharingResponse{Sharing: v}), nil
}

// nodeSharingReport serves node.sharing: the node reports its local
// ceiling and gets the owner's hub-side settings back.
func (s *server) nodeSharingReport(ctx context.Context, node string, req *mycelv1.NodeSharingRequest) (proto.Message, error) {
	n, err := s.st.getNode(ctx, node)
	if errors.Is(err, errNotFound) {
		return nil, rpcErr(connect.CodeNotFound, "unknown node")
	}
	if err != nil {
		return nil, s.internalError("get node", err)
	}
	local := req.GetLocal()
	if local != nil {
		if proto.Size(local) > maxSharingBytes {
			return nil, rpcErr(connect.CodeInvalidArgument, "settings exceed %d bytes", maxSharingBytes)
		}
		local = proto.CloneOf(local)
		if err := sharing.Validate(local); err != nil {
			s.auditDenied(ctx, "node:"+node, "node.sharing", "node:"+node, map[string]any{"reason": err.Error()})
			return nil, rpcErr(connect.CodeInvalidArgument, "invalid settings: %v", err)
		}
		normalizeOffers(local)
	}
	if err := s.st.setNodeSharing(ctx, node, "local", local); err != nil {
		return nil, s.internalError("store local sharing", err)
	}
	details := map[string]any{"local_set": local != nil}
	if local != nil {
		details["offer"] = local.GetOffer().GetNames()
		details["when"] = local.GetWhen()
	}
	s.audit(ctx, "node:"+node, "node.sharing", "node:"+node, outcomeOK, details)
	v, err := s.sharingView(ctx, n)
	if err != nil {
		return nil, err
	}
	return &mycelv1.NodeSharingResponse{Hub: v.GetHub()}, nil
}
