// Package policy decides who may do what. In v1 the rules are Go code behind
// the Policy interface (spec: "Decisions for v1", Policy). A declarative engine
// can replace Default later without touching callers.
//
// Contract (owned by the orchestrator): the types and the Policy interface are
// fixed. The hub work package implements Default.
package policy

import (
	"context"

	"github.com/richard-sen27/mycelium/internal/channels"
)

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

	// Phase 3: agent lifecycle and budgets. Resource.Kind "agent" with the
	// agent's owner, or "channel" for channel usage.
	ActionAgentGet          = Action("agent.get")
	ActionAgentSetBudget    = Action("agent.budget.set")
	ActionAgentPause        = Action("agent.pause")
	ActionAgentResume       = Action("agent.resume")
	ActionAgentStop         = Action("agent.stop")
	ActionChannelUsageGet   = Action("channel.usage.get")
	ActionChannelUsageReset = Action("channel.usage.reset")

	// Phase 4: jobs, artifacts and profiles. Resource.Kind "job" (owner =
	// the job's owner user), "artifact" (owner), "profile" (no owner).
	ActionJobRun         = Action("job.run")
	ActionJobGet         = Action("job.get")
	ActionJobList        = Action("job.list")
	ActionJobCancel      = Action("job.cancel")
	ActionJobLogs        = Action("job.logs")
	ActionArtifactList   = Action("artifact.list")
	ActionArtifactGet    = Action("artifact.get")
	ActionArtifactPut    = Action("artifact.put")
	ActionArtifactDelete = Action("artifact.delete")
	ActionProfileList    = Action("profile.list")
	ActionProfileSet     = Action("profile.set")
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
//     policy, add and remove members and archive channels they own (a
//     channel without an owner, such as a direct channel, only admins).
//     Agents: list agents and channels (the caller filters to what the agent
//     may read); get, post, read and use the blackboard on channels they are
//     a member of (for joining, the caller passes Member true when the
//     channel is open to members); create task channels (name prefix
//     "#task-"); add members to task channels they created
//     (Principal.Creator true);
//   - agent lifecycle (phase 3): admins everything; members get, set the
//     budget of, pause, resume and stop agents they own, and get or reset
//     usage of channels they own; agents nothing (spawning and stopping
//     one's own workers happens on the node, not through this interface);
//   - jobs and artifacts (phase 4): admins everything; members and their
//     agents may run jobs (the submitter's owner becomes the job owner),
//     get, cancel and read logs of jobs they own, list their own jobs, put
//     artifacts, and get or delete artifacts they own or that are attached
//     to a channel they may read (Principal.Member); profiles are listed by
//     everyone and set by admins only;
//   - anything else is denied.
func Default() Policy { return defaultPolicy{} }

// knownActions is the set of actions with a written rule. Actions outside it are denied for
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

	ActionChannelCreate:       true,
	ActionChannelList:         true,
	ActionChannelGet:          true,
	ActionChannelPolicyUpdate: true,
	ActionChannelMemberAdd:    true,
	ActionChannelMemberRemove: true,
	ActionChannelArchive:      true,
	ActionChannelPost:         true,
	ActionChannelRead:         true,
	ActionBlackboardRead:      true,
	ActionBlackboardWrite:     true,

	ActionAgentGet:          true,
	ActionAgentSetBudget:    true,
	ActionAgentPause:        true,
	ActionAgentResume:       true,
	ActionAgentStop:         true,
	ActionChannelUsageGet:   true,
	ActionChannelUsageReset: true,
}

// channelActions are the phase 2 actions agents may be considered for.
var channelActions = map[Action]bool{
	ActionChannelCreate:       true,
	ActionChannelList:         true,
	ActionChannelGet:          true,
	ActionChannelPolicyUpdate: true,
	ActionChannelMemberAdd:    true,
	ActionChannelMemberRemove: true,
	ActionChannelArchive:      true,
	ActionChannelPost:         true,
	ActionChannelRead:         true,
	ActionBlackboardRead:      true,
	ActionBlackboardWrite:     true,
}

// memberChannelActions are allowed to members of the channel.
var memberChannelActions = map[Action]bool{
	ActionChannelGet:      true,
	ActionChannelPost:     true,
	ActionChannelRead:     true,
	ActionBlackboardRead:  true,
	ActionBlackboardWrite: true,
}

type defaultPolicy struct{}

func allow() Decision { return Decision{Allow: true} }

func deny(reason string) Decision { return Decision{Allow: false, Reason: reason} }

// Authorize implements Policy.
func (defaultPolicy) Authorize(_ context.Context, p Principal, a Action, r Resource) Decision {
	if !knownActions[a] {
		return deny("unknown action")
	}
	if p.Kind == KindAgent && p.ID != "" && (channelActions[a] || a == ActionAgentList) {
		return authorizeAgentChannel(p, a, r)
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
	case ActionTokenRevoke, ActionNodeRemove, ActionNodeSetCaps, ActionAgentRevoke,
		ActionAgentGet, ActionAgentSetBudget, ActionAgentPause, ActionAgentResume, ActionAgentStop:
		if owns {
			return allow()
		}
		return deny("only the owner or an admin may do this")
	case ActionAPIKeyCreate:
		if r.ID != "" && r.ID == p.ID {
			return allow()
		}
		return deny("members may only create API keys for themselves")
	case ActionChannelCreate, ActionChannelList:
		return allow()
	case ActionChannelGet, ActionChannelPost, ActionChannelRead, ActionBlackboardRead, ActionBlackboardWrite:
		if p.Member {
			return allow()
		}
		return deny("not a member of this channel")
	case ActionChannelPolicyUpdate, ActionChannelMemberAdd, ActionChannelMemberRemove, ActionChannelArchive,
		ActionChannelUsageGet, ActionChannelUsageReset:
		if owns {
			return allow()
		}
		return deny("only the channel owner or an admin may do this")
	default:
		return deny("admin role required")
	}
}

// authorizeAgentChannel applies the channel rules to an agent principal.
func authorizeAgentChannel(p Principal, a Action, r Resource) Decision {
	switch {
	case a == ActionChannelList, a == ActionAgentList:
		return allow()
	case memberChannelActions[a]:
		if p.Member {
			return allow()
		}
		return deny("agent is not a member of this channel")
	case a == ActionChannelCreate:
		if isTaskName(r.ID) {
			return allow()
		}
		return deny("agents may only create task channels (#task-...)")
	case a == ActionChannelMemberAdd:
		if p.Creator && isTaskName(r.ID) {
			return allow()
		}
		return deny("agents may only add members to task channels they created")
	default:
		return deny("agents may not do this")
	}
}

func isTaskName(name string) bool { return channels.IsTask(name) }
