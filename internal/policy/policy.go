// Package policy decides who may do what. In v1 the rules are Go code behind
// the Policy interface (spec: "Decisions for v1", Policy). A declarative engine
// can replace Default later without touching callers.
//
// Contract (owned by the orchestrator): the types and the Policy interface are
// fixed. The hub work package implements Default.
package policy

import (
	"context"
	"strings"
	"time"

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
	// For artifact actions of agents: the agent made the artifact.
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

	// Phase 5: grants, sharing, kill switch. Resource.Kind "grant" (owner =
	// the subject's owner), "node" for sharing, "channel" for freezes.
	ActionGrantRequest    = Action("grant.request")
	ActionGrantList       = Action("grant.list")
	ActionGrantGet        = Action("grant.get")
	ActionGrantApprove    = Action("grant.approve")
	ActionGrantDeny       = Action("grant.deny")
	ActionGrantRevoke     = Action("grant.revoke")
	ActionNodeSharingGet  = Action("node.sharing.get")
	ActionNodeSharingSet  = Action("node.sharing.set")
	ActionChannelFreeze   = Action("channel.freeze")
	ActionChannelUnfreeze = Action("channel.unfreeze")
	ActionFreezeAll       = Action("freeze.all")
	ActionUnfreezeAll     = Action("unfreeze.all")
	ActionFreezeStatus    = Action("freeze.status")
)

// GrantRule is one auto-approval rule (spec example: "agents in
// #training-runs may lease one GPU on gpu-box for up to 4 hours without
// approval; anything else needs the owner"). Rules are evaluated by the
// Policy implementation for every grant request; the first match wins and
// a request matching no rule requires approval. They are loaded from the
// hub's configuration (hub.yaml `grants.auto`).
type GrantRule struct {
	Channel  string // "#training-runs" or "*" for any
	Kind     string // ollama, gpu, docker-host, mount, egress or "*"
	Node     string // node name or "*"
	MaxTTL   string // Go duration, e.g. "4h"
	MaxCount int    // concurrent active grants of this kind per subject; 0 = 1
	ReadOnly bool   // mounts: only read-only mounts match
}

// GrantDecider decides whether a grant request needs approval. The default
// policy implements it; the hub calls it after Authorize.
type GrantDecider interface {
	// NeedsApproval returns false (and the allowed TTL) when a rule
	// auto-grants the request. ctxInfo carries the channel, kind, node, the
	// requested TTL and the subject's active grants of that kind.
	NeedsApproval(req GrantRequestInfo) (needs bool, ttl string, rule *GrantRule)
}

// GrantRequestInfo is the context a GrantDecider sees.
type GrantRequestInfo struct {
	Channel       string
	Kind          string
	Node          string
	TTL           string
	ReadOnlyMount bool
	ActiveOfKind  int
	Rules         []GrantRule
}

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
//     agents may run jobs (the submitter's owner becomes the job owner:
//     Resource.OwnerID must be the member, or the agent's owner), get,
//     cancel and read logs of jobs they own (an agent: jobs of its owner),
//     list their own jobs (the caller filters) and put artifacts. Members
//     get artifacts they own or that are attached to a channel they may
//     read (Principal.Member) and delete artifacts they own. Agents get
//     artifacts attached to a channel they are a member of (Member) or
//     that they made (Principal.Creator: created by the agent or produced
//     by a job it submitted), and delete only those they made. Profiles
//     are listed by users and agents and set by admins only;
//   - grants and control (phase 5): admins everything; members request
//     grants for subjects they own, list and get their own grants, revoke
//     their own; approve and deny require admin OR ownership of the node the
//     grant attaches to (the node owner decides about their machine); node
//     sharing: get for the owner and admins, set for the owner and admins;
//     freezes: channel freeze/unfreeze by the channel owner or admins;
//     freeze all and unfreeze all by admins only (spec: the global freeze
//     is a hub-admin action); freeze status for every user;
//     agents may request grants for themselves and list their own;
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

	ActionJobRun:         true,
	ActionJobGet:         true,
	ActionJobList:        true,
	ActionJobCancel:      true,
	ActionJobLogs:        true,
	ActionArtifactList:   true,
	ActionArtifactGet:    true,
	ActionArtifactPut:    true,
	ActionArtifactDelete: true,
	ActionProfileList:    true,
	ActionProfileSet:     true,

	ActionGrantRequest:    true,
	ActionGrantList:       true,
	ActionGrantGet:        true,
	ActionGrantApprove:    true,
	ActionGrantDeny:       true,
	ActionGrantRevoke:     true,
	ActionNodeSharingGet:  true,
	ActionNodeSharingSet:  true,
	ActionChannelFreeze:   true,
	ActionChannelUnfreeze: true,
	ActionFreezeAll:       true,
	ActionUnfreezeAll:     true,
	ActionFreezeStatus:    true,
}

// grantAgentActions are the phase 5 actions agents may be considered for.
var grantAgentActions = map[Action]bool{
	ActionGrantRequest: true,
	ActionGrantList:    true,
}

// jobActions are the phase 4 actions, decided by authorizeJob for members
// and agents.
var jobActions = map[Action]bool{
	ActionJobRun:         true,
	ActionJobGet:         true,
	ActionJobList:        true,
	ActionJobCancel:      true,
	ActionJobLogs:        true,
	ActionArtifactList:   true,
	ActionArtifactGet:    true,
	ActionArtifactPut:    true,
	ActionArtifactDelete: true,
	ActionProfileList:    true,
	ActionProfileSet:     true,
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
	if p.Kind == KindAgent && p.ID != "" && grantAgentActions[a] {
		return authorizeAgentGrant(p, a, r)
	}
	if p.Kind == KindAgent && p.ID != "" && jobActions[a] {
		if p.OwnerID == "" {
			return deny("agent without owner")
		}
		return authorizeJob(p.OwnerID, p, a, r)
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
		ActionChannelUsageGet, ActionChannelUsageReset, ActionChannelFreeze, ActionChannelUnfreeze:
		if owns {
			return allow()
		}
		return deny("only the channel owner or an admin may do this")
	case ActionGrantList, ActionFreezeStatus:
		return allow()
	case ActionGrantRequest, ActionGrantGet, ActionGrantRevoke:
		if owns {
			return allow()
		}
		return deny("only the owner or an admin may do this")
	case ActionGrantApprove, ActionGrantDeny:
		if owns {
			return allow()
		}
		return deny("only an admin or the owner of the node the grant attaches to may decide it")
	case ActionNodeSharingGet, ActionNodeSharingSet:
		if owns {
			return allow()
		}
		return deny("only the node's owner or an admin may do this")
	}
	if jobActions[a] {
		return authorizeJob(p.ID, p, a, r)
	}
	return deny("admin role required")
}

// authorizeJob applies the phase 4 rules to a member (owner = the member)
// or an agent (owner = the agent's owner).
func authorizeJob(owner string, p Principal, a Action, r Resource) Decision {
	owns := r.OwnerID != "" && r.OwnerID == owner
	switch a {
	case ActionJobRun:
		if owns {
			return allow()
		}
		return deny("jobs are owned by the submitter's owner")
	case ActionJobList, ActionArtifactList, ActionArtifactPut, ActionProfileList:
		return allow()
	case ActionJobGet, ActionJobCancel, ActionJobLogs:
		if owns {
			return allow()
		}
		return deny("only the job's owner or an admin may do this")
	case ActionArtifactGet:
		if p.Kind == KindAgent {
			owns = p.Creator
		}
		if owns || p.Member {
			return allow()
		}
		return deny("artifact is not visible to you")
	case ActionArtifactDelete:
		if p.Kind == KindAgent {
			owns = p.Creator
		}
		if owns {
			return allow()
		}
		return deny("only the artifact's owner or an admin may delete it")
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

// authorizeAgentGrant applies the phase 5 rules to an agent principal: it
// may request grants for itself (Resource.ID is the subject) and list its
// own grants (the caller filters).
func authorizeAgentGrant(p Principal, a Action, r Resource) Decision {
	switch a {
	case ActionGrantList:
		return allow()
	case ActionGrantRequest:
		if r.ID != "" && r.ID == p.ID {
			return allow()
		}
		return deny("agents may only request grants for themselves")
	default:
		return deny("agents may not do this")
	}
}

// MaxGrantTTL is the longest grant duration.
const MaxGrantTTL = 24 * time.Hour

// NeedsApproval implements GrantDecider: the first rule in req.Rules that
// matches the request auto-grants it with the requested TTL. A rule
// matches when its channel, kind and node patterns match ("*" matches any
// run of characters, an empty pattern matches anything), the requested TTL
// is within its max_ttl (an empty max_ttl allows up to MaxGrantTTL), the
// subject holds fewer active grants of the kind than max_count (0 = 1),
// egress and docker-host requests match only rules naming that kind,
// and, for a read-write mount, the rule has ReadOnly false (a read_only
// rule matches only read-only mounts; hub.yaml defaults read_only to
// true, so read-write mounts never auto-approve by accident). Requests that
// match no rule, or carry an invalid TTL, need approval.
func (defaultPolicy) NeedsApproval(req GrantRequestInfo) (bool, string, *GrantRule) {
	ttl, err := time.ParseDuration(req.TTL)
	if err != nil || ttl <= 0 || ttl > MaxGrantTTL {
		return true, req.TTL, nil
	}
	for i := range req.Rules {
		r := req.Rules[i]
		if ruleMatches(r, req, ttl) {
			return false, req.TTL, &r
		}
	}
	return true, req.TTL, nil
}

func ruleMatches(r GrantRule, req GrantRequestInfo, ttl time.Duration) bool {
	if !Glob(r.Channel, req.Channel) || !Glob(r.Kind, req.Kind) || !Glob(r.Node, req.Node) {
		return false
	}
	// Egress and host Docker widen what a sandbox can reach: only a rule
	// naming the kind exactly auto-approves them, never a wildcard.
	if (req.Kind == "egress" || req.Kind == "docker-host") && r.Kind != req.Kind {
		return false
	}
	// A read-write mount matches only a rule that allows read-write
	// (ReadOnly false); ReadOnly has no effect on other kinds.
	if req.Kind == "mount" && !req.ReadOnlyMount && r.ReadOnly {
		return false
	}
	limit := MaxGrantTTL
	if r.MaxTTL != "" {
		d, err := time.ParseDuration(r.MaxTTL)
		if err != nil || d <= 0 {
			return false
		}
		limit = min(d, MaxGrantTTL)
	}
	if ttl > limit {
		return false
	}
	maxCount := r.MaxCount
	if maxCount <= 0 {
		maxCount = 1
	}
	return req.ActiveOfKind < maxCount
}

// Glob reports whether value matches pattern, where '*' matches any run of
// characters (including none) and every other character matches itself. An
// empty pattern matches everything.
func Glob(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(value, mid)
		if i < 0 {
			return false
		}
		value = value[i+len(mid):]
	}
	return strings.HasSuffix(value, last)
}

// Default implements GrantDecider.
var _ GrantDecider = defaultPolicy{}
