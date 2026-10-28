package hub

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
)

// testAgent is a registered agent reachable through a stand-in node.
type testAgent struct {
	id    string
	node  string
	sn    *standIn
	token string
}

// call sends an agent RPC and returns the error text of an error reply.
func (a *testAgent) call(t *testing.T, method string, req, resp proto.Message, timeout time.Duration) error {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.sn.nc.Request(bus.NodeRPC(a.node, method), b, timeout)
	if err != nil {
		t.Fatalf("rpc %s: %v", method, err)
	}
	if m.Header.Get(bus.StatusHeader) != bus.StatusOK {
		var cr mycelv1.CtlResponse
		if err := proto.Unmarshal(m.Data, &cr); err != nil || cr.GetError() == "" {
			t.Fatalf("error reply without CtlResponse error: %v", err)
		}
		return errors.New(cr.GetError())
	}
	if err := proto.Unmarshal(m.Data, resp); err != nil {
		t.Fatal(err)
	}
	return nil
}

func (a *testAgent) ac() *mycelv1.AgentCall { return &mycelv1.AgentCall{Token: a.token} }

func (a *testAgent) post(t *testing.T, p *mycelv1.PostRequest) (*mycelv1.Message, error) {
	t.Helper()
	var resp mycelv1.PostResponse
	err := a.call(t, bus.RPCAgentPost, &mycelv1.AgentPostRequest{Call: a.ac(), Post: p}, &resp, 5*time.Second)
	return resp.GetMessage(), err
}

func (a *testAgent) mustPost(t *testing.T, p *mycelv1.PostRequest) *mycelv1.Message {
	t.Helper()
	m, err := a.post(t, p)
	if err != nil {
		t.Fatalf("%s post to %s: %v", a.id, p.GetChannel(), err)
	}
	return m
}

func (a *testAgent) join(t *testing.T, name string, sub *mycelv1.Subscription) (*mycelv1.AgentJoinChannelResponse, error) {
	t.Helper()
	var resp mycelv1.AgentJoinChannelResponse
	err := a.call(t, bus.RPCAgentJoinChannel, &mycelv1.AgentJoinChannelRequest{Call: a.ac(), Channel: name, Subscription: sub}, &resp, 5*time.Second)
	return &resp, err
}

func (a *testAgent) pull(t *testing.T, after uint64, waitMs int32) *mycelv1.InboxPullResponse {
	t.Helper()
	var resp mycelv1.InboxPullResponse
	if err := a.call(t, bus.RPCAgentInboxPull, &mycelv1.InboxPullRequest{Call: a.ac(), AfterSeq: after, WaitMs: waitMs},
		&resp, time.Duration(waitMs)*time.Millisecond+10*time.Second); err != nil {
		t.Fatalf("%s pull: %v", a.id, err)
	}
	return &resp
}

func registerAgent(t *testing.T, sn *standIn, node, name string) *testAgent {
	t.Helper()
	var reg mycelv1.RegisterAgentResponse
	if _, err := sn.rpc(t, node, bus.RPCRegisterAgent, &mycelv1.RegisterAgentRequest{AgentName: name, Harness: "fake"}, &reg); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return &testAgent{id: reg.GetAgentId(), node: node, sn: sn, token: reg.GetToken()}
}

// streamBearer adds the API key to unary and streaming calls.
type streamBearer string

func (k streamBearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(k))
		return next(ctx, req)
	}
}

func (k streamBearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(k))
		return conn
	}
}

func (k streamBearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func wantPrefix(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("want error starting with %q, got %v", prefix, err)
	}
}

func texts(msgs []*mycelv1.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.GetPayload().GetText())
	}
	return out
}

func TestChannelsEndToEnd(t *testing.T) {
	srvCh := make(chan *server, 1)
	testHookServer = func(s *server) { srvCh <- s }
	t.Cleanup(func() { testHookServer = nil })
	h := startTestHub(t)
	srv := <-srvCh
	ctx := context.Background()

	admin := h.clients(h.init.AdminAPIKey, nil)
	anon := h.clients("", nil)
	aliceID, aliceKey := createUser(t, admin, "alice", mycelv1.Role_ROLE_MEMBER)
	bobID, bobKey := createUser(t, admin, "bob", mycelv1.Role_ROLE_MEMBER)
	alice, bob := h.clients(aliceKey, nil), h.clients(bobKey, nil)
	hc := h.httpClient(nil)
	chans := func(key string) mycelv1connect.ChannelServiceClient {
		return mycelv1connect.NewChannelServiceClient(hc, h.ready.HubURL, connect.WithInterceptors(streamBearer(key)))
	}
	adminCh, aliceCh, bobCh := chans(h.init.AdminAPIKey), chans(aliceKey), chans(bobKey)

	// System channels exist from init.
	lc, err := aliceCh.ListChannels(ctx, connect.NewRequest(&mycelv1.ListChannelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range lc.Msg.GetChannels() {
		names = append(names, c.GetName())
	}
	if !slices.Contains(names, channels.General) || !slices.Contains(names, channels.Alerts) {
		t.Fatalf("system channels missing: %v", names)
	}

	// Two nodes, owned by alice and bob, with agents.
	n1 := enrol(t, anon, createToken(t, alice, "n1", nil, ""), "n1")
	n2 := enrol(t, anon, createToken(t, bob, "n2", nil, ""), "n2")
	s1, s2 := h.connectNode(t, n1), h.connectNode(t, n2)
	a1 := registerAgent(t, s1, "n1", "a1")
	a2 := registerAgent(t, s2, "n2", "a2")
	a3 := registerAgent(t, s1, "n1", "a3")

	// #alerts carries enrol and register events; humans read it.
	eventually(t, 5*time.Second, "alerts", func() bool {
		r, err := aliceCh.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: channels.Alerts}))
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(texts(r.Msg.GetMessages()), "\n")
		return strings.Contains(all, "node n2 enrolled") && strings.Contains(all, "agent agent:a3@n1 registered")
	})

	// ---- humans: create, members, post, list, watch ----
	cr, err := aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "#backend"}))
	if err != nil {
		t.Fatal(err)
	}
	if cr.Msg.GetChannel().GetOwnerId() != aliceID || cr.Msg.GetChannel().GetType() != mycelv1.ChannelType_CHANNEL_TYPE_TOPIC ||
		cr.Msg.GetChannel().GetPolicy().GetMaxHops() != channels.DefaultMaxHops {
		t.Fatalf("created: %+v", cr.Msg.GetChannel())
	}
	_, err = aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "#backend"}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "#alerts"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "Bad"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = bobCh.GetChannel(ctx, connect.NewRequest(&mycelv1.GetChannelRequest{Name: "#backend"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = bobCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#backend", Text: "hi"}))
	wantCode(t, err, connect.CodePermissionDenied)

	// A member user cannot update another owner's policy.
	if _, err := aliceCh.AddMember(ctx, connect.NewRequest(&mycelv1.AddMemberRequest{Channel: "#backend", Member: "user:" + bobID})); err != nil {
		t.Fatal(err)
	}
	_, err = bobCh.UpdateChannelPolicy(ctx, connect.NewRequest(&mycelv1.UpdateChannelPolicyRequest{Name: "#backend", Policy: &mycelv1.ChannelPolicy{MaxHops: 1}}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = bobCh.AddMember(ctx, connect.NewRequest(&mycelv1.AddMemberRequest{Channel: "#backend", Member: a2.id}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = aliceCh.AddMember(ctx, connect.NewRequest(&mycelv1.AddMemberRequest{Channel: "#backend", Member: "agent:ghost@n1"}))
	wantCode(t, err, connect.CodeNotFound)

	// Watch #backend as alice.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	stream, err := aliceCh.Watch(wctx, connect.NewRequest(&mycelv1.WatchRequest{Channels: []string{"#backend"}}))
	if err != nil {
		t.Fatal(err)
	}
	watched := make(chan *mycelv1.Message, 16)
	go func() {
		for stream.Receive() {
			watched <- stream.Msg().GetMessage()
		}
		close(watched)
	}()

	pr, err := bobCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#backend", Text: "hello backend", Tags: []string{"greeting"}}))
	if err != nil {
		t.Fatal(err)
	}
	hm := pr.Msg.GetMessage()
	if hm.GetFrom() != "user:"+bobID || hm.GetSeq() == 0 || !channels.ValidMessageID(hm.GetId()) ||
		hm.GetMeta().GetProvenance().GetOrigin() != "human" || hm.GetMeta().GetProvenance().GetUntrusted() ||
		hm.GetType() != mycelv1.MessageType_MESSAGE_TYPE_CHAT || hm.GetMeta().GetTraceId() != hm.GetId() {
		t.Fatalf("human message: %+v", hm)
	}
	lm, err := aliceCh.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: "#backend"}))
	if err != nil || len(lm.Msg.GetMessages()) != 1 || lm.Msg.GetMessages()[0].GetId() != hm.GetId() {
		t.Fatalf("ListMessages: %v %+v", err, lm)
	}
	select {
	case m := <-watched:
		if m.GetId() != hm.GetId() {
			t.Fatalf("watched %+v (stream err %v)", m, stream.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not deliver")
	}
	wcancel()

	// ---- agents: task channel, inbox, long poll ----
	jr, err := a1.join(t, "#task-demo", nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if jr.GetChannel().GetType() != mycelv1.ChannelType_CHANNEL_TYPE_TASK || jr.GetChannel().GetOwnerId() != aliceID ||
		!jr.GetChannel().GetPolicy().GetOpenToMembers() || len(jr.GetChannel().GetMembers()) != 1 ||
		jr.GetChannel().GetMembers()[0].GetRole() != "owner" || jr.GetSummary().GetSummary() == "" {
		t.Fatalf("join created: %+v", jr)
	}
	if _, err := a2.join(t, "#task-demo", nil); err != nil {
		t.Fatalf("a2 join: %v", err)
	}
	_, err = a1.join(t, "#nope", nil)
	wantPrefix(t, err, "not_found: ")

	cursor := a2.pull(t, 0, 0).GetNextSeq()
	type pullResult struct {
		resp *mycelv1.InboxPullResponse
		took time.Duration
	}
	pulled := make(chan pullResult, 1)
	go func() {
		start := time.Now()
		var resp mycelv1.InboxPullResponse
		b, _ := proto.Marshal(&mycelv1.InboxPullRequest{Call: a2.ac(), AfterSeq: cursor, WaitMs: 20000})
		m, err := a2.sn.nc.Request(bus.NodeRPC("n2", bus.RPCAgentInboxPull), b, 30*time.Second)
		if err == nil && m.Header.Get(bus.StatusHeader) == bus.StatusOK {
			_ = proto.Unmarshal(m.Data, &resp)
		}
		pulled <- pullResult{&resp, time.Since(start)}
	}()
	eventually(t, 5*time.Second, "long poll waiting", func() bool {
		return srv.ch.count(srv.ch.agentPolls, a2.id) == 1
	})
	m1 := a1.mustPost(t, &mycelv1.PostRequest{Channel: "#task-demo", Text: "please review", Type: mycelv1.MessageType_MESSAGE_TYPE_TASK})
	select {
	case r := <-pulled:
		if len(r.resp.GetMessages()) != 1 || r.resp.GetMessages()[0].GetId() != m1.GetId() || r.took > 10*time.Second ||
			r.resp.GetNextSeq() != m1.GetSeq() {
			t.Fatalf("long poll: %+v after %v", r.resp, r.took)
		}
		got := r.resp.GetMessages()[0]
		p := got.GetMeta().GetProvenance()
		if got.GetFrom() != a1.id || p.GetOrigin() != "agent" || !p.GetUntrusted() || p.GetNode() != "n1" ||
			got.GetType() != mycelv1.MessageType_MESSAGE_TYPE_TASK {
			t.Fatalf("agent message: %+v", got)
		}
		cursor = r.resp.GetNextSeq()
	case <-time.After(15 * time.Second):
		t.Fatal("long poll did not return")
	}
	// The sender does not get its own message.
	if r := a1.pull(t, 0, 0); len(r.GetMessages()) != 0 {
		t.Fatalf("sender received own message: %v", texts(r.GetMessages()))
	}

	// mentions_only: non-matching traffic advances the cursor, mentions arrive.
	if _, err := a2.join(t, "#task-demo", &mycelv1.Subscription{MentionsOnly: true}); err != nil {
		t.Fatal(err)
	}
	m2 := a1.mustPost(t, &mycelv1.PostRequest{Channel: "#task-demo", Text: "status chatter"})
	r := a2.pull(t, cursor, 300)
	if len(r.GetMessages()) != 0 || r.GetNextSeq() < m2.GetSeq() {
		t.Fatalf("mentions_only pull: %v next %d (m2 %d)", texts(r.GetMessages()), r.GetNextSeq(), m2.GetSeq())
	}
	cursor = r.GetNextSeq()
	m3 := a1.mustPost(t, &mycelv1.PostRequest{Channel: "#task-demo", Text: "@a2 your turn", Mentions: []string{a2.id}})
	r = a2.pull(t, cursor, 5000)
	if len(r.GetMessages()) != 1 || r.GetMessages()[0].GetId() != m3.GetId() || !slices.Equal(r.GetMessages()[0].GetTo(), []string{a2.id}) {
		t.Fatalf("mention pull: %+v", r)
	}
	cursor = r.GetNextSeq()
	_, err = a1.post(t, &mycelv1.PostRequest{Channel: "#task-demo", Text: "x", Mentions: []string{a3.id}})
	wantPrefix(t, err, "invalid_argument: ")
	for name, bad := range map[string]*mycelv1.PostRequest{
		"tag upper":       {Tags: []string{"CI"}},
		"tag space":       {Tags: []string{"a b"}},
		"tag long":        {Tags: []string{strings.Repeat("a", 33)}},
		"thread":          {Thread: "thr_1"},
		"thread newline":  {Thread: "thr_0123456789ABCDEFGHJKMNPQR\n"},
		"reply_to":        {ReplyTo: "thr_0123456789ABCDEFGHJKMNPQRS"},
		"mention":         {Mentions: []string{"agent:x"}},
		"mention bracket": {Mentions: []string{"user:a]b"}},
		"part kind":       {Parts: []*mycelv1.Part{{Kind: "Art1", Ref: "r"}}},
		"part kind long":  {Parts: []*mycelv1.Part{{Kind: strings.Repeat("a", 17), Ref: "r"}}},
		"part ref ctrl":   {Parts: []*mycelv1.Part{{Kind: "link", Ref: "a\nb"}}},
		"part ref long":   {Parts: []*mycelv1.Part{{Kind: "link", Ref: strings.Repeat("r", 257)}}},
		"part name ls":    {Parts: []*mycelv1.Part{{Kind: "link", Ref: "r", Name: "a\u2028b"}}},
		"channel":         {Channel: "#Bad"},
		"empty":           {Text: "-"},
	} {
		if bad.Channel == "" {
			bad.Channel = "#task-demo"
		}
		switch bad.Text {
		case "":
			bad.Text = "x"
		case "-":
			bad.Text = ""
		}
		_, err = a1.post(t, bad)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid_argument: ") {
			t.Fatalf("%s: want invalid_argument, got %v", name, err)
		}
	}
	if _, err := a2.join(t, "#task-demo", nil); err != nil {
		t.Fatal(err)
	}

	// ---- direct message ----
	dm := a1.mustPost(t, &mycelv1.PostRequest{Channel: "@" + a2.id, Text: "psst"})
	if dm.GetChannel() != channels.DirectName(a1.id, a2.id) || !slices.Contains(dm.GetTo(), a2.id) {
		t.Fatalf("dm: %+v", dm)
	}
	r = a2.pull(t, cursor, 5000)
	if len(r.GetMessages()) != 1 || r.GetMessages()[0].GetId() != dm.GetId() {
		t.Fatalf("dm pull: %+v", r)
	}
	cursor = r.GetNextSeq()
	if r := a3.pull(t, 0, 0); len(r.GetMessages()) != 0 {
		t.Fatalf("third agent received: %v", texts(r.GetMessages()))
	}
	_, err = bobCh.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: dm.GetChannel()}))
	wantCode(t, err, connect.CodePermissionDenied)
	var reply mycelv1.ListMessagesResponse
	if err := a2.call(t, bus.RPCAgentListMessages, &mycelv1.AgentListMessagesRequest{Call: a2.ac(), List: &mycelv1.ListMessagesRequest{Channel: dm.GetChannel()}}, &reply, 5*time.Second); err != nil || len(reply.GetMessages()) != 1 {
		t.Fatalf("dm list: %v %+v", err, &reply)
	}

	// ---- hop limit ----
	if _, err := aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#loop", Policy: &mycelv1.ChannelPolicy{MaxHops: 3}, Members: []string{a1.id, a2.id},
	})); err != nil {
		t.Fatal(err)
	}
	parent := a1.mustPost(t, &mycelv1.PostRequest{Channel: "#loop", Text: "ping 0"})
	agents := []*testAgent{a2, a1}
	for i := 1; i <= 3; i++ {
		parent = agents[i%2].mustPost(t, &mycelv1.PostRequest{Channel: "#loop", Text: "pong", ReplyTo: parent.GetId()})
		if parent.GetMeta().GetHop() != int32(i) || parent.GetThread() == "" {
			t.Fatalf("hop %d: %+v", i, parent.GetMeta())
		}
	}
	for range 2 {
		_, err = a2.post(t, &mycelv1.PostRequest{Channel: "#loop", Text: "pong", ReplyTo: parent.GetId()})
		wantPrefix(t, err, "resource_exhausted: hop limit (3) reached")
	}
	lr, err := aliceCh.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: "#loop"}))
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	for _, m := range lr.Msg.GetMessages() {
		if m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_EVENT && m.GetFrom() == "hub" && strings.Contains(m.GetPayload().GetText(), "hop limit (3)") {
			events++
			if m.GetThread() != parent.GetThread() || m.GetMeta().GetProvenance().GetOrigin() != "hub" {
				t.Fatalf("event: %+v", m)
			}
			// Replying to the event cannot restart the loop.
			_, err := a1.post(t, &mycelv1.PostRequest{Channel: "#loop", Text: "again", ReplyTo: m.GetId()})
			wantPrefix(t, err, "resource_exhausted: hop limit")
		}
	}
	if events != 1 {
		t.Fatalf("%d hop events", events)
	}
	thr, err := aliceCh.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: "#loop", Thread: parent.GetThread()}))
	if err != nil || len(thr.Msg.GetMessages()) != 5 {
		t.Fatalf("thread listing: %v %d", err, len(thr.Msg.GetMessages()))
	}

	// ---- rate limit ----
	if _, err := aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#rate", Policy: &mycelv1.ChannelPolicy{RatePerMinute: 2}, Members: []string{a1.id},
	})); err != nil {
		t.Fatal(err)
	}
	a1.mustPost(t, &mycelv1.PostRequest{Channel: "#rate", Text: "1"})
	a1.mustPost(t, &mycelv1.PostRequest{Channel: "#rate", Text: "2"})
	_, err = a1.post(t, &mycelv1.PostRequest{Channel: "#rate", Text: "3"})
	wantPrefix(t, err, "resource_exhausted: rate limit")

	// ---- system channels and non-members ----
	if _, err := a1.join(t, channels.Alerts, nil); err != nil {
		t.Fatalf("join #alerts: %v", err)
	}
	_, err = a1.post(t, &mycelv1.PostRequest{Channel: channels.Alerts, Text: "fake alert"})
	wantPrefix(t, err, "permission_denied: ")
	_, err = adminCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: channels.Alerts, Text: "admin alert"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = a3.post(t, &mycelv1.PostRequest{Channel: "#backend", Text: "intrude"})
	wantPrefix(t, err, "permission_denied: ")
	err = a3.call(t, bus.RPCAgentListMessages, &mycelv1.AgentListMessagesRequest{Call: a3.ac(), List: &mycelv1.ListMessagesRequest{Channel: "#backend"}}, &reply, 5*time.Second)
	wantPrefix(t, err, "permission_denied: ")
	_, err = a3.join(t, "#backend", nil)
	wantPrefix(t, err, "permission_denied: ")
	var bbr mycelv1.BlackboardGetResponse
	err = a3.call(t, bus.RPCAgentBBGet, &mycelv1.AgentBlackboardGetRequest{Call: a3.ac(), Get: &mycelv1.BlackboardGetRequest{Channel: "#task-demo"}}, &bbr, 5*time.Second)
	wantPrefix(t, err, "permission_denied: ")
	// #general is open: a3 may join and post.
	if _, err := a3.join(t, channels.General, nil); err != nil {
		t.Fatal(err)
	}
	a3.mustPost(t, &mycelv1.PostRequest{Channel: channels.General, Text: "hello all"})
	// Observers read but do not post.
	if _, err := aliceCh.AddMember(ctx, connect.NewRequest(&mycelv1.AddMemberRequest{Channel: "#backend", Member: a3.id, Role: "observer"})); err != nil {
		t.Fatal(err)
	}
	_, err = a3.post(t, &mycelv1.PostRequest{Channel: "#backend", Text: "observing"})
	wantPrefix(t, err, "permission_denied: observers")

	// ---- blackboard ----
	put := func(c mycelv1connect.ChannelServiceClient, key, value string, expect uint64) (*mycelv1.BlackboardEntry, error) {
		r, err := c.BlackboardPut(ctx, connect.NewRequest(&mycelv1.BlackboardPutRequest{Channel: "#backend", Key: key, Value: value, ExpectVersion: expect}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetEntry(), nil
	}
	e, err := put(aliceCh, "status", "red", 0)
	if err != nil || e.GetVersion() != 1 || e.GetUpdatedBy() != "user:"+aliceID {
		t.Fatalf("put: %v %+v", err, e)
	}
	if e, err = put(bobCh, "status", "green", 1); err != nil || e.GetVersion() != 2 {
		t.Fatalf("CAS put: %v %+v", err, e)
	}
	_, err = put(aliceCh, "status", "blue", 1)
	wantCode(t, err, connect.CodeAborted)
	_, err = put(aliceCh, "big", strings.Repeat("x", channels.MaxBlackboardValue+1), 0)
	wantCode(t, err, connect.CodeInvalidArgument)
	g, err := aliceCh.BlackboardGet(ctx, connect.NewRequest(&mycelv1.BlackboardGetRequest{Channel: "#backend", Key: "status"}))
	if err != nil || len(g.Msg.GetEntries()) != 1 || g.Msg.GetEntries()[0].GetValue() != "green" {
		t.Fatalf("get: %v %+v", err, g)
	}
	if _, err := aliceCh.BlackboardDelete(ctx, connect.NewRequest(&mycelv1.BlackboardDeleteRequest{Channel: "#backend", Key: "status"})); err != nil {
		t.Fatal(err)
	}
	_, err = aliceCh.BlackboardGet(ctx, connect.NewRequest(&mycelv1.BlackboardGetRequest{Channel: "#backend", Key: "status"}))
	wantCode(t, err, connect.CodeNotFound)
	var bp mycelv1.BlackboardPutResponse
	if err := a1.call(t, bus.RPCAgentBBPut, &mycelv1.AgentBlackboardPutRequest{Call: a1.ac(), Put: &mycelv1.BlackboardPutRequest{
		Channel: "#task-demo", Key: "plan", Value: "step one: review auth"}}, &bp, 5*time.Second); err != nil || bp.GetEntry().GetUpdatedBy() != a1.id {
		t.Fatalf("agent bb put: %v %+v", err, &bp)
	}
	if err := a2.call(t, bus.RPCAgentBBGet, &mycelv1.AgentBlackboardGetRequest{Call: a2.ac(), Get: &mycelv1.BlackboardGetRequest{Channel: "#task-demo"}}, &bbr, 5*time.Second); err != nil ||
		len(bbr.GetEntries()) != 1 || bbr.GetEntries()[0].GetValue() != "step one: review auth" {
		t.Fatalf("agent bb get: %v %+v", err, &bbr)
	}
	err = a2.call(t, bus.RPCAgentBBPut, &mycelv1.AgentBlackboardPutRequest{Call: a2.ac(), Put: &mycelv1.BlackboardPutRequest{
		Channel: "#task-demo", Key: "plan", Value: "mine", ExpectVersion: 7}}, &bp, 5*time.Second)
	wantPrefix(t, err, "aborted: ")

	// ---- summary ----
	sum, err := adminCh.ChannelSummary(ctx, connect.NewRequest(&mycelv1.ChannelSummaryRequest{Channel: "#task-demo"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#task-demo (task)", "plan = step one: review auth", "please review", "@a2 your turn"} {
		if !strings.Contains(sum.Msg.GetSummary(), want) {
			t.Errorf("summary lacks %q:\n%s", want, sum.Msg.GetSummary())
		}
	}
	if len(sum.Msg.GetBlackboard()) != 1 || len(sum.Msg.GetRecent()) != 3 {
		t.Fatalf("summary parts: %d bb, %d recent", len(sum.Msg.GetBlackboard()), len(sum.Msg.GetRecent()))
	}

	// ---- discovery ----
	var who mycelv1.AgentWhoamiResponse
	if err := a1.call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{Call: a1.ac()}, &who, 5*time.Second); err != nil ||
		who.GetAgent().GetId() != a1.id || who.GetAgent().GetOwnerId() != aliceID || who.GetTrustDomain() != "localhost" ||
		!slices.Contains(who.GetAgent().GetChannels(), dm.GetChannel()) {
		t.Fatalf("whoami: %v %+v", err, &who)
	}
	var la mycelv1.AgentListAgentsResponse
	if err := a1.call(t, bus.RPCAgentListAgents, &mycelv1.AgentListAgentsRequest{Call: a1.ac(), Channel: "#task-demo"}, &la, 5*time.Second); err != nil || len(la.GetAgents()) != 2 {
		t.Fatalf("list agents: %v %+v", err, &la)
	}
	// a3 is in neither the dm nor #loop: listings never show it those.
	for _, req := range []*mycelv1.AgentListAgentsRequest{{Call: a3.ac(), Channel: channels.General}, {Call: a3.ac()}} {
		if err := a3.call(t, bus.RPCAgentListAgents, req, &la, 5*time.Second); err != nil || len(la.GetAgents()) == 0 {
			t.Fatalf("a3 list agents: %v %+v", err, &la)
		}
		for _, ag := range la.GetAgents() {
			for _, c := range ag.GetChannels() {
				if channels.IsDirect(c) || c == "#loop" || c == "#rate" {
					t.Fatalf("listing reveals %s", c)
				}
			}
		}
	}
	var lch mycelv1.ListChannelsResponse
	if err := a3.call(t, bus.RPCAgentListChannels, &mycelv1.AgentListChannelsRequest{Call: a3.ac()}, &lch, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, c := range lch.GetChannels() {
		seen = append(seen, c.GetName())
	}
	if !slices.Contains(seen, channels.General) || !slices.Contains(seen, "#task-demo") || slices.Contains(seen, "#loop") || slices.Contains(seen, dm.GetChannel()) {
		t.Fatalf("a3 sees %v", seen)
	}
	var leave mycelv1.AgentLeaveChannelResponse
	if err := a2.call(t, bus.RPCAgentLeaveChannel, &mycelv1.AgentLeaveChannelRequest{Call: a2.ac(), Channel: "#task-demo"}, &leave, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a1.mustPost(t, &mycelv1.PostRequest{Channel: "#task-demo", Text: "after leave", Mentions: nil})
	if r := a2.pull(t, cursor, 0); slices.Contains(texts(r.GetMessages()), "after leave") {
		t.Fatal("left agent still receives")
	}

	// ---- JetStream CH and AUDIT ----
	js, err := jetstream.New(srv.nc)
	if err != nil {
		t.Fatal(err)
	}
	chStream, err := js.Stream(ctx, bus.StreamChannels)
	if err != nil {
		t.Fatal(err)
	}
	last, err := chStream.GetLastMsgForSubject(ctx, bus.ChannelSubject("#loop"))
	if err != nil {
		t.Fatal(err)
	}
	var sm mycelv1.Message
	if err := proto.Unmarshal(last.Data, &sm); err != nil || sm.GetChannel() != "#loop" || sm.GetType() != mycelv1.MessageType_MESSAGE_TYPE_EVENT || sm.GetSeq() == 0 {
		t.Fatalf("CH entry: %v %+v", err, &sm)
	}
	if _, err := chStream.GetLastMsgForSubject(ctx, bus.ChannelSubject(dm.GetChannel())); err != nil {
		t.Fatalf("dm not on CH: %v", err)
	}
	info, err := chStream.Info(ctx)
	if err != nil || info.State.Msgs < 15 {
		t.Fatalf("CH stream: %v %+v", err, info)
	}
	auStream, err := js.Stream(ctx, bus.StreamAudit)
	if err != nil {
		t.Fatal(err)
	}
	var rec auditRecord
	eventually(t, 5*time.Second, "audit on AUDIT", func() bool {
		am, err := auStream.GetLastMsgForSubject(ctx, bus.AuditSubject("channel.create"))
		return err == nil && json.Unmarshal(am.Data, &rec) == nil && rec.Action == "channel.create"
	})
	if rec.Outcome != outcomeOK || rec.Resource != "channel:#rate" {
		t.Fatalf("audit record: %+v", rec)
	}

	// ---- auth failures ----
	err = (&testAgent{id: a1.id, node: "n2", sn: s2, token: a1.token}).call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{Call: a1.ac()}, &who, 5*time.Second)
	wantPrefix(t, err, "unauthenticated: invalid agent token")
	bogus := &testAgent{id: a1.id, node: "n1", sn: s1, token: "mca1_bogus.x"} //nolint:gosec // a deliberately invalid token
	err = bogus.call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{}, &who, 5*time.Second)
	wantPrefix(t, err, "unauthenticated: invalid agent token")
	err = a1.call(t, "agent.bogus", &mycelv1.AgentWhoamiRequest{Call: a1.ac()}, &who, 5*time.Second)
	wantPrefix(t, err, "not_found: unknown method agent.bogus")
	var badReg mycelv1.RegisterAgentResponse
	err = a1.call(t, bus.RPCRegisterAgent, &mycelv1.RegisterAgentRequest{AgentName: "Bad.Name"}, &badReg, 5*time.Second)
	wantPrefix(t, err, "invalid_argument: ")

	// A revoked agent's RPCs are refused.
	if _, err := alice.agents.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: a3.id})); err != nil {
		t.Fatal(err)
	}
	_, err = a3.post(t, &mycelv1.PostRequest{Channel: channels.General, Text: "still here"})
	wantPrefix(t, err, "permission_denied: agent revoked")
	var ref mycelv1.RefreshAgentTokenResponse
	err = a3.call(t, bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: a3.id, Token: a3.token}, &ref, 5*time.Second)
	wantPrefix(t, err, "permission_denied: agent revoked")
	_, err = bob.agents.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: a1.id}))
	wantCode(t, err, connect.CodePermissionDenied)

	// Archive: no more posts.
	if _, err := aliceCh.ArchiveChannel(ctx, connect.NewRequest(&mycelv1.ArchiveChannelRequest{Name: "#rate"})); err != nil {
		t.Fatal(err)
	}
	_, err = aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#rate", Text: "late"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = aliceCh.ArchiveChannel(ctx, connect.NewRequest(&mycelv1.ArchiveChannelRequest{Name: channels.General}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = adminCh.ArchiveChannel(ctx, connect.NewRequest(&mycelv1.ArchiveChannelRequest{Name: channels.General}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}
