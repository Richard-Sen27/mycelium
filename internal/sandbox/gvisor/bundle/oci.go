package bundle

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// The OCI runtime spec types this package needs (a subset of
// opencontainers/runtime-spec, kept local to avoid a dependency).

// OCISpec is config.json.
type OCISpec struct {
	Version  string     `json:"ociVersion"`
	Process  *Process   `json:"process,omitempty"`
	Root     *Root      `json:"root,omitempty"`
	Hostname string     `json:"hostname,omitempty"`
	Mounts   []OCIMount `json:"mounts"`
	Linux    *Linux     `json:"linux,omitempty"`
}

// Process is the container (or exec) process.
type Process struct {
	Terminal        bool          `json:"terminal"`
	User            User          `json:"user"`
	Args            []string      `json:"args"`
	Env             []string      `json:"env,omitempty"`
	Cwd             string        `json:"cwd"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
	Rlimits         []Rlimit      `json:"rlimits,omitempty"`
	NoNewPrivileges bool          `json:"noNewPrivileges"`
}

// User is the uid/gid inside the sandbox.
type User struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// Capabilities are the Linux capability sets (as seen by the gVisor sentry).
type Capabilities struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Permitted   []string `json:"permitted"`
	Inheritable []string `json:"inheritable"`
}

// Rlimit is one POSIX rlimit.
type Rlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

// Root is the root filesystem.
type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

// OCIMount is one mount.
type OCIMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

// Linux is the Linux section.
type Linux struct {
	CgroupsPath   string      `json:"cgroupsPath,omitempty"`
	Resources     *Resources  `json:"resources,omitempty"`
	Namespaces    []Namespace `json:"namespaces"`
	Devices       []Device    `json:"devices"`
	MaskedPaths   []string    `json:"maskedPaths"`
	ReadonlyPaths []string    `json:"readonlyPaths"`
}

// Device is a device node (always empty here: no devices).
type Device struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// Namespace is one namespace; Path joins an existing one.
type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// Resources are the cgroup limits.
type Resources struct {
	Devices []DeviceCgroup `json:"devices,omitempty"`
	Memory  *Memory        `json:"memory,omitempty"`
	CPU     *CPU           `json:"cpu,omitempty"`
	Pids    *Pids          `json:"pids,omitempty"`
}

// DeviceCgroup is a device cgroup rule.
type DeviceCgroup struct {
	Allow  bool   `json:"allow"`
	Access string `json:"access"`
}

// Memory limits in bytes.
type Memory struct {
	Limit int64 `json:"limit"`
	Swap  int64 `json:"swap"`
}

// CPU quota per period, in microseconds.
type CPU struct {
	Quota  int64  `json:"quota"`
	Period uint64 `json:"period"`
}

// Pids caps the number of tasks.
type Pids struct {
	Limit int64 `json:"limit"`
}

// CPUPeriod is the CFS period used for the CPU quota.
const CPUPeriod = 100000

// NofileLimit is the open-files rlimit inside sandboxes.
const NofileLimit = 65536

// DefaultPath is PATH inside sandboxes.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// WorkspaceDir is the guest working directory.
const WorkspaceDir = "/workspace"

// DefaultInit keeps the container alive; commands run through runsc exec.
var DefaultInit = []string{"sleep", "infinity"}

// MaskedPaths and ReadonlyPaths are the OCI (runc spec) defaults. Under
// gVisor they are mostly no-ops: /proc and /sys are the sentry's own
// emulation, not the host's, and the same holds for an OCI seccomp profile
// (none is set): the boundary is the sentry and its host seccomp filter, not
// these settings. They are kept so the bundle stays valid for other runtimes.
var (
	MaskedPaths = []string{
		"/proc/acpi", "/proc/asound", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats",
		"/proc/sched_debug", "/proc/scsi", "/sys/firmware",
	}
	ReadonlyPaths = []string{
		"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
	}
	// DefaultCaps is the capability set (bounding, permitted, effective):
	// Docker's default minus NET_RAW, MKNOD, SETFCAP, SETPCAP, AUDIT_WRITE,
	// CHOWN and FSETID. They apply to the sentry's emulated kernel; the
	// default workload user (1000) has none of them effective anyway.
	DefaultCaps = []string{
		"CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_SETGID", "CAP_SETUID",
		"CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL",
	}
)

// DefaultUser is the uid:gid workloads run as.
var DefaultUser = User{UID: 1000, GID: 1000}

// DefaultTmpBytes bounds /tmp when the sandbox has no memory limit.
const DefaultTmpBytes = 512 << 20

// ConfigInput is what OCIConfig needs besides the Spec.
type ConfigInput struct {
	Spec        sandbox.Spec
	Rootfs      string   // absolute rootfs (shared lower) path on the sandbox host
	Workspace   string   // absolute per-sandbox workspace dir on the sandbox host
	Netns       string   // network namespace path, "" for none
	ProxyURL    string   // proxy URL as reachable from the sandbox, "" for no egress
	Init        []string // container init argv; DefaultInit when empty
	Hostname    string
	CgroupsPath string // e.g. /mycel/<container id>; required for the limits to apply
	User        *User  // DefaultUser when nil
}

// HomeFor is HOME for a user: /root for root, /tmp otherwise.
func HomeFor(u User) string {
	if u.UID == 0 {
		return "/root"
	}
	return "/tmp"
}

// TmpSize bounds the /tmp tmpfs: half the memory limit (it is memory-backed
// and counts against that limit), DefaultTmpBytes without a limit.
func TmpSize(l sandbox.Limits) int64 {
	if l.MemoryBytes > 0 {
		return l.MemoryBytes / 2
	}
	return DefaultTmpBytes
}

// OCIConfig builds the OCI runtime config for one sandbox. It refuses
// Spec.Mounts: the only host directory a sandbox sees is its own workspace.
func OCIConfig(in ConfigInput) (*OCISpec, error) {
	if in.Rootfs == "" || in.Workspace == "" {
		return nil, errors.New("bundle: rootfs and workspace paths are required")
	}
	if in.CgroupsPath == "" {
		return nil, errors.New("bundle: cgroups path is required")
	}
	if len(in.Spec.Mounts) > 0 {
		return nil, errors.New("bundle: host mount grants are not supported by the gVisor backends in v1")
	}
	init := in.Init
	if len(init) == 0 {
		init = DefaultInit
	}
	user := DefaultUser
	if in.User != nil {
		user = *in.User
	}
	caps := append([]string(nil), DefaultCaps...)
	proc := &Process{
		User: user,
		Args: append([]string(nil), init...),
		Env:  Env(in.Spec.Env, in.ProxyURL, HomeFor(user)),
		Cwd:  WorkspaceDir,
		Capabilities: &Capabilities{
			Bounding: caps, Effective: caps, Permitted: caps, Inheritable: []string{},
		},
		Rlimits:         Rlimits(in.Spec.Limits),
		NoNewPrivileges: true,
	}
	ns := []Namespace{{Type: "pid"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"}, {Type: "network", Path: in.Netns}}
	return &OCISpec{
		Version:  "1.0.2",
		Process:  proc,
		Root:     &Root{Path: in.Rootfs},
		Hostname: in.Hostname,
		Mounts: []OCIMount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", fmt.Sprintf("size=%d", TmpSize(in.Spec.Limits))}},
			{Destination: WorkspaceDir, Type: "bind", Source: in.Workspace, Options: []string{"rbind", "rw", "nosuid", "nodev"}},
		},
		Linux: &Linux{
			CgroupsPath:   in.CgroupsPath,
			Resources:     OCIResources(in.Spec.Limits),
			Namespaces:    ns,
			Devices:       []Device{},
			MaskedPaths:   append([]string(nil), MaskedPaths...),
			ReadonlyPaths: append([]string(nil), ReadonlyPaths...),
		},
	}, nil
}

// Rlimits maps limits to rlimits: NPROC = max_processes, NOFILE fixed.
func Rlimits(l sandbox.Limits) []Rlimit {
	out := []Rlimit{{Type: "RLIMIT_NOFILE", Hard: NofileLimit, Soft: NofileLimit}}
	if l.MaxProcesses > 0 {
		n := uint64(l.MaxProcesses)
		out = append(out, Rlimit{Type: "RLIMIT_NPROC", Hard: n, Soft: n})
	}
	return out
}

// OCIResources maps limits to linux.resources (memory, CPU quota, pids) and
// denies all devices.
func OCIResources(l sandbox.Limits) *Resources {
	r := &Resources{Devices: []DeviceCgroup{{Allow: false, Access: "rwm"}}}
	if l.MemoryBytes > 0 {
		r.Memory = &Memory{Limit: l.MemoryBytes, Swap: l.MemoryBytes}
	}
	if l.CPUs > 0 {
		r.CPU = &CPU{Quota: int64(l.CPUs) * CPUPeriod, Period: CPUPeriod}
	}
	if l.MaxProcesses > 0 {
		r.Pids = &Pids{Limit: int64(l.MaxProcesses)}
	}
	return r
}

// proxyKeys are the variables pointing clients at the egress proxy; they
// always win over Spec.Env.
var proxyKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy"}

func isProxyKey(k string) bool {
	for _, p := range proxyKeys {
		if strings.EqualFold(k, p) {
			return true
		}
	}
	return false
}

// Env builds the sandbox environment: PATH and HOME defaults, the spec's
// variables in key order (they may override PATH and HOME), then the proxy
// variables when proxyURL is set.
func Env(env map[string]string, proxyURL, home string) []string {
	vals := map[string]string{"PATH": DefaultPath, "HOME": home}
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || isProxyKey(k) {
			continue
		}
		vals[k] = v
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)+6)
	for _, k := range keys {
		out = append(out, k+"="+vals[k])
	}
	if proxyURL != "" {
		out = append(out,
			"HTTP_PROXY="+proxyURL, "HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL, "https_proxy="+proxyURL,
			"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1")
	}
	return out
}

// RewriteProxyURL replaces the host of the proxy URL with host, keeping the
// scheme, credentials and port. The port is required.
func RewriteProxyURL(proxyURL, host string) (string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("bundle: proxy URL needs a scheme and host:port")
	}
	port := u.Port()
	if port == "" {
		return "", errors.New("bundle: proxy URL has no port")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host + ":" + port
	return u.String(), nil
}

// ProxyPort returns the port of the proxy URL.
func ProxyPort(proxyURL string) (string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "", err
	}
	if u.Port() == "" {
		return "", errors.New("bundle: proxy URL has no port")
	}
	return u.Port(), nil
}
