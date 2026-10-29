package setup

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// --- fakes ---------------------------------------------------------------

// fakeRunner records every command and answers through handle.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []string
	handle func(c Cmd) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, c Cmd) error {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(c.Argv, " "))
	f.mu.Unlock()
	out, err := f.handle(c)
	if c.Stdout != nil && out != "" {
		_, _ = io.WriteString(c.Stdout, out)
	}
	return err
}

func (f *fakeRunner) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// fakeDownloader serves fixed bodies and counts requests.
type fakeDownloader struct {
	mu     sync.Mutex
	bodies map[string][]byte
	gets   []string
}

func (d *fakeDownloader) Get(_ context.Context, url string, _ int64, w io.Writer) error {
	d.mu.Lock()
	d.gets = append(d.gets, url)
	b, ok := d.bodies[url]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("404 %s", url)
	}
	_, err := w.Write(b)
	return err
}

// fakeBackend is a sandbox.Backend with a scripted Available.
type fakeBackend struct {
	name     string
	ok       bool
	why      string
	cached   bool
	prepared []string
	created  []sandbox.Spec
	destroys int
	exitCode int
}

func (b *fakeBackend) Name() string                             { return b.name }
func (b *fakeBackend) Available(context.Context) (bool, string) { return b.ok, b.why }
func (b *fakeBackend) Capabilities() sandbox.Capabilities       { return sandbox.Capabilities{} }
func (b *fakeBackend) ImageCached(string) (bool, string, error) {
	return b.cached, "https://example.test/vm.img", nil
}
func (b *fakeBackend) Prepare(_ context.Context, image string, progress func(string)) error {
	b.prepared = append(b.prepared, image)
	progress("image ready")
	b.cached = true
	return nil
}
func (b *fakeBackend) Create(_ context.Context, s sandbox.Spec) (sandbox.Sandbox, error) {
	b.created = append(b.created, s)
	return &fakeSandbox{b: b}, nil
}

type fakeSandbox struct {
	sandbox.Sandbox
	b    *fakeBackend
	argv []string
}

func (s *fakeSandbox) Exec(_ context.Context, argv []string, _ sandbox.ExecOpts) (sandbox.Process, error) {
	s.argv = argv
	return fakeProc{code: s.b.exitCode}, nil
}
func (s *fakeSandbox) Destroy(context.Context) error { s.b.destroys++; return nil }

type fakeProc struct{ code int }

func (fakeProc) Stdout() io.Reader    { return strings.NewReader("") }
func (fakeProc) Stderr() io.Reader    { return strings.NewReader("") }
func (p fakeProc) Wait() (int, error) { return p.code, nil }
func (fakeProc) Signal(string) error  { return nil }

func utf16le(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, 2+2*len(u))
	b = append(b, 0xff, 0xfe)
	for _, r := range u {
		b = append(b, byte(r&0xff), byte(r>>8)) //nolint:gosec // split into bytes
	}
	return string(b)
}

func digestOf(algo string, b []byte) string {
	if algo == "sha512" {
		s := sha512.Sum512(b)
		return "sha512:" + hex.EncodeToString(s[:])
	}
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// pinFakes points the pinned sources at fake bodies for one test.
func pinFakes(t *testing.T) (rootfs, release []byte) {
	t.Helper()
	release, err := os.ReadFile("testdata/gvisor-fake.tar.bz2")
	if err != nil {
		t.Fatal(err)
	}
	rootfs = []byte("fake alpine rootfs tarball")
	oldA, oldG := AlpineRootfs, GVisor
	t.Cleanup(func() { AlpineRootfs, GVisor = oldA, oldG })
	AlpineRootfs = Source{Name: "rootfs", Version: "1", URL: "https://example.test/rootfs.tar.gz", File: "rootfs.tar.gz",
		Digest: digestOf("sha256", rootfs), SizeMB: 4}
	g := Source{Name: "gvisor", Version: "release-test", URL: "https://example.test/gvisor.tar.bz2", File: "gvisor.tar.bz2",
		Digest: digestOf("sha512", release), SizeMB: 100}
	GVisor = map[string]Source{"amd64": g, "arm64": g}
	return rootfs, release
}

// --- macOS ---------------------------------------------------------------

func newLima(t *testing.T, b *fakeBackend, haveLima bool) (*Lima, *fakeRunner) {
	r := &fakeRunner{handle: func(c Cmd) (string, error) {
		switch strings.Join(c.Argv[1:], " ") {
		case "--version":
			return "limactl version 2.1.1\n", nil
		case "list --format {{.Name}} {{.Status}}":
			if !slices.Contains(c.Env, "LIMA_HOME=/lima/home") {
				t.Errorf("limactl list without LIMA_HOME: %v", c.Env)
			}
			return "sbx-0123456789ab Running\nother Running\nsbx-ba9876543210 Stopped\n", nil
		}
		t.Fatalf("unexpected limactl %v", c.Argv)
		return "", nil
	}}
	l := &Lima{Options: Options{Runner: r, Dir: t.TempDir(), Yes: true}, Backend: b, LimaHome: "/lima/home",
		LookPath: func(string) (string, error) {
			if !haveLima {
				return "", errors.New("not found")
			}
			return "/opt/homebrew/bin/limactl", nil
		},
		NewID: func() string { return "sbx:01JAAAAAAAAAAAAAAAAAAAAAAA" }}
	return l, r
}

func TestLimaMissing(t *testing.T) {
	l, _ := newLima(t, &fakeBackend{name: sandbox.BackendAppleVF}, false)
	var pe *PrereqError
	if err := l.Setup(context.Background()); !errors.As(err, &pe) || !strings.Contains(pe.Instruction, "brew install lima") {
		t.Fatalf("err = %v", err)
	}
	if rep := l.Status(context.Background()); !strings.Contains(fmt.Sprint(rep.Details["hint"]), "brew install lima") {
		t.Fatalf("status %+v", rep)
	}
}

func TestLimaSetupPreparesAndBootsOnce(t *testing.T) {
	b := &fakeBackend{name: sandbox.BackendAppleVF, ok: true}
	l, _ := newLima(t, b, true)
	if err := l.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.prepared, []string{"docker.io/library/debian:bookworm-slim"}) {
		t.Fatalf("prepared %v", b.prepared)
	}
	if len(b.created) != 1 || b.destroys != 1 {
		t.Fatalf("created %d, destroyed %d", len(b.created), b.destroys)
	}
	s := b.created[0]
	if s.ID != "sbx:01JAAAAAAAAAAAAAAAAAAAAAAA" || s.Profile.GetName() != "default" || s.Egress.ProxyURL != "" || len(s.Mounts) != 0 {
		t.Fatalf("spec %+v", s)
	}
	if filepath.Base(s.Dir) != "setup-vm-01jaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("test VM dir %s is not per run", s.Dir)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatalf("test VM dir left behind: %v", err)
	}
}

func TestLimaSetupUnavailableAndFailedBoot(t *testing.T) {
	l, _ := newLima(t, &fakeBackend{name: sandbox.BackendAppleVF, why: "Lima 1.0 is too old"}, true)
	if err := l.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("err = %v", err)
	}
	b := &fakeBackend{name: sandbox.BackendAppleVF, ok: true, exitCode: 3}
	l, _ = newLima(t, b, true)
	if err := l.Setup(context.Background()); err == nil || b.destroys != 1 {
		t.Fatalf("err = %v, destroys %d", err, b.destroys)
	}
}

func TestLimaSetupAsksBeforeImageDownload(t *testing.T) {
	b := &fakeBackend{name: sandbox.BackendAppleVF, ok: true}
	l, _ := newLima(t, b, true)
	l.Yes = false
	if err := l.Setup(context.Background()); !errors.Is(err, ErrNeedsConsent) || len(b.prepared) != 0 {
		t.Fatalf("err = %v, prepared %v", err, b.prepared)
	}
	b.cached = true // nothing to download: no question
	if err := l.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLimaStatus(t *testing.T) {
	b := &fakeBackend{name: sandbox.BackendAppleVF, ok: true, cached: true}
	l, _ := newLima(t, b, true)
	rep := l.Status(context.Background())
	d := rep.Details
	if !rep.Available || rep.Backend != sandbox.BackendAppleVF || d["lima"] != "2.1.1" || d["image_cached"] != true ||
		d["vms"] != 2 || d["vms_running"] != 1 {
		t.Fatalf("status %+v", rep)
	}
}

// --- Linux ---------------------------------------------------------------

func newLinux(t *testing.T, files map[string]string, runscOnPath bool) (*Linux, *fakeRunner) {
	r := &fakeRunner{handle: func(c Cmd) (string, error) {
		if len(c.Argv) == 2 && c.Argv[1] == "--version" && strings.HasSuffix(c.Argv[0], "runsc") {
			return "runsc version release-test\n", nil
		}
		t.Fatalf("unexpected %v", c.Argv)
		return "", nil
	}}
	l := &Linux{Options: Options{Runner: r, Dir: t.TempDir()}, BinDir: filepath.Join(t.TempDir(), "bin"), GOARCH: "amd64",
		RootBinDir: filepath.Join(t.TempDir(), "root-bin"), Geteuid: func() int { return 1000 }, Getenv: func(string) string { return "/usr/bin:/bin" },
		Backend: &fakeBackend{name: sandbox.BackendGVisor, ok: true},
		LookPath: func(string) (string, error) {
			if runscOnPath {
				return "/usr/local/bin/runsc", nil
			}
			return "", errors.New("not found")
		},
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}}
	return l, r
}

var goodKernel = map[string]string{"/sys/fs/cgroup/cgroup.controllers": "cpu memory pids\n", "/proc/sys/user/max_user_namespaces": "63432\n"}

func TestLinuxChecks(t *testing.T) {
	l, _ := newLinux(t, goodKernel, true)
	if err := l.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := l.Status(context.Background())
	if !rep.Available || rep.Details["cgroup_v2"] != true || rep.Details["user_namespaces"] != true || rep.Details["runsc"] != "runsc version release-test" {
		t.Fatalf("status %+v", rep)
	}

	l, _ = newLinux(t, map[string]string{"/proc/sys/user/max_user_namespaces": "0\n"}, true)
	var pe *PrereqError
	if err := l.Setup(context.Background()); !errors.As(err, &pe) ||
		!strings.Contains(pe.Instruction, "cgroup v2") || !strings.Contains(pe.Instruction, "user namespaces") {
		t.Fatalf("err = %v", err)
	}
	if rep := l.Status(context.Background()); rep.Details["cgroup_v2"] != false || rep.Details["user_namespaces"] != false {
		t.Fatalf("status %+v", rep)
	}
}

func TestLinuxRunscMissing(t *testing.T) {
	_, release := pinFakes(t)
	l, _ := newLinux(t, goodKernel, false)
	var pe *PrereqError
	if err := l.Setup(context.Background()); !errors.As(err, &pe) || !strings.Contains(pe.Instruction, "gvisor.dev") {
		t.Fatalf("err = %v", err)
	}
	dl := &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: release}}
	l.Downloader, l.Yes = dl, true
	if err := l.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"runsc", "gvisor-bin/gvisor_sentry", "gvisor-bin/runsc-fd-parking"} {
		fi, err := os.Stat(filepath.Join(l.BinDir, f))
		if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755) {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	for _, f := range []string{"containerd-shim-runsc-v1", "gvisor-bin/evil", "../escape"} {
		if _, err := os.Lstat(filepath.Join(l.BinDir, f)); err == nil {
			t.Errorf("%s was installed", f)
		}
	}
	// Re-run: runsc is found in BinDir, nothing downloaded again.
	dl.gets = nil
	if err := l.Setup(context.Background()); err != nil || len(dl.gets) != 0 {
		t.Fatalf("re-run: %v, downloads %v", err, dl.gets)
	}
}

func TestLinuxChecksumFailureInstallsNothing(t *testing.T) {
	pinFakes(t)
	l, _ := newLinux(t, goodKernel, false)
	l.Yes = true
	l.Downloader = &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: []byte("evil")}}
	if err := l.Setup(context.Background()); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(l.BinDir); !os.IsNotExist(err) {
		t.Fatalf("bin dir touched: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(l.Dir, "downloads")); len(ents) != 0 {
		t.Fatalf("partial download left: %v", ents)
	}
}

// --- shared --------------------------------------------------------------

func TestPinnedSourcesAreWellFormed(t *testing.T) {
	for _, s := range append([]Source{AlpineRootfs}, GVisor["amd64"], GVisor["arm64"]) {
		if _, _, err := newDigest(s.Digest); err != nil {
			t.Errorf("%s: %v", s.URL, err)
		}
		if !strings.HasPrefix(s.URL, "https://") || s.File == "" || s.SizeMB <= 0 {
			t.Errorf("bad source %+v", s)
		}
	}
	if !strings.Contains(GVisor["amd64"].URL, strings.TrimPrefix(GVisorVersion, "release-")) {
		t.Error("gVisor URL and version disagree")
	}
}

func TestProbeWithoutBackend(t *testing.T) {
	r := probe(context.Background(), nil, "darwin")
	if r.Available || r.Backend != sandbox.BackendAppleVF || r.Details["reason"] == nil {
		t.Fatalf("%+v", r)
	}
}

func TestNewSandboxID(t *testing.T) {
	id := newSandboxID()
	if len(id) != 30 || !strings.HasPrefix(id, "sbx:") || strings.ContainsAny(id, "ILOU") {
		t.Fatal(id)
	}
}

func TestLinuxInstallDirAndPathHint(t *testing.T) {
	_, release := pinFakes(t)
	// As root: /usr/local/bin (here RootBinDir), no user notes.
	l, _ := newLinux(t, goodKernel, false)
	var log []string
	l.Log = func(s string) { log = append(log, s) }
	l.Yes, l.Geteuid = true, func() int { return 0 }
	l.Downloader = &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: release}}
	if err := l.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.RootBinDir, "runsc")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.BinDir); !os.IsNotExist(err) {
		t.Fatalf("user bin dir touched as root: %v", err)
	}
	if s := strings.Join(log, "\n"); strings.Contains(s, "PATH") || strings.Contains(s, "sudo") {
		t.Errorf("root log:\n%s", s)
	}

	// As a user whose PATH lacks ~/.local/bin: PATH hint and the root note.
	l, _ = newLinux(t, goodKernel, false)
	log = nil
	l.Log = func(s string) { log = append(log, s) }
	l.Yes = true
	l.Downloader = &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: release}}
	if err := l.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := strings.Join(log, "\n")
	if !strings.Contains(s, l.BinDir+" is not on your PATH") || !strings.Contains(s, "sudo cp -r") {
		t.Errorf("user log:\n%s", s)
	}

	// On PATH: no PATH hint.
	l2, _ := newLinux(t, goodKernel, false)
	log = nil
	l2.Log = func(s string) { log = append(log, s) }
	l2.Yes = true
	l2.Getenv = func(string) string { return "/usr/bin" + string(os.PathListSeparator) + l2.BinDir + "/" }
	l2.Downloader = &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: release}}
	if err := l2.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(log, "\n"), "not on your PATH") {
		t.Errorf("PATH hint although on PATH:\n%s", strings.Join(log, "\n"))
	}
}
