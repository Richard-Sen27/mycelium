package hub

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
)

// grantEnv is a hub with auto rules, users alice (member, owns node
// gpu-box with one GPU and agent w) and bob (member), and Grant, Control
// and Channel clients for everyone. It changes no package globals: the
// hub's settings go through hub.yaml.
type grantEnv struct {
	h                        *testHub
	opts                     InitOptions
	res                      *InitResult
	stop                     func()
	aliceID, bobID           string
	aliceKey, bobKey         string
	adminG, aliceG, bobG     mycelv1connect.GrantServiceClient
	adminC, aliceC, bobC     mycelv1connect.ControlServiceClient
	adminCh, aliceCh         mycelv1connect.ChannelServiceClient
	adminCl, aliceCl, anonCl clients
	node                     *enrolled
	sn                       *standIn
	w                        *testAgent
}

var testGrantRules = []GrantRuleConfig{
	{Channel: "#training-runs", Kind: "gpu", Node: "gpu-box", MaxTTL: "4h"},
	{Kind: "ollama"},
}

type grantEnvOpts struct {
	pendingTTL string
	allowFake  bool
}

func newGrantEnv(t *testing.T, o ...grantEnvOpts) *grantEnv {
	t.Helper()
	var opt grantEnvOpts
	if len(o) > 0 {
		opt = o[0]
	}
	opts, res := initTestHub(t)
	cfg, err := ReadConfig(opts.HubDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Grants = GrantsConfig{Auto: testGrantRules, PendingTTL: opt.pendingTTL}
	if err := writeConfig(opts.HubDir, cfg); err != nil {
		t.Fatal(err)
	}
	h, stop := runTestHub(t, opts, res, opt.allowFake)
	e := &grantEnv{opts: opts, res: res, stop: stop}
	e.connect(t, h)
	e.aliceID, e.aliceKey = createUser(t, e.adminCl, "alice", mycelv1.Role_ROLE_MEMBER)
	e.bobID, e.bobKey = createUser(t, e.adminCl, "bob", mycelv1.Role_ROLE_MEMBER)
	e.connect(t, h)
	e.node = enrol(t, e.anonCl, createToken(t, e.aliceCl, "gpu-box", []string{"docker", "gpu", "ollama", "sandbox"}, ""), "gpu-box")
	e.sn = h.connectNode(t, e.node)
	gpuBeat(t, e.sn, "gpu-box")
	eventually(t, 5*time.Second, "gpu-box inventory", func() bool {
		return len(getNode(t, e.adminCl, "gpu-box").GetInventory().GetGpus()) == 1
	})
	e.w = registerAgent(t, e.sn, "gpu-box", "w")
	return e
}

// connect (re)builds the clients for the running hub h.
func (e *grantEnv) connect(t *testing.T, h *testHub) {
	t.Helper()
	e.h = h
	e.adminCl, e.anonCl, e.aliceCl = h.clients(e.res.AdminAPIKey, nil), h.clients("", nil), h.clients(e.aliceKey, nil)
	hc := h.httpClient(nil)
	o := func(k string) connect.ClientOption { return connect.WithInterceptors(streamBearer(k)) }
	e.adminG = mycelv1connect.NewGrantServiceClient(hc, h.ready.HubURL, o(e.res.AdminAPIKey))
	e.aliceG = mycelv1connect.NewGrantServiceClient(hc, h.ready.HubURL, o(e.aliceKey))
	e.bobG = mycelv1connect.NewGrantServiceClient(hc, h.ready.HubURL, o(e.bobKey))
	e.adminC = mycelv1connect.NewControlServiceClient(hc, h.ready.HubURL, o(e.res.AdminAPIKey))
	e.aliceC = mycelv1connect.NewControlServiceClient(hc, h.ready.HubURL, o(e.aliceKey))
	e.bobC = mycelv1connect.NewControlServiceClient(hc, h.ready.HubURL, o(e.bobKey))
	e.adminCh = mycelv1connect.NewChannelServiceClient(hc, h.ready.HubURL, o(e.res.AdminAPIKey))
	e.aliceCh = mycelv1connect.NewChannelServiceClient(hc, h.ready.HubURL, o(e.aliceKey))
}

// store opens the hub database read-side for checks the API does not show.
func (e *grantEnv) store(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(e.opts.HubDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// online reports whether the hub sees node as connected.
func (e *grantEnv) online(t *testing.T, node string) bool {
	t.Helper()
	return getNode(t, e.adminCl, node).GetOnline()
}

func gpuBeat(t *testing.T, sn *standIn, node string) {
	t.Helper()
	b, err := proto.Marshal(&mycelv1.Heartbeat{NodeName: node, Inventory: &mycelv1.Inventory{
		Os: "linux", Gpus: []*mycelv1.Gpu{{Index: 0, Vendor: "nvidia", Model: "test"}}, SandboxBackends: []string{"fake"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sn.nc.Publish(bus.NodeTelemetry(node), b); err != nil {
		t.Fatal(err)
	}
	if err := sn.nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

func ollamaSpec(models ...string) *mycelv1.GrantSpec {
	return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{Models: models}}}
}

func gpuSpec(dev int32) *mycelv1.GrantSpec {
	return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Gpu{Gpu: &mycelv1.GpuGrant{Device: dev, Exclusive: true}}}
}

// agentGrant sends agent.grant.request for a.
func agentGrant(t *testing.T, a *testAgent, spec *mycelv1.GrantSpec, duration, channel string) (*mycelv1.Grant, error) {
	t.Helper()
	var resp mycelv1.RequestGrantResponse
	err := a.call(t, bus.RPCAgentGrantRequest, &mycelv1.AgentRequestGrantRequest{
		Call: a.ac(), Spec: spec, Duration: duration, Reason: "need it for tests", Channel: channel,
	}, &resp, 5*time.Second)
	return resp.GetGrant(), err
}

func mustAgentGrant(t *testing.T, a *testAgent, spec *mycelv1.GrantSpec, duration, channel string) *mycelv1.Grant {
	t.Helper()
	g, err := agentGrant(t, a, spec, duration, channel)
	if err != nil {
		t.Fatalf("grant request: %v", err)
	}
	return g
}

// waitCtlMatch waits for a control message matching f, skipping others.
func waitCtlMatch(t *testing.T, sn *standIn, what string, f func(*mycelv1.CtlRequest) bool) *mycelv1.CtlRequest {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case req := <-sn.ctl:
			if f(req) {
				return req
			}
		case <-deadline:
			t.Fatalf("no control message: %s", what)
			return nil
		}
	}
}

func waitAttach(t *testing.T, sn *standIn, id string) *mycelv1.AttachGrant {
	t.Helper()
	return waitCtlMatch(t, sn, "AttachGrant "+id, func(r *mycelv1.CtlRequest) bool {
		return r.GetAttachGrant().GetGrant().GetId() == id
	}).GetAttachGrant()
}

func waitDetach(t *testing.T, sn *standIn, id string) *mycelv1.DetachGrant {
	t.Helper()
	return waitCtlMatch(t, sn, "DetachGrant "+id, func(r *mycelv1.CtlRequest) bool {
		return r.GetDetachGrant().GetGrantId() == id
	}).GetDetachGrant()
}

func (e *grantEnv) grant(t *testing.T, id string) *mycelv1.Grant {
	t.Helper()
	r, err := e.adminG.GetGrant(context.Background(), connect.NewRequest(&mycelv1.GetGrantRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetGrant %s: %v", id, err)
	}
	return r.Msg.GetGrant()
}

func (e *grantEnv) waitGrantState(t *testing.T, id string, st mycelv1.GrantState) *mycelv1.Grant {
	t.Helper()
	var g *mycelv1.Grant
	eventually(t, 5*time.Second, id+" "+st.String(), func() bool {
		g = e.grant(t, id)
		return g.GetState() == st
	})
	return g
}

func (e *grantEnv) messages(t *testing.T, channel string) []*mycelv1.Message {
	t.Helper()
	r, err := e.adminCh.ListMessages(context.Background(), connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: channel, Limit: 500}))
	if err != nil {
		t.Fatalf("list %s: %v", channel, err)
	}
	return r.Msg.GetMessages()
}

// inboxHas waits for a message of type typ mentioning text in a's inbox.
func inboxHas(t *testing.T, a *testAgent, typ mycelv1.MessageType, text string) *mycelv1.Message {
	t.Helper()
	var found *mycelv1.Message
	eventually(t, 5*time.Second, "inbox message "+text, func() bool {
		for _, m := range a.pull(t, 0, 0).GetMessages() {
			if m.GetType() == typ && strings.Contains(m.GetPayload().GetText(), text) {
				found = m
				return true
			}
		}
		return false
	})
	return found
}

func TestGrantAutoApprovedOllama(t *testing.T) {
	e := newGrantEnv(t)
	g := mustAgentGrant(t, e.w, ollamaSpec("llama3:8b"), "", "")
	if g.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE || g.GetDecidedBy() != "policy" || g.GetRequiresApproval() {
		t.Fatalf("grant: %+v", g)
	}
	if g.GetSubject() != e.w.id || g.GetNode() != "gpu-box" || g.GetOwnerId() != e.aliceID || g.GetDuration() != "1h0m0s" {
		t.Fatalf("grant fields: %+v", g)
	}
	at := waitAttach(t, e.sn, g.GetId())
	if !proto.Equal(at.GetGrant().GetSpec(), ollamaSpec("llama3:8b")) {
		t.Fatalf("attached spec: %v", at.GetGrant().GetSpec())
	}
	// max_count defaults to 1: a second ollama grant needs approval.
	g2 := mustAgentGrant(t, e.w, ollamaSpec(), "", "")
	if g2.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING || !g2.GetRequiresApproval() {
		t.Fatalf("second grant: %+v", g2)
	}
	var list mycelv1.ListGrantsResponse
	if err := e.w.call(t, bus.RPCAgentGrantList, &mycelv1.AgentListGrantsRequest{Call: e.w.ac()}, &list, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(list.GetGrants()) != 2 {
		t.Fatalf("agent list: %d", len(list.GetGrants()))
	}
	// Validation.
	for _, bad := range []*mycelv1.GrantSpec{
		ollamaSpec("Bad Model"),
		gpuSpec(3),
		{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{Images: []string{"not a ref"}}}},
		{Kind: &mycelv1.GrantSpec_Mount{Mount: &mycelv1.MountGrant{HostPath: "relative/path"}}},
		{Kind: &mycelv1.GrantSpec_Mount{Mount: &mycelv1.MountGrant{HostPath: "/etc"}}},
		{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"bad host!"}}}},
		{},
	} {
		if _, err := agentGrant(t, e.w, bad, "", ""); err == nil {
			t.Errorf("spec %v accepted", bad)
		}
	}
	if _, err := agentGrant(t, e.w, ollamaSpec(), "25h", ""); err == nil {
		t.Error("25h accepted")
	}
	for _, ok := range []*mycelv1.GrantSpec{
		{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{Images: []string{"ghcr.io/org/", "python:3.12", "nvcr.io/nvidia/pytorch:24.01-py3"}}}},
		{Kind: &mycelv1.GrantSpec_Mount{Mount: &mycelv1.MountGrant{HostPath: "/srv/datasets/does-not-exist-here", ReadOnly: true}}},
		{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"pypi.org", "*.example.com"}}}},
	} {
		if g, err := agentGrant(t, e.w, ok, "", ""); err != nil {
			t.Errorf("spec %v refused: %v", ok, err)
		} else if g.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
			t.Errorf("spec %v: %v", ok, g.GetState())
		}
	}
	// Humans request for subjects they own only.
	_, err := e.bobG.RequestGrant(context.Background(), connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: e.w.id, Spec: ollamaSpec()}))
	wantCode(t, err, connect.CodePermissionDenied)
	r, err := e.aliceG.RequestGrant(context.Background(), connect.NewRequest(&mycelv1.RequestGrantRequest{
		Subject: e.w.id, Spec: gpuSpec(0), Duration: "2h",
	}))
	if err != nil || r.Msg.GetGrant().GetState() != mycelv1.GrantState_GRANT_STATE_PENDING || r.Msg.GetGrant().GetRequestedBy() != e.aliceID {
		t.Fatalf("alice request: %v %+v", err, r)
	}
}

func TestGrantApprovals(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	// A channel the agent is not a member of is ignored: no auto rule.
	g := mustAgentGrant(t, e.w, gpuSpec(0), "2h", "#training-runs")
	if g.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING || g.GetChannel() != "" {
		t.Fatalf("gpu outside the rules: %+v", g)
	}
	// The approval card.
	var card *mycelv1.Message
	for _, m := range e.messages(t, channels.Approvals) {
		if m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_REQUEST && strings.Contains(m.GetPayload().GetText(), g.GetId()) {
			card = m
		}
	}
	if card == nil || card.GetFrom() != "hub" ||
		!strings.Contains(card.GetPayload().GetText(), "requires_approval: true") ||
		!strings.Contains(card.GetPayload().GetText(), "kind: gpu\n") ||
		strings.Contains(card.GetPayload().GetText(), "need it for tests") || card.GetPayload().GetParts()[0].GetRef() != g.GetId() {
		t.Fatalf("approval card: %+v", card)
	}
	// Agents cannot post approval decisions; bob is not the node owner.
	_, err := e.bobG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: g.GetId()}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.aliceG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: g.GetId(), Duration: "3h"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	ar, err := e.aliceG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: g.GetId(), Duration: "30m", Note: "go ahead"}))
	if err != nil {
		t.Fatal(err)
	}
	if ag := ar.Msg.GetGrant(); ag.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE || ag.GetDuration() != "30m0s" || ag.GetDecidedBy() != e.aliceID {
		t.Fatalf("approved: %+v", ag)
	}
	waitAttach(t, e.sn, g.GetId())
	inboxHas(t, e.w, mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION, g.GetId()+" approved")
	_, err = e.aliceG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: g.GetId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	decisions := 0
	for _, m := range e.messages(t, channels.Approvals) {
		if m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION {
			decisions++
		}
	}
	if decisions != 1 {
		t.Fatalf("decisions in #approvals: %d", decisions)
	}

	// Admin approves; deny sends a DM too.
	g2 := mustAgentGrant(t, e.w, ollamaSpec("a"), "", "")
	g3 := mustAgentGrant(t, e.w, ollamaSpec("b"), "", "")
	if g2.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE || g3.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatalf("g2 %v g3 %v", g2.GetState(), g3.GetState())
	}
	g4 := mustAgentGrant(t, e.w, ollamaSpec("c"), "", "")
	if _, err := e.adminG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: g3.GetId()})); err != nil {
		t.Fatalf("admin approve: %v", err)
	}
	_, err = e.bobG.DenyGrant(ctx, connect.NewRequest(&mycelv1.DenyGrantRequest{Id: g4.GetId()}))
	wantCode(t, err, connect.CodePermissionDenied)
	dr, err := e.aliceG.DenyGrant(ctx, connect.NewRequest(&mycelv1.DenyGrantRequest{Id: g4.GetId(), Note: "no"}))
	if err != nil || dr.Msg.GetGrant().GetState() != mycelv1.GrantState_GRANT_STATE_DENIED {
		t.Fatalf("deny: %v %+v", err, dr)
	}
	inboxHas(t, e.w, mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION, g4.GetId()+" denied")

	// Bob sees none of alice's grants; alice and the admin see them.
	lr, err := e.bobG.ListGrants(ctx, connect.NewRequest(&mycelv1.ListGrantsRequest{IncludeFinished: true}))
	if err != nil || len(lr.Msg.GetGrants()) != 0 {
		t.Fatalf("bob list: %v %d", err, len(lr.Msg.GetGrants()))
	}
	_, err = e.bobG.GetGrant(ctx, connect.NewRequest(&mycelv1.GetGrantRequest{Id: g.GetId()}))
	wantCode(t, err, connect.CodePermissionDenied)
	lr, err = e.aliceG.ListGrants(ctx, connect.NewRequest(&mycelv1.ListGrantsRequest{IncludeFinished: true}))
	if err != nil || len(lr.Msg.GetGrants()) != 4 {
		t.Fatalf("alice list: %v %d", err, len(lr.Msg.GetGrants()))
	}
	// Agents can neither join nor read #approvals.
	if _, err := e.w.join(t, channels.Approvals, nil); err == nil || !strings.HasPrefix(err.Error(), "permission_denied") {
		t.Fatalf("agent joined #approvals: %v", err)
	}
	var lm mycelv1.ListMessagesResponse
	if err := e.w.call(t, bus.RPCAgentListMessages, &mycelv1.AgentListMessagesRequest{Call: e.w.ac(),
		List: &mycelv1.ListMessagesRequest{Channel: channels.Approvals}}, &lm, 5*time.Second); err == nil {
		t.Fatal("agent read #approvals")
	}
	_, err = e.w.post(t, &mycelv1.PostRequest{Channel: channels.General, Text: "approve me", Type: mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_REQUEST})
	if err == nil {
		t.Fatal("agent posted an approval request")
	}

	// Once the agent is a member of #training-runs, the rule auto-grants.
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#training-runs", Policy: &mycelv1.ChannelPolicy{OpenToMembers: true},
	})); err != nil {
		t.Fatal(err)
	}
	// A self-join is not channel context for the rules.
	if _, err := e.w.join(t, "#training-runs", nil); err != nil {
		t.Fatal(err)
	}
	if sj := mustAgentGrant(t, e.w, gpuSpec(0), "2h", "#training-runs"); sj.GetChannel() != "" || sj.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatalf("self-joined channel counted: %+v", sj)
	}
	// Added by the channel owner, it is.
	if _, err := e.aliceCh.AddMember(ctx, connect.NewRequest(&mycelv1.AddMemberRequest{Channel: "#training-runs", Member: e.w.id})); err != nil {
		t.Fatal(err)
	}
	// The approved GPU grant is active: max_count 1 means approval again.
	g5 := mustAgentGrant(t, e.w, gpuSpec(0), "2h", "#training-runs")
	if g5.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING || g5.GetChannel() != "#training-runs" {
		t.Fatalf("second gpu: %+v", g5)
	}
	if _, err := e.aliceG.RevokeGrant(ctx, connect.NewRequest(&mycelv1.RevokeGrantRequest{Id: g.GetId()})); err != nil {
		t.Fatal(err)
	}
	waitDetach(t, e.sn, g.GetId())
	g6 := mustAgentGrant(t, e.w, gpuSpec(0), "4h", "#training-runs")
	if g6.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE {
		t.Fatalf("gpu in #training-runs: %+v", g6)
	}
	if g7 := mustAgentGrant(t, e.w, gpuSpec(0), "5h", "#training-runs"); g7.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatalf("over max_ttl: %+v", g7)
	}
}

func TestGrantExpiryRevokeAndUsage(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	g := mustAgentGrant(t, e.w, ollamaSpec(), "1s", "")
	waitAttach(t, e.sn, g.GetId())
	if d := waitDetach(t, e.sn, g.GetId()); d.GetReason() != "expired" {
		t.Fatalf("detach: %+v", d)
	}
	e.waitGrantState(t, g.GetId(), mycelv1.GrantState_GRANT_STATE_EXPIRED)

	g2 := mustAgentGrant(t, e.w, ollamaSpec(), "1h", "")
	usage := func(id string) (*mycelv1.BrokerUsageResponse, error) {
		var resp mycelv1.BrokerUsageResponse
		_, err := e.sn.rpc(t, "gpu-box", bus.RPCBrokerUsage, &mycelv1.BrokerUsageRequest{
			GrantId: id, Subject: e.w.id, Broker: "ollama", Tokens: 100, Requests: 2, Model: "llama3",
		}, &resp)
		return &resp, err
	}
	r, err := usage(g2.GetId())
	if err != nil || r.GetDeny() {
		t.Fatalf("usage: %v %+v", err, r)
	}
	if r, err := usage(g.GetId()); err != nil || !r.GetDeny() {
		t.Fatalf("usage of expired grant: %v %+v", err, r)
	}
	rows, err := e.store(t).grantUsage(ctx, g2.GetId())
	if err != nil || len(rows) != 1 || rows[0].Tokens != 100 || rows[0].Requests != 2 {
		t.Fatalf("usage rows: %v %+v", err, rows)
	}
	var rr mycelv1.GrantRevokedResponse
	if _, err := e.sn.rpc(t, "gpu-box", bus.RPCGrantRevoked, &mycelv1.GrantRevokedRequest{GrantId: g2.GetId(), Reason: "owner said no"}, &rr); err != nil {
		t.Fatal(err)
	}
	rg := e.waitGrantState(t, g2.GetId(), mycelv1.GrantState_GRANT_STATE_REVOKED)
	if rg.GetDecidedBy() != "node:gpu-box" || rg.GetDecisionNote() != "owner said no" {
		t.Fatalf("revoked: %+v", rg)
	}
	if r, err := usage(g2.GetId()); err != nil || !r.GetDeny() {
		t.Fatalf("usage after revoke: %v %+v", err, r)
	}
	if _, err := usage("grt_00000000000000000000000000"); err == nil || !strings.HasPrefix(err.Error(), "permission_denied") {
		t.Fatalf("unknown grant: %v", err)
	}
	// Another node cannot report on or revoke gpu-box's grants.
	other := enrol(t, e.anonCl, createToken(t, e.aliceCl, "other", []string{"ollama"}, ""), "other")
	osn := e.h.connectNode(t, other)
	if _, err := osn.rpc(t, "other", bus.RPCGrantRevoked, &mycelv1.GrantRevokedRequest{GrantId: g2.GetId()}, &rr); err == nil {
		t.Fatal("other node revoked a grant")
	}
}

func TestGrantReattachOnReconnect(t *testing.T) {
	e := newGrantEnv(t)
	g := mustAgentGrant(t, e.w, ollamaSpec("m"), "", "")
	waitAttach(t, e.sn, g.GetId())
	e.sn.nc.Close()
	eventually(t, 5*time.Second, "node offline", func() bool { return !e.online(t, "gpu-box") })
	sn2 := e.h.connectNode(t, e.node)
	gpuBeat(t, sn2, "gpu-box") // the full inventory after a reconnect triggers the sync
	at := waitAttach(t, sn2, g.GetId())
	if at.GetGrant().GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE {
		t.Fatalf("reattached: %+v", at.GetGrant())
	}
}

func TestNodeSharing(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	r, err := e.aliceC.GetNodeSharing(ctx, connect.NewRequest(&mycelv1.GetNodeSharingRequest{Name: "gpu-box"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Msg.GetEffective().GetOffer().GetNames(); strings.Join(got, ",") != "docker,gpu,ollama,sandbox" || r.Msg.GetLocal() != nil {
		t.Fatalf("default: %v", r.Msg)
	}
	_, err = e.bobC.GetNodeSharing(ctx, connect.NewRequest(&mycelv1.GetNodeSharingRequest{Name: "gpu-box"}))
	wantCode(t, err, connect.CodePermissionDenied)

	// A node whose token ceiling is narrower.
	small := enrol(t, e.anonCl, createToken(t, e.aliceCl, "small", []string{"ollama", "sandbox"}, ""), "small")
	ssn := e.h.connectNode(t, small)
	set := func(c mycelv1connect.ControlServiceClient, name string, offers ...string) error {
		_, err := c.SetNodeSharing(ctx, connect.NewRequest(&mycelv1.SetNodeSharingRequest{Name: name, Settings: &mycelv1.SharingSettings{
			Offer: &mycelv1.Capabilities{Names: offers}, When: "always", LocalFirst: true,
		}}))
		return err
	}
	wantCode(t, set(e.aliceC, "small", "ollama", "gpu"), connect.CodeInvalidArgument)
	wantCode(t, set(e.aliceC, "small", "printers"), connect.CodeInvalidArgument)
	wantCode(t, set(e.bobC, "small", "ollama"), connect.CodePermissionDenied)
	if err := set(e.aliceC, "small", "ollama", "sandbox"); err != nil {
		t.Fatal(err)
	}
	ctl := waitCtlMatch(t, ssn, "SharingSettings", func(r *mycelv1.CtlRequest) bool { return r.GetSharing() != nil })
	if strings.Join(ctl.GetSharing().GetOffer().GetNames(), ",") != "ollama,sandbox" {
		t.Fatalf("ctl sharing: %v", ctl.GetSharing())
	}
	// The node reports a tighter local ceiling and gets the hub settings.
	var nr mycelv1.NodeSharingResponse
	if _, err := ssn.rpc(t, "small", bus.RPCNodeSharing, &mycelv1.NodeSharingRequest{Local: &mycelv1.SharingSettings{
		Offer: &mycelv1.Capabilities{Names: []string{"sandbox"}}, When: "idle",
	}}, &nr); err != nil {
		t.Fatal(err)
	}
	if strings.Join(nr.GetHub().GetOffer().GetNames(), ",") != "ollama,sandbox" {
		t.Fatalf("node.sharing reply: %v", nr.GetHub())
	}
	if _, err := ssn.rpc(t, "small", bus.RPCNodeSharing, &mycelv1.NodeSharingRequest{Local: &mycelv1.SharingSettings{When: "sometimes"}}, &nr); err == nil {
		t.Fatal("invalid local settings accepted")
	}
	r, err = e.adminC.GetNodeSharing(ctx, connect.NewRequest(&mycelv1.GetNodeSharingRequest{Name: "small"}))
	if err != nil {
		t.Fatal(err)
	}
	eff := r.Msg.GetEffective()
	if strings.Join(eff.GetOffer().GetNames(), ",") != "sandbox" || eff.GetWhen() != "idle" || r.Msg.GetLocal() == nil {
		t.Fatalf("effective: %v", r.Msg)
	}
	// Grant requests check the effective offers.
	ag := registerAgent(t, ssn, "small", "s")
	if _, err := agentGrant(t, ag, ollamaSpec(), "", ""); err == nil || !strings.HasPrefix(err.Error(), "failed_precondition") {
		t.Fatalf("ollama on a node that does not offer it: %v", err)
	}
	if _, err := agentGrant(t, ag, &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"pypi.org"}}}}, "", ""); err != nil {
		t.Fatalf("egress (sandbox offered): %v", err)
	}
}

func TestFreezeChannel(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "#proj"})); err != nil {
		t.Fatal(err)
	}
	worker := mustRegister(t, e.sn, "gpu-box", &mycelv1.RegisterAgentRequest{AgentName: "worker", HomeChannel: "#proj"})
	agents := e.aliceCl.agents
	_, err := e.bobC.FreezeChannel(ctx, connect.NewRequest(&mycelv1.FreezeChannelRequest{Channel: "#proj"}))
	wantCode(t, err, connect.CodePermissionDenied)
	drainCtl(e.sn)
	if _, err := e.aliceC.FreezeChannel(ctx, connect.NewRequest(&mycelv1.FreezeChannelRequest{Channel: "#proj", Reason: "oops"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, agents, worker.id, statePaused, reasonFrozenChannel)
	waitCtl(t, e.sn, worker.id, "pause")
	wantState(t, agents, e.w.id, stateRunning, "")
	_, err = e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#proj", Text: "hi"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), "channel frozen") {
		t.Fatalf("post: %v", err)
	}
	if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: channels.General, Text: "hi"})); err != nil {
		t.Fatalf("post elsewhere: %v", err)
	}
	// An operator cannot resume a frozen agent.
	_, err = agents.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: worker.id}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	st, err := e.bobC.FreezeStatus(ctx, connect.NewRequest(&mycelv1.FreezeStatusRequest{}))
	if err != nil || st.Msg.GetAllFrozen() || strings.Join(st.Msg.GetFrozenChannels(), ",") != "#proj" {
		t.Fatalf("status: %v %+v", err, st)
	}
	if _, err := e.aliceC.UnfreezeChannel(ctx, connect.NewRequest(&mycelv1.UnfreezeChannelRequest{Channel: "#proj"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, agents, worker.id, stateRunning, "")
	waitCtl(t, e.sn, worker.id, "resume")
	if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#proj", Text: "hi"})); err != nil {
		t.Fatal(err)
	}
}

func TestFreezeAll(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	g := mustAgentGrant(t, e.w, ollamaSpec(), "", "")
	drainCtl(e.sn)
	_, err := e.bobC.FreezeAll(ctx, connect.NewRequest(&mycelv1.FreezeAllRequest{Reason: "smoke"}))
	wantCode(t, err, connect.CodePermissionDenied)
	if _, err := e.adminC.FreezeAll(ctx, connect.NewRequest(&mycelv1.FreezeAllRequest{Reason: "smoke"})); err != nil {
		t.Fatalf("freeze all: %v", err)
	}
	f := waitCtlMatch(t, e.sn, "Freeze", func(r *mycelv1.CtlRequest) bool { return r.GetFreeze() != nil }).GetFreeze()
	if f.GetScope() != "all" || f.GetReason() != "smoke" {
		t.Fatalf("freeze: %+v", f)
	}
	agents := e.aliceCl.agents
	wantState(t, agents, e.w.id, statePaused, reasonFrozenNetwork)
	_, err = e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: channels.General, Text: "hi"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	jobs := mycelv1connect.NewJobServiceClient(e.h.httpClient(nil), e.h.ready.HubURL, connect.WithInterceptors(streamBearer(e.aliceKey)))
	_, err = jobs.RunJob(ctx, connect.NewRequest(&mycelv1.RunJobRequest{Spec: &mycelv1.JobSpec{Profile: "default", Command: []string{"true"}}}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = e.aliceG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: e.w.id, Spec: ollamaSpec()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	var ur mycelv1.BrokerUsageResponse
	if _, err := e.sn.rpc(t, "gpu-box", bus.RPCBrokerUsage, &mycelv1.BrokerUsageRequest{GrantId: g.GetId(), Broker: "ollama"}, &ur); err != nil || !ur.GetDeny() {
		t.Fatalf("usage while frozen: %v %+v", err, &ur)
	}
	alerted := false
	for _, m := range e.messages(t, channels.Alerts) {
		alerted = alerted || strings.Contains(m.GetPayload().GetText(), "network frozen by "+e.res.AdminUserID)
	}
	if !alerted {
		t.Fatal("no #alerts event")
	}
	// A node that connects while frozen is told at once.
	late := enrol(t, e.anonCl, createToken(t, e.aliceCl, "late", nil, ""), "late")
	lsn := e.h.connectNode(t, late)
	gpuBeat(t, lsn, "late")
	waitCtlMatch(t, lsn, "Freeze for late node", func(r *mycelv1.CtlRequest) bool { return r.GetFreeze().GetScope() == "all" })
	// A newly registered agent starts paused.
	if a := mustRegister(t, lsn, "late", &mycelv1.RegisterAgentRequest{AgentName: "fresh"}); a != nil {
		wantState(t, agents, a.id, statePaused, reasonFrozenNetwork)
	}

	_, err = e.bobC.UnfreezeAll(ctx, connect.NewRequest(&mycelv1.UnfreezeAllRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	drainCtl(e.sn)
	if _, err := e.adminC.UnfreezeAll(ctx, connect.NewRequest(&mycelv1.UnfreezeAllRequest{})); err != nil {
		t.Fatal(err)
	}
	waitCtlMatch(t, e.sn, "Unfreeze", func(r *mycelv1.CtlRequest) bool { return r.GetUnfreeze().GetScope() == "all" })
	wantState(t, agents, e.w.id, stateRunning, "")
	if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: channels.General, Text: "hi"})); err != nil {
		t.Fatal(err)
	}
	st, err := e.aliceC.FreezeStatus(ctx, connect.NewRequest(&mycelv1.FreezeStatusRequest{}))
	if err != nil || st.Msg.GetAllFrozen() {
		t.Fatalf("status: %v %+v", err, st)
	}
}

// ---- approvals page ----

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

type browser struct {
	t    *testing.T
	c    *http.Client
	base string
}

func (e *grantEnv) browser(t *testing.T) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := e.h.httpClient(nil)
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &browser{t: t, c: c, base: e.h.ready.HubURL}
}

func (b *browser) do(method, path string, form url.Values) (int, string, http.Header) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

func (b *browser) login(key string) {
	b.t.Helper()
	code, page, _ := b.do("GET", "/ui/login", nil)
	m := csrfRE.FindStringSubmatch(page)
	if code != http.StatusOK || m == nil {
		b.t.Fatalf("login page: %d", code)
	}
	code, _, hdr := b.do("POST", "/ui/login", url.Values{"csrf": {m[1]}, "api_key": {key}})
	if code != http.StatusSeeOther || hdr.Get("Location") != "/ui/approvals" {
		b.t.Fatalf("login: %d %v", code, hdr)
	}
}

func (b *browser) approvals() (string, string) {
	b.t.Helper()
	code, page, hdr := b.do("GET", "/ui/approvals", nil)
	if code != http.StatusOK {
		b.t.Fatalf("approvals: %d", code)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") {
		b.t.Fatalf("csp: %q", hdr.Get("Content-Security-Policy"))
	}
	m := csrfRE.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatal("no csrf token")
	}
	return page, m[1]
}

func TestApprovalsPage(t *testing.T) {
	e := newGrantEnv(t)
	evil := "<script>alert(1)</script>"
	var resp mycelv1.RequestGrantResponse
	if err := e.w.call(t, bus.RPCAgentGrantRequest, &mycelv1.AgentRequestGrantRequest{
		Call: e.w.ac(), Spec: gpuSpec(0), Duration: "1h", Reason: evil,
	}, &resp, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	g1 := resp.GetGrant()
	g2 := mustAgentGrant(t, e.w, &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"pypi.org"}}}}, "", "")

	anon := e.browser(t)
	if code, _, hdr := anon.do("GET", "/ui/approvals", nil); code != http.StatusSeeOther || hdr.Get("Location") != "/ui/login" {
		t.Fatalf("anonymous: %d %v", code, hdr)
	}
	// A bad key, and a login post without the login form's token.
	code, page, _ := anon.do("GET", "/ui/login", nil)
	tok := csrfRE.FindStringSubmatch(page)[1]
	if code, _, _ := anon.do("POST", "/ui/login", url.Values{"csrf": {tok}, "api_key": {"mck_nope"}}); code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}
	if code, _, _ := e.browser(t).do("POST", "/ui/login", url.Values{"csrf": {tok}, "api_key": {e.aliceKey}}); code != http.StatusForbidden {
		t.Fatalf("login without cookie: %d", code)
	}
	_ = code

	// Bob is not the node owner: nothing listed, and a direct post fails.
	bob := e.browser(t)
	bob.login(e.bobKey)
	page, bobCSRF := bob.approvals()
	if strings.Contains(page, g1.GetId()) {
		t.Fatal("bob sees alice's grant")
	}
	if code, _, _ := bob.do("POST", "/ui/approvals/"+g1.GetId()+"/approve", url.Values{"csrf": {bobCSRF}}); code != http.StatusSeeOther {
		t.Fatalf("bob approve: %d", code)
	}
	page, _ = bob.approvals()
	if !strings.Contains(page, "Could not approve") {
		t.Fatal("bob's refusal not shown")
	}
	if e.grant(t, g1.GetId()).GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatal("bob approved via the page")
	}

	alice := e.browser(t)
	alice.login(e.aliceKey)
	page, csrf := alice.approvals()
	if !strings.Contains(page, g1.GetId()) || !strings.Contains(page, g2.GetId()) {
		t.Fatal("alice's page does not list the grants")
	}
	if strings.Contains(page, evil) || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("reason not escaped")
	}
	// Without and with a wrong CSRF token: refused.
	if code, _, _ := alice.do("POST", "/ui/approvals/"+g1.GetId()+"/approve", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", code)
	}
	if code, _, _ := alice.do("POST", "/ui/approvals/"+g1.GetId()+"/approve", url.Values{"csrf": {bobCSRF}}); code != http.StatusForbidden {
		t.Fatalf("bob's csrf: %d", code)
	}
	if code, _, _ := alice.do("POST", "/ui/approvals/"+g1.GetId()+"/approve", url.Values{"csrf": {csrf}, "duration": {"20m"}}); code != http.StatusSeeOther {
		t.Fatalf("approve: %d", code)
	}
	ag := e.waitGrantState(t, g1.GetId(), mycelv1.GrantState_GRANT_STATE_ACTIVE)
	if ag.GetDuration() != "20m0s" || ag.GetDecidedBy() != e.aliceID {
		t.Fatalf("approved via page: %+v", ag)
	}
	waitAttach(t, e.sn, g1.GetId())
	if code, _, _ := alice.do("POST", "/ui/approvals/"+g2.GetId()+"/deny", url.Values{"csrf": {csrf}, "note": {"not now"}}); code != http.StatusSeeOther {
		t.Fatalf("deny: %d", code)
	}
	e.waitGrantState(t, g2.GetId(), mycelv1.GrantState_GRANT_STATE_DENIED)
	page, _ = alice.approvals()
	if strings.Contains(page, "/ui/approvals/"+g1.GetId()+"/") || strings.Contains(page, "/ui/approvals/"+g2.GetId()+"/") ||
		!strings.Contains(page, g2.GetId()+" denied") {
		t.Fatalf("decided grants still listed or no flash: %s", page)
	}
	// Admins see and decide everything.
	g3 := mustAgentGrant(t, e.w, ollamaSpec("x"), "", "")
	g4 := mustAgentGrant(t, e.w, ollamaSpec("y"), "", "")
	_ = g3
	admin := e.browser(t)
	admin.login(e.h.init.AdminAPIKey)
	page, acsrf := admin.approvals()
	if !strings.Contains(page, g4.GetId()) {
		t.Fatal("admin does not see the grant")
	}
	if code, _, _ := admin.do("POST", "/ui/approvals/"+g4.GetId()+"/approve", url.Values{"csrf": {acsrf}}); code != http.StatusSeeOther {
		t.Fatal("admin approve")
	}
	e.waitGrantState(t, g4.GetId(), mycelv1.GrantState_GRANT_STATE_ACTIVE)
	// Logout ends the session.
	if code, _, _ := alice.do("POST", "/ui/logout", url.Values{"csrf": {csrf}}); code != http.StatusSeeOther {
		t.Fatalf("logout: %d", code)
	}
	if code, _, _ := alice.do("GET", "/ui/approvals", nil); code != http.StatusSeeOther {
		t.Fatalf("after logout: %d", code)
	}
}
