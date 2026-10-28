package hub

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/web"
)

// ---- helpers ----

// headers adds fixed headers to unary and streaming calls.
type headers map[string]string

func (h headers) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		for k, v := range h {
			req.Header().Set(k, v)
		}
		return next(ctx, req)
	}
}

func (h headers) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		for k, v := range h {
			conn.RequestHeader().Set(k, v)
		}
		return conn
	}
}

func (h headers) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func keyHeaders(key string) headers { return headers{"Authorization": "Bearer " + key} }

// scoped is a node certificate with the read scope header.
func scopedHeaders() headers { return headers{scopeHeader: scopeNetworkRead} }

func (e *grantEnv) events(h headers, cert *tls.Certificate) mycelv1connect.EventServiceClient {
	return mycelv1connect.NewEventServiceClient(e.h.httpClient(cert), e.h.ready.HubURL, connect.WithInterceptors(h))
}

// evStream collects a WatchEvents stream.
type evStream struct {
	mu     sync.Mutex
	evs    []*mycelv1.Event
	beats  int
	first  uint64 // seq of the opening heartbeat
	bad    bool   // the opening heartbeat was missing or malformed
	err    error
	done   chan struct{}
	cancel func()
}

func watch(t *testing.T, c mycelv1connect.EventServiceClient, req *mycelv1.WatchEventsRequest) *evStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.WatchEvents(ctx, connect.NewRequest(req))
	if err != nil {
		cancel()
		t.Fatalf("WatchEvents: %v", err)
	}
	s := &evStream{done: make(chan struct{}), cancel: cancel}
	t.Cleanup(func() { cancel(); <-s.done })
	go func() {
		defer close(s.done)
		for st.Receive() {
			s.mu.Lock()
			if ev := st.Msg().GetEvent(); ev != nil && ev.GetKind() == mycelv1.EventKind_EVENT_KIND_UNSPECIFIED {
				if s.first != 0 || len(s.evs) > 0 || ev.GetSeq() == 0 || ev.GetBody() != nil {
					s.bad = true
				}
				s.first = ev.GetSeq()
			} else if ev != nil {
				s.evs = append(s.evs, ev)
			} else {
				s.beats++
			}
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.err = st.Err()
		s.mu.Unlock()
		_ = st.Close()
	}()
	return s
}

func (s *evStream) snapshot() []*mycelv1.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.evs)
}

func (s *evStream) find(f func(*mycelv1.Event) bool) *mycelv1.Event {
	for _, ev := range s.snapshot() {
		if f(ev) {
			return ev
		}
	}
	return nil
}

func (s *evStream) wait(t *testing.T, what string, f func(*mycelv1.Event) bool) *mycelv1.Event {
	t.Helper()
	var got *mycelv1.Event
	eventually(t, 10*time.Second, what, func() bool { got = s.find(f); return got != nil })
	return got
}

func (s *evStream) waitErr(t *testing.T) error {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

var capsToggle = [][]string{{"docker", "gpu", "ollama", "sandbox"}, {"gpu", "ollama", "sandbox"}}

// marker changes gpu-box's caps (a node event every user may see) and
// returns the caps set.
func (e *grantEnv) marker(t *testing.T, i int) []string {
	t.Helper()
	caps := capsToggle[i%2]
	if _, err := e.adminCl.nodes.SetNodeCaps(context.Background(), connect.NewRequest(&mycelv1.SetNodeCapsRequest{
		Name: "gpu-box", CapsEnabled: &mycelv1.Capabilities{Names: caps},
	})); err != nil {
		t.Fatalf("SetNodeCaps: %v", err)
	}
	return caps
}

func isNodeCaps(caps []string) func(*mycelv1.Event) bool {
	return func(ev *mycelv1.Event) bool {
		return ev.GetKind() == kindNode && ev.GetId() == "gpu-box" &&
			slices.Equal(ev.GetNode().GetCapsEnabled().GetNames(), caps)
	}
}

// baseline opens an admin stream and returns the seq of a fresh marker, so
// later streams can replay from it without a gap.
func (e *grantEnv) baseline(t *testing.T) (*evStream, uint64) {
	t.Helper()
	admin := watch(t, e.events(keyHeaders(e.res.AdminAPIKey), nil), &mycelv1.WatchEventsRequest{})
	for i := 0; ; i++ {
		caps := e.marker(t, i)
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if ev := admin.find(isNodeCaps(caps)); ev != nil {
				return admin, ev.GetSeq()
			}
			time.Sleep(20 * time.Millisecond)
		}
		if i > 20 {
			t.Fatal("no baseline event")
		}
	}
}

func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// ---- events ----

func TestEventsVisibility(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	admin, base := e.baseline(t)
	alice := watch(t, e.events(keyHeaders(e.aliceKey), nil), &mycelv1.WatchEventsRequest{AfterSeq: base})
	bob := watch(t, e.events(keyHeaders(e.bobKey), nil), &mycelv1.WatchEventsRequest{AfterSeq: base})
	scope := watch(t, e.events(scopedHeaders(), &e.node.cert), &mycelv1.WatchEventsRequest{AfterSeq: base})
	onlyMsgs := watch(t, e.events(keyHeaders(e.aliceKey), nil), &mycelv1.WatchEventsRequest{
		AfterSeq: base, Kinds: []mycelv1.EventKind{kindMessage},
	})

	// Alice's private channel and a message in it.
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#alice-private", Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC,
		Policy: &mycelv1.ChannelPolicy{HumansMayPost: true},
	})); err != nil {
		t.Fatal(err)
	}
	post, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#alice-private", Text: "needle secret"}))
	if err != nil {
		t.Fatal(err)
	}
	// Alice's agent and a pending grant on her node.
	agents := mycelv1connect.NewAgentServiceClient(e.h.httpClient(nil), e.h.ready.HubURL, connect.WithInterceptors(keyHeaders(e.res.AdminAPIKey)))
	if _, err := agents.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: e.w.id, Budget: &mycelv1.Budget{Tokens: 1234}})); err != nil {
		t.Fatal(err)
	}
	g := mustAgentGrant(t, e.w, gpuSpec(0), "1h", "")
	if g.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatalf("grant %v", g.GetState())
	}
	// A direct message between alice and bob: not even the admin's stream
	// carries it.
	dm, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "@user:" + e.bobID, Text: "dm secret"}))
	if err != nil {
		t.Fatal(err)
	}
	caps := e.marker(t, 7)

	isChan := func(ev *mycelv1.Event) bool { return ev.GetKind() == kindChannel && ev.GetId() == "#alice-private" }
	isMsg := func(ev *mycelv1.Event) bool {
		return ev.GetKind() == kindMessage && ev.GetMessage().GetId() == post.Msg.GetMessage().GetId()
	}
	isAgent := func(ev *mycelv1.Event) bool {
		return ev.GetKind() == kindAgent && ev.GetAgent().GetBudget().GetTokens() == 1234
	}
	isGrant := func(ev *mycelv1.Event) bool { return ev.GetKind() == kindGrant && ev.GetId() == g.GetId() }
	for name, s := range map[string]*evStream{"admin": admin, "alice": alice, "bob": bob, "scope": scope} {
		s.wait(t, name+" marker", isNodeCaps(caps))
	}
	for name, s := range map[string]*evStream{"admin": admin, "alice": alice, "scope": scope} {
		for what, f := range map[string]func(*mycelv1.Event) bool{"channel": isChan, "message": isMsg, "agent": isAgent, "grant": isGrant} {
			if s.find(f) == nil {
				t.Errorf("%s did not see the %s event", name, what)
			}
		}
	}
	if ev := admin.find(isMsg); ev != nil && ev.GetMessage().GetPayload().GetText() != "needle secret" {
		t.Errorf("message body %v", ev.GetMessage())
	}
	for _, f := range []func(*mycelv1.Event) bool{isChan, isMsg, isGrant} {
		if ev := bob.find(f); ev != nil {
			t.Errorf("bob saw %v", ev)
		}
	}
	// Agents are visible to every member, as ListAgents shows them.
	if bob.find(isAgent) == nil {
		t.Error("bob did not see the agent event")
	}
	isDM := func(ev *mycelv1.Event) bool {
		return ev.GetKind() == kindMessage && ev.GetMessage().GetId() == dm.Msg.GetMessage().GetId()
	}
	if alice.find(isDM) == nil || bob.find(isDM) == nil {
		t.Error("a DM member did not see the DM")
	}
	if admin.find(isDM) != nil {
		t.Error("the admin's event stream carried a DM")
	}
	// Audit entries: admins see all, members only their own actors.
	if admin.find(func(ev *mycelv1.Event) bool {
		return ev.GetKind() == kindAudit && ev.GetAudit().GetActor() == e.aliceID && ev.GetAudit().GetAction() == "channel.post"
	}) == nil {
		t.Error("admin did not see alice's post audit entry")
	}
	for name, s := range map[string]*evStream{"alice": alice, "bob": bob, "scope": scope} {
		own := map[string]string{"alice": e.aliceID, "bob": e.bobID, "scope": e.aliceID}[name]
		for _, ev := range s.snapshot() {
			if a := ev.GetAudit(); a != nil && a.GetActor() != own && !strings.HasPrefix(a.GetActor(), "node:gpu-box") &&
				a.GetActor() != e.w.id {
				t.Errorf("%s saw audit entry of %s", name, a.GetActor())
			}
			if a := ev.GetAudit(); a != nil && name == "bob" && a.GetActor() != e.bobID {
				t.Errorf("bob saw audit entry of %s", a.GetActor())
			}
		}
	}
	// The kinds filter.
	onlyMsgs.wait(t, "filtered message", isMsg)
	for _, ev := range onlyMsgs.snapshot() {
		if ev.GetKind() != kindMessage {
			t.Errorf("kinds filter passed %v", ev.GetKind())
		}
	}
	// Four streams per node scope, not counted against alice's own (she has
	// two open already).
	for range maxEventStreamsPerScope - 1 {
		s := watch(t, e.events(scopedHeaders(), &e.node.cert), &mycelv1.WatchEventsRequest{})
		eventually(t, 10*time.Second, "stream open", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.first != 0 })
	}
	st, err := e.events(scopedHeaders(), &e.node.cert).WatchEvents(ctx, connect.NewRequest(&mycelv1.WatchEventsRequest{}))
	if err == nil {
		for st.Receive() {
		}
		err = st.Err()
	}
	wantCode(t, err, connect.CodeResourceExhausted)
	own := watch(t, e.events(keyHeaders(e.aliceKey), nil), &mycelv1.WatchEventsRequest{})
	eventually(t, 10*time.Second, "alice's third stream open", func() bool { own.mu.Lock(); defer own.mu.Unlock(); return own.first != 0 })
	// No user: Unauthenticated.
	st, err = e.events(nil, nil).WatchEvents(ctx, connect.NewRequest(&mycelv1.WatchEventsRequest{}))
	if err == nil {
		for st.Receive() {
		}
		err = st.Err()
	}
	wantCode(t, err, connect.CodeUnauthenticated)

	// Freeze events reach everyone.
	if _, err := e.adminC.FreezeChannel(ctx, connect.NewRequest(&mycelv1.FreezeChannelRequest{Channel: "#alice-private", Reason: "test"})); err != nil {
		t.Fatal(err)
	}
	bob.wait(t, "freeze event", func(ev *mycelv1.Event) bool {
		return ev.GetKind() == kindFreeze && ev.GetId() == "channel:#alice-private" &&
			slices.Contains(ev.GetFreeze().GetFrozenChannels(), "#alice-private")
	})
}

func TestEventsReplayAndTooOld(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	_, base := e.baseline(t)
	caps1 := e.marker(t, 1)
	caps2 := e.marker(t, 2)
	c := e.events(keyHeaders(e.bobKey), nil)
	replay := watch(t, c, &mycelv1.WatchEventsRequest{AfterSeq: base})
	first := replay.wait(t, "replayed marker", isNodeCaps(caps1))
	second := replay.wait(t, "second marker", func(ev *mycelv1.Event) bool {
		return isNodeCaps(caps2)(ev) && ev.GetSeq() > first.GetSeq()
	})
	if first.GetSeq() <= base || first.GetAt() == "" {
		t.Fatalf("seq %d at %q after base %d", first.GetSeq(), first.GetAt(), base)
	}
	replay.mu.Lock()
	openSeq, bad := replay.first, replay.bad
	replay.mu.Unlock()
	if bad || openSeq < base {
		t.Fatalf("opening heartbeat seq %d (bad %v), base %d", openSeq, bad, base)
	}
	if replay.find(func(ev *mycelv1.Event) bool { return ev.GetSeq() <= base }) != nil {
		t.Fatal("replay included events at or before after_seq")
	}
	for _, after := range []uint64{1, second.GetSeq() + 1000000} {
		s := watch(t, c, &mycelv1.WatchEventsRequest{AfterSeq: after})
		err := s.waitErr(t)
		wantCode(t, err, connect.CodeFailedPrecondition)
		if !strings.Contains(err.Error(), "too old") {
			t.Fatalf("err %v", err)
		}
	}
	_, err := c.Search(ctx, connect.NewRequest(&mycelv1.SearchRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestEventRing(t *testing.T) {
	h := newEventHub()
	now := time.Now()
	h.now = func() time.Time { return now }
	start, _ := h.start(0)
	mk := func(n int) []ringEntry {
		out := make([]ringEntry, n)
		for i := range out {
			out[i] = ringEntry{ev: &mycelv1.Event{Kind: kindNode, Id: "n"}}
		}
		return out
	}
	h.append(mk(3), false)
	evs, err := h.after(start, 100)
	if err != nil || len(evs) != 3 || evs[0].ev.GetSeq() != start+1 {
		t.Fatalf("after: %v %v", evs, err)
	}
	h.append(mk(eventRingMax), false)
	if _, err := h.start(start); err == nil {
		t.Fatal("cursor beyond the ring accepted")
	}
	cur, err := h.start(start + 3)
	if err != nil {
		t.Fatalf("oldest kept cursor refused: %v", err)
	}
	if evs, _ := h.after(cur, 5); len(evs) != 5 || evs[0].ev.GetSeq() != start+4 {
		t.Fatalf("after oldest: %v", evs)
	}
	// Age: an hour later only the new event stays.
	now = now.Add(eventRingAge + time.Minute)
	h.append(mk(1), false)
	if _, err := h.start(cur); err == nil {
		t.Fatal("expired cursor accepted")
	}
	last := h.seq
	if evs, err := h.after(last-1, 10); err != nil || len(evs) != 1 {
		t.Fatalf("after age prune: %v %v", evs, err)
	}
	// Overflow: open cursors become too old.
	for range eventQueueMax + 1 {
		h.emit(change{kind: kindNode, op: opUpdated, id: "n"})
	}
	_, reset := h.take()
	if !reset {
		t.Fatal("no overflow")
	}
	h.append(nil, true)
	if _, err := h.after(last, 10); err == nil {
		t.Fatal("cursor survived an overflow")
	}
	// Byte budgets: the ring keeps what fits, the queue resets.
	setVar(t, &eventRingBytes, 20*changeOverhead)
	h.append(mk(50), false)
	h.mu.Lock()
	kept, bytes, seq := len(h.ring), h.ringBytes, h.seq
	h.mu.Unlock()
	if kept == 0 || kept >= 50 || bytes > eventRingBytes {
		t.Fatalf("ring after byte trim: %d events, %d bytes", kept, bytes)
	}
	if _, err := h.after(seq-uint64(kept)-1, 10); err == nil {
		t.Fatal("trimmed cursor accepted")
	}
	setVar(t, &eventQueueBytes, 4096)
	h.emit(change{kind: kindMessage, id: "m", body: &mycelv1.Message{Payload: &mycelv1.Payload{Text: strings.Repeat("x", 8192)}}})
	if _, reset := h.take(); !reset {
		t.Fatal("a change past the queue budget did not reset")
	}
	// Streams: per user, and per node scope without touching the user's.
	for range maxEventStreamsPerScope {
		if !h.acquire("u", "n1") {
			t.Fatal("scope limit too low")
		}
	}
	if h.acquire("u", "n1") {
		t.Fatal("scope limit not enforced")
	}
	for range maxEventStreamsPerUser {
		if !h.acquire("u", "") {
			t.Fatal("user limit counted scope streams")
		}
	}
	if h.acquire("u", "") {
		t.Fatal("user limit not enforced")
	}
	h.release("u", "n1")
	if !h.acquire("u3", "n1") {
		t.Fatal("scope not released")
	}
}

func TestEventsHeartbeat(t *testing.T) {
	setVar(t, &eventHeartbeat, 150*time.Millisecond)
	e := newGrantEnv(t)
	s := watch(t, e.events(keyHeaders(e.bobKey), nil), &mycelv1.WatchEventsRequest{})
	eventually(t, 5*time.Second, "two heartbeats", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.beats >= 2
	})
}

// ---- search ----

func TestSearch(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#alpha-secret", Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC, Policy: &mycelv1.ChannelPolicy{HumansMayPost: true},
	})); err != nil {
		t.Fatal(err)
	}
	for _, txt := range []string{"a NEEDLE in the haystack", "another needle", "nothing here"} {
		if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#alpha-secret", Text: txt})); err != nil {
			t.Fatal(err)
		}
	}
	g := mustAgentGrant(t, e.w, gpuSpec(0), "1h", "")
	search := func(key string, req *mycelv1.SearchRequest) []*mycelv1.SearchHit {
		t.Helper()
		resp, err := e.events(keyHeaders(key), nil).Search(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		return resp.Msg.GetHits()
	}
	count := func(hits []*mycelv1.SearchHit, k mycelv1.EventKind) int {
		n := 0
		for _, h := range hits {
			if h.GetKind() == k {
				n++
			}
		}
		return n
	}
	alice := search(e.aliceKey, &mycelv1.SearchRequest{Query: "needle"})
	if count(alice, kindMessage) != 2 || alice[0].GetChannel() != "#alpha-secret" || !strings.Contains(strings.ToLower(alice[0].GetSnippet()), "needle") {
		t.Fatalf("alice: %v", alice)
	}
	if bob := search(e.bobKey, &mycelv1.SearchRequest{Query: "needle"}); len(bob) != 0 {
		t.Fatalf("bob sees %v", bob)
	}
	if admin := search(e.res.AdminAPIKey, &mycelv1.SearchRequest{Query: "NEEDLE"}); count(admin, kindMessage) != 2 {
		t.Fatalf("admin: %v", admin)
	}
	// Channels by name; nodes are visible to everyone.
	if hits := search(e.bobKey, &mycelv1.SearchRequest{Query: "alpha"}); len(hits) != 0 {
		t.Fatalf("bob sees the private channel: %v", hits)
	}
	if hits := search(e.aliceKey, &mycelv1.SearchRequest{Query: "alpha", Kinds: []mycelv1.EventKind{kindChannel}}); len(hits) != 1 || hits[0].GetTitle() != "#alpha-secret" {
		t.Fatalf("alice channels: %v", hits)
	}
	if hits := search(e.bobKey, &mycelv1.SearchRequest{Query: "gpu-b", Kinds: []mycelv1.EventKind{kindNode}}); len(hits) != 1 || hits[0].GetId() != "gpu-box" {
		t.Fatalf("bob nodes: %v", hits)
	}
	// Grants follow Get (the owner, not other members); agents are listed
	// for every member, as ListAgents does.
	if hits := search(e.aliceKey, &mycelv1.SearchRequest{Query: "gpu", Kinds: []mycelv1.EventKind{kindGrant, kindAgent}}); count(hits, kindGrant) != 1 || count(hits, kindAgent) != 1 || hits[1].GetId() != g.GetId() && hits[0].GetId() != g.GetId() {
		t.Fatalf("alice grants: %v", hits)
	}
	if hits := search(e.bobKey, &mycelv1.SearchRequest{Query: "gpu", Kinds: []mycelv1.EventKind{kindGrant, kindAgent}}); count(hits, kindGrant) != 0 || count(hits, kindAgent) != 1 {
		t.Fatalf("bob grants: %v", hits)
	}
	// Limits.
	if hits := search(e.aliceKey, &mycelv1.SearchRequest{Query: "e", Limit: 1}); len(hits) != 1 {
		t.Fatalf("limit 1: %v", hits)
	}
	c := e.events(keyHeaders(e.aliceKey), nil)
	for _, req := range []*mycelv1.SearchRequest{
		{Query: "  "}, {Query: strings.Repeat("x", 201)}, {Query: "x", Kinds: []mycelv1.EventKind{kindAudit}},
		{Query: "x", Kinds: []mycelv1.EventKind{99}},
	} {
		_, err := c.Search(ctx, connect.NewRequest(req))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	// LIKE wildcards are literal.
	if hits := search(e.aliceKey, &mycelv1.SearchRequest{Query: "%"}); len(hits) != 0 {
		t.Fatalf("wildcard matched: %v", hits)
	}
}

// ---- audit ----

func TestListAudit(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	post, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#general", Text: "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	list := func(key string, req *mycelv1.ListAuditRequest) *mycelv1.ListAuditResponse {
		t.Helper()
		resp, err := e.events(keyHeaders(key), nil).ListAudit(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		return resp.Msg
	}
	all := list(e.res.AdminAPIKey, &mycelv1.ListAuditRequest{Limit: 1000})
	actors := map[string]bool{}
	for _, en := range all.GetEntries() {
		actors[en.GetActor()] = true
	}
	if !actors[e.aliceID] || !actors["node:gpu-box"] || len(all.GetEntries()) < 3 {
		t.Fatalf("admin sees %v", actors)
	}
	for i := 1; i < len(all.GetEntries()); i++ {
		if all.GetEntries()[i-1].GetAt() < all.GetEntries()[i].GetAt() {
			t.Fatal("not newest first")
		}
	}
	// Members: their own actors only (alice owns gpu-box and its agent).
	for _, en := range list(e.aliceKey, &mycelv1.ListAuditRequest{Limit: 1000}).GetEntries() {
		if a := en.GetActor(); a != e.aliceID && a != "node:gpu-box" && a != e.w.id {
			t.Fatalf("alice sees %s", a)
		}
	}
	bob := list(e.bobKey, &mycelv1.ListAuditRequest{Limit: 1000}).GetEntries()
	for _, en := range bob {
		if en.GetActor() != e.bobID {
			t.Fatalf("bob sees %s", en.GetActor())
		}
	}
	if got := list(e.bobKey, &mycelv1.ListAuditRequest{Actor: e.aliceID}); len(got.GetEntries()) != 0 {
		t.Fatalf("bob filtered by alice: %v", got)
	}
	// Filters.
	trace := post.Msg.GetMessage().GetMeta().GetTraceId()
	tr := list(e.aliceKey, &mycelv1.ListAuditRequest{TraceId: trace})
	if len(tr.GetEntries()) != 1 || tr.GetEntries()[0].GetAction() != "channel.post" || tr.GetEntries()[0].GetTraceId() != trace ||
		tr.GetEntries()[0].GetOutcome() != "allowed" || !strings.Contains(tr.GetEntries()[0].GetDetailsJson(), post.Msg.GetMessage().GetId()) {
		t.Fatalf("trace filter: %v", tr)
	}
	if r := list(e.res.AdminAPIKey, &mycelv1.ListAuditRequest{Resource: "channel:#general", Actor: e.aliceID}); len(r.GetEntries()) != 1 {
		t.Fatalf("resource filter: %v", r)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if r := list(e.res.AdminAPIKey, &mycelv1.ListAuditRequest{Since: future}); len(r.GetEntries()) != 0 {
		t.Fatalf("since: %v", r)
	}
	if r := list(e.res.AdminAPIKey, &mycelv1.ListAuditRequest{Until: "2000-01-01T00:00:00Z"}); len(r.GetEntries()) != 0 {
		t.Fatalf("until: %v", r)
	}
	// Paging.
	p1 := list(e.res.AdminAPIKey, &mycelv1.ListAuditRequest{Limit: 2})
	if len(p1.GetEntries()) != 2 || p1.GetNextPageToken() == "" {
		t.Fatalf("page 1: %v", p1)
	}
	p2 := list(e.res.AdminAPIKey, &mycelv1.ListAuditRequest{Limit: 2, PageToken: p1.GetNextPageToken()})
	if len(p2.GetEntries()) == 0 || p2.GetEntries()[0].GetId() == p1.GetEntries()[1].GetId() ||
		p2.GetEntries()[0].GetId() != all.GetEntries()[2].GetId() {
		t.Fatalf("page 2: %v", p2)
	}
	c := e.events(keyHeaders(e.aliceKey), nil)
	for _, req := range []*mycelv1.ListAuditRequest{{Since: "yesterday"}, {PageToken: "x"}, {Actor: strings.Repeat("a", 600)}} {
		_, err := c.ListAudit(ctx, connect.NewRequest(req))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
}

// ---- sessions and CSRF ----

// jsonDo sends a JSON request from the browser b with extra headers.
func (b *browser) jsonDo(method, path string, body any, hdr map[string]string) (int, []byte, http.Header) {
	b.t.Helper()
	var r io.Reader
	switch v := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			b.t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.base+path, r)
	if err != nil {
		b.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

type meJSON struct {
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	CSRF     string `json:"csrf"`
	Passkeys []struct {
		ID, Name string
	} `json:"passkeys"`
}

func (b *browser) me() (int, meJSON) {
	b.t.Helper()
	code, data, hdr := b.jsonDo("GET", "/api/auth/me", nil, nil)
	var m meJSON
	if code == http.StatusOK {
		if err := json.Unmarshal(data, &m); err != nil {
			b.t.Fatalf("me: %v %s", err, data)
		}
		if hdr.Get("Cache-Control") != "no-store" {
			b.t.Fatalf("me cache-control %q", hdr.Get("Cache-Control"))
		}
	}
	return code, m
}

func (b *browser) users(h headers) mycelv1connect.UserServiceClient {
	return mycelv1connect.NewUserServiceClient(b.c, b.base, connect.WithInterceptors(h))
}

func TestSessionCSRF(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	b := e.browser(t)
	if code, _ := b.me(); code != http.StatusUnauthorized {
		t.Fatalf("me before login: %d", code)
	}
	b.login(e.aliceKey)
	code, me := b.me()
	if code != http.StatusOK || me.UserID != e.aliceID || me.Role != roleMember || me.CSRF == "" || me.Passkeys == nil {
		t.Fatalf("me: %d %+v", code, me)
	}
	// A cookie without the header: unauthenticated.
	_, err := b.users(nil).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	// Wrong token, or a cross-site origin: unauthenticated.
	_, err = b.users(headers{csrfHeader: "nope"}).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = b.users(headers{csrfHeader: me.CSRF, "Origin": "https://evil.example"}).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = b.users(headers{csrfHeader: me.CSRF, "Sec-Fetch-Site": "cross-site"}).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	// With the header the Connect API sees the user as with an API key.
	resp, err := b.users(headers{csrfHeader: me.CSRF, "Origin": e.h.ready.HubURL}).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	if err != nil || resp.Msg.GetUser().GetId() != e.aliceID {
		t.Fatalf("whoami: %v %v", resp, err)
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("connect cache-control %q", got)
	}
	// A cross-site GET of /api/auth/me does not see the session.
	if code, _, _ := b.jsonDo("GET", "/api/auth/me", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusUnauthorized {
		t.Fatalf("cross-site me: %d", code)
	}
	// Logout needs the token.
	if code, _, _ := b.jsonDo("POST", "/api/auth/logout", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("logout without token: %d", code)
	}
	if code, _, _ := b.jsonDo("POST", "/api/auth/logout", nil, map[string]string{csrfHeader: me.CSRF}); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := b.me(); code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", code)
	}
	// The no-JavaScript page still works with the shared session.
	b.login(e.bobKey)
	if page, _ := b.approvals(); !strings.Contains(page, "bob") {
		t.Fatal("approvals page after login")
	}
}

func TestUIConfigAndSPA(t *testing.T) {
	e := newGrantEnv(t)
	b := e.browser(t)
	code, data, hdr := b.jsonDo("GET", "/api/ui-config", nil, nil)
	var cfg map[string]any
	if code != http.StatusOK || json.Unmarshal(data, &cfg) != nil || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("ui-config: %d %s", code, data)
	}
	if cfg["mode"] != "hub" || cfg["hub"] != "https://localhost:7777" || cfg["rp_id"] != "localhost" || cfg["passkeys"] != true || cfg["push"] != true {
		t.Fatalf("ui-config: %v", cfg)
	}
	if code, data, _ := b.jsonDo("GET", "/api/nope", nil, nil); code != http.StatusNotFound || !strings.Contains(string(data), "not found") {
		t.Fatalf("unknown api path: %d %s", code, data)
	}
	code, data, hdr = b.jsonDo("GET", "/some/client/route", nil, nil)
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("spa headers: %v", hdr)
	}
	if web.Built() {
		if code != http.StatusOK {
			t.Fatalf("spa: %d", code)
		}
	} else if code != http.StatusNotFound || !strings.Contains(string(data), "not built") {
		t.Fatalf("spa without build: %d %s", code, data)
	}
	for _, p := range []string{"/healthz", "/ca.pem"} {
		if code, _, _ := b.jsonDo("GET", p, nil, nil); code != http.StatusOK {
			t.Fatalf("%s: %d", p, code)
		}
	}
}

// ---- node read scope ----

// allProcedures lists every Connect procedure of package mycel.v1.
func allProcedures(t *testing.T) map[string]protoreflect.MethodDescriptor {
	t.Helper()
	out := map[string]protoreflect.MethodDescriptor{}
	protoregistry.GlobalFiles.RangeFilesByPackage("mycel.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				out["/"+string(sd.FullName())+"/"+string(md.Name())] = md
			}
		}
		return true
	})
	if len(out) < 50 {
		t.Fatalf("only %d procedures found", len(out))
	}
	return out
}

func TestNodeReadScope(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	hc := e.h.httpClient(&e.node.cert)
	// Every procedure outside the read-only list is refused.
	procs := allProcedures(t)
	for p, md := range procs {
		if readOnlyMethods[p] {
			continue
		}
		var req *http.Request
		if md.IsStreamingServer() {
			env := append([]byte{0, 0, 0, 0, 2}, []byte("{}")...)
			req, _ = http.NewRequest("POST", e.h.ready.HubURL+p, bytes.NewReader(env))
			req.Header.Set("Content-Type", "application/connect+json")
		} else {
			req, _ = http.NewRequest("POST", e.h.ready.HubURL+p, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set(scopeHeader, scopeNetworkRead)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "permission_denied") || !strings.Contains(string(body), "node read scope") {
			t.Errorf("%s under the read scope: %d %s", p, resp.StatusCode, body)
		}
	}
	// Reads work as alice, a member.
	nodes := mycelv1connect.NewNodeServiceClient(hc, e.h.ready.HubURL, connect.WithInterceptors(scopedHeaders()))
	if _, err := nodes.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{})); err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	users := mycelv1connect.NewUserServiceClient(hc, e.h.ready.HubURL, connect.WithInterceptors(scopedHeaders()))
	who, err := users.Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	if err != nil || who.Msg.GetUser().GetId() != e.aliceID {
		t.Fatalf("whoami: %v %v", who, err)
	}
	grants := mycelv1connect.NewGrantServiceClient(hc, e.h.ready.HubURL, connect.WithInterceptors(scopedHeaders()))
	if _, err := grants.ListGrants(ctx, connect.NewRequest(&mycelv1.ListGrantsRequest{})); err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	_, err = grants.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: "gr_x"}))
	wantCode(t, err, connect.CodePermissionDenied)
	// An admin's node gets member visibility under the scope.
	adminNode := enrol(t, e.anonCl, createToken(t, e.adminCl, "admin-box", nil, ""), "admin-box")
	aw, err := mycelv1connect.NewUserServiceClient(e.h.httpClient(&adminNode.cert), e.h.ready.HubURL, connect.WithInterceptors(scopedHeaders())).
		Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	if err != nil || aw.Msg.GetUser().GetRole() != mycelv1.Role_ROLE_MEMBER {
		t.Fatalf("admin node scope: %v %v", aw, err)
	}
	// Without the header a node certificate has no user rights.
	plain := mycelv1connect.NewNodeServiceClient(hc, e.h.ready.HubURL)
	_, err = plain.ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = mycelv1connect.NewNodeServiceClient(hc, e.h.ready.HubURL, connect.WithInterceptors(headers{scopeHeader: "network:write"})).
		ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	// The header without a node certificate means nothing.
	_, err = mycelv1connect.NewNodeServiceClient(e.h.httpClient(nil), e.h.ready.HubURL, connect.WithInterceptors(scopedHeaders())).
		ListNodes(ctx, connect.NewRequest(&mycelv1.ListNodesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	// HTTP routes: only artifact downloads and the UI config.
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/auth/me", http.StatusForbidden},
		{"POST", "/api/push/subscriptions", http.StatusForbidden},
		{"PUT", "/api/artifacts?name=x", http.StatusForbidden},
		{"POST", "/api/auth/passkey/login/begin", http.StatusForbidden},
		{"GET", "/api/artifacts/art_missing", 0}, // reaches the handler (not 403)
		{"GET", "/api/ui-config", http.StatusOK},
	} {
		req, _ := http.NewRequest(tc.method, e.h.ready.HubURL+tc.path, strings.NewReader("{}"))
		req.Header.Set(scopeHeader, scopeNetworkRead)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want && (tc.want != 0 || resp.StatusCode == http.StatusForbidden) {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
	// Denials are audited.
	resp, err := e.events(keyHeaders(e.res.AdminAPIKey), nil).ListAudit(ctx, connect.NewRequest(&mycelv1.ListAuditRequest{Actor: "node:gpu-box", Limit: 1000}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, en := range resp.Msg.GetEntries() {
		found = found || (en.GetAction() == "scope.network_read" && en.GetOutcome() == outcomeDenied)
	}
	if !found {
		t.Fatal("no audited scope denial")
	}
}

func TestReadOnlyListMatchesDaemon(t *testing.T) {
	var hub []string
	for p := range readOnlyMethods {
		hub = append(hub, p)
	}
	d := slices.Clone(daemon.HubReadOnlyProcedures)
	slices.Sort(hub)
	slices.Sort(d)
	if !slices.Equal(hub, d) {
		t.Fatalf("hub %v\ndaemon %v", hub, d)
	}
	procs := allProcedures(t)
	for _, p := range hub {
		if procs[p] == nil {
			t.Errorf("unknown procedure %s", p)
		}
	}
}

// ---- passkeys ----

// vAuth is a minimal software authenticator: ES256, "none" attestation.
type vAuth struct {
	key    *ecdsa.PrivateKey
	id     []byte
	count  uint32
	rpID   string
	origin string
}

func newVAuth(t *testing.T, rpID, origin string) *vAuth {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &vAuth{key: k, id: id, rpID: rpID, origin: origin}
}

var b64 = base64.RawURLEncoding.EncodeToString

func challengeOf(t *testing.T, opts []byte) (string, string) {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(opts, &o); err != nil || o.PublicKey.Challenge == "" {
		t.Fatalf("options %s: %v", opts, err)
	}
	return o.PublicKey.Challenge, o.PublicKey.User.ID
}

func (a *vAuth) authData(flags byte, extra []byte) []byte {
	h := sha256.Sum256([]byte(a.rpID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.count)
	return append(out, extra...)
}

func (a *vAuth) create(t *testing.T, opts []byte, origin string) []byte {
	t.Helper()
	chal, _ := challengeOf(t, opts)
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": chal, "origin": origin, "crossOrigin": false})
	x, y := make([]byte, 32), make([]byte, 32)
	a.key.X.FillBytes(x)
	a.key.Y.FillBytes(y)
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: 2, Algorithm: -7}, Curve: 1, XCoord: x, YCoord: y,
	})
	if err != nil {
		t.Fatal(err)
	}
	att := make([]byte, 16)                                     // AAGUID
	att = binary.BigEndian.AppendUint16(att, uint16(len(a.id))) //nolint:gosec // 16-byte id
	att = append(att, a.id...)
	att = append(att, cose...)
	obj, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(0x45, att)})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64(a.id), "rawId": b64(a.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response":               map[string]any{"clientDataJSON": b64(cdj), "attestationObject": b64(obj), "transports": []string{"internal"}},
		"clientExtensionResults": map[string]any{},
	})
	return out
}

func (a *vAuth) get(t *testing.T, opts []byte, userHandle string) []byte {
	t.Helper()
	chal, _ := challengeOf(t, opts)
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": chal, "origin": a.origin, "crossOrigin": false})
	ad := a.authData(0x05, nil)
	ch := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(slices.Clone(ad), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64(a.id), "rawId": b64(a.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64(cdj), "authenticatorData": b64(ad), "signature": b64(sig), "userHandle": userHandle,
		},
		"clientExtensionResults": map[string]any{},
	})
	return out
}

func TestPasskeys(t *testing.T) {
	var srv *server
	setVar(t, &testHookServer, func(s *server) { srv = s })
	e := newGrantEnv(t)
	const origin = "https://localhost:7777"
	b := e.browser(t)
	b.login(e.aliceKey)
	_, me := b.me()
	csrf := map[string]string{csrfHeader: me.CSRF}

	// Registration needs a session with the CSRF token.
	if code, _, _ := e.browser(t).jsonDo("POST", "/api/auth/passkey/register/begin", map[string]string{"name": "x"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("begin without session: %d", code)
	}
	code, opts, _ := b.jsonDo("POST", "/api/auth/passkey/register/begin", map[string]string{"name": "laptop"}, csrf)
	if code != http.StatusOK || !strings.Contains(string(opts), `"residentKey":"required"`) || !strings.Contains(string(opts), `"rp":{"name":"Mycelium","id":"localhost"}`) {
		t.Fatalf("register begin: %d %s", code, opts)
	}
	_, userHandle := challengeOf(t, opts)
	if userHandle != b64([]byte(e.aliceID)) {
		t.Fatalf("user handle %q", userHandle)
	}
	a := newVAuth(t, "localhost", origin)
	// A response from another origin is rejected, and the challenge is spent.
	if code, data, _ := b.jsonDo("POST", "/api/auth/passkey/register/finish", a.create(t, opts, "https://evil.example"), csrf); code != http.StatusBadRequest {
		t.Fatalf("bad origin: %d %s", code, data)
	}
	if code, data, _ := b.jsonDo("POST", "/api/auth/passkey/register/finish", a.create(t, opts, origin), csrf); code != http.StatusBadRequest ||
		!strings.Contains(string(data), "no passkey registration in progress") {
		t.Fatalf("reused challenge: %d %s", code, data)
	}
	_, opts, _ = b.jsonDo("POST", "/api/auth/passkey/register/begin", map[string]string{"name": "laptop"}, csrf)
	code, data, _ := b.jsonDo("POST", "/api/auth/passkey/register/finish", a.create(t, opts, origin), csrf)
	if code != http.StatusCreated || !strings.Contains(string(data), b64(a.id)) {
		t.Fatalf("register finish: %d %s", code, data)
	}
	if _, me = b.me(); len(me.Passkeys) != 1 || me.Passkeys[0].Name != "laptop" {
		t.Fatalf("me passkeys: %+v", me.Passkeys)
	}

	// Discoverable login from a fresh browser.
	nb := e.browser(t)
	code, lopts, _ := nb.jsonDo("POST", "/api/auth/passkey/login/begin", map[string]string{}, nil)
	if code != http.StatusOK || !strings.Contains(string(lopts), `"rpId":"localhost"`) || strings.Contains(string(lopts), "allowCredentials") {
		t.Fatalf("login begin: %d %s", code, lopts)
	}
	a.count = 1
	code, data, _ = nb.jsonDo("POST", "/api/auth/passkey/login/finish", a.get(t, lopts, b64([]byte(e.aliceID))), nil)
	var lr struct {
		UserID string `json:"user_id"`
		CSRF   string `json:"csrf"`
	}
	if code != http.StatusOK || json.Unmarshal(data, &lr) != nil || lr.UserID != e.aliceID || lr.CSRF == "" {
		t.Fatalf("login finish: %d %s", code, data)
	}
	if code, m := nb.me(); code != http.StatusOK || m.UserID != e.aliceID || m.CSRF != lr.CSRF {
		t.Fatalf("me after passkey login: %d %+v", code, m)
	}
	// The same signature counter again: refused (possible clone).
	_, lopts, _ = nb.jsonDo("POST", "/api/auth/passkey/login/begin", nil, nil)
	if code, data, _ := nb.jsonDo("POST", "/api/auth/passkey/login/finish", a.get(t, lopts, b64([]byte(e.aliceID))), nil); code != http.StatusUnauthorized {
		t.Fatalf("replayed counter: %d %s", code, data)
	}
	// A wrong user handle: refused.
	_, lopts, _ = nb.jsonDo("POST", "/api/auth/passkey/login/begin", nil, nil)
	a.count = 5
	if code, _, _ := nb.jsonDo("POST", "/api/auth/passkey/login/finish", a.get(t, lopts, b64([]byte(e.bobID))), nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong user handle: %d", code)
	}
	// Expired challenge.
	_, lopts, _ = nb.jsonDo("POST", "/api/auth/passkey/login/begin", nil, nil)
	srv.passkeys.mu.Lock()
	srv.passkeys.now = func() time.Time { return time.Now().Add(passkeyChallengeTTL + time.Second) }
	srv.passkeys.mu.Unlock()
	a.count = 6
	if code, data, _ := nb.jsonDo("POST", "/api/auth/passkey/login/finish", a.get(t, lopts, b64([]byte(e.aliceID))), nil); code != http.StatusBadRequest ||
		!strings.Contains(string(data), "no passkey sign-in in progress") {
		t.Fatalf("expired challenge: %d %s", code, data)
	}
	srv.passkeys.mu.Lock()
	srv.passkeys.now = time.Now
	srv.passkeys.mu.Unlock()

	// Audit records registration and login, never key material.
	resp, err := e.events(keyHeaders(e.res.AdminAPIKey), nil).ListAudit(context.Background(), connect.NewRequest(&mycelv1.ListAuditRequest{Actor: e.aliceID, Limit: 1000}))
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, en := range resp.Msg.GetEntries() {
		actions[en.GetAction()+"/"+en.GetOutcome()]++
		if strings.Contains(en.GetDetailsJson(), me.CSRF) || strings.Contains(en.GetDetailsJson(), "publicKey") {
			t.Fatalf("secret in audit: %s", en.GetDetailsJson())
		}
	}
	if actions["passkey.register/allowed"] != 1 || actions["passkey.login/allowed"] != 1 || actions["passkey.login/denied"] < 1 {
		t.Fatalf("audit actions %v", actions)
	}

	// Delete: alice keeps her API key, so the last passkey may go.
	if code, _, _ := b.jsonDo("DELETE", "/api/auth/passkey/"+b64(a.id), nil, csrf); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _, _ := b.jsonDo("DELETE", "/api/auth/passkey/"+b64(a.id), nil, csrf); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
}

func TestPasskeyLastCredential(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	u, _, _, err := st.createUser(ctx, "carol", roleMember)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pk1", "pk2"} {
		if err := st.insertPasskey(ctx, &passkeyRow{ID: id, UserID: u.ID, Name: id, Credential: "{}", CreatedAt: nowString()}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.insertPasskey(ctx, &passkeyRow{ID: "pk3", UserID: u.ID, Name: "x", Credential: "{}", CreatedAt: nowString()}, 2); err != errTooMany {
		t.Fatalf("third passkey: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.deletePasskey(ctx, u.ID, "pk1"); err != nil {
		t.Fatalf("delete one of two: %v", err)
	}
	if err := st.deletePasskey(ctx, u.ID, "pk2"); err != errLastCredential {
		t.Fatalf("delete last without api key: %v", err)
	}
	if err := st.deletePasskey(ctx, "usr_other", "pk2"); err != errNotFound {
		t.Fatalf("delete someone else's: %v", err)
	}
}

func TestPasskeyLoginRateLimit(t *testing.T) {
	e := newGrantEnv(t)
	b := e.browser(t)
	limited := false
	for range uiLoginBurst + 2 {
		code, _, _ := b.jsonDo("POST", "/api/auth/passkey/login/begin", nil, nil)
		if code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("passkey login begin is not rate limited")
	}
}

func TestPasskeysDisabledForIPHosts(t *testing.T) {
	cfg := resolveUISettings(&Config{AdvertiseURL: "https://10.0.0.5:7777"}, "")
	if _, err := newPasskeyState(cfg); err == nil {
		t.Fatal("passkeys enabled for an IP address")
	}
	cfg = resolveUISettings(&Config{AdvertiseURL: "https://10.0.0.5:7777", UI: UIConfig{
		ExternalURL: "https://hub.example.com/", Origins: []string{"https://hub.example.com", "https://alt.example.com/"},
	}}, "")
	if cfg.rpID != "hub.example.com" || cfg.externalURL != "https://hub.example.com" || len(cfg.origins) != 2 || cfg.origins[1] != "https://alt.example.com" {
		t.Fatalf("settings %+v", cfg)
	}
	if _, err := newPasskeyState(cfg); err != nil {
		t.Fatal(err)
	}
}

// ---- push ----

type pushRecorder struct {
	mu   sync.Mutex
	hits map[string]int
	gone string
}

func (p *pushRecorder) Do(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") || r.Header.Get("Content-Encoding") != "aes128gcm" {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	p.hits[r.URL.String()]++
	code := http.StatusCreated
	if r.URL.String() == p.gone {
		code = http.StatusGone
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (p *pushRecorder) count(u string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[u]
}

// subscription makes a PushSubscription JSON with real keys.
func subscription(t *testing.T, endpoint string) map[string]any {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return map[string]any{"endpoint": endpoint, "keys": map[string]string{"p256dh": b64(pub.Bytes()), "auth": b64(auth)}}
}

func TestPush(t *testing.T) {
	rec := &pushRecorder{hits: map[string]int{}, gone: "https://push.example.com/admin-gone"}
	setVar[webpush.HTTPClient](t, &pushHTTPClient, rec)
	e := newGrantEnv(t)
	login := func(key string) (*browser, map[string]string) {
		b := e.browser(t)
		b.login(key)
		_, me := b.me()
		return b, map[string]string{csrfHeader: me.CSRF}
	}
	admin, adminCSRF := login(e.res.AdminAPIKey)
	alice, aliceCSRF := login(e.aliceKey)
	bob, bobCSRF := login(e.bobKey)

	if code, _, _ := e.browser(t).jsonDo("GET", "/api/push/vapid", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("vapid without session: %d", code)
	}
	code, data, _ := alice.jsonDo("GET", "/api/push/vapid", nil, nil)
	var vk struct {
		PublicKey string `json:"public_key"`
	}
	if code != http.StatusOK || json.Unmarshal(data, &vk) != nil || len(vk.PublicKey) < 80 {
		t.Fatalf("vapid: %d %s", code, data)
	}
	fi, err := os.Stat(filepath.Join(e.opts.HubDir, vapidFile))
	if err != nil || (fi.Mode().Perm() != 0o600 && runtime.GOOS != "windows") {
		t.Fatalf("vapid file: %v %v", fi, err)
	}
	if _, data, _ := bob.jsonDo("GET", "/api/push/vapid", nil, nil); !strings.Contains(string(data), vk.PublicKey) {
		t.Fatal("vapid key changed")
	}

	// Validation.
	for _, bad := range []map[string]any{
		subscription(t, "http://push.example.com/x"), subscription(t, "https://127.0.0.1/x"),
		subscription(t, "https://localhost/x"), subscription(t, "https://push.example.com:8443/x"),
		{"endpoint": "https://push.example.com/x", "keys": map[string]string{"p256dh": "!!", "auth": "x"}},
	} {
		if code, data, _ := alice.jsonDo("POST", "/api/push/subscriptions", bad, aliceCSRF); code != http.StatusBadRequest {
			t.Fatalf("bad subscription %v: %d %s", bad, code, data)
		}
	}
	// At most 8 per user: the oldest goes.
	for i := range maxPushSubsPerUser + 1 {
		ep := "https://push.example.com/alice-" + string(rune('a'+i))
		if code, data, _ := alice.jsonDo("POST", "/api/push/subscriptions", subscription(t, ep), aliceCSRF); code != http.StatusCreated {
			t.Fatalf("subscribe %d: %d %s", i, code, data)
		}
	}
	st := e.store(t)
	if n, err := st.countPushSubs(context.Background(), e.aliceID); err != nil || n != maxPushSubsPerUser {
		t.Fatalf("alice subscriptions: %d %v", n, err)
	}
	for _, ep := range []string{"https://push.example.com/admin-ok", rec.gone} {
		if code, _, _ := admin.jsonDo("POST", "/api/push/subscriptions", subscription(t, ep), adminCSRF); code != http.StatusCreated {
			t.Fatalf("admin subscribe: %d", code)
		}
	}
	if code, _, _ := bob.jsonDo("POST", "/api/push/subscriptions", subscription(t, "https://push.example.com/bob"), bobCSRF); code != http.StatusCreated {
		t.Fatal("bob subscribe")
	}

	// A pending grant on alice's node notifies the admins and alice, not bob.
	g := mustAgentGrant(t, e.w, gpuSpec(0), "1h", "")
	if g.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatalf("grant %v", g.GetState())
	}
	eventually(t, 10*time.Second, "pushes", func() bool {
		return rec.count("https://push.example.com/admin-ok") == 1 && rec.count("https://push.example.com/alice-i") == 1
	})
	if rec.count("https://push.example.com/alice-a") != 0 || rec.count("https://push.example.com/bob") != 0 {
		t.Fatalf("unexpected pushes: %v", rec.hits)
	}
	// The 410 endpoint is pruned.
	eventually(t, 10*time.Second, "gone endpoint pruned", func() bool {
		n, _ := st.countPushSubs(context.Background(), e.res.AdminUserID)
		return n == 1
	})
	// A network freeze notifies everyone.
	if _, err := e.adminC.FreezeAll(context.Background(), connect.NewRequest(&mycelv1.FreezeAllRequest{Reason: "secret reason"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "freeze pushes", func() bool {
		return rec.count("https://push.example.com/bob") == 1 && rec.count("https://push.example.com/admin-ok") == 2
	})
	// Unsubscribe.
	if code, _, _ := bob.jsonDo("DELETE", "/api/push/subscriptions", map[string]string{"endpoint": "https://push.example.com/bob"}, bobCSRF); code != http.StatusNoContent {
		t.Fatalf("unsubscribe: %d", code)
	}
	if n, _ := st.countPushSubs(context.Background(), e.bobID); n != 0 {
		t.Fatalf("bob subscriptions after delete: %d", n)
	}
}

func TestPushPayload(t *testing.T) {
	g := &mycelv1.Grant{Id: "gr_1", Node: "gpu-box", Reason: "secret reason", Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Mount{
		Mount: &mycelv1.MountGrant{HostPath: "/home/alice/secret", GuestPath: "/data"},
	}}}
	var got map[string]string
	if err := json.Unmarshal(pushPayload(pushJob{approval: g}), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["type"] != "approval" || got["grant_id"] != "gr_1" || got["kind"] != "mount" || got["node"] != "gpu-box" {
		t.Fatalf("approval payload %v", got)
	}
	if string(pushPayload(pushJob{})) != `{"type":"freeze"}` {
		t.Fatalf("freeze payload %s", pushPayload(pushJob{}))
	}
	for ep, ok := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc": true, "https://updates.push.services.mozilla.com/wpush/v2/x": true,
		"https://push.example.com:443/x": true, "http://push.example.com/x": false, "https://[::1]/x": false,
		"https://intranet/x": false, "https://user@push.example.com/x": false, "https://a.localhost/x": false,
	} {
		if validPushEndpoint(ep) != ok {
			t.Errorf("validPushEndpoint(%q) = %v", ep, !ok)
		}
	}
}

func TestGrantUsageFilled(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	g1 := mustAgentGrant(t, e.w, gpuSpec(0), "1h", "")
	g2 := mustAgentGrant(t, e.w, ollamaSpec("llama3"), "1h", "")
	st := e.store(t)
	for _, u := range []struct {
		broker               string
		tokens, reqs, gpuSec int64
	}{{"ollama", 10, 1, 0}, {"ollama", 5, 2, 0}, {"gpu", 0, 0, 30}} {
		if err := st.addGrantUsage(ctx, g1.GetId(), e.w.id, u.broker, u.tokens, u.reqs, u.gpuSec); err != nil {
			t.Fatal(err)
		}
	}
	check := func(g *mycelv1.Grant) {
		t.Helper()
		switch g.GetId() {
		case g1.GetId():
			u := g.GetUsage()
			if u.GetTokens() != 15 || u.GetRequests() != 3 || u.GetGpuSeconds() != 30 || u.GetUpdatedAt() == "" {
				t.Fatalf("usage of %s: %v", g.GetId(), u)
			}
		case g2.GetId():
			if g.GetUsage() != nil {
				t.Fatalf("usage without reports: %v", g.GetUsage())
			}
		}
	}
	got, err := e.aliceG.GetGrant(ctx, connect.NewRequest(&mycelv1.GetGrantRequest{Id: g1.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	check(got.Msg.GetGrant())
	list, err := e.adminG.ListGrants(ctx, connect.NewRequest(&mycelv1.ListGrantsRequest{IncludeFinished: true}))
	if err != nil || len(list.Msg.GetGrants()) < 2 {
		t.Fatalf("ListGrants: %v %v", list, err)
	}
	for _, g := range list.Msg.GetGrants() {
		check(g)
	}
	sums, err := st.grantUsageSums(ctx, []string{g1.GetId(), g2.GetId(), "gr_none"})
	if err != nil || len(sums) != 1 || sums[g1.GetId()].GetTokens() != 15 {
		t.Fatalf("sums: %v %v", sums, err)
	}
}
