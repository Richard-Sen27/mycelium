//go:build darwin

package applevf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// ---- fake runner ----

type call struct {
	args  []string
	stdin string
}

type exitErr struct{ code int }

func (e *exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *exitErr) ExitCode() int { return e.code }

type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	// respond returns the output of a call; nil means success with no output.
	respond func(args []string, stdin string) (stdout, stderr string, err error)
	// block, if set, makes started commands run until killed.
	block bool
}

func (f *fakeRunner) record(args []string, stdin io.Reader) call {
	c := call{args: slices.Clone(args)}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.stdin = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return c
}

func (f *fakeRunner) reply(c call) (string, string, error) {
	if f.respond == nil {
		return "", "", nil
	}
	return f.respond(c.args, c.stdin)
}

func (f *fakeRunner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	c := f.record(args, stdin)
	out, errOut, err := f.reply(c)
	return []byte(out), []byte(errOut), err
}

func (f *fakeRunner) Start(stdin io.Reader, args ...string) (runningCmd, error) {
	var c call
	if f.block {
		c = f.record(args, nil) // stdin of a blocking command is never read
	} else {
		c = f.record(args, stdin)
	}
	out, errOut, err := f.reply(c)
	fc := &fakeCmd{stdout: newBufPipe(1<<20, ""), stderr: newBufPipe(1<<20, ""), exit: make(chan struct{})}
	_, _ = fc.stdout.Write([]byte(out))
	_, _ = fc.stderr.Write([]byte(errOut))
	fc.code = exitCode(err)
	if err == nil {
		fc.code = 0
	}
	if !f.block {
		fc.Kill()
	}
	return fc, nil
}

func (f *fakeRunner) argsOf() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.args
	}
	return out
}

func (f *fakeRunner) allCalls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeRunner) callsTo(sub string) []call {
	var out []call
	for _, c := range f.allCalls() {
		if len(c.args) > 0 && c.args[0] == sub {
			out = append(out, c)
		}
	}
	return out
}

// helperCalls returns the calls of one helper operation.
func (f *fakeRunner) helperCalls(op string) []call {
	var out []call
	for _, c := range f.allCalls() {
		if o, _ := helperOp(c.args); o == op {
			out = append(out, c)
		}
	}
	return out
}

type fakeCmd struct {
	stdout, stderr *bufPipe
	code           int
	exit           chan struct{}
	once           sync.Once
}

func (c *fakeCmd) Stdout() io.Reader { return c.stdout }
func (c *fakeCmd) Stderr() io.Reader { return c.stderr }
func (c *fakeCmd) Kill()             { c.once.Do(func() { close(c.exit) }) }

// Wait ends the output streams only after exit, like the real runner, so a
// caller that reads to EOF before Wait would hang here as it would there.
func (c *fakeCmd) Wait() (int, error) {
	<-c.exit
	c.stdout.CloseWrite()
	c.stderr.CloseWrite()
	return c.code, nil
}

// helperOp returns the helper operation and its arguments of a shell call.
func helperOp(args []string) (string, []string) {
	i := slices.Index(args, guestHelper)
	if i < 0 || i+1 >= len(args) {
		return "", nil
	}
	return args[i+1], args[i+2:]
}

func decArg(t *testing.T, a string) string {
	t.Helper()
	if !strings.HasPrefix(a, "b") {
		t.Fatalf("argument %q is not encoded", a)
	}
	b, err := base64.StdEncoding.DecodeString(a[1:])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// shortTempDir is a temp dir short enough to serve as LIMA_HOME.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func testBackend(t *testing.T, goarch string) *Backend {
	b := New()
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	b.goarch = goarch
	b.osVersion = func() (string, error) { return "26.6", nil }
	b.lookPath = func(string) (string, error) { return "/opt/homebrew/bin/limactl", nil }
	b.newRunner = func(string, []string, string) runner {
		return &fakeRunner{respond: func([]string, string) (string, string, error) {
			return "limactl version 2.1.1\n", "", nil
		}}
	}
	b.SetLimaHome(shortTempDir(t))
	return b
}

func testSandbox(r runner, dir string) *limaSandbox {
	return &limaSandbox{
		id: "sbx:01J0TEST", inst: "sbx-01j0test", dir: dir, r: r,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		env:     map[string]string{},
		procs:   map[*process]struct{}{},
		created: time.Now(),
	}
}

// sandboxDir is a removable sandbox directory (three or more path elements).
func sandboxDir(t *testing.T) string { return filepath.Join(t.TempDir(), "sbx") }

var helperPrefix = []string{"shell", "--tty=false", "--shell=" + guestShell, "--workdir=/workspace", "sbx-01j0test", "--", guestHelper}

// ---- YAML ----

func renderMap(t *testing.T, p vmParams) (map[string]any, string) {
	t.Helper()
	b, err := renderLimaYAML(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	prov := m["provision"].([]any)
	if len(prov) != 1 {
		t.Fatalf("provision: %v", prov)
	}
	pm := prov[0].(map[string]any)
	if pm["mode"] != "system" {
		t.Errorf("provision mode %v", pm["mode"])
	}
	return m, pm["script"].(string)
}

func TestRenderLimaYAML(t *testing.T) {
	img, err := resolveImage("docker.io/library/debian:bookworm-slim", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	m, script := renderMap(t, vmParams{
		Image:     img,
		Limits:    sandbox.Limits{CPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 40 << 30, MaxProcesses: 2048},
		ProxyPort: 3128,
	})
	checks := map[string]any{
		"vmType": "vz", "os": "Linux", "arch": "aarch64", "cpus": 4,
		"memory": "8192MiB", "disk": "40960MiB", "plain": true, "propagateProxyEnv": false,
	}
	for k, want := range checks {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	for _, k := range []string{"mounts", "networks"} {
		v, ok := m[k].([]any)
		if !ok || len(v) != 0 {
			t.Errorf("%s = %#v, want an explicit empty list", k, m[k])
		}
	}
	if c := m["containerd"].(map[string]any); c["system"] != false || c["user"] != false {
		t.Errorf("containerd = %v", c)
	}
	if s := m["ssh"].(map[string]any); s["forwardAgent"] != false || s["loadDotSSHPubKeys"] != false {
		t.Errorf("ssh = %v", s)
	}
	for _, pf := range m["portForwards"].([]any) {
		if pf.(map[string]any)["ignore"] != true {
			t.Errorf("port forward not ignored: %v", pf)
		}
	}
	imgs := m["images"].([]any)
	if len(imgs) != 1 || imgs[0].(map[string]any)["digest"] != img.Digest || imgs[0].(map[string]any)["location"] != img.Location {
		t.Errorf("images = %v", imgs)
	}
	if u := m["user"].(map[string]any); u["name"] != guestUser || u["uid"] != guestUID {
		t.Errorf("user = %v", u)
	}
	for _, want := range []string{
		"MYCEL_PROXY_PORT=3128",
		"MYCEL_MAX_PROCS=2048",
		"MYCEL_DOCKER=0",
		`ip daddr 192.168.5.2 tcp dport $MYCEL_PROXY_PORT accept`,
		"ip daddr { 192.168.5.2, 255.255.255.255 } udp sport 68 udp dport 67 accept",
		"type filter hook output priority 0; policy drop;",
		"type filter hook forward priority 0; policy drop;",
		"meta l4proto tcp counter reject with tcp reset",
		"counter reject",
		"table inet mycel", // inet: IPv4 and IPv6
		"ct state established,related accept",
		"user.max_user_namespaces = 0",
		"kernel.unprivileged_userns_clone = 0",
		"TasksMax=$MYCEL_MAX_PROCS",
		"nproc $MYCEL_MAX_PROCS",
		"install -d -o \"$MYCEL_USER\"",
		"exec /bin/bash --noprofile --norc",
		"usermod -s /usr/local/libexec/mycel/shell",
		"PermitUserRC no",
		"/workspace",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("provision script lacks %q", want)
		}
	}
	for _, bad := range []string{"192.168.5.3", "dport 53", "counter drop"} {
		if strings.Contains(script, bad) {
			t.Errorf("provision script contains %q", bad)
		}
	}
	// The embedded helpers decode to the shipped scripts.
	for name, src := range map[string]string{"HELPER": helperScript, "CONFIGURE": configureScript} {
		start := strings.Index(script, "<<'"+name+"'\n")
		end := strings.Index(script, "\n"+name+"\n")
		if start < 0 || end < 0 {
			t.Fatalf("%s heredoc missing", name)
		}
		b64 := strings.ReplaceAll(script[start+len("<<'"+name+"'\n"):end], "\n", "")
		got, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || string(got) != src {
			t.Errorf("%s heredoc does not decode to the script (%v)", name, err)
		}
	}
	// Docker is installed during provisioning only when asked for.
	_, dscript := renderMap(t, vmParams{Image: img, ProxyPort: 3128, Docker: true})
	if !strings.Contains(dscript, "MYCEL_DOCKER=1") {
		t.Error("docker flag not rendered")
	}
	if strings.Contains(configureScript, "apt-get") {
		t.Error("the configure step must not install packages (it runs through the job's proxy)")
	}
}

func TestRenderLimaYAMLNoProxyAndDefaults(t *testing.T) {
	img, _ := resolveImage("ubuntu:24.04", "amd64")
	m, script := renderMap(t, vmParams{Image: img})
	if m["cpus"] != defaultCPUs || m["memory"] != "4096MiB" || m["disk"] != "20480MiB" || m["arch"] != "x86_64" {
		t.Errorf("defaults: cpus %v memory %v disk %v arch %v", m["cpus"], m["memory"], m["disk"], m["arch"])
	}
	if !strings.Contains(script, "MYCEL_PROXY_PORT=0\n") || !strings.Contains(script, "MYCEL_MAX_PROCS=512\n") {
		t.Error("no-proxy/default process limit not rendered")
	}
	// The proxy rule is guarded by a port > 0 test in the script.
	if !strings.Contains(script, `if [ "$MYCEL_PROXY_PORT" -gt 0 ]; then`) {
		t.Error("proxy rule is not conditional")
	}
	if _, err := renderLimaYAML(vmParams{Image: img, ProxyPort: 70000}); err == nil {
		t.Error("bad port accepted")
	}
	small, _ := renderMap(t, vmParams{Image: img, Limits: sandbox.Limits{MemoryBytes: 1 << 20, DiskBytes: 1 << 20}})
	if small["memory"] != "512MiB" || small["disk"] != "4096MiB" {
		t.Errorf("minimums: %v %v", small["memory"], small["disk"])
	}
}

func TestWriteLimaYAML(t *testing.T) {
	dir := sandboxDir(t)
	p, err := writeLimaYAML(dir, []byte("a: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil { //nolint:gosec // simulate a pre-existing world-readable file
		t.Fatal(err)
	}
	if _, err := writeLimaYAML(dir, []byte("a: 2\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v %v", fi.Mode(), err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", di.Mode())
	}
	// A symlink planted at lima.yaml is replaced, not followed.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p)
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	if _, err := writeLimaYAML(dir, []byte("a: 3\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("symlink target overwritten: %q", b)
	}
	if fi, _ := os.Lstat(p); fi.Mode()&os.ModeSymlink != 0 {
		t.Error("lima.yaml is still a symlink")
	}
}

// ---- images, names, proxy, environment ----

func TestImageMapping(t *testing.T) {
	cases := []struct {
		ref, arch, want string
	}{
		{"docker.io/library/debian:bookworm-slim", "arm64", "debian-12-genericcloud-arm64"},
		{"docker.io/library/debian:bookworm", "amd64", "debian-12-genericcloud-amd64"},
		{"debian:12", "arm64", "debian-12-genericcloud-arm64"},
		{"library/debian:bookworm-20260101", "arm64", "debian-12-genericcloud-arm64"},
		{"ubuntu:24.04", "arm64", "ubuntu-24.04-server-cloudimg-arm64"},
		{"ubuntu:noble-20260101", "arm64", "ubuntu-24.04-server-cloudimg-arm64"},
		{"ubuntu:latest", "arm64", "ubuntu-24.04-server-cloudimg-arm64"},
		{"docker.io/library/ubuntu", "amd64", "ubuntu-24.04-server-cloudimg-amd64"},
	}
	for _, c := range cases {
		img, err := resolveImage(c.ref, c.arch)
		if err != nil || !strings.Contains(img.Location, c.want) || !strings.HasPrefix(img.Location, "https://") {
			t.Errorf("%s/%s: %v %v", c.ref, c.arch, img.Location, err)
		}
		if _, _, _, err := digestHash(img.Digest); err != nil {
			t.Errorf("%s: %v", c.ref, err)
		}
	}
	for _, ref := range []string{
		"debian:bullseye", "docker.io/library/debian", "alpine:3", "none", "", "ghcr.io/x/debian:bookworm",
		"debian:bookworm@sha256:abcd", "ubuntu:22.04", "ubuntu:jammy", "ubuntu:rolling", "ubuntu:24.10",
	} {
		_, err := resolveImage(ref, "arm64")
		if !errors.Is(err, sandbox.ErrUnavailable) || !strings.Contains(err.Error(), "applevf") {
			t.Errorf("%q: err %v, want ErrUnavailable", ref, err)
		}
	}
	if _, err := resolveImage("debian:bookworm", "riscv64"); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("riscv64: %v", err)
	}
}

func TestInstanceName(t *testing.T) {
	a, err := instanceName("sbx:01JABCDEFGHJKMNPQRSTVWXYZ0")
	if err != nil || len(a) != len("sbx-")+12 || !strings.HasPrefix(a, "sbx-") || strings.Trim(a[4:], "0123456789abcdef") != "" {
		t.Errorf("%q %v", a, err)
	}
	b, _ := instanceName("sbx:01JABCDEFGHJKMNPQRSTVWXYZ1")
	if a == b {
		t.Error("different ids share an instance name")
	}
	if again, _ := instanceName("sbx:01JABCDEFGHJKMNPQRSTVWXYZ0"); again != a {
		t.Error("instance name is not deterministic")
	}
	for _, in := range []string{"", "a\x00b", strings.Repeat("a", 129)} {
		if _, err := instanceName(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestGuestProxyURL(t *testing.T) {
	cases := []struct {
		in, want string
		port     int
		bad      bool
	}{
		{"", "", 0, false},
		{"http://sbx%3A01J:tok@127.0.0.1:3128", "http://sbx%3A01J:tok@192.168.5.2:3128", 3128, false},
		{"http://user:p%40ss@0.0.0.0:41000/", "http://user:p%40ss@192.168.5.2:41000", 41000, false},
		{"http://u:t@[::1]:8080", "http://u:t@192.168.5.2:8080", 8080, false},
		{"http://proxy.local", "http://192.168.5.2:80", 80, false},
		{"https://h:1", "", 0, true},
		{"socks5://h:1", "", 0, true},
		{"http://h:0", "", 0, true},
		{"http://h:99999", "", 0, true},
		{"http://:3128", "", 0, true},
		{"::bad", "", 0, true},
	}
	for _, c := range cases {
		got, port, err := guestProxyURL(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q accepted: %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want || port != c.port {
			t.Errorf("%q: got %q %d %v, want %q %d", c.in, got, port, err, c.want, c.port)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	got, err := mergeEnv(map[string]string{"A": "1", "B": "x"}, map[string]string{"A": "2"})
	if err != nil || got["A"] != "2" || got["B"] != "x" {
		t.Errorf("%v %v", got, err)
	}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "Https_Proxy", "no_proxy", "ALL_PROXY", "all_proxy", "1X", "A-B", ""} {
		if _, err := mergeEnv(map[string]string{k: "v"}); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
	if _, err := mergeEnv(map[string]string{"A": "a\x00"}); err == nil {
		t.Error("NUL accepted")
	}
}

func TestLimaEnvScrubbed(t *testing.T) {
	t.Setenv("SSH", "/tmp/evil-ssh")
	t.Setenv("HTTPS_PROXY", "http://corp:3128")
	t.Setenv("http_proxy", "http://corp:3128")
	t.Setenv("LIMA_INSTANCE", "other")
	t.Setenv("LIMA_HOME", "/elsewhere")
	env := limaEnv("/data/lima")
	if !slices.Contains(env, "LIMA_HOME=/data/lima") {
		t.Errorf("LIMA_HOME missing: %q", env)
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "SSH", strings.HasSuffix(strings.ToUpper(k), "_PROXY"), k == "LIMA_INSTANCE":
			t.Errorf("%s leaked into the limactl environment", k)
		case k == "LIMA_HOME" && kv != "LIMA_HOME=/data/lima":
			t.Errorf("host LIMA_HOME leaked: %s", kv)
		}
	}
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Error("PATH missing")
	}
}

func TestCheckLimaHome(t *testing.T) {
	home := shortTempDir(t)
	if err := checkLimaHome(home); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"override.yaml", "default.yaml"} {
		if err := os.MkdirAll(filepath.Join(home, "_config"), 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(home, "_config", f)
		if err := os.WriteFile(p, []byte("mounts: [{location: \"~\"}]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkLimaHome(home); !errors.Is(err, sandbox.ErrUnavailable) || !strings.Contains(err.Error(), f) {
			t.Errorf("%s: %v", f, err)
		}
		_ = os.Remove(p)
	}
	if err := checkLimaHome("/" + strings.Repeat("x", 80)); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("long path: %v", err)
	}
}

func TestCheckRemovable(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, d := range []string{"/", "/Users", "/tmp/x", home, filepath.Dir(home), "/data/lima", "/data", "relative/a/b/c"} {
		if err := checkRemovable(d, "/data/lima"); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
	for _, d := range []string{filepath.Join(home, "Library", "x", "sbx"), "/data/sandboxes/sbx1", "/var/tmp/mycel/sbx"} {
		if err := checkRemovable(d, "/data/lima"); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
}

func TestLimaVersion(t *testing.T) {
	for out, ok := range map[string]bool{
		"limactl version 2.1.1\n": true, "limactl version 3.0.0": true, "limactl version v2.0.0-beta": true,
		"limactl version 1.2.1": false, "": false, "garbage": false,
	} {
		if _, got := limaVersionOK(out); got != ok {
			t.Errorf("%q: %v", out, got)
		}
	}
}

func TestAvailable(t *testing.T) {
	b := testBackend(t, "arm64")
	if ok, why := b.Available(context.Background()); !ok {
		t.Fatalf("not available: %s", why)
	}
	b.newRunner = func(string, []string, string) runner {
		return &fakeRunner{respond: func([]string, string) (string, string, error) { return "limactl version 1.0.4\n", "", nil }}
	}
	if ok, why := b.Available(context.Background()); ok || !strings.Contains(why, "1.0.4") {
		t.Errorf("old Lima: %v %q", ok, why)
	}
	b.osVersion = func() (string, error) { return "12.7.1", nil }
	if ok, why := b.Available(context.Background()); ok || !strings.Contains(why, "12.7.1") {
		t.Errorf("macOS 12: %v %q", ok, why)
	}
	b.osVersion = func() (string, error) { return "13.0", nil }
	b.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if ok, why := b.Available(context.Background()); ok || !strings.Contains(why, "limactl") {
		t.Errorf("no limactl: %v %q", ok, why)
	}
	c := b.Capabilities()
	if c.Isolation != "vm" || !c.Egress || !c.NestedDocker || !c.ProcessLimits || !c.MemoryLimit || !c.CPULimit {
		t.Errorf("capabilities %+v", c)
	}
	if b.Name() != sandbox.BackendAppleVF {
		t.Error(b.Name())
	}
	if !slices.ContainsFunc(sandbox.Backends(), func(x sandbox.Backend) bool { return x.Name() == sandbox.BackendAppleVF }) {
		t.Error("backend not registered")
	}
}

func TestWorkspacePaths(t *testing.T) {
	ok := map[string]string{
		"a.txt": "a.txt", "dir/b": "dir/b", "./x/../y": "y", "/workspace": ".",
		"/workspace/a/b": "a/b", "/workspace/a/../b": "b", "a b/c$d": "a b/c$d", ".": ".",
	}
	for in, want := range ok {
		if got, err := workspaceRel(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "..", "../x", "a/../../x", "/etc/passwd", "/workspace/../etc", "/workspacex/a", "/", "a\x00b"} {
		if got, err := workspaceRel(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
	for _, p := range []string{"*.txt", "**/out/*", "a/b?c"} {
		if err := globPattern(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/etc/*", "../*", "a/../../b", "x\x00"} {
		if err := globPattern(p); err == nil {
			t.Errorf("glob %q accepted", p)
		}
	}
}

// ---- Create / Destroy ----

const testToken = "s3cretT0ken"

func testSpec(dir string, brokers []string) sandbox.Spec {
	return sandbox.Spec{
		ID:      "sbx:01JTESTSANDBOX",
		Profile: &mycelv1.Profile{Name: "dev", Image: "docker.io/library/debian:bookworm", Brokers: brokers},
		Limits:  sandbox.Limits{CPUs: 2, MemoryBytes: 2 << 30, DiskBytes: 10 << 30, MaxProcesses: 100},
		Env:     map[string]string{"FOO": "bar"},
		Egress:  sandbox.Egress{ProxyURL: "http://sbx%3A01JTESTSANDBOX:" + testToken + "@127.0.0.1:41234"},
		Dir:     dir,
	}
}

func TestCreateFlow(t *testing.T) {
	for _, docker := range []bool{false, true} {
		t.Run(fmt.Sprintf("docker=%v", docker), func(t *testing.T) {
			dir := sandboxDir(t)
			in := filepath.Join(t.TempDir(), "in.sh")
			if err := os.WriteFile(in, []byte("echo hi"), 0o750); err != nil { //nolint:gosec // the mode is what the test checks
				t.Fatal(err)
			}
			brokers := []string{"ollama"}
			if docker {
				brokers = append(brokers, brokerDockerNested)
			}
			spec := testSpec(dir, brokers)
			spec.Workspace.Inputs = []sandbox.Input{{LocalPath: in, Path: "bin/in.sh"}}
			f := &fakeRunner{}
			b := testBackend(t, "arm64")
			home, _ := b.limaHomeDir()
			sb, err := b.create(context.Background(), f, home, spec)
			if err != nil {
				t.Fatal(err)
			}
			inst, _ := instanceName(spec.ID)
			calls := f.argsOf()
			if len(calls) != 4 {
				t.Fatalf("calls: %q", calls)
			}
			yamlPath := filepath.Join(dir, "lima.yaml")
			want0 := []string{"start", "--tty=false", "--timeout=10m0s", "--name=" + inst, yamlPath}
			if !slices.Equal(calls[0], want0) {
				t.Errorf("start argv %q", calls[0])
			}
			mode := "nodocker"
			if docker {
				mode = "docker"
			}
			want1 := []string{"shell", "--tty=false", "--shell=" + guestShell, "--workdir=/", inst, "--", "sudo", "-n", guestConfigure, guestUser, mode}
			if !slices.Equal(calls[1], want1) {
				t.Errorf("configure argv %q", calls[1])
			}
			if f.calls[1].stdin != "http://sbx%3A01JTESTSANDBOX:"+testToken+"@192.168.5.2:41234\n" {
				t.Errorf("configure stdin %q", f.calls[1].stdin)
			}
			if op, _ := helperOp(calls[2]); op != "check" || calls[2][2] != "--reconnect" {
				t.Errorf("third call %q", calls[2])
			}
			op, args := helperOp(calls[3])
			if op != "copyin" || len(args) != 2 || decArg(t, args[0]) != "bin/in.sh" || args[1] != "750" || f.calls[3].stdin != "echo hi" {
				t.Errorf("copy-in call %q stdin %q", calls[3], f.calls[3].stdin)
			}
			data, err := os.ReadFile(yamlPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), testToken) || !strings.Contains(string(data), "MYCEL_PROXY_PORT=41234") {
				t.Error("YAML must hold the proxy port but not the credential")
			}
			if strings.Contains(string(data), "MYCEL_DOCKER=1") != docker {
				t.Error("docker provisioning flag does not follow the profile")
			}
			if fi, _ := os.Stat(yamlPath); fi.Mode().Perm() != 0o600 {
				t.Errorf("yaml mode %v", fi.Mode())
			}
			ls := sb.(*limaSandbox)
			if len(ls.env) != 1 || ls.env["FOO"] != "bar" {
				t.Errorf("env %v: the proxy variables must not be passed per command", ls.env)
			}
		})
	}
}

// No runner argv may contain the proxy credential or a secret from the
// environment, neither plain nor base64-encoded.
func TestNoSecretInArgv(t *testing.T) {
	const envSecret = "env-Secret-Value-42" //nolint:gosec // test marker, not a credential
	f := &fakeRunner{}
	b := testBackend(t, "arm64")
	home, _ := b.limaHomeDir()
	spec := testSpec(sandboxDir(t), []string{brokerDockerNested})
	spec.Env["API_KEY"] = envSecret
	sb, err := b.create(context.Background(), f, home, spec)
	if err != nil {
		t.Fatal(err)
	}
	p, err := sb.Exec(context.Background(), []string{"sh", "-c", "echo " + envSecret}, sandbox.ExecOpts{Env: map[string]string{"TOKEN": envSecret}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Wait()
	ctx := context.Background()
	_ = sb.CopyIn(ctx, strings.NewReader(envSecret), "x", 0o600)
	_, _ = sb.Glob(ctx, []string{"*"})
	_ = sb.Pause(ctx)
	_ = sb.Resume(ctx)
	_, _ = sb.Stats(ctx)
	_ = sb.Destroy(ctx)

	secrets := []string{testToken, envSecret}
	for _, c := range f.allCalls() {
		for _, a := range c.args {
			texts := []string{a}
			if strings.HasPrefix(a, "b") {
				if d, err := base64.StdEncoding.DecodeString(a[1:]); err == nil {
					texts = append(texts, string(d))
				}
			}
			if d, err := base64.StdEncoding.DecodeString(a); err == nil {
				texts = append(texts, string(d))
			}
			for _, txt := range texts {
				for _, s := range secrets {
					if strings.Contains(txt, s) {
						t.Errorf("secret %q in argv %q", s, c.args)
					}
				}
			}
		}
	}
	// The request went over stdin.
	req := f.helperCalls("putreq")
	if len(req) != 1 || !strings.Contains(req[0].stdin, "TOKEN="+envSecret) || !strings.Contains(req[0].stdin, "API_KEY="+envSecret) {
		t.Errorf("exec request: %+v", req)
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	for _, failAt := range []string{"start", "configure", "check", "copyin"} {
		t.Run(failAt, func(t *testing.T) {
			dir := sandboxDir(t)
			spec := testSpec(dir, nil)
			if failAt == "copyin" {
				in := filepath.Join(t.TempDir(), "in")
				_ = os.WriteFile(in, []byte("x"), 0o600)
				spec.Workspace.Inputs = []sandbox.Input{{LocalPath: in, Path: "in"}}
			}
			f := &fakeRunner{respond: func(args []string, _ string) (string, string, error) {
				step := args[0]
				if step == "shell" && slices.Contains(args, "sudo") {
					step = "configure"
				} else if op, _ := helperOp(args); op != "" {
					step = op
				}
				if step == failAt {
					return "", "boom on " + step, &exitErr{1}
				}
				if step == "list" {
					return "other\n", "", nil
				}
				return "", "", nil
			}}
			b := testBackend(t, "arm64")
			home, _ := b.limaHomeDir()
			_, err := b.create(context.Background(), f, home, spec)
			if err == nil || !strings.Contains(err.Error(), "boom on "+failAt) {
				t.Fatalf("err = %v", err)
			}
			if len(f.callsTo("stop")) != 1 || len(f.callsTo("delete")) != 1 {
				t.Errorf("no cleanup: %q", f.argsOf())
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("dir left behind: %v", err)
			}
		})
	}
}

func TestCreateRejects(t *testing.T) {
	b := testBackend(t, "arm64")
	home, _ := b.limaHomeDir()
	f := &fakeRunner{}
	dir := sandboxDir(t)
	bad := []func(*sandbox.Spec){
		func(s *sandbox.Spec) { s.Mounts = []sandbox.Mount{{HostPath: "/tmp/x", GuestPath: "/data"}} },
		func(s *sandbox.Spec) { s.Profile.Image = "alpine:3" },
		func(s *sandbox.Spec) { s.Dir = "relative" },
		func(s *sandbox.Spec) { s.Dir = "/" },
		func(s *sandbox.Spec) { s.Dir = home },
		func(s *sandbox.Spec) { s.ID = "" },
		func(s *sandbox.Spec) { s.Egress.ProxyURL = "socks5://127.0.0.1:1" },
		func(s *sandbox.Spec) { s.Env = map[string]string{"BAD-NAME": "x"} },
		func(s *sandbox.Spec) { s.Env = map[string]string{"https_proxy": "http://evil:1"} },
	}
	for i, mut := range bad {
		s := testSpec(dir, nil)
		mut(&s)
		if _, err := b.create(context.Background(), f, home, s); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if len(f.argsOf()) != 0 {
		t.Errorf("limactl called for invalid specs: %q", f.argsOf())
	}
}

func TestCreateRefusesOverride(t *testing.T) {
	b := testBackend(t, "arm64")
	home, _ := b.limaHomeDir()
	if err := os.MkdirAll(filepath.Join(home, "_config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "_config", "override.yaml"), []byte("mounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(context.Background(), testSpec(sandboxDir(t), nil)); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("Create with override.yaml: %v", err)
	}
	if ok, why := b.Available(context.Background()); ok || !strings.Contains(why, "override.yaml") {
		t.Errorf("Available with override.yaml: %v %q", ok, why)
	}
}

func TestDestroyIdempotent(t *testing.T) {
	dir := sandboxDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	s := testSandbox(f, dir)
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"stop", "--tty=false", "--force", "sbx-01j0test"},
		{"delete", "--tty=false", "--force", "sbx-01j0test"},
	}
	if !slices.EqualFunc(f.argsOf(), want, slices.Equal[[]string]) {
		t.Errorf("argv %q", f.argsOf())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir left: %v", err)
	}
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.argsOf()) != 2 {
		t.Errorf("second Destroy ran limactl again: %q", f.argsOf())
	}
	if _, err := s.Exec(context.Background(), []string{"true"}, sandbox.ExecOpts{}); !errors.Is(err, errDestroyed) {
		t.Errorf("Exec after Destroy: %v", err)
	}

	// A missing instance counts as deleted; an existing one that fails to
	// delete is an error.
	for _, present := range []bool{false, true} {
		f := &fakeRunner{respond: func(args []string, _ string) (string, string, error) {
			switch args[0] {
			case "stop", "delete":
				return "", "instance not found", &exitErr{1}
			case "list":
				if present {
					return "sbx-01j0test\n", "", nil
				}
				return "", "", nil
			}
			return "", "", nil
		}}
		s := testSandbox(f, sandboxDir(t))
		err := s.Destroy(context.Background())
		if present != (err != nil) {
			t.Errorf("present=%v: err %v", present, err)
		}
	}

	// Never removes a protected directory.
	home, _ := os.UserHomeDir()
	s = testSandbox(&fakeRunner{}, home)
	if err := s.Destroy(context.Background()); err == nil {
		t.Error("Destroy accepted the home directory")
	}
}

// ---- operations ----

func TestOperationArgv(t *testing.T) {
	ctx := context.Background()
	f := &fakeRunner{respond: func(args []string, _ string) (string, string, error) {
		switch op, _ := helperOp(args); op {
		case "glob":
			// Duplicates and hostile entries from the guest.
			return "b/2.txt\x00a/1.txt\x00b/2.txt\x00../etc/passwd\x00/etc/shadow\x00.\x00a/../../x\x00", "", nil
		case "stats":
			return "load 0.50\nmemtotal 2000000\nmemavail 1500000\ndisk 123456\nprocs 7\n", "", nil
		case "copyout":
			return "TARDATA", "", nil
		}
		return "", "", nil
	}}
	s := testSandbox(f, t.TempDir())

	if err := s.CopyIn(ctx, strings.NewReader("data"), "/workspace/x/y z.txt", 0o100755); err != nil {
		t.Fatal(err)
	}
	got, err := s.Glob(ctx, []string{"*.txt", "**/x"})
	if err != nil || !slices.Equal(got, []string{"a/1.txt", "b/2.txt"}) {
		t.Errorf("Glob %q %v", got, err)
	}
	rc, err := s.CopyOut(ctx, "out dir")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil || string(b) != "TARDATA" {
		t.Errorf("CopyOut %q %v", b, err)
	}
	_ = rc.Close()
	if err := s.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.CPUMilli != 500 || st.MemoryBytes != 500000*1024 || st.DiskBytes != 123456 || st.Processes != 7 || st.EgressBytes != 0 || st.RunningSince.IsZero() {
		t.Errorf("Stats %+v %v", st, err)
	}

	calls := f.argsOf()
	if len(calls) != 6 {
		t.Fatalf("calls %q", calls)
	}
	for _, c := range calls {
		if !slices.Equal(c[:len(helperPrefix)], helperPrefix) {
			t.Errorf("prefix of %q", c)
		}
	}
	type opCase struct {
		op   string
		args []string // decoded
	}
	wants := []opCase{
		{"copyin", []string{"x/y z.txt", "755"}},
		{"glob", []string{"*.txt", "**/x"}},
		{"copyout", []string{"out dir"}},
		{"pause", nil},
		{"resume", nil},
		{"stats", nil},
	}
	for i, w := range wants {
		op, args := helperOp(calls[i])
		if op != w.op || len(args) != len(w.args) {
			t.Errorf("call %d: %q", i, calls[i])
			continue
		}
		for j, a := range args {
			if w.op == "copyin" && j == 1 {
				if a != w.args[j] {
					t.Errorf("mode %q", a)
				}
				continue
			}
			if d := decArg(t, a); d != w.args[j] {
				t.Errorf("call %d arg %d: %q, want %q", i, j, d, w.args[j])
			}
		}
	}
	if f.calls[0].stdin != "data" {
		t.Errorf("copy-in stdin %q", f.calls[0].stdin)
	}

	// Rejected before any limactl call.
	n := len(f.argsOf())
	if err := s.CopyIn(ctx, strings.NewReader(""), "../x", 0o644); err == nil {
		t.Error("CopyIn ../x accepted")
	}
	if err := s.CopyIn(ctx, strings.NewReader(""), "/etc/x", 0o644); err == nil {
		t.Error("CopyIn /etc/x accepted")
	}
	if _, err := s.CopyOut(ctx, "/Users/someone"); err == nil {
		t.Error("CopyOut outside accepted")
	}
	if _, err := s.Glob(ctx, []string{"../*"}); err == nil {
		t.Error("Glob ../* accepted")
	}
	if _, err := s.Exec(ctx, []string{"true"}, sandbox.ExecOpts{Dir: "../"}); err == nil {
		t.Error("Exec dir ../ accepted")
	}
	if _, err := s.Exec(ctx, nil, sandbox.ExecOpts{}); err == nil {
		t.Error("empty argv accepted")
	}
	if _, err := s.Exec(ctx, []string{"x"}, sandbox.ExecOpts{Env: map[string]string{"HTTP_PROXY": "x"}}); err == nil {
		t.Error("proxy override accepted")
	}
	if len(f.argsOf()) != n {
		t.Errorf("limactl called for rejected input: %q", f.argsOf()[n:])
	}
}

func TestTransfersUseCallerContext(t *testing.T) {
	var deadlines []time.Duration
	var mu sync.Mutex
	r := &ctxRunner{onRun: func(ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		if d, ok := ctx.Deadline(); ok {
			deadlines = append(deadlines, time.Until(d))
		} else {
			deadlines = append(deadlines, -1)
		}
	}}
	s := testSandbox(r, t.TempDir())
	ctx := context.Background()
	_ = s.CopyIn(ctx, strings.NewReader("x"), "a", 0o600)
	_, _ = s.Glob(ctx, []string{"*"})
	_ = s.Pause(ctx)
	if len(deadlines) != 3 || deadlines[0] != -1 || deadlines[1] != -1 || deadlines[2] <= 0 || deadlines[2] > shortCallTimeout {
		t.Errorf("deadlines %v: transfers must not get the short-call timeout, control calls must", deadlines)
	}
}

type ctxRunner struct {
	fakeRunner
	onRun func(context.Context)
}

func (r *ctxRunner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	r.onRun(ctx)
	return r.fakeRunner.Run(ctx, stdin, args...)
}

func TestCopyOutFailure(t *testing.T) {
	f := &fakeRunner{respond: func([]string, string) (string, string, error) {
		return "", "tar: missing: Cannot stat", &exitErr{2}
	}}
	s := testSandbox(f, t.TempDir())
	rc, err := s.CopyOut(context.Background(), "missing")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	if err == nil || !strings.Contains(err.Error(), "Cannot stat") {
		t.Errorf("err %v", err)
	}
	_ = rc.Close()
}

func decodePayload(t *testing.T, p string) []string {
	t.Helper()
	if !strings.HasSuffix(p, "\x00") {
		t.Fatalf("payload not NUL-terminated: %q", p)
	}
	return strings.Split(strings.TrimSuffix(p, "\x00"), "\x00")
}

func TestExecArgvAndExit(t *testing.T) {
	f := &fakeRunner{respond: func(args []string, stdin string) (string, string, error) {
		if op, _ := helperOp(args); op == "exec" {
			return "out:" + stdin, "err", &exitErr{3}
		}
		return "", "", nil
	}}
	s := testSandbox(f, t.TempDir())
	s.env = map[string]string{"A": "base", "C": "keep"}
	argv := []string{"sh", "-c", `echo "$1" $(id)`, "x y", "*"}
	p, err := s.Exec(context.Background(), argv, sandbox.ExecOpts{Dir: "sub dir", Env: map[string]string{"A": "over ride", "B": "$HOME"}, Stdin: strings.NewReader("in")})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout())
	errOut, _ := io.ReadAll(p.Stderr())
	code, err := p.Wait()
	if code != 3 || err != nil || string(out) != "out:in" || string(errOut) != "err" {
		t.Errorf("code %d err %v out %q stderr %q", code, err, out, errOut)
	}
	calls := f.allCalls()
	if len(calls) != 2 {
		t.Fatalf("calls %q", f.argsOf())
	}
	op0, a0 := helperOp(calls[0].args)
	op1, a1 := helperOp(calls[1].args)
	if op0 != "putreq" || op1 != "exec" || len(a0) != 1 || !slices.Equal(a0, a1) {
		t.Fatalf("calls %q", f.argsOf())
	}
	if id := a0[0]; len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("id %q", id)
	}
	for _, c := range calls {
		if !slices.Equal(c.args[:len(helperPrefix)], helperPrefix) {
			t.Errorf("prefix %q", c.args)
		}
	}
	fields := decodePayload(t, calls[0].stdin)
	want := append([]string{"sub dir", "3", "A=over ride", "B=$HOME", "C=keep"}, argv...)
	if !slices.Equal(fields, want) {
		t.Errorf("payload %q\nwant %q", fields, want)
	}
	// Only shell-inert characters reach limactl.
	for _, c := range calls {
		for _, a := range c.args {
			if strings.ContainsAny(a, " $`'\"\\;|&<>*?(){}~\n") {
				t.Errorf("shell-active argument %q", a)
			}
		}
	}
	if _, err := s.Exec(context.Background(), []string{"x"}, sandbox.ExecOpts{Env: map[string]string{"1BAD": "x"}}); err == nil {
		t.Error("bad env name accepted")
	}
	if _, err := s.Exec(context.Background(), []string{"x\x00y"}, sandbox.ExecOpts{}); err == nil {
		t.Error("NUL in argv accepted")
	}
}

func TestExecSignalAndCancel(t *testing.T) {
	f := &fakeRunner{block: true}
	s := testSandbox(f, t.TempDir())
	lastOp := func() (string, []string) {
		a := f.argsOf()
		return helperOp(a[len(a)-1])
	}

	// Signal goes to the guest process group by id.
	p, err := s.Exec(context.Background(), []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	_, ea := lastOp()
	id := ea[0]
	if err := p.Signal("TERM"); err != nil {
		t.Fatal(err)
	}
	if err := p.Signal("BOGUS"); err == nil {
		t.Error("bogus signal accepted")
	}
	if op, args := lastOp(); op != "signal" || !slices.Equal(args, []string{id, "TERM"}) {
		t.Errorf("signal argv %q", f.argsOf())
	}

	// Cancelling the context kills the group in the guest and the local
	// limactl; Wait reports ErrKilled.
	ctx, cancel := context.WithCancel(context.Background())
	p2, err := s.Exec(ctx, []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := p2.Wait(); !errors.Is(err, sandbox.ErrKilled) {
		t.Errorf("Wait after cancel: %v", err)
	}
	if op, args := lastOp(); op != "signal" || args[1] != "KILL" {
		t.Errorf("no KILL in guest: %q", f.argsOf())
	}

	// Deadline → ErrTimeout; wall clock → ErrTimeout, and up front once past.
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer dcancel()
	p3, err := s.Exec(dctx, []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err == nil {
		if _, err := p3.Wait(); !errors.Is(err, sandbox.ErrTimeout) {
			t.Errorf("Wait after deadline: %v", err)
		}
	}
	s.wall = 50 * time.Millisecond
	s.created = time.Now()
	p4, err := s.Exec(context.Background(), []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p4.Wait(); !errors.Is(err, sandbox.ErrTimeout) {
		t.Errorf("Wait after wall clock: %v", err)
	}
	n := len(f.argsOf())
	if _, err := s.Exec(context.Background(), []string{"true"}, sandbox.ExecOpts{}); !errors.Is(err, sandbox.ErrTimeout) {
		t.Errorf("Exec past the wall clock: %v", err)
	}
	if len(f.argsOf()) != n {
		t.Error("limactl called after the wall clock")
	}
	s.wall = 0

	// Destroy kills what is still running.
	p5, _ := s.Exec(context.Background(), []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p5.Wait(); !errors.Is(err, sandbox.ErrKilled) {
		t.Errorf("Wait after Destroy: %v", err)
	}
	// KILL after Destroy cannot reach the guest: the local limactl dies.
	_ = p.Signal("KILL")
	if _, err := p.Wait(); !errors.Is(err, sandbox.ErrKilled) {
		t.Errorf("Wait after KILL: %v", err)
	}
}

func TestBufPipe(t *testing.T) {
	p := newBufPipe(8, "")
	done := make(chan struct{})
	go func() {
		_, _ = p.Write([]byte("0123456789abcdef"))
		p.CloseWrite()
		close(done)
	}()
	b, err := io.ReadAll(p)
	<-done
	if err != nil || string(b) != "0123456789abcdef" {
		t.Errorf("%q %v", b, err)
	}
	q := newBufPipe(4, "")
	go q.CloseRead()
	if n, err := q.Write([]byte("0123456789")); n != 10 || err != nil {
		t.Errorf("write after CloseRead: %d %v", n, err)
	}

	// With a spill dir the writer never blocks, and order is preserved
	// across memory and file.
	dir := t.TempDir()
	sp := newBufPipe(16, dir)
	var want bytes.Buffer
	for i := range 1000 {
		line := fmt.Sprintf("line %04d\n", i)
		want.WriteString(line)
		if _, err := sp.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if i == 500 { // read some in the middle
			buf := make([]byte, 37)
			n, _ := io.ReadFull(sp, buf)
			want.Next(n)
		}
	}
	sp.CloseWrite()
	got, err := io.ReadAll(sp)
	if err != nil || !bytes.Equal(got, want.Bytes()) {
		t.Errorf("spilled stream differs (%d vs %d bytes, %v)", len(got), want.Len(), err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("spill files left: %v", left)
	}
}

func TestParseStatsTolerant(t *testing.T) {
	st := parseStats([]byte("junk\nload x\nmemtotal 10\nmemavail 20\nprocs 2\n"))
	if st.MemoryBytes != 0 || st.Processes != 2 || st.CPUMilli != 0 {
		t.Errorf("%+v", st)
	}
}

// ---- Prepare ----

func TestPrepareCache(t *testing.T) {
	payload := []byte(strings.Repeat("qcow2!", 10000))
	sum := sha256.Sum256(payload)
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.URL.Path == "/missing.img" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	cache := t.TempDir()
	b := testBackend(t, "arm64")
	b.http = srv.Client()
	b.cacheDir = func() (string, error) { return cache, nil }

	img := limaImage{Location: srv.URL + "/disk.img", Arch: "aarch64", Digest: "sha256:" + hex.EncodeToString(sum[:])}
	var msgs []string
	if err := b.warmCache(context.Background(), img, func(s string) { msgs = append(msgs, s) }); err != nil {
		t.Fatal(err)
	}
	urlSum := sha256.Sum256([]byte(img.Location))
	entry := filepath.Join(cache, "download", "by-url-sha256", hex.EncodeToString(urlSum[:]))
	data, err := os.ReadFile(filepath.Join(entry, "data"))
	if err != nil || string(data) != string(payload) {
		t.Fatalf("data: %v", err)
	}
	if u, _ := os.ReadFile(filepath.Join(entry, "url")); string(u) != img.Location {
		t.Errorf("url file %q", u)
	}
	if d, _ := os.ReadFile(filepath.Join(entry, "sha256.digest")); string(d) != img.Digest {
		t.Errorf("digest file %q", d)
	}
	if len(msgs) == 0 {
		t.Error("no progress")
	}
	// Second call: cached, no download.
	if err := b.warmCache(context.Background(), img, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("hits %d", hits)
	}
	// Cached data without digest record is verified, not downloaded again.
	_ = os.Remove(filepath.Join(entry, "sha256.digest"))
	if err := b.warmCache(context.Background(), img, func(string) {}); err != nil || hits != 1 {
		t.Errorf("verify cached: %v hits %d", err, hits)
	}

	// Digest mismatch: nothing lands in the cache.
	bad := limaImage{Location: srv.URL + "/other.img", Arch: "aarch64", Digest: "sha256:" + strings.Repeat("00", 32)}
	if err := b.warmCache(context.Background(), bad, func(string) {}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("mismatch: %v", err)
	}
	badSum := sha256.Sum256([]byte(bad.Location))
	left, _ := os.ReadDir(filepath.Join(cache, "download", "by-url-sha256", hex.EncodeToString(badSum[:])))
	if len(left) != 0 {
		t.Errorf("files left after mismatch: %v", left)
	}
	missing := limaImage{Location: srv.URL + "/missing.img", Arch: "aarch64", Digest: img.Digest}
	if err := b.warmCache(context.Background(), missing, func(string) {}); err == nil {
		t.Error("404 accepted")
	}
	if err := b.Prepare(context.Background(), "alpine:3", nil); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("Prepare unknown image: %v", err)
	}
}
