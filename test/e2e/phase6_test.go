//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/web"
	fh "github.com/richard-sen27/mycelium/test/fakeharness"
)

// Phase 6 acceptance: a node UI token cannot change anything outside its
// own machine (TestNodeUIScopeCannotWrite); the hub serves the app, signs
// a browser in, guards cookie requests with the CSRF header and the origin
// check, streams filtered events, searches, replays the audit log, starts
// passkey registration and stores push subscriptions
// (TestUIServedAndEvents). The browser smoke test is phase6_browser_test.go.

// ---- Connect over plain HTTP ----

// hubProcedures lists every Connect procedure of the mycel.v1 services,
// mapped to whether it is a server stream.
func hubProcedures(t *testing.T) map[string]bool {
	t.Helper()
	_ = mycelv1.File_mycel_v1_ui_proto // registers the package's files
	out := map[string]bool{}
	protoregistry.GlobalFiles.RangeFilesByPackage("mycel.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				out["/"+string(sd.FullName())+"/"+string(md.Name())] = md.IsStreamingServer()
			}
		}
		return true
	})
	if len(out) < 50 {
		t.Fatalf("only %d Connect procedures found", len(out))
	}
	return out
}

// connectReply is the outcome of one raw Connect call.
type connectReply struct {
	status  int    // HTTP status
	code    string // Connect error code ("" on success)
	message string // Connect error message
	body    []byte // unary: the response body; stream: the first message
}

// connectCall posts {} to procedure at base (unary JSON, or one enveloped
// message for a server stream) with the extra headers.
func connectCall(t *testing.T, c *http.Client, base, procedure string, stream bool, hdr map[string]string) connectReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, ctype := []byte("{}"), "application/json"
	if stream {
		body, ctype = append([]byte{0, 0, 0, 0, 2}, body...), "application/connect+json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+procedure, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Connect-Protocol-Version", "1")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s%s: %v", base, procedure, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("POST %s%s: reading: %v", base, procedure, err)
	}
	r := connectReply{status: resp.StatusCode, body: b}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if !stream || resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/connect+") {
		if resp.StatusCode != http.StatusOK && json.Unmarshal(b, &e) == nil {
			r.code, r.message = e.Code, e.Message
		}
		return r
	}
	// A server stream: envelopes; the end-stream one (flag 2) may carry an error.
	r.body = nil
	for len(b) >= 5 {
		flags, n := b[0], int(binary.BigEndian.Uint32(b[1:5]))
		if len(b) < 5+n {
			break
		}
		msg := b[5 : 5+n]
		b = b[5+n:]
		if flags&2 == 0 {
			if r.body == nil {
				r.body = msg
			}
			continue
		}
		if json.Unmarshal(msg, &e) == nil && e.Error != nil {
			r.code, r.message = e.Error.Code, e.Error.Message
		}
	}
	return r
}

// hubCAPool is the hub's CA (the browser trusts it; nodes pin it).
func hubCAPool(t *testing.T, h *Hub) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(h.Home, "data", "hub", "ca.crt")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("hub ca.crt holds no certificate")
	}
	return pool
}

// nodeHTTP is an HTTP client that presents n's node certificate to the hub.
func nodeHTTP(t *testing.T, h *Hub, n *Node) *http.Client {
	t.Helper()
	dir := filepath.Join(n.Home, "data", "node")
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, daemon.CertFile), filepath.Join(dir, daemon.KeyFile))
	if err != nil {
		t.Fatalf("%s: node key pair: %v", n.Name, err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: hubCAPool(t, h), Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
	}, ForceAttemptHTTP2: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

// ---- browser sessions ----

// headerTransport adds fixed headers to every request.
type headerTransport struct {
	base http.RoundTripper
	hdr  map[string]string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.hdr {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

// uiSession is a browser signed in to the hub UI with an API key.
type uiSession struct {
	h    *Hub
	jar  http.CookieJar
	tr   *http.Transport
	csrf string
	me   jsonMe
}

type jsonMe struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	CSRF   string `json:"csrf"`
}

// signIn signs in through the API-key form at /ui/login (what the app's
// sign-in screen does) and reads the session's CSRF token from
// /api/auth/me.
func signIn(t *testing.T, h *Hub, apiKey string) *uiSession {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: hubCAPool(t, h), MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
	t.Cleanup(tr.CloseIdleConnections)
	s := &uiSession{h: h, jar: jar, tr: tr}
	c := &http.Client{Jar: jar, Transport: tr, Timeout: 15 * time.Second}
	code, page := uiDo(t, c, http.MethodGet, h.HubURL+"/ui/login", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /ui/login: %d\n%s", code, page)
	}
	if code, page = uiDo(t, c, http.MethodPost, h.HubURL+"/ui/login",
		url.Values{"csrf": {csrfOf(t, page)}, "api_key": {apiKey}}); code != http.StatusOK {
		t.Fatalf("POST /ui/login: %d\n%s", code, page)
	}
	code, body := s.do(t, http.MethodGet, "/api/auth/me", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/auth/me after sign-in: %d %s", code, body)
	}
	if err := json.Unmarshal(body, &s.me); err != nil || s.me.CSRF == "" || s.me.UserID == "" {
		t.Fatalf("GET /api/auth/me: %v %s", err, body)
	}
	s.csrf = s.me.CSRF
	return s
}

// client is an HTTP client with the session cookie and the given headers
// (no overall timeout, so it can carry streams).
func (s *uiSession) client(hdr map[string]string) *http.Client {
	return &http.Client{Jar: s.jar, Transport: headerTransport{base: s.tr, hdr: hdr}}
}

// browserHeaders are what the app sends: the CSRF token and its origin.
func (s *uiSession) browserHeaders() map[string]string {
	return map[string]string{"Mycel-Csrf": s.csrf, "Origin": s.h.HubURL}
}

// do sends one JSON request with the browser headers.
func (s *uiSession) do(t *testing.T, method, path, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.h.HubURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if hdr == nil && method != http.MethodGet {
		hdr = s.browserHeaders()
	}
	resp, err := s.client(hdr).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

// events is the session's EventService client.
func (s *uiSession) events() mycelv1connect.EventServiceClient {
	return mycelv1connect.NewEventServiceClient(s.client(s.browserHeaders()), s.h.HubURL)
}

// eventStream collects one WatchEvents stream.
type eventStream struct {
	mu     sync.Mutex
	events []*mycelv1.Event
	err    error
	done   chan struct{}
	cancel context.CancelFunc
}

func watchEvents(t *testing.T, c mycelv1connect.EventServiceClient) *eventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.WatchEvents(ctx, connect.NewRequest(&mycelv1.WatchEventsRequest{}))
	if err != nil {
		cancel()
		t.Fatalf("WatchEvents: %v", err)
	}
	es := &eventStream{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(es.done)
		for st.Receive() {
			ev := st.Msg().GetEvent()
			if ev == nil {
				continue // a heartbeat
			}
			es.mu.Lock()
			es.events = append(es.events, ev)
			es.mu.Unlock()
		}
		es.mu.Lock()
		es.err = st.Err()
		es.mu.Unlock()
	}()
	t.Cleanup(func() {
		cancel()
		<-es.done
	})
	return es
}

func (es *eventStream) snapshot() []*mycelv1.Event {
	es.mu.Lock()
	defer es.mu.Unlock()
	return slices.Clone(es.events)
}

// wait waits for an event matching f and returns it.
func (es *eventStream) wait(t *testing.T, timeout time.Duration, what string, f func(*mycelv1.Event) bool) *mycelv1.Event {
	t.Helper()
	var hit *mycelv1.Event
	if !waitFor(t, timeout, func() bool {
		for _, ev := range es.snapshot() {
			if f(ev) {
				hit = ev
				return true
			}
		}
		return false
	}) {
		es.mu.Lock()
		err := es.err
		es.mu.Unlock()
		t.Fatalf("no %s event within %s (stream error: %v); got %d events", what, timeout, err, len(es.snapshot()))
	}
	return hit
}

func isMessage(text string) func(*mycelv1.Event) bool {
	return func(ev *mycelv1.Event) bool {
		return ev.GetKind() == mycelv1.EventKind_EVENT_KIND_MESSAGE && ev.GetMessage().GetPayload().GetText() == text
	}
}

// ---- helpers ----

type jsonPosted struct {
	Message struct {
		ID   string `json:"id"`
		Meta struct {
			TraceID string `json:"trace_id"`
		} `json:"meta"`
	} `json:"message"`
}

// waitConnected waits until the nodes' daemons report a hub connection.
func waitConnected(t *testing.T, nodes ...*Node) {
	t.Helper()
	for _, n := range nodes {
		if !waitFor(t, 20*time.Second, func() bool {
			st := n.Status(t)
			return st != nil && st["connected"] == true
		}) {
			t.Fatalf("%s never connected: %v", n.Name, n.Status(t))
		}
	}
}

// localAs sends a request to n's local API with the given bearer token.
func localAs(t *testing.T, n *Node, token, method, path, body string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, n.LocalURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, n.Name, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b
}

// ---- tests ----

// TestNodeUIScopeCannotWrite proves the spec's phase 6 rule: a node UI
// token reads the owner's network view but cannot change anything outside
// its own machine, neither through the daemon's proxy nor by presenting
// the node certificate to the hub directly.
func TestNodeUIScopeCannotWrite(t *testing.T) {
	t.Parallel()
	up := newFakeOllama(t)
	h := startHubWith(t, hubOptions{HubYAML: "grants:\n  auto:\n    - kind: ollama\n      node: alpha\n      max_ttl: 5m\n"})
	aliceKey, bobKey := addMember(t, h, "alice"), addMember(t, h, "bob")
	dir := t.TempDir()
	env := []string{fakeOK, "MYCEL_OLLAMA_UPSTREAM=" + up.URL, "DOCKER_HOST=unix://" + filepath.Join(dir, "no-docker.sock")}
	alpha := joinNode(t, h, "alpha", "ollama,sandbox", aliceKey, env...)
	beta := joinNode(t, h, "beta", "ollama,sandbox", bobKey, env...)
	waitConnected(t, alpha, beta)
	alphaTok, betaTok := alpha.localToken(t), beta.localToken(t)

	// The node UI's config.
	var cfg struct {
		Mode string `json:"mode"`
		Node string `json:"node"`
		Hub  string `json:"hub"`
	}
	if code, b := localAs(t, alpha, "", http.MethodGet, "/api/ui-config", ""); code != http.StatusOK || json.Unmarshal(b, &cfg) != nil ||
		cfg.Mode != "node" || cfg.Node != "alpha" || cfg.Hub != h.HubURL {
		t.Fatalf("alpha GET /api/ui-config: %d %s", code, b)
	}

	// The network view through the proxy: alice's view, as a member.
	proxy := func(token, proc string, stream bool) connectReply {
		t.Helper()
		return connectCall(t, http.DefaultClient, alpha.LocalURL+"/api/hub", proc, stream,
			map[string]string{"Authorization": "Bearer " + token})
	}
	r := proxy(alphaTok, mycelv1connect.NodeServiceListNodesProcedure, false)
	var nodes struct {
		Nodes []jsonNode `json:"nodes"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &nodes) != nil {
		t.Fatalf("ListNodes through alpha's proxy: %d %s", r.status, r.body)
	}
	names := []string{}
	for _, n := range nodes.Nodes {
		names = append(names, n.Name)
	}
	if !slices.Contains(names, "alpha") || !slices.Contains(names, "beta") {
		t.Errorf("ListNodes through the proxy: %v; want alpha and beta (nodes are visible to every member)", names)
	}
	r = proxy(alphaTok, mycelv1connect.UserServiceWhoamiProcedure, false)
	var who struct {
		User struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &who) != nil || who.User.Name != "alice" || who.User.Role != "ROLE_MEMBER" {
		t.Errorf("Whoami through alpha's proxy: %d %s; want alice as a member", r.status, r.body)
	}
	r = proxy(alphaTok, mycelv1connect.TokenServiceListJoinTokensProcedure, false)
	var toks struct { // Connect's JSON uses the camelCase field names
		Tokens []struct {
			NodeName string `json:"nodeName"`
			OwnerID  string `json:"ownerId"`
		} `json:"tokens"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &toks) != nil {
		t.Fatalf("ListJoinTokens through the proxy: %d %s", r.status, r.body)
	}
	for _, tk := range toks.Tokens {
		if tk.NodeName != "alpha" {
			t.Errorf("alice's view lists the join token of %q (owner %s)", tk.NodeName, tk.OwnerID)
		}
	}
	if len(toks.Tokens) == 0 {
		t.Errorf("alice's view lists no join token; want alpha's")
	}

	// Every procedure outside the read-only allowlist: 403 from the proxy,
	// and PermissionDenied "node read scope" from the hub when the node
	// certificate is presented directly with the read scope header.
	readOnly := map[string]bool{}
	for _, p := range daemon.HubReadOnlyProcedures {
		readOnly[p] = true
	}
	procs := hubProcedures(t)
	nc := nodeHTTP(t, h, alpha)
	scoped := map[string]string{"Mycel-Scope": "network:read"}
	var refused []string
	for _, p := range slices.Sorted(maps.Keys(procs)) {
		if readOnly[p] {
			continue
		}
		stream := procs[p]
		if r := proxy(alphaTok, p, stream); r.status != http.StatusForbidden {
			t.Errorf("%s through alpha's proxy: %d %s; want 403", p, r.status, r.body)
		}
		if r := connectCall(t, nc, h.HubURL, p, stream, scoped); r.code != "permission_denied" || r.message != "node read scope" {
			t.Errorf("%s to the hub with alpha's certificate and the read scope: %d %s %q; want permission_denied \"node read scope\"",
				p, r.status, r.code, r.message)
		}
		refused = append(refused, p)
	}
	for _, must := range []string{
		mycelv1connect.ChannelServicePostProcedure, mycelv1connect.ChannelServiceCreateChannelProcedure,
		mycelv1connect.JobServiceRunJobProcedure, mycelv1connect.GrantServiceRequestGrantProcedure,
		mycelv1connect.GrantServiceApproveGrantProcedure, mycelv1connect.ControlServiceSetNodeSharingProcedure,
		mycelv1connect.NodeServiceSetNodeCapsProcedure, mycelv1connect.NodeServiceRemoveNodeProcedure,
		mycelv1connect.UserServiceCreateUserProcedure, mycelv1connect.UserServiceCreateApiKeyProcedure,
		mycelv1connect.AgentServicePauseAgentProcedure, mycelv1connect.ControlServiceFreezeAllProcedure,
	} {
		if !slices.Contains(refused, must) {
			t.Errorf("%s was not checked (not in the proto services, or on the read-only list)", must)
		}
	}
	t.Logf("%d procedures refused through the proxy and by the hub; %d read-only", len(refused), len(readOnly))
	// An allowed read with the scope works directly too; without the header
	// the certificate carries no user rights.
	if r := connectCall(t, nc, h.HubURL, mycelv1connect.NodeServiceListNodesProcedure, false, scoped); r.status != http.StatusOK {
		t.Errorf("ListNodes with alpha's certificate and the read scope: %d %s", r.status, r.body)
	}
	if r := connectCall(t, nc, h.HubURL, mycelv1connect.NodeServiceListNodesProcedure, false, nil); r.code != "unauthenticated" {
		t.Errorf("ListNodes with alpha's certificate and no scope header: %d %s; want unauthenticated", r.status, r.body)
	}
	if r := connectCall(t, nc, h.HubURL, mycelv1connect.ChannelServicePostProcedure, false, nil); r.code != "unauthenticated" {
		t.Errorf("Post with alpha's certificate and no scope header: %d %s; want unauthenticated", r.status, r.body)
	}

	// "This machine" (restrict only) works with alpha's token: tighten the
	// sharing settings and revoke a grant attached on alpha.
	localFirstOff(t, h, alpha)
	mustRun(t, alpha, "llm", []fh.Step{{Tool: "whoami", Expect: "agent:llm@alpha"}})
	var req struct {
		Grant jsonGrant `json:"grant"`
	}
	mustJSON(t, h.MustCLI("", "grants", "request", "--kind", "ollama", "--subject", "agent:llm@alpha", "--duration", "5m", "--json"), &req)
	g := req.Grant
	if g.State != grantActive {
		t.Fatalf("ollama grant on alpha: %+v; want active by the auto rule", g)
	}
	if l, ok := waitLocal(t, alpha, "/api/local/grants", 15*time.Second, func(l jsonLocalGrants) bool { return l.has(g.ID) }); !ok {
		t.Fatalf("alpha never attached %s: %+v", g.ID, l)
	}
	if code, b := localAs(t, alpha, alphaTok, http.MethodDelete, "/api/local/grants/"+g.ID, ""); code != http.StatusNoContent {
		t.Errorf("DELETE /api/local/grants/%s with alpha's token: %d %s; want 204", g.ID, code, b)
	}
	waitGrant(t, h, g.ID, 10*time.Second, "revoked", func(x jsonGrant) bool { return x.State == grantRevoked })
	// Tighten: the effective settings with fewer offers.
	var sharing map[string]json.RawMessage
	code, b := localAs(t, alpha, alphaTok, http.MethodGet, "/api/local/sharing", "")
	if code != http.StatusOK || json.Unmarshal(b, &sharing) != nil {
		t.Fatalf("GET /api/local/sharing with alpha's token: %d %s", code, b)
	}
	var eff map[string]any
	if err := json.Unmarshal(sharing["effective"], &eff); err != nil {
		t.Fatalf("effective sharing settings: %v: %s", err, b)
	}
	eff["offer"] = map[string]any{"names": []string{"sandbox"}}
	tighter, err := json.Marshal(eff)
	if err != nil {
		t.Fatal(err)
	}
	if code, b := localAs(t, alpha, alphaTok, http.MethodPut, "/api/local/sharing", string(tighter)); code != http.StatusOK {
		t.Errorf("PUT /api/local/sharing (tighten to %s) with alpha's token: %d %s; want 200", tighter, code, b)
	}
	var v jsonSharingView
	if !alpha.LocalJSON(t, "/api/local/sharing", &v) || !slices.Equal(v.Effective.offers(), []string{"sandbox"}) {
		t.Errorf("alpha's effective offers after the local tightening: %+v; want sandbox only", v.Effective)
	}

	// Alpha's token means nothing to beta's daemon.
	if betaTok == alphaTok {
		t.Fatal("alpha and beta have the same local token")
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/local/sharing", ""},
		{http.MethodPut, "/api/local/sharing", `{"offer":{"names":[]}}`},
		{http.MethodGet, "/api/local/grants", ""},
		{http.MethodPost, "/api/hub" + mycelv1connect.NodeServiceListNodesProcedure, "{}"},
		{http.MethodPost, "/api/local/ui-code", ""},
	} {
		if code, b := localAs(t, beta, alphaTok, c.method, c.path, c.body); code != http.StatusUnauthorized {
			t.Errorf("%s %s on beta with alpha's token: %d %s; want 401", c.method, c.path, code, b)
		}
	}

	// The hub audited the scope refusals.
	var audit struct {
		Entries []struct {
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Outcome  string `json:"outcome"`
		} `json:"entries"`
	}
	mustJSON(t, h.MustCLI("", "audit", "ls", "--actor", "node:alpha", "--limit", "1000", "--json"), &audit)
	denied := 0
	for _, e := range audit.Entries {
		if e.Action == "scope.network_read" && e.Outcome == "denied" {
			denied++
		}
	}
	if denied == 0 {
		t.Errorf("the hub's audit log holds no scope.network_read refusal by node:alpha: %+v", audit.Entries)
	}
	t.Logf("audit: %d scope refusals recorded for node:alpha (sampled per actor)", denied)
}

var assetRE = regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`)

// TestUIServedAndEvents covers the hub UI's server side: the app, sign-in,
// CSRF and origin rules, the event stream with its filtering, search,
// audit replay, passkey registration options and push subscriptions.
func TestUIServedAndEvents(t *testing.T) {
	t.Parallel()
	h := startHubWith(t, hubOptions{HubYAML: "ui:\n  rp_id: localhost\n"})
	aliceKey, bobKey := addMember(t, h, "alice"), addMember(t, h, "bob")
	pool := hubCAPool(t, h)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(tr.CloseIdleConnections)
	anon := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	get := func(c *http.Client, u string) (*http.Response, string) {
		t.Helper()
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return resp, string(b)
	}

	// The app (or the "not built" page when this tree has no UI build).
	resp, page := get(anon, h.HubURL+"/")
	csp := resp.Header.Get("Content-Security-Policy")
	if web.Built() {
		if resp.StatusCode != http.StatusOK || !strings.Contains(csp, "script-src 'self'") ||
			!strings.Contains(page, `<div id="root"`) || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET / with the UI built: %d CSP %q cache %q\n%s", resp.StatusCode, csp, resp.Header.Get("Cache-Control"), page)
		}
		m := assetRE.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("index.html references no /assets/ file:\n%s", page)
		}
		ar, _ := get(anon, h.HubURL+m[1])
		if ar.StatusCode != http.StatusOK || !strings.Contains(ar.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("GET %s: %d cache %q; want 200 immutable", m[1], ar.StatusCode, ar.Header.Get("Cache-Control"))
		}
		if rr, body := get(anon, h.HubURL+"/approvals?grant=x"); rr.StatusCode != http.StatusOK || body != page {
			t.Errorf("GET /approvals (client route): %d; want index.html", rr.StatusCode)
		}
		t.Logf("UI build embedded: / serves index.html, %s is immutable", m[1])
	} else {
		if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Mycelium hub e2e") {
			t.Fatalf("GET / without a UI build: %d\n%s", resp.StatusCode, page)
		}
		if rr, body := get(anon, h.HubURL+"/approvals"); rr.StatusCode != http.StatusNotFound || !strings.Contains(body, "not built") ||
			!strings.Contains(rr.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("GET /approvals without a UI build: %d %s", rr.StatusCode, body)
		}
		t.Log("no UI build embedded (run `make web` first): checked the \"not built\" page")
	}
	var cfg map[string]any
	if rr, body := get(anon, h.HubURL+"/api/ui-config"); rr.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &cfg) != nil ||
		cfg["mode"] != "hub" || cfg["rp_id"] != "localhost" || cfg["passkeys"] != true {
		t.Fatalf("GET /api/ui-config: %d %s", rr.StatusCode, body)
	}

	// Sign-in with an API key yields a session; /api/auth/me returns the CSRF token.
	admin := signIn(t, h, h.AdminKey)
	if admin.me.Role != "admin" || admin.me.UserID != h.AdminUserID {
		t.Fatalf("admin session: %+v", admin.me)
	}
	bob := signIn(t, h, bobKey)

	// Connect with the cookie: works with the CSRF header and same origin;
	// without the header, or from another origin, the call is unauthenticated.
	whoami := func(hdr map[string]string) (*connect.Response[mycelv1.WhoamiResponse], error) {
		c := mycelv1connect.NewUserServiceClient(admin.client(hdr), h.HubURL)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return c.Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
	}
	if w, err := whoami(admin.browserHeaders()); err != nil || w.Msg.GetUser().GetId() != h.AdminUserID {
		t.Fatalf("Whoami with the session and CSRF header: %v %v", w, err)
	}
	if _, err := whoami(map[string]string{"Origin": h.HubURL}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Whoami with the cookie but no CSRF header: %v; want unauthenticated", err)
	}
	if _, err := whoami(map[string]string{"Mycel-Csrf": admin.csrf, "Origin": "https://evil.example"}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Whoami with the cookie from a cross-site origin: %v; want unauthenticated", err)
	}
	if _, err := whoami(map[string]string{"Mycel-Csrf": bob.csrf, "Origin": h.HubURL}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Whoami with another session's CSRF token: %v; want unauthenticated", err)
	}

	// The event stream: the opening heartbeat carries the resume point.
	adminEv, bobEv := watchEvents(t, admin.events()), watchEvents(t, bob.events())
	first := adminEv.wait(t, 10*time.Second, "opening heartbeat", func(*mycelv1.Event) bool { return true })
	if first.GetKind() != mycelv1.EventKind_EVENT_KIND_UNSPECIFIED || first.GetBody() != nil || first.GetSeq() == 0 {
		t.Fatalf("first event %v; want a heartbeat (kind unspecified, no body) with the current seq", first)
	}
	bobEv.wait(t, 10*time.Second, "opening heartbeat", func(*mycelv1.Event) bool { return true })

	// A node connects: a NODE event.
	gamma := joinNode(t, h, "gamma", "", aliceKey)
	waitConnected(t, gamma)
	nodeEv := adminEv.wait(t, 20*time.Second, "NODE gamma", func(ev *mycelv1.Event) bool {
		return ev.GetKind() == mycelv1.EventKind_EVENT_KIND_NODE && ev.GetId() == "gamma"
	})
	if nodeEv.GetSeq() <= first.GetSeq() {
		t.Errorf("NODE event seq %d not after the opening seq %d", nodeEv.GetSeq(), first.GetSeq())
	}

	// Posts: alice's private channel reaches the admin, not bob.
	const private, public = "e2e-private-7c1f only for alice", "e2e-public-7c1f hello everyone"
	h.MustCLI(aliceKey, "channels", "create", "#alice-private", "--json")
	h.MustCLI(aliceKey, "post", "#alice-private", private, "--json")
	var posted jsonPosted
	mustJSON(t, h.MustCLI(aliceKey, "post", "#general", public, "--json"), &posted)
	adminEv.wait(t, 10*time.Second, "MESSAGE (private)", isMessage(private))
	adminEv.wait(t, 10*time.Second, "MESSAGE (public)", isMessage(public))
	bobEv.wait(t, 10*time.Second, "MESSAGE (public)", isMessage(public))
	for _, ev := range bobEv.snapshot() {
		if isMessage(private)(ev) || ev.GetMessage().GetChannel() == "#alice-private" {
			t.Errorf("bob's stream carries a message of alice's private channel: %v", ev)
		}
		if ev.GetKind() == mycelv1.EventKind_EVENT_KIND_CHANNEL && ev.GetId() == "#alice-private" {
			t.Errorf("bob's stream carries alice's private channel: %v", ev)
		}
	}

	// Search: a node by name, a message by body (and bob cannot find the private one).
	search := func(s *uiSession, q string) []*mycelv1.SearchHit {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r, err := s.events().Search(ctx, connect.NewRequest(&mycelv1.SearchRequest{Query: q}))
		if err != nil {
			t.Fatalf("Search %q: %v", q, err)
		}
		return r.Msg.GetHits()
	}
	hasHit := func(hits []*mycelv1.SearchHit, kind mycelv1.EventKind, id string) bool {
		return slices.ContainsFunc(hits, func(x *mycelv1.SearchHit) bool { return x.GetKind() == kind && (id == "" || x.GetId() == id) })
	}
	if hits := search(admin, "gamm"); !hasHit(hits, mycelv1.EventKind_EVENT_KIND_NODE, "gamma") {
		t.Errorf("Search gamm: %v; want node gamma", hits)
	}
	if hits := search(admin, "e2e-public-7c1f"); !hasHit(hits, mycelv1.EventKind_EVENT_KIND_MESSAGE, posted.Message.ID) {
		t.Errorf("Search e2e-public-7c1f: %v; want message %s", hits, posted.Message.ID)
	}
	if hits := search(bob, "e2e-private-7c1f"); len(hits) != 0 {
		t.Errorf("bob's search finds alice's private message: %v", hits)
	}

	// Audit replay by trace id.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	trace := posted.Message.Meta.TraceID
	al, err := admin.events().ListAudit(ctx, connect.NewRequest(&mycelv1.ListAuditRequest{TraceId: trace}))
	if err != nil {
		t.Fatalf("ListAudit by trace: %v", err)
	}
	if trace == "" || len(al.Msg.GetEntries()) != 1 || al.Msg.GetEntries()[0].GetAction() != "channel.post" ||
		al.Msg.GetEntries()[0].GetTraceId() != trace {
		t.Errorf("ListAudit trace %q: %v; want the one channel.post entry", trace, al.Msg.GetEntries())
	}
	// The audit event of that post reached the admin's stream.
	adminEv.wait(t, 10*time.Second, "AUDIT channel.post", func(ev *mycelv1.Event) bool {
		return ev.GetKind() == mycelv1.EventKind_EVENT_KIND_AUDIT && ev.GetAudit().GetTraceId() == trace
	})

	// Passkey registration options carry the RP id from hub.yaml.
	code, body := admin.do(t, http.MethodPost, "/api/auth/passkey/register/begin", `{"name":"e2e key"}`, nil)
	var opts struct {
		PublicKey struct {
			RP struct {
				ID string `json:"id"`
			} `json:"rp"`
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &opts) != nil || opts.PublicKey.RP.ID != "localhost" || opts.PublicKey.Challenge == "" {
		t.Errorf("POST /api/auth/passkey/register/begin: %d %s; want options with rp.id localhost", code, body)
	}
	if code, body := admin.do(t, http.MethodPost, "/api/auth/passkey/register/begin", `{"name":"x"}`, map[string]string{"Origin": h.HubURL}); code != http.StatusUnauthorized {
		t.Errorf("register/begin without the CSRF header: %d %s; want 401", code, body)
	}

	// Push: the VAPID key, and a subscription that is stored.
	code, body = admin.do(t, http.MethodGet, "/api/push/vapid", "", nil)
	var vapid struct {
		PublicKey string `json:"public_key"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &vapid) != nil || len(vapid.PublicKey) < 80 {
		t.Errorf("GET /api/push/vapid: %d %s", code, body)
	}
	sub := `{"endpoint":"https://push.example.com/e2e-7c1f","keys":{"p256dh":"` + strings.Repeat("A", 87) + `","auth":"` + strings.Repeat("B", 22) + `"}}`
	if code, body := admin.do(t, http.MethodPost, "/api/push/subscriptions", sub, nil); code != http.StatusCreated {
		t.Errorf("POST /api/push/subscriptions: %d %s; want 201", code, body)
	}
	// Stored: the same endpoint is now the admin's, so bob cannot take it.
	if code, body := bob.do(t, http.MethodPost, "/api/push/subscriptions", sub, nil); code != http.StatusConflict {
		t.Errorf("bob subscribing the admin's endpoint: %d %s; want 409 (the subscription is stored for the admin)", code, body)
	}
}
