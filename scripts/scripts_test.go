// Package scripts holds the tests of the install and demo scripts.
package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func needSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

// installSh runs scripts/install.sh under sh with a clean install
// environment and returns its combined output and exit code.
func installSh(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...) //nolint:gosec // fixed script, test arguments
	cmd.Env = append(os.Environ(), "MYCEL_REPO=", "MYCEL_DOWNLOAD_BASE=", "GITHUB_TOKEN=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func platform() (string, string) {
	return runtime.GOOS, runtime.GOARCH
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestInstallShDryRun(t *testing.T) {
	needSh(t)
	osName, arch := platform()
	dir := filepath.Join(t.TempDir(), "bin")
	out, code := installSh(t, []string{"MYCEL_INSTALL_DIR=" + dir}, "--dry-run", "--version", "1.2.3")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	archive := fmt.Sprintf("mycel_1.2.3_%s_%s.tar.gz", osName, arch)
	base := "https://github.com/Richard-Sen27/mycelium/releases/download/v1.2.3/"
	wantLines(t, out,
		"dry run: nothing is downloaded",
		"platform: "+osName+"/"+arch+"; install dir: "+dir,
		"release: v1.2.3 ("+archive+")",
		"+ download "+base+archive+" -> <tmp>/"+archive,
		"+ download "+base+"checksums.txt -> <tmp>/checksums.txt",
		"+ verify sha256 of "+archive+" against checksums.txt",
		"+ tar -xzf <tmp>/"+archive,
		"+ mkdir -p "+dir,
		"+ mv -f "+dir+"/mycel.new "+dir+"/mycel",
		"mycel join <token>",
		"mycel hub init --advertise",
		"is not on your PATH",
	)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dry run created %s", dir)
	}

	// Latest release: the GitHub API is asked first.
	out, code = installSh(t, []string{"MYCEL_INSTALL_DIR=" + dir, "MYCEL_REPO=acme/fork"}, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out, "+ download https://api.github.com/repos/acme/fork/releases/latest", "releases/download/<latest>/")

	// A token is used but never printed.
	out, _ = installSh(t, []string{"MYCEL_INSTALL_DIR=" + dir, "GITHUB_TOKEN=ghp_secret123"}, "--dry-run")
	if strings.Contains(out, "ghp_secret123") {
		t.Errorf("the token was printed:\n%s", out)
	}
}

func TestInstallShFromSourceDryRun(t *testing.T) {
	needSh(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := filepath.Join(t.TempDir(), "bin")
	out, code := installSh(t, []string{"MYCEL_INSTALL_DIR=" + dir}, "--from-source", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out,
		"+ check: go version is 1.27 or newer",
		"+ git clone --quiet --depth 1 --branch main https://github.com/Richard-Sen27/mycelium.git <tmp>/src",
		"go build -ldflags \"-X github.com/richard-sen27/mycelium/internal/version.Version=main\" -o "+dir+"/mycel ./cmd/mycel",
		"would install: "+dir+"/mycel",
	)
	if strings.Contains(out, "releases/download") {
		t.Errorf("from-source downloaded a release:\n%s", out)
	}
	out, _ = installSh(t, []string{"MYCEL_INSTALL_DIR=" + dir}, "--from-source", "--version", "v0.9.0", "--dry-run")
	wantLines(t, out, "--branch v0.9.0 ")
	out, _ = installSh(t, []string{"MYCEL_INSTALL_DIR=" + dir}, "--from-source", "--ref", "feature/x", "--dry-run")
	wantLines(t, out, "--branch feature/x ")
}

func TestInstallShArgs(t *testing.T) {
	needSh(t)
	out, code := installSh(t, nil, "--bogus")
	if code == 0 || !strings.Contains(out, "unknown argument: --bogus") {
		t.Errorf("unknown argument: exit %d\n%s", code, out)
	}
	if out, code = installSh(t, nil, "--version"); code == 0 {
		t.Errorf("--version without a value: exit 0\n%s", out)
	}
	if out, code = installSh(t, nil, "--help"); code != 0 || !strings.Contains(out, "Usage: install.sh") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
}

// fakeRelease writes a release tree <root>/v9.9.9/{archive,checksums.txt}
// whose "mycel" is a shell script printing its version.
func fakeRelease(t *testing.T) (root, archive string) {
	t.Helper()
	osName, arch := platform()
	root = t.TempDir()
	archive = fmt.Sprintf("mycel_9.9.9_%s_%s.tar.gz", osName, arch)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"mycel":     "#!/bin/sh\necho v9.9.9\n",
		"README.md": "readme\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	dir := filepath.Join(root, "v9.9.9")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, archive), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + archive + "\n" + strings.Repeat("0", 64) + "  mycel_9.9.9_other.zip\n"
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, archive
}

func TestInstallShFromFakeRelease(t *testing.T) {
	needSh(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	root, archive := fakeRelease(t)
	dir := filepath.Join(t.TempDir(), "bin")
	env := []string{"MYCEL_INSTALL_DIR=" + dir, "MYCEL_DOWNLOAD_BASE=file://" + root}
	out, code := installSh(t, env, "--version", "v9.9.9")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out, "checksum ok:", "installed: "+dir+"/mycel (v9.9.9)")
	st, err := os.Stat(filepath.Join(dir, "mycel"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed binary: %v %v", st, err)
	}

	// A tampered archive is refused and nothing is replaced.
	if err := os.Remove(filepath.Join(dir, "mycel")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, "v9.9.9", archive), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("tampered")
	_ = f.Close()
	out, code = installSh(t, env, "--version", "v9.9.9")
	if code == 0 || !strings.Contains(out, "checksum mismatch") {
		t.Errorf("tampered archive: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "mycel")); !os.IsNotExist(err) {
		t.Errorf("a tampered archive was installed: %v", err)
	}

	// A release without the archive fails.
	out, code = installSh(t, env, "--version", "v0.0.1")
	if code == 0 || !strings.Contains(out, "download failed") {
		t.Errorf("missing release: exit %d\n%s", code, out)
	}
}

// TestInstallShCurlArgs runs install.sh with a fake curl that records its
// arguments and stdin: HTTPS-only flags, and the token on stdin, never in
// argv.
func TestInstallShCurlArgs(t *testing.T) {
	needSh(t)
	bin := t.TempDir()
	rec := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + rec + "/args\ncase \" $* \" in *' -K - '*) cat >> " + rec + "/stdin ;; esac\nexit 22\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fake), 0o700); err != nil { //nolint:gosec // an executable test stub
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GITHUB_TOKEN=ghp_secret123",
		"MYCEL_INSTALL_DIR=rel/bin"}
	out, code := installSh(t, env)
	if code == 0 || !strings.Contains(out, "download failed: https://api.github.com/repos/Richard-Sen27/mycelium/releases/latest") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wd, _ := os.Getwd()
	wantLines(t, out, "install dir: "+filepath.Join(wd, "rel/bin"))
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	stdin, _ := os.ReadFile(filepath.Join(rec, "stdin"))
	if strings.Contains(string(args), "ghp_secret123") || strings.Contains(out, "ghp_secret123") {
		t.Errorf("the token is on the command line or printed:\n%s", args)
	}
	if !strings.Contains(string(stdin), `header = "Authorization: Bearer ghp_secret123"`) {
		t.Errorf("curl stdin = %q", stdin)
	}
	for _, w := range []string{"--proto\n=https\n", "--proto-redir\n=https\n", "--tlsv1.2\n"} {
		if !strings.Contains(string(args), w) {
			t.Errorf("curl args lack %q:\n%s", w, args)
		}
	}
}

func TestShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	out, err := exec.Command(sc, "--shell=sh", "install.sh", "demo.sh").CombinedOutput() //nolint:gosec // shellcheck from PATH
	if err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

// TestInstallPs1Parses parses install.ps1 with PowerShell's own parser
// (pwsh on any OS, e.g. a Mac with PowerShell installed).
func TestInstallPs1Parses(t *testing.T) {
	ps, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	script := `$errs = $null; [void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path 'install.ps1'), [ref]$null, [ref]$errs); ` +
		`if ($errs.Count -gt 0) { $errs | ForEach-Object { $_.ToString() }; exit 1 }`
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput() //nolint:gosec // fixed script
	if err != nil {
		t.Fatalf("install.ps1 does not parse: %v\n%s", err, out)
	}
}

// TestInstallPs1DryRun runs install.ps1 -DryRun when PowerShell is available
// (Windows CI runners have it).
func TestInstallPs1DryRun(t *testing.T) {
	ps := ""
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			ps = p
			break
		}
	}
	if ps == "" {
		t.Skip("PowerShell not installed")
	}
	dir := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "install.ps1", "-DryRun", "-Version", "1.2.3")
	cmd.Env = append(os.Environ(), "MYCEL_INSTALL_DIR="+dir, "MYCEL_REPO=", "MYCEL_DOWNLOAD_BASE=", "LOCALAPPDATA="+t.TempDir(), "PROCESSOR_ARCHITECTURE=AMD64")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if err != nil {
		t.Fatalf("install.ps1: %v\n%s", err, out)
	}
	base := "https://github.com/Richard-Sen27/mycelium/releases/download/v1.2.3/"
	wantLines(t, out, "dry run: nothing is downloaded", "release: v1.2.3 (mycel_1.2.3_windows_amd64.zip)",
		"+ download "+base+"mycel_1.2.3_windows_amd64.zip", "+ download "+base+"checksums.txt",
		"+ verify sha256", "would install: "+filepath.Join(dir, "mycel.exe"), "mycel join <token>")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dry run created %s", dir)
	}
}
