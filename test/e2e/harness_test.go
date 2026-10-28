//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fh "github.com/richard-sen27/mycelium/test/fakeharness"
)

const (
	readyTimeout = 30 * time.Second
	cliTimeout   = 60 * time.Second
	pollStep     = 100 * time.Millisecond
)

// binPath is the mycel binary built once by TestMain.
var binPath string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// The test binary doubles as a job command (see jobHelper).
	if mode := os.Getenv(envJobHelper); mode != "" {
		return jobHelper(mode)
	}
	dir, err := os.MkdirTemp("", "mycel-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: temp dir:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	name := "mycel"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath = filepath.Join(dir, name)
	args := []string{"build", "-tags", "fakesandbox,fakeharness"}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "-o", binPath, "../../cmd/mycel")
	build := exec.Command("go", args...) //nolint:gosec // test-built binary path
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: building mycel:", err)
		return 1
	}
	return m.Run()
}

// freePorts asks the OS for n distinct free TCP ports on 127.0.0.1. Every
// listener stays open until all n ports are chosen, then all are released.
// There is a small race: another process may grab a port before the daemon
// binds it; startHub retries then. Only the hub's API and bus ports need
// this (a restarted hub must keep its advertised URL); local API addresses
// are bound to port 0 and read back from the daemon's ready line.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	ls := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("free port: %v", err)
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports
}

// childEnv is the test process's environment without any MYCEL_* variables,
// with GORACE=halt_on_error=1, plus the given overrides (fh.ChildEnv).
func childEnv(extra ...string) []string {
	return fh.ChildEnv(extra...)
}

// checkRace fails the test when a child's stderr shows a data race or the
// child exited with the race detector's exit code.
func checkRace(t *testing.T, what, stderr string, exit int) {
	t.Helper()
	if strings.Contains(stderr, fh.RaceDetected) {
		t.Errorf("%s: data race reported:\n%s", what, stderr)
	} else if exit == fh.RaceExitCode {
		t.Errorf("%s: exit code %d (race detector)\n%s", what, exit, stderr)
	}
}

// runCLI runs the binary to completion and returns its output and exit code.
func runCLI(t *testing.T, env []string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...) //nolint:gosec // the binary under test
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	exit = 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("mycel %s: %v", strings.Join(args, " "), err)
		}
		exit = ee.ExitCode()
	}
	if ctx.Err() != nil {
		t.Fatalf("mycel %s: timed out after %s\nstderr: %s", strings.Join(args, " "), cliTimeout, errb.String())
	}
	checkRace(t, "mycel "+strings.Join(args, " "), errb.String(), exit)
	return out.String(), errb.String(), exit
}

// mustJSON decodes stdout (one JSON document) into v, failing the test on error.
func mustJSON(t *testing.T, stdout string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		t.Fatalf("decoding JSON output: %v\noutput: %q", err, stdout)
	}
}

// waitFor polls cond every 100 ms until it returns true or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollStep)
	}
}

// proc is a background mycel process (a daemon).
type proc struct {
	name       string
	cmd        *exec.Cmd
	stdoutPath string
	stderrPath string
	done       chan struct{}

	mu       sync.Mutex
	exitCode int
}

func startProc(t *testing.T, name, logDir string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{
		name:       name,
		stdoutPath: filepath.Join(logDir, name+".stdout.log"),
		stderrPath: filepath.Join(logDir, name+".stderr.log"),
		done:       make(chan struct{}),
		exitCode:   -1,
	}
	stdout, err := os.OpenFile(p.stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(p.stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Separator so restarts in the same log file are visible.
	_, _ = fmt.Fprintf(stderr, "---- e2e: starting %s %s\n", name, strings.Join(args, " "))
	p.cmd = exec.Command(binPath, args...) //nolint:gosec // the binary under test
	p.cmd.Env = env
	p.cmd.Stdout = stdout
	p.cmd.Stderr = stderr
	if err := p.cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		t.Fatalf("starting %s: %v", name, err)
	}
	go func() {
		err := p.cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		_ = stdout.Close()
		_ = stderr.Close()
		p.mu.Lock()
		p.exitCode = code
		p.mu.Unlock()
		close(p.done)
	}()

	t.Cleanup(func() {
		p.stop()
		code, _ := p.Wait(0)
		checkRace(t, name, readFile(p.stderrPath), code)
		if t.Failed() {
			t.Logf("==== %s stdout ====\n%s", name, readFile(p.stdoutPath))
			t.Logf("==== %s stderr ====\n%s", name, readFile(p.stderrPath))
		}
	})
	return p
}

// stop ends the process: Interrupt then Kill after 3 s on unix, Kill on Windows.
func (p *proc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	if runtime.GOOS != "windows" {
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
			return
		case <-time.After(3 * time.Second):
		}
	}
	p.kill()
}

// kill kills the process and waits for it to be reaped.
func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.done
}

// Wait waits up to timeout for the process to exit and returns its exit code.
func (p *proc) Wait(timeout time.Duration) (exitCode int, ok bool) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exitCode, true
	case <-time.After(timeout):
		return 0, false
	}
}

// waitReady waits for the first complete JSON line on stdout written after
// offset and decodes it into v.
func (p *proc) waitReady(t *testing.T, offset int64, v any) {
	t.Helper()
	if err := p.waitReadyErr(offset, v); err != nil {
		t.Fatal(err)
	}
}

// errAddrInUse marks a daemon that exited before it was ready because a
// port it was given was taken meanwhile (see freePorts).
var errAddrInUse = errors.New("address already in use")

func (p *proc) waitReadyErr(offset int64, v any) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		if line, ok := firstLine(p.stdoutPath, offset); ok {
			if err := json.Unmarshal([]byte(line), v); err != nil {
				return fmt.Errorf("%s: ready line is not JSON: %w: %q", p.name, err, line)
			}
			return nil
		}
		select {
		case <-p.done:
			code, _ := p.Wait(0)
			stderr := readFile(p.stderrPath)
			if strings.Contains(stderr, errAddrInUse.Error()) {
				return fmt.Errorf("%s exited with code %d before it was ready: %w\nstderr:\n%s", p.name, code, errAddrInUse, stderr)
			}
			return fmt.Errorf("%s exited with code %d before it was ready\nstderr:\n%s", p.name, code, stderr)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s\nstderr:\n%s", p.name, readyTimeout, readFile(p.stderrPath))
		}
		time.Sleep(pollStep)
	}
}

func firstLine(path string, offset int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, 0); err != nil {
		return "", false
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil {
		return "", false // no complete line yet
	}
	return strings.TrimSpace(line), true
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return string(b)
}

// Hub is a running hub daemon (which is also the node "hub-node").
type Hub struct {
	t             *testing.T
	Root          string // per-test temp root; node homes live next to the hub home
	Home          string
	HubURL        string
	AdminKey      string
	AdminUserID   string
	CAFingerprint string
	APIPort       int
	BusPort       int
	LocalAddr     string // host:port of the hub daemon's local API (bound to port 0)
	Proc          *proc
	// DaemonArgs and DaemonEnv are added to every start of the hub daemon
	// (hubOptions), e.g. --allow-fake-profile and MYCEL_FAKE_SANDBOX_OK=1.
	DaemonArgs []string
	DaemonEnv  []string
}

type hubInitOut struct {
	TrustDomain   string `json:"trust_domain"`
	HubURL        string `json:"hub_url"`
	CAFingerprint string `json:"ca_fingerprint"`
	AdminUserID   string `json:"admin_user_id"`
	AdminAPIKey   string `json:"admin_api_key"`
	LocalNodeName string `json:"local_node_name"`
}

type daemonReady struct {
	Hub *struct {
		APIAddr string `json:"api_addr"`
		BusAddr string `json:"bus_addr"`
	} `json:"hub"`
	Node *struct {
		LocalAddr string `json:"local_addr"`
	} `json:"node"`
}

// startHubAttempts bounds the retries of startHub when a chosen port was
// taken between freePorts and the daemon binding it.
const startHubAttempts = 3

func startHub(t *testing.T) *Hub {
	t.Helper()
	return startHubWith(t, hubOptions{})
}

// hubOptions changes how startHubWith sets up the hub.
type hubOptions struct {
	Args []string // extra daemon arguments
	Env  []string // extra daemon environment
	// Root returns a fresh root directory for the hub (and its nodes);
	// nil means t.TempDir().
	Root func(t *testing.T) string
	// HubYAML is appended to hub.yaml after `hub init`, e.g. a grants
	// section with auto rules and pending_ttl.
	HubYAML string
}

// startHubWith is startHub with extra daemon arguments, environment or root.
func startHubWith(t *testing.T, o hubOptions) *Hub {
	t.Helper()
	for attempt := 1; ; attempt++ {
		root := ""
		if o.Root != nil {
			root = o.Root(t)
		}
		h := initHubAt(t, root)
		h.DaemonArgs, h.DaemonEnv = o.Args, o.Env
		if o.HubYAML != "" {
			appendHubYAML(t, h, o.HubYAML)
		}
		err := h.start()
		if err == nil {
			return h
		}
		if !errors.Is(err, errAddrInUse) || attempt == startHubAttempts {
			t.Fatal(err)
		}
		t.Logf("hub start attempt %d: a port was taken; retrying with new ports", attempt)
	}
}

// initHubAt runs `hub init` in root ("" for a fresh t.TempDir()) with fresh
// ports.
func initHubAt(t *testing.T, root string) *Hub {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	ports := freePorts(t, 2)
	h := &Hub{
		t:       t,
		Root:    root,
		Home:    filepath.Join(root, "hub"),
		APIPort: ports[0],
		BusPort: ports[1],
	}
	api := "127.0.0.1:" + strconv.Itoa(h.APIPort)
	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+h.Home),
		"hub", "init",
		"--advertise", "https://"+api,
		"--api-listen", api,
		"--bus-listen", "127.0.0.1:"+strconv.Itoa(h.BusPort),
		"--trust-domain", "e2e",
		"--node-name", "hub-node",
		"--no-mdns", // tests never advertise on the network
		"--json")
	if code != 0 {
		t.Fatalf("hub init: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	var init hubInitOut
	mustJSON(t, out, &init)
	if init.AdminAPIKey == "" || init.CAFingerprint == "" || init.HubURL == "" {
		t.Fatalf("hub init: incomplete output: %s", out)
	}
	h.HubURL = init.HubURL
	h.AdminKey = init.AdminAPIKey
	h.AdminUserID = init.AdminUserID
	h.CAFingerprint = init.CAFingerprint
	return h
}

// Start starts (or restarts) the hub daemon in the hub home with the same
// API and bus ports.
func (h *Hub) Start() {
	h.t.Helper()
	if err := h.start(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *Hub) start() error {
	offset := fileSize(filepath.Join(h.Root, "hub.stdout.log"))
	args := append([]string{"daemon", "--json", "--local-addr", "127.0.0.1:0"}, h.DaemonArgs...)
	h.Proc = startProc(h.t, "hub", h.Root, append(childEnv("MYCEL_HOME="+h.Home), h.DaemonEnv...), args...)
	var ready daemonReady
	if err := h.Proc.waitReadyErr(offset, &ready); err != nil {
		return err
	}
	if ready.Hub == nil || ready.Node == nil || ready.Node.LocalAddr == "" {
		return fmt.Errorf("hub daemon ready line lacks hub, node or node.local_addr: %+v", ready)
	}
	h.LocalAddr = ready.Node.LocalAddr
	return nil
}

// CLI runs mycel as the admin (MYCEL_HOME = hub home, whose config.yaml holds the admin key).
func (h *Hub) CLI(args ...string) (stdout, stderr string, exit int) {
	h.t.Helper()
	return runCLI(h.t, childEnv("MYCEL_HOME="+h.Home), args...)
}

// CLIAs runs mycel against the hub with another API key.
func (h *Hub) CLIAs(apiKey string, args ...string) (stdout, stderr string, exit int) {
	h.t.Helper()
	return runCLI(h.t, childEnv("MYCEL_HOME="+h.Home, "MYCEL_API_KEY="+apiKey), args...)
}

// MustCLI runs mycel as admin (or with apiKey if non-empty) and fails on a non-zero exit.
func (h *Hub) MustCLI(apiKey string, args ...string) string {
	h.t.Helper()
	var out, errOut string
	var code int
	if apiKey == "" {
		out, errOut, code = h.CLI(args...)
	} else {
		out, errOut, code = h.CLIAs(apiKey, args...)
	}
	if code != 0 {
		h.t.Fatalf("mycel %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// Node is a joined node running its own daemon.
type Node struct {
	t         *testing.T
	Home      string
	Name      string
	LocalAddr string // host:port of the daemon's local API
	LocalURL  string
	Token     string
	Proc      *proc
}

type tokenCreateOut struct {
	Token     string    `json:"token"`
	JoinToken jsonToken `json:"join_token"`
}

type jsonToken struct {
	ID        string `json:"id"`
	NodeName  string `json:"node_name"`
	OwnerID   string `json:"owner_id"`
	ExpiresAt string `json:"expires_at"`
	UsedAt    string `json:"used_at"`
	RevokedAt string `json:"revoked_at"`
}

// createToken creates a join token as admin (apiKey "") or with apiKey.
func createToken(t *testing.T, h *Hub, apiKey string, args ...string) tokenCreateOut {
	t.Helper()
	out := h.MustCLI(apiKey, append([]string{"token", "create", "--json"}, args...)...)
	var tok tokenCreateOut
	mustJSON(t, out, &tok)
	if !strings.HasPrefix(tok.Token, "mcl1_") {
		t.Fatalf("token create: unexpected token %q", tok.Token)
	}
	return tok
}

// joinNode creates a token for name (as admin, or with apiKey), joins a fresh
// home with it and starts the node's daemon. extraEnv is added to the
// daemon's environment (and so to the workers it spawns).
func joinNode(t *testing.T, h *Hub, name, caps, apiKey string, extraEnv ...string) *Node {
	t.Helper()
	args := []string{"--name", name}
	if caps != "" {
		args = append(args, "--caps", caps)
	}
	tok := createToken(t, h, apiKey, args...)
	return joinWithToken(t, h, name, tok.Token, extraEnv...)
}

func joinWithToken(t *testing.T, h *Hub, name, token string, extraEnv ...string) *Node {
	t.Helper()
	n := &Node{t: t, Home: filepath.Join(h.Root, name), Name: name, Token: token}
	env := childEnv("MYCEL_HOME=" + n.Home)
	out, errOut, code := runCLI(t, env, "join", token, "--json")
	if code != 0 {
		t.Fatalf("join %s: exit %d\nstdout: %s\nstderr: %s", name, code, out, errOut)
	}
	// Workers write their stderr under the node home; a race there is the
	// worker's (mycel fakeharness) own.
	t.Cleanup(func() { checkWorkerLogs(t, n.Home) })
	n.Proc = startProc(t, name, h.Root, append(env, extraEnv...), "daemon", "--json", "--local-addr", "127.0.0.1:0")
	var ready daemonReady
	n.Proc.waitReady(t, 0, &ready)
	if ready.Node == nil || ready.Node.LocalAddr == "" {
		t.Fatalf("%s: ready line lacks node.local_addr: %+v", name, ready)
	}
	n.LocalAddr = ready.Node.LocalAddr
	n.LocalURL = "http://" + ready.Node.LocalAddr
	return n
}

// workerLogs returns the stderr logs of the workers a node daemon started,
// by worker name.
func workerLogs(home string) map[string]string {
	logs := map[string]string{}
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best effort: skip unreadable entries
		}
		if !d.IsDir() && d.Name() == "stderr.log" && filepath.Base(filepath.Dir(filepath.Dir(p))) == "workers" {
			logs[filepath.Base(filepath.Dir(p))] = readFile(p)
		}
		return nil
	})
	return logs
}

// checkWorkerLogs fails the test on a data race in a worker's stderr and
// logs every worker's stderr when the test failed.
func checkWorkerLogs(t *testing.T, home string) {
	t.Helper()
	for name, log := range workerLogs(home) {
		checkRace(t, "worker "+name, log, 0)
		if t.Failed() {
			t.Logf("==== worker %s stderr ====\n%s", name, log)
		}
	}
}

// Status fetches the node's local /api/status. It returns nil if the endpoint
// is not reachable (callers poll).
func (n *Node) Status(t *testing.T) map[string]any {
	t.Helper()
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(n.LocalURL + "/api/status")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Errorf("%s: decoding /api/status: %v", n.Name, err)
		return nil
	}
	return m
}

// Wait waits for the node's daemon to exit.
func (n *Node) Wait(timeout time.Duration) (exitCode int, ok bool) {
	return n.Proc.Wait(timeout)
}

// ---- hub view helpers ----

type jsonNode struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OwnerID     string    `json:"owner_id"`
	Online      bool      `json:"online"`
	CapsCeiling jsonCaps  `json:"caps_ceiling"`
	Inventory   jsonInvty `json:"inventory"`
}

type jsonCaps struct {
	Names []string `json:"names"`
}

type jsonInvty struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	CPUCount int64  `json:"cpu_count"`
	// protojson encodes uint64 as a string; accept either form.
	MemBytes json.Number `json:"mem_bytes"`
}

func (i jsonInvty) memBytes() int64 {
	s := strings.Trim(i.MemBytes.String(), `"`)
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// UnmarshalJSON accepts mem_bytes as number or string.
func (i *jsonInvty) UnmarshalJSON(b []byte) error {
	var raw struct {
		OS       string          `json:"os"`
		Arch     string          `json:"arch"`
		Hostname string          `json:"hostname"`
		CPUCount int64           `json:"cpu_count"`
		MemBytes json.RawMessage `json:"mem_bytes"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	i.OS, i.Arch, i.Hostname, i.CPUCount = raw.OS, raw.Arch, raw.Hostname, raw.CPUCount
	i.MemBytes = json.Number(strings.Trim(string(raw.MemBytes), `"`))
	return nil
}

// listNodes returns the hub's nodes by name, as admin (apiKey "") or with apiKey.
func listNodes(t *testing.T, h *Hub, apiKey string) map[string]jsonNode {
	t.Helper()
	out := h.MustCLI(apiKey, "nodes", "ls", "--json")
	var resp struct {
		Nodes []jsonNode `json:"nodes"`
	}
	mustJSON(t, out, &resp)
	m := make(map[string]jsonNode, len(resp.Nodes))
	for _, n := range resp.Nodes {
		m[n.Name] = n
	}
	return m
}

func listTokens(t *testing.T, h *Hub, apiKey string) []jsonToken {
	t.Helper()
	out := h.MustCLI(apiKey, "token", "ls", "--json")
	var resp struct {
		Tokens []jsonToken `json:"tokens"`
	}
	mustJSON(t, out, &resp)
	return resp.Tokens
}

// appendHubYAML appends text to the hub's hub.yaml (written by `hub init`).
func appendHubYAML(t *testing.T, h *Hub, text string) {
	t.Helper()
	path := filepath.Join(h.Home, "data", "hub", "hub.yaml")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatalf("hub.yaml: %v", err)
	}
	if _, err := f.WriteString("\n" + text + "\n"); err != nil {
		_ = f.Close()
		t.Fatalf("hub.yaml: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("hub.yaml: %v", err)
	}
}

// ---- the node's local API ----

// localToken reads the node daemon's local API token (written at every
// daemon start). It is never logged.
func (n *Node) localToken(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(n.Home, "data", "node", "local.token")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatalf("%s: local token: %v", n.Name, err)
	}
	return strings.TrimSpace(string(b))
}

// Local sends one request to the node's local API with its local token and
// returns the status code and body; code 0 means the daemon did not answer.
func (n *Node) Local(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, n.LocalURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.localToken(t))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, b
}

// LocalJSON GETs a local API route and decodes a 200 answer into v; it
// reports whether it did.
func (n *Node) LocalJSON(t *testing.T, path string, v any) bool {
	t.Helper()
	code, b := n.Local(t, http.MethodGet, path, "")
	if code != http.StatusOK {
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Errorf("%s: GET %s: %v: %q", n.Name, path, err, b)
		return false
	}
	return true
}

// waitLocal polls GET path on the node's local API every 100 ms until cond
// holds for a decoded 200 answer or timeout passes; it returns the last
// decoded answer and whether cond held.
func waitLocal[T any](t *testing.T, n *Node, path string, timeout time.Duration, cond func(T) bool) (T, bool) {
	t.Helper()
	var last T
	ok := waitFor(t, timeout, func() bool {
		var v T
		if !n.LocalJSON(t, path, &v) {
			return false
		}
		last = v
		return cond(v)
	})
	return last, ok
}
