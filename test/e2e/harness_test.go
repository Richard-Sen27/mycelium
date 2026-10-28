//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	args := []string{"build", "-tags", "fakesandbox"}
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
// binds it. On a test runner this is rare enough to accept; the alternative
// (passing :0 and reading back bound addresses) would not let a restarted
// hub keep the advertised URL.
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
// plus the given overrides.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "MYCEL_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
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
	deadline := time.Now().Add(readyTimeout)
	for {
		if line, ok := firstLine(p.stdoutPath, offset); ok {
			if err := json.Unmarshal([]byte(line), v); err != nil {
				t.Fatalf("%s: ready line is not JSON: %v: %q", p.name, err, line)
			}
			return
		}
		select {
		case <-p.done:
			code, _ := p.Wait(0)
			t.Fatalf("%s exited with code %d before it was ready\nstderr:\n%s", p.name, code, readFile(p.stderrPath))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready after %s\nstderr:\n%s", p.name, readyTimeout, readFile(p.stderrPath))
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
	LocalPort     int
	Proc          *proc
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

func startHub(t *testing.T) *Hub {
	t.Helper()
	root := t.TempDir()
	ports := freePorts(t, 3)
	h := &Hub{
		t:         t,
		Root:      root,
		Home:      filepath.Join(root, "hub"),
		APIPort:   ports[0],
		BusPort:   ports[1],
		LocalPort: ports[2],
	}
	api := "127.0.0.1:" + strconv.Itoa(h.APIPort)
	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+h.Home),
		"hub", "init",
		"--advertise", "https://"+api,
		"--api-listen", api,
		"--bus-listen", "127.0.0.1:"+strconv.Itoa(h.BusPort),
		"--trust-domain", "e2e",
		"--node-name", "hub-node",
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
	h.Start()
	return h
}

// Start starts (or restarts) the hub daemon in the hub home with the same ports.
func (h *Hub) Start() {
	t := h.t
	t.Helper()
	offset := fileSize(filepath.Join(h.Root, "hub.stdout.log"))
	h.Proc = startProc(t, "hub", h.Root, childEnv("MYCEL_HOME="+h.Home),
		"daemon", "--json", "--local-addr", "127.0.0.1:"+strconv.Itoa(h.LocalPort))
	var ready daemonReady
	h.Proc.waitReady(t, offset, &ready)
	if ready.Hub == nil || ready.Node == nil {
		t.Fatalf("hub daemon ready line lacks hub or node: %+v", ready)
	}
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
// home with it and starts the node's daemon.
func joinNode(t *testing.T, h *Hub, name, caps, apiKey string) *Node {
	t.Helper()
	args := []string{"--name", name}
	if caps != "" {
		args = append(args, "--caps", caps)
	}
	tok := createToken(t, h, apiKey, args...)
	return joinWithToken(t, h, name, tok.Token)
}

func joinWithToken(t *testing.T, h *Hub, name, token string) *Node {
	t.Helper()
	n := &Node{t: t, Home: filepath.Join(h.Root, name), Name: name, Token: token}
	env := childEnv("MYCEL_HOME=" + n.Home)
	out, errOut, code := runCLI(t, env, "join", token, "--json")
	if code != 0 {
		t.Fatalf("join %s: exit %d\nstdout: %s\nstderr: %s", name, code, out, errOut)
	}
	port := freePorts(t, 1)[0]
	n.Proc = startProc(t, name, h.Root, env, "daemon", "--json", "--local-addr", "127.0.0.1:"+strconv.Itoa(port))
	var ready daemonReady
	n.Proc.waitReady(t, 0, &ready)
	if ready.Node == nil || ready.Node.LocalAddr == "" {
		t.Fatalf("%s: ready line lacks node.local_addr: %+v", name, ready)
	}
	n.LocalAddr = ready.Node.LocalAddr
	n.LocalURL = "http://" + ready.Node.LocalAddr
	return n
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
