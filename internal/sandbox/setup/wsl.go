package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/gvisor/bundle"
)

// SandboxUser is the distro's default (unprivileged) user; the backend
// itself always runs commands with -u root.
const SandboxUser = "mycel"

// WSLConf is /etc/wsl.conf of the mycel-sbx distro: no Windows drives
// mounted (and no fstab), no interop (the distro cannot start Windows
// programs and gets no Windows PATH), no GPU passthrough, no systemd, and an
// unprivileged default user. It takes effect after `wsl --terminate
// mycel-sbx`. It is hygiene, not a security boundary: root inside the
// distro could undo it; the isolation of jobs rests on gVisor.
const WSLConf = `# Written by mycel sandbox setup. The sandbox distro does not mount the
# Windows host's drives or start Windows programs.
[automount]
enabled = false
mountFsTab = false

[interop]
enabled = false
appendWindowsPath = false

[gpu]
enabled = false

[boot]
systemd = false

[user]
default = ` + SandboxUser + `
`

const wslInstall = "run `wsl --install --no-distribution` in an administrator PowerShell, reboot, then run `mycel sandbox setup` again"

// runscSmoke runs a trivial command under gVisor in the distro (host root
// file system, no network, no cgroups): the last step of the verification.
var runscSmoke = []string{"runsc", "--network=none", "--ignore-cgroups", "do", "true"}

// WSL provisions the mycel-sbx distro on Windows.
type WSL struct {
	Options
	Backend sandbox.Backend
	Host    bundle.WSLHost // wsl.exe and mycel-sbx when zero
	GOARCH  string         // runtime.GOARCH when empty
}

func (w *WSL) distro() string {
	if w.Host.Distro == "" {
		return bundle.DefaultDistro
	}
	return w.Host.Distro
}

func (w *WSL) exe() string {
	if w.Host.Exe == "" {
		return "wsl.exe"
	}
	return w.Host.Exe
}

func (w *WSL) arch() string {
	if w.GOARCH == "" {
		return runtime.GOARCH
	}
	return w.GOARCH
}

// in runs argv inside the distro as root.
func (w *WSL) in(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	if err := w.Host.Check(argv); err != nil {
		return err
	}
	return w.Runner.Run(ctx, Cmd{Argv: w.Host.Wrap(argv), Stdin: stdin, Stdout: stdout})
}

func (w *WSL) inOut(ctx context.Context, argv []string) (string, error) {
	var b strings.Builder
	err := w.in(ctx, argv, nil, &b)
	return b.String(), err
}

func (w *WSL) wslText(ctx context.Context, args ...string) (string, error) {
	out, err := w.output(ctx, append([]string{w.exe()}, args...))
	return bundle.DecodeWSLText(out), err
}

// installed reports whether the distro exists. With no distro at all,
// `wsl --list --quiet` fails with a message; that means "not installed".
func (w *WSL) installed(ctx context.Context) (bool, error) {
	out, err := w.output(ctx, []string{w.exe(), "--list", "--quiet"})
	ok, err := bundle.WSLDistroListed(out, err, w.distro())
	if err != nil {
		return false, fmt.Errorf("cannot list WSL distros: %w", err)
	}
	return ok, nil
}

// distroVersion reads the WSL version of the distro from `wsl --list
// --verbose` (rows: [*] NAME STATE VERSION; the header is localised and
// skipped because no row field matches the name).
func (w *WSL) distroVersion(ctx context.Context) (string, error) {
	out, err := w.wslText(ctx, "--list", "--verbose")
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "\uFEFF"))
		if len(f) > 0 && f[0] == "*" {
			f = f[1:]
		}
		if len(f) >= 3 && f[0] == w.distro() {
			return f[len(f)-1], nil
		}
	}
	return "", fmt.Errorf("%s not in `wsl --list --verbose`", w.distro())
}

// vmPlatformOff reports an import failure that means the Virtual Machine
// Platform (Hyper-V) is not enabled, which `wsl --install` fixes.
func vmPlatformOff(err error) bool {
	l := strings.ToLower(err.Error())
	for _, s := range []string{"virtual machine platform", "hcs_e_hyperv_not_installed", "0x80370102", "wsl_e_vm_mode_invalid_state", "enablevirtualization"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// Setup implements Platform. The order matters: the distro is isolated
// (user, wsl.conf, restart) before anything is installed in it, so packages
// never install while Windows drives and interop are on.
func (w *WSL) Setup(ctx context.Context) error {
	w.defaults()
	gv, ok := GVisor["amd64"]
	if w.arch() != "amd64" || !ok {
		return &PrereqError{
			Missing:     "the sandbox distro is available for x86_64 Windows only (this is " + w.arch() + ")",
			Instruction: "use an x86_64 Windows machine as a sandbox node; ARM64 support needs pinned ARM64 Alpine and gVisor releases",
		}
	}
	if err := w.Runner.Run(ctx, Cmd{Argv: []string{w.exe(), "--status"}}); err != nil {
		w.logf("WSL check failed: %v", err)
		return &PrereqError{Missing: "WSL 2 is not installed or not enabled", Instruction: wslInstall}
	}
	w.logf("WSL is available")
	have, err := w.installed(ctx)
	if err != nil {
		return err
	}
	consented := false
	if !have {
		names, mb := w.uncached(AlpineRootfs, gv)
		if len(names) > 0 {
			if err := w.consent(strings.Join(names, " and "), mb); err != nil {
				return err
			}
		}
		consented = true
		rootfs, err := w.fetch(ctx, AlpineRootfs)
		if err != nil {
			return err
		}
		dir := filepath.Join(w.Dir, "distro")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		w.logf("creating the WSL distro %s (Alpine %s, WSL 2)", w.distro(), AlpineRootfs.Version)
		if err := w.Runner.Run(ctx, Cmd{Argv: []string{w.exe(), "--import", w.distro(), dir, rootfs, "--version", "2"}}); err != nil {
			if vmPlatformOff(err) {
				return &PrereqError{Missing: "the Virtual Machine Platform that WSL 2 needs is not enabled (" + err.Error() + ")", Instruction: wslInstall}
			}
			return fmt.Errorf("wsl --import %s: %w", w.distro(), err)
		}
	} else {
		w.logf("WSL distro %s exists; checking it", w.distro())
	}
	if v, err := w.distroVersion(ctx); err != nil {
		return err
	} else if v != "2" {
		w.logf("%s runs on WSL %s; converting it to WSL 2", w.distro(), v)
		if err := w.Runner.Run(ctx, Cmd{Argv: []string{w.exe(), "--set-version", w.distro(), "2"}}); err != nil {
			return fmt.Errorf("wsl --set-version %s 2: %w", w.distro(), err)
		}
	}

	// 1. Isolation first: the default user and wsl.conf, then a restart.
	if err := w.in(ctx, []string{"id", "-u", SandboxUser}, nil, nil); err != nil {
		w.logf("creating user %s", SandboxUser)
		if err := w.in(ctx, []string{"adduser", "-D", "-H", "-s", "/sbin/nologin", SandboxUser}, nil, nil); err != nil {
			return fmt.Errorf("adduser: %w", err)
		}
	}
	if err := w.configure(ctx); err != nil {
		return err
	}

	// 2. Packages from the pinned, signed Alpine repositories.
	if err := w.packages(ctx); err != nil {
		return err
	}

	// 3. gVisor.
	if err := w.gvisor(ctx, gv, consented); err != nil {
		return err
	}

	// 4. Repair path: wsl.conf is checked again (a package must not have
	// changed it), then everything is verified.
	if err := w.configure(ctx); err != nil {
		return err
	}
	return w.verify(ctx, gv)
}

// writeFile replaces path in the distro with content (mode 0644) through a
// temporary file and a rename. No double quotes: wsl.exe re-parses them.
func (w *WSL) writeFile(ctx context.Context, path, content string) error {
	script := []string{"sh", "-c", "cat > $1.mycel && chmod 0644 $1.mycel && mv $1.mycel $1", "sh", path}
	if err := w.in(ctx, script, strings.NewReader(content), nil); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// configure writes /etc/wsl.conf when it differs and restarts the distro so
// it applies.
func (w *WSL) configure(ctx context.Context) error {
	cur, _ := w.inOut(ctx, []string{"cat", "/etc/wsl.conf"})
	if cur == WSLConf {
		w.logf("/etc/wsl.conf is current")
		return nil
	}
	w.logf("writing /etc/wsl.conf (no Windows drives, no interop, no GPU, no systemd)")
	if err := w.writeFile(ctx, "/etc/wsl.conf", WSLConf); err != nil {
		return err
	}
	w.logf("restarting %s so wsl.conf applies", w.distro())
	if err := w.Runner.Run(ctx, Cmd{Argv: []string{w.exe(), "--terminate", w.distro()}}); err != nil {
		return fmt.Errorf("wsl --terminate %s: %w", w.distro(), err)
	}
	return nil
}

// packages pins /etc/apk/repositories, upgrades the distro and installs the
// backend's tools when one is missing.
func (w *WSL) packages(ctx context.Context) error {
	if cur, _ := w.inOut(ctx, []string{"cat", "/etc/apk/repositories"}); cur != AlpineRepositories {
		w.logf("pinning /etc/apk/repositories to Alpine %s", AlpineBranch)
		if err := w.writeFile(ctx, "/etc/apk/repositories", AlpineRepositories); err != nil {
			return err
		}
	}
	missing := false
	for _, argv := range bundle.DistroRequirements()[1:] {
		if err := w.in(ctx, argv, nil, nil); err != nil {
			w.logf("%s is missing in %s", argv[0], w.distro())
			missing = true
		}
	}
	w.logf("updating the distro's packages")
	if err := w.in(ctx, []string{"apk", "upgrade", "--no-cache", "--quiet"}, nil, nil); err != nil {
		if missing {
			return fmt.Errorf("apk upgrade: %w", err)
		}
		// Offline re-runs still verify and repair the rest.
		w.logf("warning: apk upgrade failed, continuing with the installed packages: %v", err)
	}
	if !missing {
		w.logf("packages present (iproute2, nftables, util-linux, coreutils, tar, findutils)")
		return nil
	}
	w.logf("installing %s from Alpine's signed repositories", strings.Join(AlpinePackages, " "))
	if err := w.in(ctx, append([]string{"apk", "add", "--no-cache", "--quiet"}, AlpinePackages...), nil, nil); err != nil {
		return fmt.Errorf("apk add: %w", err)
	}
	return nil
}

// runscVersion is the version `runsc --version` prints on its first line
// ("runsc version release-20261012.0").
func runscVersion(out string) string {
	f := strings.Fields(firstLine(out))
	if len(f) < 3 || f[0] != "runsc" || f[1] != "version" {
		return ""
	}
	return f[2]
}

// gvisor installs the pinned release unless exactly that version is there.
func (w *WSL) gvisor(ctx context.Context, gv Source, consented bool) error {
	if out, err := w.inOut(ctx, bundle.Runsc{}.Version()); err == nil && runscVersion(out) == gv.Version {
		w.logf("runsc %s present", gv.Version)
		return nil
	}
	if !consented {
		if names, mb := w.uncached(gv); len(names) > 0 {
			if err := w.consent(strings.Join(names, " and "), mb); err != nil {
				return err
			}
		}
	}
	rel, err := w.fetch(ctx, gv)
	if err != nil {
		return err
	}
	w.logf("installing runsc %s into /usr/local/bin in %s", gv.Version, w.distro())
	if err := w.installGVisor(ctx, rel); err != nil {
		return err
	}
	out, err := w.inOut(ctx, bundle.Runsc{}.Version())
	if err != nil {
		return fmt.Errorf("installed runsc does not run: %w", err)
	}
	if got := runscVersion(out); got != gv.Version {
		return fmt.Errorf("installed runsc reports version %q, want %s", got, gv.Version)
	}
	return nil
}

// installGVisor streams runsc and gvisor-bin/ from the verified release
// into /usr/local/bin inside the distro.
func (w *WSL) installGVisor(ctx context.Context, release string) error {
	f, err := os.Open(release) // #nosec G304 -- our verified download
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := gvisorTar(f, pw)
		_ = pw.CloseWithError(err)
		errc <- err
	}()
	err = w.in(ctx, []string{"tar", "-x", "-f", "-", "-C", "/usr/local/bin"}, pr, nil)
	_ = pr.CloseWithError(errors.New("setup: extractor exited"))
	perr := <-errc
	if err != nil {
		return fmt.Errorf("install runsc: %w", err)
	}
	if perr != nil {
		return fmt.Errorf("install runsc: reading the release: %w", perr)
	}
	return nil
}

// verify checks every tool the backend runs, that no Windows file system is
// mounted in the distro, and finally runs a command under gVisor.
func (w *WSL) verify(ctx context.Context, gv Source) error {
	for _, argv := range bundle.DistroRequirements() {
		out, err := w.inOut(ctx, argv)
		if err != nil {
			return fmt.Errorf("verify %s in %s: %w", argv[0], w.distro(), err)
		}
		if argv[0] == "runsc" {
			if got := runscVersion(out); got != gv.Version {
				return fmt.Errorf("verify runsc in %s: version %q, want %s", w.distro(), got, gv.Version)
			}
		}
		if argv[0] == "runsc" || argv[0] == "ip" {
			w.logf("ok: %s", firstLine(out))
		}
	}
	mounts, err := w.inOut(ctx, []string{"cat", "/proc/mounts"})
	if err != nil {
		return fmt.Errorf("verify mounts: %w", err)
	}
	if m := windowsMounts(mounts); len(m) > 0 {
		return fmt.Errorf("file systems of the Windows host are still mounted in %s (%s); check /etc/wsl.conf and run `wsl --terminate %s`", w.distro(), strings.Join(m, ", "), w.distro())
	}
	w.logf("ok: no Windows file systems mounted")
	if !w.dockerClient(ctx) {
		w.logf("note: no Docker client found on Windows; jobs need one (Docker Desktop) to fetch their images")
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := w.in(sctx, runscSmoke, nil, nil); err != nil {
		return fmt.Errorf("gVisor cannot run in %s (%s): %w", w.distro(), strings.Join(runscSmoke, " "), err)
	}
	w.logf("ok: gVisor ran a command")
	r := probe(ctx, w.Backend, "windows")
	if w.Backend != nil && !r.Available {
		return fmt.Errorf("the %s backend still reports: %v", r.Backend, r.Details["reason"])
	}
	w.logf("sandbox distro %s is ready", w.distro())
	return nil
}

// dockerClient reports whether the Windows Docker client the backend uses
// to export images is installed.
func (w *WSL) dockerClient(ctx context.Context) bool {
	return w.Runner.Run(ctx, Cmd{Argv: bundle.Docker{}.Version()}) == nil
}

// windowsMounts lists mounts of Windows file systems (drvfs, 9p, virtiofs)
// anywhere in the distro, except WSL's own read-only driver store.
func windowsMounts(procMounts string) []string {
	var out []string
	for _, l := range strings.Split(procMounts, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || f[1] == "/usr/lib/wsl/drivers" {
			continue
		}
		switch f[2] {
		case "drvfs", "9p", "virtiofs":
			out = append(out, f[1])
		}
	}
	return out
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l)
}

// Status implements Platform.
func (w *WSL) Status(ctx context.Context) Report {
	w.defaults()
	r := probe(ctx, w.Backend, "windows")
	d := r.Details
	d["distro"] = w.distro()
	if err := w.Runner.Run(ctx, Cmd{Argv: []string{w.exe(), "--status"}}); err != nil {
		d["wsl"] = false
		d["hint"] = wslInstall
		return r
	}
	d["wsl"] = true
	d["docker_client"] = w.dockerClient(ctx)
	have, err := w.installed(ctx)
	d["distro_installed"] = have
	if err != nil {
		d["error"] = err.Error()
	}
	if err != nil || !have {
		d["hint"] = "run `mycel sandbox setup`"
		return r
	}
	if v, err := w.distroVersion(ctx); err == nil {
		d["wsl_version"] = v
	}
	if out, err := w.inOut(ctx, bundle.Runsc{}.Version()); err == nil {
		d["runsc"] = firstLine(out)
	} else {
		d["runsc"] = ""
	}
	if !r.Available {
		d["hint"] = "run `mycel sandbox setup`"
	}
	return r
}
