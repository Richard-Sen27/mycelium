package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/config"
)

// fakeChannels implements ChannelService and AgentService for CLI tests; it
// shares the auth check and request recording of fakeHub.
type fakeChannels struct {
	mycelv1connect.UnimplementedChannelServiceHandler
	mycelv1connect.UnimplementedAgentServiceHandler
	hub *fakeHub

	mu        sync.Mutex
	watchAuth string
	extraText string // when set, ListMessages also returns a message with this text
}

func testMsg(seq uint64, text string) *mycelv1.Message {
	return &mycelv1.Message{
		Id: "msg_" + text, Channel: "#backend", From: "agent:coder@mac", Ts: "2026-10-28T14:00:00Z", Seq: seq,
		Payload: &mycelv1.Payload{Text: text},
		Meta:    &mycelv1.Meta{Provenance: &mycelv1.Provenance{Untrusted: true}},
	}
}

func testChannel(name string) *mycelv1.Channel {
	return &mycelv1.Channel{
		Name: name, Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC, OwnerId: "usr_1",
		Policy:  &mycelv1.ChannelPolicy{MaxHops: 4, RatePerMinute: 60, OpenToMembers: true, AgentsMayPost: true, HumansMayPost: true},
		Members: []*mycelv1.Member{{Id: "user:usr_1", Role: "owner"}, {Id: "agent:coder@mac", Role: "agent"}},
	}
}

func (f *fakeChannels) CreateChannel(_ context.Context, r *connect.Request[mycelv1.CreateChannelRequest]) (*connect.Response[mycelv1.CreateChannelResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	c := testChannel(r.Msg.GetName())
	c.Type = r.Msg.GetType()
	return connect.NewResponse(&mycelv1.CreateChannelResponse{Channel: c}), nil
}

func (f *fakeChannels) ListChannels(_ context.Context, r *connect.Request[mycelv1.ListChannelsRequest]) (*connect.Response[mycelv1.ListChannelsResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListChannelsResponse{Channels: []*mycelv1.Channel{testChannel("#backend"), testChannel("#general")}}), nil
}

func (f *fakeChannels) GetChannel(_ context.Context, r *connect.Request[mycelv1.GetChannelRequest]) (*connect.Response[mycelv1.GetChannelResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetChannelResponse{Channel: testChannel(r.Msg.GetName())}), nil
}

func (f *fakeChannels) AddMember(_ context.Context, r *connect.Request[mycelv1.AddMemberRequest]) (*connect.Response[mycelv1.AddMemberResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.AddMemberResponse{Channel: testChannel(r.Msg.GetChannel())}), nil
}

func (f *fakeChannels) RemoveMember(_ context.Context, r *connect.Request[mycelv1.RemoveMemberRequest]) (*connect.Response[mycelv1.RemoveMemberResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.RemoveMemberResponse{}), nil
}

func (f *fakeChannels) ArchiveChannel(_ context.Context, r *connect.Request[mycelv1.ArchiveChannelRequest]) (*connect.Response[mycelv1.ArchiveChannelResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ArchiveChannelResponse{}), nil
}

func (f *fakeChannels) Post(_ context.Context, r *connect.Request[mycelv1.PostRequest]) (*connect.Response[mycelv1.PostResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	m := testMsg(42, r.Msg.GetText())
	m.Id, m.Thread, m.Meta.Hop = "msg_42", "thr_1", 1
	return connect.NewResponse(&mycelv1.PostResponse{Message: m}), nil
}

func (f *fakeChannels) ListMessages(_ context.Context, r *connect.Request[mycelv1.ListMessagesRequest]) (*connect.Response[mycelv1.ListMessagesResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	// Deliberately newest first: the CLI prints oldest first.
	msgs := []*mycelv1.Message{testMsg(11, "second"), testMsg(10, "first")}
	if f.extraText != "" {
		msgs = append(msgs, testMsg(12, f.extraText))
	}
	return connect.NewResponse(&mycelv1.ListMessagesResponse{Messages: msgs}), nil
}

func (f *fakeChannels) Watch(_ context.Context, r *connect.Request[mycelv1.WatchRequest], s *connect.ServerStream[mycelv1.WatchResponse]) error {
	f.mu.Lock()
	f.watchAuth = r.Header().Get("Authorization")
	f.mu.Unlock()
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return err
	}
	for _, m := range []*mycelv1.Message{testMsg(11, "old"), testMsg(12, "live one"), testMsg(13, "live two")} {
		if err := s.Send(&mycelv1.WatchResponse{Message: m}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeChannels) BlackboardGet(_ context.Context, r *connect.Request[mycelv1.BlackboardGetRequest]) (*connect.Response[mycelv1.BlackboardGetResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.BlackboardGetResponse{Entries: []*mycelv1.BlackboardEntry{
		{Channel: r.Msg.GetChannel(), Key: "plan", Value: "step one\nstep two", Version: 3, UpdatedBy: "agent:coder@mac"},
	}}), nil
}

func (f *fakeChannels) BlackboardPut(_ context.Context, r *connect.Request[mycelv1.BlackboardPutRequest]) (*connect.Response[mycelv1.BlackboardPutResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.BlackboardPutResponse{Entry: &mycelv1.BlackboardEntry{
		Channel: r.Msg.GetChannel(), Key: r.Msg.GetKey(), Value: r.Msg.GetValue(), Version: 4,
	}}), nil
}

func (f *fakeChannels) BlackboardDelete(_ context.Context, r *connect.Request[mycelv1.BlackboardDeleteRequest]) (*connect.Response[mycelv1.BlackboardDeleteResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.BlackboardDeleteResponse{}), nil
}

func (f *fakeChannels) ListAgents(_ context.Context, r *connect.Request[mycelv1.ListAgentsRequest]) (*connect.Response[mycelv1.ListAgentsResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListAgentsResponse{Agents: []*mycelv1.Agent{
		{Id: "agent:coder@mac", Name: "coder", NodeName: "mac", OwnerId: "usr_1", Harness: "claude-code", State: "running"},
		{Id: "agent:coder-w1@mac", Name: "coder-w1", NodeName: "mac", OwnerId: "usr_1", Harness: "claude-code", State: "paused",
			Parent: "agent:coder@mac", PausedReason: "budget: agent", Used: &mycelv1.Budget{Tokens: 1200, Usd: 0.0123}},
	}}), nil
}

func wantReq(want proto.Message) func(t *testing.T, m proto.Message) {
	return func(t *testing.T, m proto.Message) {
		t.Helper()
		if !proto.Equal(m, want) {
			t.Errorf("request = %v, want %v", m, want)
		}
	}
}

func TestChannelCommands(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		notOut   []string
		wantErr  []string
		checkReq func(t *testing.T, m proto.Message)
		jsonLine bool
	}{
		{
			name:    "channels create with policy",
			args:    []string{"channels", "create", "#pingpong", "--max-hops", "4", "--open"},
			wantOut: []string{"Created topic channel #pingpong."},
			checkReq: wantReq(&mycelv1.CreateChannelRequest{
				Name: "#pingpong", Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC,
				Policy: &mycelv1.ChannelPolicy{MaxHops: 4, OpenToMembers: true, AgentsMayPost: true, HumansMayPost: true},
			}),
		},
		{
			name:     "channels create task with members, bare name",
			args:     []string{"--json", "channels", "create", "task-1", "--type", "task", "--rate", "10", "--members", "agent:a@m,user:usr_2"},
			wantOut:  []string{`"name":"#task-1"`, `"type":"CHANNEL_TYPE_TASK"`},
			jsonLine: true,
			checkReq: wantReq(&mycelv1.CreateChannelRequest{
				Name: "#task-1", Type: mycelv1.ChannelType_CHANNEL_TYPE_TASK, Members: []string{"agent:a@m", "user:usr_2"},
				Policy: &mycelv1.ChannelPolicy{RatePerMinute: 10, AgentsMayPost: true, HumansMayPost: true},
			}),
		},
		{
			name:     "channels create without policy flags sends no policy",
			args:     []string{"channels", "create", "#plain"},
			checkReq: wantReq(&mycelv1.CreateChannelRequest{Name: "#plain", Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC}),
		},
		{
			name:     "channels create bad type",
			args:     []string{"channels", "create", "#x", "--type", "system"},
			wantCode: 2,
			wantErr:  []string{"invalid channel type"},
		},
		{
			name:     "channels ls",
			args:     []string{"channels", "ls", "--all"},
			wantOut:  []string{"NAME", "TYPE", "MEMBERS", "#backend", "#general", "topic"},
			checkReq: wantReq(&mycelv1.ListChannelsRequest{IncludeArchived: true}),
		},
		{
			name:     "channels get json has policy and members",
			args:     []string{"channels", "get", "#backend", "--json"},
			wantOut:  []string{`"policy":{"max_hops":4`, `"open_to_members":true`, `"members":[{"id":"user:usr_1","role":"owner"`},
			jsonLine: true,
		},
		{
			name:    "channels get human",
			args:    []string{"channels", "get", "backend"},
			wantOut: []string{"Name:", "#backend", "Max hops:", "4", "Open to members:", "yes", "MEMBER", "agent:coder@mac"},
		},
		{
			name:     "channels add",
			args:     []string{"channels", "add", "#backend", "agent:reviewer@box", "--role", "observer"},
			wantOut:  []string{"Added agent:reviewer@box to #backend."},
			checkReq: wantReq(&mycelv1.AddMemberRequest{Channel: "#backend", Member: "agent:reviewer@box", Role: "observer"}),
		},
		{
			name:     "channels rm",
			args:     []string{"channels", "rm", "#backend", "agent:reviewer@box"},
			wantOut:  []string{"Removed agent:reviewer@box from #backend."},
			checkReq: wantReq(&mycelv1.RemoveMemberRequest{Channel: "#backend", Member: "agent:reviewer@box"}),
		},
		{
			name:     "channels archive",
			args:     []string{"channels", "archive", "#old"},
			wantOut:  []string{"Archived #old."},
			checkReq: wantReq(&mycelv1.ArchiveChannelRequest{Name: "#old"}),
		},
		{
			name:    "post with flags",
			args:    []string{"post", "#backend", "@reviewer", "please", "check", "--mentions", "agent:reviewer@box", "--reply-to", "msg_1", "--thread", "thr_1", "--type", "task"},
			wantOut: []string{"posted msg_42 seq 42 in #backend (thread thr_1, hop 1)"},
			checkReq: wantReq(&mycelv1.PostRequest{
				Channel: "#backend", Text: "@reviewer please check", Mentions: []string{"agent:reviewer@box"},
				ReplyTo: "msg_1", Thread: "thr_1", Type: mycelv1.MessageType_MESSAGE_TYPE_TASK,
			}),
		},
		{
			name:     "post defaults to chat",
			args:     []string{"post", "#backend", "hi"},
			checkReq: wantReq(&mycelv1.PostRequest{Channel: "#backend", Text: "hi", Type: mycelv1.MessageType_MESSAGE_TYPE_CHAT}),
		},
		{
			name:     "post bad type",
			args:     []string{"post", "#backend", "hi", "--type", "shout"},
			wantCode: 2,
			wantErr:  []string{"invalid message type"},
		},
		{
			name:     "post without text",
			args:     []string{"post", "#backend"},
			wantCode: 2,
		},
		{
			name:     "read json newest last with default limit",
			args:     []string{"read", "#task-e2e-1", "--json"},
			wantOut:  []string{`{"messages":[{"id":"msg_first"`, `"seq":"10"`},
			jsonLine: true,
			checkReq: wantReq(&mycelv1.ListMessagesRequest{Channel: "#task-e2e-1", Limit: 50}),
		},
		{
			name:     "read human",
			args:     []string{"read", "#backend", "--limit", "5", "--thread", "thr_1"},
			wantOut:  []string{"[2026-10-28T14:00:00Z] #backend agent:coder@mac: first\n[2026-10-28T14:00:00Z] #backend agent:coder@mac: second\n"},
			checkReq: wantReq(&mycelv1.ListMessagesRequest{Channel: "#backend", Thread: "thr_1", Limit: 5}),
		},
		{
			name:    "read quote",
			args:    []string{"read", "#backend", "--quote"},
			wantOut: []string{"[mycel message msg_first in #backend from agent:coder@mac (untrusted)", "> first\n[end mycel message]"},
		},
		{
			name:     "read follow streams new messages",
			args:     []string{"read", "#backend", "--follow"},
			wantOut:  []string{": first\n", ": second\n", "agent:coder@mac: live one\n", ": live two\n"},
			notOut:   []string{"old"},
			checkReq: wantReq(&mycelv1.WatchRequest{Channels: []string{"#backend"}, AfterSeq: 11}),
		},
		{
			name:     "bb get lists",
			args:     []string{"bb", "get", "#backend"},
			wantOut:  []string{"KEY", "VERSION", "plan", "3", "step one step two"},
			checkReq: wantReq(&mycelv1.BlackboardGetRequest{Channel: "#backend"}),
		},
		{
			name:     "bb get key prints the value",
			args:     []string{"bb", "get", "#backend", "plan"},
			wantOut:  []string{"step one\nstep two\n"},
			notOut:   []string{"KEY"},
			checkReq: wantReq(&mycelv1.BlackboardGetRequest{Channel: "#backend", Key: "plan"}),
		},
		{
			name:     "bb put",
			args:     []string{"bb", "put", "#backend", "plan", "v2", "--expect-version", "3"},
			wantOut:  []string{"Stored plan on the blackboard of #backend (version 4)."},
			checkReq: wantReq(&mycelv1.BlackboardPutRequest{Channel: "#backend", Key: "plan", Value: "v2", ExpectVersion: 3}),
		},
		{
			name:     "bb rm",
			args:     []string{"bb", "rm", "#backend", "plan"},
			wantOut:  []string{"Removed plan from the blackboard of #backend."},
			checkReq: wantReq(&mycelv1.BlackboardDeleteRequest{Channel: "#backend", Key: "plan"}),
		},
		{
			name:    "agents ls",
			args:    []string{"agents", "ls"},
			wantOut: []string{"ID", "HARNESS", "agent:coder@mac", "claude-code", "running"},
		},
		{
			name:     "agents ls json",
			args:     []string{"--json", "agents", "ls"},
			wantOut:  []string{`"agents":[{"id":"agent:coder@mac"`},
			jsonLine: true,
		},
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
			for _, s := range tt.notOut {
				if strings.Contains(res.stdout, s) {
					t.Errorf("stdout contains %q:\n%s", s, res.stdout)
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

func TestWatchSendsBearer(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	if res := runCLI(t, "", "read", "#backend", "--follow", "--json"); res.code != 0 {
		t.Fatalf("read --follow: %+v", res)
	}
	env.ch.mu.Lock()
	defer env.ch.mu.Unlock()
	if env.ch.watchAuth != "Bearer "+testAPIKey {
		t.Fatalf("Watch Authorization = %q", env.ch.watchAuth)
	}
}

// ---- local API (mcp, hook, agents ls --local) ----

type localFake struct {
	mu       sync.Mutex
	reqs     []string
	bodies   []string // request bodies, parallel to reqs
	agents   string
	inbox    string
	inboxErr bool
	workers  string // GET /api/local/workers
	spawnErr bool
	joinErr  bool            // agent.channel.join is refused
	joined   map[string]bool // channels the session joined; spawn requires membership like the hub
}

func (f *localFake) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func newLocalFake(t *testing.T, token string) (*localFake, *httptest.Server) {
	t.Helper()
	f := &localFake{agents: `{"agents":[]}`, inbox: `{}`, workers: `{"workers":[]}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		agents, inbox, inboxErr, workers, spawnErr, joinErr := f.agents, f.inbox, f.inboxErr, f.workers, f.spawnErr, f.joinErr
		f.mu.Unlock()
		var chBody struct{ Channel string }
		_ = json.Unmarshal(b, &chBody)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad token"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/local/agents":
			_, _ = w.Write([]byte(agents))
		case r.Method == http.MethodPost && r.URL.Path == "/api/local/agents":
			var reg struct{ Name string }
			_ = json.Unmarshal(b, &reg)
			if reg.Name == "" {
				reg.Name = "coder"
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"agent_id":"agent:%s@mac","session":"tmp"}`, reg.Name)
		case r.Method == http.MethodPost && r.URL.Path == "/api/local/agents/tmp/rpc/agent.channel.join":
			if joinErr {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"permission_denied: ` + chBody.Channel + ` is not open to members"}`))
				return
			}
			f.mu.Lock()
			if f.joined == nil {
				f.joined = map[string]bool{}
			}
			f.joined[chBody.Channel] = true
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"channel":{"name":%q}}`, chBody.Channel)
		case r.Method == http.MethodPost && r.URL.Path == "/api/local/agents/tmp/spawn":
			f.mu.Lock()
			member := f.joined[chBody.Channel]
			f.mu.Unlock()
			if !member {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"permission_denied: the parent is not a member of ` + chBody.Channel + `"}`))
				return
			}
			if spawnErr {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unknown harness nope"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"agent_id":"agent:cli-w1@mac","worker":"agent:cli-w1@mac"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/local/workers":
			_, _ = w.Write([]byte(workers))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/rpc/agent.inbox.pull"):
			if inboxErr {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"bus not connected"}`))
				return
			}
			_, _ = w.Write([]byte(inbox))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// writeLocalToken writes <MYCEL_HOME>/data/node/local.token.
func writeLocalToken(t *testing.T, tok string) {
	t.Helper()
	d, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.NodeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.NodeDir(), "local.token"), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHook(t *testing.T) {
	const tok = "tok123"
	msg := `{"id":"msg_9","channel":"#backend","from":"agent:reviewer@box","payload":{"text":"review done"},"meta":{"hop":1,"provenance":{"untrusted":true}},"ts":"2026-10-28T14:00:00Z","seq":"9"}`

	t.Run("prints rendered messages using the live session", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		f.agents = `{"agents":[{"agent_id":"agent:coder@mac","session":"s1","harness":"claude-code"}]}`
		f.inbox = `{"messages":[` + msg + `,` + msg + `],"next_seq":"9"}`
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", srv.URL)
		if res.code != 0 {
			t.Fatalf("hook: %+v", res)
		}
		want := hookHeader + "\n\n[mycel message msg_9 in #backend from agent:reviewer@box (untrusted) hop 1 seq 9 at 2026-10-28T14:00:00Z]\n> review done\n[end mycel message]\n\n[mycel message msg_9"
		if !strings.HasPrefix(res.stdout, want) || !strings.HasSuffix(res.stdout, "[end mycel message]\n") {
			t.Errorf("stdout = %q", res.stdout)
		}
		got := f.log()
		if len(got) != 2 || got[1] != "POST /api/local/agents/s1/rpc/agent.inbox.pull" {
			t.Errorf("requests = %v", got)
		}
	})
	t.Run("json", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		f.agents = `{"agents":[{"agent_id":"agent:coder@mac","session":"s1"}]}`
		f.inbox = `{"messages":[` + msg + `]}`
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", srv.URL, "--json")
		if res.code != 0 || !strings.HasPrefix(res.stdout, `{"messages":[{"id":"msg_9"`) {
			t.Errorf("hook --json: %+v", res)
		}
	})
	t.Run("nothing when empty", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		f.agents = `{"agents":[{"agent_id":"agent:coder@mac","session":"s1"}]}`
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", srv.URL)
		if res.code != 0 || res.stdout != "" || res.stderr != "" {
			t.Errorf("hook: %+v", res)
		}
	})
	t.Run("nothing and no registration without a live session", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		f.agents = `{"agents":[{"agent_id":"agent:other@mac","session":"s2"}]}`
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", srv.URL)
		if res.code != 0 || res.stdout != "" || res.stderr != "" {
			t.Errorf("hook: %+v", res)
		}
		if got := f.log(); strings.Join(got, "|") != "GET /api/local/agents" {
			t.Errorf("requests = %v, want only the session lookup", got)
		}
	})
	t.Run("bounded time against a hanging daemon", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer srv.Close()
		defer close(release)
		start := time.Now()
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", srv.URL)
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("hook took %v, want <= 5s", d)
		}
		if res.code != 0 || res.stdout != "" || res.stderr == "" {
			t.Errorf("hook: %+v", res)
		}
	})
	t.Run("non-loopback local-addr is a usage error", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		for _, addr := range []string{"10.0.0.5:7777", "https://127.0.0.1:7777", "http://evil.example:7777"} {
			for _, args := range [][]string{
				{"hook", "--agent", "coder", "--local-addr", addr},
				{"mcp", "--agent", "coder", "--local-addr", addr},
				{"agents", "ls", "--local", "--local-addr", addr},
			} {
				res := runCLI(t, "", args...)
				if res.code != 2 || !strings.Contains(res.stderr, "--local-addr") || strings.Contains(res.stderr, "not running") {
					t.Errorf("%v: %+v", args, res)
				}
			}
		}
	})
	t.Run("exit 0 when the daemon is not running", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		res := runCLI(t, "", "hook", "--agent", "coder")
		if res.code != 0 || res.stdout != "" || !strings.Contains(res.stderr, "not running") {
			t.Errorf("hook: %+v", res)
		}
	})
	t.Run("exit 0 when the daemon is unreachable", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", "127.0.0.1:1", "--json")
		if res.code != 0 || res.stdout != `{"messages":[]}`+"\n" || !strings.Contains(res.stderr, "not reachable") {
			t.Errorf("hook: %+v", res)
		}
	})
	t.Run("exit 0 on a local API error", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		f.agents = `{"agents":[{"agent_id":"agent:coder@mac","session":"s1"}]}`
		f.inboxErr = true
		res := runCLI(t, "", "hook", "--agent", "coder", "--local-addr", srv.URL)
		if res.code != 0 || res.stdout != "" || !strings.Contains(res.stderr, "bus not connected") {
			t.Errorf("hook: %+v", res)
		}
	})
}

func TestAgentsLsLocal(t *testing.T) {
	const tok = "tok123"
	t.Setenv(config.EnvHome, t.TempDir())
	writeLocalToken(t, tok)
	f, srv := newLocalFake(t, tok)
	f.agents = `{"agents":[{"agent_id":"agent:coder@mac","session":"s1","harness":"claude-code","since":"2026-10-28T14:59:00Z"}]}`
	res := runCLI(t, "", "agents", "ls", "--local", "--local-addr", srv.URL)
	for _, w := range []string{"AGENT", "SESSION", "agent:coder@mac", "claude-code", "s1", "1m ago"} {
		if !strings.Contains(res.stdout, w) {
			t.Errorf("stdout missing %q: %+v", w, res)
		}
	}
	res = runCLI(t, "", "agents", "ls", "--local", "--local-addr", srv.URL, "--json")
	if res.stdout != `{"agents":[{"agent_id":"agent:coder@mac","session":"s1","harness":"claude-code","since":"2026-10-28T14:59:00Z"}]}`+"\n" {
		t.Errorf("json = %q", res.stdout)
	}
}

func TestMCPCommand(t *testing.T) {
	t.Run("daemon not running", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		res := runCLI(t, "", "mcp", "--agent", "coder")
		if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "daemon is not running") {
			t.Errorf("mcp: %+v", res)
		}
	})
	t.Run("agent required", func(t *testing.T) {
		if res := runCLI(t, "", "mcp"); res.code != 2 {
			t.Errorf("mcp: %+v", res)
		}
	})
	t.Run("serves until stdin closes", func(t *testing.T) {
		const tok = "tok123"
		t.Setenv(config.EnvHome, t.TempDir())
		writeLocalToken(t, tok)
		f, srv := newLocalFake(t, tok)
		res := runCLI(t, "", "mcp", "--agent", "coder", "--harness", "claude-code", "--local-addr", srv.URL)
		if res.code != 0 || res.stdout != "" {
			t.Errorf("mcp: %+v", res)
		}
		want := []string{"POST /api/local/agents", "DELETE /api/local/agents/tmp"}
		if got := f.log(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("requests = %v, want %v", got, want)
		}
	})
}
