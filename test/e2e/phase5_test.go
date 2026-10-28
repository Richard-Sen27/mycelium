//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fh "github.com/richard-sen27/mycelium/test/fakeharness"
)

// Phase 5 acceptance with the fake sandbox backend: an agent requests a
// GPU, an admin approves it on the hub's approvals page and the lease
// expires on its own; a job reaches a local model through the Ollama
// broker; a locally tightened setting rejects a hub grant even while the
// hub is offline; freezing everything stops all sandboxes within 5 s; a
// channel freeze pauses its agents.
//
// Every node gets a fake nvidia-smi on its PATH (one GPU), a fake Ollama
// upstream (MYCEL_OLLAMA_UPSTREAM) and a DOCKER_HOST that points nowhere,
// so nothing touches a real GPU or the host's Docker Engine.

// p5Caps are the capabilities of alpha and beta.
const p5Caps = "docker,ollama,gpu,sandbox"

// e2eModel is the one model the fake Ollama upstream has.
const e2eModel = "e2e-model:latest"

// e2eCompletionTokens is what the fake upstream reports as generated.
const e2eCompletionTokens = 7

// envJobHelper makes the test binary run jobHelper instead of the tests;
// a job's command is the test binary with this variable in its environment.
const envJobHelper = "E2E_JOB_HELPER"

const (
	grantPending = "GRANT_STATE_PENDING"
	grantActive  = "GRANT_STATE_ACTIVE"
	grantExpired = "GRANT_STATE_EXPIRED"
	grantRevoked = "GRANT_STATE_REVOKED"
)

// ---- environment ----

// fakeOllama is the model endpoint behind the nodes' Ollama brokers.
type fakeOllama struct {
	*httptest.Server
	chats atomic.Int64
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/tags" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q,"size":1}]}`, e2eModel, e2eModel)
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			_, _ = io.Copy(io.Discard, r.Body)
			f.chats.Add(1)
			_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-e2e","object":"chat.completion","created":1,"model":%q,`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from e2e"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":5,"completion_tokens":%d,"total_tokens":%d}}`, e2eModel, e2eCompletionTokens, 5+e2eCompletionTokens)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// fakeNvidiaSMI answers the nvidia-smi queries of the inventory probe and
// the GPU broker with one device.
const fakeNvidiaSMI = `#!/bin/sh
case "$*" in
*--query-gpu=index,uuid,name,memory.total*) echo "0, GPU-e2e00000-0000-4000-8000-000000000000, NVIDIA E2E Fake, 16384" ;;
*--query-gpu=index,name,memory.total,driver_version*) echo "0, NVIDIA E2E Fake, 16384, 550.00" ;;
*--query-gpu=index,utilization.gpu,memory.used,power.draw*) echo "0, 0, 0, 10.00" ;;
*--query-compute-apps*) ;;
-L) echo "GPU 0: NVIDIA E2E Fake (UUID: GPU-e2e00000-0000-4000-8000-000000000000)" ;;
*) echo "fake nvidia-smi: unsupported arguments: $*" >&2; exit 2 ;;
esac
`

// p5NodeEnv is the daemon environment of a phase 5 node: the fake backend,
// the fake nvidia-smi first on PATH, the fake Ollama upstream and a Docker
// Engine address that does not exist.
func p5NodeEnv(t *testing.T, upstream string) []string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(fakeNvidiaSMI), 0o700); err != nil { //nolint:gosec // executable test script
		t.Fatal(err)
	}
	return []string{
		fakeOK,
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKER_HOST=unix://" + filepath.Join(dir, "no-docker.sock"),
		"MYCEL_OLLAMA_UPSTREAM=" + upstream,
	}
}

// p5 is one phase 5 network: a hub with the fake profile and the nodes
// alpha and beta.
type p5 struct {
	h           *Hub
	alpha, beta *Node
	up          *fakeOllama
	keys        map[string]string // API keys of the members, by name
}

type p5Opts struct {
	hubYAML   string   // appended to hub.yaml
	members   []string // members created before the nodes join
	betaOwner string   // member who owns beta ("" = admin)
}

// skipNoUnix skips tests that need a POSIX shell, shell scripts on PATH and
// SIGSTOP.
func skipNoUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell for the fake nvidia-smi and job scripts")
	}
}

func startP5(t *testing.T, o p5Opts) *p5 {
	t.Helper()
	skipNoUnix(t)
	p := &p5{keys: map[string]string{}}
	p.up = newFakeOllama(t)
	p.h = startHubWith(t, hubOptions{Args: []string{"--allow-fake-profile"}, HubYAML: o.hubYAML})
	for _, m := range o.members {
		p.keys[m] = addMember(t, p.h, m)
	}
	p.alpha = joinNode(t, p.h, "alpha", p5Caps, "", p5NodeEnv(t, p.up.URL)...)
	p.beta = joinNode(t, p.h, "beta", p5Caps, p.keys[o.betaOwner], p5NodeEnv(t, p.up.URL)...)
	waitBackends(t, p.h, "fake", p.alpha, p.beta)
	localFirstOff(t, p.h, p.alpha, p.beta)
	return p
}

// localFirstOff switches local_first off on the nodes' hub-side settings
// and waits until their daemons apply it. local_first is on by default and
// the idle detector reads this machine's real input, so a person at the
// keyboard would otherwise pause every job once the first idle sample is
// taken (30 s after the daemon starts).
func localFirstOff(t *testing.T, h *Hub, nodes ...*Node) {
	t.Helper()
	for _, n := range nodes {
		h.MustCLI("", "nodes", "share", n.Name, "--local-first=false", "--json")
	}
	for _, n := range nodes {
		if v, ok := waitLocal(t, n, "/api/local/sharing", 15*time.Second, func(v jsonSharingView) bool {
			return v.Effective != nil && !v.Effective.LocalFirst
		}); !ok {
			t.Fatalf("%s: local_first still on: %+v", n.Name, v.Effective)
		}
	}
}

// addMember adds a member and returns its API key.
func addMember(t *testing.T, h *Hub, name string) string {
	t.Helper()
	var u struct {
		APIKey string `json:"api_key"`
	}
	mustJSON(t, h.MustCLI("", "user", "add", name, "--role", "member", "--json"), &u)
	if u.APIKey == "" {
		t.Fatalf("user add %s: no API key", name)
	}
	return u.APIKey
}

// nodeCLI runs mycel against n's daemon (MYCEL_HOME = node home).
func nodeCLI(t *testing.T, n *Node, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	return runCLI(t, childEnv("MYCEL_HOME="+n.Home), append(args, "--local-addr", n.LocalAddr)...)
}

// ---- JSON views ----

type jsonGrant struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Subject      string `json:"subject"`
	Node         string `json:"node"`
	Reason       string `json:"reason"`
	Duration     string `json:"duration"`
	ExpiresAt    string `json:"expires_at"`
	DecidedBy    string `json:"decided_by"`
	DecisionNote string `json:"decision_note"`
}

func getGrant(t *testing.T, h *Hub, apiKey, id string) jsonGrant {
	t.Helper()
	var resp struct {
		Grant jsonGrant `json:"grant"`
	}
	mustJSON(t, h.MustCLI(apiKey, "grants", "get", id, "--json"), &resp)
	return resp.Grant
}

func listGrants(t *testing.T, h *Hub, apiKey string, args ...string) []jsonGrant {
	t.Helper()
	var resp struct {
		Grants []jsonGrant `json:"grants"`
	}
	mustJSON(t, h.MustCLI(apiKey, append([]string{"grants", "ls", "--json"}, args...)...), &resp)
	return resp.Grants
}

// waitGrant polls the grant as admin until cond holds.
func waitGrant(t *testing.T, h *Hub, id string, timeout time.Duration, what string, cond func(jsonGrant) bool) jsonGrant {
	t.Helper()
	var g jsonGrant
	if !waitFor(t, timeout, func() bool {
		g = getGrant(t, h, "", id)
		return cond(g)
	}) {
		t.Fatalf("grant %s not %s within %s: %+v", id, what, timeout, g)
	}
	return g
}

// jsonLocalGrants is GET /api/local/grants.
type jsonLocalGrants struct {
	Grants    []jsonGrant       `json:"grants"`
	Endpoints map[string]string `json:"endpoints"`
}

func (l jsonLocalGrants) has(id string) bool {
	return slices.ContainsFunc(l.Grants, func(g jsonGrant) bool { return g.ID == id })
}

type jsonSharing struct {
	Offer *struct {
		Names []string `json:"names"`
	} `json:"offer"`
	When       string `json:"when"`
	LocalFirst bool   `json:"local_first"`
}

// offers returns the offer names, nil when the settings name none.
func (s *jsonSharing) offers() []string {
	if s == nil || s.Offer == nil {
		return nil
	}
	return s.Offer.Names
}

// jsonSharingView is GET /api/local/sharing and `nodes share --json`.
type jsonSharingView struct {
	Hub       *jsonSharing `json:"hub"`
	Local     *jsonSharing `json:"local"`
	Effective *jsonSharing `json:"effective"`
}

type jsonFreeze struct {
	Frozen bool   `json:"frozen"`
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

type jsonFreezeStatus struct {
	AllFrozen      bool     `json:"all_frozen"`
	AllReason      string   `json:"all_reason"`
	FrozenChannels []string `json:"frozen_channels"`
}

func freezeStatus(t *testing.T, h *Hub) jsonFreezeStatus {
	t.Helper()
	var s jsonFreezeStatus
	mustJSON(t, h.MustCLI("", "freeze", "status", "--json"), &s)
	return s
}

// ---- the job helper ----

// jobHelper is the test binary run as a job's command (envJobHelper set):
// it prints key=value lines and returns the exit code.
func jobHelper(mode string) int {
	switch mode {
	case "ollama":
		return ollamaJob()
	default:
		fmt.Fprintln(os.Stderr, "unknown job helper", mode)
		return 2
	}
}

// ollamaJob uses the grant the sandbox environment carries: it lists the
// models and asks for one chat completion through the egress proxy, and
// shows that the broker refuses a request without the credential. The
// credential and the proxy URL are never printed.
func ollamaJob() int {
	base, key, proxy := os.Getenv("OPENAI_BASE_URL"), os.Getenv("OPENAI_API_KEY"), os.Getenv("HTTP_PROXY")
	fmt.Printf("base=%s key-set=%t proxy-set=%t\n", base, key != "", proxy != "")
	if base == "" || key == "" || proxy == "" {
		return 3
	}
	pu, err := url.Parse(proxy)
	if err != nil {
		fmt.Println("proxy-error=unparsable")
		return 3
	}
	c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	do := func(method, u, body string, auth bool) (int, []byte) {
		req, err := http.NewRequestWithContext(context.Background(), method, u, strings.NewReader(body)) //nolint:gosec // G704: the broker the sandbox environment names
		if err != nil {
			return -1, nil
		}
		if auth {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req) //nolint:gosec // G704: the broker the sandbox environment names
		if err != nil {
			fmt.Printf("request-error=%s %s\n", method, strings.ReplaceAll(err.Error(), key, "<key>"))
			return -1, nil
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, b
	}
	root := strings.TrimSuffix(base, "/v1")
	code, body := do(http.MethodGet, root+"/api/tags", "", true)
	fmt.Printf("tags-status=%d\n", code)
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.Unmarshal(body, &tags)
	for _, m := range tags.Models {
		fmt.Printf("model=%s\n", m.Name)
	}
	noauth, _ := do(http.MethodGet, root+"/api/tags", "", false)
	fmt.Printf("noauth-status=%d\n", noauth)
	if code != http.StatusOK || len(tags.Models) == 0 {
		return 1
	}
	chat := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"say hi"}]}`, tags.Models[0].Name)
	ccode, cbody := do(http.MethodPost, base+"/chat/completions", chat, true)
	var cr struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(cbody, &cr)
	fmt.Printf("chat-status=%d\nchat-tokens=%d\n", ccode, cr.Usage.CompletionTokens)
	if ccode != http.StatusOK || noauth != http.StatusUnauthorized {
		return 1
	}
	return 0
}

// ---- the approvals page ----

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// uiClient is a browser stand-in for the hub's approvals page: a cookie
// jar and the hub CA pinned.
func uiClient(t *testing.T, h *Hub) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(h.Home, "data", "hub", "ca.crt")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("hub ca.crt holds no certificate")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

// uiDo sends one request and returns the final status and page.
func uiDo(t *testing.T, c *http.Client, method, u string, form url.Values) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func csrfOf(t *testing.T, page string) string {
	t.Helper()
	m := csrfRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("page has no CSRF token:\n%s", page)
	}
	return m[1]
}

// ---- tests ----

func TestGrantApprovedInHubUI(t *testing.T) {
	t.Parallel()
	p := startP5(t, p5Opts{members: []string{"bob"}, betaOwner: "bob"})
	h := p.h
	const agent = "agent:gpuer@alpha"
	const reason = "e2e-reason-6b1d fine-tune the probe"

	// The hub knows alpha's (fake) GPU 0.
	type gpu struct {
		Index  int    `json:"index"`
		Vendor string `json:"vendor"`
	}
	if !waitFor(t, 30*time.Second, func() bool {
		var resp struct {
			Nodes []struct {
				Name      string `json:"name"`
				Inventory struct {
					GPUs []gpu `json:"gpus"`
				} `json:"inventory"`
			} `json:"nodes"`
		}
		mustJSON(t, h.MustCLI("", "nodes", "ls", "--json"), &resp)
		for _, n := range resp.Nodes {
			if n.Name == "alpha" && slices.Contains(n.Inventory.GPUs, gpu{Index: 0, Vendor: "nvidia"}) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the hub never saw alpha's fake GPU 0: %s", h.MustCLI("", "nodes", "get", "alpha", "--json"))
	}

	// Agents cannot read #approvals, not even a member's own agent.
	peek := mustRun(t, p.beta, "peek", []fh.Step{
		{Tool: "read_messages", Args: map[string]any{"channel": "#approvals"}, ExpectError: true},
	})
	t.Logf("bob's agent reading #approvals: %s", peek.Calls[0].Text)

	// The agent asks for GPU 0; no rule matches, so it waits for a decision.
	run := goRun(t, p.alpha, "gpuer", []fh.Step{
		{Tool: "request_grant", Args: map[string]any{
			"resource": map[string]any{"kind": "gpu", "device": 0}, "duration": "1m", "reason": reason,
		}, Expect: "pending approval"},
		{Tool: "wait_for_message", Args: map[string]any{"timeout_seconds": 10}, Expect: "approved", Repeat: 9},
	})
	var g jsonGrant
	if !waitFor(t, 20*time.Second, func() bool {
		for _, x := range listGrants(t, h, "", "--pending") {
			if x.Subject == agent {
				g = x
				return true
			}
		}
		return false
	}) {
		t.Fatalf("grants ls --pending never listed a grant for %s: %+v", agent, listGrants(t, h, "", "--all"))
	}
	if g.State != grantPending || g.Node != "alpha" || !strings.HasPrefix(g.ID, "grt_") {
		t.Fatalf("pending grant %+v", g)
	}

	// The card in #approvals carries the hub-checked fields, not the reason.
	var card *jsonMsg
	if !waitFor(t, 10*time.Second, func() bool {
		for _, m := range readChannel(t, h, "#approvals") {
			if m.Type == "MESSAGE_TYPE_APPROVAL_REQUEST" && strings.Contains(m.Payload.Text, g.ID) {
				card = &m
				return true
			}
		}
		return false
	}) {
		t.Fatalf("#approvals has no approval card for %s", g.ID)
	}
	for _, want := range []string{"kind: gpu", "subject: " + agent, "node: alpha"} {
		if !strings.Contains(card.Payload.Text, want) {
			t.Errorf("approval card lacks %q:\n%s", want, card.Payload.Text)
		}
	}
	if strings.Contains(card.Payload.Text, "e2e-reason") {
		t.Errorf("approval card in #approvals carries the requester's reason:\n%s", card.Payload.Text)
	}

	// The admin signs in to the approvals page with the API key and approves.
	c := uiClient(t, h)
	code, page := uiDo(t, c, http.MethodGet, h.HubURL+"/ui/login", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /ui/login: %d\n%s", code, page)
	}
	code, page = uiDo(t, c, http.MethodPost, h.HubURL+"/ui/login",
		url.Values{"csrf": {csrfOf(t, page)}, "api_key": {h.AdminKey}})
	if code != http.StatusOK || !strings.Contains(page, "Approvals") {
		t.Fatalf("sign-in: %d\n%s", code, page)
	}
	code, page = uiDo(t, c, http.MethodGet, h.HubURL+"/ui/approvals", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /ui/approvals: %d\n%s", code, page)
	}
	for _, want := range []string{g.ID, "gpu device 0", "subject " + agent, "node alpha"} {
		if !strings.Contains(page, want) {
			t.Errorf("approvals page lacks %q:\n%s", want, page)
		}
	}
	approved := time.Now()
	code, page = uiDo(t, c, http.MethodPost, h.HubURL+"/ui/approvals/"+g.ID+"/approve",
		url.Values{"csrf": {csrfOf(t, page)}, "duration": {"20s"}})
	if code != http.StatusOK || !strings.Contains(page, "Grant "+g.ID+" approved.") {
		t.Fatalf("approve on the page: %d\n%s", code, page)
	}

	// The decision reaches the agent as a direct message.
	tr := collect(t, run, 100*time.Second)
	if t.Failed() {
		t.FailNow()
	}
	if last := tr.Calls[len(tr.Calls)-1].Text; !strings.Contains(last, g.ID) {
		t.Errorf("the agent's decision message does not name %s:\n%s", g.ID, last)
	}

	a := getGrant(t, h, "", g.ID)
	if a.State != grantActive {
		t.Fatalf("grant after the approval: %+v", a)
	}
	exp, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
	if err != nil || exp.Sub(approved) > 25*time.Second || exp.Before(approved) {
		t.Errorf("grant expires at %q; want about 20s after the approval at %s", a.ExpiresAt, approved.UTC().Format(time.RFC3339))
	}
	l, ok := waitLocal(t, p.alpha, "/api/local/grants", 10*time.Second, func(l jsonLocalGrants) bool {
		return l.has(g.ID) && strings.HasPrefix(l.Endpoints[g.ID], "NVIDIA_VISIBLE_DEVICES=GPU-")
	})
	if !ok {
		t.Fatalf("alpha does not list %s with an NVIDIA_VISIBLE_DEVICES endpoint: %+v", g.ID, l)
	}

	// The lease ends on its own: EXPIRED on the hub and gone from the node.
	waitGrant(t, h, g.ID, 35*time.Second, "expired", func(x jsonGrant) bool { return x.State == grantExpired })
	if l, ok := waitLocal(t, p.alpha, "/api/local/grants", 10*time.Second, func(l jsonLocalGrants) bool {
		return !l.has(g.ID)
	}); !ok {
		t.Fatalf("alpha still lists %s after it expired: %+v", g.ID, l)
	}
	t.Logf("grant %s expired %s after the approval", g.ID, time.Since(approved).Round(100*time.Millisecond))
}

// nodeCapacity is how many jobs the hub runs on a node at once (two, or
// one on a node with fewer than four CPUs).
func nodeCapacity(t *testing.T, h *Hub, name string) int {
	t.Helper()
	cpus := int(listNodes(t, h, "")[name].Inventory.CPUCount)
	if cpus <= 0 {
		return 2
	}
	return max(1, min(2, cpus/2))
}

func TestOllamaGrantFromSandbox(t *testing.T) {
	t.Parallel()
	p := startP5(t, p5Opts{members: []string{"bob"}, hubYAML: "grants:\n  auto:\n    - kind: ollama\n      node: beta\n      max_ttl: 5m\n"})
	h, bob := p.h, p.keys["bob"]
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// A sandbox gets its broker credentials when it is created, so the
	// grant has to exist before the job starts: beta is kept busy while
	// bob's job waits in the queue for it and gets its grant there.
	var fillers []string
	for range nodeCapacity(t, h, "beta") {
		j := runJob(t, h, "", append([]string{"--profile", "fake", "--node", "beta", "--timeout", "3m", "--"}, sleepCmd("150")...)...)
		waitState(t, h, j.ID, jobRunning, 30*time.Second)
		fillers = append(fillers, j.ID)
	}
	t.Cleanup(func() {
		for _, id := range fillers {
			_, _, _ = h.CLI("job", "cancel", id)
		}
	})
	job := runJob(t, h, bob, "--profile", "fake", "--node", "beta", "--timeout", "90s",
		"--env", envJobHelper+"=ollama", "--", self)
	if j := getJob(t, h, job.ID); j.State != "JOB_STATE_QUEUED" {
		t.Fatalf("bob's job is %s, want queued behind the fillers: %+v", j.State, j)
	}

	var req struct {
		Grant jsonGrant `json:"grant"`
	}
	mustJSON(t, h.MustCLI(bob, "grants", "request", "--kind", "ollama", "--subject", "job:"+job.ID,
		"--duration", "5m", "--reason", "e2e model access", "--json"), &req)
	g := req.Grant
	if g.State != grantActive || g.DecidedBy != "policy" || g.Node != "beta" {
		t.Fatalf("ollama grant for the job: %+v; want active by policy on beta", g)
	}
	if l, ok := waitLocal(t, p.beta, "/api/local/grants", 15*time.Second, func(l jsonLocalGrants) bool {
		return l.has(g.ID) && l.Endpoints[g.ID] != ""
	}); !ok {
		t.Fatalf("beta never attached %s: %+v", g.ID, l)
	} else if strings.Contains(l.Endpoints[g.ID], "grant:") {
		t.Errorf("GET /api/local/grants reveals the Ollama credential in %s's endpoint", g.ID)
	}

	for _, id := range fillers {
		h.MustCLI("", "job", "cancel", id)
	}
	j := waitFinished(t, h, job.ID, 60*time.Second)
	logs := jobLogs(t, h, job.ID)
	t.Logf("job %s: %s exit %d\n%s", j.ID, j.State, j.ExitCode, logs)
	if j.State != jobSucceeded || j.Node != "beta" {
		t.Fatalf("job %+v, want succeeded on beta", j)
	}
	for _, want := range []string{
		"base=http://ollama.mycel.internal/v1", "tags-status=200", "model=" + e2eModel,
		"noauth-status=401", "chat-status=200", "chat-tokens=" + strconv.Itoa(e2eCompletionTokens),
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("job output lacks %q", want)
		}
	}
	if p.up.chats.Load() == 0 {
		t.Errorf("the fake upstream never got the chat completion")
	}

	// The broker's usage reaches the hub. The Grant message has no usage
	// field, so the hub's audit record of broker.usage is the evidence.
	usage := regexp.MustCompile(`action=broker\.usage resource=grant:` + regexp.QuoteMeta(g.ID) + `\b.*\btokens=(\d+)`)
	var tokens int
	if !waitFor(t, 30*time.Second, func() bool {
		tokens = 0
		for _, m := range usage.FindAllStringSubmatch(readFile(h.Proc.stderrPath), -1) {
			n, _ := strconv.Atoi(m[1])
			tokens += n
		}
		return tokens >= e2eCompletionTokens
	}) {
		t.Errorf("the hub recorded %d tokens of usage for %s, want %d", tokens, g.ID, e2eCompletionTokens)
	}
	// The job's grant ends with the job.
	waitGrant(t, h, g.ID, 15*time.Second, "revoked with the job", func(x jsonGrant) bool { return x.State == grantRevoked })
}

func TestLocalCeilingBeatsHubOffline(t *testing.T) {
	t.Parallel()
	p := startP5(t, p5Opts{hubYAML: "grants:\n  auto:\n    - kind: ollama\n      node: beta\n      max_ttl: 5m\n"})
	h, beta := p.h, p.beta
	const subject = "agent:llm@beta"

	// The owner's hub-side settings for beta: everything but the GPU.
	h.MustCLI("", "nodes", "share", "beta", "--offer", "docker,ollama,sandbox", "--json")
	if v, ok := waitLocal(t, beta, "/api/local/sharing", 15*time.Second, func(v jsonSharingView) bool {
		return slices.Equal(v.Hub.offers(), []string{"docker", "ollama", "sandbox"})
	}); !ok {
		t.Fatalf("beta never got the hub-side offers: %+v", v.Hub)
	}

	mustRun(t, beta, "llm", []fh.Step{{Tool: "whoami", Expect: subject}})
	var req struct {
		Grant jsonGrant `json:"grant"`
	}
	mustJSON(t, h.MustCLI("", "grants", "request", "--kind", "ollama", "--subject", subject, "--duration", "5m", "--json"), &req)
	g := req.Grant
	if g.State != grantActive {
		t.Fatalf("ollama grant: %+v, want active by the auto rule", g)
	}
	if l, ok := waitLocal(t, beta, "/api/local/grants", 15*time.Second, func(l jsonLocalGrants) bool { return l.has(g.ID) }); !ok {
		t.Fatalf("beta never attached %s: %+v", g.ID, l)
	}

	// The owner takes ollama away locally: the node detaches the grant and
	// the hub records the revocation by the node's ceiling.
	if out, errOut, code := nodeCLI(t, beta, "node", "share", "--offer", "docker,sandbox", "--json"); code != 0 {
		t.Fatalf("node share --offer docker,sandbox: exit %d\n%s\n%s", code, out, errOut)
	}
	start := time.Now()
	if l, ok := waitLocal(t, beta, "/api/local/grants", 5*time.Second, func(l jsonLocalGrants) bool { return !l.has(g.ID) }); !ok {
		t.Fatalf("beta still serves %s 5s after the local ceiling dropped ollama: %+v", g.ID, l)
	}
	r := waitGrant(t, h, g.ID, 5*time.Second, "revoked", func(x jsonGrant) bool { return x.State == grantRevoked })
	if r.DecidedBy != "node:beta" || !strings.Contains(r.DecisionNote, "ceiling") {
		t.Errorf("revoked grant %+v; want decided by node:beta with a reason naming the local ceiling", r)
	}
	t.Logf("grant %s detached and revoked %s after the local change", g.ID, time.Since(start).Round(10*time.Millisecond))

	// With the hub down the owner can still tighten, and cannot widen past
	// the hub-side settings beta last received.
	h.Proc.kill()
	if out, errOut, code := nodeCLI(t, beta, "node", "share", "--offer", "sandbox", "--json"); code != 0 {
		t.Fatalf("node share --offer sandbox with the hub down: exit %d\n%s\n%s", code, out, errOut)
	}
	var v jsonSharingView
	if !beta.LocalJSON(t, "/api/local/sharing", &v) ||
		!slices.Equal(v.Local.offers(), []string{"sandbox"}) || !slices.Equal(v.Effective.offers(), []string{"sandbox"}) {
		t.Fatalf("GET /api/local/sharing with the hub down: local %+v effective %+v; want sandbox only", v.Local, v.Effective)
	}
	code, body := beta.Local(t, http.MethodPut, "/api/local/sharing", `{"offer":{"names":["docker","ollama","gpu","sandbox"]}}`)
	if code != http.StatusBadRequest {
		t.Errorf("PUT /api/local/sharing widening past the hub-side settings: %d %s; want 400", code, body)
	}
	if !beta.LocalJSON(t, "/api/local/sharing", &v) || !slices.Equal(v.Effective.offers(), []string{"sandbox"}) {
		t.Errorf("effective offers after the refused widening: %+v", v.Effective)
	}

	// Back online, the hub shows the local ceiling and refuses an ollama grant.
	h.Start()
	var view jsonSharingView
	if !waitFor(t, 30*time.Second, func() bool {
		out, _, code := h.CLI("nodes", "share", "beta", "--json")
		if code != 0 || json.Unmarshal([]byte(out), &view) != nil {
			return false
		}
		return slices.Equal(view.Local.offers(), []string{"sandbox"}) && slices.Equal(view.Effective.offers(), []string{"sandbox"})
	}) {
		t.Fatalf("hub view of beta's sharing after the restart: hub %+v local %+v effective %+v",
			view.Hub, view.Local, view.Effective)
	}
	out, errOut, code2 := h.CLI("grants", "request", "--kind", "ollama", "--subject", subject, "--duration", "5m", "--json")
	if code2 == 0 || !strings.Contains(errOut, "does not offer ollama") {
		t.Errorf("ollama grant on beta after the local ceiling: exit %d\nstdout: %s\nstderr: %s; want refused as not offered", code2, out, errOut)
	}
}

// beatScript appends a line to beat in the job's workspace every 250 ms.
const beatScript = "while :; do echo b >> beat; sleep 0.25; done"

// beatPath is the beat file of a running fake-backend job on n: the
// sandbox runner's directory is <node dir>/jobs/<id>/work/sandbox and the
// fake backend's /workspace is its workspace subdirectory.
func beatPath(n *Node, jobID string) string {
	return filepath.Join(n.Home, "data", "node", "jobs", jobID, "work", "sandbox", "workspace", "beat")
}

func TestFreezeAllStopsSandboxes(t *testing.T) {
	t.Parallel()
	p := startP5(t, p5Opts{members: []string{"bob"}})
	h, bob := p.h, p.keys["bob"]
	nodes := []*Node{p.alpha, p.beta}
	const ch = "#task-fz"

	beats := map[string]string{}
	var jobs []string
	for _, n := range nodes {
		j := runJob(t, h, "", append([]string{"--profile", "fake", "--node", n.Name, "--timeout", "3m", "--"}, shell(beatScript, "")...)...)
		waitState(t, h, j.ID, jobRunning, 30*time.Second)
		beats[n.Name] = beatPath(n, j.ID)
		jobs = append(jobs, j.ID)
	}
	t.Cleanup(func() {
		for _, id := range jobs {
			_, _, _ = h.CLI("job", "cancel", id)
		}
	})
	sizes := func() map[string]int64 {
		m := map[string]int64{}
		for name, path := range beats {
			m[name] = fileSize(path)
		}
		return m
	}
	growing := func(timeout time.Duration) bool {
		before := sizes()
		return waitFor(t, timeout, func() bool {
			now := sizes()
			for name := range beats {
				if now[name] <= before[name] {
					return false
				}
			}
			return true
		})
	}
	if !growing(10 * time.Second) {
		t.Fatalf("the beat files do not grow: %v\nalpha's job logs:\n%s", sizes(), jobLogs(t, h, jobs[0]))
	}
	if s := freezeStatus(t, h); s.AllFrozen {
		t.Fatalf("freeze status before the freeze: %+v", s)
	}

	// An agent on alpha works through the freeze: it posts before, is
	// refused during and posts again after. observe holds the session
	// between the steps until the test has done its part.
	const agent = "agent:watch@alpha"
	frozen, unfrozen, during := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ready := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	res := make(chan runResult, 1)
	go func() {
		wait := func(c chan struct{}) {
			select {
			case <-c:
			case <-ctx.Done():
			}
		}
		tr, err := fh.RunObserved(ctx, binPath, agentEnv(p.alpha), "watch", harnessName, []fh.Step{
			join(ch),
			{Tool: "post", Args: map[string]any{"channel": ch, "text": "before"}, Expect: "posted msg_"},
			{Tool: "post", Args: map[string]any{"channel": ch, "text": "during"}, ExpectError: true},
			{Tool: "post", Args: map[string]any{"channel": ch, "text": "after"}, Expect: "posted msg_", Repeat: 40, AllowError: true},
		}, func(c fh.Call) {
			switch {
			case c.Step == 1:
				close(ready)
				wait(frozen)
			case c.Step == 2:
				close(during)
				wait(unfrozen)
			case c.Step == 3 && c.IsError:
				time.Sleep(250 * time.Millisecond)
			}
		})
		res <- runResult{name: agent, tr: tr, err: err}
	}()
	select {
	case <-ready:
	case r := <-res:
		t.Fatalf("the agent ended before the freeze: %v\n%s", r.err, r.tr)
	case <-time.After(60 * time.Second):
		t.Fatal("the agent did not post within 60s")
	}

	start := time.Now()
	h.MustCLI("", "freeze", "--all", "--reason", "drill")
	for _, n := range nodes {
		if f, ok := waitLocal(t, n, "/api/local/freeze", 5*time.Second, func(f jsonFreeze) bool { return f.Frozen }); !ok {
			t.Fatalf("%s not frozen within 5s: %+v", n.Name, f)
		} else if f.Scope != "all" || f.Reason != "drill" {
			t.Errorf("%s freeze state %+v; want scope all, reason drill", n.Name, f)
		}
	}
	// Stopped: the beat files do not change for 2 s, from a point within 5 s.
	var still map[string]int64
	stopped := waitFor(t, 5*time.Second-time.Since(start), func() bool {
		still = sizes()
		time.Sleep(300 * time.Millisecond)
		return maps.Equal(still, sizes())
	})
	if !stopped {
		t.Fatalf("the beat files still grow %s after the freeze: %v", time.Since(start).Round(time.Millisecond), sizes())
	}
	stopAt := time.Since(start)
	time.Sleep(2 * time.Second)
	if now := sizes(); !maps.Equal(still, now) {
		t.Fatalf("the beat files grew while frozen: %v -> %v", still, now)
	}
	t.Logf("sandboxes stopped within %s of the freeze", stopAt.Round(10*time.Millisecond))

	if s := freezeStatus(t, h); !s.AllFrozen || s.AllReason != "drill" {
		t.Errorf("freeze status while frozen: %+v", s)
	}
	if out, errOut, code := h.CLI("post", "#general", "hi"); code == 0 || !strings.Contains(errOut, "frozen") {
		t.Errorf("post while frozen: exit %d %s %s; want refused as frozen", code, out, errOut)
	}
	if out, errOut, code := h.CLI(append([]string{"job", "run", "--profile", "fake", "--"}, sleepCmd("1")...)...); code == 0 || !strings.Contains(errOut, "frozen") {
		t.Errorf("job run while frozen: exit %d %s %s; want refused as frozen", code, out, errOut)
	}
	if _, errOut, code := h.CLIAs(bob, "freeze", "--all"); code == 0 {
		t.Errorf("a member froze the network: %s", errOut)
	}
	if got, ok := waitAgents(t, h, 10*time.Second, []string{agent}, func(a jsonAgent) bool {
		return a.State == "paused" && a.PausedReason == "frozen: network"
	}); !ok {
		t.Errorf("%s while frozen: %+v; want paused by the network freeze", agent, got[agent])
	}
	close(frozen)
	select {
	case <-during:
	case <-time.After(30 * time.Second):
		t.Fatal("the agent did not try to post during the freeze")
	}

	if _, errOut, code := h.CLIAs(bob, "unfreeze", "--all"); code == 0 {
		t.Errorf("a member unfroze the network: %s", errOut)
	}
	if s := freezeStatus(t, h); !s.AllFrozen {
		t.Fatalf("network unfrozen by a member's attempt: %+v", s)
	}
	h.MustCLI("", "unfreeze", "--all")
	for _, n := range nodes {
		if f, ok := waitLocal(t, n, "/api/local/freeze", 10*time.Second, func(f jsonFreeze) bool { return !f.Frozen }); !ok {
			t.Errorf("%s still frozen 10s after the unfreeze: %+v", n.Name, f)
		}
	}
	if !growing(10 * time.Second) {
		t.Errorf("the beat files do not grow after the unfreeze: %v", sizes())
	}
	if s := freezeStatus(t, h); s.AllFrozen {
		t.Errorf("freeze status after the unfreeze: %+v", s)
	}
	if got, ok := waitAgents(t, h, 10*time.Second, []string{agent}, func(a jsonAgent) bool { return a.State == "running" }); !ok {
		t.Errorf("%s after the unfreeze: %+v; want running", agent, got[agent])
	}
	close(unfrozen)
	var r runResult
	select {
	case r = <-res:
	case <-time.After(60 * time.Second):
		t.Fatal("the agent did not finish within 60s of the unfreeze")
	}
	checkRace(t, "fake harness "+agent, r.tr.Stderr, 0)
	if r.err != nil {
		t.Fatalf("%s: %v\n%s", agent, r.err, r.tr)
	}
	var duringText, afterText string
	afterOK := false
	for _, c := range r.tr.Calls {
		switch c.Step {
		case 2:
			duringText = c.Text
		case 3:
			afterText, afterOK = c.Text, !c.IsError && strings.Contains(c.Text, "posted msg_")
		}
	}
	if low := strings.ToLower(duringText); !strings.Contains(low, "frozen") && !strings.Contains(low, "paused") {
		t.Errorf("the agent's post during the freeze failed with %q; want a frozen or paused refusal", duringText)
	}
	if !afterOK {
		t.Errorf("the agent could not post after the unfreeze: %q\n%s", afterText, r.tr)
	}
	h.MustCLI("", "post", "#general", "back to work")
}

func TestFreezeChannel(t *testing.T) {
	t.Parallel()
	skipNoUnix(t)
	const ch = "#task-f"
	const worker = "agent:f1@alpha"
	dir := t.TempDir()
	p := &p3{script: filepath.Join(dir, "worker.json"), transcripts: filepath.Join(dir, "transcripts"), workdir: t.TempDir()}
	p.setScript(t, []fh.Step{{SleepMS: 90_000}})
	h := startHub(t)
	p.h = h
	bob, carol := addMember(t, h, "bob"), addMember(t, h, "carol")
	p.alpha = joinNode(t, h, "alpha", "", bob, "MYCEL_FAKE_SCRIPT="+p.script, fh.EnvTranscriptDir+"="+p.transcripts)
	if !waitFor(t, 15*time.Second, func() bool {
		st := p.alpha.Status(t)
		return st != nil && st["connected"] == true
	}) {
		t.Fatalf("alpha never connected: %v", p.alpha.Status(t))
	}

	// bob owns the channel and the node; his worker lives in the channel.
	h.MustCLI(bob, "channels", "create", ch, "--type", "task", "--open")
	p.spawnCLI(t, "f1", ch, "wait")
	if got, ok := waitAgents(t, h, 15*time.Second, []string{worker}, func(a jsonAgent) bool {
		return a.State == "running" && a.HomeChannel == ch
	}); !ok {
		t.Fatalf("%s not running in %s: %+v", worker, ch, got[worker])
	}
	h.MustCLI(bob, "post", ch, "before")

	h.MustCLI(bob, "freeze", "--channel", ch, "--reason", "drill")
	if got, ok := waitAgents(t, h, 10*time.Second, []string{worker}, func(a jsonAgent) bool {
		return a.State == "paused" && a.PausedReason == "frozen: channel"
	}); !ok {
		t.Fatalf("%s after the channel freeze: %+v; want paused by it", worker, got[worker])
	}
	if !waitFor(t, 10*time.Second, func() bool { return p.localWorkers(t)[worker].State == "paused" }) {
		t.Errorf("alpha's worker is %q while its channel is frozen, want paused", p.localWorkers(t)[worker].State)
	}
	if out, errOut, code := h.CLIAs(bob, "post", ch, "during"); code == 0 || !strings.Contains(errOut, "frozen") {
		t.Errorf("post in the frozen channel: exit %d %s %s; want refused as frozen", code, out, errOut)
	}
	if s := freezeStatus(t, h); !slices.Contains(s.FrozenChannels, ch) || s.AllFrozen {
		t.Errorf("freeze status: %+v; want %s frozen, not the network", s, ch)
	}
	if _, errOut, code := h.CLIAs(carol, "unfreeze", "--channel", ch); code == 0 {
		t.Errorf("carol unfroze bob's channel: %s", errOut)
	}
	if s := freezeStatus(t, h); !slices.Contains(s.FrozenChannels, ch) {
		t.Fatalf("%s unfrozen by carol's attempt: %+v", ch, s)
	}

	h.MustCLI(bob, "unfreeze", "--channel", ch)
	if got, ok := waitAgents(t, h, 10*time.Second, []string{worker}, func(a jsonAgent) bool {
		return a.State == "running" && a.PausedReason == ""
	}); !ok {
		t.Errorf("%s after the unfreeze: %+v; want running", worker, got[worker])
	}
	if !waitFor(t, 10*time.Second, func() bool { return p.localWorkers(t)[worker].State == "running" }) {
		t.Errorf("alpha's worker is %q after the unfreeze, want running", p.localWorkers(t)[worker].State)
	}
	h.MustCLI(bob, "post", ch, "after")
	h.MustCLI("", "agents", "stop", worker)
}
