package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func TestCheckWorkspacePath(t *testing.T) {
	for _, p := range []string{"a", "inputs/x.txt", "a/./b"} {
		if err := checkWorkspacePath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "a\\b", ".", "a/.."} {
		if err := checkWorkspacePath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestDispositionName(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="data.txt"`:        "data.txt",
		`attachment; filename="../../etc/pwd"`:   "pwd",
		`attachment; filename="a\\b.txt"`:        "b.txt",
		`attachment; filename=".."`:              "",
		`attachment`:                             "",
		``:                                       "",
		`attachment; filename="x` + "\x01" + `"`: "",
	}
	for in, want := range cases {
		if got := dispositionName(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// ---- jobLog ----

type logSink struct {
	mu     sync.Mutex
	next   uint64
	data   []byte
	recv   []uint64
	drop   map[uint64]bool // lose the first chunk with this seq
	block  chan struct{}   // if set, sends wait for it
	failN  int
	sends  int
	hubErr string
}

func (s *logSink) send(_ context.Context, c *mycelv1.JobLogChunk) (*mycelv1.JobLogAck, error) {
	s.mu.Lock()
	block := s.block
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	if s.hubErr != "" {
		return nil, &hubError{msg: s.hubErr}
	}
	if s.failN > 0 {
		s.failN--
		return nil, errHubTimeout
	}
	s.recv = append(s.recv, c.GetSeq())
	if s.next == 0 {
		s.next = 1
	}
	switch {
	case s.drop[c.GetSeq()]:
		delete(s.drop, c.GetSeq())
	case c.GetSeq() == s.next:
		s.data = append(s.data, c.GetData()...)
		s.next++
	}
	return &mycelv1.JobLogAck{Seq: s.next - 1}, nil
}

func noWarn(string, ...any) {}

func TestJobLogGapResend(t *testing.T) {
	s := &logSink{drop: map[uint64]bool{2: true}}
	l := newJobLog("job_1", 20*time.Millisecond, 1<<20, s.send, noWarn, nil)
	var want strings.Builder
	for i := range 5 {
		line := strings.Repeat(string(rune('a'+i)), 10) + "\n"
		want.WriteString(line)
		_, _ = l.Write([]byte(line))
		time.Sleep(40 * time.Millisecond)
	}
	big := strings.Repeat("z", 100<<10) // spans two chunks
	want.WriteString(big)
	_, _ = l.Write([]byte(big))
	l.close(time.Now().Add(5 * time.Second))
	s.mu.Lock()
	defer s.mu.Unlock()
	if string(s.data) != want.String() {
		t.Fatalf("hub has %d bytes, want %d", len(s.data), want.Len())
	}
	if countSeq(s.recv, 2) != 2 {
		t.Fatalf("seq 2 sent %d times, want 2 (recv %v)", countSeq(s.recv, 2), s.recv)
	}
	if s.recv[0] != 1 {
		t.Fatalf("first seq %d", s.recv[0])
	}
}

func TestJobLogDropsWhenHubBehind(t *testing.T) {
	block := make(chan struct{})
	s := &logSink{block: block}
	l := newJobLog("job_1", 10*time.Millisecond, 1000, s.send, noWarn, nil)
	_, _ = l.Write([]byte(strings.Repeat("a", 600)))
	time.Sleep(50 * time.Millisecond) // the sender now holds 600 bytes, blocked
	for range 5 {
		_, _ = l.Write([]byte(strings.Repeat("b", 600))) // over the cap: dropped
	}
	_, _ = l.Write([]byte(strings.Repeat("c", 100))) // fits again
	close(block)
	l.close(time.Now().Add(5 * time.Second))
	s.mu.Lock()
	defer s.mu.Unlock()
	got := string(s.data)
	if strings.Count(got, strings.TrimSpace(jobLogDroppedMarker)) != 1 {
		t.Fatalf("marker count != 1: %q", got)
	}
	if strings.Contains(got, "bb") || !strings.HasSuffix(got, strings.Repeat("c", 100)) {
		t.Fatalf("unexpected log %q", got)
	}
}

func TestJobLogTimerFlush(t *testing.T) {
	s := &logSink{}
	l := newJobLog("job_1", 100*time.Millisecond, 1<<20, s.send, noWarn, nil)
	defer l.close(time.Now().Add(time.Second))
	_, _ = l.Write([]byte("small"))
	time.Sleep(30 * time.Millisecond)
	s.mu.Lock()
	early := len(s.data)
	s.mu.Unlock()
	if early != 0 {
		t.Fatal("a small write was sent before the flush interval")
	}
	waitFor(t, 2*time.Second, "timer flush", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return string(s.data) == "small"
	})
}

// A hub that always acks seq-1 never lets the stream advance: the sender
// gives up instead of resending forever, and close still returns.
func TestJobLogNeverEndingLowAcks(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	send := func(_ context.Context, c *mycelv1.JobLogChunk) (*mycelv1.JobLogAck, error) {
		mu.Lock()
		sends++
		mu.Unlock()
		return &mycelv1.JobLogAck{Seq: c.GetSeq() - 1}, nil
	}
	var warned sync.Once
	gaveUp := make(chan struct{})
	l := newJobLog("job_1", 10*time.Millisecond, 1<<20, send, func(string, ...any) { warned.Do(func() { close(gaveUp) }) }, nil)
	_, _ = l.Write([]byte("x"))
	select {
	case <-gaveUp:
	case <-time.After(10 * time.Second):
		t.Fatal("sender did not give up")
	}
	start := time.Now()
	_, _ = l.Write([]byte("more"))
	l.close(time.Now().Add(5 * time.Second))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("close took %v", took)
	}
	mu.Lock()
	defer mu.Unlock()
	if sends != jobLogMaxLowAcks {
		t.Fatalf("%d sends, want %d", sends, jobLogMaxLowAcks)
	}
}

// close is observed while the hub keeps the sender busy.
func TestJobLogCloseWhileBusy(t *testing.T) {
	s := &logSink{}
	l := newJobLog("job_1", 5*time.Millisecond, 64<<20, s.send, noWarn, nil)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = l.Write(make([]byte, 32<<10))
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	l.close(time.Now().Add(500 * time.Millisecond))
	close(stop)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("close took %v", took)
	}
}

func TestJobLogTransientRetryAndRefusal(t *testing.T) {
	s := &logSink{failN: 2}
	l := newJobLog("job_1", 10*time.Millisecond, 1<<20, s.send, noWarn, nil)
	_, _ = l.Write([]byte("hello\n"))
	l.close(time.Now().Add(10 * time.Second))
	s.mu.Lock()
	if string(s.data) != "hello\n" {
		t.Fatalf("data %q", s.data)
	}
	s.mu.Unlock()

	finished := make(chan struct{})
	s2 := &logSink{hubErr: "failed_precondition: job finished"}
	l2 := newJobLog("job_2", 10*time.Millisecond, 1<<20, s2.send, noWarn, func() { close(finished) })
	_, _ = l2.Write([]byte("x"))
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("onFinished not called")
	}
	_, _ = l2.Write([]byte("y"))
	l2.close(time.Now().Add(time.Second))
	s2.mu.Lock()
	defer s2.mu.Unlock()
	if s2.sends != 1 {
		t.Fatalf("%d sends after the hub said finished", s2.sends)
	}
}

// ---- ctl refusals that need no backend ----

func createCtl(cs *mycelv1.CreateSandbox) *mycelv1.CtlRequest {
	return &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_CreateSandbox{CreateSandbox: cs}}
}

func TestCreateSandboxRefusals(t *testing.T) {
	e := startLocal(t, nil)
	hc := e.h.hubConn()
	base := func() *mycelv1.CreateSandbox {
		return &mycelv1.CreateSandbox{
			Job:     &mycelv1.Job{Id: "job_R1", Node: "n1", Spec: &mycelv1.JobSpec{Profile: "x", Command: []string{"true"}}},
			Profile: &mycelv1.Profile{Name: "x", Backends: []string{"firecracker"}, Image: "img"},
		}
	}
	cases := []struct {
		mut  func(*mycelv1.CreateSandbox)
		want string
	}{
		{func(*mycelv1.CreateSandbox) {}, "failed_precondition: no backend for profile x"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Node = "n2" }, "invalid_argument"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Id = "../x" }, "invalid_argument"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Env = map[string]string{"MYCEL_HOME": "/"} }, "invalid_argument: env"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Env = map[string]string{"HTTP_PROXY": "http://evil"} }, "invalid_argument: env"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Command = nil }, "invalid_argument"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Timeout = "soon" }, "invalid_argument"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Outputs = []string{"../etc"} }, "invalid_argument: output pattern"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Outputs = []string{"/abs"} }, "invalid_argument: invalid output pattern"},
		{func(c *mycelv1.CreateSandbox) { c.Job.Spec.Outputs = []string{"a["} }, "invalid_argument: output pattern"},
		{func(c *mycelv1.CreateSandbox) {
			c.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_A", Path: "../x"}}
		}, "invalid_argument: input art_A"},
		{func(c *mycelv1.CreateSandbox) {
			c.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_A", Path: "a/b"}, {Artifact: "art_B", Path: "a/./b"}}
		}, "invalid_argument: input path"},
		{func(c *mycelv1.CreateSandbox) {
			c.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "nope"}}
		}, "invalid_argument: invalid input artifact"},
		{func(c *mycelv1.CreateSandbox) {
			c.Profile.Egress = &mycelv1.EgressPolicy{Allow: []string{"pypi.org", "not a rule!"}}
		}, "invalid_argument: profile x: egress rule"},
	}
	for i, c := range cases {
		cs := base()
		c.mut(cs)
		resp := ctl(t, hc, "n1", createCtl(cs))
		if resp.GetOk() || !strings.HasPrefix(resp.GetError(), c.want) {
			t.Errorf("case %d: %+v, want %q", i, resp, c.want)
		}
	}
	resp := ctl(t, hc, "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_KillSandbox{KillSandbox: &mycelv1.KillSandbox{JobId: "job_nope"}}})
	if resp.GetOk() || !strings.HasPrefix(resp.GetError(), "not_found") {
		t.Fatalf("kill unknown: %+v", resp)
	}
	if s := getStatus(t, e.r.addr); s.Jobs != 0 || s.SandboxBackendsAvailable == nil || !strings.HasPrefix(s.EgressProxy, "127.0.0.1:") {
		t.Fatalf("status %+v", s)
	}
}

// ---- local artifact endpoints ----

func TestLocalArtifactExportFetch(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	code, b := e.c.do(http.MethodPost, "/api/local/agents/"+rr.Session+"/artifacts?name=notes.txt&channel=%23task-a", "artifact bytes")
	if code != http.StatusCreated {
		t.Fatalf("export: %d %s", code, b)
	}
	a := &mycelv1.Artifact{}
	if err := protojson.Unmarshal(b, a); err != nil || a.GetId() == "" {
		t.Fatalf("reply %s: %v", b, err)
	}
	stored := e.h.arts.byIDGet(a.GetId())
	if stored == nil || string(stored.data) != "artifact bytes" || stored.a.GetCreatedBy() != "agent:coder@n1" ||
		stored.a.GetChannel() != "#task-a" || stored.a.GetName() != "notes.txt" {
		t.Fatalf("stored %+v", stored)
	}

	code, b = e.c.do(http.MethodGet, "/api/local/agents/"+rr.Session+"/artifacts/"+a.GetId(), "")
	if code != http.StatusOK || string(b) != "artifact bytes" {
		t.Fatalf("fetch: %d %q", code, b)
	}
	e.h.arts.mu.Lock()
	agents := append([]string{}, e.h.arts.agents...)
	callers := append([]string{}, e.h.arts.callers...)
	e.h.arts.mu.Unlock()
	if len(agents) != 1 || agents[0] != "agent:coder@n1" {
		t.Fatalf("Mycel-Agent headers %v", agents)
	}
	for _, c := range callers {
		if c != "n1" {
			t.Fatalf("caller %q", c)
		}
	}

	code, b = e.c.do(http.MethodGet, "/api/local/agents/"+rr.Session+"/artifacts/art_missing", "")
	if code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", code, b)
	}
	var eb struct{ Error string }
	if json.Unmarshal(b, &eb) != nil || eb.Error == "" {
		t.Fatalf("error body %s", b)
	}
	if code, _ := e.c.do(http.MethodGet, "/api/local/agents/"+rr.Session+"/artifacts/..%2fx", ""); code != http.StatusBadRequest && code != http.StatusNotFound {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := e.c.do(http.MethodGet, "/api/local/agents/nosession/artifacts/"+a.GetId(), ""); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
	if code, _ := e.c.do(http.MethodPost, "/api/local/agents/"+rr.Session+"/artifacts", "x"); code != http.StatusBadRequest {
		t.Fatalf("no name: %d", code)
	}
	if code, _ := e.c.doWith("", http.MethodGet, "/api/local/agents/"+rr.Session+"/artifacts/"+a.GetId(), ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	e.h.arts.set(func(f *fakeArtifacts) { f.putErr = http.StatusUnauthorized })
	if code, _ := e.c.do(http.MethodPost, "/api/local/agents/"+rr.Session+"/artifacts?name=x", "x"); code != http.StatusForbidden {
		t.Fatalf("hub 401 maps to %d, want 403", code)
	}
}

func TestLocalArtifactLimits(t *testing.T) {
	e := startLocal(t, func(c *runConfig) { c.localArtifactMax = 1024 })
	rr := e.c.mustRegister("coder")
	path := "/api/local/agents/" + rr.Session + "/artifacts?name=big"
	big := strings.Repeat("x", 2048)
	// With Content-Length: refused before anything reaches the hub.
	if code, b := e.c.do(http.MethodPost, path, big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("with length: %d %s", code, b)
	}
	// Chunked: the cap trips while streaming (MaxBytesError).
	req, err := http.NewRequest(http.MethodPost, "http://"+e.c.addr+path, io.MultiReader(strings.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+e.c.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked: %d", res.StatusCode)
	}
	if len(e.h.arts.find(func(a *mycelv1.Artifact) bool { return a.GetName() == "big" })) != 0 {
		t.Fatal("an oversized artifact was stored")
	}
	// Within the cap it works, chunked too.
	req, _ = http.NewRequest(http.MethodPost, "http://"+e.c.addr+"/api/local/agents/"+rr.Session+"/artifacts?name=small", io.MultiReader(strings.NewReader("tiny")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+e.c.token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("small chunked: %d", res.StatusCode)
	}
	// Fetch is capped like export.
	e.h.arts.add("art_BIG", "big.bin", []byte(big))
	if code, _ := e.c.do(http.MethodGet, "/api/local/agents/"+rr.Session+"/artifacts/art_BIG", ""); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("fetch over cap: %d", code)
	}
}

func TestHubRedirectNotFollowed(t *testing.T) {
	e := startLocal(t, nil)
	var hit atomic.Bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		_, _ = w.Write([]byte("elsewhere"))
	}))
	t.Cleanup(other.Close)
	e.h.arts.set(func(f *fakeArtifacts) { f.redirect = other.URL + "/steal" })
	rr := e.c.mustRegister("coder")
	code, b := e.c.do(http.MethodGet, "/api/local/agents/"+rr.Session+"/artifacts/art_X", "")
	if code != http.StatusBadGateway || strings.Contains(string(b), "elsewhere") {
		t.Fatalf("redirect: %d %s", code, b)
	}
	if code, _ := e.c.do(http.MethodPost, "/api/local/agents/"+rr.Session+"/artifacts?name=a", "data"); code != http.StatusBadGateway {
		t.Fatalf("redirected put: %d", code)
	}
	if hit.Load() {
		t.Fatal("the redirect target was contacted")
	}
}

// An unreported job.done in a summary is sent again at startup.
func TestJobDoneReplayedAtStartup(t *testing.T) {
	h := newFakeHub(t)
	fa := newFakeAgents(h)
	dir := joinNode(t, h, "n1")
	jd := filepath.Join(dir, JobsDir, "job_RP1")
	if err := os.MkdirAll(jd, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := jobSummary{JobID: "job_RP1", State: "JOB_STATE_SUCCEEDED", ExitCode: 0, Artifacts: []string{"art_1"}, EgressBytes: 7}
	b, _ := json.Marshal(sum)
	if err := os.WriteFile(filepath.Join(jd, jobSummaryFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	cfg.jobDoneRetryEvery = 100 * time.Millisecond
	r := startRunWith(t, dir, cfg, p, sl)
	waitFor(t, 10*time.Second, "replayed job.done", func() bool {
		fa.jobs.mu.Lock()
		defer fa.jobs.mu.Unlock()
		d := fa.jobs.done["job_RP1"]
		return len(d) == 1 && d[0].GetState() == mycelv1.JobState_JOB_STATE_SUCCEEDED && d[0].GetStats().GetEgressBytes() == 7
	})
	waitFor(t, 5*time.Second, "summary marked reported", func() bool {
		b, err := os.ReadFile(filepath.Join(jd, jobSummaryFile))
		var s jobSummary
		return err == nil && json.Unmarshal(b, &s) == nil && s.Reported
	})
	// A retried CreateSandbox for it is refused, a kill is a no-op.
	resp := ctl(t, h.hubConn(), "n1", createCtl(&mycelv1.CreateSandbox{
		Job:     &mycelv1.Job{Id: "job_RP1", Node: "n1", Spec: &mycelv1.JobSpec{Command: []string{"true"}}},
		Profile: &mycelv1.Profile{Name: "x", Backends: []string{"fake"}},
	}))
	if resp.GetOk() || !strings.HasPrefix(resp.GetError(), "failed_precondition: job finished") {
		t.Fatalf("create: %+v", resp)
	}
	resp = ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_KillSandbox{KillSandbox: &mycelv1.KillSandbox{JobId: "job_RP1"}}})
	if !resp.GetOk() {
		t.Fatalf("kill finished: %+v", resp)
	}
	_ = r
}

func (f *fakeArtifacts) set(fn func(*fakeArtifacts)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}
