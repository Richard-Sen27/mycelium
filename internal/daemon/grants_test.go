package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// ---- fake hub: phase 5 RPCs ----

// fakeP5 answers grant.revoked, broker.usage and node.sharing like the hub.
type fakeP5 struct {
	mu        sync.Mutex
	revoked   []*mycelv1.GrantRevokedRequest
	usage     []*mycelv1.BrokerUsageRequest
	sharing   []*mycelv1.NodeSharingRequest
	denyUsage bool
	hub       *mycelv1.SharingSettings // node.sharing reply
}

func (f *fakeP5) serve(method string, data []byte) (proto.Message, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case bus.RPCGrantRevoked:
		req := &mycelv1.GrantRevokedRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed"
		}
		f.revoked = append(f.revoked, req)
		return &mycelv1.GrantRevokedResponse{}, ""
	case bus.RPCBrokerUsage:
		req := &mycelv1.BrokerUsageRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed"
		}
		f.usage = append(f.usage, req)
		return &mycelv1.BrokerUsageResponse{Deny: f.denyUsage}, ""
	case bus.RPCNodeSharing:
		req := &mycelv1.NodeSharingRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed"
		}
		f.sharing = append(f.sharing, req)
		return &mycelv1.NodeSharingResponse{Hub: f.hub}, ""
	}
	return nil, "unknown method"
}

func (f *fakeP5) set(fn func(*fakeP5)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeP5) revokedFor(id string) *mycelv1.GrantRevokedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.revoked {
		if r.GetGrantId() == id {
			return r
		}
	}
	return nil
}

func (f *fakeP5) usageRequests(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, u := range f.usage {
		if u.GetGrantId() == id {
			n += u.GetRequests()
		}
	}
	return n
}

func (f *fakeP5) lastSharing() *mycelv1.NodeSharingRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sharing) == 0 {
		return nil
	}
	return f.sharing[len(f.sharing)-1]
}

// ---- fake model endpoint ----

func newFakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"llama3:latest","model":"llama3:latest"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- helpers ----

func grantTweak(upstream string) func(*runConfig) {
	return func(c *runConfig) {
		c.ollamaUpstream = upstream
		c.brokerUsageEvery = 100 * time.Millisecond
	}
}

func startGrants(t *testing.T, tweak func(*runConfig)) (*localEnv, *httptest.Server) {
	t.Helper()
	up := newFakeOllama(t)
	return startLocal(t, func(c *runConfig) {
		grantTweak(up.URL)(c)
		if tweak != nil {
			tweak(c)
		}
	}), up
}

func ollamaGrant(id, subject string, ttl time.Duration) *mycelv1.Grant {
	return &mycelv1.Grant{
		Id: id, Subject: subject, Node: "n1", State: mycelv1.GrantState_GRANT_STATE_ACTIVE,
		Spec:      &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{}}},
		ExpiresAt: time.Now().Add(ttl).UTC().Format(time.RFC3339Nano),
	}
}

func attachCtl(t *testing.T, e *localEnv, g *mycelv1.Grant) *mycelv1.CtlResponse {
	t.Helper()
	return ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_AttachGrant{AttachGrant: &mycelv1.AttachGrant{Grant: g}}})
}

func mustAttach(t *testing.T, e *localEnv, g *mycelv1.Grant) {
	t.Helper()
	if r := attachCtl(t, e, g); !r.GetOk() {
		t.Fatalf("AttachGrant %s refused: %s", g.GetId(), r.GetError())
	}
}

type grantsList struct {
	Grants    []json.RawMessage `json:"grants"`
	Endpoints map[string]string `json:"endpoints"`
}

func (l grantsList) ids(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, raw := range l.Grants {
		g := &mycelv1.Grant{}
		if err := protojson.Unmarshal(raw, g); err != nil {
			t.Fatal(err)
		}
		out = append(out, g.GetId())
	}
	return out
}

func (c *localClient) grants(path string) grantsList {
	c.t.Helper()
	code, b := c.do(http.MethodGet, path, "")
	if code != http.StatusOK {
		c.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	var out grantsList
	if err := json.Unmarshal(b, &out); err != nil {
		c.t.Fatalf("grants reply %s: %v", b, err)
	}
	return out
}

func (c *localClient) sharing() *mycelv1.GetNodeSharingResponse {
	c.t.Helper()
	code, b := c.do(http.MethodGet, "/api/local/sharing", "")
	if code != http.StatusOK {
		c.t.Fatalf("GET sharing: %d %s", code, b)
	}
	out := &mycelv1.GetNodeSharingResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

func (c *localClient) putSharing(s *mycelv1.SharingSettings) (int, []byte) {
	c.t.Helper()
	b, err := protojson.Marshal(s)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(http.MethodPut, "/api/local/sharing", string(b))
}

// withoutOffer returns the effective settings minus one offer, a valid
// local tightening.
func (c *localClient) withoutOffer(name string) *mycelv1.SharingSettings {
	c.t.Helper()
	s := proto.Clone(c.sharing().GetEffective()).(*mycelv1.SharingSettings)
	s.Offer.Names = slices.DeleteFunc(s.Offer.Names, func(n string) bool { return n == name })
	return s
}

// brokerGet calls the broker endpoint (credential in the URL) at path.
func brokerGet(t *testing.T, endpoint, path string) int {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = path
	resp, err := http.Get(u.String()) //nolint:gosec,noctx // test broker on loopback
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// ---- tests ----

func TestGrantOllamaAttachUsageAndEndpoints(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	other := e.c.mustRegister("other")
	g := ollamaGrant("grt_OLL1", coder.AgentID, time.Hour)
	mustAttach(t, e, g)
	mustAttach(t, e, ollamaGrant("grt_OLL2", other.AgentID, time.Hour))

	mine := e.c.grants("/api/local/agents/" + coder.Session + "/grants")
	if ids := mine.ids(t); len(ids) != 1 || ids[0] != "grt_OLL1" {
		t.Fatalf("agent grants %v", ids)
	}
	ep := mine.Endpoints["grt_OLL1"]
	u, err := url.Parse(ep)
	if err != nil || u.User == nil {
		t.Fatalf("endpoint %q", ep)
	}
	if _, ok := u.User.Password(); !ok {
		t.Fatalf("endpoint without credential: %q", ep)
	}
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusOK {
		t.Fatalf("broker answered %d", code)
	}
	// Without the credential the broker refuses.
	if code := brokerGet(t, "http://"+u.Host, "/api/tags"); code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", code)
	}
	// The node-wide listing hides credentials.
	all := e.c.grants("/api/local/grants")
	if ids := all.ids(t); len(ids) != 2 {
		t.Fatalf("all grants %v", ids)
	}
	if red := all.Endpoints["grt_OLL1"]; strings.Contains(red, ":"+func() string { p, _ := u.User.Password(); return p }()+"@") || red == "" {
		t.Fatalf("listing leaks the credential: %q", red)
	}
	waitFor(t, 5*time.Second, "broker.usage", func() bool { return e.fa.p5.usageRequests("grt_OLL1") >= 1 })
	if s := getStatus(t, e.r.addr); s.Grants != 2 || !s.SharingActive || s.Frozen {
		t.Fatalf("status %+v", s)
	}

	// A resend is idempotent: same credential.
	mustAttach(t, e, g)
	if again := e.c.grants("/api/local/agents/" + coder.Session + "/grants").Endpoints["grt_OLL1"]; again != ep {
		t.Fatalf("resend changed the endpoint")
	}

	// A hub "deny" on broker.usage detaches the grant locally.
	e.fa.p5.set(func(f *fakeP5) { f.denyUsage = true })
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusOK {
		t.Fatalf("broker answered %d", code)
	}
	waitFor(t, 5*time.Second, "grant detached after deny", func() bool {
		return len(e.c.grants("/api/local/grants").ids(t)) == 1
	})
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusUnauthorized {
		t.Fatalf("detached grant still served: %d", code)
	}
	if code, _ := e.c.do(http.MethodGet, "/api/local/agents/nosuch/grants", ""); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
}

func TestGrantAttachValidation(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	cases := []struct {
		name string
		g    *mycelv1.Grant
		want string
	}{
		{"bad id", func() *mycelv1.Grant { g := ollamaGrant("x", coder.AgentID, time.Hour); return g }(), "invalid_argument"},
		{"other node", func() *mycelv1.Grant { g := ollamaGrant("grt_V1", coder.AgentID, time.Hour); g.Node = "n2"; return g }(), "invalid_argument"},
		{"bad subject", ollamaGrant("grt_V2", "agent:coder@n2", time.Hour), "invalid_argument"},
		{"expired", ollamaGrant("grt_V3", coder.AgentID, -time.Minute), "failed_precondition"},
		{"home mount", &mycelv1.Grant{Id: "grt_V4", Subject: coder.AgentID, Node: "n1", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
			Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Mount{Mount: &mycelv1.MountGrant{HostPath: os.Getenv("HOME"), GuestPath: "/data"}}}}, "invalid_argument"},
		{"loopback egress", &mycelv1.Grant{Id: "grt_V5", Subject: coder.AgentID, Node: "n1", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
			Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"127.0.0.1:22"}}}}}, "invalid_argument"},
		{"no kind", &mycelv1.Grant{Id: "grt_V6", Subject: coder.AgentID, Node: "n1", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}, "invalid_argument"},
	}
	for _, c := range cases {
		r := attachCtl(t, e, c.g)
		if r.GetOk() || !strings.HasPrefix(r.GetError(), c.want) {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
	// An egress grant for an agent is remembered for its sandboxes.
	eg := &mycelv1.Grant{Id: "grt_V7", Subject: coder.AgentID, Node: "n1", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"pypi.org"}}}}}
	mustAttach(t, e, eg)
	if ids := e.c.grants("/api/local/grants").ids(t); len(ids) != 1 {
		t.Fatalf("grants %v", ids)
	}
}

func TestGrantRefusedByLocalCeiling(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	if code, b := e.c.putSharing(e.c.withoutOffer("ollama")); code != http.StatusOK {
		t.Fatalf("PUT sharing: %d %s", code, b)
	}
	r := attachCtl(t, e, ollamaGrant("grt_CEIL1", coder.AgentID, time.Hour))
	if r.GetOk() || r.GetError() != "failed_precondition: refused by local sharing settings" {
		t.Fatalf("attach: %+v", r)
	}
	waitFor(t, 5*time.Second, "grant.revoked", func() bool { return e.fa.p5.revokedFor("grt_CEIL1") != nil })
	if rv := e.fa.p5.revokedFor("grt_CEIL1"); rv.GetReason() != "local ceiling" {
		t.Fatalf("reason %q", rv.GetReason())
	}
	// The hub-side settings narrow too: a SharingSettings ctl without gpu.
	hub := &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"sandbox", "docker"}}, When: "always", MaxJobs: 2, LocalFirst: true}
	if resp := ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Sharing{Sharing: hub}}); !resp.GetOk() {
		t.Fatalf("sharing ctl: %+v", resp)
	}
	sh := e.c.sharing()
	if !proto.Equal(sh.GetHub(), hub) || slices.Contains(sh.GetEffective().GetOffer().GetNames(), "gpu") {
		t.Fatalf("sharing after ctl: %v", sh)
	}
	if _, err := os.Stat(filepath.Join(e.dir, SharingHubFile)); err != nil {
		t.Fatal(err)
	}
}

func TestCeilingTightenedWhileHubOffline(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	mustAttach(t, e, ollamaGrant("grt_OFF1", coder.AgentID, time.Hour))
	ep := e.c.grants("/api/local/agents/" + coder.Session + "/grants").Endpoints["grt_OFF1"]
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusOK {
		t.Fatalf("broker answered %d", code)
	}
	waitFor(t, 5*time.Second, "first node.sharing", func() bool { return e.fa.p5.lastSharing() != nil })

	port := e.h.busServer().Port()
	e.h.stopBus()
	waitFor(t, 5*time.Second, "disconnected", func() bool { return !getStatus(t, e.r.addr).Connected })
	if code, b := e.c.putSharing(e.c.withoutOffer("ollama")); code != http.StatusOK {
		t.Fatalf("PUT sharing offline: %d %s", code, b)
	}
	if ids := e.c.grants("/api/local/grants").ids(t); len(ids) != 0 {
		t.Fatalf("grant still attached: %v", ids)
	}
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusUnauthorized {
		t.Fatalf("detached grant still served: %d", code)
	}
	// The hub, back online, gets the new ceiling and the revocation.
	e.h.startBus(port)
	e.fa.subscribe()
	waitFor(t, 20*time.Second, "node.sharing with the new ceiling", func() bool {
		l := e.fa.p5.lastSharing()
		return l != nil && l.GetLocal() != nil && !slices.Contains(l.GetLocal().GetOffer().GetNames(), "ollama")
	})
	waitFor(t, 10*time.Second, "grant.revoked after reconnect", func() bool {
		rv := e.fa.p5.revokedFor("grt_OFF1")
		return rv != nil && rv.GetReason() == "local ceiling"
	})
}

func TestGrantExpiresLocallyWithBusDown(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	mustAttach(t, e, ollamaGrant("grt_EXP1", coder.AgentID, 1500*time.Millisecond))
	ep := e.c.grants("/api/local/agents/" + coder.Session + "/grants").Endpoints["grt_EXP1"]
	e.h.stopBus()
	waitFor(t, 5*time.Second, "local expiry", func() bool { return len(e.c.grants("/api/local/grants").ids(t)) == 0 })
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusUnauthorized {
		t.Fatalf("expired grant still served: %d", code)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, GrantsFile))
	if err != nil || strings.Contains(string(b), "grt_EXP1") {
		t.Fatalf("grants.json %s %v", b, err)
	}
}

func TestGrantsReattachedAtRestart(t *testing.T) {
	up := newFakeOllama(t)
	h := newFakeHub(t)
	fa := newFakeAgents(h)
	dir := joinNode(t, h, "n1")
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	grantTweak(up.URL)(&cfg)
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	e := &localEnv{h: h, fa: fa, dir: dir, r: r, c: newLocalClient(t, r, dir)}
	mustAttach(t, e, ollamaGrant("grt_RST1", "job:job_R1", time.Hour))

	r.cancel()
	if err := r.wait(t, 10*time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The hub is down while the node restarts: the grant comes back from grants.json.
	h.stopBus()
	r2 := startRunWith(t, dir, cfg, p, sl)
	c2 := newLocalClient(t, r2, dir)
	l := c2.grants("/api/local/grants")
	if ids := l.ids(t); len(ids) != 1 || ids[0] != "grt_RST1" {
		t.Fatalf("after restart %v", ids)
	}
	if ep := l.Endpoints["grt_RST1"]; ep == "" {
		t.Fatal("no endpoint after restart")
	}
}

func TestPutSharingTightenOnly(t *testing.T) {
	e, _ := startGrants(t, nil)
	sh := e.c.sharing()
	if sh.GetLocal() != nil || sh.GetHub() == nil || sh.GetEffective() == nil {
		t.Fatalf("initial %v", sh)
	}
	wider := proto.Clone(sh.GetEffective()).(*mycelv1.SharingSettings)
	wider.MaxJobs = 10
	if code, b := e.c.putSharing(wider); code != http.StatusBadRequest {
		t.Fatalf("wider PUT: %d %s", code, b)
	}
	noLocalFirst := proto.Clone(sh.GetEffective()).(*mycelv1.SharingSettings)
	noLocalFirst.LocalFirst = false
	if code, _ := e.c.putSharing(noLocalFirst); code != http.StatusBadRequest {
		t.Fatalf("local_first off: %d", code)
	}
	if code, _ := e.c.do(http.MethodPut, "/api/local/sharing", `{"when":"sometimes"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid: %d", code)
	}
	if code, _ := e.c.do(http.MethodPut, "/api/local/sharing", `not json`); code != http.StatusBadRequest {
		t.Fatalf("not json: %d", code)
	}
	tighter := proto.Clone(sh.GetEffective()).(*mycelv1.SharingSettings)
	tighter.MaxJobs = 1
	code, b := e.c.putSharing(tighter)
	if code != http.StatusOK {
		t.Fatalf("tighter PUT: %d %s", code, b)
	}
	eff := &mycelv1.SharingSettings{}
	if err := protojson.Unmarshal(b, eff); err != nil || eff.GetMaxJobs() != 1 {
		t.Fatalf("effective %s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "sharing.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := e.c.sharing(); got.GetLocal().GetMaxJobs() != 1 || got.GetEffective().GetMaxJobs() != 1 {
		t.Fatalf("after PUT %v", got)
	}
	waitFor(t, 5*time.Second, "node.sharing", func() bool {
		l := e.fa.p5.lastSharing()
		return l != nil && l.GetLocal().GetMaxJobs() == 1
	})
}

func TestDeleteLocalGrant(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	g := ollamaGrant("grt_DEL1", coder.AgentID, time.Hour)
	mustAttach(t, e, g)
	ep := e.c.grants("/api/local/agents/" + coder.Session + "/grants").Endpoints["grt_DEL1"]
	if code, b := e.c.do(http.MethodDelete, "/api/local/grants/grt_DEL1", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", code, b)
	}
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusUnauthorized {
		t.Fatalf("revoked grant still served: %d", code)
	}
	waitFor(t, 5*time.Second, "grant.revoked", func() bool { return e.fa.p5.revokedFor("grt_DEL1") != nil })
	if code, _ := e.c.do(http.MethodDelete, "/api/local/grants/grt_DEL1", ""); code != http.StatusNotFound {
		t.Fatalf("second DELETE: %d", code)
	}
	if code, _ := e.c.do(http.MethodDelete, "/api/local/grants/nonsense", ""); code != http.StatusNotFound {
		t.Fatalf("bad id: %d", code)
	}
	// The hub resending the grant does not bring it back.
	if r := attachCtl(t, e, g); r.GetOk() || !strings.HasPrefix(r.GetError(), "failed_precondition") {
		t.Fatalf("re-attach after local revoke: %+v", r)
	}
}

func TestFrozenSurvivesRestart(t *testing.T) {
	h := newFakeHub(t)
	_ = newFakeAgents(h)
	dir := joinNode(t, h, "n1")
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	freeze := &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Freeze{Freeze: &mycelv1.Freeze{Scope: "all", Reason: "drill"}}}
	if resp := ctl(t, h.hubConn(), "n1", freeze); !resp.GetOk() {
		t.Fatalf("freeze: %+v", resp)
	}
	c := newLocalClient(t, r, dir)
	sess := c.mustRegister("coder")
	if code, b := c.rpc(sess.Session, "post", `{"channel":"#general","text":"hi"}`); code != http.StatusLocked || !strings.Contains(string(b), "frozen") {
		t.Fatalf("post while frozen: %d %s", code, b)
	}
	r.cancel()
	_ = r.wait(t, 10*time.Second)

	r2 := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r2.addr)
	c2 := newLocalClient(t, r2, dir)
	var fz freezeState
	code, b := c2.do(http.MethodGet, "/api/local/freeze", "")
	if code != http.StatusOK || json.Unmarshal(b, &fz) != nil || !fz.Frozen || fz.Scope != "all" || fz.Reason != "drill" {
		t.Fatalf("freeze after restart: %d %s", code, b)
	}
	if !getStatus(t, r2.addr).Frozen {
		t.Fatal("status not frozen")
	}
	// A channel scope is the hub's business; the node stays as it is.
	if resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Unfreeze{Unfreeze: &mycelv1.Unfreeze{Scope: "channel:#x"}}}); !resp.GetOk() {
		t.Fatalf("channel unfreeze: %+v", resp)
	}
	if !getStatus(t, r2.addr).Frozen {
		t.Fatal("channel unfreeze unfroze the node")
	}
	if resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Unfreeze{Unfreeze: &mycelv1.Unfreeze{Scope: "all"}}}); !resp.GetOk() {
		t.Fatalf("unfreeze: %+v", resp)
	}
	if getStatus(t, r2.addr).Frozen {
		t.Fatal("still frozen")
	}
	if _, err := os.Stat(filepath.Join(dir, FrozenFile)); !os.IsNotExist(err) {
		t.Fatalf("frozen.json: %v", err)
	}
	if resp := ctl(t, h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Freeze{Freeze: &mycelv1.Freeze{Scope: "bogus"}}}); resp.GetOk() {
		t.Fatal("bogus scope accepted")
	}
}

func TestFreezeStopsWorkers(t *testing.T) {
	if !canPause {
		t.Skip("no SIGSTOP on " + runtime.GOOS)
	}
	e := startLocal(t, workerTweak)
	boss := e.c.mustRegister("boss")
	dir := t.TempDir()
	tick := filepath.Join(dir, "tick.log")
	sr := e.c.mustSpawn(boss.Session, spawnReq("test-family", map[string]any{"name": "w1", "workdir": dir}))
	waitFor(t, 10*time.Second, "child ticking", func() bool { return fileSize(tick) > 0 })

	if r := ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Freeze{Freeze: &mycelv1.Freeze{Scope: "all"}}}); !r.GetOk() {
		t.Fatalf("freeze: %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	before := fileSize(tick)
	time.Sleep(400 * time.Millisecond)
	if after := fileSize(tick); after != before {
		t.Fatalf("worker kept running while frozen: %d -> %d", before, after)
	}
	// A hub resume during the freeze keeps the process stopped.
	ctlAgent(t, e, sr.AgentID, "pause", "x")
	ctlAgent(t, e, sr.AgentID, "resume", "")
	time.Sleep(300 * time.Millisecond)
	if after := fileSize(tick); after != before {
		t.Fatalf("resume during freeze continued the worker: %d -> %d", before, after)
	}
	if r := ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Unfreeze{Unfreeze: &mycelv1.Unfreeze{}}}); !r.GetOk() {
		t.Fatalf("unfreeze: %+v", r)
	}
	waitFor(t, 5*time.Second, "worker ticking again", func() bool { return fileSize(tick) > before })
	if code, _, _ := e.c.deleteWorker(sr.AgentID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	e.c.waitWorkerState(sr.AgentID, workerExited)
}
