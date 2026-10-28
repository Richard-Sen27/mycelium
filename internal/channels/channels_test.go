package channels

import (
	"strings"
	"sync"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func TestNames(t *testing.T) {
	tests := []struct {
		name                     string
		valid, task, sys, direct bool
	}{
		{"#general", true, false, false, false},
		{"#a", true, false, false, false},
		{"#backend-2", true, false, false, false},
		{"#" + strings.Repeat("a", 63), true, false, false, false},
		{"#" + strings.Repeat("a", 64), false, false, false, false},
		{"#", false, false, false, false},
		{"general", false, false, false, false},
		{"#-x", false, false, false, false},
		{"#x-", false, false, false, false},
		{"#Upper", false, false, false, false},
		{"#a.b", false, false, false, false},
		{"#a b", false, false, false, false},
		{"#a*", false, false, false, false},
		{"#a>", false, false, false, false},
		{"##a", false, false, false, false},
		{"#task-4821-fix-auth", true, true, false, false},
		{"#task-", false, false, false, false},
		{"#task", true, false, false, false},
		{"#alerts", true, false, true, false},
		{"#approvals", true, false, true, false},
		{"#node-gpu-box", true, false, true, false},
		{"#nodes", true, false, false, false},
		{"dm:agent:a@n:user:usr_1", false, false, false, true},
		{"dm:user:usr_1:agent:a@n", false, false, false, false}, // not sorted
		{"dm:agent:a@n:agent:a@n", false, false, false, false},  // same participant
		{"dm:agent:a@n", false, false, false, false},
		{"dm:bot:x:user:y", false, false, false, false},
		{"dm:agent:A@n:user:y", false, false, false, false},
		{"dm:agent:a.b@n:user:y", false, false, false, false},
		{"dm:user:a.b:user:y", false, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidName(tc.name); got != tc.valid {
				t.Errorf("ValidName = %v", got)
			}
			if got := IsTask(tc.name); got != tc.task {
				t.Errorf("IsTask = %v", got)
			}
			if got := IsSystem(tc.name); got != tc.sys {
				t.Errorf("IsSystem = %v", got)
			}
			if got := IsDirect(tc.name); got != tc.direct {
				t.Errorf("IsDirect = %v", got)
			}
		})
	}
}

func TestDirectName(t *testing.T) {
	a, b := "user:usr_1", "agent:coder@mac"
	n := DirectName(a, b)
	if n != "dm:agent:coder@mac:user:usr_1" || DirectName(b, a) != n {
		t.Fatalf("DirectName = %q", n)
	}
	x, y, ok := DirectParticipants(n)
	if !ok || x != b || y != a {
		t.Fatalf("DirectParticipants = %q %q %v", x, y, ok)
	}
	if _, _, ok := DirectParticipants("#general"); ok {
		t.Fatal("topic name parsed as direct")
	}
	if NodeChannel("gpu-box") != "#node-gpu-box" || !IsSystem(NodeChannel("gpu-box")) {
		t.Fatal("NodeChannel")
	}
}

func TestValidParticipant(t *testing.T) {
	for id, want := range map[string]bool{
		"agent:coder@mac": true, "user:usr_ab12": true, "user:x-Y_1": true,
		"agent:coder": false, "agent:@mac": false, "agent:c@": false, "agent:C@mac": false,
		"user:": false, "user:a:b": false, "user:a.b": false, "hub": false, "node:x": false, "": false,
		"user:" + strings.Repeat("a", 129): false,
	} {
		if got := ValidParticipant(id); got != want {
			t.Errorf("ValidParticipant(%q) = %v", id, got)
		}
	}
}

func TestIDs(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_123)
	seen := map[string]bool{}
	for range 1000 {
		id := NewMessageID(now)
		if !ValidMessageID(id) || !ValidThreadID(id) || len(id) != 30 || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
	th := NewThreadID(now)
	if !strings.HasPrefix(th, "thr_") || !ValidThreadID(th) || ValidMessageID(th) {
		t.Fatalf("thread id %q", th)
	}
	// The time prefix sorts with time.
	a, b := NewMessageID(now), NewMessageID(now.Add(time.Millisecond))
	if a[:14] >= b[:14] {
		t.Fatalf("not time-ordered: %s %s", a, b)
	}
	// Known encoding of the time part (48-bit ms, Crockford).
	if got := newULID(time.UnixMilli(0))[:10]; got != "0000000000" {
		t.Fatalf("zero time = %s", got)
	}
	if got := newULID(time.UnixMilli(1<<48 - 1))[:10]; got != "7ZZZZZZZZZ" {
		t.Fatalf("max time = %s", got)
	}
	for _, bad := range []string{"", "msg_", "msg_0123456789ABCDEFGHJKMNPQRI", "msg_0123456789abcdefghjkmnpqrs", "thr_x"} {
		if ValidMessageID(bad) {
			t.Errorf("ValidMessageID(%q)", bad)
		}
	}
}

func TestWithDefaults(t *testing.T) {
	topic := mycelv1.ChannelType_CHANNEL_TYPE_TOPIC
	sys := mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM
	tests := []struct {
		name          string
		in            *mycelv1.ChannelPolicy
		typ           mycelv1.ChannelType
		hops, rate    int32
		agents, human bool
	}{
		{"nil", nil, topic, DefaultMaxHops, DefaultRatePerMinute, true, true},
		{"zero", &mycelv1.ChannelPolicy{}, topic, 8, 60, true, true},
		{"set", &mycelv1.ChannelPolicy{MaxHops: 3, RatePerMinute: 2, HumansMayPost: true}, topic, 3, 2, false, true},
		{"negative", &mycelv1.ChannelPolicy{MaxHops: -1, RatePerMinute: -5}, topic, 8, 60, true, true},
		{"system", &mycelv1.ChannelPolicy{AgentsMayPost: true, HumansMayPost: true}, sys, 8, 60, false, false},
		{"system nil", nil, sys, 8, 60, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var orig *mycelv1.ChannelPolicy
			if tc.in != nil {
				orig = &mycelv1.ChannelPolicy{MaxHops: tc.in.MaxHops, AgentsMayPost: tc.in.AgentsMayPost}
			}
			got := WithDefaults(tc.in, tc.typ)
			if got.MaxHops != tc.hops || got.RatePerMinute != tc.rate || got.AgentsMayPost != tc.agents || got.HumansMayPost != tc.human {
				t.Fatalf("got %+v", got)
			}
			if tc.in != nil && (tc.in.MaxHops != orig.MaxHops || tc.in.AgentsMayPost != orig.AgentsMayPost) {
				t.Fatal("input modified")
			}
		})
	}
	p := WithDefaults(&mycelv1.ChannelPolicy{OpenToMembers: true, AllowedProfiles: []string{"dev"}, Retention: "720h"}, topic)
	if !p.OpenToMembers || len(p.AllowedProfiles) != 1 || p.Retention != "720h" {
		t.Fatalf("other fields lost: %+v", p)
	}
}

func TestHopAndThread(t *testing.T) {
	root := &mycelv1.Message{Id: "msg_root", Meta: &mycelv1.Meta{Hop: 0}}
	threaded := &mycelv1.Message{Id: "msg_t", Thread: "thr_1", Meta: &mycelv1.Meta{Hop: 2}}
	tests := []struct {
		name     string
		parent   *mycelv1.Message
		max      int32
		hop      int32
		exceeded bool
	}{
		{"no parent", nil, 3, 0, false},
		{"reply to root", root, 3, 1, false},
		{"at limit", threaded, 3, 3, false},
		{"over limit", threaded, 2, 3, true},
		{"default max", &mycelv1.Message{Meta: &mycelv1.Meta{Hop: 8}}, 0, 9, true},
		{"parent without meta", &mycelv1.Message{}, 3, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hop, ex := Hop(tc.parent, tc.max)
			if hop != tc.hop || ex != tc.exceeded {
				t.Fatalf("Hop = %d %v", hop, ex)
			}
		})
	}
	for _, tc := range []struct {
		explicit string
		parent   *mycelv1.Message
		want     string
	}{
		{"", nil, ""},
		{"thr_x", nil, "thr_x"},
		{"thr_x", threaded, "thr_1"},
		{"thr_x", root, "msg_root"},
		{"", threaded, "thr_1"},
		{"", root, "msg_root"},
	} {
		if got := Thread(tc.explicit, tc.parent); got != tc.want {
			t.Errorf("Thread(%q, %v) = %q, want %q", tc.explicit, tc.parent.GetId(), got, tc.want)
		}
	}
}

func TestMatches(t *testing.T) {
	const me = "agent:me@n"
	chat := &mycelv1.Message{Type: mycelv1.MessageType_MESSAGE_TYPE_CHAT, Payload: &mycelv1.Payload{Tags: []string{"ci"}}}
	task := &mycelv1.Message{Type: mycelv1.MessageType_MESSAGE_TYPE_TASK, To: []string{me}}
	untyped := &mycelv1.Message{}
	tests := []struct {
		name string
		sub  *mycelv1.Subscription
		msg  *mycelv1.Message
		want bool
	}{
		{"nil sub", nil, chat, true},
		{"nil msg", nil, nil, false},
		{"empty sub", &mycelv1.Subscription{}, chat, true},
		{"mentions only, not mentioned", &mycelv1.Subscription{MentionsOnly: true}, chat, false},
		{"mentions only, mentioned", &mycelv1.Subscription{MentionsOnly: true}, task, true},
		{"types match", &mycelv1.Subscription{Types: []mycelv1.MessageType{mycelv1.MessageType_MESSAGE_TYPE_TASK}}, task, true},
		{"types miss", &mycelv1.Subscription{Types: []mycelv1.MessageType{mycelv1.MessageType_MESSAGE_TYPE_TASK}}, chat, false},
		{"unspecified is chat", &mycelv1.Subscription{Types: []mycelv1.MessageType{mycelv1.MessageType_MESSAGE_TYPE_CHAT}}, untyped, true},
		{"tags match", &mycelv1.Subscription{Tags: []string{"x", "ci"}}, chat, true},
		{"tags miss", &mycelv1.Subscription{Tags: []string{"x"}}, chat, false},
		{"tags, message without tags", &mycelv1.Subscription{Tags: []string{"x"}}, task, false},
		{"all filters", &mycelv1.Subscription{MentionsOnly: true, Types: []mycelv1.MessageType{mycelv1.MessageType_MESSAGE_TYPE_TASK}}, task, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Matches(tc.sub, tc.msg, me); got != tc.want {
				t.Fatalf("Matches = %v", got)
			}
		})
	}
}

func TestRateLimiter(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	r := NewRateLimiter(clock)
	for i := range 2 {
		if !r.Allow("a", "#c", 2) {
			t.Fatalf("post %d refused", i)
		}
	}
	if r.Allow("a", "#c", 2) {
		t.Fatal("third post allowed")
	}
	if !r.Allow("a", "#other", 2) || !r.Allow("b", "#c", 2) {
		t.Fatal("buckets are not per (agent, channel)")
	}
	advance(29 * time.Second)
	if r.Allow("a", "#c", 2) {
		t.Fatal("allowed before a token refilled")
	}
	advance(2 * time.Second)
	if !r.Allow("a", "#c", 2) {
		t.Fatal("refused after refill")
	}
	advance(time.Hour)
	for range 2 {
		if !r.Allow("a", "#c", 2) {
			t.Fatal("refused after full refill")
		}
	}
	if r.Allow("a", "#c", 2) {
		t.Fatal("burst exceeded rate")
	}
	// Default rate for zero.
	r2 := NewRateLimiter(clock)
	n := 0
	for r2.Allow("a", "#c", 0) {
		n++
	}
	if n != DefaultRatePerMinute {
		t.Fatalf("default burst %d", n)
	}
	// Concurrency.
	r3 := NewRateLimiter(nil)
	var wg sync.WaitGroup
	var allowed sync.Map
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r3.Allow("a", "#c", 10) {
				allowed.Store(i, true)
			}
		}()
	}
	wg.Wait()
	count := 0
	allowed.Range(func(any, any) bool { count++; return true })
	if count != 10 {
		t.Fatalf("concurrent allowed %d, want 10", count)
	}
}

func TestSummary(t *testing.T) {
	ch := &mycelv1.Channel{Name: "#task-x", Type: mycelv1.ChannelType_CHANNEL_TYPE_TASK, Members: []*mycelv1.Member{{Id: "a"}, {Id: "b"}}}
	bb := []*mycelv1.BlackboardEntry{{Key: "status", Value: "green"}, {Key: "long", Value: strings.Repeat("é", 300)}}
	var recent []*mycelv1.Message
	for i := range 25 {
		recent = append(recent, &mycelv1.Message{From: "agent:a@n", Ts: "2026-10-28T14:03:11Z", Payload: &mycelv1.Payload{Text: "msg " + string(rune('a'+i)) + "\nsecond line"}})
	}
	s := Summary(ch, bb, recent)
	for _, want := range []string{"Channel #task-x (task), 2 members.", "status = green", "Last 20 messages:", "[2026-10-28T14:03:11Z] agent:a@n: msg y second line"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "msg a ") || strings.Contains(s, "msg e ") {
		t.Errorf("older messages included:\n%s", s)
	}
	if !strings.Contains(s, "…") || !strings.Contains(s, "long = ") {
		t.Errorf("long value not truncated:\n%s", s)
	}
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 600 {
			t.Errorf("line too long: %d", len(line))
		}
	}
	empty := Summary(&mycelv1.Channel{Name: "#general", Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC}, nil, nil)
	if !strings.Contains(empty, "Blackboard: empty.") || !strings.Contains(empty, "No messages yet.") {
		t.Errorf("empty summary:\n%s", empty)
	}
}
