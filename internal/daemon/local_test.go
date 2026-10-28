package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// ---- helpers ----

type localClient struct {
	t     *testing.T
	addr  string
	token string
}

func readLocalToken(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, LocalTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newLocalClient(t *testing.T, r *running, dir string) *localClient {
	t.Helper()
	return &localClient{t: t, addr: r.addr, token: readLocalToken(t, dir)}
}

func (c *localClient) do(method, path, body string) (int, []byte) {
	c.t.Helper()
	return c.doWith(c.token, method, path, body)
}

func (c *localClient) doWith(token, method, path, body string) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, "http://"+c.addr+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (c *localClient) register(name, harness string) (int, registerReply) {
	c.t.Helper()
	body, _ := json.Marshal(registerBody{Name: name, Harness: harness})
	code, b := c.do(http.MethodPost, "/api/local/agents", string(body))
	var rr registerReply
	if code == http.StatusCreated {
		if err := json.Unmarshal(b, &rr); err != nil {
			c.t.Fatalf("register reply %q: %v", b, err)
		}
	}
	return code, rr
}

func (c *localClient) mustRegister(name string) registerReply {
	c.t.Helper()
	code, rr := c.register(name, "claude-code")
	if code != http.StatusCreated {
		c.t.Fatalf("register %s: %d", name, code)
	}
	return rr
}

func (c *localClient) rpc(session, method, body string) (int, []byte) {
	c.t.Helper()
	return c.do(http.MethodPost, "/api/local/agents/"+session+"/rpc/"+method, body)
}

func (c *localClient) list() []agentListEntry {
	c.t.Helper()
	code, b := c.do(http.MethodGet, "/api/local/agents", "")
	if code != http.StatusOK {
		c.t.Fatalf("list: %d %s", code, b)
	}
	var out struct {
		Agents []agentListEntry `json:"agents"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		c.t.Fatal(err)
	}
	return out.Agents
}

func (c *localClient) pull(session, body string) (int, *mycelv1.InboxPullResponse) {
	c.t.Helper()
	code, b := c.rpc(session, "inbox.pull", body)
	if code != http.StatusOK {
		return code, nil
	}
	resp := &mycelv1.InboxPullResponse{}
	if err := protojson.Unmarshal(b, resp); err != nil {
		c.t.Fatalf("pull reply %q: %v", b, err)
	}
	return code, resp
}

func readCursorFile(t *testing.T, dir, name string) uint64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, AgentsDir, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var c cursorFile
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c.Cursor
}

type localEnv struct {
	h   *fakeHub
	fa  *fakeAgents
	dir string
	r   *running
	c   *localClient
}

func startLocal(t *testing.T, tweak func(*runConfig)) *localEnv {
	t.Helper()
	h := newFakeHub(t)
	fa := newFakeAgents(h)
	dir := joinNode(t, h, "n1")
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	if tweak != nil {
		tweak(&cfg)
	}
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	return &localEnv{h: h, fa: fa, dir: dir, r: r, c: newLocalClient(t, r, dir)}
}

// ---- tests ----

func TestLocalTokenFile(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	path := filepath.Join(dir, LocalTokenFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !isWindows() && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	tok1 := readLocalToken(t, dir)
	if raw, err := base64.RawURLEncoding.DecodeString(tok1); err != nil || len(raw) != 32 {
		t.Fatalf("token %q: %v (%d bytes)", tok1, err, len(raw))
	}
	c := &localClient{t: t, addr: r.addr, token: tok1}
	if code, _ := c.doWith("", http.MethodGet, "/api/local/agents", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := c.doWith(tok1+"x", http.MethodGet, "/api/local/agents", ""); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := c.doWith("", http.MethodPost, "/api/local/agents/x/rpc/whoami", ""); code != http.StatusUnauthorized {
		t.Fatalf("rpc without token: %d", code)
	}
	if code, _ := c.doWith("", http.MethodGet, "/api/local/nothing", ""); code != http.StatusUnauthorized {
		t.Fatalf("unknown path without token: %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/api/local/nothing", ""); code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/api/local/agents", ""); code != http.StatusOK {
		t.Fatalf("right token: %d", code)
	}
	if code, _ := c.doWith("", http.MethodGet, "/api/status", ""); code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if code, _ := c.doWith("", http.MethodGet, "/", ""); code != http.StatusOK {
		t.Fatalf("page: %d", code)
	}
	// The Host guard covers the local API too, even with the right token.
	req, _ := http.NewRequest(http.MethodGet, "http://"+r.addr+"/api/local/agents", nil)
	req.Host = "rebind.example:7777"
	req.Header.Set("Authorization", "Bearer "+tok1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", resp.StatusCode)
	}

	r.cancel()
	if err := r.wait(t, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file after shutdown: %v", err)
	}
	r2 := startRun(t, dir)
	tok2 := readLocalToken(t, dir)
	if tok2 == tok1 {
		t.Fatal("token not renewed on start")
	}
	c2 := &localClient{t: t, addr: r2.addr, token: tok1}
	if code, _ := c2.do(http.MethodGet, "/api/local/agents", ""); code != http.StatusUnauthorized {
		t.Fatalf("old token accepted: %d", code)
	}
}

func TestLocalRegisterListDelete(t *testing.T) {
	e := startLocal(t, nil)
	c := e.c

	code, rr := c.register("coder", "claude-code")
	if code != http.StatusCreated || rr.AgentID != "agent:coder@n1" || rr.Session == "" {
		t.Fatalf("register: %d %+v", code, rr)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(rr.Session); err != nil || len(raw) != 24 {
		t.Fatalf("session id %q", rr.Session)
	}
	code, again := c.register("coder", "claude-code")
	if code != http.StatusCreated || again != rr {
		t.Fatalf("second register: %d %+v", code, again)
	}
	if regs, _ := e.fa.counts(); regs != 1 {
		t.Fatalf("hub saw %d registrations", regs)
	}
	list := c.list()
	if len(list) != 1 || list[0].AgentID != rr.AgentID || list[0].Session != rr.Session || list[0].Harness != "claude-code" {
		t.Fatalf("list %+v", list)
	}
	if _, err := time.Parse(time.RFC3339, list[0].Since); err != nil {
		t.Fatalf("since %q", list[0].Since)
	}
	if s := getStatus(t, e.r.addr); s.LocalAgents != 1 {
		t.Fatalf("status local_agents %d", s.LocalAgents)
	}

	for _, bad := range []registerBody{
		{Name: "Bad_Name", Harness: "claude-code"},
		{Name: "", Harness: "claude-code"},
		{Name: "-x", Harness: "claude-code"},
		{Name: "ok", Harness: ""},
		{Name: "ok", Harness: "Claude Code"},
		{Name: "ok", Harness: strings.Repeat("a", 65)},
	} {
		if code, _ := c.register(bad.Name, bad.Harness); code != http.StatusBadRequest {
			t.Fatalf("register %+v: %d", bad, code)
		}
	}
	if code, _ := c.do(http.MethodPost, "/api/local/agents", "{not json"); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
	if code, _ := c.do(http.MethodPost, "/api/local/agents", `{"name":"x","harness":"y","extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	if code, _ := c.do(http.MethodPost, "/api/local/agents", `{"name":"`+strings.Repeat("a", 8<<10)+`"}`); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized register: %d", code)
	}
	if code, _ := c.register("coder", "codex"); code != http.StatusConflict {
		t.Fatalf("other harness: %d", code)
	}
	if code, _ := c.rpc(rr.Session, "post", `{"post":{"text":"`+strings.Repeat("a", 2<<20)+`"}}`); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized rpc: %d", code)
	}

	if code, _ := c.do(http.MethodDelete, "/api/local/agents/"+rr.Session, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := c.do(http.MethodDelete, "/api/local/agents/"+rr.Session, ""); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
	if code, _ := c.rpc(rr.Session, "whoami", ""); code != http.StatusNotFound {
		t.Fatalf("rpc after delete: %d", code)
	}
	if len(c.list()) != 0 {
		t.Fatal("list not empty after delete")
	}
	// A new registration after the delete opens a new session.
	if rr2 := c.mustRegister("coder"); rr2.Session == rr.Session {
		t.Fatal("session id reused")
	}
}

func TestLocalRPCWhoami(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	code, b := e.c.rpc(rr.Session, "whoami", "")
	if code != http.StatusOK {
		t.Fatalf("whoami: %d %s", code, b)
	}
	resp := &mycelv1.AgentWhoamiResponse{}
	if err := protojson.Unmarshal(b, resp); err != nil {
		t.Fatal(err)
	}
	if resp.GetAgent().GetId() != "agent:coder@n1" || resp.GetTrustDomain() != string(testTD) {
		t.Fatalf("whoami %v", resp)
	}
	if !strings.Contains(string(b), `"trust_domain"`) {
		t.Fatalf("not proto names: %s", b)
	}
	calls := e.fa.callsOf(bus.RPCAgentWhoami)
	if len(calls) != 1 || !strings.HasPrefix(calls[0].token, identity.AgentTokenPrefix) {
		t.Fatalf("hub calls %+v", calls)
	}
	if strings.Contains(string(b), calls[0].token) || strings.Contains(string(b), identity.AgentTokenPrefix) {
		t.Fatal("agent token in response")
	}
	// The caller may not supply the call field.
	if code, _ := e.c.rpc(rr.Session, "whoami", `{"call":{"token":"mca1_x"}}`); code != http.StatusBadRequest {
		t.Fatalf("call supplied: %d", code)
	}
	if code, _ := e.c.rpc(rr.Session, "whoami", `{"nope":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	// post round trip
	code, b = e.c.rpc(rr.Session, "post", `{"post":{"channel":"#general","text":"hi"}}`)
	if code != http.StatusOK {
		t.Fatalf("post: %d %s", code, b)
	}
	pr := &mycelv1.PostResponse{}
	if err := protojson.Unmarshal(b, pr); err != nil {
		t.Fatal(err)
	}
	if pr.GetMessage().GetFrom() != "agent:coder@n1" || pr.GetMessage().GetPayload().GetText() != "hi" {
		t.Fatalf("post %v", pr)
	}
	// The full bus constant works as the method name too.
	code, b = e.c.rpc(rr.Session, "agent.post", `{"post":{"channel":"#general","text":"again"}}`)
	if code != http.StatusOK || !strings.Contains(string(b), `"again"`) {
		t.Fatalf("agent.post: %d %s", code, b)
	}
}

func TestLocalRPCErrorMapping(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	if code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#forbidden","text":"x"}}`); code != http.StatusForbidden ||
		!strings.Contains(string(b), `"error":"permission_denied: nope"`) {
		t.Fatalf("post forbidden: %d %s", code, b)
	}
	for msg, want := range map[string]int{
		"permission_denied: nope":    http.StatusForbidden,
		"not_found: no such channel": http.StatusNotFound,
		"resource_exhausted: slow":   http.StatusTooManyRequests,
		"invalid_argument: bad key":  http.StatusBadRequest,
		"unauthenticated: expired":   http.StatusForbidden,
		"boom":                       http.StatusInternalServerError,
		"internal: db: locked":       http.StatusInternalServerError,
	} {
		body, _ := json.Marshal(map[string]any{"get": map[string]string{"channel": "#general", "key": msg}})
		code, b := e.c.rpc(rr.Session, "bb.get", string(body))
		var out map[string]string
		_ = json.Unmarshal(b, &out)
		if code != want || out["error"] != msg {
			t.Fatalf("%q: %d %s", msg, code, b)
		}
	}
	// A method the hub does not serve maps through its error text.
	if code, _ := e.c.rpc(rr.Session, "channel.list", ""); code != http.StatusInternalServerError {
		t.Fatalf("unimplemented: %d", code)
	}
}

func TestLocalUnknownMethodAndSession(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	for _, m := range []string{"foo", "agent.foo", "agent.agent.post", "register", "refresh", "agent.register", "POST"} {
		if code, _ := e.c.rpc(rr.Session, m, ""); code != http.StatusNotFound {
			t.Fatalf("method %q: %d", m, code)
		}
	}
	if code, _ := e.c.rpc("nosuchsession", "whoami", ""); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
	if code, _ := e.c.do(http.MethodGet, "/api/local/agents/"+rr.Session+"/rpc/whoami", ""); code == http.StatusOK {
		t.Fatal("GET rpc accepted")
	}
}

func TestAgentMethodsTable(t *testing.T) {
	want := []string{bus.RPCAgentPost, bus.RPCAgentJoinChannel, bus.RPCAgentLeaveChannel, bus.RPCAgentListChannels,
		bus.RPCAgentListAgents, bus.RPCAgentWhoami, bus.RPCAgentInboxPull, bus.RPCAgentBBGet, bus.RPCAgentBBPut, bus.RPCAgentListMessages,
		bus.RPCAgentJobRun, bus.RPCAgentJobGet, bus.RPCAgentJobCancel, bus.RPCAgentJobLogs, bus.RPCAgentArtifactList}
	if len(agentMethods) != len(want) {
		t.Fatalf("%d methods", len(agentMethods))
	}
	for _, sub := range want {
		am, ok := agentMethods[strings.TrimPrefix(sub, "agent.")]
		if !ok || am.subject != sub {
			t.Fatalf("%s missing", sub)
		}
		fd := am.req().ProtoReflect().Descriptor().Fields().ByName(callField)
		if fd == nil || fd.Kind() != protoreflect.MessageKind || fd.Message().FullName() != "mycel.v1.AgentCall" {
			t.Fatalf("%s: no call field", sub)
		}
		if am.resp() == nil {
			t.Fatalf("%s: no response type", sub)
		}
	}
}

func TestLocalInboxCursor(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	for i := range 3 {
		e.fa.appendMessage("#general", "m"+string(rune('1'+i)))
	}

	code, resp := e.c.pull(rr.Session, `{}`)
	if code != http.StatusOK || len(resp.GetMessages()) != 3 || resp.GetNextSeq() != 3 {
		t.Fatalf("first pull: %d %v", code, resp)
	}
	calls := e.fa.callsOf(bus.RPCAgentInboxPull)
	if calls[0].afterSeq != 0 {
		t.Fatalf("first pull sent after_seq %d", calls[0].afterSeq)
	}
	if got := readCursorFile(t, e.dir, "coder"); got != 3 {
		t.Fatalf("cursor %d", got)
	}

	code, resp = e.c.pull(rr.Session, `{"after_seq":"0"}`)
	if code != http.StatusOK || len(resp.GetMessages()) != 0 || resp.GetNextSeq() != 3 {
		t.Fatalf("second pull: %d %v", code, resp)
	}
	if calls = e.fa.callsOf(bus.RPCAgentInboxPull); calls[1].afterSeq != 3 {
		t.Fatalf("second pull sent after_seq %d", calls[1].afterSeq)
	}

	// Explicit re-read: passed through, the cursor does not move back.
	code, resp = e.c.pull(rr.Session, `{"after_seq":1}`)
	if code != http.StatusOK || len(resp.GetMessages()) != 2 || resp.GetNextSeq() != 3 {
		t.Fatalf("re-read: %d %v", code, resp)
	}
	if calls = e.fa.callsOf(bus.RPCAgentInboxPull); calls[2].afterSeq != 1 {
		t.Fatalf("re-read sent after_seq %d", calls[2].afterSeq)
	}
	if got := readCursorFile(t, e.dir, "coder"); got != 3 {
		t.Fatalf("cursor moved to %d", got)
	}
	e.fa.appendMessage("#general", "m4")
	code, resp = e.c.pull(rr.Session, `{}`)
	if code != http.StatusOK || len(resp.GetMessages()) != 1 || resp.GetMessages()[0].GetSeq() != 4 {
		t.Fatalf("pull after new message: %d %v", code, resp)
	}
	if got := readCursorFile(t, e.dir, "coder"); got != 4 {
		t.Fatalf("cursor %d", got)
	}

	// wait_ms is capped.
	_, _ = e.c.pull(rr.Session, `{"after_seq":1,"wait_ms":999999}`)
	calls = e.fa.callsOf(bus.RPCAgentInboxPull)
	if w := calls[len(calls)-1].waitMs; w != 60000 {
		t.Fatalf("wait_ms sent %d", w)
	}
}

func TestLocalInboxWaitReturnsEarly(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	type result struct {
		code int
		resp *mycelv1.InboxPullResponse
		took time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		code, resp := e.c.pull(rr.Session, `{"wait_ms":20000}`)
		done <- result{code, resp, time.Since(start)}
	}()
	waitFor(t, 5*time.Second, "pull at the hub", func() bool { return len(e.fa.callsOf(bus.RPCAgentInboxPull)) == 1 })
	e.fa.appendMessage("#general", "wake up")
	select {
	case res := <-done:
		if res.code != http.StatusOK || len(res.resp.GetMessages()) != 1 || res.took > 10*time.Second {
			t.Fatalf("pull: %d %v after %v", res.code, res.resp, res.took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("pull did not return")
	}
	if got := readCursorFile(t, e.dir, "coder"); got != 1 {
		t.Fatalf("cursor %d", got)
	}
}

func TestLocalBusDown(t *testing.T) {
	var mu sync.Mutex
	var nd *node
	e := startLocal(t, func(c *runConfig) {
		c.onNode = func(n *node) { mu.Lock(); nd = n; mu.Unlock() }
	})
	mu.Lock()
	n := nd
	mu.Unlock()
	rr := e.c.mustRegister("coder")
	port := e.h.busServer().Port()
	e.h.stopBus()
	waitFor(t, 5*time.Second, "status disconnected", func() bool { return !getStatus(t, e.r.addr).Connected })

	if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("whoami while down: %d", code)
	}
	if code, _ := e.c.pull(rr.Session, `{}`); code != http.StatusServiceUnavailable {
		t.Fatalf("pull without wait while down: %d", code)
	}
	if code, _ := e.c.register("other", "claude-code"); code != http.StatusServiceUnavailable {
		t.Fatalf("register while down: %d", code)
	}

	type result struct {
		code int
		resp *mycelv1.InboxPullResponse
	}
	done := make(chan result, 1)
	go func() {
		code, resp := e.c.pull(rr.Session, `{"wait_ms":20000}`)
		done <- result{code, resp}
	}()
	waitFor(t, 5*time.Second, "pull waiting for the bus", func() bool { return n.local.busWaits.Load() == 1 })
	// Hold node authentication until the fake hub serves agent RPCs again.
	release := e.h.rev.hold()
	defer release()
	e.h.startBus(port)
	e.fa.subscribe()
	release()
	waitFor(t, 15*time.Second, "pull at the hub", func() bool { return len(e.fa.callsOf(bus.RPCAgentInboxPull)) == 1 })
	e.fa.appendMessage("#general", "back")
	select {
	case res := <-done:
		if res.code != http.StatusOK || len(res.resp.GetMessages()) != 1 {
			t.Fatalf("pull: %d %v", res.code, res.resp)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("pull did not return")
	}
}

func TestLocalTokenRefresh(t *testing.T) {
	e := startLocal(t, nil)
	e.fa.setTTL(3 * time.Second)
	rr := e.c.mustRegister("coder")
	if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusOK {
		t.Fatal("whoami")
	}
	first := e.fa.callsOf(bus.RPCAgentWhoami)[0].token
	waitFor(t, 10*time.Second, "token refresh", func() bool {
		regs, refs := e.fa.counts()
		return refs >= 1 || regs >= 2
	})
	waitFor(t, 5*time.Second, "new token in use", func() bool {
		if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusOK {
			t.Fatalf("whoami after refresh: %d", code)
		}
		calls := e.fa.callsOf(bus.RPCAgentWhoami)
		return calls[len(calls)-1].token != first
	})
	if len(e.c.list()) != 1 {
		t.Fatal("session lost")
	}
}

func TestLocalAgentRevokedEndsSession(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "prefix", true: "legacy"}[legacy], func(t *testing.T) {
			e := startLocal(t, func(c *runConfig) { c.agentRetryEvery = 200 * time.Millisecond })
			e.fa.set(func(f *fakeAgents) { f.ttl = 3 * time.Second; f.legacy = legacy })
			rr := e.c.mustRegister("coder")
			e.fa.revoke(rr.AgentID)
			waitFor(t, 10*time.Second, "session ended", func() bool { return len(e.c.list()) == 0 })
			if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusNotFound {
				t.Fatalf("rpc after revocation: %d", code)
			}
		})
	}
}

func TestLocalRevokedReplyKicksRefresh(t *testing.T) {
	e := startLocal(t, nil) // 1 h tokens: only the kick can end the session in time
	rr := e.c.mustRegister("coder")
	e.fa.revoke(rr.AgentID)
	if code, b := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusForbidden {
		t.Fatalf("whoami: %d %s", code, b)
	}
	waitFor(t, 3*time.Second, "session ended", func() bool { return len(e.c.list()) == 0 })
	if _, refs := e.fa.counts(); refs != 1 {
		t.Fatalf("refreshes %d", refs)
	}
}

func TestLocalRefreshRefusalNoReregister(t *testing.T) {
	e := startLocal(t, func(c *runConfig) { c.agentRetryEvery = 200 * time.Millisecond })
	e.fa.set(func(f *fakeAgents) { f.ttl = 3 * time.Second; f.refreshErr = "internal: database locked" })
	e.c.mustRegister("coder")
	waitFor(t, 10*time.Second, "refresh retries", func() bool { _, refs := e.fa.counts(); return refs >= 3 })
	if regs, _ := e.fa.counts(); regs != 1 {
		t.Fatalf("re-registered %d times", regs-1)
	}
	if len(e.c.list()) != 1 {
		t.Fatal("session ended")
	}
	// A refusal of the token itself registers again.
	e.fa.set(func(f *fakeAgents) { f.refreshErr = "unauthenticated: invalid agent token" })
	waitFor(t, 10*time.Second, "re-register", func() bool { regs, _ := e.fa.counts(); return regs >= 2 })
	if len(e.c.list()) != 1 {
		t.Fatal("session ended")
	}
}

func TestLocalConcurrentPullsNoDuplicates(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	for range 3 {
		e.fa.appendMessage("#general", "m")
	}
	e.fa.set(func(f *fakeAgents) { f.pullDelay = 300 * time.Millisecond })
	var wg sync.WaitGroup
	results := make([]*mycelv1.InboxPullResponse, 2)
	for i := range 2 {
		wg.Go(func() {
			code, resp := e.c.pull(rr.Session, `{}`)
			if code != http.StatusOK {
				t.Errorf("pull %d: %d", i, code)
				return
			}
			results[i] = resp
		})
	}
	wg.Wait()
	seen := map[uint64]int{}
	for _, r := range results {
		for _, m := range r.GetMessages() {
			seen[m.GetSeq()]++
		}
	}
	if len(seen) != 3 {
		t.Fatalf("messages %v", seen)
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("seq %d delivered %d times", seq, n)
		}
	}
	calls := e.fa.callsOf(bus.RPCAgentInboxPull)
	if len(calls) != 2 || calls[0].afterSeq != 0 || calls[1].afterSeq != 3 {
		t.Fatalf("hub calls %+v", calls)
	}
}

func TestLocalSessionsEndOnShutdown(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	e.fa.appendMessage("#general", "x")
	if code, _ := e.c.pull(rr.Session, `{}`); code != http.StatusOK {
		t.Fatal("pull")
	}
	e.r.cancel()
	if err := e.r.wait(t, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	r2 := startRun(t, e.dir)
	c2 := newLocalClient(t, r2, e.dir)
	if len(c2.list()) != 0 {
		t.Fatal("sessions survived a restart")
	}
	if code, _ := c2.rpc(rr.Session, "whoami", ""); code != http.StatusNotFound {
		t.Fatalf("old session: %d", code)
	}
	if got := readCursorFile(t, e.dir, "coder"); got != 1 {
		t.Fatalf("cursor lost: %d", got)
	}
}

func TestLocalSessionsEndOnRevocation(t *testing.T) {
	var mu sync.Mutex
	var nd *node
	e := startLocal(t, func(c *runConfig) {
		c.onNode = func(n *node) { mu.Lock(); nd = n; mu.Unlock() }
	})
	rr := e.c.mustRegister("coder")
	e.fa.appendMessage("#general", "x")
	if code, _ := e.c.pull(rr.Session, `{}`); code != http.StatusOK {
		t.Fatal("pull")
	}
	mu.Lock()
	n := nd
	mu.Unlock()
	if n.local.count() != 1 {
		t.Fatal("no session")
	}
	resp := ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Revoked{Revoked: &mycelv1.Revoked{Reason: "removed"}}})
	if !resp.GetOk() {
		t.Fatal(resp)
	}
	if err := e.r.wait(t, 5*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	if n.local.count() != 0 {
		t.Fatal("sessions survived revocation")
	}
	if _, err := os.Stat(filepath.Join(e.dir, LocalTokenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file after revocation: %v", err)
	}
	if got := readCursorFile(t, e.dir, "coder"); got != 1 {
		t.Fatalf("cursor lost: %d", got)
	}
	_, refs := e.fa.counts()
	time.Sleep(200 * time.Millisecond)
	if _, refs2 := e.fa.counts(); refs2 != refs {
		t.Fatal("refresh after revocation")
	}
}

func TestLocalSecretsNotLogged(t *testing.T) {
	h := newFakeHub(t)
	fa := newFakeAgents(h)
	fa.setTTL(2 * time.Second)
	dir := joinNode(t, h, "n1")
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := newFakeProbe()
	sl := &stateLog{}
	r := startRunLogged(t, dir, testRunConfig(p, sl), p, sl, log)
	waitConnected(t, r.addr)
	c := newLocalClient(t, r, dir)
	rr := c.mustRegister("coder")
	if code, _ := c.rpc(rr.Session, "whoami", ""); code != http.StatusOK {
		t.Fatal("whoami")
	}
	_, _ = c.rpc(rr.Session, "bb.get", `{"get":{"key":"permission_denied: nope"}}`)
	waitFor(t, 10*time.Second, "token refresh", func() bool { _, refs := fa.counts(); return refs >= 1 })
	r.cancel()
	_ = r.wait(t, 5*time.Second)

	out := buf.String()
	if !strings.Contains(out, "local request") {
		t.Fatalf("no debug request log: %s", out)
	}
	for _, secret := range []string{c.token, rr.Session, identity.AgentTokenPrefix} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q in log", secret)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
