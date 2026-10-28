package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
)

// chanEnv is a running hub with admin, alice and bob, node n1 (alice: a1)
// and node n2 (bob: a2).
type chanEnv struct {
	h                       *testHub
	srv                     *server
	aliceID, bobID          string
	aliceKey, bobKey        string
	adminCh, aliceCh, bobCh mycelv1connect.ChannelServiceClient
	s1, s2                  *standIn
	a1, a2                  *testAgent
}

func newChanEnv(t *testing.T) *chanEnv {
	t.Helper()
	srvCh := make(chan *server, 1)
	testHookServer = func(s *server) { srvCh <- s }
	t.Cleanup(func() { testHookServer = nil })
	h := startTestHub(t)
	e := &chanEnv{h: h, srv: <-srvCh}
	admin := h.clients(h.init.AdminAPIKey, nil)
	anon := h.clients("", nil)
	var aliceKey, bobKey string
	e.aliceID, aliceKey = createUser(t, admin, "alice", mycelv1.Role_ROLE_MEMBER)
	e.bobID, bobKey = createUser(t, admin, "bob", mycelv1.Role_ROLE_MEMBER)
	hc := h.httpClient(nil)
	chans := func(key string) mycelv1connect.ChannelServiceClient {
		return mycelv1connect.NewChannelServiceClient(hc, h.ready.HubURL, connect.WithInterceptors(streamBearer(key)))
	}
	e.adminCh, e.aliceCh, e.bobCh = chans(h.init.AdminAPIKey), chans(aliceKey), chans(bobKey)
	e.aliceKey, e.bobKey = aliceKey, bobKey
	n1 := enrol(t, anon, createToken(t, h.clients(aliceKey, nil), "n1", nil, ""), "n1")
	n2 := enrol(t, anon, createToken(t, h.clients(bobKey, nil), "n2", nil, ""), "n2")
	e.s1, e.s2 = h.connectNode(t, n1), h.connectNode(t, n2)
	e.a1 = registerAgent(t, e.s1, "n1", "a1")
	e.a2 = registerAgent(t, e.s2, "n2", "a2")
	return e
}

func (e *chanEnv) create(t *testing.T, c mycelv1connect.ChannelServiceClient, name string, pol *mycelv1.ChannelPolicy, members ...string) {
	t.Helper()
	if _, err := c.CreateChannel(context.Background(), connect.NewRequest(&mycelv1.CreateChannelRequest{Name: name, Policy: pol, Members: members})); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func (e *chanEnv) list(t *testing.T, c mycelv1connect.ChannelServiceClient, name string) []*mycelv1.Message {
	t.Helper()
	r, err := c.ListMessages(context.Background(), connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: name, Limit: 500}))
	if err != nil {
		t.Fatalf("list %s: %v", name, err)
	}
	return r.Msg.GetMessages()
}

func TestChannelsHardening(t *testing.T) {
	e := newChanEnv(t)
	ctx := context.Background()
	a1, a2 := e.a1, e.a2

	t.Run("reply_to in another channel", func(t *testing.T) {
		e.create(t, e.aliceCh, "#one", nil, a1.id)
		e.create(t, e.aliceCh, "#two", nil, a1.id)
		m := a1.mustPost(t, &mycelv1.PostRequest{Channel: "#one", Text: "in one"})
		_, err := a1.post(t, &mycelv1.PostRequest{Channel: "#two", Text: "reply", ReplyTo: m.GetId()})
		wantPrefix(t, err, "invalid_argument: reply_to")
		_, err = a1.post(t, &mycelv1.PostRequest{Channel: "#one", Text: "reply", ReplyTo: channels.NewMessageID(time.Now())})
		wantPrefix(t, err, "invalid_argument: reply_to")
	})

	t.Run("join, history and inbox limit", func(t *testing.T) {
		if _, err := a1.join(t, "#task-hist", nil); err != nil {
			t.Fatal(err)
		}
		a1.mustPost(t, &mycelv1.PostRequest{Channel: "#task-hist", Text: "before join"})
		if _, err := a2.join(t, "#task-hist", nil); err != nil {
			t.Fatal(err)
		}
		if r := a2.pull(t, 0, 0); len(r.GetMessages()) != 0 {
			t.Fatalf("message from before the join delivered: %v", texts(r.GetMessages()))
		}
		var sent []*mycelv1.Message
		for i := range 3 {
			sent = append(sent, a1.mustPost(t, &mycelv1.PostRequest{Channel: "#task-hist", Text: fmt.Sprintf("after %d", i)}))
		}
		var r mycelv1.InboxPullResponse
		if err := a2.call(t, bus.RPCAgentInboxPull, &mycelv1.InboxPullRequest{Call: a2.ac(), Limit: 2}, &r, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if len(r.GetMessages()) != 2 || r.GetNextSeq() != sent[1].GetSeq() {
			t.Fatalf("limited pull: %v next %d (want %d)", texts(r.GetMessages()), r.GetNextSeq(), sent[1].GetSeq())
		}
		rest := a2.pull(t, r.GetNextSeq(), 0)
		if len(rest.GetMessages()) != 1 || rest.GetMessages()[0].GetId() != sent[2].GetId() {
			t.Fatalf("rest: %v", texts(rest.GetMessages()))
		}
	})

	t.Run("hop-limit event spam", func(t *testing.T) {
		e.create(t, e.aliceCh, "#spam", &mycelv1.ChannelPolicy{MaxHops: 1, RatePerMinute: 6000}, a1.id, a2.id)
		m0 := a1.mustPost(t, &mycelv1.PostRequest{Channel: "#spam", Text: "ping"})
		r1 := a2.mustPost(t, &mycelv1.PostRequest{Channel: "#spam", Text: "pong", ReplyTo: m0.GetId()})
		// A reply naming another thread is refused outright.
		_, err := a1.post(t, &mycelv1.PostRequest{Channel: "#spam", Text: "x", ReplyTo: r1.GetId(), Thread: channels.NewThreadID(time.Now())})
		wantPrefix(t, err, "invalid_argument: a reply stays in its parent's thread")
		for i := range 100 {
			req := &mycelv1.PostRequest{Channel: "#spam", Text: "again", ReplyTo: r1.GetId()}
			if i%2 == 0 {
				req.Thread = channels.NewThreadID(time.Now())
			}
			_, err := a1.post(t, req)
			if err == nil {
				t.Fatalf("refused reply %d accepted", i)
			}
		}
		events := 0
		for _, m := range e.list(t, e.aliceCh, "#spam") {
			if m.GetFrom() == "hub" {
				events++
				if m.GetThread() != r1.GetThread() {
					t.Fatalf("event in thread %s, chain thread %s", m.GetThread(), r1.GetThread())
				}
			}
		}
		if events != 1 {
			t.Fatalf("%d hop events, want 1", events)
		}
	})

	t.Run("direct channels", func(t *testing.T) {
		dm := a1.mustPost(t, &mycelv1.PostRequest{Channel: "@" + a2.id, Text: "private"})
		g, err := e.adminCh.GetChannel(ctx, connect.NewRequest(&mycelv1.GetChannelRequest{Name: dm.GetChannel()}))
		if err != nil || g.Msg.GetChannel().GetOwnerId() != "" || len(g.Msg.GetChannel().GetMembers()) != 2 {
			t.Fatalf("dm channel: %v %+v", err, g)
		}
		// a1's owner may not change or archive it, nor read it.
		_, err = e.aliceCh.UpdateChannelPolicy(ctx, connect.NewRequest(&mycelv1.UpdateChannelPolicyRequest{Name: dm.GetChannel(), Policy: &mycelv1.ChannelPolicy{MaxHops: 1}}))
		wantCode(t, err, connect.CodePermissionDenied)
		_, err = e.aliceCh.ArchiveChannel(ctx, connect.NewRequest(&mycelv1.ArchiveChannelRequest{Name: dm.GetChannel()}))
		wantCode(t, err, connect.CodePermissionDenied)
		_, err = e.bobCh.ListMessages(ctx, connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: dm.GetChannel()}))
		wantCode(t, err, connect.CodePermissionDenied)
		var leave mycelv1.AgentLeaveChannelResponse
		err = a1.call(t, bus.RPCAgentLeaveChannel, &mycelv1.AgentLeaveChannelRequest{Call: a1.ac(), Channel: dm.GetChannel()}, &leave, 5*time.Second)
		wantPrefix(t, err, "failed_precondition: ")
		// Admins may read it; each read is audited.
		if got := e.list(t, e.adminCh, dm.GetChannel()); len(got) != 1 {
			t.Fatalf("admin dm read: %d messages", len(got))
		}
		entries, err := e.srv.st.listAudit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, en := range entries {
			if en.Action == "channel.read" && en.Resource == "channel:"+dm.GetChannel() && en.Actor == e.h.init.AdminUserID {
				found = true
			}
		}
		if !found {
			t.Fatal("admin read of a dm not audited")
		}
		// A refused DM post does not create the channel.
		_, err = a1.post(t, &mycelv1.PostRequest{Channel: "@user:" + e.bobID, Text: "x", ReplyTo: dm.GetId()})
		wantPrefix(t, err, "invalid_argument: ")
		if _, err := e.srv.st.getChannel(ctx, channels.DirectName(a1.id, "user:"+e.bobID)); err == nil {
			t.Fatal("refused dm post created the channel")
		}
	})

	t.Run("watch stops when access is lost", func(t *testing.T) {
		e.create(t, e.adminCh, "#private", nil, "user:"+e.bobID)
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stream, err := e.bobCh.Watch(wctx, connect.NewRequest(&mycelv1.WatchRequest{Channels: []string{"#private"}}))
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan *mycelv1.Message, 8)
		go func() {
			for stream.Receive() {
				if m := stream.Msg().GetMessage(); m != nil {
					got <- m
				}
			}
			close(got)
		}()
		if _, err := e.adminCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#private", Text: "one"})); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-got:
			if m.GetPayload().GetText() != "one" {
				t.Fatalf("watched %+v", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no message")
		}
		if _, err := e.adminCh.RemoveMember(ctx, connect.NewRequest(&mycelv1.RemoveMemberRequest{Channel: "#private", Member: "user:" + e.bobID})); err != nil {
			t.Fatal(err)
		}
		if _, err := e.adminCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#private", Text: "two"})); err != nil {
			t.Fatal(err)
		}
		select {
		case m, ok := <-got:
			if ok {
				t.Fatalf("message after access was lost: %+v", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not end")
		}
		if connect.CodeOf(stream.Err()) != connect.CodePermissionDenied {
			t.Fatalf("stream error: %v", stream.Err())
		}
	})

	t.Run("pull caps and node busy", func(t *testing.T) {
		var bs []*testAgent
		for i := range 5 {
			bs = append(bs, registerAgent(t, e.s2, "n2", fmt.Sprintf("b%d", i)))
		}
		var wg sync.WaitGroup
		startPull := func(a *testAgent) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b, _ := proto.Marshal(&mycelv1.InboxPullRequest{Call: a.ac(), WaitMs: 3000})
				_, _ = a.sn.nc.Request(bus.NodeRPC(a.node, bus.RPCAgentInboxPull), b, 20*time.Second)
			}()
		}
		for range maxPullsPerAgent {
			startPull(bs[0])
		}
		eventually(t, 5*time.Second, "b0 pulls waiting", func() bool { return e.srv.ch.count(e.srv.ch.agentPolls, bs[0].id) == maxPullsPerAgent })
		var r mycelv1.InboxPullResponse
		err := bs[0].call(t, bus.RPCAgentInboxPull, &mycelv1.InboxPullRequest{Call: bs[0].ac(), WaitMs: 1000}, &r, 5*time.Second)
		wantPrefix(t, err, "resource_exhausted: too many concurrent inbox pulls for this agent")
		for _, b := range bs[1:4] {
			for range maxPullsPerAgent {
				startPull(b)
			}
		}
		eventually(t, 5*time.Second, "n2 at its long-poll cap", func() bool { return e.srv.ch.count(e.srv.ch.nodePolls, "n2") == maxNodePolls })
		err = bs[4].call(t, bus.RPCAgentInboxPull, &mycelv1.InboxPullRequest{Call: bs[4].ac(), WaitMs: 1000}, &r, 5*time.Second)
		wantPrefix(t, err, "resource_exhausted: node busy")
		// Other RPCs of the busy node, and other nodes, still work.
		var who mycelv1.AgentWhoamiResponse
		if err := bs[4].call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{Call: bs[4].ac()}, &who, 5*time.Second); err != nil {
			t.Fatalf("whoami on busy node: %v", err)
		}
		if r := a1.pull(t, 0, 50); r == nil {
			t.Fatal("pull on n1")
		}
		wg.Wait()
	})

	t.Run("audit of refusals is sampled", func(t *testing.T) {
		before := countAudit(t, e.srv, "agent.auth", outcomeDenied)
		bogus := &testAgent{id: "x", node: "n1", sn: e.s1, token: "mca1_bogus.x"} //nolint:gosec // a deliberately invalid token
		for range 20 {
			var who mycelv1.AgentWhoamiResponse
			err := bogus.call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{Call: bogus.ac()}, &who, 5*time.Second)
			wantPrefix(t, err, "unauthenticated: ")
		}
		if got := countAudit(t, e.srv, "agent.auth", outcomeDenied) - before; got != 1 {
			t.Fatalf("%d audit rows for 20 refusals", got)
		}
	})

	t.Run("watch cap", func(t *testing.T) {
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var streams []*connect.ServerStreamForClient[mycelv1.WatchResponse]
		for range maxWatchesPerUser {
			s, err := e.aliceCh.Watch(wctx, connect.NewRequest(&mycelv1.WatchRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			streams = append(streams, s)
		}
		eventually(t, 5*time.Second, "watches registered", func() bool { return e.srv.ch.count(e.srv.ch.userWatches, e.aliceID) == maxWatchesPerUser })
		s, err := e.aliceCh.Watch(wctx, connect.NewRequest(&mycelv1.WatchRequest{}))
		if err == nil {
			for s.Receive() {
			}
			err = s.Err()
		}
		wantCode(t, err, connect.CodeResourceExhausted)
		cancel()
		for _, s := range streams {
			_ = s.Close()
		}
	})
}

func countAudit(t *testing.T, srv *server, action, outcome string) int {
	t.Helper()
	entries, err := srv.st.listAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == action && e.Outcome == outcome {
			n++
		}
	}
	return n
}
