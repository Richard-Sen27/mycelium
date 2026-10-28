package policy

import (
	"context"
	"testing"
)

func TestGrantAndControlRules(t *testing.T) {
	p := Default()
	admin := Principal{Kind: KindUser, ID: "usr_a", Role: RoleAdmin, OwnerID: "usr_a"}
	alice := Principal{Kind: KindUser, ID: "usr_alice", Role: RoleMember, OwnerID: "usr_alice"}
	agent := Principal{Kind: KindAgent, ID: "agent:w@n1", OwnerID: "usr_alice"}
	node := Principal{Kind: KindNode, ID: "node:n1", OwnerID: "usr_alice"}
	own := Resource{Kind: "grant", ID: "grt_1", OwnerID: "usr_alice"}
	other := Resource{Kind: "grant", ID: "grt_1", OwnerID: "usr_bob"}
	cases := []struct {
		name string
		p    Principal
		a    Action
		r    Resource
		want bool
	}{
		{"admin approves anything", admin, ActionGrantApprove, other, true},
		{"admin unfreezes all", admin, ActionUnfreezeAll, Resource{}, true},
		{"member requests for own subject", alice, ActionGrantRequest, own, true},
		{"member requests for another's subject", alice, ActionGrantRequest, other, false},
		{"member lists", alice, ActionGrantList, Resource{}, true},
		{"member gets own", alice, ActionGrantGet, own, true},
		{"member gets other", alice, ActionGrantGet, other, false},
		{"node owner approves", alice, ActionGrantApprove, own, true},
		{"non-owner approves", alice, ActionGrantApprove, other, false},
		{"non-owner denies", alice, ActionGrantDeny, other, false},
		{"member revokes own", alice, ActionGrantRevoke, own, true},
		{"member revokes other", alice, ActionGrantRevoke, other, false},
		{"owner gets sharing", alice, ActionNodeSharingGet, Resource{Kind: "node", OwnerID: "usr_alice"}, true},
		{"other sets sharing", alice, ActionNodeSharingSet, Resource{Kind: "node", OwnerID: "usr_bob"}, false},
		{"owner freezes channel", alice, ActionChannelFreeze, Resource{Kind: "channel", OwnerID: "usr_alice"}, true},
		{"other freezes channel", alice, ActionChannelFreeze, Resource{Kind: "channel", OwnerID: "usr_bob"}, false},
		{"other unfreezes channel", alice, ActionChannelUnfreeze, Resource{Kind: "channel", OwnerID: "usr_bob"}, false},
		{"member freezes all", alice, ActionFreezeAll, Resource{}, false},
		{"member unfreezes all", alice, ActionUnfreezeAll, Resource{}, false},
		{"member freeze status", alice, ActionFreezeStatus, Resource{}, true},
		{"agent requests for itself", agent, ActionGrantRequest, Resource{Kind: "grant", ID: "agent:w@n1", OwnerID: "usr_alice"}, true},
		{"agent requests for another", agent, ActionGrantRequest, Resource{Kind: "grant", ID: "agent:x@n1", OwnerID: "usr_alice"}, false},
		{"agent lists", agent, ActionGrantList, Resource{}, true},
		{"agent approves", agent, ActionGrantApprove, own, false},
		{"agent freezes", agent, ActionFreezeAll, Resource{}, false},
		{"node requests", node, ActionGrantRequest, own, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.Authorize(context.Background(), c.p, c.a, c.r).Allow; got != c.want {
				t.Fatalf("Authorize(%s) = %v, want %v", c.a, got, c.want)
			}
		})
	}
}

func TestNeedsApproval(t *testing.T) {
	dec, ok := Default().(GrantDecider)
	if !ok {
		t.Fatal("Default does not implement GrantDecider")
	}
	gpuRule := GrantRule{Channel: "#training-runs", Kind: "gpu", Node: "gpu-box", MaxTTL: "4h", ReadOnly: true}
	roMounts := GrantRule{Channel: "*", Kind: "mount", Node: "*", ReadOnly: true}
	rwMounts := GrantRule{Channel: "#data", Kind: "mount", Node: "*", ReadOnly: false}
	ollama := GrantRule{Channel: "*", Kind: "ollama", Node: "lab-*", MaxCount: 2, ReadOnly: true}
	rules := []GrantRule{gpuRule, roMounts, rwMounts, ollama}
	cases := []struct {
		name  string
		req   GrantRequestInfo
		auto  bool
		match *GrantRule
	}{
		{"spec example", GrantRequestInfo{Channel: "#training-runs", Kind: "gpu", Node: "gpu-box", TTL: "2h"}, true, &gpuRule},
		{"too long", GrantRequestInfo{Channel: "#training-runs", Kind: "gpu", Node: "gpu-box", TTL: "5h"}, false, nil},
		{"other channel", GrantRequestInfo{Channel: "#general", Kind: "gpu", Node: "gpu-box", TTL: "1h"}, false, nil},
		{"no channel", GrantRequestInfo{Kind: "gpu", Node: "gpu-box", TTL: "1h"}, false, nil},
		{"other node", GrantRequestInfo{Channel: "#training-runs", Kind: "gpu", Node: "laptop", TTL: "1h"}, false, nil},
		{"one gpu already", GrantRequestInfo{Channel: "#training-runs", Kind: "gpu", Node: "gpu-box", TTL: "1h", ActiveOfKind: 1}, false, nil},
		{"read-only mount", GrantRequestInfo{Channel: "#x", Kind: "mount", Node: "n", TTL: "1h", ReadOnlyMount: true}, true, &roMounts},
		{"read-write mount, read_only rule only", GrantRequestInfo{Channel: "#x", Kind: "mount", Node: "n", TTL: "1h"}, false, nil},
		{"read-write mount, explicit read_only false", GrantRequestInfo{Channel: "#data", Kind: "mount", Node: "n", TTL: "1h"}, true, &rwMounts},
		{"read_only does not affect other kinds", GrantRequestInfo{Kind: "ollama", Node: "lab-1", TTL: "1h"}, true, &ollama},
		{"max_count 2, one active", GrantRequestInfo{Kind: "ollama", Node: "lab-1", TTL: "1h", ActiveOfKind: 1}, true, &ollama},
		{"max_count 2, two active", GrantRequestInfo{Kind: "ollama", Node: "lab-1", TTL: "1h", ActiveOfKind: 2}, false, nil},
		{"glob miss", GrantRequestInfo{Kind: "ollama", Node: "mac", TTL: "1h"}, false, nil},
		{"over 24h", GrantRequestInfo{Kind: "ollama", Node: "lab-1", TTL: "25h"}, false, nil},
		{"bad ttl", GrantRequestInfo{Kind: "ollama", Node: "lab-1", TTL: "soon"}, false, nil},
		{"no rules", GrantRequestInfo{Kind: "egress", Node: "n", TTL: "1h"}, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.Rules = rules
			needs, ttl, r := dec.NeedsApproval(c.req)
			if needs == c.auto {
				t.Fatalf("needs = %v, want %v", needs, !c.auto)
			}
			if ttl != c.req.TTL {
				t.Fatalf("ttl = %q", ttl)
			}
			if (r == nil) != (c.match == nil) || (r != nil && *r != *c.match) {
				t.Fatalf("rule = %+v, want %+v", r, c.match)
			}
		})
	}
	// Egress and docker-host need a rule that names the kind.
	anyKind := GrantRule{Kind: "*", ReadOnly: true}
	noKind := GrantRule{ReadOnly: true}
	egress := GrantRule{Kind: "egress", ReadOnly: true}
	for _, kind := range []string{"egress", "docker-host"} {
		for _, r := range []GrantRule{anyKind, noKind, {Kind: "e*"}, {Kind: "docker-*"}} {
			if needs, _, _ := dec.NeedsApproval(GrantRequestInfo{Kind: kind, Node: "n", TTL: "1h", Rules: []GrantRule{r}}); !needs {
				t.Errorf("%s auto-approved by rule kind %q", kind, r.Kind)
			}
		}
	}
	if needs, _, _ := dec.NeedsApproval(GrantRequestInfo{Kind: "egress", Node: "n", TTL: "1h", Rules: []GrantRule{egress}}); needs {
		t.Error("explicit egress rule did not match")
	}
	if needs, _, _ := dec.NeedsApproval(GrantRequestInfo{Kind: "gpu", Node: "n", TTL: "1h", Rules: []GrantRule{anyKind}}); needs {
		t.Error("wildcard rule did not match gpu")
	}
	// First match wins.
	first := GrantRule{Kind: "*", MaxTTL: "1h"}
	second := GrantRule{Kind: "gpu"}
	_, _, r := dec.NeedsApproval(GrantRequestInfo{Kind: "gpu", Node: "n", TTL: "30m", Rules: []GrantRule{first, second}})
	if r == nil || *r != first {
		t.Fatalf("first match: %+v", r)
	}
	// A rule that does not match (TTL) falls through to the next.
	_, _, r = dec.NeedsApproval(GrantRequestInfo{Kind: "gpu", Node: "n", TTL: "2h", Rules: []GrantRule{first, second}})
	if r == nil || *r != second {
		t.Fatalf("fall through: %+v", r)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		p, v string
		want bool
	}{
		{"", "x", true}, {"*", "", true}, {"abc", "abc", true}, {"abc", "abd", false},
		{"#train-*", "#train-runs", true}, {"#train-*", "#tra", false}, {"*-box", "gpu-box", true},
		{"a*b*c", "aXbYc", true}, {"a*b*c", "acb", false}, {"ab*ba", "aba", false}, {"a*a", "a", false},
	}
	for _, c := range cases {
		if got := Glob(c.p, c.v); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.p, c.v, got, c.want)
		}
	}
}
