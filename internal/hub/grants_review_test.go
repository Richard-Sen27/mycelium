package hub

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
)

func (e *grantEnv) freezeAll(t *testing.T) {
	t.Helper()
	if _, err := e.adminC.FreezeAll(context.Background(), connect.NewRequest(&mycelv1.FreezeAllRequest{Reason: "test"})); err != nil {
		t.Fatalf("FreezeAll: %v", err)
	}
}

func (e *grantEnv) unfreezeAll(t *testing.T) {
	t.Helper()
	if _, err := e.adminC.UnfreezeAll(context.Background(), connect.NewRequest(&mycelv1.UnfreezeAllRequest{})); err != nil {
		t.Fatalf("UnfreezeAll: %v", err)
	}
}

func (e *grantEnv) jobs(key string) mycelv1connect.JobServiceClient {
	return mycelv1connect.NewJobServiceClient(e.h.httpClient(nil), e.h.ready.HubURL, connect.WithInterceptors(streamBearer(key)))
}

// nextCtl returns the next control message a stand-in receives.
func nextCtl(t *testing.T, sn *standIn) *mycelv1.CtlRequest {
	t.Helper()
	select {
	case r := <-sn.ctl:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no control message")
		return nil
	}
}

func TestGrantPendingExpiry(t *testing.T) {
	e := newGrantEnv(t, grantEnvOpts{pendingTTL: "1s"})
	g := mustAgentGrant(t, e.w, gpuSpec(0), "", "")
	eg := e.waitGrantState(t, g.GetId(), mycelv1.GrantState_GRANT_STATE_EXPIRED)
	if eg.GetDecidedBy() != "hub" || !strings.Contains(eg.GetDecisionNote(), "no decision") {
		t.Fatalf("expired: %+v", eg)
	}
	inboxHas(t, e.w, mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION, g.GetId()+" expired")
	found := false
	for _, m := range e.messages(t, channels.Approvals) {
		found = found || (m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION && strings.Contains(m.GetPayload().GetText(), g.GetId()+" expired"))
	}
	if !found {
		t.Fatal("no expiry decision in #approvals")
	}
}

func TestGrantExpiryAcrossRestart(t *testing.T) {
	e := newGrantEnv(t)
	g := mustAgentGrant(t, e.w, ollamaSpec(), "2s", "")
	if g.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE {
		t.Fatalf("grant: %+v", g)
	}
	e.stop()
	time.Sleep(2500 * time.Millisecond)
	h2, _ := runTestHub(t, e.opts, e.res, false)
	e.connect(t, h2)
	e.waitGrantState(t, g.GetId(), mycelv1.GrantState_GRANT_STATE_EXPIRED)
}

func TestMissedUnfreezeResolvedBySync(t *testing.T) {
	e := newGrantEnv(t)
	e.freezeAll(t)
	waitCtlMatch(t, e.sn, "Freeze", func(r *mycelv1.CtlRequest) bool { return r.GetFreeze() != nil })
	e.sn.nc.Close()
	eventually(t, 5*time.Second, "node offline", func() bool { return !e.online(t, "gpu-box") })
	e.unfreezeAll(t)
	sn2 := e.h.connectNode(t, e.node)
	gpuBeat(t, sn2, "gpu-box")
	if first := nextCtl(t, sn2); first.GetUnfreeze().GetScope() != "all" {
		t.Fatalf("first control message after reconnect: %v", first)
	}
}

func TestOperatorPauseUnderFreeze(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	agents := e.aliceCl.agents
	e.freezeAll(t)
	wantState(t, agents, e.w.id, statePaused, reasonFrozenNetwork)
	if _, err := agents.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: e.w.id, Reason: "hold"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, agents, e.w.id, statePaused, "operator: hold")
	e.unfreezeAll(t)
	time.Sleep(300 * time.Millisecond)
	wantState(t, agents, e.w.id, statePaused, "operator: hold")
	if _, err := agents.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: e.w.id})); err != nil {
		t.Fatal(err)
	}
	wantState(t, agents, e.w.id, stateRunning, "")
}

func TestBudgetAutoResumeDuringFreeze(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	agents := e.aliceCl.agents
	b := mustRegister(t, e.sn, "gpu-box", &mycelv1.RegisterAgentRequest{AgentName: "b", Budget: &mycelv1.Budget{Tokens: 100}})
	if _, err := reportUsage(t, e.sn, "gpu-box", b.id, 100, 0); err != nil {
		t.Fatal(err)
	}
	wantState(t, agents, b.id, statePaused, reasonAgentBudget)
	e.freezeAll(t)
	wantState(t, agents, b.id, statePaused, reasonFrozenNetwork)
	if _, err := agents.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: b.id, Budget: &mycelv1.Budget{Tokens: 1000}})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	wantState(t, agents, b.id, statePaused, reasonFrozenNetwork)
	e.unfreezeAll(t)
	wantState(t, agents, b.id, stateRunning, "")
}

func TestGrantJobSubjects(t *testing.T) {
	e := newGrantEnv(t, grantEnvOpts{allowFake: true})
	ctx := context.Background()
	r, err := e.jobs(e.aliceKey).RunJob(ctx, connect.NewRequest(&mycelv1.RunJobRequest{Spec: fake("true")}))
	if err != nil {
		t.Fatal(err)
	}
	job := r.Msg.GetJob().GetId()
	waitSandboxCtl(t, e.sn, job, true)
	subject := "job:" + job
	_, err = e.bobG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: subject, Spec: gpuSpec(0)}))
	wantCode(t, err, connect.CodePermissionDenied)
	gr, err := e.aliceG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: subject, Spec: gpuSpec(0)}))
	if err != nil {
		t.Fatal(err)
	}
	g := gr.Msg.GetGrant()
	if g.GetNode() != "gpu-box" || g.GetOwnerId() != e.aliceID || g.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
		t.Fatalf("job grant: %+v", g)
	}
	if _, err := e.aliceG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: g.GetId()})); err != nil {
		t.Fatal(err)
	}
	waitAttach(t, e.sn, g.GetId())
	if err := nodeRPC(t, e.sn, "gpu-box", bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: job, SandboxId: "sbx-1", Backend: "fake"}, &mycelv1.JobStartedResponse{}); err != nil {
		t.Fatal(err)
	}
	if err := nodeRPC(t, e.sn, "gpu-box", bus.RPCJobDone, &mycelv1.JobDoneRequest{JobId: job, State: mycelv1.JobState_JOB_STATE_SUCCEEDED}, &mycelv1.JobDoneResponse{}); err != nil {
		t.Fatal(err)
	}
	rg := e.waitGrantState(t, g.GetId(), mycelv1.GrantState_GRANT_STATE_REVOKED)
	if rg.GetDecisionNote() != "job finished" {
		t.Fatalf("job grant after the job: %+v", rg)
	}
	waitDetach(t, e.sn, g.GetId())
	_, err = e.aliceG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: subject, Spec: gpuSpec(0)}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}

// A job that waits in the queue for its pinned node can get a grant there,
// so the grant is attached before the node creates the sandbox; an
// unpinned queued job has no node yet and cannot.
func TestGrantForQueuedPinnedJob(t *testing.T) {
	e := newGrantEnv(t, grantEnvOpts{allowFake: true})
	ctx := context.Background()
	run := func(spec *mycelv1.JobSpec) string {
		t.Helper()
		r, err := e.jobs(e.aliceKey).RunJob(ctx, connect.NewRequest(&mycelv1.RunJobRequest{Spec: spec}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetJob().GetId()
	}
	// gpu-box reports no CPU count, so it takes two jobs at a time.
	for range 2 {
		waitSandboxCtl(t, e.sn, run(fake("sleep", "60")), true)
	}
	pinned := fake("true")
	pinned.Node = "gpu-box"
	queued, loose := run(pinned), run(fake("true"))
	for _, id := range []string{queued, loose} {
		r, err := e.jobs(e.aliceKey).GetJob(ctx, connect.NewRequest(&mycelv1.GetJobRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if st := r.Msg.GetJob().GetState(); st != mycelv1.JobState_JOB_STATE_QUEUED {
			t.Fatalf("job %s is %s, want queued", id, st)
		}
	}
	gr, err := e.aliceG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: "job:" + queued, Spec: ollamaSpec()}))
	if err != nil {
		t.Fatal(err)
	}
	if g := gr.Msg.GetGrant(); g.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE || g.GetNode() != "gpu-box" {
		t.Fatalf("grant for the queued pinned job: %+v", g)
	}
	waitAttach(t, e.sn, gr.Msg.GetGrant().GetId())
	_, err = e.aliceG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: "job:" + loose, Spec: ollamaSpec()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}

func TestRemoveNodeEndsGrants(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	n2 := enrol(t, e.anonCl, createToken(t, e.aliceCl, "n2", []string{"ollama"}, ""), "n2")
	sn2 := e.h.connectNode(t, n2)
	a2 := registerAgent(t, sn2, "n2", "a2")
	g := mustAgentGrant(t, a2, ollamaSpec(), "", "")
	if g.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE {
		t.Fatalf("grant: %+v", g)
	}
	if _, err := e.aliceCl.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "n2"})); err != nil {
		t.Fatal(err)
	}
	rg := e.waitGrantState(t, g.GetId(), mycelv1.GrantState_GRANT_STATE_REVOKED)
	if rg.GetDecisionNote() != "node removed" {
		t.Fatalf("after removal: %+v", rg)
	}
}

func TestApprovalsLoginLimits(t *testing.T) {
	e := newGrantEnv(t)
	b := e.browser(t)
	_, page, _ := b.do("GET", "/ui/login", nil)
	tok := csrfRE.FindStringSubmatch(page)[1]
	// A cross-site form post is refused before anything else.
	req, err := http.NewRequest("POST", b.base+"/ui/login", strings.NewReader(url.Values{"csrf": {tok}, "api_key": {e.aliceKey}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", resp.StatusCode)
	}
	// Bad keys until the per-IP limit answers 429.
	last := 0
	for range uiLoginBurst + 1 {
		_, page, _ := b.do("GET", "/ui/login", nil)
		tok := csrfRE.FindStringSubmatch(page)[1]
		last, _, _ = b.do("POST", "/ui/login", url.Values{"csrf": {tok}, "api_key": {"mck_wrong"}})
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d bad attempts: %d", uiLoginBurst+1, last)
	}
}

func TestFreezeDispatch(t *testing.T) {
	e := newGrantEnv(t, grantEnvOpts{allowFake: true})
	ctx := context.Background()
	e.sn.nc.Close()
	eventually(t, 5*time.Second, "node offline", func() bool { return !e.online(t, "gpu-box") })
	r, err := e.jobs(e.aliceKey).RunJob(ctx, connect.NewRequest(&mycelv1.RunJobRequest{Spec: fake("true")}))
	if err != nil {
		t.Fatal(err)
	}
	job := r.Msg.GetJob().GetId()
	e.freezeAll(t)
	sn2 := e.h.connectNode(t, e.node)
	gpuBeat(t, sn2, "gpu-box")
	if first := nextCtl(t, sn2); first.GetFreeze().GetScope() != "all" {
		t.Fatalf("first control message after reconnect: %v", first)
	}
	deadline := time.After(1500 * time.Millisecond)
wait:
	for {
		select {
		case m := <-sn2.ctl:
			if m.GetCreateSandbox() != nil {
				t.Fatal("job dispatched while frozen")
			}
		case <-deadline:
			break wait
		}
	}
	e.unfreezeAll(t)
	waitSandboxCtl(t, sn2, job, true)
}

func TestApproveRechecks(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	approve := func(id string) error {
		_, err := e.aliceG.ApproveGrant(ctx, connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: id}))
		return err
	}
	g := mustAgentGrant(t, e.w, gpuSpec(0), "", "")
	// Not while frozen.
	e.freezeAll(t)
	err := approve(g.GetId())
	wantCode(t, err, connect.CodeFailedPrecondition)
	e.unfreezeAll(t)
	wantState(t, e.aliceCl.agents, e.w.id, stateRunning, "")
	// Not once the node's local ceiling withdrew the GPU.
	var nr mycelv1.NodeSharingResponse
	if _, err := e.sn.rpc(t, "gpu-box", bus.RPCNodeSharing, &mycelv1.NodeSharingRequest{Local: &mycelv1.SharingSettings{
		Offer: &mycelv1.Capabilities{Names: []string{"docker", "ollama", "sandbox"}},
	}}, &nr); err != nil {
		t.Fatal(err)
	}
	err = approve(g.GetId())
	wantCode(t, err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), "no longer offers gpu") {
		t.Fatalf("approve after withdrawal: %v", err)
	}
	if _, err := e.sn.rpc(t, "gpu-box", bus.RPCNodeSharing, &mycelv1.NodeSharingRequest{}, &nr); err != nil {
		t.Fatal(err)
	}
	if err := approve(g.GetId()); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Withdrawing an offer on the hub ends the open grants of that kind.
	og := mustAgentGrant(t, e.w, ollamaSpec(), "", "")
	if og.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE {
		t.Fatalf("ollama: %+v", og)
	}
	if _, err := e.aliceC.SetNodeSharing(ctx, connect.NewRequest(&mycelv1.SetNodeSharingRequest{Name: "gpu-box", Settings: &mycelv1.SharingSettings{
		Offer: &mycelv1.Capabilities{Names: []string{"gpu", "sandbox"}}, When: "always",
	}})); err != nil {
		t.Fatal(err)
	}
	e.waitGrantState(t, og.GetId(), mycelv1.GrantState_GRANT_STATE_REVOKED)
	waitDetach(t, e.sn, og.GetId())
	if e.grant(t, g.GetId()).GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE {
		t.Fatal("the gpu grant ended too")
	}
	// No requests for a subject whose home channel is frozen.
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{Name: "#proj"})); err != nil {
		t.Fatal(err)
	}
	worker := mustRegister(t, e.sn, "gpu-box", &mycelv1.RegisterAgentRequest{AgentName: "worker", HomeChannel: "#proj"})
	if _, err := e.aliceC.FreezeChannel(ctx, connect.NewRequest(&mycelv1.FreezeChannelRequest{Channel: "#proj"})); err != nil {
		t.Fatal(err)
	}
	_, err = e.aliceG.RequestGrant(ctx, connect.NewRequest(&mycelv1.RequestGrantRequest{Subject: worker.id, Spec: gpuSpec(0)}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), "channel frozen") {
		t.Fatalf("request for a frozen home channel: %v", err)
	}
}
