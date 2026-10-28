package hub

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// grantService implements mycelv1connect.GrantServiceHandler for humans.
type grantService struct{ s *server }

var (
	_ mycelv1connect.GrantServiceHandler   = (*grantService)(nil)
	_ mycelv1connect.ControlServiceHandler = (*controlService)(nil)
)

func (gs *grantService) RequestGrant(ctx context.Context, req *connect.Request[mycelv1.RequestGrantRequest]) (*connect.Response[mycelv1.RequestGrantResponse], error) {
	s := gs.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	g, err := s.requestGrant(ctx, userActor(u), grantRequest{
		subject: m.GetSubject(), spec: m.GetSpec(), duration: m.GetDuration(), reason: m.GetReason(), channel: m.GetChannel(),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.RequestGrantResponse{Grant: g}), nil
}

func (gs *grantService) ListGrants(ctx context.Context, req *connect.Request[mycelv1.ListGrantsRequest]) (*connect.Response[mycelv1.ListGrantsResponse], error) {
	s := gs.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionGrantList, policy.Resource{Kind: "grant"}, false); err != nil {
		return nil, err
	}
	f := grantFilter{node: req.Msg.GetNode(), subject: req.Msg.GetSubject(), limit: maxGrantList}
	if u.Role != roleAdmin {
		// Members see the grants of subjects they own and the grants on
		// nodes they own, filtered before the limit.
		f.visibleTo = u.ID
	}
	switch {
	case req.Msg.GetPendingOnly():
		f.states = []mycelv1.GrantState{mycelv1.GrantState_GRANT_STATE_PENDING}
	case !req.Msg.GetIncludeFinished():
		f.states = openStates
	}
	rows, err := s.st.listGrants(ctx, f)
	if err != nil {
		return nil, s.internalError("list grants", err)
	}
	return connect.NewResponse(&mycelv1.ListGrantsResponse{Grants: rows}), nil
}

func (gs *grantService) GetGrant(ctx context.Context, req *connect.Request[mycelv1.GetGrantRequest]) (*connect.Response[mycelv1.GetGrantResponse], error) {
	s := gs.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	g, err := s.loadGrant(ctx, u, policy.ActionGrantGet, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	ok, reason, err := s.userGrantAllowed(ctx, u, policy.ActionGrantGet, g)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(reason))
	}
	return connect.NewResponse(&mycelv1.GetGrantResponse{Grant: proto.CloneOf(g)}), nil
}

func (gs *grantService) ApproveGrant(ctx context.Context, req *connect.Request[mycelv1.ApproveGrantRequest]) (*connect.Response[mycelv1.ApproveGrantResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	g, err := gs.s.approveGrant(ctx, u, req.Msg.GetId(), req.Msg.GetDuration(), req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ApproveGrantResponse{Grant: g}), nil
}

func (gs *grantService) DenyGrant(ctx context.Context, req *connect.Request[mycelv1.DenyGrantRequest]) (*connect.Response[mycelv1.DenyGrantResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	g, err := gs.s.denyGrant(ctx, u, req.Msg.GetId(), req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.DenyGrantResponse{Grant: g}), nil
}

func (gs *grantService) RevokeGrant(ctx context.Context, req *connect.Request[mycelv1.RevokeGrantRequest]) (*connect.Response[mycelv1.RevokeGrantResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	g, err := gs.s.revokeGrant(ctx, u, req.Msg.GetId(), req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.RevokeGrantResponse{Grant: g}), nil
}
