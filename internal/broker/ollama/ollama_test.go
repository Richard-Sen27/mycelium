package ollama

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
)

// fakeUpstream imitates Ollama's native and OpenAI-compatible endpoints.
type fakeUpstream struct {
	srv       *httptest.Server
	tagsCalls atomic.Int64
	mu        sync.Mutex
	lastAuth  string
	lastQuery string
	lastBody  string
	release   chan struct{} // the "slow" model blocks until this closes or the request ends
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
		f.srv.Close()
	})
	return f
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.lastAuth, f.lastQuery, f.lastBody = r.Header.Get("Authorization"), r.URL.RawQuery, string(body)
	f.mu.Unlock()
	var req struct {
		Model         string `json:"model"`
		Stream        *bool  `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &req)
	fl := w.(http.Flusher)
	switch r.URL.Path {
	case "/api/tags":
		f.tagsCalls.Add(1)
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3:latest","model":"llama3:latest","size":1},{"name":"qwen:7b","model":"qwen:7b","size":2},{"name":"slow:latest","model":"slow:latest"}]}`)
	case "/v1/models":
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"llama3:latest","object":"model"},{"id":"qwen:7b","object":"model"}]}`)
	case "/api/generate", "/api/chat":
		if req.Stream != nil && !*req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"model":"`+req.Model+`","response":"hi","done":true,"eval_count":5,"prompt_eval_count":3}`)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"response":"a","done":false}`+"\n")
		fl.Flush()
		if req.Model == "slow" {
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, `{"response":"b","done":false}`+"\n")
		_, _ = io.WriteString(w, `{"response":"","done":true,"eval_count":7,"prompt_eval_count":2}`+"\n")
	case "/v1/chat/completions", "/v1/completions":
		if req.Stream != nil && *req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 3; i++ {
				_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
				fl.Flush()
			}
			if req.StreamOptions.IncludeUsage {
				_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":4}}`+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":2,"completion_tokens":6}}`)
	case "/api/embed", "/api/embeddings", "/v1/embeddings", "/api/show", "/api/pull", "/api/delete":
		_, _ = io.WriteString(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

type fakeCeiling struct {
	offers atomic.Bool
	active atomic.Bool
}

func newCeiling() *fakeCeiling {
	c := &fakeCeiling{}
	c.offers.Store(true)
	c.active.Store(true)
	return c
}

func (c *fakeCeiling) Offers(kind string) bool { return kind == broker.KindOllama && c.offers.Load() }
func (c *fakeCeiling) Active() bool            { return c.active.Load() }

type auditLog struct {
	mu   sync.Mutex
	recs []broker.Record
}

func (a *auditLog) add(r broker.Record) {
	a.mu.Lock()
	a.recs = append(a.recs, r)
	a.mu.Unlock()
}

func (a *auditLog) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.recs)
}

func (a *auditLog) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for audit record %d", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (a *auditLog) last() broker.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.recs) == 0 {
		return broker.Record{}
	}
	return a.recs[len(a.recs)-1]
}

type env struct {
	t       *testing.T
	up      *fakeUpstream
	b       *Broker
	ceiling *fakeCeiling
	audit   *auditLog
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, up: newFakeUpstream(t), ceiling: newCeiling(), audit: &auditLog{}}
	b, err := New(Options{Upstream: e.up.srv.URL, Audit: e.audit.add, Ceiling: e.ceiling})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	e.b = b
	return e
}

func (e *env) attach(id string, spec *mycelv1.OllamaGrant) (endpoint, token string) {
	e.t.Helper()
	ep, err := e.b.Attach(context.Background(), broker.Attachment{
		Grant: &mycelv1.Grant{
			Id:      id,
			Subject: "agent:" + id + "@node",
			Spec:    &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: spec}},
		},
		SandboxID: "sbx_" + id,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := url.Parse(ep)
	if err != nil {
		e.t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.User.Username() != "grant" || pw == "" || u.Host != e.b.Addr().String() {
		e.t.Fatalf("endpoint %q", ep)
	}
	return ep, pw
}

func (e *env) url(path string) string { return "http://" + e.b.Addr().String() + path }

func (e *env) do(method, path, token, body string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url(path), strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	before := e.audit.count()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// The client can finish reading before the handler accounts for the
	// request; every request is audited exactly once, so wait for it.
	e.audit.waitCount(e.t, before+1)
	return resp.StatusCode, string(b)
}

func usageOf(b *Broker, grantID string) broker.Usage {
	for _, u := range b.Usage() {
		if u.GrantID == grantID {
			return u
		}
	}
	return broker.Usage{}
}

func TestCredentialForms(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})

	check := func(name string, req *http.Request, want int) {
		t.Helper()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	check("bearer", req, 200)

	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	req.SetBasicAuth("grant", tok)
	check("basic", req, 200)

	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	req.SetBasicAuth("other", tok)
	check("basic wrong user", req, 401)

	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags?token="+url.QueryEscape(tok)+"&x=1"), nil)
	check("query", req, 200)
	e.up.mu.Lock()
	q := e.up.lastQuery
	e.up.mu.Unlock()
	if strings.Contains(q, "token") || q != "x=1" {
		t.Errorf("token leaked upstream in query %q", q)
	}

	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	check("bearer again", req, 200)
	e.up.mu.Lock()
	auth := e.up.lastAuth
	e.up.mu.Unlock()
	if auth != "" {
		t.Errorf("credential forwarded upstream: %q", auth)
	}

	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	check("none", req, 401)
	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	req.Header.Set("Authorization", "Bearer "+tok+"x")
	check("unknown", req, 401)
	req, _ = http.NewRequest(http.MethodGet, e.url("/api/tags"), nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("garbage")))
	check("malformed basic", req, 401)

	// The endpoint URL itself works with a stock client (Basic from userinfo).
	ep, _ := e.attach("g1", &mycelv1.OllamaGrant{})
	resp, err := http.Get(ep + "/api/tags")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("endpoint userinfo: %d", resp.StatusCode)
	}
}

func TestPathsAndGates(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	_, tokPD := e.attach("g2", &mycelv1.OllamaGrant{AllowPull: true, AllowDelete: true})

	for _, p := range []string{"/", "/api/ps", "/api/create", "/api/copy", "/api/push", "/api/blobs/sha256:00", "/api/version", "/v1/models/llama3", "/api/tags/", "/API/tags"} {
		if code, _ := e.do(http.MethodPost, p, tok, `{"model":"llama3"}`); code != 403 {
			t.Errorf("%s: %d, want 403", p, code)
		}
	}
	if code, _ := e.do(http.MethodPost, "/api/pull", tok, `{"model":"llama3"}`); code != 403 {
		t.Errorf("pull without allow_pull: %d", code)
	}
	if code, _ := e.do(http.MethodDelete, "/api/delete", tok, `{"model":"llama3"}`); code != 403 {
		t.Errorf("delete without allow_delete: %d", code)
	}
	if code, body := e.do(http.MethodPost, "/api/pull", tokPD, `{"model":"brand-new"}`); code != 200 {
		t.Errorf("pull with allow_pull: %d %s", code, body)
	}
	if code, _ := e.do(http.MethodDelete, "/api/delete", tokPD, `{"name":"qwen:7b"}`); code != 200 {
		t.Errorf("delete with allow_delete: %d", code)
	}
	if code, _ := e.do(http.MethodDelete, "/api/delete", tokPD, `{"name":"absent"}`); code != 403 {
		t.Errorf("delete of an absent model: %d", code)
	}
	if code, _ := e.do(http.MethodGet, "/api/generate", tok, ``); code != 405 {
		t.Errorf("GET generate: %d", code)
	}
	for _, p := range []string{"/api/embed", "/api/embeddings", "/v1/embeddings", "/api/chat", "/v1/completions"} {
		if code, body := e.do(http.MethodPost, p, tok, `{"model":"llama3","stream":false}`); code != 200 {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	if code, _ := e.do(http.MethodPost, "/api/show", tok, `{"name":"llama3"}`); code != 200 {
		t.Errorf("show by name: %d", code)
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `{"prompt":"x"}`); code != 400 {
		t.Errorf("no model: %d", code)
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `not json`); code != 400 {
		t.Errorf("bad json: %d", code)
	}
	big := `{"model":"llama3","prompt":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, big); code != 413 {
		t.Errorf("big body: %d", code)
	}
	rec := e.audit.last()
	if rec.Allowed || rec.Op != "/api/generate" || rec.GrantID != "g1" {
		t.Errorf("audit %+v", rec)
	}
}

func TestModelAllowlist(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{Models: []string{"llama3"}})
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3:latest","stream":false}`); code != 200 {
		t.Errorf("allowed model: %d", code)
	}
	if code, _ := e.do(http.MethodPost, "/v1/chat/completions", tok, `{"model":"Library/llama3"}`); code != 200 {
		t.Errorf("allowed model, other spelling: %d", code)
	}
	if code, body := e.do(http.MethodPost, "/api/generate", tok, `{"model":"qwen:7b"}`); code != 403 || !strings.Contains(body, "allowlist") {
		t.Errorf("other model: %d %s", code, body)
	}
	if rec := e.audit.last(); rec.Target != "qwen:7b" || rec.Allowed {
		t.Errorf("audit %+v", rec)
	}
	if code, _ := e.do(http.MethodPost, "/api/show", tok, `{"name":"qwen:7b"}`); code != 403 {
		t.Errorf("show other model: %d", code)
	}

	// Model lists are filtered to the allowlist.
	code, body := e.do(http.MethodGet, "/api/tags", tok, "")
	if code != 200 || !strings.Contains(body, "llama3") || strings.Contains(body, "qwen") || !strings.Contains(body, `"size":1`) {
		t.Errorf("tags: %d %s", code, body)
	}
	code, body = e.do(http.MethodGet, "/v1/models", tok, "")
	if code != 200 || !strings.Contains(body, "llama3") || strings.Contains(body, "qwen") || !strings.Contains(body, `"object":"list"`) {
		t.Errorf("models: %d %s", code, body)
	}

	// Empty allowlist: any model present upstream, discovered at most once a minute.
	_, any := e.attach("g2", &mycelv1.OllamaGrant{})
	before := e.up.tagsCalls.Load()
	for i := 0; i < 3; i++ {
		if code, _ := e.do(http.MethodPost, "/api/generate", any, `{"model":"qwen:7b","stream":false}`); code != 200 {
			t.Errorf("present model: %d", code)
		}
	}
	if code, body := e.do(http.MethodPost, "/api/generate", any, `{"model":"absent"}`); code != 403 || !strings.Contains(body, "not present") {
		t.Errorf("absent model: %d %s", code, body)
	}
	if n := e.up.tagsCalls.Load() - before; n != 1 {
		t.Errorf("tags fetched %d times, want 1", n)
	}
	code, body = e.do(http.MethodGet, "/api/tags", any, "")
	if code != 200 || !strings.Contains(body, "qwen") {
		t.Errorf("unfiltered tags: %d %s", code, body)
	}
}

func TestTokenAccounting(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})

	_, body := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`)
	if strings.Count(body, "\n") != 3 {
		t.Errorf("stream body %q", body)
	}
	if rec := e.audit.last(); !rec.Allowed || rec.Tokens != 7 || rec.Target != "llama3" {
		t.Errorf("audit %+v", rec)
	}
	e.do(http.MethodPost, "/api/chat", tok, `{"model":"llama3","stream":false}`)
	u := usageOf(e.b, "g1")
	if u.Tokens != 12 || u.Requests != 2 || u.Subject != "agent:g1@node" || u.Broker != broker.KindOllama || u.Model != "llama3" {
		t.Errorf("usage %+v", u)
	}
	if u := usageOf(e.b, "g1"); u.Requests != 0 {
		t.Errorf("usage not reset: %+v", u)
	}

	// OpenAI streaming: the broker asks for the usage chunk.
	_, body = e.do(http.MethodPost, "/v1/chat/completions", tok, `{"model":"llama3","stream":true}`)
	if !strings.Contains(body, `"completion_tokens":4`) {
		t.Errorf("sse body %q", body)
	}
	e.up.mu.Lock()
	sent := e.up.lastBody
	e.up.mu.Unlock()
	if !strings.Contains(sent, `"include_usage":true`) {
		t.Errorf("include_usage not injected: %s", sent)
	}
	// A client that opted out: one token per chunk.
	e.do(http.MethodPost, "/v1/completions", tok, `{"model":"llama3","stream":true,"stream_options":{"include_usage":false}}`)
	e.do(http.MethodPost, "/v1/chat/completions", tok, `{"model":"llama3"}`)
	if u := usageOf(e.b, "g1"); u.Tokens != 4+3+6 || u.Requests != 3 {
		t.Errorf("openai usage %+v", u)
	}
}

func TestStreamingIsFlushed(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	req, _ := http.NewRequest(http.MethodPost, e.url("/api/generate"), strings.NewReader(`{"model":"slow"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, `"a"`) {
		t.Fatalf("first line %q %v", line, err)
	}
	close(e.up.release)
}

func TestTokenCeiling(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{MaxTokens: 10})
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`); code != 200 {
		t.Fatal(code)
	}
	code, body := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`)
	if code != 403 || !strings.Contains(body, "token limit") {
		t.Errorf("over the ceiling: %d %s", code, body)
	}
	// Re-attaching with new terms keeps what was used.
	_, tok2 := e.attach("g1", &mycelv1.OllamaGrant{MaxTokens: 20})
	if tok2 != tok {
		t.Error("re-attach changed the credential")
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`); code != 200 {
		t.Errorf("raised ceiling: %d", code)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{RequestsPerMinute: 2})
	_, other := e.attach("g2", &mycelv1.OllamaGrant{RequestsPerMinute: 2})
	for i := 0; i < 2; i++ {
		if code, _ := e.do(http.MethodGet, "/api/tags", tok, ""); code != 200 {
			t.Fatal(code)
		}
	}
	if code, _ := e.do(http.MethodGet, "/api/tags", tok, ""); code != 429 {
		t.Errorf("third request: %d", code)
	}
	if code, _ := e.do(http.MethodGet, "/api/tags", other, ""); code != 200 {
		t.Errorf("other grant limited: %d", code)
	}
	b := bucket{}
	now := time.Now()
	for i := 0; i < 60; i++ {
		if !b.allow(now, 1, 60) {
			t.Fatal("default burst")
		}
	}
	if b.allow(now, 1, 60) || !b.allow(now.Add(time.Second), 1, 60) {
		t.Error("refill")
	}
}

func TestGrantsIsolated(t *testing.T) {
	e := newEnv(t)
	_, a := e.attach("ga", &mycelv1.OllamaGrant{Models: []string{"llama3"}})
	_, b := e.attach("gb", &mycelv1.OllamaGrant{Models: []string{"qwen:7b"}})
	if a == b {
		t.Fatal("shared credential")
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", a, `{"model":"qwen:7b"}`); code != 403 {
		t.Errorf("a used b's model: %d", code)
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", b, `{"model":"qwen:7b"}`); code != 200 {
		t.Errorf("b: %d", code)
	}
	if rec := e.audit.last(); rec.GrantID != "gb" || rec.SandboxID != "sbx_gb" {
		t.Errorf("audit %+v", rec)
	}
	// Denied requests are audited but not counted as usage.
	us := e.b.Usage()
	if len(us) != 1 || us[0].GrantID != "gb" || us[0].Tokens != 7 || us[0].Requests != 1 {
		t.Fatalf("usage %+v", us)
	}
}

// startSlow opens a streaming request that blocks after its first line.
func (e *env) startSlow(tok string) (*http.Response, *bufio.Reader) {
	e.t.Helper()
	return e.startSlowBody(tok, `{"model":"slow"}`)
}

// startSlowBody is startSlow with a custom request body.
func (e *env) startSlowBody(tok, body string) (*http.Response, *bufio.Reader) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.url("/api/generate"), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		e.t.Fatal(err)
	}
	return resp, br
}

func expectCut(t *testing.T, resp *http.Response, br *bufio.Reader) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(br)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("stream ended cleanly, want it cut")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request not cancelled")
	}
}

func TestDetach(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	resp, br := e.startSlow(tok)
	if err := e.b.Detach(context.Background(), "g1", "expired"); err != nil {
		t.Fatal(err)
	}
	expectCut(t, resp, br)
	if code, _ := e.do(http.MethodGet, "/api/tags", tok, ""); code != 401 {
		t.Errorf("after detach: %d", code)
	}
	if err := e.b.Detach(context.Background(), "g1", "again"); err != nil {
		t.Errorf("second detach: %v", err)
	}
	// Usage of a detached grant is still reported once.
	if u := usageOf(e.b, "g1"); u.Requests != 1 {
		t.Errorf("detached usage %+v", u)
	}
	if u := usageOf(e.b, "g1"); u.Requests != 0 {
		t.Errorf("reported twice %+v", u)
	}
}

func TestFreeze(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	resp, br := e.startSlow(tok)
	e.b.Freeze("network frozen")
	expectCut(t, resp, br)
	code, body := e.do(http.MethodGet, "/api/tags", tok, "")
	if code != 503 || !strings.Contains(body, "frozen") {
		t.Errorf("frozen: %d %s", code, body)
	}
	if code, _ := e.do(http.MethodGet, "/api/tags", "", ""); code != 503 {
		t.Errorf("frozen, no credential: %d", code)
	}
	e.b.Unfreeze()
	if code, _ := e.do(http.MethodGet, "/api/tags", tok, ""); code != 200 {
		t.Errorf("unfrozen: %d", code)
	}
}

func TestCeiling(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	e.ceiling.active.Store(false)
	if code, body := e.do(http.MethodGet, "/api/tags", tok, ""); code != 503 || !strings.Contains(body, "not active") {
		t.Errorf("inactive: %d %s", code, body)
	}
	e.ceiling.active.Store(true)
	e.ceiling.offers.Store(false)
	if code, body := e.do(http.MethodGet, "/api/tags", tok, ""); code != 503 || !strings.Contains(body, "switched off") {
		t.Errorf("not offered: %d %s", code, body)
	}
	if rec := e.audit.last(); rec.Allowed || rec.GrantID != "g1" {
		t.Errorf("audit %+v", rec)
	}
	_, err := e.b.Attach(context.Background(), broker.Attachment{Grant: &mycelv1.Grant{
		Id: "g2", Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{}}},
	}})
	if !errors.Is(err, broker.ErrCeiling) {
		t.Errorf("attach: %v", err)
	}
}

func TestAttachValidation(t *testing.T) {
	e := newEnv(t)
	if _, err := e.b.Attach(context.Background(), broker.Attachment{Grant: &mycelv1.Grant{Id: "g"}}); err == nil {
		t.Error("grant without an ollama spec accepted")
	}
	if _, err := e.b.Attach(context.Background(), broker.Attachment{}); err == nil {
		t.Error("nil grant accepted")
	}
	_, err := e.b.Attach(context.Background(), broker.Attachment{
		Grant:     &mycelv1.Grant{Id: "g", Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{}}}},
		ExpiresAt: time.Now().Add(-time.Second),
	})
	if err == nil {
		t.Error("expired grant accepted")
	}
	ok, why := e.b.Available(context.Background())
	if !ok || e.b.Kind() != broker.KindOllama {
		t.Errorf("available %v %s", ok, why)
	}
}

func TestNormModel(t *testing.T) {
	for in, want := range map[string]string{
		"llama3":                             "llama3:latest",
		"Llama3:8B":                          "llama3:8b",
		"library/llama3":                     "llama3:latest",
		"registry.ollama.ai/library/qwen:7b": "qwen:7b",
		"hf.co/org/model":                    "hf.co/org/model:latest",
		"localhost:5000/ns/model":            "localhost:5000/ns/model:latest",
		"":                                   "",
		"bad name":                           "",
		"ns/":                                "",
	} {
		if got := normModel(in); got != want {
			t.Errorf("normModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCounter(t *testing.T) {
	c := &counter{}
	c.setMode("application/x-ndjson")
	stream := `{"done":false}` + "\n" + `{"done":true,"eval_count":11,"prompt_eval_count":4}`
	for i := 0; i < len(stream); i += 3 {
		c.write([]byte(stream[i:min(i+3, len(stream))]))
	}
	c.finish()
	if c.gen != 11 || c.prompt != 4 {
		t.Errorf("ndjson %d %d", c.gen, c.prompt)
	}
	c = &counter{}
	c.setMode("application/json; charset=utf-8")
	c.write([]byte("{\n  \"eval_count\": 3\n}"))
	c.finish()
	if c.gen != 3 {
		t.Errorf("json %d", c.gen)
	}
	c = &counter{}
	c.setMode("application/x-ndjson")
	c.write([]byte(strings.Repeat("x", maxLineBytes+10)))
	c.write([]byte("\n" + `{"eval_count":2}` + "\n"))
	c.finish()
	if c.gen != 2 {
		t.Errorf("long line %d", c.gen)
	}
}
