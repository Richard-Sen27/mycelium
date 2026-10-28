package bundle

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

const (
	digest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	imageDir = "/st/images/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sid      = "sbx:01J9Z3K7M2N4P6Q8R0S1T2V3W4"
	cid      = "sbx-01J9Z3K7M2N4P6Q8R0S1T2V3W4"
	sdir     = "/st/" + cid
)

var testNow = time.Now()

func init() { killGrace = 200 * time.Millisecond }

func gone(msg string) error { return &ExitError{Code: 1, Stderr: msg} }

func flags2(network string) string {
	return "runsc --root=/st/runsc --network=" + network + " --host-uds=none --host-fifo=none --overlay2=root:dir=" + sdir + "/overlay"
}

// ex is the prefix of a helper run inside the sandbox as the workload user.
var ex = flags2("sandbox") + " exec --cwd=/workspace --user=1000:1000 " + cid + " "

func newTestBackendCfg(f *fakeRunner, cfg Config) *Backend {
	cfg.Runner = f
	cfg.StateDir = "/st/"
	cfg.Firewall = FirewallNFT
	cfg.Now = func() time.Time { return testNow }
	if cfg.Mode == ModeWSL {
		cfg.Name = sandbox.BackendWSL2GVisor
		cfg.Host = WSLHost{}
	} else {
		cfg.Name = sandbox.BackendGVisor
	}
	b := New(cfg)
	b.gcOnce.Do(func() {}) // GC has its own test
	return b
}

func newTestBackend(f *fakeRunner, mode Mode) *Backend {
	return newTestBackendCfg(f, Config{Mode: mode})
}

func TestAvailableLinux(t *testing.T) {
	tests := []struct {
		name    string
		replies map[string]reply
		ok      bool
		reason  string
	}{
		{"all present", nil, true, ""},
		{"runsc missing", map[string]reply{"runsc --version": {err: errors.New("exec: \"runsc\": executable file not found in $PATH")}}, false, "runsc not found"},
		{"ip missing", map[string]reply{"ip -V": {err: failed("ip")}}, false, "ip (iproute2) not found"},
		{"no firewall tool", map[string]reply{"nft": {err: failed("nft")}, "iptables": {err: failed("iptables")}}, false, "neither nft nor iptables"},
		{"docker missing", map[string]reply{"docker --version": {err: failed("docker")}}, false, "docker client not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			for k, v := range tt.replies {
				f.on(k, v)
			}
			b := New(Config{Name: sandbox.BackendGVisor, Runner: f})
			ok, reason := b.Available(context.Background())
			if ok != tt.ok || !strings.Contains(reason, tt.reason) {
				t.Errorf("Available = %v %q", ok, reason)
			}
		})
	}
	// iptables is the fallback when nft is missing.
	f := newFake()
	f.on("nft", reply{err: failed("nft")})
	b := New(Config{Name: sandbox.BackendGVisor, Runner: f})
	if ok, _ := b.Available(context.Background()); !ok || b.firewall != FirewallIPTables {
		t.Errorf("firewall = %q", b.firewall)
	}
}

func TestAvailableWSL(t *testing.T) {
	list := string(utf16le("Ubuntu\r\nmycel-sbx\r\n", true))
	tests := []struct {
		name    string
		replies map[string]reply
		ok      bool
		reason  string
	}{
		{"all present", nil, true, ""},
		{"wsl missing", map[string]reply{"wsl.exe --status": {err: errors.New("executable file not found")}}, false, "WSL is not available"},
		{"distro missing", map[string]reply{"wsl.exe --list --quiet": {stdout: string(utf16le("Ubuntu\r\n", true))}}, false, "mycel-sbx is not installed"},
		{"runsc missing", map[string]reply{"wsl.exe -d mycel-sbx -u root --exec runsc --version": {err: failed("runsc")}}, false, "runsc not found in the WSL distro"},
		{"no firewall", map[string]reply{"wsl.exe -d mycel-sbx -u root --exec nft": {err: failed()}, "wsl.exe -d mycel-sbx -u root --exec iptables": {err: failed()}}, false, "neither nft nor iptables found in the WSL distro"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.on("wsl.exe --list --quiet", reply{stdout: list})
			for k, v := range tt.replies {
				f.on(k, v)
			}
			b := New(Config{Name: sandbox.BackendWSL2GVisor, Mode: ModeWSL, Runner: f})
			ok, reason := b.Available(context.Background())
			if ok != tt.ok || !strings.Contains(reason, tt.reason) {
				t.Errorf("Available = %v %q", ok, reason)
			}
			for _, l := range f.lines() {
				if strings.HasPrefix(l, "docker") {
					t.Errorf("WSL availability ran %s", l)
				}
			}
		})
	}
}

func exportTar(t *testing.T) string {
	return string(mkTar(t, []tarEntry{
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", typ: tar.TypeReg, mode: 0o644, body: "root"},
		{name: "usr/bin/su", typ: tar.TypeReg, mode: 0o4755, body: "elf"},
		{name: "dev/null", typ: tar.TypeChar, mode: 0o666},
	}))
}

func TestPrepare(t *testing.T) {
	ctx := context.Background()
	t.Run("cached", func(t *testing.T) {
		f := newFake()
		f.on("docker image inspect", reply{stdout: digest + "\n"})
		b := newTestBackend(f, ModeLinux)
		if err := b.Prepare(ctx, "debian:bookworm", nil); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"docker image inspect --format={{.Id}} -- debian:bookworm",
			"test -d " + imageDir,
		}
		if got := f.lines(); !reflect.DeepEqual(got, want) {
			t.Errorf("calls:\n%s", strings.Join(got, "\n"))
		}
	})
	t.Run("pull, export, filter, extract", func(t *testing.T) {
		f := newFake()
		f.on("docker image inspect", reply{err: failed("docker")})
		f.on("docker pull", reply{})
		f.on("test -d", reply{err: failed("test")})
		f.on("docker create", reply{stdout: "c0ffee\n"})
		f.on("docker export", reply{stdout: exportTar(t)})
		b := newTestBackend(f, ModeLinux)
		pulled := false
		var progress []string
		err := b.Prepare(ctx, "debian:bookworm", func(s string) {
			progress = append(progress, s)
			if !pulled {
				pulled = true
				f.on("docker image inspect", reply{stdout: digest})
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		got := f.lines()
		if len(got) != 11 {
			t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
		}
		part := strings.TrimPrefix(got[5], "mkdir -p -m 755 -- ")
		if !strings.HasPrefix(part, imageDir+".part.") || len(part) != len(imageDir)+len(".part.")+12 {
			t.Fatalf("part dir %q", part)
		}
		want := []string{
			"docker image inspect --format={{.Id}} -- debian:bookworm",
			"docker pull -- debian:bookworm",
			"docker image inspect --format={{.Id}} -- debian:bookworm",
			"test -d " + imageDir,
			"mkdir -p -m 700 -- /st/images",
			"mkdir -p -m 755 -- " + part,
			"docker create --pull=never --network=none -- debian:bookworm /nonexistent",
			"tar -x --numeric-owner -C " + part + " -f -",
			"docker export -- c0ffee",
			"mv -f -T -- " + part + " " + imageDir,
			"docker rm --force -- c0ffee",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		c, _ := f.find("tar -x --numeric-owner")
		tr := tar.NewReader(strings.NewReader(c.stdin))
		var names []string
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			names = append(names, h.Name)
			if h.Mode&0o6000 != 0 {
				t.Errorf("%s keeps setuid", h.Name)
			}
		}
		if !reflect.DeepEqual(names, []string{"etc/", "etc/passwd", "usr/bin/su"}) {
			t.Errorf("extracted %v", names)
		}
		if p, _ := b.imagePath("debian:bookworm"); p != imageDir {
			t.Errorf("image dir %q", p)
		}
		if len(progress) != 3 || !strings.Contains(progress[2], "1 dropped, 1 setuid/setgid stripped") {
			t.Errorf("progress %v", progress)
		}
	})
	t.Run("export failure removes the part dir", func(t *testing.T) {
		f := newFake()
		f.on("docker image inspect", reply{stdout: digest})
		f.on("test -d", reply{err: failed("test")})
		f.on("docker create", reply{stdout: "c1"})
		f.on("docker export", reply{stdout: "garbage that is not a tar stream"})
		b := newTestBackend(f, ModeLinux)
		if err := b.Prepare(ctx, "debian:bookworm", nil); err == nil {
			t.Fatal("bad export accepted")
		}
		if _, ok := f.find("rm -rf --one-file-system -- " + imageDir + ".part."); !ok {
			t.Errorf("part dir not removed: %v", f.lines())
		}
		if _, ok := f.find("mv "); ok {
			t.Error("bad export renamed into place")
		}
	})
	t.Run("bad digest and image", func(t *testing.T) {
		f := newFake()
		f.on("docker image inspect", reply{stdout: "sha256:../../etc"})
		b := newTestBackend(f, ModeLinux)
		if err := b.Prepare(ctx, "x", nil); err == nil {
			t.Error("bad digest accepted")
		}
		if err := b.Prepare(ctx, "--help", nil); err == nil {
			t.Error("flag-like image accepted")
		}
	})
	t.Run("wsl extracts inside the distro", func(t *testing.T) {
		f := newFake()
		f.on("docker image inspect", reply{stdout: digest})
		f.on("wsl.exe -d mycel-sbx -u root --exec test -d", reply{err: failed("test")})
		f.on("docker create", reply{stdout: "c1"})
		f.on("docker export", reply{stdout: exportTar(t)})
		b := newTestBackend(f, ModeWSL)
		if err := b.Prepare(ctx, "debian:bookworm", nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.find("wsl.exe -d mycel-sbx -u root --exec tar -x --numeric-owner -C " + imageDir + ".part."); !ok {
			t.Errorf("no extraction inside the distro: %v", f.lines())
		}
		if _, ok := f.find("docker export -- c1"); !ok {
			t.Error("docker export not run on the host")
		}
	})
}

func TestContainerID(t *testing.T) {
	for _, ok := range []string{sid, "sbx:01j9z3k7m2n4p6q8r0s1t2v3w4"} {
		got, err := ContainerID(ok)
		if err != nil || got != "sbx-"+strings.TrimPrefix(ok, "sbx:") {
			t.Errorf("ContainerID(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{
		"", "..", ".", "images", "runsc", "sbx:..", "sbx:../../etc/passwd/xxxxxxxxxxxx",
		"sbx:01ABC", "sbx:01J9Z3K7M2N4P6Q8R0S1T2V3WU", "sbx:01J9Z3K7M2N4P6Q8R0S1T2V3W4/", "sbx-01J9Z3K7M2N4P6Q8R0S1T2V3W4",
		"sbx:01J9Z3K7M2N4P6Q8R0S1T2V3W4\n",
	} {
		if _, err := ContainerID(bad); !errors.Is(err, ErrSandboxID) {
			t.Errorf("ContainerID(%q) accepted", bad)
		}
	}
	f := newFake()
	b := newTestBackend(f, ModeLinux)
	for _, bad := range []string{"..", "images", "runsc", "sbx:01ABC"} {
		if _, err := b.Create(context.Background(), sandbox.Spec{ID: bad, Image: "debian:bookworm"}); !errors.Is(err, ErrSandboxID) {
			t.Errorf("Create(%q) = %v", bad, err)
		}
	}
	if len(f.lines()) != 0 {
		t.Errorf("commands for invalid ids: %v", f.lines())
	}
}

func testSpec(t *testing.T, proxy string) sandbox.Spec {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "seed.tar.gz")
	if err := os.WriteFile(tarball, []byte{0x1f, 0x8b, 8, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(filepath.Join(scratch, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	return sandbox.Spec{
		ID:        sid,
		Image:     "debian:bookworm",
		Limits:    sandbox.Limits{CPUs: 1, MemoryBytes: 1 << 30, MaxProcesses: 64, WallClock: time.Hour},
		Env:       map[string]string{"TOKEN": "s3cret"},
		Workspace: sandbox.WorkspaceSeed{Tarball: tarball, Inputs: []sandbox.Input{{LocalPath: input, Path: "in/data.txt"}}},
		Egress:    sandbox.Egress{ProxyURL: proxy, Allow: []string{"pypi.org"}},
		Dir:       scratch,
	}
}

func prepared(f *fakeRunner) {
	f.on("docker image inspect", reply{stdout: digest})
	f.on("wsl.exe -d mycel-sbx -u root --exec ip route show default", reply{stdout: "default via 172.22.16.1 dev eth0 proto kernel\n"})
	f.on("* state "+cid, reply{err: gone("loading container: file does not exist")})
}

func indexOf(t *testing.T, lines []string, prefix string) int {
	t.Helper()
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	t.Errorf("missing call %s", prefix)
	return -1
}

func TestCreateLinux(t *testing.T) {
	f := newFake()
	prepared(f)
	b := newTestBackend(f, ModeLinux)
	spec := testSpec(t, "http://sbx-1:tok@127.0.0.1:3128")
	sb, err := b.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	s := sb.(*Sandbox)
	n := s.net
	if n == nil {
		t.Fatal("no network")
	}
	if s.ProxyURL() != "http://sbx-1:tok@"+n.HostAddr.String()+":3128" {
		t.Errorf("proxy url %s", s.ProxyURL())
	}
	root := flags2("sandbox") + " exec --cwd=/workspace --user=0:0 " + cid + " "
	lines := f.lines()
	order := []string{
		"docker image inspect",
		"mkdir -p -m 0700 -- " + sdir + " " + sdir + "/proc " + sdir + "/overlay",
		"mkdir -p -m 755 -- " + sdir + "/workspace",
		"chown 1000:1000 -- " + sdir + "/workspace",
		"ip -o -4 addr show",
		"nft -f -",
		"ip netns add " + n.Namespace,
		"nft add table inet mycel_" + n.Short,
		"install -m 0600 /dev/null " + sdir + "/config.json",
		"dd of=" + sdir + "/config.json",
		flags2("sandbox") + " run --detach --bundle=" + sdir + " " + cid,
		root + "dd of=/etc/resolv.conf status=none",
		ex + "tar -x -z --no-same-owner --no-same-permissions -C /workspace -f -",
		ex + "mkdir -p -- /workspace/in",
		ex + "dd of=/workspace/in/data.txt bs=1M status=none",
		ex + "chmod 644 -- /workspace/in/data.txt",
	}
	last := -1
	for _, o := range order {
		i := indexOf(t, lines, o)
		if i <= last {
			t.Errorf("%s out of order (%d after %d)", o, i, last)
		}
		last = i
	}
	if _, ok := f.find("tar -x --numeric-owner -f"); ok {
		t.Error("rootfs extracted per sandbox")
	}
	host, _ := f.find("nft -f -")
	if host.stdin != HostRulesNFT(3128) {
		t.Errorf("host rules %q", host.stdin)
	}
	cfgCall, _ := f.find("dd of=" + sdir + "/config.json")
	var cfg OCISpec
	if err := json.Unmarshal([]byte(cfgCall.stdin), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Root.Path != imageDir || cfg.Linux.Namespaces[4].Path != n.NetnsPath() || cfg.Linux.CgroupsPath != "/mycel/"+cid {
		t.Errorf("config root %+v cgroups %q", cfg.Root, cfg.Linux.CgroupsPath)
	}
	if cfg.Process.User != (User{UID: 1000, GID: 1000}) {
		t.Errorf("user %+v", cfg.Process.User)
	}
	if !contains(cfg.Process.Env, "HTTPS_PROXY="+s.ProxyURL()) || !contains(cfg.Process.Env, "TOKEN=s3cret") {
		t.Errorf("env %v", cfg.Process.Env)
	}
	if c, _ := f.find(root + "dd of=/etc/resolv.conf"); strings.Contains(c.stdin, "nameserver") {
		t.Errorf("resolv.conf has a nameserver: %q", c.stdin)
	}
	if c, _ := f.find(ex + "dd of=/workspace/in/data.txt"); c.stdin != "input" {
		t.Errorf("input %q", c.stdin)
	}
	for _, l := range lines {
		if strings.Contains(l, "s3cret") {
			t.Errorf("secret in argv: %s", l)
		}
		if strings.Contains(l, "docker.sock") {
			t.Errorf("docker socket in argv: %s", l)
		}
		if strings.Contains(l, "dport 53") {
			t.Errorf("DNS rule without AllowDNS: %s", l)
		}
	}
	if s.timer == nil {
		t.Error("no wall-clock timer")
	}
	if _, err := b.Create(context.Background(), spec); err == nil {
		t.Error("duplicate sandbox id accepted")
	}
}

func TestCreateAllowDNS(t *testing.T) {
	f := newFake()
	prepared(f)
	b := newTestBackendCfg(f, Config{AllowDNS: true})
	sb, err := b.Create(context.Background(), testSpec(t, "http://a:b@127.0.0.1:3128"))
	if err != nil {
		t.Fatal(err)
	}
	n := sb.(*Sandbox).net
	c, _ := f.find(flags2("sandbox") + " exec --cwd=/workspace --user=0:0 " + cid + " dd of=/etc/resolv.conf")
	if c.stdin != "nameserver "+n.HostAddr.String()+"\n" {
		t.Errorf("resolv.conf %q", c.stdin)
	}
	if _, ok := f.find("nft add rule inet mycel_" + n.Short + " input iifname " + n.HostIf + " ip saddr " + n.PeerAddr.String() + " ip daddr " + n.HostAddr.String() + " udp dport 53 accept"); !ok {
		t.Error("no DNS rule with AllowDNS")
	}
}

func TestCreateSkipsAssignedSlot(t *testing.T) {
	first, _ := NewSlotAllocator(DefaultPool).Get(sid, nil)
	taken, _ := NewNet(sid, DefaultPool, first)
	f := newFake()
	prepared(f)
	f.on("ip -o -4 addr show", reply{stdout: "7: eth1    inet " + taken.HostAddr.String() + "/30 scope global eth1\n"})
	b := newTestBackend(f, ModeLinux)
	sb, err := b.Create(context.Background(), testSpec(t, "http://a:b@127.0.0.1:3128"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sb.(*Sandbox).net.HostAddr; got == taken.HostAddr {
		t.Errorf("assigned address %s reused", got)
	}
}

func TestCreateNoEgress(t *testing.T) {
	f := newFake()
	prepared(f)
	b := newTestBackend(f, ModeLinux)
	spec := testSpec(t, "")
	spec.Workspace = sandbox.WorkspaceSeed{}
	sb, err := b.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range f.lines() {
		if strings.HasPrefix(l, "ip ") || strings.HasPrefix(l, "nft") || strings.Contains(l, "--network=sandbox") {
			t.Errorf("network command without egress: %s", l)
		}
	}
	if _, ok := f.find(flags2("none") + " run --detach"); !ok {
		t.Error("runsc run without --network=none")
	}
	if sb.(*Sandbox).ProxyURL() != "" {
		t.Error("proxy URL without egress")
	}
}

func TestCreateWSL(t *testing.T) {
	f := newFake()
	prepared(f)
	b := newTestBackend(f, ModeWSL)
	spec := testSpec(t, "http://sbx-1:tok@127.0.0.1:41000")
	sb, err := b.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	s := sb.(*Sandbox)
	if s.ProxyURL() != "http://sbx-1:tok@172.22.16.1:41000" { // #nosec G101 -- test credential
		t.Errorf("proxy url %s", s.ProxyURL())
	}
	const w = "wsl.exe -d mycel-sbx -u root --exec "
	for _, l := range f.lines() {
		if !strings.HasPrefix(l, w) && !strings.HasPrefix(l, "docker ") {
			t.Errorf("command outside the distro: %s", l)
		}
	}
	tbl := "mycel_" + s.net.Short
	if _, ok := f.find(w + "nft add rule inet " + tbl + " forward iifname " + s.net.HostIf + " ip saddr " + s.net.PeerAddr.String() + " ip daddr 172.22.16.1 tcp dport 41000 accept"); !ok {
		t.Errorf("no forward rule to the Windows proxy:\n%s", strings.Join(f.lines(), "\n"))
	}
	host, _ := f.find(w + "nft -f -")
	if host.stdin != HostRulesNFT(0) {
		t.Errorf("WSL host rules guard a local port: %q", host.stdin)
	}
	run, ok := f.find(w + "sh -s -- " + sdir + "/runsc-run.log " + flags2("sandbox") + " run --detach --bundle=" + sdir + " " + cid)
	if !ok || run.stdin != DetachScript {
		t.Errorf("runsc run not detached through setsid: %+v", run)
	}
	if _, err := s.Exec(context.Background(), []string{"sh"}, sandbox.ExecOpts{TTY: true}); !errors.Is(err, ErrTTY) {
		t.Errorf("TTY exec: %v", err)
	}
}

func TestCreateWSLRefusesQuotes(t *testing.T) {
	f := newFake()
	prepared(f)
	b := newTestBackend(f, ModeWSL)
	b.cfg.StateDir = `/st/"x`
	if _, err := b.Create(context.Background(), testSpec(t, "")); !errors.Is(err, ErrWSLArg) {
		t.Errorf("quote in path: %v", err)
	}
	for _, l := range f.lines() {
		if strings.Contains(l, `"`) {
			t.Errorf("quoted argument reached wsl.exe: %s", l)
		}
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	f := newFake()
	prepared(f)
	f.on(flags2("sandbox")+" run", reply{err: failed("runsc")})
	f.on("* kill --all", reply{err: gone("container does not exist")})
	f.on("* delete --force", reply{err: gone("container does not exist")})
	b := newTestBackend(f, ModeLinux)
	spec := testSpec(t, "http://a:b@127.0.0.1:3128")
	if _, err := b.Create(context.Background(), spec); err == nil {
		t.Fatal("create succeeded")
	}
	lines := f.lines()
	if lines[len(lines)-1] != "rm -rf --one-file-system -- "+sdir {
		t.Errorf("last call %s", lines[len(lines)-1])
	}
	if _, ok := f.find("ip netns del mycel-"); !ok {
		t.Error("namespace not removed")
	}
	if len(b.slots.used) != 0 || len(b.live) != 0 {
		t.Error("slot or live entry kept")
	}
	if _, err := b.Create(context.Background(), sandbox.Spec{ID: sid, Image: "i", Mounts: []sandbox.Mount{{HostPath: "/d", GuestPath: "/d"}}}); err == nil {
		t.Error("mount grant accepted")
	}
}

func newSandbox(t *testing.T, f *fakeRunner) *Sandbox {
	t.Helper()
	prepared(f)
	b := newTestBackend(f, ModeLinux)
	sb, err := b.Create(context.Background(), testSpec(t, "http://sbx:tok@127.0.0.1:3128"))
	if err != nil {
		t.Fatal(err)
	}
	f.reset()
	return sb.(*Sandbox)
}

func TestDestroy(t *testing.T) {
	f := newFake()
	s := newSandbox(t, f)
	scratch := s.spec.Dir
	n := s.net
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Destroy ignores the caller's cancellation
	if err := s.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		flags2("sandbox") + " kill --all " + cid + " KILL",
		flags2("sandbox") + " delete --force " + cid,
		flags2("sandbox") + " state " + cid,
		"nft delete table inet mycel_" + n.Short,
		"ip link del " + n.HostIf,
		"ip netns del " + n.Namespace,
		"rm -rf --one-file-system -- " + sdir,
	}
	if got := f.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("destroy:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Errorf("Spec.Dir not emptied: %v", entries)
	}
	if len(s.b.live) != 0 || len(s.b.slots.used) != 0 {
		t.Error("live entry or slot kept")
	}
	f.reset()
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.lines()) != 0 {
		t.Errorf("second destroy ran %v", f.lines())
	}
	if _, err := s.Exec(context.Background(), []string{"true"}, sandbox.ExecOpts{}); !errors.Is(err, sandbox.ErrKilled) {
		t.Errorf("exec after destroy: %v", err)
	}
}

func TestDestroyIgnoresOnlyNotExist(t *testing.T) {
	t.Run("already gone", func(t *testing.T) {
		f := newFake()
		s := newSandbox(t, f)
		f.on("runsc", reply{err: gone("container does not exist")})
		f.on("nft", reply{err: gone("Error: No such file or directory; did you mean table 'x'?")})
		f.on("ip link", reply{err: gone(`Cannot find device "myhx"`)})
		f.on("ip netns", reply{err: gone("Cannot remove namespace file: No such file or directory")})
		if err := s.Destroy(context.Background()); err != nil {
			t.Fatalf("not-exist errors reported: %v", err)
		}
	})
	t.Run("real failure is reported and retried", func(t *testing.T) {
		f := newFake()
		s := newSandbox(t, f)
		f.on("* delete --force", reply{err: gone("permission denied")})
		if err := s.Destroy(context.Background()); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("failure not reported: %v", err)
		}
		if s.gone || len(s.b.live) != 1 {
			t.Error("marked gone after a failure")
		}
		f.on("* delete --force", reply{})
		f.reset()
		if err := s.Destroy(context.Background()); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if len(f.lines()) != 7 {
			t.Errorf("retry calls %v", f.lines())
		}
	})
	t.Run("container still present", func(t *testing.T) {
		f := newFake()
		s := newSandbox(t, f)
		f.on("* state "+cid, reply{stdout: `{"status":"running"}`})
		if err := s.Destroy(context.Background()); err == nil || !strings.Contains(err.Error(), "still present") {
			t.Fatalf("lingering container not reported: %v", err)
		}
	})
	t.Run("network teardown failure keeps the slot", func(t *testing.T) {
		f := newFake()
		s := newSandbox(t, f)
		f.on("ip link del", reply{err: gone("Operation not permitted")})
		if err := s.Destroy(context.Background()); err == nil {
			t.Fatal("teardown failure not reported")
		}
		if len(s.b.slots.used) != 1 {
			t.Error("slot released although the link is still there")
		}
	})
}

func TestGC(t *testing.T) {
	const deadULID = "01J9Z3K7M2N4P6Q8R0S1T2V3W5"
	deadCID := "sbx-" + deadULID
	live, dead := ShortID(sid), ShortID("sbx:"+deadULID)
	f := newFake()
	b := New(Config{Name: sandbox.BackendGVisor, Runner: f, StateDir: "/st"})
	b.setLive(cid, true)
	rs := "runsc --root=/st/runsc --network=none --host-uds=none --host-fifo=none --overlay2=root:self"
	f.on(rs+" list --quiet", reply{stdout: cid + "\n" + deadCID + "\nsomething-else\n"})
	f.on("nft list tables", reply{stdout: "table inet mycel\ntable inet mycel_" + live + "\ntable inet mycel_" + dead + "\n"})
	f.on("iptables -w -S", reply{stdout: "-N MYCEL-HOST\n-N MYCEL-" + dead + "\n-N MYCEL-" + live + "\n-A INPUT -i myh" + dead + " -j MYCEL-" + dead + "\n-A INPUT -i myh" + live + " -j MYCEL-" + live + "\n"})
	f.on("iptables -w -t nat -S", reply{stdout: "-A POSTROUTING -s 10.237.0.2/32 ! -o myh" + dead + " -j MASQUERADE\n"})
	f.on("ip6tables -w -S", reply{stdout: "-A INPUT -i myh" + live + " -j DROP\n"})
	f.on("ip -o link show", reply{stdout: "5: myh" + dead + "@if4: <UP>\n6: myh" + live + "@if5: <UP>\n"})
	f.on("ip netns list", reply{stdout: "mycel-" + dead + " (id: 1)\nmycel-" + live + " (id: 2)\n"})
	if err := b.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mutations []string
	for _, l := range f.lines() {
		if strings.Contains(l, " list") || strings.HasSuffix(l, " -S") || strings.Contains(l, "show") {
			continue
		}
		mutations = append(mutations, l)
	}
	want := []string{
		rs + " kill --all " + deadCID + " KILL",
		rs + " delete --force " + deadCID,
		"rm -rf --one-file-system -- /st/" + deadCID,
		"nft delete table inet mycel_" + dead,
		"iptables -w -t nat -D POSTROUTING -s 10.237.0.2/32 ! -o myh" + dead + " -j MASQUERADE",
		"iptables -w -D INPUT -i myh" + dead + " -j MYCEL-" + dead,
		"iptables -w -F MYCEL-" + dead,
		"iptables -w -X MYCEL-" + dead,
		"ip link del myh" + dead,
		"ip netns del mycel-" + dead,
	}
	if !reflect.DeepEqual(mutations, want) {
		t.Errorf("gc:\n%s\nwant:\n%s", strings.Join(mutations, "\n"), strings.Join(want, "\n"))
	}
	// GC runs once, before the first Create.
	f2 := newFake()
	prepared(f2)
	b2 := New(Config{Name: sandbox.BackendGVisor, Runner: f2, StateDir: "/st", Firewall: FirewallNFT})
	if _, err := b2.Create(context.Background(), testSpec(t, "")); err != nil {
		t.Fatal(err)
	}
	if i := indexOf(t, f2.lines(), "runsc --root=/st/runsc --network=none --host-uds=none --host-fifo=none --overlay2=root:self list --quiet"); i != 0 {
		t.Errorf("gc not first: %d", i)
	}
}

func TestCopyInOutGlob(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := newSandbox(t, f)

	if err := s.CopyIn(ctx, strings.NewReader("data"), "a/b.txt", 0o4755); err != nil {
		t.Fatal(err)
	}
	want := []string{ex + "mkdir -p -- /workspace/a", ex + "dd of=/workspace/a/b.txt bs=1M status=none", ex + "chmod 755 -- /workspace/a/b.txt"}
	if got := f.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("copy in:\n%s", strings.Join(got, "\n"))
	}
	for _, bad := range []string{"/etc/passwd", "../x", "a/../../x", ""} {
		if err := s.CopyIn(ctx, strings.NewReader(""), bad, 0o644); !errors.Is(err, ErrPath) {
			t.Errorf("CopyIn(%q) = %v", bad, err)
		}
		if _, err := s.CopyOut(ctx, bad); bad != "" && !errors.Is(err, ErrPath) {
			t.Errorf("CopyOut(%q) = %v", bad, err)
		}
		if _, err := s.Glob(ctx, []string{bad}); !errors.Is(err, ErrPath) {
			t.Errorf("Glob(%q) = %v", bad, err)
		}
	}

	f.reset()
	f.on(ex+"stat -L -c %F -- /workspace/out.txt", reply{stdout: "regular file\n"})
	f.on(ex+"cat -- /workspace/out.txt", reply{stdout: "result"})
	rc, err := s.CopyOut(ctx, "out.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(b) != "result" {
		t.Errorf("copy out %q %v", b, err)
	}

	f.on(ex+"stat -L -c %F -- /workspace/dist", reply{stdout: "directory\n"})
	f.on(ex+"tar -c -f - -C /workspace -- dist", reply{stdout: "TAR"})
	rc, err = s.CopyOut(ctx, "dist")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "TAR" {
		t.Errorf("dir copy out %q", b)
	}
	f.on(ex+"stat -L -c %F -- /workspace", reply{stdout: "directory\n"})
	if rc, err := s.CopyOut(ctx, "."); err != nil {
		t.Errorf("whole workspace: %v", err)
	} else {
		_, _ = io.ReadAll(rc)
		_ = rc.Close()
	}
	if _, ok := f.find(ex + "tar -c -f - -C / -- workspace"); !ok {
		t.Error("whole workspace tar argv")
	}
	f.on(ex+"stat -L -c %F -- /workspace/fifo", reply{stdout: "fifo\n"})
	if _, err := s.CopyOut(ctx, "fifo"); err == nil {
		t.Error("fifo copied out")
	}

	f.on(ex+"find . -type f -print0", reply{stdout: "./a.txt\x00./dist/x.whl\x00"})
	got, err := s.Glob(ctx, []string{"dist/*.whl"})
	if err != nil || !reflect.DeepEqual(got, []string{"dist/x.whl"}) {
		t.Errorf("glob %v %v", got, err)
	}
}

func TestPauseResumeStats(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := newSandbox(t, f)
	f.on(flags2("sandbox")+" events --stats "+cid, reply{stdout: `{"type":"stats","id":"x","data":{"cpu":{"usage":{"total":1500000000}},"memory":{"usage":{"usage":2048}},"pids":{"current":3}}}`})
	f.on("du -s -B1 -x -- "+sdir, reply{stdout: "8192\t" + sdir + "\n"})
	if err := s.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := sandbox.Stats{CPUMilli: 1500, MemoryBytes: 2048, DiskBytes: 8192, Processes: 3, RunningSince: testNow}
	if st != want {
		t.Errorf("stats %+v", st)
	}
	wantCalls := []string{flags2("sandbox") + " pause " + cid, flags2("sandbox") + " resume " + cid}
	if got := f.lines()[:2]; !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("calls %v", got)
	}
}

const psTree = `[{"uid":1000,"pid":17,"ppid":0,"cmd":"make"},{"uid":1000,"pid":18,"ppid":17,"cmd":"sh"},{"uid":1000,"pid":19,"ppid":18,"cmd":"cc"},{"uid":1000,"pid":1,"ppid":0,"cmd":"sleep"},{"uid":1000,"pid":20,"ppid":1,"cmd":"other"}]`

func TestTree(t *testing.T) {
	ps, err := ParsePS([]byte(psTree))
	if err != nil {
		t.Fatal(err)
	}
	if got := Tree(ps, 17); !reflect.DeepEqual(got, []int{19, 18, 17}) {
		t.Errorf("tree %v", got)
	}
	if got := Tree(ps, 99); !reflect.DeepEqual(got, []int{99}) {
		t.Errorf("unknown pid %v", got)
	}
	if _, err := ParsePS([]byte("nope")); err == nil {
		t.Error("bad ps accepted")
	}
}

func killed(f *fakeRunner, sig string) []string {
	var out []string
	for _, l := range f.lines() {
		if i := strings.Index(l, " kill --pid="); i >= 0 && strings.HasSuffix(l, sig) {
			out = append(out, strings.TrimPrefix(strings.Fields(l[i:])[1], "--pid="))
		}
	}
	return out
}

func TestExec(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := newSandbox(t, f)
	exe := flags2("sandbox") + " exec --process=" + sdir + "/proc/1.json --internal-pid-file=" + sdir + "/proc/1.pid " + cid
	release := make(chan struct{})
	f.on(exe, reply{stdout: "out", err: &ExitError{Code: 3}, block: release})
	f.on("cat -- "+sdir+"/proc/1.pid", reply{stdout: "17\n"})
	f.on("* ps --format=json", reply{stdout: psTree})

	p, err := s.Exec(ctx, []string{"make", "test"}, sandbox.ExecOpts{Dir: "src", Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := f.lines()
	if indexOf(t, lines, "install -m 0600 /dev/null "+sdir+"/proc/1.json") >= indexOf(t, lines, "dd of="+sdir+"/proc/1.json") {
		t.Error("process file written before it was made private")
	}
	pc, _ := f.find("dd of=" + sdir + "/proc/1.json")
	var ps Process
	if err := json.Unmarshal([]byte(pc.stdin), &ps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ps.Args, []string{"make", "test"}) || ps.Cwd != "/workspace/src" || !ps.NoNewPrivileges || ps.User.UID != 1000 {
		t.Errorf("process %+v", ps)
	}
	if !contains(ps.Env, "X=1") || !contains(ps.Env, "TOKEN=s3cret") || !contains(ps.Env, "HTTP_PROXY="+s.ProxyURL()) || !contains(ps.Env, "HOME=/tmp") {
		t.Errorf("env %v", ps.Env)
	}
	if err := p.Signal("TERM"); err != nil {
		t.Fatal(err)
	}
	if got := killed(f, "SIGTERM"); !reflect.DeepEqual(got, []string{"17"}) {
		t.Errorf("TERM went to %v", got)
	}
	if err := p.Signal("KILL"); err != nil {
		t.Fatal(err)
	}
	if got := killed(f, "SIGKILL"); !reflect.DeepEqual(got, []string{"19", "18", "17", "19", "18", "17"}) {
		t.Errorf("KILL went to %v", got)
	}
	if err := p.Signal("SEGV"); err == nil {
		t.Error("bad signal accepted")
	}
	close(release)
	out, _ := io.ReadAll(p.Stdout())
	code, err := p.Wait()
	if code != 3 || err != nil || string(out) != "out" {
		t.Errorf("wait %d %v %q", code, err, out)
	}
	if _, ok := f.find("rm -f -- " + sdir + "/proc/1.json"); !ok {
		t.Error("process file not removed")
	}
	if _, err := s.Exec(ctx, []string{"x"}, sandbox.ExecOpts{Dir: "../.."}); !errors.Is(err, ErrPath) {
		t.Errorf("escaping dir: %v", err)
	}
	if _, err := s.Exec(ctx, nil, sandbox.ExecOpts{}); err == nil {
		t.Error("empty argv accepted")
	}
	if _, err := s.Exec(ctx, []string{"sh"}, sandbox.ExecOpts{TTY: true}); !errors.Is(err, ErrTTY) {
		t.Errorf("TTY: %v", err)
	}
}

func TestExecCancelKillsTree(t *testing.T) {
	f := newFake()
	s := newSandbox(t, f)
	f.on(flags2("sandbox")+" exec --process=", reply{err: &ExitError{Code: 137}, block: make(chan struct{})})
	f.on("cat -- "+sdir+"/proc/", reply{stdout: "17"})
	f.on("* ps --format=json", reply{stdout: psTree})

	ctx, cancel := context.WithCancel(context.Background())
	p, err := s.Exec(ctx, []string{"make"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	code, err := waitResult(t, p)
	if !errors.Is(err, sandbox.ErrKilled) || code != 137 {
		t.Errorf("cancel: %d %v", code, err)
	}
	if got := killed(f, "SIGKILL"); !reflect.DeepEqual(got, []string{"19", "18", "17", "19", "18", "17"}) {
		t.Errorf("cancel killed %v", got)
	}
}

func TestWallClock(t *testing.T) {
	f := newFake()
	s := newSandbox(t, f)
	release := make(chan struct{})
	f.on(flags2("sandbox")+" exec --process=", reply{err: &ExitError{Code: 137}, block: release})
	p, err := s.Exec(context.Background(), []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s.expire()
	if _, ok := f.find(flags2("sandbox") + " kill --all " + cid + " KILL"); !ok {
		t.Error("wall clock did not kill the sandbox")
	}
	close(release)
	if _, err := waitResult(t, p); !errors.Is(err, sandbox.ErrTimeout) {
		t.Errorf("wall clock: %v", err)
	}
	if _, err := s.Exec(context.Background(), []string{"true"}, sandbox.ExecOpts{}); !errors.Is(err, sandbox.ErrTimeout) {
		t.Errorf("exec after wall clock: %v", err)
	}
	if err := s.Destroy(context.Background()); err != nil {
		t.Errorf("destroy after wall clock: %v", err)
	}
}

func TestExecKilledByDestroy(t *testing.T) {
	f := newFake()
	s := newSandbox(t, f)
	release := make(chan struct{})
	f.on(flags2("sandbox")+" exec --process=", reply{err: &ExitError{Code: 137}, block: release})
	p, err := s.Exec(context.Background(), []string{"sleep", "100"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := p.Wait(); !errors.Is(err, sandbox.ErrKilled) {
		t.Errorf("wait after destroy: %v", err)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func waitResult(t *testing.T, p sandbox.Process) (int, error) {
	t.Helper()
	type res struct {
		code int
		err  error
	}
	ch := make(chan res, 1)
	go func() { c, e := p.Wait(); ch <- res{c, e} }()
	select {
	case r := <-ch:
		return r.code, r.err
	case <-time.After(killGrace + 5*time.Second):
		t.Fatal("wait did not return")
		return 0, nil
	}
}
