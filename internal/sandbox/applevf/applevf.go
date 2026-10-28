//go:build darwin

package applevf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// Timeouts and sizes.
const (
	startTimeout       = 10 * time.Minute // includes provisioning (apt for docker-nested)
	configureTimeout   = 5 * time.Minute
	shortCallTimeout   = 2 * time.Minute
	versionTimeout     = 15 * time.Second
	stopTimeout        = time.Minute
	deleteTimeout      = 2 * time.Minute
	listTimeout        = 30 * time.Second
	waitDelay          = 5 * time.Second
	maxBufferedOutput  = 8 << 20
	minMacOSMajor      = 13
	minLimaMajor       = 2
	instanceNamePrefix = "sbx-"
	maxUnixSocketPath  = 103 // sun_path is 104 bytes on macOS, NUL included
)

func init() { sandbox.Register(New()) }

// Backend is the macOS backend: one Lima VM (Apple Virtualization.framework,
// vmType vz) per sandbox.
type Backend struct {
	log       *slog.Logger
	lookPath  func(string) (string, error)
	osVersion func() (string, error)
	goarch    string
	newRunner func(limactl string, env []string, spillDir string) runner
	cacheDir  func() (string, error)
	http      *http.Client

	homeMu   sync.Mutex
	limaHome string // "" = <node data dir>/lima

	prepMu   sync.Mutex
	prepLock map[string]*sync.Mutex // per image URL
}

// New returns the backend with the real limactl.
func New() *Backend {
	return &Backend{
		log:       slog.Default().With("backend", sandbox.BackendAppleVF),
		lookPath:  exec.LookPath,
		osVersion: func() (string, error) { return unix.Sysctl("kern.osproductversion") },
		goarch:    runtime.GOARCH,
		newRunner: func(p string, env []string, spill string) runner {
			return limactlRunner{path: p, env: env, spillDir: spill}
		},
		cacheDir: limaCacheDir,
		http:     &http.Client{},
		prepLock: map[string]*sync.Mutex{},
	}
}

// SetLimaHome sets the dedicated LIMA_HOME of this backend's instances. The
// default is <node data dir>/lima (config.Dirs.NodeDir). The user's own
// ~/.lima is never used.
func (b *Backend) SetLimaHome(dir string) {
	b.homeMu.Lock()
	b.limaHome = dir
	b.homeMu.Unlock()
}

func (b *Backend) limaHomeDir() (string, error) {
	b.homeMu.Lock()
	h := b.limaHome
	b.homeMu.Unlock()
	if h == "" {
		d, err := config.Resolve()
		if err != nil {
			return "", fmt.Errorf("applevf: resolve data dir: %w", err)
		}
		h = filepath.Join(d.NodeDir(), "lima")
	}
	if !filepath.IsAbs(h) {
		return "", fmt.Errorf("applevf: LIMA_HOME %q is not absolute", h)
	}
	return filepath.Clean(h), nil
}

// checkLimaHome refuses a LIMA_HOME that could change the generated
// configuration (Lima merges _config/default.yaml and override.yaml into
// every instance) or whose instance sockets would exceed the unix socket
// path limit.
func checkLimaHome(home string) error {
	for _, f := range []string{"override.yaml", "default.yaml"} {
		p := filepath.Join(home, "_config", f)
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%w: applevf: %s exists; it would alter every sandbox VM, remove it", sandbox.ErrUnavailable, p)
		}
	}
	// Lima's own check: <home>/<instance>/ssh.sock plus the 17 bytes ssh
	// appends to its control socket, on the symlink-resolved path.
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	probe := filepath.Join(home, instanceNamePrefix+strings.Repeat("0", 12), "ssh.sock.1234567890123456")
	if len(probe) > maxUnixSocketPath {
		return fmt.Errorf("%w: applevf: LIMA_HOME %s is too long for Lima's unix sockets (%d > %d bytes); use a shorter MYCEL_HOME", sandbox.ErrUnavailable, home, len(probe), maxUnixSocketPath)
	}
	return nil
}

// limaEnv is the complete environment of every limactl call: LIMA_HOME plus a
// short allowlist from the daemon's environment. In particular $SSH (which
// would replace the ssh binary) and all *_proxy variables are dropped.
func limaEnv(home string) []string {
	env := []string{"LIMA_HOME=" + home}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LIMA_CACHE_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if _, ok := os.LookupEnv("PATH"); !ok {
		env = append(env, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	}
	return env
}

// tool resolves limactl and the LIMA_HOME of this backend.
func (b *Backend) tool() (path, home string, err error) {
	path, err = b.lookPath("limactl")
	if err != nil {
		return "", "", fmt.Errorf("%w: applevf: limactl (Lima) is not installed or not on PATH: install it with `brew install lima`", sandbox.ErrUnavailable)
	}
	home, err = b.limaHomeDir()
	if err != nil {
		return "", "", err
	}
	if err := checkLimaHome(home); err != nil {
		return "", "", err
	}
	return path, home, nil
}

// Name implements sandbox.Backend.
func (b *Backend) Name() string { return sandbox.BackendAppleVF }

// Available implements sandbox.Backend: arm64 or amd64, macOS 13 or later,
// limactl 2.x on PATH, a usable LIMA_HOME.
func (b *Backend) Available(ctx context.Context) (bool, string) {
	if _, err := limaArch(b.goarch); err != nil {
		return false, err.Error()
	}
	v, err := b.osVersion()
	if err != nil {
		return false, "cannot read the macOS version: " + err.Error()
	}
	major, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	if n, err := strconv.Atoi(major); err != nil || n < minMacOSMajor {
		return false, fmt.Sprintf("macOS %s is too old: Lima with Virtualization.framework needs macOS %d or later", v, minMacOSMajor)
	}
	path, home, err := b.tool()
	if err != nil {
		return false, err.Error()
	}
	vctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, _, err := b.newRunner(path, limaEnv(home), "").Run(vctx, nil, "--version")
	if err != nil {
		return false, "limactl --version failed: " + err.Error()
	}
	if ver, ok := limaVersionOK(string(out)); !ok {
		return false, fmt.Sprintf("Lima %s is too old: version %d or later is required", ver, minLimaMajor)
	}
	return true, ""
}

// limaVersionOK parses "limactl version X.Y.Z" and checks the major version.
func limaVersionOK(out string) (string, bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) == 0 {
		return "?", false
	}
	ver := strings.TrimPrefix(f[len(f)-1], "v")
	major, _, _ := strings.Cut(ver, ".")
	n, err := strconv.Atoi(major)
	return ver, err == nil && n >= minLimaMajor
}

// Capabilities implements sandbox.Backend.
func (b *Backend) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		Egress:        true,
		NestedDocker:  true,
		ProcessLimits: true,
		MemoryLimit:   true,
		CPULimit:      true,
		Isolation:     "vm",
	}
}

// Prepare implements sandbox.Backend: it warms Lima's download cache with the
// pinned cloud image for the profile's image reference (see cache.go).
func (b *Backend) Prepare(ctx context.Context, image string, progress func(string)) error {
	img, err := resolveImage(image, b.goarch)
	if err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	return b.warmCache(ctx, img, progress)
}

// Create implements sandbox.Backend.
func (b *Backend) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	path, home, err := b.tool()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("applevf: create LIMA_HOME: %w", err)
	}
	return b.create(ctx, b.newRunner(path, limaEnv(home), spec.Dir), home, spec)
}

func (b *Backend) create(ctx context.Context, r runner, home string, spec sandbox.Spec) (sandbox.Sandbox, error) {
	inst, err := instanceName(spec.ID)
	if err != nil {
		return nil, err
	}
	if spec.Dir == "" || !filepath.IsAbs(spec.Dir) {
		return nil, fmt.Errorf("applevf: sandbox dir %q must be an absolute path", spec.Dir)
	}
	if err := checkRemovable(spec.Dir, home); err != nil {
		return nil, err
	}
	if len(spec.Mounts) > 0 {
		return nil, fmt.Errorf("%w: applevf: host mounts are not supported (the VM gets no host mounts in v1)", sandbox.ErrUnavailable)
	}
	ref := spec.Image
	if ref == "" {
		ref = spec.Profile.GetImage()
	}
	img, err := resolveImage(ref, b.goarch)
	if err != nil {
		return nil, err
	}
	proxy, proxyPort, err := guestProxyURL(spec.Egress.ProxyURL)
	if err != nil {
		return nil, err
	}
	docker := wantsDockerNested(spec.Profile.GetBrokers())
	baseEnv, err := mergeEnv(spec.Env)
	if err != nil {
		return nil, err
	}
	data, err := renderLimaYAML(vmParams{Image: img, Limits: spec.Limits, ProxyPort: proxyPort, Docker: docker})
	if err != nil {
		return nil, err
	}
	yamlPath, err := writeLimaYAML(spec.Dir, data)
	if err != nil {
		return nil, err
	}

	s := &limaSandbox{
		id:      spec.ID,
		inst:    inst,
		dir:     spec.Dir,
		home:    home,
		r:       r,
		log:     b.log.With("sandbox", spec.ID, "instance", inst),
		env:     baseEnv,
		wall:    spec.Limits.WallClock,
		procs:   map[*process]struct{}{},
		created: time.Now(),
	}
	fail := func(err error) (sandbox.Sandbox, error) {
		if derr := s.Destroy(context.WithoutCancel(ctx)); derr != nil {
			s.log.Warn("applevf: cleanup after failed create", "err", derr)
		}
		return nil, err
	}

	start := time.Now()
	s.log.Info("applevf: starting VM", "image", img.Location, "cpus", spec.Limits.CPUs, "memory", spec.Limits.MemoryBytes, "docker", docker)
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	_, stderr, err := r.Run(sctx, nil, "start", "--tty=false", "--timeout="+startTimeout.String(), "--name="+inst, yamlPath)
	cancel()
	if err != nil {
		return fail(cmdError("limactl start", err, stderr))
	}
	s.log.Info("applevf: VM running", "elapsed", time.Since(start).Round(time.Millisecond))

	mode := "nodocker"
	if docker {
		mode = "docker"
	}
	cctx, cancel := context.WithTimeout(ctx, configureTimeout)
	// The proxy URL (with its credential) travels on stdin, never in argv.
	_, stderr, err = r.Run(cctx, strings.NewReader(proxy+"\n"),
		"shell", "--tty=false", "--shell="+guestShell, "--workdir=/", inst, "--", "sudo", "-n", guestConfigure, guestUser, mode)
	cancel()
	if err != nil {
		return fail(cmdError("configure guest", err, stderr))
	}
	// --reconnect: Lima keeps one ssh ControlMaster per instance, opened
	// during boot; its sessions keep the login shell and sshd settings of
	// that time (/bin/bash, which reads ~/.bashrc). A new master picks up the
	// mycel shell and PermitUserRC no for every later call.
	cctx, cancel = context.WithTimeout(ctx, shortCallTimeout)
	checkArgs := slices.Insert(s.helperArgs("check"), 2, "--reconnect")
	_, stderr, err = r.Run(cctx, nil, checkArgs...)
	cancel()
	if err != nil {
		return fail(cmdError("guest lockdown check", err, stderr))
	}
	if err := s.seed(ctx, spec.Workspace); err != nil {
		return fail(err)
	}
	s.created = time.Now()
	s.log.Info("applevf: sandbox ready", "elapsed", time.Since(start).Round(time.Millisecond))
	return s, nil
}

// instanceName maps a sandbox id to a short Lima instance name,
// "sbx-<first 12 hex digits of sha256(id)>": short enough for Lima's unix
// socket paths under a LIMA_HOME in the data directory.
func instanceName(id string) (string, error) {
	if id == "" || len(id) > 128 || strings.ContainsRune(id, 0) {
		return "", fmt.Errorf("applevf: invalid sandbox id %q", id)
	}
	sum := sha256.Sum256([]byte(id))
	return instanceNamePrefix + hex.EncodeToString(sum[:6]), nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are names a caller may not set: the proxy variables come only
// from the guest's /etc/environment (any case).
var reservedEnv = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true, "FTP_PROXY": true,
}

// mergeEnv merges maps left to right after validating names and values.
func mergeEnv(maps ...map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			if !envKeyRe.MatchString(k) {
				return nil, fmt.Errorf("applevf: invalid environment variable name %q", k)
			}
			if reservedEnv[strings.ToUpper(k)] {
				return nil, fmt.Errorf("applevf: environment variable %s is reserved for the egress proxy", k)
			}
			if strings.ContainsRune(v, 0) {
				return nil, fmt.Errorf("applevf: environment variable %s contains a NUL byte", k)
			}
			out[k] = v
		}
	}
	return out, nil
}

// checkRemovable refuses sandbox directories whose removal would be
// catastrophic: the root, the home directory or an ancestor of it, LIMA_HOME
// or an ancestor of it, or anything with fewer than three path elements.
func checkRemovable(dir, limaHome string) error {
	c := filepath.Clean(dir)
	if !filepath.IsAbs(c) || len(strings.Split(strings.Trim(c, "/"), "/")) < 3 {
		return fmt.Errorf("applevf: refusing sandbox dir %q", dir)
	}
	protected := []string{limaHome}
	if h, err := os.UserHomeDir(); err == nil {
		protected = append(protected, h)
	}
	for _, p := range protected {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if c == p || strings.HasPrefix(p+"/", c+"/") {
			return fmt.Errorf("applevf: refusing sandbox dir %q (contains %s)", dir, p)
		}
	}
	return nil
}

// errDestroyed is returned by operations on a destroyed sandbox.
var errDestroyed = errors.New("applevf: sandbox destroyed")

// limaCacheDir is Lima's download cache: $LIMA_CACHE_HOME, else
// ~/Library/Caches/lima on macOS.
func limaCacheDir() (string, error) {
	if d := os.Getenv("LIMA_CACHE_HOME"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "lima"), nil
}
