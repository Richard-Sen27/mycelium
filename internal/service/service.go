package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/richard-sen27/mycelium/internal/config"
)

// Names of the installed unit on each platform.
const (
	Label       = "io.mycel.daemon" // launchd
	UnitName    = "mycel.service"   // systemd
	TaskName    = "Mycel"           // Task Scheduler
	LogFileName = "daemon.log"
)

// Roles accepted by Install. Both run `mycel daemon`, which starts every
// role initialised in the data directory (D006); the role is recorded in
// the unit's description.
const (
	RoleNode = "node"
	RoleHub  = "hub"
)

// ErrNotInstalled is returned by Uninstall, Start and Stop when no unit is
// installed.
var ErrNotInstalled = errors.New("the mycel service is not installed (run `mycel service install`)")

// Noter is implemented by managers that have a hint to print after a
// successful install (systemd: the linger hint).
type Noter interface {
	Notes() []string
}

// Runner runs an external command and returns its combined output.
type Runner interface {
	Run(name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed service tools (launchctl, systemctl, schtasks)
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// env is everything a manager needs from the machine; tests fill it by hand.
type env struct {
	home       string // MYCEL_HOME ("" when unset)
	logDir     string // directory of daemon.log
	userHome   string
	configHome string      // ~/.config (Linux; systemd always reads user units there)
	xdg        [][2]string // XDG_CONFIG_HOME / XDG_DATA_HOME when set, recorded in the unit without MYCEL_HOME
	uid        int
	username   string // DOMAIN\user (Windows)
	systemRoot string // %SystemRoot% (Windows)
	path       string // PATH recorded in the unit so the daemon finds limactl, docker, ollama
	exe        string // absolute path of the mycel binary to record
	tempDirs   []string
	run        Runner
	sleep      func(time.Duration)
	findPID    func(exe string) int
	killPID    func(pid int) error
}

// New returns the platform manager for the given MYCEL_HOME ("" when the
// default OS directories are used).
func New(home string) (Manager, error) {
	e, err := defaultEnv(home)
	if err != nil {
		return nil, err
	}
	return newManager(runtime.GOOS, e)
}

func newManager(goos string, e env) (Manager, error) {
	switch goos {
	case "darwin":
		return &launchd{e}, nil
	case "linux":
		return &systemd{e}, nil
	case "windows":
		return &schtasks{e}, nil
	default:
		return nil, fmt.Errorf("service: %s is not supported (run `mycel daemon` under your own supervisor)", goos)
	}
}

// LogDir returns the directory holding daemon.log: <MYCEL_HOME>/logs, or
// <data dir>/logs when MYCEL_HOME is unset.
func LogDir(home string) (string, error) {
	if home != "" {
		abs, err := filepath.Abs(home)
		if err != nil {
			return "", err
		}
		return filepath.Join(abs, "logs"), nil
	}
	d, err := config.Resolve()
	if err != nil {
		return "", err
	}
	return filepath.Join(d.Data, "logs"), nil
}

// LogFile returns the daemon log file the service writes.
func LogFile(home string) (string, error) {
	dir, err := LogDir(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, LogFileName), nil
}

func defaultEnv(home string) (env, error) {
	if home != "" {
		abs, err := filepath.Abs(home)
		if err != nil {
			return env{}, err
		}
		home = abs
	}
	logDir, err := LogDir(home)
	if err != nil {
		return env{}, err
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return env{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return env{}, fmt.Errorf("locate the mycel executable: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return env{}, err
	}
	cfgHome := filepath.Join(uh, ".config")
	var xdg [][2]string
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		if v := os.Getenv(k); v != "" && filepath.IsAbs(v) {
			xdg = append(xdg, [2]string{k, v})
		}
	}
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	return env{
		home:       home,
		logDir:     logDir,
		userHome:   uh,
		configHome: cfgHome,
		xdg:        xdg,
		uid:        os.Getuid(),
		username:   username,
		systemRoot: sysRoot,
		path:       os.Getenv("PATH"),
		exe:        exe,
		tempDirs:   tempDirs(),
		run:        execRunner{},
		sleep:      time.Sleep,
		findPID:    findDaemonPID,
		killPID:    killPID,
	}, nil
}

// tempDirs lists the directories an installed binary must not live in.
func tempDirs() []string {
	dirs := []string{os.TempDir(), os.Getenv("TMPDIR"), os.Getenv("TEMP"), os.Getenv("TMP")}
	if runtime.GOOS != "windows" {
		dirs = append(dirs, "/tmp", "/var/tmp", "/private/tmp", "/private/var/folders", "/var/folders")
	}
	var out []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		out = append(out, d)
		if r, err := filepath.EvalSymlinks(d); err == nil && r != d {
			out = append(out, r)
		}
	}
	return out
}

// checkRole validates the --role value; "" means node.
func checkRole(role string) (string, error) {
	switch role {
	case "", RoleNode:
		return RoleNode, nil
	case RoleHub:
		return RoleHub, nil
	default:
		return "", fmt.Errorf("unknown role %q (want node or hub)", role)
	}
}

// checkExecutable refuses a binary the unit cannot rely on: a relative path
// or one inside a temporary directory (`go run`, a download folder that gets
// cleaned).
func (e env) checkExecutable() error {
	if e.exe == "" || !isAbs(e.exe) {
		return fmt.Errorf("the mycel executable path %q is not absolute", e.exe)
	}
	resolved := e.exe
	if r, err := filepath.EvalSymlinks(e.exe); err == nil {
		resolved = r
	}
	for _, d := range e.tempDirs {
		for _, p := range []string{e.exe, resolved} {
			if within(p, d) {
				return fmt.Errorf("refusing to install %s: it is inside the temporary directory %s; install mycel first (scripts/install.sh or install.ps1) and run `mycel service install` from there", e.exe, d)
			}
		}
	}
	return nil
}

func (e env) logFile() string { return joinPath(e.logDir, LogFileName) }

// isAbs accepts both POSIX and Windows absolute paths so the renderers can
// be tested on any OS.
func isAbs(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// within reports whether p is dir or below it (case-insensitive for Windows
// style paths).
func within(p, dir string) bool {
	dir = strings.TrimRight(dir, `/\`)
	if dir == "" {
		return false
	}
	norm := func(s string) string {
		if strings.Contains(s, `\`) || (len(s) > 1 && s[1] == ':') {
			return strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
		}
		return s
	}
	p, dir = norm(p), norm(dir)
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// joinPath joins with the separator of base (so Windows paths render on any
// OS in tests).
func joinPath(base string, elem ...string) string {
	sep := "/"
	if strings.Contains(base, `\`) {
		sep = `\`
	}
	out := strings.TrimRight(base, `/\`)
	for _, e := range elem {
		out += sep + e
	}
	return out
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // ~/Library/LaunchAgents and ~/.config/systemd/user are world-readable by convention
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
