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
//   - nodes and agents may do nothing through this interface in phase 1;
//   - anything else is denied.
func Default() Policy { panic("policy: not implemented") }
