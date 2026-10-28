//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fh "github.com/richard-sen27/mycelium/test/fakeharness"
)

// Phase 3 "Done when": an orchestrator spawns two local workers that join a
// channel and report results; an exhausted budget pauses them. Plus operator
// pause, resume and stop. Workers run `mycel fakeharness` (harnesses/fake.yaml)
// with the script named by the node daemon's MYCEL_FAKE_SCRIPT; the script
// file is read when a worker starts, so a test rewrites it between spawns.

// fakeHarness is the manifest of the scripted worker harness.
const fakeHarness = "fake"

// p3 is one phase 3 network: a hub and the node alpha whose daemon spawns
// workers from script and has them write their transcripts to transcripts.
type p3 struct {
	h           *Hub
	alpha       *Node
	script      string
	transcripts string
	workdir     string
}

func startP3(t *testing.T, workerScript []fh.Step) *p3 {
	t.Helper()
	dir := t.TempDir()
	p := &p3{
		script:      filepath.Join(dir, "worker.json"),
		transcripts: filepath.Join(dir, "transcripts"),
		workdir:     t.TempDir(),
	}
	p.setScript(t, workerScript)
	p.h = startHub(t)
	p.alpha = joinNode(t, p.h, "alpha", "", "",
		"MYCEL_FAKE_SCRIPT="+p.script, fh.EnvTranscriptDir+"="+p.transcripts)
	if !waitFor(t, 15*time.Second, func() bool {
		st := p.alpha.Status(t)
		return st != nil && st["connected"] == true
	}) {
		t.Fatalf("alpha never connected: %v", p.alpha.Status(t))
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range p.transcriptNames() {
				t.Logf("==== worker %s transcript ====\n%s", name, p.transcript(name))
			}
		}
	})
	return p
}

// setScript (re)writes the worker script; workers started from now on run it.
func (p *p3) setScript(t *testing.T, steps []fh.Step) {
	t.Helper()
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.script, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// transcript returns a finished worker's transcript, "" while there is none.
func (p *p3) transcript(name string) string {
	b, err := os.ReadFile(filepath.Join(p.transcripts, name+".txt")) //nolint:gosec // test temp dir
	if err != nil {
		return ""
	}
	return string(b)
}

func (p *p3) transcriptNames() []string {
	ents, _ := os.ReadDir(p.transcripts)
	var names []string
	for _, e := range ents {
		names = append(names, strings.TrimSuffix(e.Name(), ".txt"))
	}
	return names
}

// spawnCLI spawns a worker with `mycel agent spawn` on alpha.
func (p *p3) spawnCLI(t *testing.T, name, channel, task string) {
	t.Helper()
	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+p.alpha.Home), "agent", "spawn", fakeHarness,
		"--task", task, "--channel", channel, "--name", name, "--workdir", p.workdir,
		"--local-addr", p.alpha.LocalAddr, "--json")
	if code != 0 {
		t.Fatalf("agent spawn %s: exit %d\nstdout: %s\nstderr: %s", name, code, out, errOut)
	}
	var res struct {
		AgentID string `json:"agent_id"`
	}
	mustJSON(t, out, &res)
	if want := "agent:" + name + "@alpha"; res.AgentID != want {
		t.Fatalf("agent spawn: agent_id %q, want %q", res.AgentID, want)
	}
}

// spawnStep is an orchestrator's spawn_agent call.
func (p *p3) spawnStep(name, channel, task string) fh.Step {
	return fh.Step{Tool: "spawn_agent", Args: map[string]any{
		"harness": fakeHarness, "task": task, "channel": channel, "name": name, "workdir": p.workdir,
	}, Expect: "spawned agent:" + name + "@alpha"}
}

// flexInt decodes protojson's int64 (a JSON string) as well as a number.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*f = flexInt(v)
	return err
}

type jsonBudget struct {
	Tokens flexInt `json:"tokens"`
	USD    float64 `json:"usd"`
}

type jsonAgent struct {
	ID           string     `json:"id"`
	NodeName     string     `json:"node_name"`
	Harness      string     `json:"harness"`
	State        string     `json:"state"`
	Parent       string     `json:"parent"`
	HomeChannel  string     `json:"home_channel"`
	Budget       jsonBudget `json:"budget"`
	Used         jsonBudget `json:"used"`
	Turns        int        `json:"turns"`
	PausedReason string     `json:"paused_reason"`
	ExitedAt     string     `json:"exited_at"`
	ExitCode     int        `json:"exit_code"`
}

// listAgents returns the hub's agents by id (`mycel agents ls --json`).
func listAgents(t *testing.T, h *Hub) map[string]jsonAgent {
	t.Helper()
	var resp struct {
		Agents []jsonAgent `json:"agents"`
	}
	mustJSON(t, h.MustCLI("", "agents", "ls", "--json"), &resp)
	m := make(map[string]jsonAgent, len(resp.Agents))
	for _, a := range resp.Agents {
		m[a.ID] = a
	}
	return m
}

type jsonChannelUsage struct {
	Channel string     `json:"channel"`
	Budget  jsonBudget `json:"budget"`
	Used    jsonBudget `json:"used"`
	Paused  bool       `json:"paused"`
}

func channelUsage(t *testing.T, h *Hub, ch string) jsonChannelUsage {
	t.Helper()
	var resp struct {
		Usage jsonChannelUsage `json:"usage"`
	}
	mustJSON(t, h.MustCLI("", "channels", "usage", ch, "--json"), &resp)
	return resp.Usage
}

type jsonWorker struct {
	AgentID  string `json:"agent_id"`
	Parent   string `json:"parent"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
	ExitCode int    `json:"exit_code"`
}

// localWorkers lists alpha's workers (`mycel workers ls --json`).
func (p *p3) localWorkers(t *testing.T) map[string]jsonWorker {
	t.Helper()
	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+p.alpha.Home), "workers", "ls", "--local-addr", p.alpha.LocalAddr, "--json")
	if code != 0 {
		t.Fatalf("workers ls: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	var resp struct {
		Workers []jsonWorker `json:"workers"`
	}
	mustJSON(t, out, &resp)
	m := make(map[string]jsonWorker, len(resp.Workers))
	for _, w := range resp.Workers {
		m[w.AgentID] = w
	}
	return m
}

// waitAgents polls `agents ls` until cond holds for every id; it returns the
// last listing and whether cond held.
func waitAgents(t *testing.T, h *Hub, timeout time.Duration, ids []string, cond func(jsonAgent) bool) (map[string]jsonAgent, bool) {
	t.Helper()
	var last map[string]jsonAgent
	ok := waitFor(t, timeout, func() bool {
		last = listAgents(t, h)
		for _, id := range ids {
			a, found := last[id]
			if !found || !cond(a) {
				return false
			}
		}
		return true
	})
	return last, ok
}

// hubEvents returns the texts of the hub's EVENT messages in ch.
func hubEvents(msgs []jsonMsg) []string {
	var out []string
	for _, m := range msgs {
		if m.From == "hub" && m.Type == "MESSAGE_TYPE_EVENT" {
			out = append(out, m.Payload.Text)
		}
	}
	return out
}

func anyContains(texts []string, sub string) bool {
	for _, s := range texts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestOrchestratorSpawnsWorkers(t *testing.T) {
	t.Parallel()
	const ch = "#task-p3"
	const orch = "agent:orch@alpha"
	workers := []string{"agent:w1@alpha", "agent:w2@alpha"}
	p := startP3(t, []fh.Step{
		join(ch),
		{Tool: "post", Args: map[string]any{"channel": ch, "text": "result: ok", "type": "result", "mentions": []any{orch}},
			Expect: "posted msg_", Usage: &fh.StepUsage{Input: 400, Output: 200}},
	})
	h := p.h

	// One wait step gathers both results, whether they arrive in one inbox
	// pull or in two (between EVENTs about the workers' exits).
	run := goRunHarness(t, p.alpha, "orch", fakeHarness, []fh.Step{
		join(ch),
		p.spawnStep("w1", ch, "do part 1"),
		p.spawnStep("w2", ch, "do part 2"),
		{Tool: "wait_for_message", Args: map[string]any{"timeout_seconds": 20}, Expect: "result: ok", Repeat: 8,
			ExpectAll: []string{"from agent:w1@alpha (untrusted) type result", "from agent:w2@alpha (untrusted) type result"}},
		{Tool: "list_workers", Expect: "agent:w1@alpha"},
	})
	otr := collect(t, run, 90*time.Second)
	if t.Failed() {
		t.FailNow()
	}
	if last := otr.Calls[len(otr.Calls)-1].Text; !strings.Contains(last, "agent:w2@alpha") {
		t.Errorf("list_workers lacks w2:\n%s", last)
	}

	agents, ok := waitAgents(t, h, 30*time.Second, workers, func(a jsonAgent) bool {
		return a.State == "stopped" && a.ExitedAt != "" && a.Used.Tokens >= 600
	})
	if !ok {
		t.Fatalf("workers did not stop with their usage reported within 30s: %+v", agents)
	}
	for _, id := range workers {
		a := agents[id]
		if a.Parent != orch || a.HomeChannel != ch || a.ExitCode != 0 || a.Harness != fakeHarness {
			t.Errorf("%s: parent %q home %q exit %d harness %q; want %s, %s, 0, %s",
				id, a.Parent, a.HomeChannel, a.ExitCode, a.Harness, orch, ch, fakeHarness)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(id, "agent:"), "@alpha")
		if tr := p.transcript(name); !strings.Contains(tr, "run succeeded") {
			t.Errorf("%s transcript does not end in success:\n%s", id, tr)
		}
	}
	if u := channelUsage(t, h, ch); u.Used.Tokens < 1200 || u.Paused {
		t.Errorf("%s usage %+v, want at least 1200 tokens and not paused", ch, u)
	}

	msgs := readChannel(t, h, ch)
	results := map[string]bool{}
	for _, m := range msgs {
		if m.Type == "MESSAGE_TYPE_RESULT" && m.Payload.Text == "result: ok" {
			results[m.From] = true
			if len(m.To) != 1 || m.To[0] != orch {
				t.Errorf("result from %s mentions %v, want %s", m.From, m.To, orch)
			}
		}
	}
	events := hubEvents(msgs)
	for _, id := range workers {
		if !results[id] {
			t.Errorf("%s has no result message from %s", ch, id)
		}
		if !anyContains(events, id+" exited") {
			t.Errorf("%s has no hub EVENT about %s exiting: %q", ch, id, events)
		}
	}
	if t.Failed() {
		t.Logf("orchestrator transcript:\n%s\nmessages: %+v", otr, msgs)
	}
}

func TestBudgetPausesWorkers(t *testing.T) {
	t.Parallel()
	const ch = "#task-budget"
	workers := []string{"agent:b1@alpha", "agent:b2@alpha"}

	// Each worker posts 5 times at 400 tokens within about a second, well
	// before its first usage report (every 5 s), so the first reports put
	// the channel at 4000 of 1000 tokens and nothing is reported after the
	// reset. Then it waits for messages: once resumed, its inbox holds the
	// hub's EVENTs about the pause.
	script := []fh.Step{join(ch)}
	for i := range 5 {
		script = append(script,
			fh.Step{Tool: "post", Args: map[string]any{"channel": ch, "text": "work " + strconv.Itoa(i)},
				AllowError: true, Usage: &fh.StepUsage{Input: 300, Output: 100}},
			fh.Step{SleepMS: 200})
	}
	script = append(script, fh.Step{Tool: "wait_for_message", Args: map[string]any{"timeout_seconds": 5},
		Expect: "paused", Repeat: 8, AllowError: true})
	p := startP3(t, script)
	h := p.h
	h.MustCLI("", "channels", "create", ch, "--type", "task", "--budget-tokens", "1000", "--open")

	// The orchestrator stays in a session while the workers run.
	run := goRunHarness(t, p.alpha, "orch", fakeHarness, []fh.Step{
		join(ch),
		p.spawnStep("b1", ch, "spend tokens"),
		p.spawnStep("b2", ch, "spend tokens"),
		{Tool: "wait_for_message", Args: map[string]any{"timeout_seconds": 5}, Expect: "never sent", Repeat: 6, AllowError: true},
	})

	agents, ok := waitAgents(t, h, 40*time.Second, workers, func(a jsonAgent) bool {
		return a.State == "paused" && a.PausedReason == "budget: channel"
	})
	if !ok {
		t.Fatalf("workers not paused for the channel budget within 40s: %+v\nusage: %+v", agents, channelUsage(t, h, ch))
	}
	if u := channelUsage(t, h, ch); !u.Paused || u.Used.Tokens < 1000 {
		t.Errorf("%s usage %+v, want paused with at least 1000 tokens used", ch, u)
	}
	if events := hubEvents(readChannel(t, h, ch)); !anyContains(events, "paused") {
		t.Errorf("%s has no hub EVENT about the pause: %q", ch, events)
	}

	// Usage parsed before the pause may still be in flight to the hub: wait
	// until a whole report interval passes without a change.
	used, since := channelUsage(t, h, ch).Used.Tokens, time.Now()
	waitFor(t, 20*time.Second, func() bool {
		if u := channelUsage(t, h, ch).Used.Tokens; u != used {
			used, since = u, time.Now()
		}
		return time.Since(since) > 6*time.Second
	})

	h.MustCLI("", "channels", "reset-usage", ch)
	agents, ok = waitAgents(t, h, 20*time.Second, workers, func(a jsonAgent) bool {
		return (a.State == "running" || a.State == "stopped") && a.PausedReason == ""
	})
	if !ok {
		t.Fatalf("workers still paused after reset-usage: %+v\nusage: %+v", agents, channelUsage(t, h, ch))
	}

	// Resumed, each worker reads the pause EVENTs and ends.
	if !waitFor(t, 40*time.Second, func() bool { return p.transcript("b1") != "" && p.transcript("b2") != "" }) {
		t.Errorf("worker transcripts missing after the resume: %v", p.transcriptNames())
	}
	if !strings.Contains(p.transcript("b1")+p.transcript("b2"), "paused") {
		t.Errorf("no worker transcript mentions the pause")
	}
	orchTr := collect(t, run, 60*time.Second)

	// Operator pause and resume of a fresh worker in the reset channel.
	if u := channelUsage(t, h, ch); u.Paused {
		t.Fatalf("%s paused again after the reset: %+v", ch, u)
	}
	// The pause below can reach the hub before c1's first call, which is
	// then refused ("agent paused"); c1 must keep running through that, so
	// its join may fail (it is a member of its home channel already).
	cJoin := join(ch)
	cJoin.AllowError = true
	p.setScript(t, []fh.Step{cJoin, {SleepMS: 60_000}})
	p.spawnCLI(t, "c1", ch, "wait")
	const c1 = "agent:c1@alpha"
	if got, ok := waitAgents(t, h, 15*time.Second, []string{c1}, func(a jsonAgent) bool { return a.State == "running" }); !ok {
		t.Fatalf("c1 not running: %+v", got[c1])
	}
	h.MustCLI("", "agents", "pause", c1, "--reason", "investigating")
	if got, ok := waitAgents(t, h, 10*time.Second, []string{c1}, func(a jsonAgent) bool {
		return a.State == "paused" && strings.HasPrefix(a.PausedReason, "operator")
	}); !ok {
		t.Errorf("c1 after an operator pause: %+v, want paused with an operator reason", got[c1])
	}
	if w := p.localWorkers(t)[c1]; w.State != "paused" {
		t.Errorf("alpha's worker c1 is %q after the pause, want paused", w.State)
	}
	h.MustCLI("", "agents", "resume", c1)
	if got, ok := waitAgents(t, h, 10*time.Second, []string{c1}, func(a jsonAgent) bool {
		return a.State == "running" && a.PausedReason == ""
	}); !ok {
		t.Errorf("c1 after resume: %+v, want running and no pause reason", got[c1])
	}
	if !waitFor(t, 10*time.Second, func() bool { return p.localWorkers(t)[c1].State == "running" }) {
		t.Errorf("alpha's worker c1 is %q after the resume, want running", p.localWorkers(t)[c1].State)
	}
	h.MustCLI("", "agents", "stop", c1)

	if t.Failed() {
		t.Logf("orchestrator transcript:\n%s\nusage: %+v\nagents: %+v", orchTr, channelUsage(t, h, ch), listAgents(t, h))
	}
}

func TestOperatorStopsWorker(t *testing.T) {
	t.Parallel()
	const ch = "#task-stop"
	const s1 = "agent:s1@alpha"
	p := startP3(t, []fh.Step{join(ch), {SleepMS: 60_000}})
	h := p.h
	p.spawnCLI(t, "s1", ch, "sleep")

	var w jsonWorker
	if !waitFor(t, 15*time.Second, func() bool {
		w = p.localWorkers(t)[s1]
		return w.State == "running" && w.PID > 0
	}) {
		t.Fatalf("s1 is not running with a pid: %+v", w)
	}
	if got, ok := waitAgents(t, h, 10*time.Second, []string{s1}, func(a jsonAgent) bool { return a.State == "running" }); !ok {
		t.Fatalf("hub does not show s1 running: %+v", got[s1])
	}

	h.MustCLI("", "agents", "stop", s1)
	stopped := time.Now()
	if !waitFor(t, 10*time.Second, func() bool {
		w = p.localWorkers(t)[s1]
		return w.State == "exited"
	}) {
		t.Fatalf("s1 still in alpha's running workers 10s after the stop: %+v", w)
	}
	t.Logf("s1 exited %s after the stop", time.Since(stopped).Round(time.Millisecond))
	if got, ok := waitAgents(t, h, 10*time.Second, []string{s1}, func(a jsonAgent) bool {
		return a.State == "stopped" && a.ExitedAt != ""
	}); !ok {
		t.Errorf("hub shows s1 %+v after the stop, want stopped with an exit time", got[s1])
	}
	var events []string
	if !waitFor(t, 10*time.Second, func() bool {
		events = hubEvents(readChannel(t, h, ch))
		return anyContains(events, s1+" exited")
	}) {
		t.Errorf("%s has no hub EVENT about s1 exiting: %q", ch, events)
	}
}
