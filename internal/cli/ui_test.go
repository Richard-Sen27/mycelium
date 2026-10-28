package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
)

// fakeEvents implements EventService for CLI tests.
type fakeEvents struct {
	mycelv1connect.UnimplementedEventServiceHandler
	hub *fakeHub
}

func (f *fakeEvents) Search(_ context.Context, r *connect.Request[mycelv1.SearchRequest]) (*connect.Response[mycelv1.SearchResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.SearchResponse{Hits: []*mycelv1.SearchHit{
		{Kind: mycelv1.EventKind_EVENT_KIND_MESSAGE, Id: "msg_1", Title: "#general", Snippet: "deploy \x1b[2J the gpu box", Channel: "#general", At: "2026-10-28T14:00:00Z"},
		{Kind: mycelv1.EventKind_EVENT_KIND_NODE, Id: "gpu-box", Title: "gpu-box", Snippet: "gpu-box"},
	}}), nil
}

func (f *fakeEvents) ListAudit(_ context.Context, r *connect.Request[mycelv1.ListAuditRequest]) (*connect.Response[mycelv1.ListAuditResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListAuditResponse{Entries: []*mycelv1.AuditEntry{
		{Id: "7", At: "2026-10-28T14:59:00Z", Actor: "usr_1", Action: "grant.approve", Resource: "grant:gr_1", Outcome: "allowed", TraceId: "tr_1"},
	}, NextPageToken: "7"}), nil
}

// fakeDaemon answers POST /api/local/ui-code for the local token tok.
func fakeDaemon(t *testing.T, tok string, status int) (string, *int) {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/local/ui-code" || r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing or wrong local token"}`))
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"code":"one-time-code","expires_in":60}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"too many outstanding sign-in codes"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), calls
}

func TestUI(t *testing.T) {
	const tok = "tok_SECRET123"
	addr, calls := fakeDaemon(t, tok, http.StatusOK)
	base := "http://" + addr
	tests := []struct {
		name      string
		hubRole   bool
		args      []string
		wantOut   string
		wantOpen  string
		tokenOnly bool // the token may appear in stdout
	}{
		{name: "code flow", args: []string{"ui", "--local-addr", addr}, wantOpen: base + "/#code=one-time-code",
			wantOut: "Opened " + base + "/ in your browser (the sign-in link works once, for 60 s)."},
		{name: "json", args: []string{"ui", "--json", "--local-addr", addr}, wantOpen: base + "/#code=one-time-code",
			wantOut: `{"opened":true,"url":"` + base + `/"}`},
		{name: "print", args: []string{"ui", "--print"}, wantOut: "http://127.0.0.1:7777/#local=" + tok, tokenOnly: true},
		{name: "print on the hub machine", hubRole: true, args: []string{"ui", "--print"}, wantOut: "http://127.0.0.1:7779/#local=" + tok, tokenOnly: true},
		{name: "json print", args: []string{"ui", "--print", "--json"}, wantOut: `{"url":"http://127.0.0.1:7777/#local=` + tok + `"}`, tokenOnly: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			t.Setenv(EnvLocalAddr, "")
			writeLocalToken(t, tok)
			if tt.hubRole {
				writeFile(t, filepath.Join(env.home, "data", "hub", "hub.yaml"), "x: 1\n")
			}
			ta := newTestApp(t, "")
			var opened string
			ta.openURL = func(u string) { opened = u }
			res := ta.run(tt.args...)
			if res.code != 0 || strings.TrimSpace(res.stdout) != tt.wantOut {
				t.Fatalf("ui: %+v", res)
			}
			if opened != tt.wantOpen {
				t.Fatalf("opened %q, want %q", opened, tt.wantOpen)
			}
			if strings.Contains(opened, tok) {
				t.Fatal("the opened URL carries the token")
			}
			if !tt.tokenOnly && (strings.Contains(res.stdout, tok) || strings.Contains(res.stderr, tok)) {
				t.Fatalf("token printed without --print: %+v", res)
			}
		})
	}
	if *calls != 2 {
		t.Fatalf("daemon asked %d times for a code, want 2", *calls)
	}
}

func TestUICodeRefused(t *testing.T) {
	addr, _ := fakeDaemon(t, "tok", http.StatusTooManyRequests)
	newTestEnv(t)
	writeLocalToken(t, "tok")
	ta := newTestApp(t, "")
	res := ta.run("ui", "--local-addr", addr)
	if res.code == 0 || !strings.Contains(res.stderr, "too many outstanding") {
		t.Fatalf("refused code: %+v", res)
	}
}

func TestUINoDaemon(t *testing.T) {
	newTestEnv(t)
	ta := newTestApp(t, "")
	res := ta.run("ui")
	if res.code == 0 || !strings.Contains(res.stderr, "not running") {
		t.Fatalf("ui without daemon: %+v", res)
	}
}

func TestHubUIURL(t *testing.T) {
	env := newTestEnv(t)
	ta := newTestApp(t, "")
	if res := ta.run("hub", "ui-url"); res.code == 0 {
		t.Fatalf("ui-url without a hub: %+v", res)
	}
	env.writeConfig(t)
	ta = newTestApp(t, "")
	if res := ta.run("hub", "ui-url"); res.code != 0 || strings.TrimSpace(res.stdout) != env.srv.URL {
		t.Fatalf("ui-url from config: %+v", res)
	}
	writeFile(t, filepath.Join(env.home, "data", "hub", "hub.yaml"), "advertise_url: https://hub.example:7777/\n")
	ta = newTestApp(t, "")
	if res := ta.run("hub", "ui-url"); res.code != 0 || strings.TrimSpace(res.stdout) != "https://hub.example:7777" {
		t.Fatalf("ui-url from hub.yaml: %+v", res)
	}
	writeFile(t, filepath.Join(env.home, "data", "hub", "hub.yaml"), "advertise_url: https://hub.example:7777\nui:\n  external_url: https://mycel.example.com\n")
	ta = newTestApp(t, "")
	if res := ta.run("hub", "ui-url", "--json"); res.code != 0 || strings.TrimSpace(res.stdout) != `{"url":"https://mycel.example.com"}` {
		t.Fatalf("ui-url external: %+v", res)
	}
}

func TestSearchCmd(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	ta := newTestApp(t, "")
	res := ta.run("search", "gpu", "--kind", "message,node", "--kind", "node")
	if res.code != 0 {
		t.Fatalf("search: %+v", res)
	}
	req, _ := env.hub.lastReq.(*mycelv1.SearchRequest)
	if req.GetQuery() != "gpu" || len(req.GetKinds()) != 2 || req.GetLimit() != 50 {
		t.Fatalf("request %v", req)
	}
	if !strings.Contains(res.stdout, "message  msg_1") || strings.Contains(res.stdout, "\x1b") {
		t.Fatalf("table:\n%s", res.stdout)
	}
	if res := newTestApp(t, "").run("search", "x", "--kind", "audit"); res.code != exitUsage {
		t.Fatalf("bad kind: %+v", res)
	}
	res = newTestApp(t, "").run("search", "gpu", "--json")
	var out struct {
		Hits []map[string]any `json:"hits"`
	}
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &out) != nil || len(out.Hits) != 2 {
		t.Fatalf("json: %+v", res)
	}
}

func TestAuditCmd(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	ta := newTestApp(t, "")
	res := ta.run("audit", "ls", "--trace", "tr_1", "--actor", "usr_1", "--resource", "grant:gr_1", "--since", "1h", "--limit", "5")
	if res.code != 0 {
		t.Fatalf("audit ls: %+v", res)
	}
	req, _ := env.hub.lastReq.(*mycelv1.ListAuditRequest)
	if req.GetTraceId() != "tr_1" || req.GetActor() != "usr_1" || req.GetResource() != "grant:gr_1" ||
		req.GetSince() != "2026-10-28T14:00:00Z" || req.GetLimit() != 5 {
		t.Fatalf("request %v", req)
	}
	if !strings.Contains(res.stdout, "grant.approve") || !strings.Contains(res.stderr, "more entries") {
		t.Fatalf("output: %+v", res)
	}
	if res := newTestApp(t, "").run("audit", "ls", "--since", "yesterday"); res.code != exitUsage {
		t.Fatalf("bad since: %+v", res)
	}
	res = newTestApp(t, "").run("audit", "ls", "--json")
	if res.code != 0 || !strings.Contains(res.stdout, `"next_page_token":"7"`) {
		t.Fatalf("json: %+v", res)
	}
}
