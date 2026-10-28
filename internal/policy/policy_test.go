package policy

import (
	"context"
	"testing"
)

var (
	admin  = Principal{Kind: KindUser, ID: "usr_admin", Role: RoleAdmin, OwnerID: "usr_admin"}
	member = Principal{Kind: KindUser, ID: "usr_alice", Role: RoleMember, OwnerID: "usr_alice"}
	other  = Principal{Kind: KindUser, ID: "usr_bob", Role: RoleMember, OwnerID: "usr_bob"}
	node   = Principal{Kind: KindNode, ID: "node:box", OwnerID: "usr_alice"}
	agent  = Principal{Kind: KindAgent, ID: "agent:coder@box", OwnerID: "usr_alice"}
	// A node principal that claims an admin role must still be denied.
	nodeAdmin = Principal{Kind: KindNode, ID: "node:box", Role: RoleAdmin, OwnerID: "usr_admin"}
	noRole    = Principal{Kind: KindUser, ID: "usr_x", OwnerID: "usr_x"}
)

var allActions = []Action{
	ActionTokenCreate, ActionTokenList, ActionTokenRevoke,
	ActionNodeList, ActionNodeGet, ActionNodeRemove, ActionNodeSetCaps,
	ActionUserCreate, ActionUserList, ActionUserRemove,
	ActionAPIKeyCreate, ActionAgentList, ActionAgentRevoke,
}

func TestAdminMayDoEverything(t *testing.T) {
	pol := Default()
	for _, a := range allActions {
		r := Resource{Kind: "x", ID: "id", OwnerID: "usr_someone"}
		if d := pol.Authorize(context.Background(), admin, a, r); !d.Allow {
			t.Errorf("admin %s denied: %s", a, d.Reason)
		}
	}
}

func TestNodesAgentsAndRolelessDenied(t *testing.T) {
	pol := Default()
	for _, p := range []Principal{node, agent, nodeAdmin, noRole, {}} {
		for _, a := range allActions {
			if p.Kind == KindAgent && a == ActionAgentList {
				continue // phase 2: agents discover other agents (TestChannelRules)
			}
			r := Resource{Kind: "node", ID: "box", OwnerID: p.OwnerID}
			if d := pol.Authorize(context.Background(), p, a, r); d.Allow {
				t.Errorf("%+v allowed %s", p, a)
			} else if d.Reason == "" {
				t.Errorf("%+v %s: empty reason", p, a)
			}
		}
	}
}

func TestMember(t *testing.T) {
	own := func(kind string) Resource { return Resource{Kind: kind, ID: "r1", OwnerID: member.ID} }
	foreign := func(kind string) Resource { return Resource{Kind: kind, ID: "r1", OwnerID: other.ID} }
	tests := []struct {
		name string
		a    Action
		r    Resource
		want bool
	}{
		{"token create", ActionTokenCreate, Resource{Kind: "token", OwnerID: member.ID}, true},
		{"token list", ActionTokenList, Resource{}, true},
		{"token revoke own", ActionTokenRevoke, own("token"), true},
		{"token revoke foreign", ActionTokenRevoke, foreign("token"), false},
		{"token revoke unowned", ActionTokenRevoke, Resource{Kind: "token", ID: "t"}, false},
		{"node list", ActionNodeList, Resource{}, true},
		{"node get foreign", ActionNodeGet, foreign("node"), true},
		{"node remove own", ActionNodeRemove, own("node"), true},
		{"node remove foreign", ActionNodeRemove, foreign("node"), false},
		{"node caps own", ActionNodeSetCaps, own("node"), true},
		{"node caps foreign", ActionNodeSetCaps, foreign("node"), false},
		{"user create", ActionUserCreate, Resource{Kind: "user"}, false},
		{"user list", ActionUserList, Resource{}, true},
		{"user remove self", ActionUserRemove, Resource{Kind: "user", ID: member.ID, OwnerID: member.ID}, false},
		{"user remove other", ActionUserRemove, Resource{Kind: "user", ID: other.ID, OwnerID: other.ID}, false},
		{"apikey self", ActionAPIKeyCreate, Resource{Kind: "user", ID: member.ID, OwnerID: member.ID}, true},
		{"apikey other", ActionAPIKeyCreate, Resource{Kind: "user", ID: other.ID, OwnerID: other.ID}, false},
		{"apikey other owner spoof", ActionAPIKeyCreate, Resource{Kind: "user", ID: other.ID, OwnerID: member.ID}, false},
		{"agent list", ActionAgentList, Resource{}, true},
		{"agent revoke own", ActionAgentRevoke, own("agent"), true},
		{"agent revoke foreign", ActionAgentRevoke, foreign("agent"), false},
	}
	pol := Default()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := pol.Authorize(context.Background(), member, tc.a, tc.r)
			if d.Allow != tc.want {
				t.Fatalf("got %v (%s), want %v", d.Allow, d.Reason, tc.want)
			}
			if !d.Allow && d.Reason == "" {
				t.Fatal("deny without reason")
			}
		})
	}
}

func TestUnknownActionDenied(t *testing.T) {
	pol := Default()
	for _, p := range []Principal{admin, member, node, agent} {
		if d := pol.Authorize(context.Background(), p, Action("network.freeze"), Resource{}); d.Allow {
			t.Errorf("%s allowed unknown action", p.ID)
		}
		if d := pol.Authorize(context.Background(), p, Action(""), Resource{}); d.Allow {
			t.Errorf("%s allowed empty action", p.ID)
		}
	}
}
