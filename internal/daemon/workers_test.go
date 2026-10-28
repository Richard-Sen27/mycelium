package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func testManifest(name, mode, usage string) []byte {
	return []byte(fmt.Sprintf(`name: %[1]s
spawn:
  command: "{{mycel}}"
  args: ["-test.run=^TestHelperProcess$", "--", "%[2]s", "{{prompt}}", "{{mcp_config_file}}", "{{max_turns}}", "{{env:MYCEL_FAKE_X}}"]
  mcp_config:
    mcpServers:
      mycel:
        command: "{{mycel}}"
        args: ["mcp", "--agent", "{{agent}}", "--harness", "{{harness}}"]
  mcp:
    instructions: ignored by the daemon
  usage: %[3]s
  max_turns_default: 7
  prompt: "agent={{agent}} parent={{parent}} channel={{channel}} task={{task}}"
`, name, mode, usage))
}

func init() {
	testManifests = map[string][]byte{
		"test-echo":        testManifest("test-echo", "echo", usageClaudeStreamJSON),
		"test-sleep":       testManifest("test-sleep", "sleep", usageNone),
		"test-usage-sleep": testManifest("test-usage-sleep", "usage-sleep", usageClaudeStreamJSON),
		"test-usage-loop":  testManifest("test-usage-loop", "usage-loop", usageClaudeStreamJSON),
		"test-family":      testManifest("test-family", "family", usageNone),
		"test-hostile":     testManifest("test-hostile", "hostile", usageClaudeStreamJSON),
		"test-fail":        testManifest("test-fail", "fail", ""),
		"test-nospawn":     []byte("name: test-nospawn\ndisplay_name: no spawn\n"),
		"test-badenv": []byte(`name: test-badenv
spawn:
  command: "{{mycel}}"
  args: ["{{env:HOME}}"]
`),
	}
}

// helperArgs returns the helper's arguments after "--", or nil when this
// is not a helper run (the normal test binary has no "--").
func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			return os.Args[i+1:]
		}
	}
	return nil
}

// TestHelperProcess is the fake harness: the test binary re-executed by the
// daemon with "-- <mode> ...".
func TestHelperProcess(t *testing.T) {
	args := helperArgs()
	if args == nil {
		t.Skip("helper process only")
	}
	if args[0] == "tick" {
		// A child of the family helper: append to a file until killed.
		for i := 0; ; i++ {
			f, err := os.OpenFile(args[1], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = fmt.Fprintln(f, i)
				_ = f.Close()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	wd, _ := os.Getwd()
	fmt.Fprintf(os.Stderr, "args=%s\ncwd=%s\nlocal=%s\n", strings.Join(args, "|"), wd, os.Getenv("MYCEL_LOCAL_ADDR"))
	assistant := func(id string, in, out, cacheRead, cacheCreate int64) {
		fmt.Printf(`{"type":"assistant","message":{"id":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`+"\n",
			id, in, out, cacheRead, cacheCreate)
	}
	switch args[0] {
	case "echo":
		fmt.Println(`{"type":"system","subtype":"init"}`)
		assistant("m1", 10, 5, 100, 7)
		assistant("m1", 10, 5, 100, 7) // second content block of m1
		fmt.Println("not json")
		assistant("m2", 20, 10, 0, 0)
		fmt.Println(`{"type":"result","subtype":"success","num_turns":2,"total_cost_usd":0.25,"usage":{"input_tokens":30,"output_tokens":15,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}`)
		os.Exit(0)
	case "hostile":
		assistant("a", math.MaxInt64, 0, 0, 0)
		assistant("b", 0, math.MaxInt64, 0, 0)
		fmt.Println(`{"type":"assistant","message":{"id":"c","usage":{"input_tokens":99999999999999999999}}}`)
		assistant("d", -50, -1, 0, -3)
		fmt.Println(`{"type":"result","total_cost_usd":1e300,"num_turns":1000000000000000000}`)
		os.Exit(0)
	case "sleep":
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stderr, "ready")
		time.Sleep(time.Hour)
	case "usage-sleep":
		assistant("m1", 1, 1, 0, 0)
		time.Sleep(time.Hour)
	case "usage-loop":
		for i := range 20 {
			assistant(fmt.Sprintf("m%d", i), 1, 1, 0, 0)
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "done")
		time.Sleep(time.Hour)
	case "family":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "tick", filepath.Join(wd, "tick.log")) // #nosec G204 G702 -- the test binary itself
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		fmt.Fprintln(os.Stderr, "ready")
		time.Sleep(time.Hour)
	case "fail":
		os.Exit(3)
	}
	os.Exit(2)
}

// ---- helpers ----

func workerTweak(c *runConfig) {
	c.usageReportEvery = 100 * time.Millisecond
	c.workerKillGrace = time.Second
}

func spawnReq(harness string, extra map[string]any) map[string]any {
	b := map[string]any{"harness": harness, "task": "do it", "channel": "#task-x", "workdir": os.TempDir()}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func (c *localClient) spawn(session string, body map[string]any) (int, []byte) {
	c.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(http.MethodPost, "/api/local/agents/"+session+"/spawn", string(b))
}

func (c *localClient) mustSpawn(session string, body map[string]any) spawnReply {
	c.t.Helper()
	code, b := c.spawn(session, body)
	if code != http.StatusCreated {
		c.t.Fatalf("spawn: %d %s", code, b)
	}
	var sr spawnReply
	if err := json.Unmarshal(b, &sr); err != nil {
		c.t.Fatal(err)
	}
	return sr
}

// deleteWorker sends DELETE /api/local/workers/{agent} with an optional
// Mycel-Session header. It is safe to call from any goroutine.
func (c *localClient) deleteWorker(agent, session string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodDelete, "http://"+c.addr+"/api/local/workers/"+agent, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (c *localClient) workers() []workerEntry {
	c.t.Helper()
	code, b := c.do(http.MethodGet, "/api/local/workers", "")
	if code != http.StatusOK {
		c.t.Fatalf("workers: %d %s", code, b)
	}
	var out struct {
		Workers []workerEntry `json:"workers"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		c.t.Fatal(err)
	}
	return out.Workers
}

func (c *localClient) worker(agentID string) *workerEntry {
	c.t.Helper()
	for _, w := range c.workers() {
		if w.AgentID == agentID {
			return &w
		}
	}
	return nil
}

func (c *localClient) waitWorkerState(agentID, state string) *workerEntry {
	c.t.Helper()
	var got *workerEntry
	waitFor(c.t, 10*time.Second, agentID+" "+state, func() bool {
		got = c.worker(agentID)
		return got != nil && got.State == state
	})
	return got
}

func ctlAgent(t *testing.T, e *localEnv, agentID, action, reason string) *mycelv1.CtlResponse {
	t.Helper()
	return ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Agent{
		Agent: &mycelv1.AgentCtl{AgentId: agentID, Action: action, Reason: reason},
	}})
}

// waitReady waits until a helper printed "ready" (its signal handling is
// set up).
func waitReady(t *testing.T, e *localEnv, name string) {
	t.Helper()
	path := filepath.Join(e.dir, WorkersDir, name, "stderr.log")
	waitFor(t, 10*time.Second, name+" ready", func() bool {
		b, _ := os.ReadFile(path)
		return strings.Contains(string(b), "ready\n")
	})
}

func waitExit(t *testing.T, e *localEnv, agentID string) *mycelv1.AgentExitedRequest {
	t.Helper()
	waitFor(t, 10*time.Second, "exit report of "+agentID, func() bool { return len(e.fa.exits(agentID)) > 0 })
	ex := e.fa.exits(agentID)
	if len(ex) != 1 {
		t.Fatalf("%d exit reports", len(ex))
	}
	return ex[0]
}

func processGone(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return p.Signal(syscall.Signal(0)) != nil
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ---- tests ----

func TestSpawnValidation(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	noWorkdir := spawnReq("test-echo", nil)
	delete(noWorkdir, "workdir")
	for _, tc := range []struct {
		body map[string]any
		code int
		msg  string
	}{
		{spawnReq("nope", nil), 400, "test-echo"},
		{spawnReq("test-nospawn", nil), 400, "cannot spawn"},
		{spawnReq("Bad Harness", nil), 400, "invalid harness"},
		{spawnReq("test-echo", map[string]any{"profile": "default"}), 400, "phase 4"},
		{spawnReq("test-echo", map[string]any{"name": "Bad_Name"}), 400, "invalid worker name"},
		{spawnReq("test-echo", map[string]any{"task": " "}), 400, "task"},
		{spawnReq("test-echo", map[string]any{"task": strings.Repeat("a", 17<<10)}), 400, "16 KiB"},
		{spawnReq("test-echo", map[string]any{"channel": ""}), 400, "channel"},
		{spawnReq("test-echo", map[string]any{"max_turns": 5000}), 400, "max_turns"},
		{spawnReq("test-echo", map[string]any{"budget": map[string]any{"tokens": -1}}), 400, "budget"},
		{spawnReq("test-echo", map[string]any{"env": map[string]string{"A": "x"}}), 400, "env is not accepted"},
		{spawnReq("test-echo", map[string]any{"env": map[string]string{}}), 400, "env is not accepted"},
		{noWorkdir, 400, "workdir is required"},
		{spawnReq("test-echo", map[string]any{"workdir": "rel/dir"}), 400, "absolute"},
		{spawnReq("test-echo", map[string]any{"workdir": missing}), 400, "not an existing directory"},
		{spawnReq("test-echo", map[string]any{"workdir": file}), 400, "not an existing directory"},
		{spawnReq("test-badenv", nil), 400, "MYCEL_FAKE_"},
		{spawnReq("test-echo", map[string]any{"name": "boss"}), 409, "already has a local session"},
	} {
		code, b := e.c.spawn(boss.Session, tc.body)
		if code != tc.code || !strings.Contains(string(b), tc.msg) {
			t.Errorf("spawn %v: %d %s; want %d containing %q", tc.body, code, b, tc.code, tc.msg)
		}
	}
	if code, _ := e.c.spawn("nosuchsession", spawnReq("test-echo", nil)); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
	if len(e.c.workers()) != 0 {
		t.Fatal("a refused spawn left a worker")
	}

	// At most 8 running workers.
	for i := range maxWorkers {
		sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", nil))
		if want := fmt.Sprintf("agent:boss-w%d@n1", i+1); sr.AgentID != want {
			t.Fatalf("worker %d: %s, want %s", i, sr.AgentID, want)
		}
	}
	code, b := e.c.spawn(boss.Session, spawnReq("test-sleep", nil))
	if code != http.StatusTooManyRequests {
		t.Fatalf("ninth worker: %d %s", code, b)
	}
	if s := getStatus(t, e.r.addr); s.Workers != maxWorkers {
		t.Fatalf("status workers %d", s.Workers)
	}
	// A worker's own session may not spawn.
	_, ws := e.c.register("boss-w1", "test-sleep")
	if code, b := e.c.spawn(ws.Session, spawnReq("test-sleep", nil)); code != http.StatusForbidden || !strings.Contains(string(b), "workers cannot spawn workers") {
		t.Fatalf("worker spawn: %d %s", code, b)
	}
}

func TestSpawnEchoReportsUsageAndExit(t *testing.T) {
	t.Setenv("MYCEL_FAKE_X", "hello")
	t.Setenv("MYCEL_LOCAL_ADDR", "127.0.0.1:1") // the daemon's own value wins
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	workdir := t.TempDir()
	regsBefore, _ := e.fa.counts()
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-echo", map[string]any{
		"task": "do {{agent}} now", "channel": "task-x", "workdir": workdir,
		"budget": map[string]any{"tokens": 1000, "usd": 2.5},
	}))
	const id = "agent:boss-w1@n1"
	if sr.AgentID != id || sr.Worker != id {
		t.Fatalf("spawn reply %+v", sr)
	}
	if regs, _ := e.fa.counts(); regs != regsBefore+1 {
		t.Fatalf("hub registrations %d -> %d", regsBefore, regs)
	}
	reg := e.fa.registered("boss-w1")
	if reg == nil || reg.GetParent() != "agent:boss@n1" || reg.GetHomeChannel() != "#task-x" ||
		reg.GetHarness() != "test-echo" || reg.GetBudget().GetTokens() != 1000 || reg.GetBudget().GetUsd() != 2.5 ||
		reg.GetWorkdir() != mustEval(t, workdir) {
		t.Fatalf("register request %v", reg)
	}

	w := e.c.waitWorkerState(id, workerExited)
	if w.ExitCode == nil || *w.ExitCode != 0 || w.ExitReason != exitFinished || w.PID <= 0 || w.StartedAt == nil || w.ExitedAt == nil ||
		w.Parent != "agent:boss@n1" || w.Harness != "test-echo" {
		t.Fatalf("worker %+v", w)
	}
	// m1 counted once (duplicate content block), cache reads excluded.
	if w.Usage.Tokens != 52 || w.Usage.USD != 0.25 || w.Usage.Turns != 2 {
		t.Fatalf("usage %+v", w.Usage)
	}
	if ex := waitExit(t, e, id); ex.GetExitCode() != 0 || ex.GetReason() != exitFinished {
		t.Fatalf("exit report %v", ex)
	}
	tokens, usd, turns, _ := e.fa.usageTotals(id)
	if tokens != 52 || usd != 0.25 || turns != 2 {
		t.Fatalf("hub usage totals %d %v %d", tokens, usd, turns)
	}
	waitFor(t, 5*time.Second, "worker session ended", func() bool {
		for _, a := range e.c.list() {
			if a.AgentID == id {
				return false
			}
		}
		return true
	})
	if s := getStatus(t, e.r.addr); s.Workers != 0 {
		t.Fatalf("status workers %d", s.Workers)
	}

	dir := filepath.Join(e.dir, WorkersDir, "boss-w1")
	mcpPath := filepath.Join(dir, "mcp.json")
	if fi, err := os.Stat(mcpPath); err != nil || (!isWindows() && fi.Mode().Perm() != 0o600) {
		t.Fatalf("mcp.json: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || (!isWindows() && fi.Mode().Perm() != 0o700) {
		t.Fatalf("worker dir: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, mcpPath), &cfg); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	srv := cfg.MCPServers["mycel"]
	if srv.Command != exe || !slices.Equal(srv.Args, []string{"mcp", "--agent", "boss-w1", "--harness", "test-echo", "--local-addr", e.r.addr}) {
		t.Fatalf("mcp config %+v", srv)
	}
	stderr := string(mustRead(t, filepath.Join(dir, "stderr.log")))
	wantArgs := "args=echo|agent=boss-w1 parent=agent:boss@n1 channel=#task-x task=do {{agent}} now|" + mcpPath + "|7|hello"
	for _, want := range []string{wantArgs, "local=" + e.r.addr + "\n"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr.log %q lacks %q", stderr, want)
		}
	}
	gotWD := ""
	for _, line := range strings.Split(stderr, "\n") {
		if v, ok := strings.CutPrefix(line, "cwd="); ok {
			gotWD = v
		}
	}
	if mustEval(t, gotWD) != mustEval(t, workdir) {
		t.Fatalf("worker cwd %q, want %q", gotWD, workdir)
	}
}

func TestSpawnFailingWorker(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-fail", map[string]any{"name": "w1"}))
	w := e.c.waitWorkerState(sr.AgentID, workerExited)
	if w.ExitCode == nil || *w.ExitCode != 3 || w.ExitReason != "error: exit 3" {
		t.Fatalf("worker %+v", w)
	}
	if r := waitExit(t, e, sr.AgentID); r.GetExitCode() != 3 || r.GetReason() != "error: exit 3" {
		t.Fatalf("exit report %v", r)
	}
	if _, _, _, n := e.fa.usageTotals(sr.AgentID); n != 0 {
		t.Fatal("usage reported for parser none")
	}
	// The name is free again once the worker exited.
	e.c.mustSpawn(boss.Session, spawnReq("test-fail", map[string]any{"name": "w1"}))
}

func TestWorkerSessionPauseResumeStop(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	w := e.c.waitWorkerState(sr.AgentID, workerRunning)
	if w.PID <= 0 || w.StartedAt == nil || w.ExitCode != nil {
		t.Fatalf("worker %+v", w)
	}
	if s := getStatus(t, e.r.addr); s.Workers != 1 {
		t.Fatalf("status workers %d", s.Workers)
	}

	// The worker's own `mycel mcp` gets the existing session, no hub call.
	regs, _ := e.fa.counts()
	code, rr := e.c.register("w1", "test-sleep")
	if code != http.StatusCreated || rr.AgentID != sr.AgentID {
		t.Fatalf("worker register: %d %+v", code, rr)
	}
	if regs2, _ := e.fa.counts(); regs2 != regs {
		t.Fatal("worker register called the hub")
	}
	// Its MCP process ending the session does not end the worker's session.
	if code, _ := e.c.do(http.MethodDelete, "/api/local/agents/"+rr.Session, ""); code != http.StatusNoContent {
		t.Fatalf("delete worker session: %d", code)
	}
	if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusOK {
		t.Fatalf("whoami after session delete: %d", code)
	}

	if resp := ctlAgent(t, e, sr.AgentID, "pause", "budget exhausted"); !resp.GetOk() {
		t.Fatalf("pause: %v", resp)
	}
	e.c.waitWorkerState(sr.AgentID, workerPaused)
	code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#task-x","text":"hi"}}`)
	if code != http.StatusLocked || !strings.Contains(string(b), "agent paused: budget exhausted") {
		t.Fatalf("post while paused: %d %s", code, b)
	}
	for _, m := range []string{"whoami", "bb.get"} {
		if code, b := e.c.rpc(rr.Session, m, ""); code != http.StatusOK {
			t.Fatalf("%s while paused: %d %s", m, code, b)
		}
	}
	if resp := ctlAgent(t, e, sr.AgentID, "resume", ""); !resp.GetOk() {
		t.Fatalf("resume: %v", resp)
	}
	e.c.waitWorkerState(sr.AgentID, workerRunning)
	if code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#task-x","text":"hi"}}`); code != http.StatusOK {
		t.Fatalf("post after resume: %d %s", code, b)
	}

	// Pause again, then stop: a stopped group that ignores SIGTERM must
	// still die.
	waitReady(t, e, "w1")
	ctlAgent(t, e, sr.AgentID, "pause", "x")
	if resp := ctlAgent(t, e, sr.AgentID, "stop", "kill switch"); !resp.GetOk() {
		t.Fatalf("stop: %v", resp)
	}
	// During the grace period the session is refused.
	if code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#task-x","text":"hi"}}`); code != http.StatusLocked && code != http.StatusNotFound {
		t.Fatalf("post while stopping: %d %s", code, b)
	}
	w = e.c.waitWorkerState(sr.AgentID, workerExited)
	if w.ExitReason != exitStopped {
		t.Fatalf("worker %+v", w)
	}
	if r := waitExit(t, e, sr.AgentID); r.GetReason() != exitStopped {
		t.Fatalf("exit report %v", r)
	}
	if !processGone(w.PID) {
		t.Fatal("worker process still alive")
	}
	if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusNotFound {
		t.Fatalf("worker session after exit: %d", code)
	}
	if resp := ctlAgent(t, e, sr.AgentID, "pause", ""); resp.GetOk() || !strings.HasPrefix(resp.GetError(), "not_found") {
		t.Fatalf("pause after exit: %v", resp)
	}
}

func TestPlainSessionAgentCtl(t *testing.T) {
	e := startLocal(t, nil)
	rr := e.c.mustRegister("coder")
	if resp := ctlAgent(t, e, rr.AgentID, "pause", "kill switch"); !resp.GetOk() {
		t.Fatalf("pause: %v", resp)
	}
	code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#general","text":"x"}}`)
	if code != http.StatusLocked || !strings.Contains(string(b), `"agent paused: kill switch"`) {
		t.Fatalf("post while paused: %d %s", code, b)
	}
	if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusOK {
		t.Fatalf("whoami while paused: %d", code)
	}
	ctlAgent(t, e, rr.AgentID, "resume", "")
	if code, _ := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#general","text":"x"}}`); code != http.StatusOK {
		t.Fatalf("post after resume: %d", code)
	}
	if resp := ctlAgent(t, e, rr.AgentID, "jump", ""); resp.GetOk() {
		t.Fatal("unknown action accepted")
	}
	if resp := ctlAgent(t, e, "agent:coder@other", "pause", ""); resp.GetOk() {
		t.Fatal("foreign agent accepted")
	}
	if resp := ctlAgent(t, e, rr.AgentID, "stop", ""); !resp.GetOk() {
		t.Fatalf("stop: %v", resp)
	}
	if code, _ := e.c.rpc(rr.Session, "whoami", ""); code != http.StatusNotFound {
		t.Fatalf("session after stop: %d", code)
	}
}

func TestRegisterReplyPaused(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	e.fa.set(func(f *fakeAgents) { f.registerPaused = "budget: agent" })
	rr := e.c.mustRegister("coder")
	code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#general","text":"x"}}`)
	if code != http.StatusLocked || !strings.Contains(string(b), "agent paused: paused by hub: budget: agent") {
		t.Fatalf("post: %d %s", code, b)
	}
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	e.c.waitWorkerState(sr.AgentID, workerPaused)
	_, ws := e.c.register("w1", "test-sleep")
	if code, _ := e.c.rpc(ws.Session, "post", `{"post":{"channel":"#task-x","text":"x"}}`); code != http.StatusLocked {
		t.Fatalf("worker post: %d", code)
	}
}

func TestDeleteWorker(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	other := e.c.mustRegister("other")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	e.c.waitWorkerState(sr.AgentID, workerRunning)
	if code, _, _ := e.c.deleteWorker("agent:nope@n1", ""); code != http.StatusNotFound {
		t.Fatalf("unknown worker: %d", code)
	}
	if code, _ := e.c.doWith("", http.MethodDelete, "/api/local/workers/"+sr.AgentID, ""); code != http.StatusUnauthorized {
		t.Fatalf("without token: %d", code)
	}
	// The worker outlives its parent's session (`mycel agent spawn` uses a
	// short-lived one) and keeps its parent recorded.
	if code, _ := e.c.do(http.MethodDelete, "/api/local/agents/"+boss.Session, ""); code != http.StatusNoContent {
		t.Fatalf("delete parent session: %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	if w := e.c.worker(sr.AgentID); w == nil || w.State != workerRunning || w.Parent != "agent:boss@n1" {
		t.Fatalf("worker after parent left: %+v", w)
	}
	for _, sid := range []string{other.Session, boss.Session, "nosuchsession"} {
		if code, _, _ := e.c.deleteWorker(sr.AgentID, sid); code != http.StatusForbidden {
			t.Fatalf("delete with session %s: %d", sid, code)
		}
	}
	// The orchestrator registered again: its new session may stop it.
	boss2 := e.c.mustRegister("boss")
	if code, b, _ := e.c.deleteWorker(sr.AgentID, boss2.Session); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	w := e.c.waitWorkerState(sr.AgentID, workerExited)
	if w.ExitReason != exitStopped {
		t.Fatalf("worker %+v", w)
	}
	if r := waitExit(t, e, sr.AgentID); r.GetReason() != exitStopped {
		t.Fatalf("exit report %v", r)
	}
	if code, _, _ := e.c.deleteWorker("w1", ""); code != http.StatusConflict {
		t.Fatalf("delete exited: %d", code)
	}
}

func TestHubPausedReplyPausesWorker(t *testing.T) {
	e := startLocal(t, workerTweak)
	e.fa.set(func(f *fakeAgents) { f.pauseOnUsage = true })
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-usage-sleep", map[string]any{"name": "w1"}))
	w := e.c.waitWorkerState(sr.AgentID, workerPaused)
	if w.Usage.Tokens != 2 || w.Usage.Turns != 1 {
		t.Fatalf("usage %+v", w.Usage)
	}
	if tokens, _, turns, _ := e.fa.usageTotals(sr.AgentID); tokens != 2 || turns != 1 {
		t.Fatalf("hub usage %d %d", tokens, turns)
	}
	_, rr := e.c.register("w1", "test-usage-sleep")
	if code, b := e.c.rpc(rr.Session, "post", `{"post":{"channel":"#task-x","text":"x"}}`); code != http.StatusLocked {
		t.Fatalf("post: %d %s", code, b)
	}
}

func TestUsageRefusedIsDropped(t *testing.T) {
	e := startLocal(t, workerTweak)
	e.fa.set(func(f *fakeAgents) { f.usageErr = "invalid_argument: usage report too large" })
	boss := e.c.mustRegister("boss")
	e.c.mustSpawn(boss.Session, spawnReq("test-usage-sleep", map[string]any{"name": "w1"}))
	attempts := func() int {
		var n int
		e.fa.set(func(f *fakeAgents) { n = f.usageAttempts })
		return n
	}
	waitFor(t, 5*time.Second, "a usage report", func() bool { return attempts() > 0 })
	time.Sleep(500 * time.Millisecond)
	if n := attempts(); n != 1 {
		t.Fatalf("refused delta sent %d times", n)
	}
}

func TestUsageSurvivesHubOutage(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-usage-loop", map[string]any{"name": "w1"}))
	waitFor(t, 5*time.Second, "first usage report", func() bool {
		_, _, _, n := e.fa.usageTotals(sr.AgentID)
		return n > 0
	})
	port := e.h.busServer().Port()
	e.h.stopBus()
	time.Sleep(700 * time.Millisecond)
	e.h.startBus(port)
	e.fa.subscribe()
	waitFor(t, 20*time.Second, "all usage reported", func() bool {
		tokens, _, turns, _ := e.fa.usageTotals(sr.AgentID)
		return tokens == 40 && turns == 20
	})
	if w := e.c.worker(sr.AgentID); w.Usage.Tokens != 40 || w.Usage.Turns != 20 {
		t.Fatalf("worker usage %+v", w.Usage)
	}
}

func TestHostileStdout(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-hostile", map[string]any{"name": "w1"}))
	w := e.c.waitWorkerState(sr.AgentID, workerExited)
	if w.Usage.Tokens != math.MaxInt64 || w.Usage.Turns != math.MaxInt32 || w.Usage.USD != maxTotalUSD {
		t.Fatalf("usage %+v", w.Usage)
	}
	waitExit(t, e, sr.AgentID)
	e.fa.set(func(f *fakeAgents) {
		for _, u := range f.usageReqs {
			if u.GetTokens() < 0 || u.GetTokens() > maxReportTokens || u.GetUsd() < 0 || u.GetUsd() > maxReportUSD ||
				u.GetTurns() < 0 || u.GetTurns() > maxReportTurns {
				t.Errorf("report out of bounds: %v", u)
			}
		}
	})
}

func TestShutdownKillsWorkers(t *testing.T) {
	// The real grace period: SIGTERM is ignored by the helper, so the
	// daemon must SIGKILL it and still stop within 6 s.
	e := startLocal(t, func(c *runConfig) { c.usageReportEvery = 100 * time.Millisecond })
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	w := e.c.waitWorkerState(sr.AgentID, workerRunning)
	waitReady(t, e, "w1")
	start := time.Now()
	e.r.cancel()
	if err := e.r.wait(t, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if !processGone(w.PID) {
		t.Fatal("worker survived shutdown")
	}
	if ex := e.fa.exits(sr.AgentID); len(ex) != 1 || ex[0].GetReason() != exitKilled {
		t.Fatalf("exit reports %v", ex)
	}
}

func TestNodeRevokeStopsWorkers(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	w := e.c.waitWorkerState(sr.AgentID, workerRunning)
	waitReady(t, e, "w1")
	resp := ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Revoked{Revoked: &mycelv1.Revoked{Reason: "removed"}}})
	if !resp.GetOk() {
		t.Fatal(resp)
	}
	if err := e.r.wait(t, 10*time.Second); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run: %v", err)
	}
	if !processGone(w.PID) {
		t.Fatal("worker survived revocation")
	}
	if ex := e.fa.exits(sr.AgentID); len(ex) != 1 || ex[0].GetReason() != exitKilled {
		t.Fatalf("exit reports %v", ex)
	}
}

func TestAgentRevokeStopsWorker(t *testing.T) {
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	e.c.waitWorkerState(sr.AgentID, workerRunning)
	_, ws := e.c.register("w1", "test-sleep")
	e.fa.revoke(sr.AgentID)
	// A refused call kicks the refresh loop, which learns of the revocation.
	if code, _ := e.c.rpc(ws.Session, "whoami", ""); code != http.StatusForbidden {
		t.Fatalf("whoami after revoke: %d", code)
	}
	w := e.c.waitWorkerState(sr.AgentID, workerExited)
	if w.ExitReason != exitKilled || !processGone(w.PID) {
		t.Fatalf("worker %+v", w)
	}
	if r := waitExit(t, e, sr.AgentID); r.GetReason() != exitKilled {
		t.Fatalf("exit report %v", r)
	}
}

// startHook blocks worker starts until released and reports each start.
type startHook struct {
	entered chan string
	release chan struct{}
}

func newStartHook() *startHook {
	return &startHook{entered: make(chan string, 4), release: make(chan struct{})}
}

func (h *startHook) hook(name string) {
	h.entered <- name
	<-h.release
}

// spawnAsync starts a spawn whose start blocks in the hook.
func spawnAsync(t *testing.T, e *localEnv, h *startHook, session string, body map[string]any) <-chan int {
	t.Helper()
	b, _ := json.Marshal(body)
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+e.c.addr+"/api/local/agents/"+session+"/spawn", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+e.c.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker start not reached")
	}
	return done
}

func TestControlRacesStartingWorker(t *testing.T) {
	h := newStartHook()
	e := startLocal(t, func(c *runConfig) {
		workerTweak(c)
		c.workerStartHook = h.hook
	})
	boss := e.c.mustRegister("boss")

	// A pause while starting is applied once the process runs, and goes
	// through the worker, not its session.
	done := spawnAsync(t, e, h, boss.Session, spawnReq("test-sleep", map[string]any{"name": "w1"}))
	if w := e.c.worker("agent:w1@n1"); w == nil || w.State != workerStarting {
		t.Fatalf("worker %+v", w)
	}
	if resp := ctlAgent(t, e, "agent:w1@n1", "pause", "early"); !resp.GetOk() {
		t.Fatalf("pause: %v", resp)
	}
	h.release <- struct{}{}
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("spawn: %d", code)
	}
	e.c.waitWorkerState("agent:w1@n1", workerPaused)

	// A stop from the hub while starting stops the process once started;
	// it never ends the session behind the worker's back.
	done = spawnAsync(t, e, h, boss.Session, spawnReq("test-sleep", map[string]any{"name": "w2"}))
	if resp := ctlAgent(t, e, "agent:w2@n1", "stop", ""); !resp.GetOk() {
		t.Fatalf("stop: %v", resp)
	}
	if !slices.ContainsFunc(e.c.list(), func(a agentListEntry) bool { return a.AgentID == "agent:w2@n1" }) {
		t.Fatal("stop while starting ended the worker's session")
	}
	h.release <- struct{}{}
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("spawn: %d", code)
	}
	if w := e.c.waitWorkerState("agent:w2@n1", workerExited); w.ExitReason != exitStopped {
		t.Fatalf("worker %+v", w)
	}

	// The same for DELETE.
	done = spawnAsync(t, e, h, boss.Session, spawnReq("test-sleep", map[string]any{"name": "w3"}))
	if code, b, err := e.c.deleteWorker("agent:w3@n1", ""); err != nil || code != http.StatusNoContent {
		t.Fatalf("delete while starting: %d %s %v", code, b, err)
	}
	h.release <- struct{}{}
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("spawn: %d", code)
	}
	if w := e.c.waitWorkerState("agent:w3@n1", workerExited); w.ExitReason != exitStopped {
		t.Fatalf("worker %+v", w)
	}
	if r := waitExit(t, e, "agent:w3@n1"); r.GetReason() != exitStopped {
		t.Fatalf("exit report %v", r)
	}
}

func TestPauseSuspendsProcessGroup(t *testing.T) {
	if !canPause {
		t.Skip("no SIGSTOP on " + runtime.GOOS)
	}
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	dir := t.TempDir()
	tick := filepath.Join(dir, "tick.log")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-family", map[string]any{"name": "w1", "workdir": dir}))
	waitFor(t, 10*time.Second, "child ticking", func() bool { return fileSize(tick) > 0 })

	ctlAgent(t, e, sr.AgentID, "pause", "x")
	time.Sleep(200 * time.Millisecond)
	before := fileSize(tick)
	time.Sleep(400 * time.Millisecond)
	if after := fileSize(tick); after != before {
		t.Fatalf("child kept running while paused: %d -> %d", before, after)
	}
	ctlAgent(t, e, sr.AgentID, "resume", "")
	waitFor(t, 5*time.Second, "child ticking again", func() bool { return fileSize(tick) > before })

	if code, _, _ := e.c.deleteWorker(sr.AgentID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	e.c.waitWorkerState(sr.AgentID, workerExited)
	time.Sleep(200 * time.Millisecond)
	end := fileSize(tick)
	time.Sleep(300 * time.Millisecond)
	if fileSize(tick) != end {
		t.Fatal("child survived the worker")
	}
}

// ---- unit tests ----

func TestRender(t *testing.T) {
	t.Setenv("MYCEL_FAKE_SCRIPT", "script.yaml")
	t.Setenv("MYCEL_RENDER_TEST", "secret")
	v := renderVars{vals: map[string]string{"agent": "w1", "task": "say {{agent}}"}}
	got, err := v.render("{{agent}}:{{task}}:{{env:MYCEL_FAKE_SCRIPT}}:{{env:MYCEL_FAKE_UNSET}}:{{open")
	if err != nil || got != "w1:say {{agent}}:script.yaml::{{open" {
		t.Fatalf("render %q %v", got, err)
	}
	for _, bad := range []string{"{{nope}}", "{{env:MYCEL_RENDER_TEST}}", "{{env:HOME}}", "{{env:MYCEL_FAKE_A-B}}"} {
		if _, err := v.render(bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	tree, err := v.renderTree(map[string]any{"a": []any{"{{agent}}", 3}, "b": map[string]any{"c": "{{agent}}"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tree)
	if string(b) != `{"a":["w1",3],"b":{"c":"w1"}}` {
		t.Fatalf("tree %s", b)
	}
}

func TestScanLines(t *testing.T) {
	long := strings.Repeat("x", 300)
	in := "a\n" + long + "\nb\n\nc"
	var got []string
	scanLines(strings.NewReader(in), 100, func(l []byte) { got = append(got, strings.TrimSuffix(string(l), "\n")) })
	if !slices.Equal(got, []string{"a", "b", "", "c"}) {
		t.Fatalf("lines %q", got)
	}
	// A line longer than the reader's buffer but within max is kept.
	big := strings.Repeat("y", 200<<10)
	got = nil
	scanLines(strings.NewReader(big+"\nz\n"), 1<<20, func(l []byte) { got = append(got, string(l)) })
	if len(got) != 2 || len(got[0]) != len(big)+1 || got[1] != "z\n" {
		t.Fatalf("big line: %d lines", len(got))
	}
}

func TestStreamUsage(t *testing.T) {
	w := &worker{}
	for _, l := range []string{
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":5,"cache_read_input_tokens":1000}}}`,
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":5}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"assistant","message":{"content":[]}}`,
		`{"type":"user"}`,
		`{"type":"result","total_cost_usd":0.5}`,
		`{"type":"result","total_cost_usd":0.1,"usage":{"input_tokens":1}}`,
		`{"type":"assistant","message":{"id":"x","usage":{"input_tokens":1e40}}}`,
		`garbage`,
	} {
		w.applyStreamLine([]byte(l))
	}
	if w.tokens != 13 || w.turns != 2 || w.usd != 0.5 {
		t.Fatalf("totals %d %d %v", w.tokens, w.turns, w.usd)
	}
	w.applyStreamLine([]byte(`{"type":"result","num_turns":9}`))
	if w.turns != 9 {
		t.Fatalf("num_turns: %d", w.turns)
	}
	// The dedupe set is bounded.
	for i := range maxSeenIDs + 10 {
		w.applyStreamLine([]byte(fmt.Sprintf(`{"type":"assistant","message":{"id":"id%d","usage":{"output_tokens":1}}}`, i)))
	}
	if len(w.seen) > maxSeenIDs {
		t.Fatalf("seen set grew to %d", len(w.seen))
	}
}

func TestSaturatingAdds(t *testing.T) {
	if satAdd64(math.MaxInt64-1, 5) != math.MaxInt64 || satAdd64(5, -3) != 5 || satAdd64(1, 2) != 3 {
		t.Fatal("satAdd64")
	}
	if satAdd32(math.MaxInt32, 1) != math.MaxInt32 || satAdd32(1, -1) != 1 {
		t.Fatal("satAdd32")
	}
}

func TestMycelHome(t *testing.T) {
	t.Setenv("MYCEL_HOME", "")
	base := t.TempDir()
	if got := mycelHome(filepath.Join(base, "data", "node")); got != base {
		t.Fatalf("derived home %q", got)
	}
	if got := mycelHome(filepath.Join(base, "other")); got != "" {
		t.Fatalf("home %q", got)
	}
	t.Setenv("MYCEL_HOME", base)
	if got := mycelHome("/x"); got != base {
		t.Fatalf("env home %q", got)
	}
}
