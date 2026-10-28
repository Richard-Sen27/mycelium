package daemon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// fakeAgents answers node agent RPCs on the fake hub's bus: agent.register
// and agent.refresh with real tokens, and canned agent.* methods.
type fakeAgents struct {
	h      *fakeHub
	signer *identity.AgentTokenSigner

	mu        sync.Mutex
	sub       *nats.Subscription
	ttl       time.Duration
	registers int
	refreshes int
	revoked   map[string]bool // agent ids
	// legacy answers a revoked agent's refresh like the phase-1 hub
	// ("invalid agent token") instead of "permission_denied: agent revoked".
	legacy bool
	// refreshErr, if set, is the answer to every refresh.
	refreshErr string
	pullDelay  time.Duration // inbox.pull sleeps this long before answering
	calls      []fakeCall
	inbox      []*mycelv1.Message
	changed    chan struct{} // closed and replaced when inbox grows
}

// fakeCall records an agent RPC as the hub saw it.
type fakeCall struct {
	method   string
	token    string
	afterSeq uint64
	waitMs   int32
}

func newFakeAgents(h *fakeHub) *fakeAgents {
	h.t.Helper()
	key, err := identity.NewAgentTokenKey()
	if err != nil {
		h.t.Fatal(err)
	}
	signer, err := identity.NewAgentTokenSigner(key)
	if err != nil {
		h.t.Fatal(err)
	}
	f := &fakeAgents{h: h, signer: signer, ttl: identity.AgentTokenLifetime, revoked: map[string]bool{}, changed: make(chan struct{})}
	f.subscribe()
	return f
}

// subscribe (re)subscribes on the current bus server, e.g. after a restart.
func (f *fakeAgents) subscribe() {
	f.h.t.Helper()
	nc := f.h.hubConn()
	sub, err := nc.Subscribe(bus.AllRPC, func(m *nats.Msg) { go f.handle(nc, m) })
	if err != nil {
		f.h.t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		f.h.t.Fatal(err)
	}
	f.mu.Lock()
	f.sub = sub
	f.mu.Unlock()
}

func (f *fakeAgents) setTTL(d time.Duration) {
	f.mu.Lock()
	f.ttl = d
	f.mu.Unlock()
}

func (f *fakeAgents) set(fn func(f *fakeAgents)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func (f *fakeAgents) revoke(agentID string) {
	f.mu.Lock()
	f.revoked[agentID] = true
	f.mu.Unlock()
}

func (f *fakeAgents) counts() (registers, refreshes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers, f.refreshes
}

func (f *fakeAgents) callsOf(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// appendMessage adds a message to the inbox with the next seq.
func (f *fakeAgents) appendMessage(channel, text string) *mycelv1.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := uint64(len(f.inbox) + 1)
	m := &mycelv1.Message{
		Id: fmt.Sprintf("msg_%d", seq), Channel: channel, From: "user:u1", Seq: seq,
		Payload: &mycelv1.Payload{Text: text},
	}
	f.inbox = append(f.inbox, m)
	close(f.changed)
	f.changed = make(chan struct{})
	return m
}

func (f *fakeAgents) handle(nc *nats.Conn, m *nats.Msg) {
	node, _ := bus.NodeFromSubject(m.Subject)
	method, _ := bus.RPCMethodFromSubject(m.Subject)
	if !bus.ValidReply(node, m.Reply) {
		return
	}
	resp, errText := f.serve(node, method, m.Data)
	out := nats.NewMsg(m.Reply)
	if errText != "" {
		out.Header.Set(bus.StatusHeader, bus.StatusError)
		out.Data, _ = proto.Marshal(&mycelv1.CtlResponse{Error: errText})
	} else {
		out.Header.Set(bus.StatusHeader, bus.StatusOK)
		out.Data, _ = proto.Marshal(resp)
	}
	_ = nc.PublishMsg(out)
}

func (f *fakeAgents) issue(agentID, node string) (string, string, error) {
	f.mu.Lock()
	ttl := f.ttl
	f.mu.Unlock()
	tok, err := f.signer.Issue(identity.AgentClaims{AgentID: agentID, NodeName: node, OwnerID: "u-owner"}, ttl)
	if err != nil {
		return "", "", err
	}
	c, err := f.signer.Verify(tok)
	if err != nil {
		return "", "", err
	}
	return tok, c.ExpiresAt.UTC().Format(time.RFC3339), nil
}

// verify checks an AgentCall token like the hub does.
func (f *fakeAgents) verify(node string, call *mycelv1.AgentCall) (*identity.AgentClaims, string) {
	c, err := f.signer.Verify(call.GetToken())
	if err != nil || c.NodeName != node {
		return nil, "unauthenticated: invalid agent token"
	}
	f.mu.Lock()
	revoked := f.revoked[c.AgentID]
	f.mu.Unlock()
	if revoked {
		return nil, "permission_denied: agent revoked"
	}
	return c, ""
}

func (f *fakeAgents) record(c fakeCall) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeAgents) serve(node, method string, data []byte) (proto.Message, string) {
	switch method {
	case bus.RPCRegisterAgent:
		req := &mycelv1.RegisterAgentRequest{}
		if proto.Unmarshal(data, req) != nil || !identity.ValidNodeName(req.GetAgentName()) || req.GetSandboxId() != "" {
			return nil, "invalid_argument: bad register request"
		}
		id := "agent:" + req.GetAgentName() + "@" + node
		f.mu.Lock()
		f.registers++
		revoked := f.revoked[id]
		f.mu.Unlock()
		if revoked {
			return nil, "agent revoked; register under another name"
		}
		tok, exp, err := f.issue(id, node)
		if err != nil {
			return nil, "internal error"
		}
		return &mycelv1.RegisterAgentResponse{AgentId: id, Token: tok, ExpiresAt: exp}, ""
	case bus.RPCRefreshAgentToken:
		req := &mycelv1.RefreshAgentTokenRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "malformed request"
		}
		f.mu.Lock()
		f.refreshes++
		legacy, forced := f.legacy, f.refreshErr
		f.mu.Unlock()
		if forced != "" {
			return nil, forced
		}
		c, e := f.verify(node, &mycelv1.AgentCall{Token: req.GetToken()})
		if e != "" {
			if legacy {
				// Like the phase-1 hub: the reason is not disclosed.
				return nil, "invalid agent token"
			}
			return nil, e
		}
		tok, exp, err := f.issue(c.AgentID, node)
		if err != nil {
			return nil, "internal error"
		}
		return &mycelv1.RefreshAgentTokenResponse{Token: tok, ExpiresAt: exp}, ""
	case bus.RPCAgentWhoami:
		req := &mycelv1.AgentWhoamiRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed request"
		}
		f.record(fakeCall{method: method, token: req.GetCall().GetToken()})
		c, e := f.verify(node, req.GetCall())
		if e != "" {
			return nil, e
		}
		return &mycelv1.AgentWhoamiResponse{
			Agent:  &mycelv1.AgentInfo{Id: c.AgentID, NodeName: c.NodeName, OwnerId: c.OwnerID, State: "running"},
			HubUrl: f.h.http.URL, TrustDomain: string(testTD),
		}, ""
	case bus.RPCAgentPost:
		req := &mycelv1.AgentPostRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed request"
		}
		f.record(fakeCall{method: method, token: req.GetCall().GetToken()})
		c, e := f.verify(node, req.GetCall())
		if e != "" {
			return nil, e
		}
		if req.GetPost().GetChannel() == "#forbidden" {
			return nil, "permission_denied: nope"
		}
		m := f.appendMessage(req.GetPost().GetChannel(), req.GetPost().GetText())
		m = proto.Clone(m).(*mycelv1.Message)
		m.From = c.AgentID
		return &mycelv1.PostResponse{Message: m}, ""
	case bus.RPCAgentBBGet:
		// The key is the error text to answer with (for status mapping tests).
		req := &mycelv1.AgentBlackboardGetRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed request"
		}
		f.record(fakeCall{method: method, token: req.GetCall().GetToken()})
		if _, e := f.verify(node, req.GetCall()); e != "" {
			return nil, e
		}
		if k := req.GetGet().GetKey(); k != "" {
			return nil, k
		}
		return &mycelv1.BlackboardGetResponse{}, ""
	case bus.RPCAgentInboxPull:
		req := &mycelv1.InboxPullRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed request"
		}
		f.record(fakeCall{method: method, token: req.GetCall().GetToken(), afterSeq: req.GetAfterSeq(), waitMs: req.GetWaitMs()})
		if _, e := f.verify(node, req.GetCall()); e != "" {
			return nil, e
		}
		f.mu.Lock()
		delay := f.pullDelay
		f.mu.Unlock()
		time.Sleep(delay)
		deadline := time.After(time.Duration(req.GetWaitMs()) * time.Millisecond)
		for {
			f.mu.Lock()
			var out []*mycelv1.Message
			for _, m := range f.inbox {
				if m.GetSeq() > req.GetAfterSeq() {
					out = append(out, m)
				}
			}
			changed := f.changed
			f.mu.Unlock()
			if len(out) > 0 || req.GetWaitMs() <= 0 {
				next := req.GetAfterSeq()
				if len(out) > 0 {
					next = out[len(out)-1].GetSeq()
				}
				return &mycelv1.InboxPullResponse{Messages: out, NextSeq: next}, ""
			}
			select {
			case <-changed:
			case <-deadline:
				return &mycelv1.InboxPullResponse{NextSeq: req.GetAfterSeq()}, ""
			}
		}
	default:
		if strings.HasPrefix(method, "agent.") {
			return nil, "unimplemented: " + method
		}
		return nil, "unknown method " + method
	}
}
