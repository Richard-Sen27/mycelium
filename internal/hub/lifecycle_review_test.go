package hub

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// register returns the raw registration reply.
func register(t *testing.T, sn *standIn, node string, req *mycelv1.RegisterAgentRequest) (*mycelv1.RegisterAgentResponse, error) {
	t.Helper()
	if req.Harness == "" {
		req.Harness = "fake"
	}
	var reg mycelv1.RegisterAgentResponse
	_, err := sn.rpc(t, node, bus.RPCRegisterAgent, req, &reg)
	return &reg, err
}

func exited(t *testing.T, sn *standIn, node, id string, code int32, reason string) error {
	t.Helper()
	_, err := sn.rpc(t, node, bus.RPCAgentExited, &mycelv1.AgentExitedRequest{AgentId: id, ExitCode: code, Reason: reason}, &mycelv1.AgentExitedResponse{})
	return err
}

func (e *lifeEnv) countEvents(t *testing.T, channel, text string) int {
	t.Helper()
	n := 0
	for _, m := range e.list(t, e.adminCh, channel) {
		if m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_EVENT && m.GetPayload().GetText() == text {
			n++
		}
	}
	return n
}

func TestPauseIsSticky(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#sticky", nil, e.a1.id)
	spawn := &mycelv1.RegisterAgentRequest{AgentName: "w1", Parent: e.a1.id, HomeChannel: "#sticky", Budget: &mycelv1.Budget{Tokens: 1000}}
	reg, err := register(t, e.s1, "n1", spawn)
	if err != nil || reg.GetState() != "running" || !reg.GetFirstRegistration() || reg.GetPausedReason() != "" {
		t.Fatalf("first registration: %+v %v", reg, err)
	}
	id := reg.GetAgentId()
	mustUsage(t, e.s1, "n1", id, 300)

	// Operator pause survives an exit and a re-registration.
	if _, err := e.adminAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if err := exited(t, e.s1, "n1", id, 0, "finished"); err != nil {
		t.Fatal(err)
	}
	if a := getAgent(t, e.aliceAg, id); a.GetState() != "stopped" || a.GetPausedReason() != "operator" {
		t.Fatalf("after exit: %+v", a)
	}
	// A second exit report for the same run changes nothing.
	first := getAgent(t, e.aliceAg, id).GetExitedAt()
	e.waitEvent(t, "#sticky", "agent "+id+" exited (finished)")
	if err := exited(t, e.s1, "n1", id, 9, "again"); err != nil {
		t.Fatal(err)
	}
	if a := getAgent(t, e.aliceAg, id); a.GetExitCode() != 0 || a.GetExitedAt() != first {
		t.Fatalf("second exit changed the agent: %+v", a)
	}
	if n := e.countEvents(t, "#sticky", "agent "+id+" exited (again)"); n != 0 {
		t.Fatalf("second exit posted %d events", n)
	}
	reg, err = register(t, e.s1, "n1", spawn)
	if err != nil || reg.GetState() != "paused" || reg.GetPausedReason() != "operator" || reg.GetFirstRegistration() {
		t.Fatalf("re-registration: %+v %v", reg, err)
	}
	a := getAgent(t, e.aliceAg, id)
	if a.GetState() != "paused" || a.GetUsed().GetTokens() != 300 || a.GetExitedAt() != "" {
		t.Fatalf("after re-register: %+v", a)
	}
	if _, err := e.aliceAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, id, "running", "")

	// Budget pause, then operator pause: raising the budget does not resume.
	mustUsage(t, e.s1, "n1", id, 800)
	wantState(t, e.aliceAg, id, "paused", "budget: agent")
	if _, err := e.aliceAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: id, Reason: "look first"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, id, "paused", "operator: look first")
	if _, err := e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: id, Budget: &mycelv1.Budget{Tokens: 100000}})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, id, "paused", "operator: look first")
	if _, err := e.aliceAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, id, "running", "")

	// Usage reported after a stop still counts.
	if _, err := e.aliceAg.StopAgent(ctx, connect.NewRequest(&mycelv1.StopAgentRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	mustUsage(t, e.s1, "n1", id, 50)
	if a := getAgent(t, e.aliceAg, id); a.GetUsed().GetTokens() != 1150 || a.GetState() != "stopped" {
		t.Fatalf("usage after stop: %+v", a)
	}

	// The exit of a revoked agent is accepted and changes nothing.
	if _, err := e.adminAg.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if err := exited(t, e.s1, "n1", id, 4, "late"); err != nil {
		t.Fatalf("exit of revoked agent: %v", err)
	}
	if a := getAgent(t, e.adminAg, id); a.GetExitCode() == 4 || a.GetPausedReason() != "" {
		t.Fatalf("revoked agent after exit: %+v", a)
	}
}

func TestSpawnFieldsFixed(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#team", &mycelv1.ChannelPolicy{Budget: &mycelv1.Budget{Tokens: 1000}}, e.a1.id)
	e.create(t, e.aliceCh, "#other", nil, e.a1.id)
	o2 := registerAgent(t, e.s1, "n1", "o2")
	if _, err := e.aliceCh.AddMember(ctx, connect.NewRequest(&mycelv1.AddMemberRequest{Channel: "#other", Member: o2.id})); err != nil {
		t.Fatal(err)
	}
	spawn := func(name, parent, home string) (*mycelv1.RegisterAgentResponse, error) {
		return register(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: name, Parent: parent, HomeChannel: home})
	}

	// An orchestrator cannot become a worker, nor a worker an orchestrator.
	_, err := spawn("a1", o2.id, "")
	wantPrefix(t, err, "failed_precondition: agent exists as an orchestrator")
	if _, err := spawn("w", e.a1.id, "#team"); err != nil {
		t.Fatal(err)
	}
	wid := "agent:w@n1"
	_, err = spawn("w", "", "")
	wantPrefix(t, err, "failed_precondition: agent exists as a worker")
	_, err = spawn("w", o2.id, "#team")
	wantPrefix(t, err, "failed_precondition: parent is fixed")
	_, err = spawn("w", e.a1.id, "#other")
	wantPrefix(t, err, "failed_precondition: home channel is fixed")
	// An omitted home channel means unchanged.
	if reg, err := spawn("w", e.a1.id, ""); err != nil || reg.GetFirstRegistration() {
		t.Fatalf("re-register without home: %+v %v", reg, err)
	}
	if a := getAgent(t, e.aliceAg, wid); a.GetHomeChannel() != "#team" {
		t.Fatalf("home changed: %q", a.GetHomeChannel())
	}

	// Exhaust #team; the paused worker cannot escape to another channel.
	mustUsage(t, e.s1, "n1", wid, 1000)
	wantState(t, e.aliceAg, wid, "paused", "budget: channel")
	_, err = spawn("w", e.a1.id, "#other") // paused route
	wantPrefix(t, err, "failed_precondition: home channel is fixed")
	if err := exited(t, e.s1, "n1", wid, 0, "done"); err != nil {
		t.Fatal(err)
	}
	_, err = spawn("w", e.a1.id, "#other") // exit route
	wantPrefix(t, err, "failed_precondition: home channel is fixed")
	reg, err := spawn("w", e.a1.id, "#team")
	if err != nil || reg.GetState() != "paused" || reg.GetPausedReason() != "budget: channel" {
		t.Fatalf("re-register after exit: %+v %v", reg, err)
	}
	// Agents whose home is elsewhere cannot post into the exhausted
	// channel either; humans still can.
	_, err = e.a1.post(t, &mycelv1.PostRequest{Channel: "#team", Text: "more work"})
	wantPrefix(t, err, "failed_precondition: channel budget exhausted")
	if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#team", Text: "stop for now"})); err != nil {
		t.Fatalf("human post: %v", err)
	}
	e.a1.mustPost(t, &mycelv1.PostRequest{Channel: "#other", Text: "elsewhere is fine"})

	// Home membership is added on the first registration only.
	if _, err := spawn("v", e.a1.id, "#other"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.aliceCh.RemoveMember(ctx, connect.NewRequest(&mycelv1.RemoveMemberRequest{Channel: "#other", Member: "agent:v@n1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := spawn("v", e.a1.id, "#other"); err != nil {
		t.Fatal(err)
	}
	ch, err := e.aliceCh.GetChannel(ctx, connect.NewRequest(&mycelv1.GetChannelRequest{Name: "#other"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ch.Msg.GetChannel().GetMembers() {
		if m.GetId() == "agent:v@n1" {
			t.Fatal("removed worker re-added on re-registration")
		}
	}
}

func TestBudgetOnReRegister(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	req := func(tokens int64) *mycelv1.RegisterAgentRequest {
		return &mycelv1.RegisterAgentRequest{AgentName: "w", Parent: e.a1.id, Budget: &mycelv1.Budget{Tokens: tokens}}
	}
	if _, err := register(t, e.s1, "n1", req(5000)); err != nil {
		t.Fatal(err)
	}
	id := "agent:w@n1"
	if _, err := e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: id, Budget: &mycelv1.Budget{Tokens: 100}})); err != nil {
		t.Fatal(err)
	}
	setBy := func() string {
		a, err := e.srv.st.getAgent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return a.BudgetSetBy
	}
	if setBy() != "operator" {
		t.Fatalf("budget_set_by = %q", setBy())
	}
	for _, tokens := range []int64{0, 1000} {
		if _, err := register(t, e.s1, "n1", req(tokens)); err != nil {
			t.Fatal(err)
		}
		if b := getAgent(t, e.aliceAg, id).GetBudget().GetTokens(); b != 100 || setBy() != "operator" {
			t.Fatalf("re-register with %d: budget %d by %s", tokens, b, setBy())
		}
	}
	mustUsage(t, e.s1, "n1", id, 60)
	drainCtl(e.s1)
	// A stricter node budget applies, and a fresh run already over it starts
	// paused: the reply says so and an AgentCtl follows.
	reg, err := register(t, e.s1, "n1", req(50))
	if err != nil || reg.GetState() != "paused" || reg.GetPausedReason() != "budget: agent" {
		t.Fatalf("stricter budget: %+v %v", reg, err)
	}
	if b := getAgent(t, e.aliceAg, id).GetBudget().GetTokens(); b != 50 || setBy() != "node" {
		t.Fatalf("stricter budget: %d by %s", b, setBy())
	}
	waitCtl(t, e.s1, id, "pause")
}

func TestBudgetsLowerAndFlip(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.create(t, e.aliceCh, "#low", nil, e.a1.id)
	w := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w", Parent: e.a1.id, HomeChannel: "#low", Budget: &mycelv1.Budget{Tokens: 100}})
	mustUsage(t, e.s1, "n1", w.id, 90)
	// Lowering the channel budget below usage pauses at once.
	if _, err := e.aliceCh.UpdateChannelPolicy(ctx, connect.NewRequest(&mycelv1.UpdateChannelPolicyRequest{
		Name: "#low", Policy: &mycelv1.ChannelPolicy{Budget: &mycelv1.Budget{Tokens: 50}},
	})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w.id, "paused", "budget: channel")
	// Both exhausted: the reason flips as each one is lifted.
	if _, err := e.aliceCh.UpdateChannelPolicy(ctx, connect.NewRequest(&mycelv1.UpdateChannelPolicyRequest{
		Name: "#low", Policy: &mycelv1.ChannelPolicy{Budget: &mycelv1.Budget{Tokens: 150}},
	})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w.id, "running", "")
	mustUsage(t, e.s1, "n1", w.id, 110) // agent 200/100, channel 200/150
	wantState(t, e.aliceAg, w.id, "paused", "budget: agent")
	if _, err := e.aliceAg.SetAgentBudget(ctx, connect.NewRequest(&mycelv1.SetAgentBudgetRequest{Id: w.id, Budget: &mycelv1.Budget{Tokens: 10000}})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w.id, "paused", "budget: channel")
	_, err := e.aliceAg.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: w.id}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if _, err := e.aliceAg.ResetChannelUsage(ctx, connect.NewRequest(&mycelv1.ResetChannelUsageRequest{Channel: "#low"})); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.aliceAg, w.id, "running", "")
}

func TestUsageReportLimits(t *testing.T) {
	e := newLifeEnv(t)
	w := mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "w", Parent: e.a1.id})
	_, err := reportUsage(t, e.s1, "n1", w.id, maxUsageTokens+1, 0)
	wantPrefix(t, err, "invalid_argument: usage report too large")
	_, err = reportUsage(t, e.s1, "n1", w.id, 1, maxUsageUSD+1)
	wantPrefix(t, err, "invalid_argument")
	if a := getAgent(t, e.aliceAg, w.id); a.GetUsed().GetTokens() != 0 {
		t.Fatalf("refused report counted: %+v", a.GetUsed())
	}
	// 10 per second, burst 20, per node.
	limited := false
	for range 40 {
		if _, err := reportUsage(t, e.s1, "n1", w.id, 1, 0); err != nil {
			wantPrefix(t, err, "resource_exhausted")
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("usage reports not rate limited")
	}
	// Another node is not affected.
	b := mustRegister(t, e.s2, "n2", &mycelv1.RegisterAgentRequest{AgentName: "b", Parent: e.a2.id})
	mustUsage(t, e.s2, "n2", b.id, 1)
}

func TestPauseRevokeRace(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	var ids []string
	for i := range 8 {
		ids = append(ids, mustRegister(t, e.s1, "n1", &mycelv1.RegisterAgentRequest{AgentName: "r" + string(rune('a'+i)), Parent: e.a1.id}).id)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = e.adminAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: id}))
		}()
		go func() {
			defer wg.Done()
			if _, err := e.adminAg.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: id})); err != nil {
				t.Errorf("revoke %s: %v", id, err)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if a := getAgent(t, e.adminAg, id); a.GetState() != "stopped" || a.GetPausedReason() != "" {
			t.Errorf("%s after pause/revoke race: %s (%s)", id, a.GetState(), a.GetPausedReason())
		}
	}
}

// TestSilentNodeDoesNotBlock: control messages to a node that never answers
// wait in that node's queue and delay nobody else.
func TestSilentNodeDoesNotBlock(t *testing.T) {
	e := newLifeEnv(t)
	ctx := context.Background()
	e.s2.silent.Store(true)
	var ids []string
	for i := range 4 {
		ids = append(ids, mustRegister(t, e.s2, "n2", &mycelv1.RegisterAgentRequest{AgentName: "s" + string(rune('a'+i)), Parent: e.a2.id}).id)
	}
	for _, id := range ids {
		start := time.Now()
		if _, err := e.adminAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: id})); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("pause took %v", d)
		}
	}
	start := time.Now()
	var ref mycelv1.RefreshAgentTokenResponse
	if _, err := e.s1.rpc(t, "n1", bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: e.a1.id, Token: e.a1.token}, &ref); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("refresh on another node took %v", d)
	}
	drainCtl(e.s1)
	if _, err := e.adminAg.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: e.a1.id})); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-e.s1.ctl:
		if req.GetAgent().GetAgentId() != e.a1.id {
			t.Fatalf("unexpected ctl %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("ctl to a healthy node delayed by a silent one")
	}
	// The silent node still gets its messages, in order.
	for _, id := range ids {
		if c := waitCtl(t, e.s2, id, "pause"); !strings.HasPrefix(c.GetReason(), "operator") {
			t.Fatalf("reason %q", c.GetReason())
		}
	}
}
