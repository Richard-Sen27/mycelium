package ollama

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
)

func (a *auditLog) find(f func(broker.Record) bool) []broker.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []broker.Record
	for _, r := range a.recs {
		if f(r) {
			out = append(out, r)
		}
	}
	return out
}

// waitOne waits until exactly one record matches and returns it.
func (a *auditLog) waitOne(t *testing.T, f func(broker.Record) bool) broker.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := a.find(f)
		if len(recs) > 1 {
			t.Fatalf("%d matching audit records: %+v", len(recs), recs)
		}
		if len(recs) == 1 {
			// Give a duplicate a moment to show up.
			time.Sleep(20 * time.Millisecond)
			if again := a.find(f); len(again) != 1 {
				t.Fatalf("%d matching audit records", len(again))
			}
			return recs[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no matching audit record")
		}
		time.Sleep(time.Millisecond)
	}
}

func isGenerate(r broker.Record) bool { return r.Op == "/api/generate" && r.Allowed }

func TestAbortAccounting(t *testing.T) {
	for _, how := range []string{"client", "detach", "freeze"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
			resp, br := e.startSlow(tok) // one chunk delivered, then the upstream blocks
			switch how {
			case "client":
				_ = resp.Body.Close()
			case "detach":
				_ = e.b.Detach(context.Background(), "g1", "revoked")
				expectCut(t, resp, br)
			case "freeze":
				e.b.Freeze("network frozen")
				expectCut(t, resp, br)
			}
			rec := e.audit.waitOne(t, isGenerate)
			if rec.Reason != "aborted" || rec.Tokens != 1 || rec.GrantID != "g1" {
				t.Errorf("audit %+v", rec)
			}
			if u := usageOf(e.b, "g1"); u.Tokens != 1 || u.Requests != 1 {
				t.Errorf("usage %+v", u)
			}
		})
	}
}

func TestTokenReservation(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{MaxTokens: 10})

	// A request that asks for 3 tokens reserves 3; the rest stays available.
	resp, br := e.startSlowBody(tok, `{"model":"slow","options":{"num_predict":3}}`)
	e.up.mu.Lock()
	sent := e.up.lastBody
	e.up.mu.Unlock()
	if !strings.Contains(sent, `"num_predict":3`) {
		t.Errorf("upstream body %s", sent)
	}
	resp2, br2 := e.startSlowBody(tok, `{"model":"slow"}`) // reserves the remaining 7
	e.up.mu.Lock()
	sent = e.up.lastBody
	e.up.mu.Unlock()
	if !strings.Contains(sent, `"num_predict":7`) {
		t.Errorf("upstream body %s", sent)
	}
	if code, body := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`); code != 403 || !strings.Contains(body, "token limit") {
		t.Errorf("third request while the budget is reserved: %d %s", code, body)
	}
	// Embeddings do not generate and are not limited by the budget.
	if code, _ := e.do(http.MethodPost, "/api/embed", tok, `{"model":"llama3"}`); code != 200 {
		t.Errorf("embed: %d", code)
	}
	_ = resp.Body.Close()
	_ = resp2.Body.Close()
	_, _ = br, br2
	deadline := time.Now().Add(5 * time.Second)
	for len(e.audit.find(isGenerate)) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Both aborted after one chunk: 2 used, the reservations released.
	if code, _ := e.do(http.MethodPost, "/v1/chat/completions", tok, `{"model":"llama3","max_tokens":100}`); code != 200 {
		t.Errorf("after release: %d", code)
	}
	e.up.mu.Lock()
	sent = e.up.lastBody
	e.up.mu.Unlock()
	if !strings.Contains(sent, `"max_tokens":8`) {
		t.Errorf("openai body %s", sent)
	}
}

func TestBudgetCutsStream(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{MaxTokens: 5})
	// The fake ignores num_predict and reports 7 tokens: the stream is cut.
	_, body := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`)
	if strings.Contains(body, `"done":true`) {
		t.Errorf("stream not cut: %q", body)
	}
	rec := e.audit.waitOne(t, isGenerate)
	if !strings.Contains(rec.Reason, "token budget") || rec.Tokens != 7 {
		t.Errorf("audit %+v", rec)
	}
	if code, _ := e.do(http.MethodPost, "/api/generate", tok, `{"model":"llama3"}`); code != 403 {
		t.Errorf("after the budget: %d", code)
	}
}

func TestKeySpelling(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{Models: []string{"llama3"}, MaxTokens: 100})
	for _, body := range []string{
		`{"Model":"qwen:7b"}`,
		`{"model":"llama3","MODEL":"qwen:7b"}`,
		`{"model":"llama3","model":"qwen:7b"}`,
		`{"model":"llama3","Stream":false}`,
		`{"model":"llama3","options":{"NUM_PREDICT":1000}}`,
		`{"model":"llama3","options":{"num_predict":1,"num_predict":1000}}`,
		`{"model":"llama3","Max_Tokens":1000}`,
	} {
		if code, _ := e.do(http.MethodPost, "/v1/completions", tok, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _ := e.do(http.MethodPost, "/api/show", tok, `{"name":"llama3","Name":"qwen:7b"}`); code != 400 {
		t.Errorf("show with two names: %d", code)
	}
}

func TestInflightCap(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	var resps []*http.Response
	for i := 0; i < maxInflight; i++ {
		resp, _ := e.startSlow(tok)
		resps = append(resps, resp)
	}
	if code, body := e.do(http.MethodGet, "/api/tags", tok, ""); code != 429 || !strings.Contains(body, "in flight") {
		t.Errorf("ninth request: %d %s", code, body)
	}
	for _, r := range resps {
		_ = r.Body.Close()
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

func TestTokenNeverLoggedOrAudited(t *testing.T) {
	up := newFakeUpstream(t)
	logs := &syncBuffer{}
	audit := &auditLog{}
	b, err := New(Options{Upstream: up.srv.URL, Audit: audit.add,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	e := &env{t: t, up: up, b: b, audit: audit}
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	for _, p := range []string{"/api/tags", "/api/ps", "/api/generate"} {
		resp, err := http.Post(e.url(p+"?token="+tok), "application/json", strings.NewReader(`{"model":"llama3","stream":false}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	_ = b.Detach(context.Background(), "g1", "done")
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(logs.String(), tok) {
		t.Error("token in logs")
	}
	for _, r := range audit.find(func(broker.Record) bool { return true }) {
		if strings.Contains(r.Op+r.Target+r.Reason, tok) {
			t.Errorf("token in audit %+v", r)
		}
	}
	if len(audit.find(func(broker.Record) bool { return true })) != 3 {
		t.Errorf("audits %+v", audit.recs)
	}
}

func TestUnauthenticatedAuditsSampled(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		resp, err := http.Get(e.url("/api/tags"))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if n := e.audit.count(); n > int(unauthAuditBurst)+2 || n == 0 {
		t.Errorf("%d unauthenticated audits", n)
	}
	long := "/" + strings.Repeat("x", 500)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{})
	e.do(http.MethodGet, long, tok, "")
	if op := e.audit.last().Op; len(op) > maxOpLen+4 {
		t.Errorf("op not truncated: %d bytes", len(op))
	}
}

func TestRateBucketKeptOnReattach(t *testing.T) {
	e := newEnv(t)
	_, tok := e.attach("g1", &mycelv1.OllamaGrant{RequestsPerMinute: 2})
	e.do(http.MethodGet, "/api/tags", tok, "")
	e.do(http.MethodGet, "/api/tags", tok, "")
	e.attach("g1", &mycelv1.OllamaGrant{RequestsPerMinute: 2})
	if code, _ := e.do(http.MethodGet, "/api/tags", tok, ""); code != 429 {
		t.Errorf("same rate after re-attach: %d", code)
	}
	e.attach("g1", &mycelv1.OllamaGrant{RequestsPerMinute: 3})
	if code, _ := e.do(http.MethodGet, "/api/tags", tok, ""); code != 200 {
		t.Errorf("new rate after re-attach: %d", code)
	}
}

func TestCounterEstimates(t *testing.T) {
	// A non-streaming body too large to parse is charged at 4 bytes per token.
	c := &counter{}
	c.setMode("application/json")
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 17; i++ {
		c.write(chunk)
	}
	c.finish()
	if c.gen != 17<<20/4 {
		t.Errorf("large body estimate %d", c.gen)
	}
	// An NDJSON stream without its final object counts chunks, and the final
	// eval_count replaces the estimate.
	c = &counter{}
	c.setMode("application/x-ndjson")
	c.write([]byte(`{"response":"a","done":false}` + "\n" + `{"response":"b","done":false}` + "\n"))
	if c.running() != 2 {
		t.Errorf("running %d", c.running())
	}
	c.write([]byte(`{"done":true,"eval_count":9}` + "\n"))
	c.finish()
	if c.gen != 9 {
		t.Errorf("final %d", c.gen)
	}
}
