//go:build darwin

package applevf

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// Integration tests: they create real Lima VMs. Run with
//
//	MYCEL_TEST_LIMA=1 go test -run TestLima -timeout 40m ./internal/sandbox/applevf/
//
// MYCEL_TEST_LIMA_DOCKER=1 additionally runs the nested Docker test (dev
// profile, rootless dockerd installed during provisioning). The proxy stub
// listens on 127.0.0.1:0 (Lima forwards 192.168.5.2 to the host's loopback)
// unless MYCEL_TEST_LIMA_PROXY_LISTEN names another host. Instances live in a
// temporary LIMA_HOME whose path contains a space.

// stubProxy is a minimal egress proxy: Basic auth, CONNECT and plain HTTP
// forwarding, host allowlist.
type stubProxy struct {
	ln    net.Listener
	user  string
	pass  string
	allow func(host string) bool

	mu   sync.Mutex
	seen []string // "host:port allowed|denied"
}

func startStubProxy(t *testing.T, listenHost string, allow func(string) bool) *stubProxy {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, "0"))
	if err != nil {
		t.Fatal(err)
	}
	p := &stubProxy{ln: ln, user: "sbx-test", pass: "tok3n", allow: allow}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

func (p *stubProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *stubProxy) url(host string) string {
	return fmt.Sprintf("http://%s:%s@%s", p.user, p.pass, net.JoinHostPort(host, strconv.Itoa(p.port())))
}

func (p *stubProxy) record(hostport string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := "denied"
	if ok {
		v = "allowed"
	}
	p.seen = append(p.seen, hostport+" "+v)
}

func (p *stubProxy) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

func (p *stubProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass))
	if r.Header.Get("Proxy-Authorization") != want {
		w.Header().Set("Proxy-Authenticate", `Basic realm="mycel"`)
		http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		host, _, err := net.SplitHostPort(r.Host)
		ok := err == nil && p.allow(host)
		p.record(r.Host, ok)
		if !ok {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		up, err := net.DialTimeout("tcp", r.Host, 20*time.Second) //nolint:gosec // test proxy, host checked against the allowlist
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, _ := w.(http.Hijacker)
		conn, buf, err := hj.Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() {
			_, _ = io.Copy(up, buf)
			_ = up.(*net.TCPConn).CloseWrite()
		}()
		_, _ = io.Copy(conn, up)
		_ = conn.Close()
		_ = up.Close()
		return
	}
	if r.URL.Host == "" {
		http.Error(w, "not a proxy request", http.StatusBadRequest)
		return
	}
	ok := p.allow(r.URL.Hostname())
	p.record(r.URL.Host, ok)
	if !ok {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type execResult struct {
	code   int
	err    error
	stdout string
	stderr string
}

func runIn(t *testing.T, sb sandbox.Sandbox, argv []string, opts sandbox.ExecOpts) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p, err := sb.Exec(ctx, argv, opts)
	if err != nil {
		t.Fatalf("Exec %q: %v", argv, err)
	}
	var out, errb bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&out, p.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errb, p.Stderr()) }()
	code, err := p.Wait()
	wg.Wait()
	return execResult{code: code, err: err, stdout: out.String(), stderr: errb.String()}
}

func mustRun(t *testing.T, sb sandbox.Sandbox, argv ...string) string {
	t.Helper()
	r := runIn(t, sb, argv, sandbox.ExecOpts{})
	if r.err != nil || r.code != 0 {
		t.Fatalf("%q: code %d err %v\nstdout: %s\nstderr: %s", argv, r.code, r.err, r.stdout, r.stderr)
	}
	return r.stdout
}

func limaInstances(t *testing.T, home string) []string {
	t.Helper()
	cmd := exec.Command("limactl", "list", "--quiet")
	cmd.Env = limaEnv(home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("limactl list: %v", err)
	}
	return strings.Fields(string(out))
}

func requireLima(t *testing.T) *Backend {
	t.Helper()
	if os.Getenv("MYCEL_TEST_LIMA") != "1" {
		t.Skip("set MYCEL_TEST_LIMA=1 to create real Lima VMs")
	}
	b := New()
	// Short enough for Lima's socket paths, with a space to exercise quoting.
	home, err := os.MkdirTemp("/tmp", "mycel lima-")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MYCEL_TEST_LIMA_KEEP") == "1" {
		t.Logf("LIMA_HOME kept: %s", home)
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(home) })
	}
	b.SetLimaHome(home)
	if ok, why := b.Available(context.Background()); !ok {
		t.Fatalf("applevf not available: %s", why)
	}
	return b
}

func testID() string { return fmt.Sprintf("sbx:it%x", time.Now().UnixNano()) }

func TestLimaDefaultProfile(t *testing.T) {
	b := requireLima(t)
	ctx := context.Background()
	listen := os.Getenv("MYCEL_TEST_LIMA_PROXY_LISTEN")
	if listen == "" {
		listen = "127.0.0.1"
	}
	proxy := startStubProxy(t, listen, func(h string) bool { return h == "example.com" })

	const image = "docker.io/library/debian:bookworm-slim"
	t0 := time.Now()
	if err := b.Prepare(ctx, image, func(s string) { t.Logf("prepare: %s", s) }); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Logf("TIMING prepare: %s", time.Since(t0).Round(time.Second))

	in := filepath.Join(t.TempDir(), "seed.txt")
	if err := os.WriteFile(in, []byte("seeded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "sbx")
	spec := sandbox.Spec{
		ID:      testID(),
		Profile: &mycelv1.Profile{Name: "default", Image: image},
		Image:   image,
		Limits: sandbox.Limits{
			CPUs: 2, MemoryBytes: 2 << 30, DiskBytes: 10 << 30, MaxProcesses: 512, WallClock: 30 * time.Minute,
		},
		Env:       map[string]string{"GREETING": "hello world"},
		Workspace: sandbox.WorkspaceSeed{Inputs: []sandbox.Input{{LocalPath: in, Path: "in/seed.txt"}}},
		Egress:    sandbox.Egress{ProxyURL: proxy.url("127.0.0.1"), Allow: []string{"example.com"}},
		Dir:       dir,
	}
	inst, _ := instanceName(spec.ID)
	t1 := time.Now()
	sb, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("TIMING create: %s", time.Since(t1).Round(time.Second))
	destroyed := false
	defer func() {
		if !destroyed && os.Getenv("MYCEL_TEST_LIMA_KEEP") != "1" {
			_ = sb.Destroy(context.Background())
		}
	}()
	if fi, err := os.Stat(filepath.Join(dir, "lima.yaml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("lima.yaml: %v %v", fi, err)
	}

	t.Run("identity", func(t *testing.T) {
		out := mustRun(t, sb, "sh", "-c", "id -u; cat /etc/os-release")
		if !strings.HasPrefix(out, strconv.Itoa(guestUID)+"\n") || !strings.Contains(out, "bookworm") {
			t.Errorf("unexpected identity/os-release:\n%s", out)
		}
		if pwd := strings.TrimSpace(mustRun(t, sb, "pwd")); pwd != "/workspace" {
			t.Errorf("cwd %q", pwd)
		}
	})
	t.Run("no host file system", func(t *testing.T) {
		out := mustRun(t, sb, "sh", "-c", "ls -A /Users 2>/dev/null | wc -l; ls -d /host* /Volumes /private 2>/dev/null | wc -l; findmnt -rn -t virtiofs,9p,fuse.sshfs | wc -l")
		if f := strings.Fields(out); len(f) != 3 || f[0] != "0" || f[1] != "0" || f[2] != "0" {
			t.Errorf("host paths visible: %q", out)
		}
	})
	t.Run("not the host", func(t *testing.T) {
		out := strings.TrimSpace(mustRun(t, sb, "ps", "-p", "1", "-o", "comm="))
		if out == "launchd" || out == "" {
			t.Errorf("pid 1 is %q", out)
		}
		t.Logf("pid 1: %s; kernel: %s", out, strings.TrimSpace(mustRun(t, sb, "uname", "-sr")))
	})
	t.Run("no root", func(t *testing.T) {
		r := runIn(t, sb, []string{"sudo", "-n", "true"}, sandbox.ExecOpts{})
		if r.code == 0 {
			t.Error("sudo works for the sandbox user")
		}
		r = runIn(t, sb, []string{"nft", "flush", "ruleset"}, sandbox.ExecOpts{})
		if r.code == 0 {
			t.Error("the sandbox user can flush the firewall")
		}
	})
	t.Run("no other network", func(t *testing.T) {
		// DNS is not allowed at all.
		if r := runIn(t, sb, []string{"getent", "hosts", "example.com"}, sandbox.ExecOpts{}); r.code == 0 {
			t.Errorf("DNS resolution works: %q", r.stdout)
		}
		// Another port of the host is closed, even one that listens.
		other, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = other.Close() }()
		accepted := make(chan struct{}, 1)
		go func() {
			if c, err := other.Accept(); err == nil {
				accepted <- struct{}{}
				_ = c.Close()
			}
		}()
		port := strconv.Itoa(other.Addr().(*net.TCPAddr).Port)
		start := time.Now()
		r := runIn(t, sb, []string{"curl", "-sS", "--noproxy", "*", "--max-time", "10", "http://192.168.5.2:" + port + "/"}, sandbox.ExecOpts{})
		if r.code == 0 {
			t.Errorf("host port %s reachable: %q", port, r.stdout)
		}
		if time.Since(start) > 8*time.Second {
			t.Errorf("blocked connection took %s: the firewall should reject, not drop", time.Since(start))
		}
		select {
		case <-accepted:
			t.Errorf("host port %s accepted a connection from the guest", port)
		case <-time.After(200 * time.Millisecond):
		}
		// IPv6 egress.
		if r := runIn(t, sb, []string{"curl", "-sS", "-6", "--noproxy", "*", "--max-time", "10", "http://[2606:4700:4700::1111]/"}, sandbox.ExecOpts{}); r.code == 0 {
			t.Error("IPv6 egress works")
		}
		// vsock to the host (CID 2).
		r = runIn(t, sb, []string{"python3", "-c", `
import socket, sys
ok = []
for port in (22, 1024, 2222, 10240):
    s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
    s.settimeout(3)
    try:
        s.connect((2, port)); ok.append(port)
    except OSError:
        pass
    s.close()
print(ok)
sys.exit(1 if ok else 0)
`}, sandbox.ExecOpts{})
		if r.code != 0 {
			t.Errorf("vsock to the host: %q %q", r.stdout, r.stderr)
		}
		// User namespaces are off (nodocker).
		if r := runIn(t, sb, []string{"unshare", "-U", "-r", "true"}, sandbox.ExecOpts{}); r.code == 0 {
			t.Error("unprivileged user namespaces available")
		}
	})
	t.Run("login files are ignored", func(t *testing.T) {
		mustRun(t, sb, "sh", "-c", `printf 'echo HIJACK; exit 99\n' > ~/.bashrc; cp ~/.bashrc ~/.profile; cp ~/.bashrc ~/.bash_profile; mkdir -p ~/.ssh; printf 'echo HIJACK\n' > ~/.ssh/rc`)
		if out := mustRun(t, sb, "echo", "clean"); out != "clean\n" {
			t.Errorf("login files ran: %q", out)
		}
		if got, err := sb.Glob(ctx, []string{"in/*"}); err != nil || len(got) != 1 {
			t.Errorf("helper after dotfiles: %q %v", got, err)
		}
	})
	t.Run("egress", func(t *testing.T) {
		out := mustRun(t, sb, "curl", "-sS", "--max-time", "60", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com")
		if out != "200" {
			t.Errorf("example.com through proxy: %q", out)
		}
		r := runIn(t, sb, []string{"curl", "-sS", "--max-time", "30", "https://github.com"}, sandbox.ExecOpts{})
		if r.code == 0 {
			t.Errorf("github.com reachable: %s", r.stdout)
		}
		r = runIn(t, sb, []string{"curl", "-sS", "--noproxy", "*", "--max-time", "15", "https://example.com"}, sandbox.ExecOpts{})
		if r.code == 0 {
			t.Error("direct connection to example.com bypassing the proxy succeeded")
		}
		r = runIn(t, sb, []string{"curl", "-sS", "--noproxy", "*", "--max-time", "15", "http://1.1.1.1"}, sandbox.ExecOpts{})
		if r.code == 0 {
			t.Error("direct connection to 1.1.1.1 succeeded")
		}
		t.Logf("proxy log: %v", proxy.log())
		if !slices.Contains(proxy.log(), "example.com:443 allowed") || !slices.Contains(proxy.log(), "github.com:443 denied") {
			t.Errorf("proxy did not see the expected requests: %v", proxy.log())
		}
	})
	t.Run("argv and env are not shell-interpreted", func(t *testing.T) {
		weird := `a b $HOME "q" 'x' ; $(id) ` + "`id`" + ` \ * ~`
		r := runIn(t, sb, []string{"printf", "%s|%s", weird, ""}, sandbox.ExecOpts{Env: map[string]string{"X_Y": "v w $HOME"}})
		if r.stdout != weird+"|" {
			t.Errorf("argv mangled: %q", r.stdout)
		}
		out := mustRun(t, sb, "sh", "-c", `printf '%s/%s' "$GREETING" "$HTTPS_PROXY"`)
		if out != "hello world/http://sbx-test:tok3n@192.168.5.2:"+strconv.Itoa(proxy.port()) {
			t.Errorf("env: %q", out)
		}
		r = runIn(t, sb, []string{"sh", "-c", `printf %s "$X_Y"; exit 7`}, sandbox.ExecOpts{Env: map[string]string{"X_Y": "v w $HOME"}})
		if r.code != 7 || r.err != nil || r.stdout != "v w $HOME" {
			t.Errorf("exit/env: %+v", r)
		}
		r = runIn(t, sb, []string{"cat"}, sandbox.ExecOpts{Stdin: strings.NewReader("from stdin"), Dir: "in"})
		if r.stdout != "from stdin" {
			t.Errorf("stdin: %+v", r)
		}
	})
	t.Run("files", func(t *testing.T) {
		if out := mustRun(t, sb, "cat", "/workspace/in/seed.txt"); out != "seeded\n" {
			t.Errorf("seed: %q", out)
		}
		if err := sb.CopyIn(ctx, strings.NewReader("payload"), "out/a b.txt", 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sb.CopyIn(ctx, strings.NewReader("x"), "../escape", 0o600); err == nil {
			t.Error("CopyIn outside /workspace accepted")
		}
		if out := mustRun(t, sb, "stat", "-c", "%a %U", "/workspace/out/a b.txt"); out != "600 mycel\n" {
			t.Errorf("mode/owner: %q", out)
		}
		rc, err := sb.CopyOut(ctx, "out")
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(rc)
		found := map[string]string{}
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(tr)
			found[h.Name] = string(b)
		}
		_ = rc.Close()
		if found["out/a b.txt"] != "payload" {
			t.Errorf("CopyOut: %v", found)
		}
		rc, err = sb.CopyOut(ctx, "missing")
		if err == nil {
			_, err = io.ReadAll(rc)
			_ = rc.Close()
		}
		if err == nil {
			t.Error("CopyOut of a missing path succeeded")
		}
		got, err := sb.Glob(ctx, []string{"out/*.txt", "**/seed.txt", "nothing*"})
		if err != nil || !slices.Equal(got, []string{"in/seed.txt", "out/a b.txt"}) {
			t.Errorf("Glob: %q %v", got, err)
		}
	})
	t.Run("signals", func(t *testing.T) {
		p, err := sb.Exec(ctx, []string{"sh", "-c", "sleep 300 & sleep 300; echo never"}, sandbox.ExecOpts{})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()
		go func() { _, _ = io.Copy(io.Discard, p.Stderr()) }()
		time.Sleep(2 * time.Second)
		start := time.Now()
		if err := p.Signal("KILL"); err != nil {
			t.Fatal(err)
		}
		_, err = p.Wait()
		if !errors.Is(err, sandbox.ErrKilled) || time.Since(start) > 30*time.Second {
			t.Errorf("Wait after KILL: %v after %s", err, time.Since(start))
		}
		if out := mustRun(t, sb, "sh", "-c", "pgrep -c -x sleep || true"); strings.TrimSpace(out) != "0" {
			t.Errorf("sleep processes left: %s", out)
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		p, err = sb.Exec(cctx, []string{"sleep", "300"}, sandbox.ExecOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Wait(); !errors.Is(err, sandbox.ErrTimeout) {
			t.Errorf("Wait after ctx deadline: %v", err)
		}
	})
	t.Run("pause and stats", func(t *testing.T) {
		pctx, cancel := context.WithCancel(ctx)
		defer cancel()
		ticker, err := sb.Exec(pctx, []string{"sh", "-c", "while :; do echo t >> /workspace/tick; sleep 0.1; done"}, sandbox.ExecOpts{})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.Copy(io.Discard, ticker.Stdout()) }()
		go func() { _, _ = io.Copy(io.Discard, ticker.Stderr()) }()
		ticks := func() int {
			n, _ := strconv.Atoi(strings.TrimSpace(mustRun(t, sb, "sh", "-c", "wc -l < /workspace/tick")))
			return n
		}
		time.Sleep(time.Second)
		if err := sb.Pause(ctx); err != nil {
			t.Fatal(err)
		}
		a := ticks()
		time.Sleep(2 * time.Second)
		if b := ticks(); b != a || a == 0 {
			t.Errorf("ticker kept running while paused: %d -> %d", a, b)
		}
		if err := sb.Resume(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if c := ticks(); c <= a {
			t.Errorf("ticker did not resume: %d -> %d", a, c)
		}
		cancel()
		if _, err := ticker.Wait(); !errors.Is(err, sandbox.ErrKilled) {
			t.Errorf("ticker Wait: %v", err)
		}
		st, err := sb.Stats(ctx)
		if err != nil || st.MemoryBytes <= 0 || st.DiskBytes <= 0 || st.Processes <= 0 {
			t.Errorf("Stats: %+v %v", st, err)
		}
		t.Logf("stats: %+v", st)
	})

	if os.Getenv("MYCEL_TEST_LIMA_KEEP") == "1" {
		t.Logf("instance %s kept", inst)
		return
	}
	t2 := time.Now()
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	destroyed = true
	t.Logf("TIMING destroy: %s", time.Since(t2).Round(time.Second))
	if err := sb.Destroy(ctx); err != nil {
		t.Errorf("second Destroy: %v", err)
	}
	if slices.Contains(limaInstances(t, b.limaHome), inst) {
		t.Errorf("instance %s still listed", inst)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sandbox dir left: %v", err)
	}
}

func TestLimaDockerNested(t *testing.T) {
	b := requireLima(t)
	if os.Getenv("MYCEL_TEST_LIMA_DOCKER") != "1" {
		t.Skip("set MYCEL_TEST_LIMA_DOCKER=1 for the nested Docker test")
	}
	ctx := context.Background()
	allowed := []string{"deb.debian.org", "security.debian.org", "registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com"}
	proxy := startStubProxy(t, "127.0.0.1", func(h string) bool {
		return slices.Contains(allowed, h) || strings.HasSuffix(h, ".cloudflare.docker.com") || strings.HasSuffix(h, ".cloudfront.docker.com")
	})
	const image = "docker.io/library/debian:bookworm"
	spec := sandbox.Spec{
		ID:      testID(),
		Profile: &mycelv1.Profile{Name: "dev", Image: image, Brokers: []string{"docker-nested", "ollama"}},
		Image:   image,
		Limits:  sandbox.Limits{CPUs: 4, MemoryBytes: 4 << 30, DiskBytes: 20 << 30, MaxProcesses: 2048},
		Egress:  sandbox.Egress{ProxyURL: proxy.url("127.0.0.1")},
		Dir:     filepath.Join(t.TempDir(), "sbx"),
	}
	t1 := time.Now()
	sb, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("TIMING create with docker: %s", time.Since(t1).Round(time.Second))
	defer func() { _ = sb.Destroy(context.Background()) }()
	r := runIn(t, sb, []string{"docker", "run", "--rm", "hello-world"}, sandbox.ExecOpts{})
	if r.code != 0 || !strings.Contains(r.stdout, "Hello from Docker!") {
		t.Errorf("docker run hello-world: %+v", r)
	}
	r = runIn(t, sb, []string{"sh", "-c", "id -nG; docker info --format '{{.SecurityOptions}}'"}, sandbox.ExecOpts{})
	t.Logf("groups and docker security options: %s %s", r.stdout, r.stderr)
	if !strings.Contains(r.stdout, "rootless") || strings.Contains(r.stdout, "docker ") {
		t.Errorf("docker is not rootless or the user is in the docker group: %q", r.stdout)
	}
	t.Logf("proxy log: %v", proxy.log())
}
