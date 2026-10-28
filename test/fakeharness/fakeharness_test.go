package fakeharness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSubstitute(t *testing.T) {
	stored := map[string]storedResult{
		"task": {text: "posted msg_01JABCDEF seq 3 in #task-x (thread none, hop 0)", expect: "posted msg_"},
		"name": {text: "worker"},
		"wait": {
			text: "[mycel message msg_AAA in #x from agent:a@n (untrusted) hop 0]\n> first\n[end mycel message]\n\n" +
				"[mycel message msg_BBB in #x from agent:b@n (untrusted) hop 1]\n> second msg_ZZZ\n[end mycel message]",
			expect: "> second",
		},
	}
	got, err := substitute(map[string]any{
		"reply_to": "{{task|msgid}}",
		"text":     "hi {{ name }}",
		"mentions": []any{"agent:{{name}}@beta"},
		"nested":   map[string]any{"x": "{{name}}"},
		"limit":    3,
		"matched":  "{{wait|msgid}}",
		"first":    "{{wait|msgid:> first}}",
		"header":   "{{wait|msgid:from agent:b@n}}",
		"nomatch":  "{{wait|msgid:absent}}",
	}, stored)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"reply_to": "msg_01JABCDEF",
		"text":     "hi worker",
		"mentions": []any{"agent:worker@beta"},
		"nested":   map[string]any{"x": "worker"},
		"limit":    3,
		"matched":  "msg_BBB",
		"first":    "msg_AAA",
		"header":   "msg_BBB",
		"nomatch":  "msg_AAA",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}

	for _, bad := range []string{"{{missing}}", "{{name|msgid}}", "{{name|upper}}"} {
		if _, err := substitute(map[string]any{"a": bad}, stored); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if got, err := substitute(nil, stored); err != nil || got != nil {
		t.Errorf("nil args: got %v, %v", got, err)
	}
}

func TestLoadScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	src := `[{"tool":"join_channel","args":{"channel":"#x"},"usage":{"input":120,"output":30,"usd":0.004}},
	{"tool":"wait_for_message","args":{"timeout_seconds":5},"expect":"hi","store":"m","repeat":3,"allow_error":true},
	{"sleep_ms":10},
	{"tool":"post","expect":"hop limit","expect_error":true,"expect_all":["a","b"]}]`
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScript(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 4 || s[1].Repeat != 3 || s[1].Store != "m" || !s[1].AllowError || s[1].Args["timeout_seconds"] != 5.0 || s[2].SleepMS != 10 {
		t.Fatalf("unexpected script: %+v", s)
	}
	if !s[3].ExpectError || !reflect.DeepEqual(s[3].ExpectAll, []string{"a", "b"}) {
		t.Fatalf("expect_error / expect_all: %+v", s[3])
	}
	if u := s[0].Usage; u == nil || *u != (StepUsage{Input: 120, Output: 30, USD: 0.004}) || s[1].Usage != nil {
		t.Fatalf("usage: %+v, %+v", s[0].Usage, s[1].Usage)
	}
	if _, err := LoadScript(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("missing file: want an error")
	}
}

func TestChildEnvAndLookup(t *testing.T) {
	t.Setenv("MYCEL_API_KEY", "secret")
	t.Setenv("GORACE", "log_path=/tmp/x")
	env := ChildEnv("MYCEL_HOME=/x", "MYCEL_LOCAL_ADDR=127.0.0.1:1")
	gorace := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "MYCEL_API_KEY=") {
			t.Fatal("parent MYCEL_API_KEY leaked into the child env")
		}
		if strings.HasPrefix(kv, "GORACE=") {
			gorace++
			if kv != "GORACE=log_path=/tmp/x halt_on_error=1" {
				t.Errorf("GORACE = %q", kv)
			}
		}
	}
	if gorace != 1 {
		t.Errorf("%d GORACE entries, want 1", gorace)
	}
	if lookupEnv(env, EnvLocalAddr) != "127.0.0.1:1" || lookupEnv(env, "MYCEL_HOME") != "/x" {
		t.Fatalf("lookupEnv: %v", env)
	}
	t.Setenv("GORACE", "halt_on_error=1")
	if g := lookupEnv(ChildEnv(), "GORACE"); g != "halt_on_error=1" {
		t.Errorf("GORACE with the option already set = %q", g)
	}
}

func TestTranscript(t *testing.T) {
	tr := Transcript{Calls: []Call{{Tool: "post", Text: "hop limit (4) reached", IsError: true}}}
	if !tr.Contains("hop limit") || tr.Contains("nope") {
		t.Fatal("Contains")
	}
	if !strings.Contains(tr.String(), "(error)") {
		t.Fatalf("String: %s", tr.String())
	}

	dir := filepath.Join(t.TempDir(), "tr")
	writeTranscript(dir, "agent:w1@alpha", tr, errors.New("boom"))
	got := readFile(t, filepath.Join(dir, "agent_w1@alpha.txt"))
	if !strings.Contains(got, "hop limit (4) reached") || !strings.Contains(got, "run failed") || !strings.Contains(got, "boom") {
		t.Fatalf("transcript file:\n%s", got)
	}
	writeTranscript(dir, "w2", Transcript{}, nil)
	if got := readFile(t, filepath.Join(dir, "w2.txt")); !strings.Contains(got, "run succeeded") {
		t.Fatalf("transcript file:\n%s", got)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- RunSession against an in-process server ----

type textIn struct {
	Text string `json:"text,omitempty"`
}

// testServer has the tools fail (always a tool error), nth (succeeds from
// its nth call on), echo (returns its text) and block (waits for the
// request's context).
type testServer struct {
	nth int

	mu    sync.Mutex
	calls map[string]int
}

func (s *testServer) count(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[tool]++
	return s.calls[tool]
}

func (s *testServer) callsOf(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[tool]
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// session starts an in-process server and returns a connected client session.
func session(t *testing.T, nth int) (*mcp.ClientSession, *testServer) {
	t.Helper()
	ts := &testServer{nth: nth, calls: map[string]int{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "fail"}, func(context.Context, *mcp.CallToolRequest, textIn) (*mcp.CallToolResult, any, error) {
		n := ts.count("fail")
		return nil, nil, fmt.Errorf("boom %d", n)
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "nth"}, func(context.Context, *mcp.CallToolRequest, textIn) (*mcp.CallToolResult, any, error) {
		n := ts.count("nth")
		if n < ts.nth {
			return textResult(fmt.Sprintf("not yet %d", n)), nil, nil
		}
		return textResult(fmt.Sprintf("ok %d", n)), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, any, error) {
		ts.count("echo")
		return textResult(in.Text), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "block"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ textIn) (*mcp.CallToolResult, any, error) {
		ts.count("block")
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Wait()
	})
	return cs, ts
}

func TestRunSessionRepeatUntilExpect(t *testing.T) {
	cs, ts := session(t, 3)
	var observed []Call
	tr, err := RunSession(context.Background(), cs, []Step{
		{Tool: "nth", Expect: "ok", Repeat: 5, Store: "r"},
		{Tool: "echo", Args: map[string]any{"text": "got {{r}}"}, Expect: "got ok 3"},
	}, func(c Call) { observed = append(observed, c) })
	if err != nil {
		t.Fatalf("%v\n%s", err, tr)
	}
	if ts.callsOf("nth") != 3 || len(tr.Calls) != 4 || len(observed) != 4 {
		t.Fatalf("nth calls %d, transcript %d, observed %d:\n%s", ts.callsOf("nth"), len(tr.Calls), len(observed), tr)
	}
	for i, c := range tr.Calls {
		if c.Index != i {
			t.Errorf("call %d has index %d", i, c.Index)
		}
	}
	if tr.Calls[3].Args["text"] != "got ok 3" {
		t.Errorf("substituted args: %v", tr.Calls[3].Args)
	}

	// Too few tries: the step fails with the last result.
	cs, _ = session(t, 3)
	_, err = RunSession(context.Background(), cs, []Step{{Tool: "nth", Expect: "ok", Repeat: 2}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not yet 2") || !strings.Contains(err.Error(), "step 0 (nth)") {
		t.Fatalf("want a mismatch error naming the last result, got %v", err)
	}
}

func TestRunSessionExpectAll(t *testing.T) {
	cs, ts := session(t, 2)
	tr, err := RunSession(context.Background(), cs, []Step{
		{Tool: "nth", ExpectAll: []string{"not yet 1", "ok 2"}, Repeat: 5},
		{Tool: "nth", ExpectAll: []string{"ok 3", "ok 4"}, Expect: "ok", Repeat: 5},
	}, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, tr)
	}
	if n := ts.callsOf("nth"); n != 4 {
		t.Fatalf("nth called %d times, want 4", n)
	}
	cs, _ = session(t, 1)
	_, err = RunSession(context.Background(), cs, []Step{{Tool: "nth", ExpectAll: []string{"ok 1", "never"}, Repeat: 2}}, nil)
	if err == nil || !strings.Contains(err.Error(), `never saw ["never"]`) {
		t.Fatalf("want a missing ExpectAll error, got %v", err)
	}
}

func TestRunSessionErrors(t *testing.T) {
	cs, ts := session(t, 1)
	ctx := context.Background()

	// A tool error fails the script...
	tr, err := RunSession(ctx, cs, []Step{{Tool: "fail"}, {Tool: "echo"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "tool error: boom 1") || len(tr.Calls) != 1 || !tr.Calls[0].IsError {
		t.Fatalf("tool error: %v\n%s", err, tr)
	}
	// ...unless allowed; Repeat retries an allowed error until Expect matches.
	tr, err = RunSession(ctx, cs, []Step{
		{Tool: "fail", AllowError: true},
		{Tool: "fail", Expect: "boom 4", AllowError: true, Repeat: 5},
		{Tool: "fail", Expect: "never", AllowError: true, Repeat: 2},
		{Tool: "echo", Args: map[string]any{"text": "{{missing}}"}, AllowError: true},
		{Tool: "echo", Args: map[string]any{"text": "after"}, Expect: "after"},
	}, nil)
	if err != nil {
		t.Fatalf("allowed errors: %v\n%s", err, tr)
	}
	if n := ts.callsOf("fail"); n != 1+1+2+2 {
		t.Errorf("fail called %d times, want 6", n)
	}
	if c := tr.Calls[len(tr.Calls)-2]; !c.IsError || !strings.Contains(c.Text, "substitution") {
		t.Errorf("substitution failure not recorded: %+v", c)
	}
	// A substitution failure that is not allowed fails the script.
	if _, err := RunSession(ctx, cs, []Step{{Tool: "echo", Args: map[string]any{"text": "{{missing}}"}}}, nil); err == nil {
		t.Error("substitution failure: want an error")
	}
}

func TestRunSessionExpectError(t *testing.T) {
	cs, _ := session(t, 3)
	ctx := context.Background()
	tr, err := RunSession(ctx, cs, []Step{
		{Tool: "fail", Expect: "boom", ExpectError: true},
		{Tool: "fail", Expect: "boom 3", ExpectError: true, Repeat: 3},
	}, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, tr)
	}
	// A success is not the expected error.
	_, err = RunSession(ctx, cs, []Step{{Tool: "echo", Args: map[string]any{"text": "boom"}, Expect: "boom", ExpectError: true}}, nil)
	if err == nil || !strings.Contains(err.Error(), "expected a tool error") {
		t.Fatalf("success with ExpectError: got %v", err)
	}
	// Nor is an error with other text.
	_, err = RunSession(ctx, cs, []Step{{Tool: "fail", Expect: "hop limit", ExpectError: true, Repeat: 2}}, nil)
	if err == nil || !strings.Contains(err.Error(), "after 2 tries") {
		t.Fatalf("other error with ExpectError: got %v", err)
	}
	// Successes are retried and then fail the step.
	_, err = RunSession(ctx, cs, []Step{{Tool: "nth", ExpectError: true, Repeat: 2}}, nil)
	if err == nil {
		t.Fatal("nth with ExpectError: want an error")
	}
}

func TestRunSessionSleep(t *testing.T) {
	cs, _ := session(t, 1)
	start := time.Now()
	tr, err := RunSession(context.Background(), cs, []Step{{SleepMS: 150}, {Tool: "echo", Args: map[string]any{"text": "x"}}}, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, tr)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("sleep took %s, want at least 150ms", d)
	}
	if len(tr.Calls) != 1 {
		t.Errorf("a sleep step made a call:\n%s", tr)
	}
	if _, err := RunSession(context.Background(), cs, []Step{{}}, nil); err == nil || !strings.Contains(err.Error(), "no tool") {
		t.Errorf("empty step: got %v", err)
	}
}

func TestRunSessionCancel(t *testing.T) {
	cs, ts := session(t, 1)

	// Cancelled while a tool call blocks.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	tr, err := RunSession(ctx, cs, []Step{{Tool: "block", AllowError: true}, {Tool: "echo"}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked call: got %v\n%s", err, tr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancellation took %s", d)
	}
	if ts.callsOf("echo") != 0 {
		t.Error("the step after the cancelled one ran")
	}

	// Cancelled while sleeping.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel2()
	}()
	if _, err := RunSession(ctx2, cs, []Step{{SleepMS: 60_000}}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep: got %v", err)
	}
}
