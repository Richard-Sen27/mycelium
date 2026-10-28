//go:build fakesandbox

package fake

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/internal/sbxtest"
)

func TestMain(m *testing.M) {
	sbxtest.MaybeRun()
	os.Exit(m.Run())
}

type entry struct {
	name, body, link string
	typ              byte
}

func writeTarball(t *testing.T, gz bool, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o640, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		if typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(t.TempDir(), "seed.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func create(t *testing.T, spec sandbox.Spec) (*Sandbox, error) {
	t.Helper()
	t.Setenv(EnvOK, "1")
	if spec.Dir == "" {
		spec.Dir = filepath.Join(t.TempDir(), "sbx")
	}
	if spec.Env == nil {
		spec.Env = sbxtest.Env()
	}
	sb, err := Backend{}.Create(context.Background(), spec)
	if err != nil {
		return nil, err
	}
	s := sb.(*Sandbox)
	t.Cleanup(func() { _ = s.Destroy(context.Background()) })
	return s, nil
}

func mustCreate(t *testing.T, spec sandbox.Spec) *Sandbox {
	t.Helper()
	s, err := create(t, spec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func run(t *testing.T, s *Sandbox, opts sandbox.ExecOpts, mode string, args ...string) (string, string, int, error) {
	t.Helper()
	p, err := s.Exec(context.Background(), sbxtest.Argv(mode, args...), opts)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&errb, p.Stderr()); close(done) }()
	_, _ = io.Copy(&out, p.Stdout())
	<-done
	code, err := p.Wait()
	return out.String(), errb.String(), code, err
}

func readWS(t *testing.T, s *Sandbox, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.WorkspaceDir(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRefusesWithoutEnv(t *testing.T) {
	t.Setenv(EnvOK, "")
	ok, why := Backend{}.Available(context.Background())
	if ok || !strings.Contains(why, EnvOK) {
		t.Fatalf("Available = %v, %q", ok, why)
	}
	if _, err := (Backend{}).Create(context.Background(), sandbox.Spec{Dir: t.TempDir()}); !errors.Is(err, ErrRefused) {
		t.Fatalf("Create = %v", err)
	}
	if err := (Backend{}).Prepare(context.Background(), "x", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("Prepare = %v", err)
	}
	t.Setenv(EnvOK, "1")
	if ok, _ := (Backend{}).Available(context.Background()); !ok {
		t.Fatal("not available with env")
	}
	if c := (Backend{}).Capabilities(); c.Isolation != "none" || !c.Egress || c.NestedDocker || c.ProcessLimits || c.MemoryLimit || c.CPULimit {
		t.Fatalf("Capabilities = %+v", c)
	}
}

func TestRegistered(t *testing.T) {
	if !slices.ContainsFunc(sandbox.Backends(), func(b sandbox.Backend) bool { return b.Name() == sandbox.BackendFake }) {
		t.Fatal("fake not registered")
	}
}

func TestSeedFromTarballAndInputs(t *testing.T) {
	for _, gz := range []bool{false, true} {
		tb := writeTarball(t, gz,
			entry{name: "./", typ: tar.TypeDir},
			entry{name: "src/", typ: tar.TypeDir},
			entry{name: "src/main.go", body: "package main"},
			entry{name: "README", body: "hi"},
			entry{name: "link", link: "src/main.go", typ: tar.TypeSymlink},
			entry{name: "hard", link: "README", typ: tar.TypeLink},
		)
		in := filepath.Join(t.TempDir(), "data.csv")
		if err := os.WriteFile(in, []byte("a,b"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := mustCreate(t, sandbox.Spec{Workspace: sandbox.WorkspaceSeed{
			Tarball: tb,
			Inputs:  []sandbox.Input{{LocalPath: in, Path: "inputs/data.csv"}},
		}})
		if got := readWS(t, s, "src/main.go"); got != "package main" {
			t.Fatalf("gz=%v main.go = %q", gz, got)
		}
		if got := readWS(t, s, "hard"); got != "hi" {
			t.Fatalf("hard = %q", got)
		}
		if got := readWS(t, s, "inputs/data.csv"); got != "a,b" {
			t.Fatalf("input = %q", got)
		}
		for _, d := range []string{"workspace", "home", "tmp"} {
			if st, err := os.Stat(filepath.Join(s.dir, d)); err != nil || !st.IsDir() {
				t.Fatalf("%s: %v", d, err)
			}
		}
	}
}

func TestMaliciousTarballs(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]entry{
		"absolute":        {{name: "/tmp/evil", body: "x"}},
		"dotdot":          {{name: "../evil", body: "x"}},
		"inner dotdot":    {{name: "a/../../evil", body: "x"}},
		"abs symlink":     {{name: "l", link: "/etc", typ: tar.TypeSymlink}},
		"escape symlink":  {{name: "a/l", link: "../../x", typ: tar.TypeSymlink}},
		"abs hardlink":    {{name: "h", link: "/etc/passwd", typ: tar.TypeLink}},
		"escape hardlink": {{name: "h", link: "../x", typ: tar.TypeLink}},
		"through symlink": {
			{name: "l", link: ".", typ: tar.TypeSymlink},
			{name: "l/../../evil", body: "x"},
		},
		"dotdot inside symlink target": {{name: "a/l", link: "b/../c", typ: tar.TypeSymlink}},
		"dotdot inside hardlink target": {
			{name: "f", body: "x"},
			{name: "h", link: "a/../f", typ: tar.TypeLink},
		},
		"file below symlink": {
			{name: "sub/", typ: tar.TypeDir},
			{name: "l", link: "sub", typ: tar.TypeSymlink},
			{name: "l/f", body: "x"},
		},
		"symlink chain": {
			{name: "d", link: ".", typ: tar.TypeSymlink},
			{name: "d/x", link: "y", typ: tar.TypeSymlink},
		},
		"overwrite symlink": {
			{name: "f", body: "x"},
			{name: "l", link: "f", typ: tar.TypeSymlink},
			{name: "l", body: "y"},
		},
		"hardlink through symlink": {
			{name: "sub/f", body: "x"},
			{name: "l", link: "sub", typ: tar.TypeSymlink},
			{name: "h", link: "l/f", typ: tar.TypeLink},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			tb := writeTarball(t, false, entries...)
			dir := filepath.Join(t.TempDir(), "sbx")
			_, err := create(t, sandbox.Spec{Dir: dir, Workspace: sandbox.WorkspaceSeed{Tarball: tb}})
			if err == nil {
				t.Fatal("malicious tarball accepted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("failed Create left %s behind: %v", dir, err)
			}
		})
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("something was written outside: %v", ents)
	}
}

func TestTarballDiskLimit(t *testing.T) {
	tb := writeTarball(t, true, entry{name: "big", body: strings.Repeat("x", 2000)})
	_, err := create(t, sandbox.Spec{Limits: sandbox.Limits{DiskBytes: 1000}, Workspace: sandbox.WorkspaceSeed{Tarball: tb}})
	if err == nil || !strings.Contains(err.Error(), "disk limit") {
		t.Fatalf("Create = %v", err)
	}
	// Directories and empty files count too.
	var many []entry
	for i := range 20 {
		many = append(many, entry{name: fmt.Sprintf("d%d/", i), typ: tar.TypeDir}, entry{name: fmt.Sprintf("e%d", i)})
	}
	tb = writeTarball(t, false, many...)
	_, err = create(t, sandbox.Spec{Limits: sandbox.Limits{DiskBytes: 10_000}, Workspace: sandbox.WorkspaceSeed{Tarball: tb}})
	if !errors.Is(err, sandbox.ErrLimit) {
		t.Fatalf("Create with many empty entries = %v", err)
	}
}

func TestCopyInDiskLimit(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{Limits: sandbox.Limits{DiskBytes: 100}})
	if err := s.CopyIn(context.Background(), strings.NewReader(strings.Repeat("x", 60)), "a", 0o644); err != nil {
		t.Fatal(err)
	}
	err := s.CopyIn(context.Background(), strings.NewReader(strings.Repeat("x", 60)), "b", 0o644)
	if !errors.Is(err, sandbox.ErrLimit) {
		t.Fatalf("CopyIn over limit = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.WorkspaceDir(), "b")); !os.IsNotExist(err) {
		t.Fatal("partial file left behind")
	}
	in := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(in, make([]byte, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = create(t, sandbox.Spec{Limits: sandbox.Limits{DiskBytes: 100}, Workspace: sandbox.WorkspaceSeed{Inputs: []sandbox.Input{{LocalPath: in, Path: "big"}}}})
	if !errors.Is(err, sandbox.ErrLimit) {
		t.Fatalf("input over limit = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.CopyIn(ctx, strings.NewReader("x"), "c", 0o644); !errors.Is(err, context.Canceled) {
		t.Fatalf("CopyIn with cancelled ctx = %v", err)
	}
}

func TestCreateDir(t *testing.T) {
	t.Setenv(EnvOK, "1")
	// A non-empty Dir is refused and left alone.
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Backend{}).Create(context.Background(), sandbox.Spec{Dir: dir}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Create in non-empty Dir = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("refused Create touched Dir")
	}
	// An existing empty Dir is used; Destroy removes only what Create made.
	dir = t.TempDir()
	s := mustCreate(t, sandbox.Spec{Dir: dir})
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 0 {
		t.Fatalf("Dir after Destroy: %v, %v", ents, err)
	}
}

const testProxyURL = "http://sbx:tok@127.0.0.1:3128" //nolint:gosec // G101: fake test credential

func TestScrubbedEnv(t *testing.T) {
	t.Setenv("MYCEL_SECRET", "s3cret")
	t.Setenv("HOST_ONLY_VAR", "leak")
	s := mustCreate(t, sandbox.Spec{
		Env:    map[string]string{sbxtest.EnvKey: "1", "GORACE": "atexit_sleep_ms=0", "JOB_VAR": "j"},
		Egress: sandbox.Egress{ProxyURL: testProxyURL},
	})
	out, _, code, err := run(t, s, sandbox.ExecOpts{Env: map[string]string{"EXEC_VAR": "e"}}, "env")
	if err != nil || code != 0 {
		t.Fatalf("env: %d %v", code, err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "http_proxy": true, "https_proxy": true,
		"NO_PROXY": true, "no_proxy": true, sbxtest.EnvKey: true, "GORACE": true, "JOB_VAR": true, "EXEC_VAR": true,
		"SystemRoot": true, "ComSpec": true, "USERPROFILE": true,
	}
	for k := range got {
		if strings.HasPrefix(strings.ToUpper(k), "MYCEL_") || !allowed[k] {
			t.Errorf("unexpected variable %s=%s", k, got[k])
		}
	}
	want := map[string]string{
		"HOME":        filepath.Join(s.dir, "home"),
		"TMPDIR":      filepath.Join(s.dir, "tmp"),
		"HTTP_PROXY":  testProxyURL,
		"https_proxy": testProxyURL,
		"NO_PROXY":    "",
		"JOB_VAR":     "j",
		"EXEC_VAR":    "e",
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("%s = %q (set %v), want %q", k, gv, ok, v)
		}
	}

	for _, bad := range []map[string]string{
		{"MYCEL_X": "1"}, {"mycel_token": "x"}, {"no_proxy": "*,"}, {"Http_Proxy": "http://evil"},
		{"HTTPS_PROXY": "http://evil"}, {"ALL_PROXY": "socks5://evil"}, {"A-B": "x"}, {"A": "x\x00"},
	} {
		if _, err := s.Exec(context.Background(), sbxtest.Argv("env"), sandbox.ExecOpts{Env: bad}); !errors.Is(err, sandbox.ErrEnv) {
			t.Errorf("Exec accepted %q: %v", bad, err)
		}
		if _, err := create(t, sandbox.Spec{Env: bad}); !errors.Is(err, sandbox.ErrEnv) {
			t.Errorf("Create accepted %q: %v", bad, err)
		}
	}
}

func TestEnvWithoutProxy(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	out, _, _, err := run(t, s, sandbox.ExecOpts{}, "env")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, "=")
		if sandbox.IsProxyEnvKey(k) && v != "" {
			t.Errorf("proxy variable set without a proxy: %s", line)
		}
	}
}

func TestArgv0UsesJobPath(t *testing.T) {
	empty := t.TempDir()
	s := mustCreate(t, sandbox.Spec{Env: map[string]string{sbxtest.EnvKey: "1", "PATH": empty}})
	// "go" (or any host tool) is on the host PATH but not on the job's.
	for _, name := range []string{"go", "ls", "cmd"} {
		if _, err := s.Exec(context.Background(), []string{name}, sandbox.ExecOpts{}); err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Errorf("Exec(%s) with an empty job PATH = %v", name, err)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	bin := t.TempDir()
	if err := os.Symlink(sbxtest.Exe(), filepath.Join(bin, "helperbin")); err != nil {
		t.Fatal(err)
	}
	s = mustCreate(t, sandbox.Spec{Env: map[string]string{sbxtest.EnvKey: "1", "GORACE": "atexit_sleep_ms=0", "PATH": "relative:" + bin}})
	p, err := s.Exec(context.Background(), []string{"helperbin", "exit", "7"}, sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(); code != 7 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestExecDir(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	if err := os.MkdirAll(filepath.Join(s.WorkspaceDir(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"/workspace/sub", "sub"} {
		out, _, _, err := run(t, s, sandbox.ExecOpts{Dir: d}, "pwd")
		if err != nil || filepath.Base(strings.TrimSpace(out)) != "sub" {
			t.Fatalf("Dir %s: pwd = %q, %v", d, out, err)
		}
	}
	for _, d := range []string{"/etc", "../x", "/workspace/../x"} {
		if _, err := s.Exec(context.Background(), sbxtest.Argv("pwd"), sandbox.ExecOpts{Dir: d}); !errors.Is(err, ErrEscape) {
			t.Errorf("Dir %s: %v", d, err)
		}
	}
}

func TestCopyInOutGlobConfinement(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyIn(ctx, strings.NewReader("data"), "out/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyIn(ctx, strings.NewReader("log"), "/workspace/out/deep/b.log", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyIn(ctx, strings.NewReader("x"), "top.txt", 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../x", "/etc/x", "a/../../x", ""} {
		if err := s.CopyIn(ctx, strings.NewReader("x"), p, 0o644); !errors.Is(err, ErrEscape) {
			t.Errorf("CopyIn(%q) = %v", p, err)
		}
	}

	rc, err := s.CopyOut(ctx, "out/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "data" {
		t.Fatalf("CopyOut = %q", b)
	}

	// Directory as tar.
	rc, err = s.CopyOut(ctx, "out")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	_ = rc.Close()
	if strings.Join(names, ",") != "a.txt,deep/,deep/b.log" {
		t.Fatalf("tar entries = %v", names)
	}

	for _, p := range []string{"../secret", "/etc/passwd", "out/../../x"} {
		if _, err := s.CopyOut(ctx, p); !errors.Is(err, ErrEscape) {
			t.Errorf("CopyOut(%q) = %v", p, err)
		}
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(s.WorkspaceDir(), "out", "escape")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(s.WorkspaceDir(), "outdir")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("a.txt", filepath.Join(s.WorkspaceDir(), "out", "inside")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CopyOut(ctx, "out/escape"); err == nil {
			t.Error("CopyOut followed an escaping symlink")
		}
		if _, err := s.CopyOut(ctx, "outdir/secret"); err == nil {
			t.Error("CopyOut followed an escaping directory symlink")
		}
		if _, err := s.CopyOut(ctx, "out/inside"); !errors.Is(err, ErrEscape) {
			t.Errorf("CopyOut followed a symlink: %v", err)
		}
		if _, err := s.Exec(ctx, sbxtest.Argv("pwd"), sandbox.ExecOpts{Dir: "outdir"}); !errors.Is(err, ErrEscape) {
			t.Errorf("Exec with Dir through an escaping symlink: %v", err)
		}
		if err := s.CopyIn(ctx, strings.NewReader("x"), "outdir/new", 0o644); err == nil {
			t.Error("CopyIn wrote through an escaping symlink")
		}
		if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
			t.Error("file written outside the workspace")
		}
	}

	got, err := s.Glob(ctx, []string{"out/**/*.log", "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "out/deep/b.log,top.txt" {
		t.Fatalf("Glob = %v", got)
	}
	got, err = s.Glob(ctx, []string{"out"})
	if err != nil {
		t.Fatal(err)
	}
	want := "out/a.txt,out/deep/b.log" // symlinks are never returned
	if strings.Join(got, ",") != want {
		t.Fatalf("Glob(out) = %v, want %s", got, want)
	}
	got, err = s.Glob(ctx, []string{"**"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if strings.HasPrefix(g, "outdir") || strings.HasSuffix(g, "escape") {
			t.Errorf("Glob(**) returned escaping path %s", g)
		}
	}
	for _, p := range []string{"../*", "/etc/*", "a/../../*", "["} {
		if _, err := s.Glob(ctx, []string{p}); err == nil {
			t.Errorf("Glob(%q) accepted", p)
		}
	}
}

func TestSignalDeathIsKilled(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	p, err := s.Exec(context.Background(), sbxtest.Argv("sleep"), sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := s.Pause(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.Resume(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Stats(context.Background())
	if err != nil || st.Processes != 1 || st.RunningSince.IsZero() {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
	if err := p.Signal("TERM"); err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait()
	if code != -1 || !errors.Is(err, sandbox.ErrKilled) {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if err := p.Signal("HUP"); err == nil {
		t.Fatal("unknown signal accepted")
	}
	st, _ = s.Stats(context.Background())
	if st.Processes != 0 {
		t.Fatalf("Processes after exit = %d", st.Processes)
	}
}

func TestExecContextCancelKills(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	ctx, cancel := context.WithCancel(context.Background())
	p, err := s.Exec(ctx, sbxtest.Argv("ignoreterm"), sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(100*time.Millisecond, cancel)
	if code, err := p.Wait(); code != -1 || !errors.Is(err, sandbox.ErrKilled) {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestStatsAfterExit(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	if _, _, code, err := run(t, s, sandbox.ExecOpts{}, "flood", "100000"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	st, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.MemoryBytes <= 0 {
		t.Fatalf("no peak memory after exit: %+v", st)
	}
}

func TestDestroy(t *testing.T) {
	tb := writeTarball(t, false, entry{name: "f", body: "x"})
	s := mustCreate(t, sandbox.Spec{Workspace: sandbox.WorkspaceSeed{Tarball: tb}})
	p, err := s.Exec(context.Background(), sbxtest.Argv("ignoreterm"), sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(); code != -1 || !errors.Is(err, sandbox.ErrKilled) {
		t.Fatalf("Wait after Destroy = %d, %v", code, err)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("Dir still exists: %v", err)
	}
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatalf("second Destroy = %v", err)
	}
	if _, err := s.Exec(context.Background(), sbxtest.Argv("pwd"), sandbox.ExecOpts{}); err == nil {
		t.Fatal("Exec after Destroy succeeded")
	}
	if _, err := s.Glob(context.Background(), []string{"*"}); err == nil {
		t.Fatal("Glob after Destroy succeeded")
	}
}
