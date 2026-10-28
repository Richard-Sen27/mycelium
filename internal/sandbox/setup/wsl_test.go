package setup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// fakeDistro is the state of a fake WSL with an Alpine distro. It fails the
// test when anything is installed before the distro is isolated.
type fakeDistro struct {
	t          *testing.T
	wslOK      bool
	noDistros  bool // `--list --quiet` fails with the "no installed distributions" message
	installed  bool
	version    string
	pkgs       bool
	runscVer   string // "" = runsc missing
	extractVer string // version the extracted runsc reports ("release-test" when empty)
	user       bool
	files      map[string]string
	confMount  bool // Windows drives mounted (the old wsl.conf still applies)
	noDocker   bool
	upgradeErr error
	importErr  error
	smokeErr   error
	extraMount string
	imported   []string
	extracted  []string
}

const wrap = "wsl.exe -d mycel-sbx -u root --exec "

func writeScript(path string) string {
	return "sh -c cat > $1.mycel && chmod 0644 $1.mycel && mv $1.mycel $1 sh " + path
}

func (d *fakeDistro) isolated() bool {
	return !d.confMount && d.files["/etc/wsl.conf"] == WSLConf
}

func (d *fakeDistro) handle(c Cmd) (string, error) {
	cmd := strings.Join(c.Argv, " ")
	fail := errors.New("exit status 1")
	switch {
	case cmd == "docker --version":
		if d.noDocker {
			return "", fail
		}
		return "Docker version 28.0.0\n", nil
	case cmd == "wsl.exe --status":
		if !d.wslOK {
			return "", fail
		}
		return utf16le("Default Version: 2\r\n"), nil
	case cmd == "wsl.exe --list --quiet":
		switch {
		case d.installed:
			return utf16le("Ubuntu\r\nmycel-sbx\r\n"), nil
		case d.noDistros:
			return utf16le("Windows Subsystem for Linux has no installed distributions.\r\nYou can resolve this by installing a distribution with the instructions below:\r\n"), fail
		}
		return utf16le("Ubuntu\r\n"), nil
	case cmd == "wsl.exe --list --verbose":
		return utf16le("  NAME         STATE      VERSION\r\n* Ubuntu       Running    2\r\n  mycel-sbx    Stopped    " + d.version + "\r\n"), nil
	case strings.HasPrefix(cmd, "wsl.exe --import "):
		d.imported = append(d.imported, cmd)
		if d.importErr != nil {
			return "", d.importErr
		}
		d.installed, d.version, d.confMount = true, "2", true
		return "", nil
	case cmd == "wsl.exe --set-version mycel-sbx 2":
		d.version = "2"
		return "", nil
	case cmd == "wsl.exe --terminate mycel-sbx":
		d.confMount = d.files["/etc/wsl.conf"] != WSLConf
		return "", nil
	case !strings.HasPrefix(cmd, wrap):
		d.t.Fatalf("unexpected command %q", cmd)
	}
	if !d.installed {
		return "", errors.New("distro not found")
	}
	in := strings.TrimPrefix(cmd, wrap)
	switch {
	case in == "runsc --version":
		if d.runscVer == "" {
			return "", fail
		}
		return "runsc version " + d.runscVer + "\nspec: 1.2.0\n", nil
	case in == "runsc --network=none --ignore-cgroups do true":
		if d.runscVer == "" {
			return "", fail
		}
		return "", d.smokeErr
	case in == "ip -V":
		if !d.pkgs {
			return "", fail
		}
		return "ip utility, iproute2-6.15.0\n", nil
	case slices.Contains([]string{"nft --version", "setsid --version", "du --version", "tar --version", "find --version"}, in):
		if !d.pkgs {
			return "", fail
		}
		return "ok\n", nil
	case in == "apk upgrade --no-cache --quiet":
		if !d.isolated() {
			d.t.Errorf("apk upgrade before the distro was isolated")
		}
		return "", d.upgradeErr
	case strings.HasPrefix(in, "apk add --no-cache --quiet "):
		if !d.isolated() {
			d.t.Errorf("apk add before the distro was isolated")
		}
		d.pkgs = true
		return "", nil
	case in == "tar -x -f - -C /usr/local/bin":
		if !d.isolated() {
			d.t.Errorf("gVisor installed before the distro was isolated")
		}
		tr := tar.NewReader(c.Stdin)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			d.extracted = append(d.extracted, fmt.Sprintf("%s %o %d", h.Name, h.Mode, h.Uid))
		}
		d.runscVer = d.extractVer
		if d.runscVer == "" {
			d.runscVer = "release-test"
		}
		return "", nil
	case in == "id -u mycel":
		if !d.user {
			return "", fail
		}
		return "1000\n", nil
	case in == "adduser -D -H -s /sbin/nologin mycel":
		d.user = true
		return "", nil
	case strings.HasPrefix(in, "cat /etc/"):
		v, ok := d.files[strings.TrimPrefix(in, "cat ")]
		if !ok {
			return "", fail
		}
		return v, nil
	case strings.HasPrefix(in, "sh -c cat > $1.mycel"):
		f := strings.Fields(in)
		b, _ := io.ReadAll(c.Stdin)
		if d.files == nil {
			d.files = map[string]string{}
		}
		d.files[f[len(f)-1]] = string(b)
		return "", nil
	case in == "cat /proc/mounts":
		m := "rootfs / ext4 rw 0 0\nnone /mnt/wsl tmpfs rw 0 0\ndrivers /usr/lib/wsl/drivers 9p ro,aname=drivers 0 0\n" + d.extraMount
		if d.confMount {
			m += "C:\\134 /mnt/c 9p rw,dirsync,aname=drvfs 0 0\n"
		}
		return m, nil
	}
	d.t.Fatalf("unexpected command in distro: %q", in)
	return "", nil
}

func newWSL(t *testing.T, d *fakeDistro, dl *fakeDownloader) (*WSL, *fakeRunner, *[]string) {
	r := &fakeRunner{handle: d.handle}
	var log []string
	w := &WSL{Options: Options{Runner: r, Downloader: dl, Dir: t.TempDir(), Yes: true,
		Log: func(s string) { log = append(log, s) }},
		Backend: &fakeBackend{name: sandbox.BackendWSL2GVisor, ok: true}, GOARCH: "amd64"}
	return w, r, &log
}

// A fresh machine after `wsl --install --no-distribution`: no distro at all.
func TestWSLFreshSetupCommandSequence(t *testing.T) {
	rootfs, release := pinFakes(t)
	d := &fakeDistro{t: t, wslOK: true, noDistros: true}
	dl := &fakeDownloader{bodies: map[string][]byte{AlpineRootfs.URL: rootfs, GVisor["amd64"].URL: release}}
	w, r, _ := newWSL(t, d, dl)
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	dlDir := filepath.Join(w.Dir, "downloads")
	tools := []string{wrap + "ip -V", wrap + "nft --version", wrap + "setsid --version", wrap + "du --version", wrap + "tar --version", wrap + "find --version"}
	want := []string{
		"wsl.exe --status",
		"wsl.exe --list --quiet",
		"wsl.exe --import mycel-sbx " + filepath.Join(w.Dir, "distro") + " " + filepath.Join(dlDir, "rootfs.tar.gz") + " --version 2",
		"wsl.exe --list --verbose",
		// isolation before anything is installed
		wrap + "id -u mycel",
		wrap + "adduser -D -H -s /sbin/nologin mycel",
		wrap + "cat /etc/wsl.conf",
		wrap + writeScript("/etc/wsl.conf"),
		"wsl.exe --terminate mycel-sbx",
		// packages
		wrap + "cat /etc/apk/repositories",
		wrap + writeScript("/etc/apk/repositories"),
	}
	want = append(want, tools...)
	want = append(want,
		wrap+"apk upgrade --no-cache --quiet",
		wrap+"apk add --no-cache --quiet iproute2 nftables util-linux-misc coreutils tar findutils",
		// gVisor
		wrap+"runsc --version",
		wrap+"tar -x -f - -C /usr/local/bin",
		wrap+"runsc --version",
		// repair path and verification
		wrap+"cat /etc/wsl.conf",
		wrap+"runsc --version",
	)
	want = append(want, tools...)
	want = append(want,
		wrap+"cat /proc/mounts",
		"docker --version",
		wrap+"runsc --network=none --ignore-cgroups do true",
	)
	if got := r.take(); !slices.Equal(got, want) {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(dl.gets) != 2 {
		t.Fatalf("downloads %v", dl.gets)
	}
	wantTar := []string{"runsc 755 0", "gvisor-bin/ 755 0", "gvisor-bin/gvisor_sentry 755 0", "gvisor-bin/runsc-fd-parking 755 0"}
	if !slices.Equal(d.extracted, wantTar) {
		t.Fatalf("extracted %v, want %v", d.extracted, wantTar)
	}
	if d.files["/etc/wsl.conf"] != WSLConf || d.files["/etc/apk/repositories"] != AlpineRepositories {
		t.Fatalf("files %q", d.files)
	}
	for _, f := range []string{"rootfs.tar.gz", "gvisor.tar.bz2"} {
		if _, err := os.Stat(filepath.Join(dlDir, f)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWSLInstalledTreatsNoDistrosAsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
		bad  bool
	}{
		{"listed", utf16le("Ubuntu\r\nmycel-sbx\r\n"), nil, true, false},
		{"others only", utf16le("Ubuntu\r\n"), nil, false, false},
		{"no distros message", utf16le("Windows Subsystem for Linux has no installed distributions.\r\n"), errors.New("exit status 1"), false, false},
		{"error code only", utf16le("Error code: Wsl/WSL_E_DEFAULT_DISTRO_NOT_FOUND\r\n"), errors.New("exit status 4294967295"), false, false},
		{"silent failure", "", errors.New("exit status 1"), false, false},
		{"real failure", utf16le("Access is denied.\r\n"), errors.New("exit status 5"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{handle: func(Cmd) (string, error) { return tc.out, tc.err }}
			w := &WSL{Options: Options{Runner: r}}
			got, err := w.installed(context.Background())
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("installed = %v, %v", got, err)
			}
		})
	}
}

func TestWSLRerunIsIdempotent(t *testing.T) {
	rootfs, release := pinFakes(t)
	d := &fakeDistro{t: t, wslOK: true}
	dl := &fakeDownloader{bodies: map[string][]byte{AlpineRootfs.URL: rootfs, GVisor["amd64"].URL: release}}
	w, r, _ := newWSL(t, d, dl)
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.take()
	dl.gets = nil
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.take()
	for _, c := range got {
		for _, bad := range []string{"--import", "apk add", "tar -x", "adduser", "$1.mycel", "--terminate"} {
			if strings.Contains(c, bad) {
				t.Errorf("re-run ran %q", c)
			}
		}
	}
	if !slices.Contains(got, wrap+"apk upgrade --no-cache --quiet") {
		t.Error("re-run did not upgrade packages")
	}
	if len(dl.gets) != 0 || len(d.imported) != 1 {
		t.Fatalf("re-run downloaded %v / imported %d times", dl.gets, len(d.imported))
	}
}

func TestWSLRerunOfflineStillVerifies(t *testing.T) {
	d := &fakeDistro{t: t, wslOK: true, installed: true, version: "2", pkgs: true, runscVer: "release-test", user: true,
		files:      map[string]string{"/etc/wsl.conf": WSLConf, "/etc/apk/repositories": AlpineRepositories},
		upgradeErr: errors.New("temporary error (try again later)")}
	pinFakes(t)
	w, _, log := newWSL(t, d, &fakeDownloader{})
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "warning: apk upgrade failed") {
		t.Errorf("log:\n%s", strings.Join(*log, "\n"))
	}
	// Offline with a tool missing is fatal.
	d.pkgs = false
	if err := w.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "apk upgrade") {
		t.Fatalf("err = %v", err)
	}
}

func TestWSLRepairsBrokenDistro(t *testing.T) {
	_, release := pinFakes(t)
	// Installed, but on WSL 1, with an old runsc, a stale wsl.conf and no
	// Docker client on Windows.
	d := &fakeDistro{t: t, wslOK: true, installed: true, version: "1", pkgs: true, user: true, runscVer: "release-20250101.0",
		files:     map[string]string{"/etc/wsl.conf": "[automount]\nenabled = true\n", "/etc/apk/repositories": AlpineRepositories},
		confMount: true, noDocker: true}
	dl := &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: release}}
	w, r, log := newWSL(t, d, dl)
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.take(), "\n")
	for _, want := range []string{"wsl.exe --set-version mycel-sbx 2", "tar -x -f -", writeScript("/etc/wsl.conf"), "wsl.exe --terminate mycel-sbx"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "--import") || strings.Contains(got, "apk add") || strings.Contains(got, writeScript("/etc/apk/repositories")) {
		t.Errorf("repair re-imported, reinstalled packages or rewrote the repositories:\n%s", got)
	}
	if !slices.Equal(dl.gets, []string{GVisor["amd64"].URL}) {
		t.Fatalf("downloads %v", dl.gets)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "no Docker client") {
		t.Errorf("no Docker note in log:\n%s", strings.Join(*log, "\n"))
	}
}

func TestWSLVerificationFailures(t *testing.T) {
	ready := func() *fakeDistro {
		return &fakeDistro{t: t, wslOK: true, installed: true, version: "2", pkgs: true, runscVer: "release-test", user: true,
			files: map[string]string{"/etc/wsl.conf": WSLConf, "/etc/apk/repositories": AlpineRepositories}}
	}
	_, release := pinFakes(t)

	d := ready()
	d.extraMount = "D:\\134 /srv/data 9p rw,aname=drvfs 0 0\n"
	w, _, _ := newWSL(t, d, &fakeDownloader{})
	if err := w.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "/srv/data") {
		t.Fatalf("mount outside /mnt: err = %v", err)
	}

	d = ready()
	d.smokeErr = errors.New("exit status 128: creating container: cannot set up cgroup")
	w, _, _ = newWSL(t, d, &fakeDownloader{})
	if err := w.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot set up cgroup") {
		t.Fatalf("smoke run: err = %v", err)
	}

	d = ready()
	d.runscVer, d.extractVer = "", "release-other"
	w, _, _ = newWSL(t, d, &fakeDownloader{bodies: map[string][]byte{GVisor["amd64"].URL: release}})
	if err := w.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), `"release-other"`) {
		t.Fatalf("version mismatch: err = %v", err)
	}
}

func TestWSLChecksumMismatchAborts(t *testing.T) {
	pinFakes(t)
	d := &fakeDistro{t: t, wslOK: true}
	dl := &fakeDownloader{bodies: map[string][]byte{AlpineRootfs.URL: []byte("tampered")}}
	w, r, _ := newWSL(t, d, dl)
	err := w.Setup(context.Background())
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	ents, _ := os.ReadDir(filepath.Join(w.Dir, "downloads"))
	if len(ents) != 0 {
		t.Fatalf("partial download left behind: %v", ents)
	}
	for _, c := range r.take() {
		if strings.Contains(c, "--import") {
			t.Fatalf("imported after a checksum failure: %q", c)
		}
	}
}

func TestWSLCachedDownloadWithWrongDigestIsReplaced(t *testing.T) {
	rootfs, release := pinFakes(t)
	d := &fakeDistro{t: t, wslOK: true}
	dl := &fakeDownloader{bodies: map[string][]byte{AlpineRootfs.URL: rootfs, GVisor["amd64"].URL: release}}
	w, _, _ := newWSL(t, d, dl)
	p := filepath.Join(w.Dir, "downloads", AlpineRootfs.File)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, rootfs) {
		t.Fatal("corrupt cached rootfs was not replaced")
	}
}

func TestWSLPrerequisites(t *testing.T) {
	rootfs, release := pinFakes(t)
	var pe *PrereqError

	// WSL absent.
	d := &fakeDistro{t: t}
	w, r, _ := newWSL(t, d, &fakeDownloader{})
	if err := w.Setup(context.Background()); !errors.As(err, &pe) || !strings.Contains(pe.Instruction, "wsl --install") {
		t.Fatalf("err = %v, want a PrereqError with wsl --install", err)
	}
	if got := r.take(); !slices.Equal(got, []string{"wsl.exe --status"}) {
		t.Fatalf("commands %v", got)
	}
	if rep := w.Status(context.Background()); rep.Details["wsl"] != false || rep.Backend != sandbox.BackendWSL2GVisor {
		t.Fatalf("status %+v", rep)
	}

	// Virtual Machine Platform off: the import fails.
	d = &fakeDistro{t: t, wslOK: true, noDistros: true,
		importErr: errors.New("wsl.exe --import: exit status 1: Please enable the Virtual Machine Platform Windows feature and ensure virtualization is enabled in the BIOS. Error code: Wsl/Service/RegisterDistro/CreateVm/HCS/HCS_E_HYPERV_NOT_INSTALLED")}
	w, _, _ = newWSL(t, d, &fakeDownloader{bodies: map[string][]byte{AlpineRootfs.URL: rootfs, GVisor["amd64"].URL: release}})
	if err := w.Setup(context.Background()); !errors.As(err, &pe) || !strings.Contains(pe.Instruction, "wsl --install") ||
		!strings.Contains(pe.Missing, "Virtual Machine Platform") {
		t.Fatalf("err = %v", err)
	}

	// ARM64 Windows.
	d = &fakeDistro{t: t, wslOK: true}
	w, r, _ = newWSL(t, d, &fakeDownloader{})
	w.GOARCH = "arm64"
	if err := w.Setup(context.Background()); !errors.As(err, &pe) || !strings.Contains(pe.Missing, "x86_64") {
		t.Fatalf("err = %v", err)
	}
	if got := r.take(); len(got) != 0 {
		t.Fatalf("ran %v on arm64", got)
	}
}

func TestWSLDownloadNeedsConsent(t *testing.T) {
	rootfs, release := pinFakes(t)
	d := &fakeDistro{t: t, wslOK: true}
	dl := &fakeDownloader{bodies: map[string][]byte{AlpineRootfs.URL: rootfs, GVisor["amd64"].URL: release}}
	w, _, _ := newWSL(t, d, dl)
	w.Yes = false
	if err := w.Setup(context.Background()); !errors.Is(err, ErrNeedsConsent) {
		t.Fatalf("err = %v, want ErrNeedsConsent", err)
	}
	var asked []string
	w.Confirm = func(q string) (bool, error) { asked = append(asked, q); return false, nil }
	if err := w.Setup(context.Background()); !errors.Is(err, ErrNeedsConsent) {
		t.Fatalf("declined: err = %v", err)
	}
	if len(dl.gets) != 0 {
		t.Fatalf("downloaded without consent: %v", dl.gets)
	}
	w.Confirm = func(q string) (bool, error) { asked = append(asked, q); return true, nil }
	if err := w.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One question per run, covering both downloads (4 + 100 MB).
	if len(asked) != 2 || !strings.Contains(asked[1], "about 104 MB") {
		t.Fatalf("questions %q", asked)
	}
}

func TestWSLConfContents(t *testing.T) {
	for _, want := range []string{
		"[automount]\nenabled = false\nmountFsTab = false\n",
		"[interop]\nenabled = false\nappendWindowsPath = false\n",
		"[gpu]\nenabled = false\n",
		"[boot]\nsystemd = false\n",
		"[user]\ndefault = mycel\n",
	} {
		if !strings.Contains(WSLConf, want) {
			t.Errorf("wsl.conf lacks %q", want)
		}
	}
	if strings.ContainsAny(WSLConf, "\"\r") {
		t.Error("wsl.conf has quotes or CR")
	}
	if !strings.HasPrefix(AlpineRepositories, "https://") || strings.Count(AlpineRepositories, "/"+AlpineBranch+"/") != 2 {
		t.Errorf("repositories %q", AlpineRepositories)
	}
}

func TestWSLStatus(t *testing.T) {
	d := &fakeDistro{t: t, wslOK: true, installed: true, version: "2", pkgs: true, runscVer: "release-test"}
	w, _, _ := newWSL(t, d, &fakeDownloader{})
	rep := w.Status(context.Background())
	if !rep.Available || rep.Details["distro_installed"] != true || rep.Details["wsl_version"] != "2" ||
		rep.Details["runsc"] != "runsc version release-test" || rep.Details["docker_client"] != true {
		t.Fatalf("status %+v", rep)
	}
	d = &fakeDistro{t: t, wslOK: true, noDistros: true}
	w, _, _ = newWSL(t, d, &fakeDownloader{})
	rep = w.Status(context.Background())
	if rep.Details["distro_installed"] != false || rep.Details["error"] != nil || rep.Details["hint"] != "run `mycel sandbox setup`" {
		t.Fatalf("status without distros %+v", rep)
	}
}

func TestWindowsMounts(t *testing.T) {
	m := windowsMounts("drivers /usr/lib/wsl/drivers 9p ro 0 0\nC:\\134 /mnt/c 9p rw 0 0\nD: /mnt/d drvfs rw 0 0\nnone /mnt/wsl tmpfs rw 0 0\n" +
		"E: /home/x/e drvfs rw 0 0\nshare /data virtiofs rw 0 0\nnone /usr/lib/wsl/lib overlay ro 0 0\n")
	if !slices.Equal(m, []string{"/mnt/c", "/mnt/d", "/home/x/e", "/data"}) {
		t.Fatal(m)
	}
}

func TestRunscVersion(t *testing.T) {
	for in, want := range map[string]string{
		"runsc version release-20261012.0\nspec: 1.2.0\n": "release-20261012.0",
		"runsc version release-20261012.0-rc1":            "release-20261012.0-rc1",
		"something else":                                  "",
		"":                                                "",
	} {
		if got := runscVersion(in); got != want {
			t.Errorf("runscVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
