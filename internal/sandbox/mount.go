package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// ErrMount is wrapped by every ValidateMount/ResolveMount refusal.
var ErrMount = errors.New("sandbox: mount refused")

// ErrEnv is wrapped by every ValidateEnv refusal.
var ErrEnv = errors.New("sandbox: environment refused")

// Host roots that may never be mounted themselves (compared lower-case,
// without drive letter).
var forbiddenExact = []string{
	"/", "/var", "/library", "/private", "/private/var", "/users", "/home", "/opt", "/applications",
	"/tmp", "/private/tmp", "/var/tmp", "/volumes", "/mnt", "/media",
}

// Host trees that may not be mounted, nor anything below them.
var forbiddenTree = []string{
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/boot", "/dev", "/proc", "/sys",
	"/run", "/var/run", "/var/lib", "/var/root", "/root",
	"/private/etc", "/private/var/run", "/private/var/db", "/private/var/root",
	"/system", "/library/keychains",
	"/windows", "/program files", "/program files (x86)", "/programdata",
}

// Paths below the home directory that are never mounted, nor their parents.
var forbiddenHome = []string{
	".ssh", ".aws", ".gnupg", ".kube", ".docker", ".colima", ".orbstack", ".lima",
	".config/gcloud", "Library/Keychains",
}

// Container runtime sockets: a directory holding one is never mounted.
var runtimeSockets = []string{"docker.sock", "podman.sock", "containerd.sock"}

// Guest top-level directories a mount may not shadow ("lib*" by prefix).
var forbiddenGuest = map[string]bool{
	"proc": true, "sys": true, "dev": true, "etc": true, "usr": true, "bin": true,
	"sbin": true, "run": true, "boot": true,
}

var winDrivePath = regexp.MustCompile(`^[A-Za-z]:\\`)

func resolveMount(m Mount) (Mount, error) {
	raw := m.HostPath
	if raw == "" {
		return Mount{}, fmt.Errorf("%w: empty host path", ErrMount)
	}
	if runtime.GOOS == "windows" {
		if !winDrivePath.MatchString(raw) {
			return Mount{}, fmt.Errorf("%w: host path %q must be a drive path like C:\\dir", ErrMount, raw)
		}
	} else if !strings.HasPrefix(raw, "/") {
		return Mount{}, fmt.Errorf("%w: host path %q is not absolute", ErrMount, raw)
	}
	for _, seg := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return Mount{}, fmt.Errorf("%w: host path %q has a .. component", ErrMount, raw)
		}
	}
	homes := homeDirs()
	host := filepath.Clean(raw)
	if err := checkHostPath(host, homes); err != nil {
		return Mount{}, err
	}
	resolved, err := filepath.EvalSymlinks(host)
	if err != nil {
		return Mount{}, fmt.Errorf("%w: host path %q: %v", ErrMount, raw, err)
	}
	if err := checkHostPath(resolved, homes); err != nil {
		return Mount{}, fmt.Errorf("%w (resolved to %s)", err, resolved)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return Mount{}, fmt.Errorf("%w: host path %q: %v", ErrMount, raw, err)
	}
	if st.Mode()&fs.ModeSocket != 0 {
		return Mount{}, fmt.Errorf("%w: %q is a socket", ErrMount, raw)
	}
	if !st.IsDir() {
		return Mount{}, fmt.Errorf("%w: %q is not a directory", ErrMount, raw)
	}
	for _, s := range runtimeSockets {
		if _, err := os.Lstat(filepath.Join(resolved, s)); err == nil {
			return Mount{}, fmt.Errorf("%w: %q contains the container runtime socket %s", ErrMount, raw, s)
		}
	}

	g := m.GuestPath
	if g == "" || !path.IsAbs(g) {
		return Mount{}, fmt.Errorf("%w: guest path %q is not absolute", ErrMount, g)
	}
	g = path.Clean(g)
	if g == "/" {
		return Mount{}, fmt.Errorf("%w: guest path is /", ErrMount)
	}
	if g == WorkspaceGuestPath || strings.HasPrefix(g, WorkspaceGuestPath+"/") {
		return Mount{}, fmt.Errorf("%w: guest path %q is under %s", ErrMount, m.GuestPath, WorkspaceGuestPath)
	}
	top := strings.SplitN(strings.TrimPrefix(g, "/"), "/", 2)[0]
	if forbiddenGuest[top] || strings.HasPrefix(top, "lib") {
		return Mount{}, fmt.Errorf("%w: guest path %q shadows the system directory /%s", ErrMount, m.GuestPath, top)
	}
	return Mount{HostPath: resolved, GuestPath: g, ReadOnly: m.ReadOnly}, nil
}

// homeDirs is the user's home directory as given and symlink-resolved.
func homeDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	out := []string{home}
	if r, err := filepath.EvalSymlinks(home); err == nil && r != home {
		out = append(out, r)
	}
	return out
}

// norm turns a host path into a lower-case slash path without volume name,
// so one table serves unix and windows (and case-insensitive file systems).
func norm(p string) string {
	p = filepath.Clean(p)
	p = strings.TrimPrefix(p, filepath.VolumeName(p))
	p = filepath.ToSlash(p)
	if p == "" {
		p = "/"
	}
	return strings.ToLower(p)
}

// under reports whether p is root or below it.
func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

func checkHostPath(p string, homes []string) error {
	n := norm(p)
	for _, f := range forbiddenExact {
		if n == f {
			return fmt.Errorf("%w: %q is a system root", ErrMount, p)
		}
	}
	for _, f := range forbiddenTree {
		if under(n, f) {
			return fmt.Errorf("%w: %q is under the system directory %s", ErrMount, p, f)
		}
	}
	for _, s := range runtimeSockets {
		if strings.EqualFold(filepath.Base(p), s) {
			return fmt.Errorf("%w: container runtime sockets are never mounted", ErrMount)
		}
	}
	for _, h := range homes {
		nh := norm(h)
		if under(nh, n) {
			return fmt.Errorf("%w: %q is the home directory or contains it", ErrMount, p)
		}
		for _, s := range forbiddenHome {
			ns := norm(filepath.Join(h, filepath.FromSlash(s)))
			if under(n, ns) || under(ns, n) {
				return fmt.Errorf("%w: %q is or contains the private directory ~/%s", ErrMount, p, s)
			}
		}
	}
	return nil
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsProxyEnvKey reports whether k names a proxy variable in any case
// (*_PROXY, ALL_PROXY, NO_PROXY).
func IsProxyEnvKey(k string) bool {
	up := strings.ToUpper(k)
	return strings.HasSuffix(up, "_PROXY") || up == "ALL_PROXY" || up == "NO_PROXY"
}

func validateEnv(env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case !envKeyRE.MatchString(k):
			return fmt.Errorf("%w: invalid key %q", ErrEnv, k)
		case strings.HasPrefix(strings.ToUpper(k), "MYCEL_"):
			return fmt.Errorf("%w: key %q is reserved", ErrEnv, k)
		case IsProxyEnvKey(k):
			return fmt.Errorf("%w: proxy variable %q is set by the sandbox", ErrEnv, k)
		case strings.ContainsRune(env[k], 0):
			return fmt.Errorf("%w: value of %q contains NUL", ErrEnv, k)
		}
	}
	return nil
}
