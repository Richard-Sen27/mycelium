package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	mcpadapter "github.com/richard-sen27/mycelium/internal/adapters/mcp"
	"github.com/richard-sen27/mycelium/internal/config"
)

// Phase-3 AgentService and ChannelService methods of the Connect fake.

func testAgent(id string) *mycelv1.Agent {
	return &mycelv1.Agent{
		Id: id, Name: "coder-w1", NodeName: "mac", OwnerId: "usr_1", Harness: "claude-code", State: "running",
		Parent: "agent:coder@mac", HomeChannel: "#task-1", Budget: &mycelv1.Budget{Tokens: 1000, Usd: 2},
		Used: &mycelv1.Budget{Tokens: 300, Usd: 0.25}, Turns: 4, CreatedAt: "2026-10-28T14:00:00Z",
	}
}

func (f *fakeChannels) GetAgent(_ context.Context, r *connect.Request[mycelv1.GetAgentRequest]) (*connect.Response[mycelv1.GetAgentResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetAgentResponse{Agent: testAgent(r.Msg.GetId())}), nil
}

func (f *fakeChannels) SetAgentBudget(_ context.Context, r *connect.Request[mycelv1.SetAgentBudgetRequest]) (*connect.Response[mycelv1.SetAgentBudgetResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	ag := testAgent(r.Msg.GetId())
	ag.Budget = r.Msg.GetBudget()
	return connect.NewResponse(&mycelv1.SetAgentBudgetResponse{Agent: ag}), nil
}

func (f *fakeChannels) PauseAgent(_ context.Context, r *connect.Request[mycelv1.PauseAgentRequest]) (*connect.Response[mycelv1.PauseAgentResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	ag := testAgent(r.Msg.GetId())
	ag.State, ag.PausedReason = "paused", r.Msg.GetReason()
	return connect.NewResponse(&mycelv1.PauseAgentResponse{Agent: ag}), nil
}

func (f *fakeChannels) ResumeAgent(_ context.Context, r *connect.Request[mycelv1.ResumeAgentRequest]) (*connect.Response[mycelv1.ResumeAgentResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ResumeAgentResponse{Agent: testAgent(r.Msg.GetId())}), nil
}

func (f *fakeChannels) StopAgent(_ context.Context, r *connect.Request[mycelv1.StopAgentRequest]) (*connect.Response[mycelv1.StopAgentResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.StopAgentResponse{}), nil
}

func testUsage(ch string) *mycelv1.ChannelUsage {
	return &mycelv1.ChannelUsage{Channel: ch, Budget: &mycelv1.Budget{Usd: 5}, Used: &mycelv1.Budget{Tokens: 900, Usd: 5.5}, Paused: true}
}

func (f *fakeChannels) GetChannelUsage(_ context.Context, r *connect.Request[mycelv1.GetChannelUsageRequest]) (*connect.Response[mycelv1.GetChannelUsageResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetChannelUsageResponse{Usage: testUsage(r.Msg.GetChannel())}), nil
}

func (f *fakeChannels) ResetChannelUsage(_ context.Context, r *connect.Request[mycelv1.ResetChannelUsageRequest]) (*connect.Response[mycelv1.ResetChannelUsageResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	u := testUsage(r.Msg.GetChannel())
	u.Used, u.Paused = &mycelv1.Budget{}, false
	return connect.NewResponse(&mycelv1.ResetChannelUsageResponse{Usage: u}), nil
}

func (f *fakeChannels) UpdateChannelPolicy(_ context.Context, r *connect.Request[mycelv1.UpdateChannelPolicyRequest]) (*connect.Response[mycelv1.UpdateChannelPolicyResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	c := testChannel(r.Msg.GetName())
	c.Policy = r.Msg.GetPolicy()
	return connect.NewResponse(&mycelv1.UpdateChannelPolicyResponse{Channel: c}), nil
}

func TestPhase3HubCommands(t *testing.T) {
	// testChannel's policy, which `channels policy` starts from.
	base := func() *mycelv1.ChannelPolicy {
		return &mycelv1.ChannelPolicy{MaxHops: 4, RatePerMinute: 60, OpenToMembers: true, AgentsMayPost: true, HumansMayPost: true}
	}
	withBudget := base()
	withBudget.RatePerMinute, withBudget.Budget = 10, &mycelv1.Budget{Usd: 5}
	closed := base()
	closed.OpenToMembers, closed.MaxHops = false, 6

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantErr  []string
		checkReq func(t *testing.T, m proto.Message)
		jsonLine bool
	}{
		{
			name:    "agents ls shows parent, state reason and usage",
			args:    []string{"agents", "ls"},
			wantOut: []string{"PARENT", "STATE", "USED", "agent:coder-w1@mac", "agent:coder@mac", "paused (budget: agent)", "1200/$0.0123"},
		},
		{
			name:     "agents get",
			args:     []string{"agents", "get", "coder-w1@mac"},
			wantOut:  []string{"ID:", "agent:coder-w1@mac", "Parent:", "agent:coder@mac", "Home channel:", "#task-1", "Budget:", "1000 tokens, $2.0000", "Used:", "300 tokens, $0.2500", "Turns:"},
			checkReq: wantReq(&mycelv1.GetAgentRequest{Id: "agent:coder-w1@mac"}),
		},
		{
			name:     "agents get json",
			args:     []string{"--json", "agents", "get", "agent:coder-w1@mac"},
			wantOut:  []string{`"parent":"agent:coder@mac"`, `"budget":{"tokens":"1000","usd":2}`},
			jsonLine: true,
		},
		{
			name:     "agents budget keeps the flag not given",
			args:     []string{"agents", "budget", "agent:coder-w1@mac", "--tokens", "500"},
			wantOut:  []string{"Budget of agent:coder-w1@mac: 500 tokens, $2.0000 (used 300 tokens, $0.2500)."},
			checkReq: wantReq(&mycelv1.SetAgentBudgetRequest{Id: "agent:coder-w1@mac", Budget: &mycelv1.Budget{Tokens: 500, Usd: 2}}),
		},
		{
			name:     "agents budget zero means unlimited",
			args:     []string{"agents", "budget", "agent:coder-w1@mac", "--tokens", "0", "--usd", "0"},
			wantOut:  []string{"Budget of agent:coder-w1@mac: unlimited"},
			checkReq: wantReq(&mycelv1.SetAgentBudgetRequest{Id: "agent:coder-w1@mac", Budget: &mycelv1.Budget{}}),
		},
		{name: "agents budget without flags", args: []string{"agents", "budget", "agent:x@y"}, wantCode: 2, wantErr: []string{"--tokens"}},
		{name: "agents budget negative", args: []string{"agents", "budget", "agent:x@y", "--usd", "-1"}, wantCode: 2},
		{
			name:     "agents pause",
			args:     []string{"agents", "pause", "agent:coder-w1@mac", "--reason", "too chatty"},
			wantOut:  []string{"Paused agent:coder-w1@mac."},
			checkReq: wantReq(&mycelv1.PauseAgentRequest{Id: "agent:coder-w1@mac", Reason: "too chatty"}),
		},
		{
			name:     "agents resume",
			args:     []string{"agents", "resume", "agent:coder-w1@mac"},
			wantOut:  []string{"Resumed agent:coder-w1@mac."},
			checkReq: wantReq(&mycelv1.ResumeAgentRequest{Id: "agent:coder-w1@mac"}),
		},
		{
			name:     "agents stop json",
			args:     []string{"--json", "agents", "stop", "agent:coder-w1@mac"},
			wantOut:  []string{`{}`},
			jsonLine: true,
			checkReq: wantReq(&mycelv1.StopAgentRequest{Id: "agent:coder-w1@mac"}),
		},
		{name: "agents stop without id", args: []string{"agents", "stop"}, wantCode: 2},
		{
			name:     "channels usage",
			args:     []string{"channels", "usage", "task-1"},
			wantOut:  []string{"Channel:", "#task-1", "Budget:", "$5.0000", "Used:", "900 tokens, $5.5000", "Paused:", "yes"},
			checkReq: wantReq(&mycelv1.GetChannelUsageRequest{Channel: "#task-1"}),
		},
		{
			name:     "channels reset-usage",
			args:     []string{"channels", "reset-usage", "#task-1"},
			wantOut:  []string{"Reset the usage of #task-1."},
			checkReq: wantReq(&mycelv1.ResetChannelUsageRequest{Channel: "#task-1"}),
		},
		{
			name:     "channels reset-usage json",
			args:     []string{"--json", "channels", "reset-usage", "#task-1"},
			wantOut:  []string{`"paused":false`},
			jsonLine: true,
		},
		{
			name:    "channels create with budget",
			args:    []string{"channels", "create", "#task-2", "--type", "task", "--budget-tokens", "5000", "--budget-usd", "1.5"},
			wantOut: []string{"Created task channel #task-2."},
			checkReq: wantReq(&mycelv1.CreateChannelRequest{
				Name: "#task-2", Type: mycelv1.ChannelType_CHANNEL_TYPE_TASK,
				Policy: &mycelv1.ChannelPolicy{AgentsMayPost: true, HumansMayPost: true, Budget: &mycelv1.Budget{Tokens: 5000, Usd: 1.5}},
			}),
		},
		{
			name:     "channels policy changes only the given flags",
			args:     []string{"channels", "policy", "#backend", "--budget-usd", "5", "--rate", "10"},
			wantOut:  []string{"Updated the policy of #backend: max hops 4, rate 10/min, open yes, budget $5.0000."},
			checkReq: wantReq(&mycelv1.UpdateChannelPolicyRequest{Name: "#backend", Policy: withBudget}),
		},
		{
			name:     "channels policy closes and sets hops",
			args:     []string{"channels", "policy", "backend", "--open=false", "--max-hops", "6"},
			checkReq: wantReq(&mycelv1.UpdateChannelPolicyRequest{Name: "#backend", Policy: closed}),
		},
		{name: "channels policy without flags", args: []string{"channels", "policy", "#backend"}, wantCode: 2, wantErr: []string{"at least one"}},
		{name: "channels policy negative", args: []string{"channels", "policy", "#backend", "--rate", "-1"}, wantCode: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.writeConfig(t)
			res := runCLI(t, "", tt.args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			for _, s := range tt.wantOut {
				if !strings.Contains(res.stdout, s) {
					t.Errorf("stdout missing %q:\n%s", s, res.stdout)
				}
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(res.stderr, s) {
					t.Errorf("stderr missing %q:\n%s", s, res.stderr)
				}
			}
			if tt.jsonLine {
				lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
				if len(lines) != 1 || !json.Valid([]byte(lines[0])) {
					t.Errorf("stdout is not one JSON line: %s", res.stdout)
				}
			}
			if tt.checkReq != nil {
				tt.checkReq(t, env.hub.lastRequest())
			}
		})
	}
}

func TestAgentSpawn(t *testing.T) {
	const tok = "tok123"
	setup := func(t *testing.T) (*localFake, string) {
		t.Helper()
		t.Setenv(config.EnvHome, t.TempDir())
		t.Setenv(EnvLocalAddr, "")
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		return f, srv.URL
	}

	sessName := "cli-" + strconv.Itoa(os.Getpid())
	okFlow := []string{"POST /api/local/agents", "POST /api/local/agents/tmp/rpc/agent.channel.join", "POST /api/local/agents/tmp/spawn", "DELETE /api/local/agents/tmp"}

	t.Run("fake refuses a spawn by a non-member", func(t *testing.T) {
		_, url := setup(t)
		c, err := mcpadapter.NewClient(url, tok)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Spawn(context.Background(), "tmp", mcpadapter.SpawnRequest{Harness: "fake", Task: "t", Channel: "#x"})
		if err == nil || !strings.Contains(err.Error(), "not a member of #x") {
			t.Fatalf("spawn without join = %v, want a membership refusal", err)
		}
	})
	t.Run("registers, joins, spawns, deletes its session", func(t *testing.T) {
		f, url := setup(t)
		wd := t.TempDir()
		res := runCLI(t, "", "agent", "spawn", "claude-code", "--task", "fix the flaky test", "--channel", "task-1",
			"--name", "w1", "--workdir", wd, "--max-turns", "12", "--budget-tokens", "1000", "--budget-usd", "0.5", "--local-addr", url)
		if res.code != 0 || res.stdout != "agent:cli-w1@mac\n" {
			t.Fatalf("spawn: %+v", res)
		}
		if got := f.log(); strings.Join(got, "|") != strings.Join(okFlow, "|") {
			t.Fatalf("requests = %v, want %v (join before spawn)", got, okFlow)
		}
		f.mu.Lock()
		reg, join, spawn := f.bodies[0], f.bodies[1], f.bodies[2]
		f.mu.Unlock()
		jsonEqual := func(got, want string) {
			t.Helper()
			var g, w any
			_ = json.Unmarshal([]byte(got), &g)
			_ = json.Unmarshal([]byte(want), &w)
			gb, _ := json.Marshal(g)
			wb, _ := json.Marshal(w)
			if string(gb) != string(wb) {
				t.Errorf("body = %s, want %s", gb, wb)
			}
		}
		jsonEqual(reg, `{"name":"`+sessName+`","harness":"cli"}`)
		jsonEqual(join, `{"channel":"#task-1"}`)
		wdJSON, _ := json.Marshal(wd)
		jsonEqual(spawn, `{"harness":"claude-code","name":"w1","task":"fix the flaky test","channel":"#task-1","profile":"trusted-host",
			"workdir":`+string(wdJSON)+`,"max_turns":12,"budget":{"tokens":1000,"usd":0.5}}`)
	})
	t.Run("workdir defaults to the current directory, json output", func(t *testing.T) {
		f, url := setup(t)
		res := runCLI(t, "", "--json", "agent", "spawn", "fake", "--task", "t", "--channel", "#x", "--local-addr", url)
		if res.code != 0 || res.stdout != `{"agent_id":"agent:cli-w1@mac","channel":"#x","parent":"agent:`+sessName+`@mac"}`+"\n" {
			t.Fatalf("spawn: %+v", res)
		}
		cwd, _ := os.Getwd()
		f.mu.Lock()
		body := f.bodies[2]
		f.mu.Unlock()
		var got struct{ Workdir string }
		_ = json.Unmarshal([]byte(body), &got)
		if got.Workdir != cwd {
			t.Errorf("workdir = %q, want %q", got.Workdir, cwd)
		}
	})
	t.Run("daemon error still deletes the session", func(t *testing.T) {
		f, url := setup(t)
		f.spawnErr = true
		res := runCLI(t, "", "agent", "spawn", "nope", "--task", "t", "--channel", "#x", "--local-addr", url)
		if res.code != 1 || !strings.Contains(res.stderr, "unknown harness nope") {
			t.Fatalf("spawn: %+v", res)
		}
		if got := f.log(); strings.Join(got, "|") != strings.Join(okFlow, "|") {
			t.Errorf("requests = %v", got)
		}
	})
	t.Run("refused join is reported, no spawn, session deleted", func(t *testing.T) {
		f, url := setup(t)
		f.joinErr = true
		res := runCLI(t, "", "agent", "spawn", "fake", "--task", "t", "--channel", "#closed", "--local-addr", url)
		if res.code != 1 || !strings.Contains(res.stderr, "join #closed as agent:"+sessName+"@mac") || !strings.Contains(res.stderr, "not open to members") {
			t.Fatalf("spawn: %+v", res)
		}
		want := []string{"POST /api/local/agents", "POST /api/local/agents/tmp/rpc/agent.channel.join", "DELETE /api/local/agents/tmp"}
		if got := f.log(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("requests = %v, want %v", got, want)
		}
	})
	t.Run("interrupt after registering still deletes the session", func(t *testing.T) {
		setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		deleted := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body) // the server notices a client abort only after the body is read
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/local/agents":
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"agent_id":"agent:cli-1@mac","session":"s9"}`))
			case strings.HasSuffix(r.URL.Path, "/rpc/agent.channel.join"):
				cancel() // Ctrl-C while the join is in flight
				<-r.Context().Done()
			case r.Method == http.MethodDelete:
				deleted <- r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()
		ta := newTestApp(t, "")
		ta.baseCtx = ctx
		if res := ta.run("agent", "spawn", "fake", "--task", "t", "--channel", "#x", "--local-addr", srv.URL); res.code == 0 {
			t.Fatalf("spawn succeeded after an interrupt: %+v", res)
		}
		select {
		case p := <-deleted:
			if p != "/api/local/agents/s9" {
				t.Errorf("deleted %s", p)
			}
		default:
			t.Error("the session was not deleted after the interrupt")
		}
	})
	t.Run("MYCEL_LOCAL_ADDR is the default address", func(t *testing.T) {
		f, url := setup(t)
		t.Setenv(EnvLocalAddr, url)
		if res := runCLI(t, "", "agent", "spawn", "fake", "--task", "t", "--channel", "#x"); res.code != 0 {
			t.Fatalf("spawn: %+v", res)
		}
		if len(f.log()) != len(okFlow) {
			t.Errorf("requests = %v", f.log())
		}
		t.Setenv(EnvLocalAddr, "10.0.0.1:7777")
		if res := runCLI(t, "", "workers", "ls"); res.code != 2 || !strings.Contains(res.stderr, "$MYCEL_LOCAL_ADDR") {
			t.Errorf("bad env address: %+v", res)
		}
	})
	for _, args := range [][]string{
		{"agent", "spawn", "fake", "--channel", "#x"},
		{"agent", "spawn", "fake", "--task", "t"},
		{"agent", "spawn", "--task", "t", "--channel", "#x"},
		{"agent", "spawn", "fake", "--task", "t", "--channel", "#x", "--name", "Bad Name"},
		{"agent", "spawn", "fake", "--task", "t", "--channel", "#x", "--max-turns", "-1"},
		{"agent", "spawn", "fake", "--task", strings.Repeat("x", 16<<10+1), "--channel", "#x"},
		{"agent", "spawn", "fake", "--task", "t", "--channel", "#x", "--workdir", "/does/not/exist"},
	} {
		t.Run("usage "+strings.Join(args[2:], " "), func(t *testing.T) {
			f, url := setup(t)
			if res := runCLI(t, "", append(args, "--local-addr", url)...); res.code != 2 {
				t.Errorf("code %d, want 2: %+v", res.code, res)
			}
			if got := f.log(); len(got) != 0 {
				t.Errorf("requests = %v, want none", got)
			}
		})
	}
}

func TestWorkersCommands(t *testing.T) {
	const tok = "tok123"
	t.Setenv(config.EnvHome, t.TempDir())
	t.Setenv(EnvLocalAddr, "")
	writeLocalToken(t, tok)
	f, srv := newLocalFake(t, tok)
	f.workers = `{"workers":[{"agent_id":"agent:coder-w1@mac","parent":"agent:coder@mac","harness":"claude-code","state":"running","pid":4242,
		"started_at":"2026-10-28T14:58:00Z","usage":{"tokens":1200,"usd":0.0123,"turns":3}},
		{"agent_id":"agent:coder-w2@mac","parent":"agent:coder@mac","harness":"fake","state":"exited","started_at":"2026-10-28T14:00:00Z",
		"exited_at":"2026-10-28T14:59:00Z","exit_code":1,"usage":{"tokens":0,"usd":0,"turns":0}}]}`

	res := runCLI(t, "", "workers", "ls", "--local-addr", srv.URL)
	for _, w := range []string{"AGENT", "PARENT", "PID", "USED", "TURNS", "agent:coder-w1@mac", "4242", "1200/$0.0123", "2m ago",
		"agent:coder-w2@mac", "exited", "1m ago (code 1)"} {
		if !strings.Contains(res.stdout, w) {
			t.Errorf("workers ls missing %q: %+v", w, res)
		}
	}
	res = runCLI(t, "", "--json", "workers", "ls", "--local-addr", srv.URL)
	if res.code != 0 || !strings.HasPrefix(res.stdout, `{"workers":[{"agent_id":"agent:coder-w1@mac","parent":"agent:coder@mac"`) {
		t.Errorf("workers ls --json: %+v", res)
	}
	f.workers = `{}`
	if res := runCLI(t, "", "--json", "workers", "ls", "--local-addr", srv.URL); res.stdout != `{"workers":[]}`+"\n" {
		t.Errorf("empty workers ls --json: %+v", res)
	}

	res = runCLI(t, "", "workers", "stop", "coder-w1@mac", "--local-addr", srv.URL)
	if res.code != 0 || res.stdout != "Stopped agent:coder-w1@mac.\n" {
		t.Errorf("workers stop: %+v", res)
	}
	got := f.log()
	if got[len(got)-1] != "DELETE /api/local/workers/agent:coder-w1@mac" {
		t.Errorf("requests = %v", got)
	}
	if res := runCLI(t, "", "workers", "stop"); res.code != 2 {
		t.Errorf("workers stop without agent: %+v", res)
	}
}

func TestSetupInstructionsOnly(t *testing.T) {
	dir := t.TempDir()
	res := runCLI(t, "", "agent", "setup", "codex", "--agent", "coder", "--dir", dir)
	if res.code != 0 || !strings.Contains(res.stdout, "nothing was written") ||
		!strings.Contains(res.stdout, "codex mcp add mycel -- mycel mcp --agent coder --harness codex") {
		t.Fatalf("setup codex: %+v", res)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("setup codex wrote %v", entries)
	}
	res = runCLI(t, "", "--json", "agent", "setup", "codex", "--agent", "coder", "--dir", dir)
	var out struct {
		Harness      string
		Files        []any
		Instructions string
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil || out.Harness != "codex" || out.Files == nil || len(out.Files) != 0 ||
		!strings.Contains(out.Instructions, `args = ["mcp", "--agent", "coder", "--harness", "codex"]`) {
		t.Errorf("setup codex --json: %+v (%v)", res, err)
	}
}

func TestSetupGemini(t *testing.T) {
	dir := t.TempDir()
	res := runCLI(t, "", "agent", "setup", "gemini-cli", "--agent", "coder", "--dir", dir)
	if res.code != 0 {
		t.Fatalf("setup gemini-cli: %+v", res)
	}
	b, err := os.ReadFile(dir + "/.gemini/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	m := doc.MCPServers["mycel"]
	if m.Command != "mycel" || strings.Join(m.Args, " ") != "mcp --agent coder --harness gemini-cli" {
		t.Errorf("settings.json = %s", b)
	}
}
