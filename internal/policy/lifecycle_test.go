package policy

import (
	"context"
	"testing"
)

func TestLifecycleRules(t *testing.T) {
	ownAgent := Resource{Kind: "agent", ID: "agent:w1@box", OwnerID: member.ID}
	foreignAgent := Resource{Kind: "agent", ID: "agent:w2@other", OwnerID: other.ID}
	unowned := Resource{Kind: "agent", ID: "agent:w3@box"}
	ownCh := Resource{Kind: "channel", ID: "#backend", OwnerID: member.ID}
	foreignCh := Resource{Kind: "channel", ID: "#backend", OwnerID: other.ID}
	directCh := Resource{Kind: "channel", ID: "dm:a:b"}
	agentActions := []Action{ActionAgentGet, ActionAgentSetBudget, ActionAgentPause, ActionAgentResume, ActionAgentStop}
	usageActions := []Action{ActionChannelUsageGet, ActionChannelUsageReset}
	asMember := func(p Principal) Principal { p.Member, p.Creator = true, true; return p }

	type tc struct {
		name string
		p    Principal
		a    Action
		r    Resource
		want bool
	}
	var tests []tc
	for _, a := range agentActions {
		tests = append(tests,
			tc{"admin " + string(a), admin, a, foreignAgent, true},
			tc{"owner " + string(a), member, a, ownAgent, true},
			tc{"other member " + string(a), other, a, ownAgent, false},
			tc{"member unowned " + string(a), member, a, unowned, false},
			tc{"agent " + string(a), agent, a, ownAgent, false},
			tc{"agent as member " + string(a), asMember(agent), a, ownAgent, false},
			tc{"node " + string(a), node, a, ownAgent, false},
			tc{"node claiming admin " + string(a), nodeAdmin, a, ownAgent, false},
			tc{"roleless " + string(a), noRole, a, Resource{Kind: "agent", OwnerID: noRole.ID}, false},
		)
	}
	for _, a := range usageActions {
		tests = append(tests,
			tc{"admin " + string(a), admin, a, foreignCh, true},
			tc{"admin direct " + string(a), admin, a, directCh, true},
			tc{"channel owner " + string(a), member, a, ownCh, true},
			tc{"channel member " + string(a), asMember(other), a, ownCh, false},
			tc{"non-owner " + string(a), member, a, foreignCh, false},
			tc{"direct " + string(a), member, a, directCh, false},
			tc{"agent " + string(a), asMember(agent), a, ownCh, false},
			tc{"node " + string(a), node, a, ownCh, false},
		)
	}
	pol := Default()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := pol.Authorize(context.Background(), tt.p, tt.a, tt.r)
			if d.Allow != tt.want {
				t.Fatalf("got %v (%s), want %v", d.Allow, d.Reason, tt.want)
			}
			if !d.Allow && d.Reason == "" {
				t.Fatal("deny without reason")
			}
		})
	}
}
