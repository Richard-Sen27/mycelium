package hub

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// Kill switch levels two and three (spec: "Kill switch"; docs/decisions.md
// D024). A channel freeze pauses every agent whose home channel it is and
// refuses posts there; the network freeze pauses every live agent, tells
// every node to pause its sandboxes and make its brokers deny, and refuses
// posts, job submissions and grant requests hub-wide. Freezes are stored
// in the freezes table and cached in memory for the hot paths.

// Pause reasons of the kill switch.
const (
	reasonFrozenChannel = "frozen: channel"
	reasonFrozenNetwork = "frozen: network"
	freezeScopeAll      = "all"
)

func channelScope(name string) string { return "channel:" + name }

// freezeState caches the stored freezes. op serialises freeze changes
// across the store write and the cache update; mu guards the cache.
type freezeState struct {
	op       sync.Mutex
	mu       sync.RWMutex
	all      *freezeRow
	channels map[string]freezeRow
}

func newFreezeState() *freezeState {
	return &freezeState{channels: map[string]freezeRow{}}
}

// changeFreeze runs a store write and, when it changed something, the
// matching cache update, as one step with respect to other freeze changes
// and to readers of the cache.
func (s *server) changeFreeze(write func() (bool, error), apply func()) (bool, error) {
	f := s.freeze
	f.op.Lock()
	defer f.op.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	changed, err := write()
	if err != nil || !changed {
		return changed, err
	}
	apply()
	return true, nil
}

// freezeCtl is the control message for the network freeze state.
func freezeCtl(frozen bool, reason string) ctlItem {
	req := &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Unfreeze{Unfreeze: &mycelv1.Unfreeze{Scope: freezeScopeAll}}}
	if frozen {
		req = &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Freeze{Freeze: &mycelv1.Freeze{Scope: freezeScopeAll, Reason: reason}}}
	}
	return ctlItem{req: req, critical: true, dedupe: ctlKeyFreeze}
}

// loadFreezes fills the cache from the store.
func (s *server) loadFreezes(ctx context.Context) error {
	rows, err := s.st.listFreezes(ctx)
	if err != nil {
		return fmt.Errorf("hub: load freezes: %w", err)
	}
	f := s.freeze
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		if r.Scope == freezeScopeAll {
			r := r
			f.all = &r
		} else if name, ok := strings.CutPrefix(r.Scope, "channel:"); ok {
			f.channels[name] = r
		}
	}
	return nil
}

// freezeAll returns the network freeze, nil when the network is not frozen.
func (s *server) freezeAll() *freezeRow {
	s.freeze.mu.RLock()
	defer s.freeze.mu.RUnlock()
	if s.freeze.all == nil {
		return nil
	}
	r := *s.freeze.all
	return &r
}

func (s *server) frozenAll() bool { return s.freezeAll() != nil }

func (s *server) frozenChannel(name string) bool {
	s.freeze.mu.RLock()
	defer s.freeze.mu.RUnlock()
	_, ok := s.freeze.channels[name]
	return ok
}

// freezeBlock returns the freeze that keeps a from running, or "".
func (s *server) freezeBlock(a *agentRow) string {
	if s.frozenAll() {
		return reasonFrozenNetwork
	}
	if a.HomeChannel != "" && s.frozenChannel(a.HomeChannel) {
		return reasonFrozenChannel
	}
	return ""
}

func isFreezeReason(r string) bool { return r == reasonFrozenChannel || r == reasonFrozenNetwork }

// errFrozen refuses a resume while a freeze applies.
func errFrozen(block string) error {
	what := "network frozen"
	if block == reasonFrozenChannel {
		what = "channel frozen"
	}
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(what))
}

// postFrozen returns the error for a post into ch while a freeze applies,
// or nil. The hub itself may always post.
func (s *server) postFrozen(a actor, channel string) error {
	if a.origin == originHub {
		return nil
	}
	if s.frozenAll() {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("network frozen"))
	}
	if channel != "" && s.frozenChannel(channel) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("channel frozen"))
	}
	return nil
}

// unfreezeAgentLocked ends a freeze pause of a once its freeze is lifted:
// another freeze or an exhausted budget keeps it paused under that reason.
// Caller holds s.life.
func (s *server) unfreezeAgentLocked(ctx context.Context, fx *lifeFx, actorID string, a *agentRow) error {
	if block := s.freezeBlock(a); block != "" {
		if block != a.PausedReason {
			if _, err := s.setState(ctx, a, a.State, block); err != nil {
				return err
			}
		}
		return nil
	}
	block, err := s.budgetBlock(ctx, a)
	if err != nil {
		return s.internalError("budget check", err)
	}
	if block != "" {
		_, err := s.setState(ctx, a, a.State, block)
		return err
	}
	return s.resumeLocked(ctx, fx, actorID, a)
}

// ---- ControlService: freezes ----

func (c *controlService) FreezeChannel(ctx context.Context, req *connect.Request[mycelv1.FreezeChannelRequest]) (*connect.Response[mycelv1.FreezeChannelResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetChannel()
	if !channels.ValidName(name) {
		return nil, invalidArg("invalid channel name %q", name)
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, policy.ActionChannelFreeze, "channel", name)
	}
	if err != nil {
		return nil, s.internalError("get channel", err)
	}
	if err := s.authorize(ctx, u, policy.ActionChannelFreeze, channelResource(ch), true); err != nil {
		return nil, err
	}
	reason := cleanReason(req.Msg.GetReason())
	row := freezeRow{Scope: channelScope(name), Reason: reason, By: u.ID, Since: nowString()}
	created, err := s.changeFreeze(func() (bool, error) { return s.st.insertFreeze(ctx, row) },
		func() { s.freeze.channels[name] = row })
	if err != nil {
		return nil, s.internalError("freeze channel", err)
	}
	if !created {
		return connect.NewResponse(&mycelv1.FreezeChannelResponse{}), nil
	}
	paused := 0
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		agents, err := s.st.agentsByHome(ctx, name)
		if err != nil {
			return s.internalError("agents by home channel", err)
		}
		for _, a := range agents {
			if a.State == stateRunning {
				paused++
			}
			if err := s.pauseLocked(ctx, fx, u.ID, a, reasonFrozenChannel); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, u.ID, string(policy.ActionChannelFreeze), "channel:"+name, outcomeOK, map[string]any{"reason": reason, "agents_paused": paused})
	text := fmt.Sprintf("channel %s frozen by %s; posts are refused and its agents are paused", name, u.ID)
	if reason != "" {
		text += " (" + reason + ")"
	}
	s.hubEvent(ctx, name, "", text, 0)
	return connect.NewResponse(&mycelv1.FreezeChannelResponse{}), nil
}

func (c *controlService) UnfreezeChannel(ctx context.Context, req *connect.Request[mycelv1.UnfreezeChannelRequest]) (*connect.Response[mycelv1.UnfreezeChannelResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetChannel()
	if !channels.ValidName(name) {
		return nil, invalidArg("invalid channel name %q", name)
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, policy.ActionChannelUnfreeze, "channel", name)
	}
	if err != nil {
		return nil, s.internalError("get channel", err)
	}
	if err := s.authorize(ctx, u, policy.ActionChannelUnfreeze, channelResource(ch), true); err != nil {
		return nil, err
	}
	removed, err := s.changeFreeze(func() (bool, error) { return s.st.deleteFreeze(ctx, channelScope(name)) },
		func() { delete(s.freeze.channels, name) })
	if err != nil {
		return nil, s.internalError("unfreeze channel", err)
	}
	if !removed {
		return connect.NewResponse(&mycelv1.UnfreezeChannelResponse{}), nil
	}
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		agents, err := s.st.agentsByHome(ctx, name)
		if err != nil {
			return s.internalError("agents by home channel", err)
		}
		for _, a := range agents {
			if a.PausedReason != reasonFrozenChannel {
				continue
			}
			if err := s.unfreezeAgentLocked(ctx, fx, u.ID, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, u.ID, string(policy.ActionChannelUnfreeze), "channel:"+name, outcomeOK, nil)
	s.hubEvent(ctx, name, "", fmt.Sprintf("channel %s unfrozen by %s", name, u.ID), 0)
	return connect.NewResponse(&mycelv1.UnfreezeChannelResponse{}), nil
}

func (c *controlService) FreezeAll(ctx context.Context, req *connect.Request[mycelv1.FreezeAllRequest]) (*connect.Response[mycelv1.FreezeAllResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionFreezeAll, policy.Resource{Kind: "network"}, true); err != nil {
		return nil, err
	}
	reason := cleanReason(req.Msg.GetReason())
	row := freezeRow{Scope: freezeScopeAll, Reason: reason, By: u.ID, Since: nowString()}
	// Nodes first: they pause every sandbox and make every broker deny.
	// Nodes offline now are told when they reconnect (syncNode). The
	// messages are queued inside the change so that a concurrent sync
	// cannot queue a stale state after them.
	var online map[string]bool
	created, err := s.changeFreeze(func() (bool, error) { return s.st.insertFreeze(ctx, row) },
		func() {
			s.freeze.all = &row
			online = s.onlineSet()
			for node := range online {
				s.enqueueCtl(node, freezeCtl(true, reason))
			}
		})
	if err != nil {
		return nil, s.internalError("freeze all", err)
	}
	if !created {
		return connect.NewResponse(&mycelv1.FreezeAllResponse{}), nil
	}
	paused := 0
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		agents, err := s.st.listAgents(ctx)
		if err != nil {
			return s.internalError("list agents", err)
		}
		for _, a := range agents {
			if a.RevokedAt != "" || a.State == stateStopped {
				continue
			}
			if a.State == stateRunning {
				paused++
			}
			if err := s.pauseLocked(ctx, fx, u.ID, a, reasonFrozenNetwork); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, u.ID, string(policy.ActionFreezeAll), "network", outcomeOK, map[string]any{
		"reason": reason, "agents_paused": paused, "nodes_told": len(online),
	})
	text := fmt.Sprintf("network frozen by %s: sandboxes paused, brokers deny, posts, jobs and grant requests are refused", u.ID)
	if reason != "" {
		text += " (" + reason + ")"
	}
	s.hubEvent(ctx, channels.Alerts, "", text, 0)
	return connect.NewResponse(&mycelv1.FreezeAllResponse{}), nil
}

func (c *controlService) UnfreezeAll(ctx context.Context, _ *connect.Request[mycelv1.UnfreezeAllRequest]) (*connect.Response[mycelv1.UnfreezeAllResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionUnfreezeAll, policy.Resource{Kind: "network"}, true); err != nil {
		return nil, err
	}
	// Nodes offline now get Unfreeze when they reconnect (syncNode).
	removed, err := s.changeFreeze(func() (bool, error) { return s.st.deleteFreeze(ctx, freezeScopeAll) },
		func() {
			s.freeze.all = nil
			for node := range s.onlineSet() {
				s.enqueueCtl(node, freezeCtl(false, ""))
			}
		})
	if err != nil {
		return nil, s.internalError("unfreeze all", err)
	}
	if !removed {
		return connect.NewResponse(&mycelv1.UnfreezeAllResponse{}), nil
	}
	fx := &lifeFx{}
	err = s.withLife(ctx, fx, func() error {
		agents, err := s.st.listAgents(ctx)
		if err != nil {
			return s.internalError("list agents", err)
		}
		for _, a := range agents {
			if a.RevokedAt != "" || a.PausedReason != reasonFrozenNetwork {
				continue
			}
			if err := s.unfreezeAgentLocked(ctx, fx, u.ID, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickScheduler() // queued jobs may be dispatched again
	s.audit(ctx, u.ID, string(policy.ActionUnfreezeAll), "network", outcomeOK, nil)
	s.hubEvent(ctx, channels.Alerts, "", fmt.Sprintf("network unfrozen by %s", u.ID), 0)
	return connect.NewResponse(&mycelv1.UnfreezeAllResponse{}), nil
}

func (c *controlService) FreezeStatus(ctx context.Context, _ *connect.Request[mycelv1.FreezeStatusRequest]) (*connect.Response[mycelv1.FreezeStatusResponse], error) {
	s := c.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, u, policy.ActionFreezeStatus, policy.Resource{Kind: "network"}, false); err != nil {
		return nil, err
	}
	out := &mycelv1.FreezeStatusResponse{}
	s.freeze.mu.RLock()
	if s.freeze.all != nil {
		out.AllFrozen, out.AllReason, out.AllSince, out.AllBy = true, s.freeze.all.Reason, s.freeze.all.Since, s.freeze.all.By
	}
	for name := range s.freeze.channels {
		out.FrozenChannels = append(out.FrozenChannels, name)
	}
	s.freeze.mu.RUnlock()
	slices.Sort(out.FrozenChannels)
	return connect.NewResponse(out), nil
}
