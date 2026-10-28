package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

const gvisorInstall = "install gVisor from https://gvisor.dev/docs/user_guide/install/ (runsc on PATH), or run `mycel sandbox setup --yes` to download gVisor " + GVisorVersion + " (into /usr/local/bin as root, ~/.local/bin otherwise)"

// Linux checks the gVisor backend's host requirements.
type Linux struct {
	Options
	Backend    sandbox.Backend
	BinDir     string // where --yes installs runsc for a normal user (~/.local/bin)
	RootBinDir string // where --yes installs runsc as root ("/usr/local/bin" when empty)
	GOARCH     string
	LookPath   func(string) (string, error)
	ReadFile   func(string) ([]byte, error)
	Geteuid    func() int          // os.Geteuid when nil
	Getenv     func(string) string // os.Getenv when nil
}

func (l *Linux) root() bool {
	if l.Geteuid == nil {
		return os.Geteuid() == 0
	}
	return l.Geteuid() == 0
}

func (l *Linux) getenv(k string) string {
	if l.Getenv == nil {
		return os.Getenv(k)
	}
	return l.Getenv(k)
}

// installDir is where --yes installs runsc.
func (l *Linux) installDir() string {
	if l.root() {
		if l.RootBinDir == "" {
			return "/usr/local/bin"
		}
		return l.RootBinDir
	}
	return l.BinDir
}

// runsc finds runsc on PATH or in the install directory.
func (l *Linux) runsc() string {
	if p, err := l.LookPath("runsc"); err == nil {
		return p
	}
	if d := l.installDir(); d != "" {
		p := filepath.Join(d, "runsc")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

func (l *Linux) onPath(dir string) bool {
	for _, p := range filepath.SplitList(l.getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

func (l *Linux) cgroupV2() bool {
	_, err := l.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

func (l *Linux) userNamespaces() bool {
	b, err := l.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && n > 0
}

// Setup implements Platform.
func (l *Linux) Setup(ctx context.Context) error {
	l.defaults()
	path := l.runsc()
	if path == "" {
		if !l.Yes {
			return &PrereqError{Missing: "runsc (gVisor) is not installed", Instruction: gvisorInstall}
		}
		src, ok := GVisor[l.GOARCH]
		if !ok {
			return &PrereqError{Missing: "no pinned gVisor release for " + l.GOARCH, Instruction: "install gVisor from https://gvisor.dev/docs/user_guide/install/"}
		}
		dir := l.installDir()
		if dir == "" {
			return &PrereqError{Missing: "no directory to install runsc into", Instruction: gvisorInstall}
		}
		rel, err := l.fetch(ctx, src)
		if err != nil {
			return err
		}
		f, err := os.Open(rel) // #nosec G304 -- our verified download
		if err != nil {
			return err
		}
		err = installGVisorLocal(f, dir)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("install runsc into %s: %w", dir, err)
		}
		path = filepath.Join(dir, "runsc")
		l.logf("installed runsc %s and gvisor-bin/ into %s", src.Version, dir)
		if !l.root() {
			if !l.onPath(dir) {
				l.logf("note: %s is not on your PATH; add it (export PATH=\"%s:$PATH\" in your shell profile)", dir, dir)
			}
			l.logf("note: the gVisor backend runs runsc as root; install it for root too: sudo cp -r %s %s /usr/local/bin/",
				path, filepath.Join(dir, "gvisor-bin"))
		}
	}
	out, err := l.output(ctx, []string{path, "--version"})
	if err != nil {
		return fmt.Errorf("%s --version: %w", path, err)
	}
	l.logf("ok: %s", firstLine(string(out)))

	var problems []string
	if l.cgroupV2() {
		l.logf("ok: cgroup v2")
	} else {
		problems = append(problems, "cgroup v2 is not mounted at /sys/fs/cgroup (boot with systemd.unified_cgroup_hierarchy=1)")
	}
	if l.userNamespaces() {
		l.logf("ok: user namespaces")
	} else {
		problems = append(problems, "user namespaces are disabled (sysctl user.max_user_namespaces=0)")
	}
	if len(problems) > 0 {
		return &PrereqError{Missing: "the kernel is not ready for gVisor", Instruction: strings.Join(problems, "; ")}
	}
	r := probe(ctx, l.Backend, "linux")
	if l.Backend != nil && !r.Available {
		return fmt.Errorf("the %s backend is not available yet: %v", r.Backend, r.Details["reason"])
	}
	l.logf("the gVisor backend is ready")
	return nil
}

// Status implements Platform.
func (l *Linux) Status(ctx context.Context) Report {
	l.defaults()
	r := probe(ctx, l.Backend, "linux")
	d := r.Details
	d["runsc"] = ""
	if path := l.runsc(); path != "" {
		if out, err := l.output(ctx, []string{path, "--version"}); err == nil {
			d["runsc"] = firstLine(string(out))
		}
	}
	d["cgroup_v2"] = l.cgroupV2()
	d["user_namespaces"] = l.userNamespaces()
	if d["runsc"] == "" {
		d["hint"] = gvisorInstall
	} else if !r.Available {
		d["hint"] = "run `mycel sandbox setup`"
	}
	return r
}
