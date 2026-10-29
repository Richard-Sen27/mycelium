//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/richard-sen27/mycelium/internal/daemon"
)

// Phase 7 acceptance on one machine: a hub behind a relay, hub discovery
// and a hub move followed by nodes, CLIs and join tokens, the export
// round trip, the install scripts' and service's entry points, and the
// quickstart and demo made executable. The two-machine run of the spec's
// done-when (a fresh Mac and a fresh Windows PC in ten minutes) is the
// owner's.
//
// Nothing here touches the network: relay, hubs and nodes listen on
// loopback, hubs run with --no-mdns and MYCEL_NO_MDNS=1, and the
// discovery test uses the test binary's fake mDNS (a shared directory,
// internal/discovery/fakemdns). Real multicast runs only on Linux CI with
// MYCEL_TEST_MDNS=1.

// ---- relay ----

// relay is a `mycel relay serve` on loopback with fixed ports, so a
// restarted relay keeps the addresses in the hub's relay URL.
type relay struct {
	t       *testing.T
	root    string
	dataDir string
	control int
	api     int // public port forwarded to the hub API
	bus     int // public port forwarded to the hub bus
	proc    *proc
}

func newRelay(t *testing.T, root string) *relay {
	t.Helper()
	ports := freePorts(t, 3)
	return &relay{t: t, root: root, dataDir: filepath.Join(root, "relay"), control: ports[0], api: ports[1], bus: ports[2]}
}

// start runs the relay and waits until its control port accepts.
func (r *relay) start() {
	r.t.Helper()
	pub := "127.0.0.1:" + strconv.Itoa(r.api) + ",127.0.0.1:" + strconv.Itoa(r.bus)
	r.proc = startProc(r.t, "relay", r.root, childEnv("MYCEL_HOME="+filepath.Join(r.root, "relay-home")),
		"relay", "serve", "--listen", "127.0.0.1:"+strconv.Itoa(r.control), "--public", pub, "--data-dir", r.dataDir)
	addr := "127.0.0.1:" + strconv.Itoa(r.control)
	if !waitFor(r.t, readyTimeout, func() bool {
		select {
		case <-r.proc.done:
			return false
		default:
		}
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}) {
		r.t.Fatalf("relay not listening on %s\nstderr:\n%s", addr, readFile(r.proc.stderrPath))
	}
}

// token creates the relay token for hub name and returns the relay URL.
// The URL holds a secret: it is never logged.
func (r *relay) token(name string) string {
	r.t.Helper()
	out, _, code := runCLI(r.t, childEnv("MYCEL_HOME="+filepath.Join(r.root, "relay-home")),
		"relay", "token", "create", "--name", name, "--host", "127.0.0.1", "--port", strconv.Itoa(r.control),
		"--data-dir", r.dataDir, "--json")
	var tok struct {
		URL string `json:"url"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &tok) != nil || !strings.HasPrefix(tok.URL, "relay://") {
		r.t.Fatalf("relay token create: exit %d (output not shown: it holds the relay secret)", code)
	}
	return tok.URL
}

type jsonTunnelStatus struct {
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Connects  uint64 `json:"connects"`
	Total     uint64 `json:"total"`
	Error     string `json:"error"`
	LastError string `json:"last_error"`
}

func tunnelStatus(t *testing.T, h *Hub) jsonTunnelStatus {
	t.Helper()
	var st jsonTunnelStatus
	mustJSON(t, h.MustCLI("", "hub", "tunnel", "status", "--json"), &st)
	return st
}

func waitTunnel(t *testing.T, h *Hub, timeout time.Duration, cond func(jsonTunnelStatus) bool) (jsonTunnelStatus, bool) {
	t.Helper()
	var st jsonTunnelStatus
	ok := waitFor(t, timeout, func() bool {
		st = tunnelStatus(t, h)
		return cond(st)
	})
	return st, ok
}

// nodeState reads the node's node.yaml.
func nodeState(t *testing.T, n *Node) daemon.State {
	t.Helper()
	st, err := daemon.ReadState(filepath.Join(n.Home, "data", "node"))
	if err != nil {
		t.Fatalf("%s: node state: %v", n.Name, err)
	}
	return *st
}

// nodeConnected waits until the node's daemon reports its hub connection up.
func nodeConnected(t *testing.T, n *Node, timeout time.Duration) bool {
	t.Helper()
	return waitFor(t, timeout, func() bool {
		st := n.Status(t)
		return st != nil && st["connected"] == true
	})
}

// fakeJobOK runs a short job with the fake profile on node and checks that
// it succeeded and printed want.
func fakeJobOK(t *testing.T, h *Hub, apiKey, node, want string) {
	t.Helper()
	args := append([]string{"--profile", "fake", "--node", node, "--timeout", "60s", "--"},
		shell("echo "+want, "echo "+want)...)
	j := runJob(t, h, apiKey, args...)
	j = waitFinished(t, h, j.ID, 60*time.Second)
	logs := jobLogs(t, h, j.ID)
	if j.State != jobSucceeded || j.Node != node || !strings.Contains(logs, want) {
		t.Fatalf("job %s: %+v, want %s on %s printing %q\nlogs:\n%s", j.ID, j, jobSucceeded, node, want, logs)
	}
}

// TestHubBehindRelay: a hub reachable only through a relay; a node joins
// with the relay's addresses alone, heartbeats and runs a job; the relay
// restarts and the hub and the node recover.
func TestHubBehindRelay(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := newRelay(t, root)
	r.start()
	relayURL := r.token("e2e-hub")

	// The hub: its own listeners on loopback ports the node never sees;
	// advertise URL and bus advertise on the relay's public ports.
	ports := freePorts(t, 2)
	h := &Hub{t: t, Root: root, Home: filepath.Join(root, "hub"), APIPort: ports[0], BusPort: ports[1],
		DaemonArgs: []string{"--allow-fake-profile"}}
	env := childEnv("MYCEL_HOME=" + h.Home)
	out, errOut, code := runCLIStdin(t, env, relayURL+"\n", "hub", "init", "--tunnel", "-",
		"--api-listen", "127.0.0.1:"+strconv.Itoa(h.APIPort), "--bus-listen", "127.0.0.1:"+strconv.Itoa(h.BusPort),
		"--trust-domain", "e2e", "--node-name", "hub-node", "--no-mdns", "--json")
	if code != 0 {
		t.Fatalf("hub init --tunnel -: exit %d\nstderr: %s", code, errOut)
	}
	var init hubInitOut
	mustJSON(t, out, &init)
	h.AdminKey, h.AdminUserID, h.CAFingerprint = init.AdminAPIKey, init.AdminUserID, init.CAFingerprint
	if strings.Contains(out, "token=") || strings.Contains(errOut, "token=") {
		t.Errorf("hub init printed the relay secret")
	}
	// hub init takes the relay's default ports (443, 7778); the test relay
	// listens on free ports, so map them.
	ports2 := strconv.Itoa(r.api) + "=api," + strconv.Itoa(r.bus) + "=bus"
	out, errOut, code = runCLIStdin(t, env, relayURL+"\n", "hub", "tunnel", "set", "-", "--ports", ports2, "--json")
	if code != 0 {
		t.Fatalf("hub tunnel set: exit %d\nstderr: %s", code, errOut)
	}
	var set struct {
		AdvertiseURL string `json:"advertise_url"`
		BusAdvertise string `json:"bus_advertise"`
		Relay        string `json:"relay"`
	}
	mustJSON(t, out, &set)
	wantAdv := "https://127.0.0.1:" + strconv.Itoa(r.api)
	wantBus := "tls://127.0.0.1:" + strconv.Itoa(r.bus)
	if set.AdvertiseURL != wantAdv || set.BusAdvertise != wantBus {
		t.Fatalf("tunnel set: advertise %q bus %q, want %q and %q", set.AdvertiseURL, set.BusAdvertise, wantAdv, wantBus)
	}
	if strings.Contains(out, "token=") || strings.Contains(set.Relay, "token=") {
		t.Errorf("tunnel set printed the relay secret")
	}
	if b, err := os.ReadFile(filepath.Join(h.Home, "data", "hub", "hub.yaml")); err != nil || strings.Contains(string(b), "token=") {
		t.Errorf("hub.yaml holds the relay secret (or is unreadable: %v)", err)
	}
	h.HubURL = set.AdvertiseURL
	h.Start()

	st, ok := waitTunnel(t, h, 15*time.Second, func(s jsonTunnelStatus) bool { return s.Connected })
	if !ok || !st.Enabled {
		t.Fatalf("tunnel not connected: %+v", st)
	}

	// The node knows only the relay's addresses.
	alpha := joinNode(t, h, "alpha", "sandbox", "", fakeOK)
	ns := nodeState(t, alpha)
	if ns.HubURL != wantAdv || ns.BusURL != wantBus {
		t.Fatalf("alpha's hub %q bus %q, want the relay's %q and %q", ns.HubURL, ns.BusURL, wantAdv, wantBus)
	}
	for _, own := range []int{h.APIPort, h.BusPort} {
		if strings.Contains(ns.HubURL+ns.BusURL, ":"+strconv.Itoa(own)) {
			t.Fatalf("alpha's state names the hub's own port %d: %+v", own, ns)
		}
	}
	if !nodeConnected(t, alpha, 30*time.Second) {
		t.Fatalf("alpha never connected through the relay: %v", alpha.Status(t))
	}
	waitBackends(t, h, "fake", alpha) // inventory and heartbeats arrive, alpha online
	localFirstOff(t, h, alpha)
	fakeJobOK(t, h, "", "alpha", "via-relay-1")
	st = tunnelStatus(t, h)
	if !st.Connected || st.Total == 0 {
		t.Fatalf("tunnel status after a job: %+v, want connected with forwarded connections", st)
	}

	// Kill the relay: the hub notices; a new relay process on the same
	// ports and data directory is attached again within 10 s.
	r.proc.kill()
	if st, ok := waitTunnel(t, h, 15*time.Second, func(s jsonTunnelStatus) bool { return !s.Connected }); !ok {
		t.Fatalf("tunnel still connected after the relay died: %+v", st)
	}
	r.start()
	restarted := time.Now()
	st, ok = waitTunnel(t, h, 10*time.Second, func(s jsonTunnelStatus) bool { return s.Connected })
	if !ok {
		t.Fatalf("hub not reattached within 10 s of the relay restart: %+v", st)
	}
	t.Logf("hub reattached %s after the relay restarted (sessions: %d)", time.Since(restarted).Round(10*time.Millisecond), st.Connects)
	if st.Connects < 2 {
		t.Errorf("tunnel status counts %d sessions, want at least 2", st.Connects)
	}
	if !nodeConnected(t, alpha, 30*time.Second) {
		t.Fatalf("alpha did not reconnect through the restarted relay: %v", alpha.Status(t))
	}
	t.Logf("alpha reconnected %s after the relay restarted", time.Since(restarted).Round(10*time.Millisecond))
	if !waitFor(t, 30*time.Second, func() bool { return listNodes(t, h, "")["alpha"].Online }) {
		t.Fatal("the hub does not list alpha online after the relay restart")
	}
	fakeJobOK(t, h, "", "alpha", "via-relay-2")
}

// ---- discovery and moving ----

// mdnsEnv is the fake mDNS of the test binary: processes sharing dir see
// each other's advertisements; nothing reaches the network.
func mdnsEnv(dir string) []string {
	return []string{"MYCEL_FAKE_MDNS_DIR=" + dir}
}

type jsonDiscovered struct {
	TrustDomain   string `json:"trust_domain"`
	URL           string `json:"url"`
	BusURL        string `json:"bus_url"`
	CAFingerprint string `json:"ca_fingerprint"`
	Pinned        bool   `json:"pinned"`
}

func discover(t *testing.T, home, mdnsDir string) []jsonDiscovered {
	t.Helper()
	out, errOut, code := runCLI(t, childEnv(append([]string{"MYCEL_HOME=" + home}, mdnsEnv(mdnsDir)...)...),
		"hub", "discover", "--timeout", "1s", "--json")
	if code != 0 {
		t.Fatalf("hub discover: exit %d\n%s", code, errOut)
	}
	var hubs []jsonDiscovered
	mustJSON(t, out, &hubs)
	return hubs
}

// startHubOnly starts `mycel daemon` in a home that holds only a hub
// (an imported one) and returns the process.
func startHubOnly(t *testing.T, name, root, home string, env []string, args ...string) *proc {
	t.Helper()
	p := startProc(t, name, root, append(childEnv("MYCEL_HOME="+home, "MYCEL_NO_MDNS=1"), env...),
		append([]string{"daemon", "--json", "--local-addr", "127.0.0.1:0"}, args...)...)
	var ready daemonReady
	p.waitReady(t, 0, &ready)
	if ready.Hub == nil {
		t.Fatalf("%s: ready line lacks the hub: %+v", name, ready)
	}
	return p
}

// joinAt joins a fresh home with token (extra join arguments and
// environment) and starts the node's daemon with env.
func joinAt(t *testing.T, root, name, token string, joinArgs, env []string) *Node {
	t.Helper()
	n := &Node{t: t, Home: filepath.Join(root, name), Name: name, Token: token}
	nenv := append(childEnv("MYCEL_HOME="+n.Home), env...)
	out, errOut, code := runCLI(t, nenv, append([]string{"join", token, "--json"}, joinArgs...)...)
	if code != 0 {
		t.Fatalf("join %s: exit %d\nstdout: %s\nstderr: %s", name, code, out, errOut)
	}
	t.Cleanup(func() { checkWorkerLogs(t, n.Home) })
	n.Proc = startProc(t, name, root, nenv, "daemon", "--json", "--local-addr", "127.0.0.1:0")
	var ready daemonReady
	n.Proc.waitReady(t, 0, &ready)
	if ready.Node == nil || ready.Node.LocalAddr == "" {
		t.Fatalf("%s: ready line lacks node.local_addr: %+v", name, ready)
	}
	n.LocalAddr = ready.Node.LocalAddr
	n.LocalURL = "http://" + ready.Node.LocalAddr
	return n
}

// configHubURL reads hub_url from the CLI's config.yaml in home.
func configHubURL(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "config.yaml")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatalf("config.yaml: %v", err)
	}
	var f struct {
		HubURL string `yaml:"hub_url"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatalf("config.yaml: %v", err)
	}
	return f.HubURL
}

// pinnedHubClient is an HTTPS client that trusts only the hub CA in home.
func pinnedHubClient(t *testing.T, hubHome string) (*http.Client, []byte) {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(hubHome, "data", "hub", "ca.crt")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("hub CA: no certificate")
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
	}}, caPEM
}

// connectPost posts to a channel with the Connect protocol (JSON) and
// returns the status, the Connect error code and the redirect header.
func connectPost(t *testing.T, c *http.Client, hubURL, apiKey, channel, text string) (status int, code, redirect string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
	req, err := http.NewRequest(http.MethodPost, hubURL+"/mycel.v1.ChannelService/Post", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", hubURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var e struct {
		Code string `json:"code"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(b, &e)
	return resp.StatusCode, e.Code, resp.Header.Get(daemon.RedirectHeader)
}

type jsonUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func listUsers(t *testing.T, h *Hub) []jsonUser {
	t.Helper()
	var resp struct {
		Users []jsonUser `json:"users"`
	}
	mustJSON(t, h.MustCLI("", "user", "ls", "--json"), &resp)
	return resp.Users
}

func channelTexts(t *testing.T, h *Hub, apiKey, ch string) []string {
	t.Helper()
	var resp struct {
		Messages []jsonMsgParts `json:"messages"`
	}
	mustJSON(t, h.MustCLI(apiKey, "read", ch, "--json", "--limit", "200"), &resp)
	var out []string
	for _, m := range resp.Messages {
		out = append(out, m.Payload.Text)
	}
	return out
}

// hubState is what must survive a move.
type hubState struct {
	users    []string // names
	channels []string
	texts    []string // #move
	grants   []string // ids
	tokens   []string // ids of unused join tokens
	nodes    []string
}

func snapshotHub(t *testing.T, h *Hub) hubState {
	t.Helper()
	var s hubState
	for _, u := range listUsers(t, h) {
		s.users = append(s.users, u.Name)
	}
	var chs struct {
		Channels []struct {
			Name string `json:"name"`
		} `json:"channels"`
	}
	mustJSON(t, h.MustCLI("", "channels", "ls", "--json"), &chs)
	for _, c := range chs.Channels {
		s.channels = append(s.channels, c.Name)
	}
	s.texts = channelTexts(t, h, "", "#move")
	for _, g := range listGrants(t, h, "", "--all") {
		s.grants = append(s.grants, g.ID+"/"+g.State)
	}
	for _, tok := range listTokens(t, h, "") {
		if tok.UsedAt == "" && tok.RevokedAt == "" {
			s.tokens = append(s.tokens, tok.ID)
		}
	}
	for name := range listNodes(t, h, "") {
		s.nodes = append(s.nodes, name)
	}
	for _, l := range [][]string{s.users, s.channels, s.grants, s.tokens, s.nodes} {
		slices.Sort(l)
	}
	return s
}

// TestDiscoveryAndRedirectFollow: a node joins with a token whose address
// is dead and finds the hub by (fake) mDNS; the hub is exported, imported
// elsewhere and redirected; the node, the CLI and an unused join token
// follow the signed redirect, and everything the hub held survives.
func TestDiscoveryAndRedirectFollow(t *testing.T) {
	t.Parallel()
	mdnsDir := t.TempDir()
	root := t.TempDir()

	// Hub A advertises into the fake mDNS directory.
	a := initHubWith(t, root, true)
	a.DaemonArgs = []string{"--allow-fake-profile"}
	a.DaemonEnv = append([]string{"MYCEL_NO_MDNS=0"}, mdnsEnv(mdnsDir)...)
	a.Start()
	var seen []jsonDiscovered
	if !waitFor(t, 10*time.Second, func() bool {
		seen = discover(t, a.Home, mdnsDir)
		return len(seen) == 1
	}) {
		t.Fatalf("hub discover: %+v, want hub A", seen)
	}
	if d := seen[0]; d.CAFingerprint != a.CAFingerprint || d.URL != a.HubURL || d.TrustDomain != "e2e" || !d.Pinned ||
		d.BusURL != "tls://127.0.0.1:"+strconv.Itoa(a.BusPort) {
		t.Fatalf("hub discover: %+v, want %s with fingerprint %s, pinned", d, a.HubURL, a.CAFingerprint)
	}

	// alpha's token is used with a dead hub address: join falls back to
	// mDNS and accepts the hub with the token's CA fingerprint.
	dead := "https://127.0.0.1:" + strconv.Itoa(freePorts(t, 1)[0])
	tok := createToken(t, a, "", "--name", "alpha", "--caps", "sandbox")
	alpha := joinAt(t, root, "alpha", tok.Token, []string{"--hub-url", dead}, append([]string{fakeOK}, mdnsEnv(mdnsDir)...))
	if st := nodeState(t, alpha); st.HubURL != a.HubURL {
		t.Fatalf("alpha joined %q, want hub A %q found by mDNS", st.HubURL, a.HubURL)
	}
	if !nodeConnected(t, alpha, 30*time.Second) {
		t.Fatalf("alpha not connected: %v", alpha.Status(t))
	}
	waitBackends(t, a, "fake", alpha)
	localFirstOff(t, a, alpha)

	// State that must survive the move.
	bob := addMember(t, a, "bob")
	a.MustCLI("", "channels", "create", "#move", "--open", "--json")
	a.MustCLI(bob, "post", "#move", "before the move", "--json")
	fakeJobOK(t, a, bob, "alpha", "before-move")
	// A grant, approved, held by a job that keeps running through the move.
	long := runJob(t, a, "", append([]string{"--profile", "fake", "--node", "alpha", "--timeout", "5m", "--"}, sleepCmd("300")...)...)
	waitState(t, a, long.ID, jobRunning, 30*time.Second)
	var gr struct {
		Grant jsonGrant `json:"grant"`
	}
	mustJSON(t, a.MustCLI("", "grants", "request", "--kind", "egress", "--allow", "example.com",
		"--subject", "job:"+long.ID, "--duration", "1h", "--reason", "move test", "--json"), &gr)
	if gr.Grant.ID == "" {
		t.Fatal("grants request: no grant id")
	}
	if gr.Grant.State == grantPending {
		a.MustCLI("", "approve", gr.Grant.ID, "--duration", "1h", "--json")
	}
	waitGrant(t, a, gr.Grant.ID, 10*time.Second, "active", func(g jsonGrant) bool { return g.State == grantActive })
	betaTok := createToken(t, a, "", "--name", "beta", "--caps", "sandbox")
	before := snapshotHub(t, a)

	// Export the running hub; import it under a new home and new ports.
	const passphrase = "correct horse battery staple e2e"
	passFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passFile, []byte(passphrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "hub.mycelhub")
	var ex struct {
		File   string `json:"file"`
		Bytes  int64  `json:"bytes"`
		Source string `json:"source"`
	}
	mustJSON(t, a.MustCLI("", "hub", "export", "--out", export, "--passphrase-file", passFile, "--json"), &ex)
	if ex.Source != "running hub" || ex.Bytes == 0 {
		t.Fatalf("hub export: %+v", ex)
	}
	checkExportFile(t, export, passphrase, a)

	bRoot := t.TempDir()
	bHome := filepath.Join(bRoot, "hub-b")
	bp := freePorts(t, 2)
	bURL := "https://127.0.0.1:" + strconv.Itoa(bp[0])
	bBus := "127.0.0.1:" + strconv.Itoa(bp[1])
	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+bHome), "hub", "import", export, "--advertise", bURL,
		"--api-listen", "127.0.0.1:"+strconv.Itoa(bp[0]), "--bus-listen", bBus, "--passphrase-file", passFile, "--json")
	if code != 0 {
		t.Fatalf("hub import: exit %d\n%s\n%s", code, out, errOut)
	}
	var imp struct {
		HubURL        string `json:"hub_url"`
		BusURL        string `json:"bus_url"`
		CAFingerprint string `json:"ca_fingerprint"`
		Nodes         int    `json:"nodes"`
		Users         int    `json:"users"`
		OldHubURL     string `json:"old_hub_url"`
	}
	mustJSON(t, out, &imp)
	if imp.HubURL != bURL || imp.CAFingerprint != a.CAFingerprint || imp.OldHubURL != a.HubURL || imp.Nodes < 2 || imp.Users < 2 {
		t.Fatalf("hub import: %+v", imp)
	}
	if strings.Contains(out+errOut, passphrase) {
		t.Error("hub import printed the passphrase")
	}
	b := &Hub{t: t, Root: bRoot, Home: bHome, HubURL: bURL, AdminKey: a.AdminKey, CAFingerprint: a.CAFingerprint,
		APIPort: bp[0]}
	b.Proc = startHubOnly(t, "hub-b", bRoot, bHome, append([]string{"MYCEL_NO_MDNS=0"}, mdnsEnv(mdnsDir)...), "--allow-fake-profile")
	// The same admin API key, checked against the same CA, works on B.
	out, errOut, code = runCLI(t, childEnv("MYCEL_HOME="+bHome), "login", bURL, "--api-key", a.AdminKey,
		"--ca-fingerprint", a.CAFingerprint, "--json")
	if code != 0 {
		t.Fatalf("login to hub B with A's admin key: exit %d\n%s\n%s", code, out, errOut)
	}
	after := snapshotHub(t, b)
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"users", after.users, before.users}, {"channels", after.channels, before.channels},
		{"#move", after.texts, before.texts}, {"grants", after.grants, before.grants},
		{"unused join tokens", after.tokens, before.tokens}, {"nodes", after.nodes, before.nodes},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s on hub B: %q, want %q as on hub A", c.what, c.got, c.want)
		}
	}
	if !slices.Contains(after.tokens, betaTok.JoinToken.ID) || !slices.Contains(after.texts, "before the move") {
		t.Fatalf("hub B lacks beta's join token or the message: %+v", after)
	}
	if texts := channelTexts(t, b, bob, "#move"); !slices.Contains(texts, "before the move") {
		t.Fatalf("bob's API key on hub B reads %q", texts)
	}

	// Redirect A to B: alpha follows within 10 s.
	var red struct {
		HubURL   string `json:"hub_url"`
		BusURL   string `json:"bus_url"`
		IssuedAt int64  `json:"issued_at"`
		Nodes    int    `json:"nodes"`
	}
	mustJSON(t, a.MustCLI("", "hub", "redirect", bURL, "--bus", bBus, "--json"), &red)
	moved := time.Now()
	if red.HubURL != bURL || red.Nodes < 1 {
		t.Fatalf("hub redirect: %+v", red)
	}
	var ns daemon.State
	if !waitFor(t, 10*time.Second, func() bool {
		ns = nodeState(t, alpha)
		return ns.HubURL == bURL
	}) {
		t.Fatalf("alpha did not follow the redirect within 10 s: state %+v", ns)
	}
	t.Logf("alpha followed the redirect in %s", time.Since(moved).Round(10*time.Millisecond))
	if ns.BusURL != "tls://"+bBus || ns.RedirectIssuedAt != red.IssuedAt {
		t.Errorf("alpha's state after the redirect: %+v, want bus tls://%s issued_at %d", ns, bBus, red.IssuedAt)
	}
	// alpha's node certificate works on B: online there, its running job
	// can be cancelled from B (on a small machine it holds alpha's only
	// slot), and it runs a new job.
	if !waitFor(t, 30*time.Second, func() bool { return listNodes(t, b, "")["alpha"].Online }) {
		t.Fatalf("hub B does not list alpha online: %+v", listNodes(t, b, "")["alpha"])
	}
	b.MustCLI("", "job", "cancel", long.ID)
	waitState(t, b, long.ID, jobCancelled, 30*time.Second)
	fakeJobOK(t, b, bob, "alpha", "after-move")

	// A refuses a post with FailedPrecondition and the signed redirect; a
	// redirect with one bit of its signature flipped does not verify.
	hc, caPEM := pinnedHubClient(t, a.Home)
	status, ccode, hdr := connectPost(t, hc, a.HubURL, bob, "#move", "to the old hub")
	if ccode != "failed_precondition" || hdr == "" {
		t.Fatalf("post to hub A: HTTP %d code %q redirect header %t, want failed_precondition with the redirect", status, ccode, hdr != "")
	}
	rd, err := daemon.DecodeRedirect(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.VerifyRedirect(caPEM, rd, time.Now()); err != nil || rd.GetHubUrl() != bURL {
		t.Fatalf("A's redirect: %v (to %q)", err, rd.GetHubUrl())
	}
	rd.Signature[len(rd.Signature)/2] ^= 0x01
	if err := daemon.VerifyRedirect(caPEM, rd, time.Now()); err == nil {
		t.Fatal("a redirect with a flipped signature bit verified")
	}

	// The CLI: "hub moved" without following, config.yaml updated with
	// --follow-redirect, then the command works against B.
	cliHome := filepath.Join(root, "bob-cli")
	if out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+cliHome), "login", a.HubURL, "--api-key", bob,
		"--ca-fingerprint", a.CAFingerprint, "--json"); code == 0 {
		t.Fatalf("login to the moved hub A succeeded: %s", out)
	} else if !strings.Contains(errOut, "hub moved") {
		t.Errorf("login to the moved hub A: stderr lacks \"hub moved\": %s", errOut)
	}
	bobCLI := func(args ...string) (string, string, int) {
		return runCLI(t, childEnv("MYCEL_HOME="+a.Home, "MYCEL_API_KEY="+bob), args...)
	}
	_, errOut, code = bobCLI("post", "#move", "after the move")
	if code == 0 || !strings.Contains(errOut, "hub moved to "+bURL) || !strings.Contains(errOut, "signed by the pinned hub CA") {
		t.Fatalf("post via the CLI to hub A: exit %d\n%s", code, errOut)
	}
	if got := configHubURL(t, a.Home); got != a.HubURL {
		t.Fatalf("config.yaml changed without --follow-redirect: %q", got)
	}
	_, errOut, code = bobCLI("post", "#move", "after the move", "--follow-redirect")
	if code == 0 || !strings.Contains(errOut, "now points at "+bURL) {
		t.Fatalf("--follow-redirect: exit %d\n%s", code, errOut)
	}
	if got := configHubURL(t, a.Home); got != bURL {
		t.Fatalf("config.yaml hub_url = %q after --follow-redirect, want %q", got, bURL)
	}
	if out, errOut, code := bobCLI("post", "#move", "after the move", "--json"); code != 0 {
		t.Fatalf("post after following: exit %d\n%s\n%s", code, out, errOut)
	}
	if texts := channelTexts(t, b, "", "#move"); !slices.Contains(texts, "after the move") {
		t.Fatalf("hub B's #move: %q", texts)
	}

	// A join token minted on A joins B through A's redirect; discovery now
	// shows only B (a moved hub stops advertising).
	beta := joinAt(t, root, "beta", betaTok.Token, nil, mdnsEnv(mdnsDir))
	if st := nodeState(t, beta); st.HubURL != bURL {
		t.Fatalf("beta joined %q, want hub B %q", st.HubURL, bURL)
	}
	if !nodeConnected(t, beta, 30*time.Second) {
		t.Fatalf("beta not connected to hub B: %v", beta.Status(t))
	}
	if seen := discover(t, a.Home, mdnsDir); len(seen) != 1 || seen[0].URL != bURL || seen[0].CAFingerprint != a.CAFingerprint {
		t.Fatalf("hub discover after the move: %+v, want only hub B", seen)
	}
}

// checkExportFile: the export holds neither the passphrase nor the CA key
// in the clear, and a wrong passphrase cannot open it.
func checkExportFile(t *testing.T, path, passphrase string, h *Hub) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("MYCELHUB2")) {
		t.Fatalf("export does not start with the MYCELHUB2 magic")
	}
	caKey, err := os.ReadFile(filepath.Join(h.Home, "data", "hub", "ca.key")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	for what, secret := range map[string][]byte{
		"the passphrase": []byte(passphrase), "the CA key": caKey, "the admin API key": []byte(h.AdminKey),
		"the CA key PEM header": []byte("PRIVATE KEY"),
	} {
		if bytes.Contains(raw, secret) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(secret))) {
			t.Errorf("the export contains %s in the clear", what)
		}
	}
	wrong := filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(wrong, []byte("not the passphrase at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "c")
	_, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+home), "hub", "import", path, "--advertise", "https://127.0.0.1:1",
		"--passphrase-file", wrong, "--json")
	if code == 0 {
		t.Fatal("hub import with a wrong passphrase succeeded")
	}
	if strings.Contains(errOut, passphrase) {
		t.Error("the failed import printed the passphrase")
	}
	if _, err := os.Stat(filepath.Join(home, "data", "hub", "ca.key")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed import left a hub behind: %v", err)
	}
}

// TestDiscoveryRealMulticast is the real-network variant: a hub
// advertises by mDNS and `mycel hub discover` finds it. Linux CI only.
func TestDiscoveryRealMulticast(t *testing.T) {
	if os.Getenv("MYCEL_TEST_MDNS") != "1" || runtime.GOOS != "linux" {
		t.Skip("real mDNS advertising runs only on Linux CI with MYCEL_TEST_MDNS=1 (on a workstation it would announce a hub on the network); the fake-mDNS variant is TestDiscoveryAndRedirectFollow")
	}
	t.Parallel()
	root := t.TempDir()
	h := initHubWith(t, root, true)
	h.DaemonEnv = []string{"MYCEL_NO_MDNS=0", "MYCEL_TEST_MDNS=1"}
	h.Start()
	env := childEnv("MYCEL_HOME="+h.Home, "MYCEL_TEST_MDNS=1")
	// On CI (the step that sets MYCEL_TEST_MDNS=1) a missing answer is a
	// failure; a local opt-in run on a network without multicast skips.
	skipOrFail := t.Skipf
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		skipOrFail = t.Fatalf
	}
	var found bool
	waitFor(t, 20*time.Second, func() bool {
		out, errOut, code := runCLI(t, env, "hub", "discover", "--timeout", "3s", "--json")
		if code != 0 {
			if strings.Contains(errOut, "browse") {
				skipOrFail("multicast refused by this environment: %s", errOut)
			}
			return false
		}
		var hubs []jsonDiscovered
		mustJSON(t, out, &hubs)
		for _, d := range hubs {
			if d.CAFingerprint == h.CAFingerprint && d.URL == h.HubURL {
				found = true
			}
		}
		return found
	})
	if !found {
		skipOrFail("the hub's mDNS advertisement was not seen within 20 s (multicast blocked here?)")
	}
}

// ---- install, service, quickstart, demo ----

// TestServiceStatusFresh: `mycel service status` on a fresh home and user
// directory reports the service as not installed.
func TestServiceStatusFresh(t *testing.T) {
	// It queries the real service manager (launchctl print, schtasks
	// /Query): on a workstation only Linux, everywhere on CI runners.
	if runtime.GOOS != "linux" && os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("queries the user's real launchd / Task Scheduler; runs on Linux, and on macOS and Windows only in CI (GITHUB_ACTIONS=true)")
	}
	t.Parallel()
	home := t.TempDir()
	env := childEnv("MYCEL_HOME="+filepath.Join(home, "mycel"), "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	out, errOut, code := runCLI(t, env, "service", "status", "--json")
	if code != 0 {
		t.Fatalf("service status: exit %d\n%s\n%s", code, out, errOut)
	}
	var st struct {
		Installed bool `json:"installed"`
		Running   bool `json:"running"`
	}
	mustJSON(t, out, &st)
	if st.Installed {
		t.Fatalf("service status on a fresh home: %s", out)
	}
	if runtime.GOOS != "windows" {
		if txt, _, _ := runCLI(t, env, "service", "status"); !regexp.MustCompile(`(?m)^installed:\s+no$`).MatchString(txt) {
			t.Errorf("service status (text): %q, want \"installed: no\"", txt)
		}
	}
}

// quickstartCommands returns every `mycel …` command line in the code
// blocks and inline code of docs/quickstart.md.
func quickstartCommands(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "quickstart.md"))
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if i := strings.Index(s, " #"); i >= 0 && !strings.Contains(s[:i], "'") { // a shell comment
			s = strings.TrimSpace(s[:i])
		}
		if strings.HasPrefix(s, "mycel ") && !strings.HasPrefix(s, "mycel ...") && !strings.HasPrefix(s, "mycel …") {
			cmds = append(cmds, s)
		}
	}
	inBlock := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	inline := regexp.MustCompile("`([^`]+)`")
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inBlock = !inBlock
			continue
		}
		if inBlock {
			add(line)
			continue
		}
		for _, m := range inline.FindAllStringSubmatch(line, -1) {
			add(m[1])
		}
	}
	if len(cmds) < 15 {
		t.Fatalf("found only %d mycel commands in the quickstart: %q", len(cmds), cmds)
	}
	return cmds
}

var wordRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// TestQuickstartCommandsExist: every `mycel` command in docs/quickstart.md
// is a real (leaf) command, and every flag it uses is in its help.
func TestQuickstartCommandsExist(t *testing.T) {
	t.Parallel()
	env := childEnv("MYCEL_HOME=" + t.TempDir())
	cmds := quickstartCommands(t)
	t.Logf("checking %d mycel commands from docs/quickstart.md", len(cmds))
	for _, line := range cmds {
		fields := strings.Fields(line)[1:]
		var words, flags []string
		for i, f := range fields {
			if f == "|" || f == "…" || f == "&&" || f == "--" { // "--": the job's own command follows
				break
			}
			if strings.HasPrefix(f, "--") || (strings.HasPrefix(f, "-") && len(f) == 2) {
				flags = append(flags, strings.SplitN(f, "=", 2)[0])
				continue
			}
			if len(flags) == 0 && wordRE.MatchString(f) && i == len(words) {
				words = append(words, f)
			}
		}
		out, errOut, code := runCLI(t, env, append(append([]string{}, words...), "--help")...)
		if code != 0 {
			t.Errorf("%q: mycel %s --help: exit %d\n%s", line, strings.Join(words, " "), code, errOut)
			continue
		}
		usage := usageLine(out)
		path := strings.Fields(usage)
		var cmdPath []string
		for _, w := range path[1:] {
			if !wordRE.MatchString(w) {
				break
			}
			cmdPath = append(cmdPath, w)
		}
		// A group without its own action shows only "mycel <group> [command]".
		if len(cmdPath) == 0 || len(cmdPath) > len(words) || !slices.Equal(cmdPath, words[:len(cmdPath)]) ||
			strings.HasSuffix(usage, "[command]") {
			t.Errorf("%q: not a command (help usage %q)", line, usage)
			continue
		}
		// Words after the command path must be documented positionals
		// ("<name>" in the usage), not a mistyped subcommand.
		placeholders := 0
		for _, w := range path[1+len(cmdPath):] {
			if strings.HasPrefix(w, "<") || strings.HasPrefix(w, "[<") {
				placeholders++
			}
		}
		if extra := len(words) - len(cmdPath); extra > placeholders {
			t.Errorf("%q: %q is not an argument of mycel %s (help usage %q)", line, words[len(cmdPath):],
				strings.Join(cmdPath, " "), usage)
			continue
		}
		for _, f := range flags {
			if f == "-h" || f == "--help" {
				continue
			}
			if !regexp.MustCompile(`(^|[\s,])` + regexp.QuoteMeta(f) + `([\s=,]|$)`).MatchString(out) {
				t.Errorf("%q: mycel %s has no flag %s", line, strings.Join(cmdPath, " "), f)
			}
		}
	}
}

// usageLine returns the first line after "Usage:" in cobra help output.
func usageLine(help string) string {
	lines := strings.Split(help, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "Usage:" && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

// consecutivePorts finds a free port p with p+1 free too (demo.sh takes
// --port N and uses N+1 for the bus).
func consecutivePorts(t *testing.T) int {
	t.Helper()
	for range 50 {
		p := freePorts(t, 1)[0]
		if p >= 65535 {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p+1))
		if err != nil {
			continue
		}
		_ = l.Close()
		return p
	}
	t.Fatal("no two consecutive free ports")
	return 0
}

// TestDemoScript runs scripts/demo.sh end to end with the test binary
// (fake sandbox backend) and checks every step, within 5 minutes.
func TestDemoScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("demo.sh is a POSIX shell script")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	// Not parallel: the demo measures a user's wait, and its daemons give
	// their bus 10 s to start, which a machine busy with the parallel
	// tests (and the race detector) can miss.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	port := consecutivePorts(t)
	cmd := exec.CommandContext(ctx, "sh", filepath.Join("..", "..", "scripts", "demo.sh"), "--port", strconv.Itoa(port)) //nolint:gosec // the repository's script
	cmd.Env = childEnv("MYCEL_BIN="+binPath, "MYCEL_NO_MDNS=1")
	// On timeout, SIGTERM: the script's trap stops its hub and node and
	// removes its directory (a SIGKILL would orphan both daemons).
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start)
	out := buf.String()
	if ctx.Err() != nil {
		t.Fatalf("demo.sh did not finish within 5 minutes\n%s", out)
	}
	if err != nil {
		t.Fatalf("demo.sh: %v after %s\n%s", err, took.Round(time.Second), out)
	}
	t.Logf("demo.sh finished in %s", took.Round(100*time.Millisecond))
	for _, want := range []string{
		"sandbox: the fake backend",
		"hub and its node are up",
		"alice and bob exist",
		"hello from alice", "hi alice, bob here",
		"JOB_STATE_SUCCEEDED", "hello from the sandbox",
		"GRANT_STATE_PENDING", "Approved grt_", ": active until",
		"hub admin UI:   https://127.0.0.1:" + strconv.Itoa(port) + "/",
		"== done",
		"demo: stopped the hub and the node",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("demo output lacks %q", want)
		}
	}
	if strings.Contains(out, "the grant request was refused") {
		t.Errorf("the demo's grant request was refused")
	}
	if t.Failed() {
		t.Logf("demo output:\n%s", out)
	}
}
