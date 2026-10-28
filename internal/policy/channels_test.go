package policy

import (
	"context"
	"testing"
)

func TestChannelRules(t *testing.T) {
	ch := func(name, owner string) Resource { return Resource{Kind: "channel", ID: name, OwnerID: owner} }
	asMember := func(p Principal) Principal { p.Member = true; return p }
	asCreator := func(p Principal) Principal { p.Member, p.Creator = true, true; return p }
	backend := ch("#backend", member.ID)
	foreignCh := ch("#backend", other.ID)
	task := ch("#task-1-fix", member.ID)
	tests := []struct {
		name string
		p    Principal
		a    Action
		r    Resource
		want bool
	}{
		// Users: owner, member, non-member, admin.
		{"user create", member, ActionChannelCreate, ch("#new", member.ID), true},
		{"user list", member, ActionChannelList, Resource{}, true},
		{"owner policy update", member, ActionChannelPolicyUpdate, backend, true},
		{"owner add member", member, ActionChannelMemberAdd, backend, true},
		{"owner remove member", member, ActionChannelMemberRemove, backend, true},
		{"owner archive", member, ActionChannelArchive, backend, true},
		{"member post", asMember(other), ActionChannelPost, backend, true},
		{"member read", asMember(other), ActionChannelRead, backend, true},
		{"member get", asMember(other), ActionChannelGet, backend, true},
		{"member bb read", asMember(other), ActionBlackboardRead, backend, true},
		{"member bb write", asMember(other), ActionBlackboardWrite, backend, true},
		{"member policy update foreign", asMember(member), ActionChannelPolicyUpdate, foreignCh, false},
		{"member add member foreign", asMember(member), ActionChannelMemberAdd, foreignCh, false},
		{"member archive foreign", asMember(member), ActionChannelArchive, foreignCh, false},
		{"member remove foreign", asMember(member), ActionChannelMemberRemove, foreignCh, false},
		{"non-member post", other, ActionChannelPost, backend, false},
		{"non-member read", other, ActionChannelRead, backend, false},
		{"non-member get", other, ActionChannelGet, backend, false},
		{"non-member bb", other, ActionBlackboardWrite, backend, false},
		{"owner not member cannot post", member, ActionChannelPost, backend, false},
		{"admin post non-member", admin, ActionChannelPost, foreignCh, true},
		{"admin policy update", admin, ActionChannelPolicyUpdate, foreignCh, true},
		{"admin archive", admin, ActionChannelArchive, foreignCh, true},
		{"roleless user", asMember(noRole), ActionChannelPost, backend, false},
		// Agents.
		{"agent member post", asMember(agent), ActionChannelPost, backend, true},
		{"agent member read", asMember(agent), ActionChannelRead, backend, true},
		{"agent member get", asMember(agent), ActionChannelGet, backend, true},
		{"agent member bb read", asMember(agent), ActionBlackboardRead, backend, true},
		{"agent member bb write", asMember(agent), ActionBlackboardWrite, backend, true},
		{"agent list channels", agent, ActionChannelList, Resource{}, true},
		{"agent list agents", agent, ActionAgentList, Resource{}, true},
		{"agent revoke agent", asMember(agent), ActionAgentRevoke, Resource{Kind: "agent", OwnerID: agent.OwnerID}, false},
		{"member policy update ownerless dm", asMember(member), ActionChannelPolicyUpdate, ch("dm:agent:a@b:user:usr_alice", ""), false},
		{"member archive ownerless dm", asMember(member), ActionChannelArchive, ch("dm:agent:a@b:user:usr_alice", ""), false},
		{"admin archive ownerless dm", admin, ActionChannelArchive, ch("dm:agent:a@b:user:usr_alice", ""), true},
		{"agent non-member post", agent, ActionChannelPost, backend, false},
		{"agent non-member read", agent, ActionChannelRead, backend, false},
		{"agent non-member bb", agent, ActionBlackboardRead, backend, false},
		{"agent create task", agent, ActionChannelCreate, ch("#task-1-fix", ""), true},
		{"agent create topic", agent, ActionChannelCreate, ch("#topic", ""), false},
		{"agent create bad task name", agent, ActionChannelCreate, ch("#task-", ""), false},
		{"agent create task prefix only", agent, ActionChannelCreate, ch("#task", ""), false},
		{"agent creator adds member", asCreator(agent), ActionChannelMemberAdd, task, true},
		{"agent non-creator adds member", asMember(agent), ActionChannelMemberAdd, task, false},
		{"agent creator of topic adds member", asCreator(agent), ActionChannelMemberAdd, backend, false},
		{"agent creator policy update", asCreator(agent), ActionChannelPolicyUpdate, task, false},
		{"agent creator archive", asCreator(agent), ActionChannelArchive, task, false},
		{"agent creator remove member", asCreator(agent), ActionChannelMemberRemove, task, false},
		{"agent without id", Principal{Kind: KindAgent, Member: true}, ActionChannelPost, backend, false},
		{"agent phase 1 action", asMember(agent), ActionNodeList, Resource{}, false},
		// Nodes.
		{"node member post", asMember(node), ActionChannelPost, backend, false},
		{"node admin create", nodeAdmin, ActionChannelCreate, ch("#task-x", ""), false},
	}
	pol := Default()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := pol.Authorize(context.Background(), tc.p, tc.a, tc.r)
			if d.Allow != tc.want {
				t.Fatalf("got %v (%s), want %v", d.Allow, d.Reason, tc.want)
			}
			if !d.Allow && d.Reason == "" {
				t.Fatal("deny without reason")
			}
		})
	}
}
