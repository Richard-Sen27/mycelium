package hub

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// lifeEnv is a chanEnv with AgentService clients.
type lifeEnv struct {
	*chanEnv
	adminAg, aliceAg, bobAg mycelv1connect.AgentServiceClient
}

func newLifeEnv(t *testing.T) *lifeEnv {
	t.Helper()
	e := newChanEnv(t)
	return &lifeEnv{
		chanEnv: e,
		adminAg: e.h.clients(e.h.init.AdminAPIKey, nil).agents,
		aliceAg: e.h.clients(e.aliceKey, nil).agents,
		bobAg:   e.h.clients(e.bobKey, nil).agents,
	}
}

func tryRegister(t *testing.T, sn *standIn, node string, req *mycelv1.RegisterAgentRequest) (*testAgent, error) {
	t.Helper()
	if req.Harness == "" {
		req.Harness = "fake"
	}
	var reg mycelv1.RegisterAgentResponse
	if _, err := sn.rpc(t, node, bus.RPCRegisterAgent, req, &reg); err != nil {
		return nil, err
	}
	return &testAgent{id: reg.GetAgentId(), node: node, sn: sn, token: reg.GetToken()}, nil
}

func mustRegister(t *testing.T, sn *standIn, node string, req *mycelv1.RegisterAgentRequest) *testAgent {
	t.Helper()
	a, err := tryRegister(t, sn, node, req)
	if err != nil {
		t.Fatalf("register %s: %v", req.GetAgentName(), err)
	}
	return a
}

func reportUsage(t *testing.T, sn *standIn, node, id string, tokens int64, usd float64) (*mycelv1.AgentUsageResponse, error) {
	t.Helper()
	var resp mycelv1.AgentUsageResponse
	_, err := sn.rpc(t, node, bus.RPCAgentUsage, &mycelv1.AgentUsageRequest{AgentId: id, Tokens: tokens, Usd: usd, Turns: 1}, &resp)
	return &resp, err
}

func mustUsage(t *testing.T, sn *standIn, node, id string, tokens int64) *mycelv1.AgentUsageResponse {
	t.Helper()
	r, err := reportUsage(t, sn, node, id, tokens, 0)
	if err != nil {
		t.Fatalf("usage %s: %v", id, err)
	}
	return r
}

func getAgent(t *testing.T, c mycelv1connect.AgentServiceClient, id string) *mycelv1.Agent {
	t.Helper()
	r, err := c.GetAgent(context.Background(), connect.NewRequest(&mycelv1.GetAgentRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetAgent %s: %v", id, err)
	}
	return r.Msg.GetAgent()
}

func channelUsage(t *testing.T, c mycelv1connect.AgentServiceClient, name string) *mycelv1.ChannelUsage {
	t.Helper()
	r, err := c.GetChannelUsage(context.Background(), connect.NewRequest(&mycelv1.GetChannelUsageRequest{Channel: name}))
	if err != nil {
		t.Fatalf("GetChannelUsage %s: %v", name, err)
	}
	return r.Msg.GetUsage()
}

// waitCtl waits for an AgentCtl for agent with action on the stand-in node,
// skipping other control messages.
func waitCtl(t *testing.T, sn *standIn, agent, action string) *mycelv1.AgentCtl {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case req := <-sn.ctl:
			if c := req.GetAgent(); c != nil && c.GetAgentId() == agent && c.GetAction() == action {
				return c
			}
		case <-deadline:
			t.Fatalf("no AgentCtl %s for %s", action, agent)
			return nil
		}
	}
}

func drainCtl(sn *standIn) {
	for {
		select {
		case <-sn.ctl:
		default:
			return
		}
	}
}

func (e *lifeEnv) waitEvent(t *testing.T, channel, text string) {
	t.Helper()
	eventually(t, 5*time.Second, "event "+text+" in "+channel, func() bool {
		for _, m := range e.list(t, e.adminCh, channel) {
			if m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_EVENT && m.GetFrom() == "hub" && m.GetPayload().GetText() == text {
				return true
			}
		}
		return false
	})
}

func wantState(t *testing.T, c mycelv1connect.AgentServiceClient, id, state, reason string) {
	t.Helper()
	eventually(t, 5*time.Second, id+" "+state+" ("+reason+")", func() bool {
		a := getAgent(t, c, id)
		return a.GetState() == state && a.GetPausedReason() == reason
	})
}

func TestAgentRegistrationPhase3(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#proj", nil, e.a1.id)
	e.create(t, e.aliceCh, "#closed", nil)

	w := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{
		AgentName: "w1", Parent: e.a1.id, HomeChannel: "#proj", Budget: &mycelv1.Budget{Tokens: 1000, Usd: 2.5}, Workdir: "/tmp/w1",
	})
	ag := getAgent(t, e.aliceAg, w.id)
	if ag.GetParent() != e.a1.id || ag.GetHomeChannel() != "#proj" || ag.GetBudget().GetTokens() != 1000 ||
		ag.GetBudget().GetUsd() != 2.5 || ag.GetState() != "running" || ag.GetOwnerId() != e.aliceID {
		t.Fatalf("agent = %+v", ag)
	}
	// The worker is a member of its home channel and can read it at once.
	ch, err := e.aliceCh.GetChannel(ctx, connect.NewRequest(&mycelv1.GetChannelRequest{Name: "#proj"}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ch.Msg.GetChannel().GetMembers(), func(m *mycelv1.Member) bool {
		return m.GetId() == w.id && m.GetRole() == "agent"
	}) {
		t.Fatalf("worker not a member: %+v", ch.Msg.GetChannel().GetMembers())
	}
	var lm mycelv1.ListMessagesResponse
	if err := w.call(t, bus.RPCAgentListMessages, &mycelv1.AgentListMessagesRequest{Call: w.ac(), List: &mycelv1.ListMessagesRequest{Channel: "#proj"}}, &lm, 5*time.Second); err != nil {
		t.Fatalf("worker reads home channel: %v", err)
	}
	// ListAgents carries the phase 3 fields.
	la, err := e.adminAg.ListAgents(ctx, connect.NewRequest(&mycelv1.ListAgentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(la.Msg.GetAgents(), func(a *mycelv1.Agent) bool { return a.GetId() == w.id && a.GetHomeChannel() == "#proj" }) {
		t.Fatal("ListAgents misses the home channel")
	}

	refusals := []struct {
		name   string
		sn     *standIn
		node   string
		req    *mycelv1.RegisterAgentRequest
		prefix string
	}{
		{"parent on another node", e.s2, "n2", &mycelv1.RegisterAgentRequest{AgentName: "x1", Parent: e.a1.id, HomeChannel: "#proj"}, "invalid_argument: parent"},
		{"unknown parent", e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x2", Parent: "agent:ghost@n1"}, "invalid_argument: parent"},
		{"own parent", e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x3", Parent: "agent:x3@n1"}, "invalid_argument"},
		{"parent not a member", e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x4", Parent: e.a1.id, HomeChannel: "#closed"}, "failed_precondition"},
		{"missing home channel", e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x5", Parent: e.a1.id, HomeChannel: "#nope"}, "failed_precondition"},
		{"system home channel", e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x6", Parent: e.a1.id, HomeChannel: "#alerts"}, "failed_precondition"},
		{"negative budget", e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x7", Budget: &mycelv1.Budget{Tokens: -1}}, "invalid_argument"},
		{"owner not a member", e.s2, "n2", &mycelv1.RegisterAgentRequest{AgentName: "x8", HomeChannel: "#closed"}, "failed_precondition"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tryRegister(t, tc.sn, tc.node, tc.req)
			wantPrefix(t, err, tc.prefix)
		})
	}
	// Without a parent, the node owner's membership counts (alice owns #closed).
	mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "x9", HomeChannel: "#closed"})
}

func TestAgentBudgetPause(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#proj", nil, e.a1.id)
	w := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{
		AgentName: "w1", Parent: e.a1.id, HomeChannel: "#proj", Budget: &mycelv1.Budget{Tokens: 1000},
	})
	drainCtl(e.s1)

	r := mustUsage(t, e.s1, "n1", w.id, 600)
	if r.GetPaused() || r.GetAgentRemaining().GetTokens() != 400 || r.GetChannelRemaining().GetTokens() != 0 {
		t.Fatalf("first report: %+v", r)
	}
	if a := getAgent(t, e.aliceAg, w.id); a.GetUsed().GetTokens() != 600 || a.GetTurns() != 1 {
		t.Fatalf("used = %+v turns %d", a.GetUsed(), a.GetTurns())
	}
	if u := channelUsage(t, e.aliceAg, "#proj"); u.GetUsed().GetTokens() != 600 || u.GetPaused() {
		t.Fatalf("channel usage = %+v", u)
	}

	r = mustUsage(t, e.s1, "n1", w.id, 600)
	if !r.GetPaused() || r.GetAgentRemaining().GetTokens() != -200 {
		t.Fatalf("second report: %+v", r)
	}
	wantState(t, e.aliceAg, w.id, "paused", "budget: agent")
	if c := waitCtl(t, e.s1, w.id, "pause"); c.GetReason() != "budget: agent" {
		t.Fatalf("ctl reason %q", c.GetReason())
	}
	e.waitEvent(t, "#proj", "agent "+w.id+" paused: budget: agent")

	// Acting is refused; reading works.
	_, err := w.post(t, &mycelv1.PostRequest{Channel: "#proj", Text: "hi"})
	wantPrefix(t, err, "failed_precondition: agent paused (budget: agent)")
	err = w.call(t, bus.RPCAgentBBPut, &mycelv1.AgentBlackboardPutRequest{Call: w.ac(), Put: &mycelv1.BlackboardPutRequest{Channel: "#proj", Key: "k", Value: "v"}}, &mycelv1.BlackboardPutResponse{}, 5*time.Second)
	wantPrefix(t, err, "failed_precondition: agent paused")
	_, err = w.join(t, "#general", nil)
	wantPrefix(t, err, "failed_precondition: agent paused")
	start := time.Now()
	pull := w.pull(t, 7, 5000)
	if time.Since(start) > 2*time.Second || len(pull.GetMessages()) != 0 || pull.GetNextSeq() != 7 {
		t.Fatalf("paused pull: %v, %+v", time.Since(start), pull)
	}
	if err := w.call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{Call: w.ac()}, &mycelv1.AgentWhoamiResponse{}, 5*time.Second); err != nil {
		t.Fatalf("whoami while paused: %v", err)
	}
	if err := w.call(t, bus.RPCAgentBBGet, &mycelv1.AgentBlackboardGetRequest{Call: w.ac(), Get: &mycelv1.BlackboardGetRequest{Channel: "#proj"}}, &mycelv1.BlackboardGetResponse{}, 5*time.Second); err != nil {
		t.Fatalf("bb get while paused: %v", err)
	}
	var ref mycelv1.RefreshAgentTokenResponse
	if _, err := e.s1.rpc(t, "n1", bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: w.id, Token: w.token}, &ref); err != nil {
		t.Fatalf("refresh while paused: %v", err)
	}

	// Resume is refused while the budget is exhausted.
	_, err = e.aliceAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: w.id}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), "budget still exhausted") {
		t.Fatalf("resume error %v", err)
	}
	// A higher budget resumes it.
	sb, err := e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: w.id, Budget: &mycelv1.Budget{Tokens: 5000}}))
	if err != nil {
		t.Fatal(err)
	}
	if sb.Msg.GetAgent().GetState() != "running" || sb.Msg.GetAgent().GetPausedReason() != "" {
		t.Fatalf("after raise: %+v", sb.Msg.GetAgent())
	}
	waitCtl(t, e.s1, w.id, "resume")
	w.mustPost(t, &mycelv1.PostRequest{Channel: "#proj", Text: "back"})
	// A budget below usage pauses it at once.
	if _, err := e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: w.id, Budget: &mycelv1.Budget{Tokens: 100}})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w.id, "paused", "budget: agent")
	// Bad budgets and bad reports.
	_, err = e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: w.id, Budget: &mycelv1.Budget{Usd: -1}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = reportUsage(t, e.s1, "n1", w.id, -5, 0)
	wantPrefix(t, err, "invalid_argument")
	// The USD budget works the same way.
	u := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w2", Budget: &mycelv1.Budget{Usd: 1}})
	if r, err := reportUsage(t, e.s1, "n1", u.id, 10, 1); err != nil || !r.GetPaused() || r.GetAgentRemaining().GetUsd() >= 0 {
		t.Fatalf("usd report: %+v %v", r, err)
	}
	wantState(t, e.aliceAg, u.id, "paused", "budget: agent")
	// Without a home channel the event goes to #alerts.
	e.waitEvent(t, "#alerts", "agent "+u.id+" paused: budget: agent")
}

func TestChannelBudgetPause(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#team", &mycelv1.ChannelPolicy{Budget: &mycelv1.Budget{Tokens: 1500}}, e.a1.id)
	e.create(t, e.aliceCh, "#other", nil, e.a1.id)
	w2 := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w2", Parent: e.a1.id, HomeChannel: "#team"})
	w3 := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w3", Parent: e.a1.id, HomeChannel: "#team"})
	w4 := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w4", Parent: e.a1.id, HomeChannel: "#other"})
	drainCtl(e.s1)

	if r := mustUsage(t, e.s1, "n1", w2.id, 800); r.GetPaused() || r.GetChannelRemaining().GetTokens() != 700 {
		t.Fatalf("w2 report: %+v", r)
	}
	mustUsage(t, e.s1, "n1", w4.id, 5000)
	r := mustUsage(t, e.s1, "n1", w3.id, 800)
	if !r.GetPaused() || r.GetChannelRemaining().GetTokens() != -100 {
		t.Fatalf("w3 report: %+v", r)
	}
	wantState(t, e.aliceAg, w2.id, "paused", "budget: channel")
	wantState(t, e.aliceAg, w3.id, "paused", "budget: channel")
	wantState(t, e.aliceAg, w4.id, "running", "")
	waitCtl(t, e.s1, w2.id, "pause")
	e.waitEvent(t, "#team", "agent "+w3.id+" paused: budget: channel")
	if u := channelUsage(t, e.aliceAg, "#team"); !u.GetPaused() || u.GetUsed().GetTokens() != 1600 || u.GetBudget().GetTokens() != 1500 {
		t.Fatalf("usage = %+v", u)
	}
	// The channel is remembered as exhausted: new workers are refused.
	_, err := tryRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w5", Parent: e.a1.id, HomeChannel: "#team"})
	wantPrefix(t, err, "failed_precondition")
	// Only the channel owner (or an admin) sees and resets usage.
	_, err = e.bobAg.GetChannelUsage(ctx, connect.NewRequest(&mycelv1.GetChannelUsageRequest{Channel: "#team"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.bobAg.ResetChannelUsage(ctx, connect.NewRequest(&mycelv1.ResetChannelUsageRequest{Channel: "#team"}))
	wantCode(t, err, connect.CodePermissionDenied)

	drainCtl(e.s1)
	rr, err := e.aliceAg.ResetChannelUsage(ctx, connect.NewRequest(&mycelv1.ResetChannelUsageRequest{Channel: "#team"}))
	if err != nil {
		t.Fatal(err)
	}
	if rr.Msg.GetUsage().GetUsed().GetTokens() != 0 || rr.Msg.GetUsage().GetPaused() {
		t.Fatalf("after reset: %+v", rr.Msg.GetUsage())
	}
	wantState(t, e.aliceAg, w2.id, "running", "")
	wantState(t, e.aliceAg, w3.id, "running", "")
	waitCtl(t, e.s1, w2.id, "resume")
	waitCtl(t, e.s1, w3.id, "resume")

	// Exhaust again; raising the channel budget resumes them.
	mustUsage(t, e.s1, "n1", w2.id, 1600)
	wantState(t, e.aliceAg, w3.id, "paused", "budget: channel")
	if _, err := e.aliceCh.UpdateChannelPolicy(ctx, connect.NewRequest(&mycelv1.UpdateChannelPolicyRequest{
		Name: "#team", Policy: &mycelv1.ChannelPolicy{Budget: &mycelv1.Budget{Tokens: 10000}},
	})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w2.id, "running", "")
	wantState(t, e.aliceAg, w3.id, "running", "")
	// An agent paused for its own budget is not resumed by a channel reset.
	if _, err := e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: w2.id, Budget: &mycelv1.Budget{Tokens: 100}})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w2.id, "paused", "budget: agent")
	if _, err := e.adminAg.ResetChannelUsage(ctx, connect.NewRequest(&mycelv1.ResetChannelUsageRequest{Channel: "#team"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w2.id, "paused", "budget: agent")
}

func TestAgentOperatorControl(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#proj", nil, e.a1.id)
	w := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w1", Parent: e.a1.id, HomeChannel: "#proj"})
	drainCtl(e.s1)

	// Another member may neither see nor control alice's agent.
	_, err := e.bobAg.GetAgent(ctx, connect.NewRequest(&mycelv1.GetAgentRequest{Id: w.id}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.bobAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: w.id}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.bobAg.StopAgent(ctx, connect.NewRequest(&mycelv1.StopAgentRequest{Id: w.id}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.bobAg.GetAgent(ctx, connect.NewRequest(&mycelv1.GetAgentRequest{Id: "agent:ghost@n1"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.adminAg.GetAgent(ctx, connect.NewRequest(&mycelv1.GetAgentRequest{Id: "agent:ghost@n1"}))
	wantCode(t, err, connect.CodeNotFound)

	// The owner pauses and resumes.
	p, err := e.aliceAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: w.id}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Msg.GetAgent().GetState() != "paused" || p.Msg.GetAgent().GetPausedReason() != "operator" {
		t.Fatalf("pause: %+v", p.Msg.GetAgent())
	}
	waitCtl(t, e.s1, w.id, "pause")
	e.waitEvent(t, "#proj", "agent "+w.id+" paused: operator")
	_, err = w.post(t, &mycelv1.PostRequest{Channel: "#proj", Text: "x"})
	wantPrefix(t, err, "failed_precondition: agent paused (operator)")
	if _, err := e.aliceAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: w.id})); err != nil {
		t.Fatal(err)
	}
	waitCtl(t, e.s1, w.id, "resume")
	w.mustPost(t, &mycelv1.PostRequest{Channel: "#proj", Text: "running"})

	// An admin may control anyone's agent.
	if _, err := e.adminAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: e.a2.id, Reason: "investigating"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.bobAg, e.a2.id, "paused", "operator: investigating")
	waitCtl(t, e.s2, e.a2.id, "pause")
	if _, err := e.adminAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: e.a2.id})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.bobAg, e.a2.id, "running", "")

	// Usage and exit reports from the wrong node are refused.
	_, err = reportUsage(t, e.s2, "n2", w.id, 10, 0)
	wantPrefix(t, err, "permission_denied: agent not registered by this node")
	_, err = e.s2.rpc(t, "n2", bus.RPCAgentExited, &mycelv1.AgentExitedRequest{AgentId: w.id}, &mycelv1.AgentExitedResponse{})
	wantPrefix(t, err, "permission_denied")
	if a := getAgent(t, e.aliceAg, w.id); a.GetUsed().GetTokens() != 0 || a.GetState() != "running" {
		t.Fatalf("wrong-node report changed the agent: %+v", a)
	}

	// Stop: no more refreshes or RPCs.
	if _, err := e.aliceAg.StopAgent(ctx, connect.NewRequest(&mycelv1.StopAgentRequest{Id: w.id})); err != nil {
		t.Fatal(err)
	}
	waitCtl(t, e.s1, w.id, "stop")
	wantState(t, e.aliceAg, w.id, "stopped", "")
	e.waitEvent(t, "#proj", "agent "+w.id+" stopped by "+e.aliceID)
	_, err = e.s1.rpc(t, "n1", bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: w.id, Token: w.token}, &mycelv1.RefreshAgentTokenResponse{})
	wantPrefix(t, err, "permission_denied: agent stopped")
	err = w.call(t, bus.RPCAgentWhoami, &mycelv1.AgentWhoamiRequest{Call: w.ac()}, &mycelv1.AgentWhoamiResponse{}, 5*time.Second)
	wantPrefix(t, err, "permission_denied: agent stopped")
	_, err = e.aliceAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: w.id}))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// Exit report: fields, state and an EVENT in the home channel.
	x := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w2", Parent: e.a1.id, HomeChannel: "#proj"})
	if _, err := e.s1.rpc(t, "n1", bus.RPCAgentExited, &mycelv1.AgentExitedRequest{AgentId: x.id, ExitCode: 3, Reason: "finished"}, &mycelv1.AgentExitedResponse{}); err != nil {
		t.Fatal(err)
	}
	a := getAgent(t, e.aliceAg, x.id)
	if a.GetState() != "stopped" || a.GetExitCode() != 3 || a.GetExitedAt() == "" {
		t.Fatalf("after exit: %+v", a)
	}
	e.waitEvent(t, "#proj", "agent "+x.id+" exited (finished)")
	// Registering again starts a new run.
	x = mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w2", Parent: e.a1.id, HomeChannel: "#proj"})
	if a := getAgent(t, e.aliceAg, x.id); a.GetState() != "running" || a.GetExitedAt() != "" {
		t.Fatalf("after re-register: %+v", a)
	}
	// A paused agent stays paused when its node registers it again.
	if _, err := e.aliceAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: x.id})); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w2", Parent: e.a1.id, HomeChannel: "#proj"})
	wantState(t, e.aliceAg, x.id, "paused", "operator")
}

// TestAgentsMigration opens a database whose agents table predates phase 3.
func TestAgentsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE agents (id TEXT PRIMARY KEY, name TEXT NOT NULL, node_name TEXT NOT NULL,
		sandbox_id TEXT NOT NULL, owner_id TEXT NOT NULL, harness TEXT NOT NULL, state TEXT NOT NULL,
		created_at TEXT NOT NULL, token_expires_at TEXT, revoked_at TEXT);
		INSERT INTO agents VALUES('agent:old@n', 'old', 'n', '', 'usr_x', 'fake', 'running', '2026-01-01T00:00:00Z', NULL, NULL);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for i := 0; i < 2; i++ { // idempotent
		st, err := openStore(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		a, err := st.getAgent(context.Background(), "agent:old@n")
		if err != nil || a.State != "running" || a.HomeChannel != "" || a.BudgetTokens != 0 || a.ExitedAt != "" {
			t.Fatalf("migrated agent: %+v %v", a, err)
		}
		_ = st.Close()
	}
}
