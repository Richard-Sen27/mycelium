// Package policy decides who may do what. In v1 the rules are Go code behind
// the Policy interface (spec: "Decisions for v1", Policy). A declarative engine
// can replace Default later without touching callers.
//
// Contract (owned by the orchestrator): the types and the Policy interface are
// fixed. The hub work package implements Default.
package policy

import "context"

// Kind of principal.
type Kind string

const (
	KindUser  Kind = "user"
	KindNode  Kind = "node"
	KindAgent Kind = "agent"
)

// Role of a user principal. Mirrors mycel.v1.Role.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Principal is who is asking.
type Principal struct {
	Kind Kind
	ID   string // user id, "node:<name>" or "agent:<name>@<node>"
	Role Role   // users only
	// OwnerID is the user that owns a node or agent principal; equals ID for users.
	OwnerID string
	// Member is set by the caller for channel actions: the principal is a
	// member of the channel in Resource.
	Member bool
	// Creator is set by the caller for channel actions: the principal
	// created the channel in Resource (agents creating task channels).
	Creator bool
}

// Action names. Phase 1 set; later phases append.
type Action string

const (
	ActionTokenCreate  = Action("token.create")
	ActionTokenList    = Action("token.list")
	ActionTokenRevoke  = Action("token.revoke")
	ActionNodeList     = Action("node.list")
	ActionNodeGet      = Action("node.get")
	ActionNodeRemove   = Action("node.remove")
	ActionNodeSetCaps  = Action("node.caps.set")
	ActionUserCreate   = Action("user.create")
	ActionUserList     = Action("user.list")
	ActionUserRemove   = Action("user.remove")
	ActionAPIKeyCreate = Action("apikey.create")
	ActionAgentList    = Action("agent.list")
	ActionAgentRevoke  = Action("agent.revoke")

	// Phase 2: channels. Resource.Kind is "channel" and Resource.ID the
	// channel name; Resource.OwnerID the channel owner (a user id).
	ActionChannelCreate       = Action("channel.create")
	ActionChannelList         = Action("channel.list")
	ActionChannelGet          = Action("channel.get")
	ActionChannelPolicyUpdate = Action("channel.policy.update")
	ActionChannelMemberAdd    = Action("channel.member.add")
	ActionChannelMemberRemove = Action("channel.member.remove")
	ActionChannelArchive      = Action("channel.archive")
	ActionChannelPost         = Action("channel.post")
	ActionChannelRead         = Action("channel.read")
	ActionBlackboardRead      = Action("bb.read")
	ActionBlackboardWrite     = Action("bb.write")
)

// Resource is what the action targets. Kind is "token", "node", "user",
// "agent" or "" for list-type actions with no single target.
type Resource struct {
	Kind    string
	ID      string
	OwnerID string
}

// Decision is the outcome. Reason is safe to return to the caller.
type Decision struct {
	Allow  bool
	Reason string
}

// Policy authorises actions. Implementations must be safe for concurrent use
// and must default to deny.
type Policy interface {
	Authorize(ctx context.Context, p Principal, a Action, r Resource) Decision
}

// Default returns the v1 rules:
//   - admins may do everything;
//   - members may create tokens (they become the owner), list and get nodes,
//     remove and set caps on nodes they own, list users and agents, revoke
//     agents they own, and create API keys for themselves;
//   - nodes may do nothing through this interface;
//   - channels (phase 2): the caller resolves membership and passes it in
//     Principal.Member; Resource carries the channel name and owner. Users:
//     create channels (they become the owner); list; get, post, read and use
//     the blackboard on channels they are a member of (admins: all); update
//     policy, add and remove members and archive channels they own. Agents:
//     post, read and use the blackboard on channels they are a member of;
//     create task channels (name prefix "#task-"); add members to task
//     channels they created (Principal.Creator true);
//   - anything else is denied.
func Default() Policy { return defaultPolicy{} }

// knownActions is the phase 1 action set. Actions outside it are denied for
// everyone, admins included, so that a new action is never allowed by
// accident before its rule is written.
var knownActions = map[Action]bool{
	ActionTokenCreate:  true,
	ActionTokenList:    true,
	ActionTokenRevoke:  true,
	ActionNodeList:     true,
	ActionNodeGet:      true,
	ActionNodeRemove:   true,
	ActionNodeSetCaps:  true,
	ActionUserCreate:   true,
	ActionUserList:     true,
	ActionUserRemove:   true,
	ActionAPIKeyCreate: true,
	ActionAgentList:    true,
	ActionAgentRevoke:  true,
}

type defaultPolicy struct{}

func allow() Decision { return Decision{Allow: true} }

func deny(reason string) Decision { return Decision{Allow: false, Reason: reason} }

// Authorize implements Policy.
func (defaultPolicy) Authorize(_ context.Context, p Principal, a Action, r Resource) Decision {
	if !knownActions[a] {
		return deny("unknown action")
	}
	if p.Kind != KindUser || p.ID == "" {
		return deny("only users may perform this action")
	}
	switch p.Role {
	case RoleAdmin:
		return allow()
	case RoleMember:
	default:
		return deny("unknown role")
	}
	owns := r.OwnerID != "" && r.OwnerID == p.ID
	switch a {
	case ActionTokenCreate, ActionTokenList, ActionNodeList, ActionNodeGet,
		ActionUserList, ActionAgentList:
		return allow()
	case ActionTokenRevoke, ActionNodeRemove, ActionNodeSetCaps, ActionAgentRevoke:
		if owns {
			return allow()
		}
		return deny("only the owner or an admin may do this")
	case ActionAPIKeyCreate:
		if r.ID != "" && r.ID == p.ID {
			return allow()
		}
		return deny("members may only create API keys for themselves")
	default:
		return deny("admin role required")
	}
}
