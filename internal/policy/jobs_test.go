package policy

import (
	"context"
	"testing"
)

func TestJobAndArtifactRules(t *testing.T) {
	ownJob := Resource{Kind: "job", ID: "job_1", OwnerID: member.ID}
	foreignJob := Resource{Kind: "job", ID: "job_2", OwnerID: other.ID}
	unownedJob := Resource{Kind: "job", ID: "job_3"}
	ownArt := Resource{Kind: "artifact", ID: "art_1", OwnerID: member.ID}
	foreignArt := Resource{Kind: "artifact", ID: "art_2", OwnerID: other.ID}
	profile := Resource{Kind: "profile", ID: "dev"}
	inChannel := func(p Principal) Principal { p.Member = true; return p }
	maker := func(p Principal) Principal { p.Creator = true; return p }
	agentNoOwner := Principal{Kind: KindAgent, ID: "agent:x@box"}

	type tc struct {
		name string
		p    Principal
		a    Action
		r    Resource
		want bool
	}
	tests := []tc{
		// run: the owner must be the submitter's owner
		{"member runs own", member, ActionJobRun, ownJob, true},
		{"member runs for other", member, ActionJobRun, foreignJob, false},
		{"member runs unowned", member, ActionJobRun, unownedJob, false},
		{"agent runs for its owner", agent, ActionJobRun, ownJob, true},
		{"agent runs for other", agent, ActionJobRun, foreignJob, false},
		{"agent without owner", agentNoOwner, ActionJobRun, unownedJob, false},
		{"admin runs anything", admin, ActionJobRun, foreignJob, true},
		{"node runs", node, ActionJobRun, ownJob, false},
		{"node admin runs", nodeAdmin, ActionJobRun, ownJob, false},
		{"roleless runs", noRole, ActionJobRun, Resource{Kind: "job", OwnerID: noRole.ID}, false},

		{"member lists jobs", member, ActionJobList, Resource{}, true},
		{"agent lists jobs", agent, ActionJobList, Resource{}, true},
		{"node lists jobs", node, ActionJobList, Resource{}, false},

		{"member lists artifacts", member, ActionArtifactList, Resource{}, true},
		{"agent lists artifacts", agent, ActionArtifactList, Resource{}, true},
		{"member puts artifact", member, ActionArtifactPut, ownArt, true},
		{"agent puts artifact", agent, ActionArtifactPut, ownArt, true},
		{"node puts artifact", node, ActionArtifactPut, ownArt, false},

		{"member gets own artifact", member, ActionArtifactGet, ownArt, true},
		{"member gets foreign artifact", member, ActionArtifactGet, foreignArt, false},
		{"channel member gets foreign artifact", inChannel(member), ActionArtifactGet, foreignArt, true},
		{"agent gets owner's artifact", agent, ActionArtifactGet, ownArt, false},
		{"agent gets an artifact it made", maker(agent), ActionArtifactGet, ownArt, true},
		{"member creator flag ignored", maker(other), ActionArtifactGet, ownArt, false},
		{"agent gets foreign artifact", agent, ActionArtifactGet, foreignArt, false},
		{"agent in channel gets foreign artifact", inChannel(agent), ActionArtifactGet, foreignArt, true},
		{"admin gets foreign artifact", admin, ActionArtifactGet, foreignArt, true},
		{"node gets artifact", node, ActionArtifactGet, ownArt, false},

		{"member deletes own artifact", member, ActionArtifactDelete, ownArt, true},
		{"member deletes foreign artifact", member, ActionArtifactDelete, foreignArt, false},
		{"channel member deletes foreign artifact", inChannel(member), ActionArtifactDelete, foreignArt, false},
		{"agent deletes owner's artifact", agent, ActionArtifactDelete, ownArt, false},
		{"agent deletes an artifact it made", maker(agent), ActionArtifactDelete, ownArt, true},
		{"agent in channel deletes", inChannel(agent), ActionArtifactDelete, foreignArt, false},
		{"admin deletes foreign artifact", admin, ActionArtifactDelete, foreignArt, true},

		{"member lists profiles", member, ActionProfileList, profile, true},
		{"agent lists profiles", agent, ActionProfileList, profile, true},
		{"node lists profiles", node, ActionProfileList, profile, false},
		{"admin sets profile", admin, ActionProfileSet, profile, true},
		{"member sets profile", member, ActionProfileSet, profile, false},
		{"agent sets profile", agent, ActionProfileSet, profile, false},
	}
	for _, a := range []Action{ActionJobGet, ActionJobCancel, ActionJobLogs} {
		tests = append(tests,
			tc{"owner " + string(a), member, a, ownJob, true},
			tc{"other member " + string(a), other, a, ownJob, false},
			tc{"channel member " + string(a), inChannel(other), a, ownJob, false},
			tc{"member unowned " + string(a), member, a, unownedJob, false},
			tc{"agent of owner " + string(a), agent, a, ownJob, true},
			tc{"agent foreign " + string(a), agent, a, foreignJob, false},
			tc{"admin " + string(a), admin, a, foreignJob, true},
			tc{"node " + string(a), node, a, ownJob, false},
			tc{"node admin " + string(a), nodeAdmin, a, ownJob, false},
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
