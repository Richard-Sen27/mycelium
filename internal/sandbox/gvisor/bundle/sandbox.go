package bundle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// destroyTimeout bounds one Destroy (it ignores the caller's cancellation).
const destroyTimeout = 2 * time.Minute

// Sandbox is one runsc container.
type Sandbox struct {
	b        *Backend
	spec     sandbox.Spec
	cid      string
	dir      string
	user     User
	runsc    Runsc
	net      *Net // nil without egress
	proxyURL string
	created  time.Time
	deadline time.Time // zero without a wall clock limit
	timer    *time.Timer

	seq       atomic.Int64
	running   atomic.Int64
	destroyed atomic.Bool // Destroy started: running processes report ErrKilled
	timedOut  atomic.Bool // the wall clock fired: running processes report ErrTimeout
	mu        sync.Mutex
	gone      bool // Destroy finished
}

var _ sandbox.Sandbox = (*Sandbox)(nil)

// Create implements sandbox.Backend.
func (b *Backend) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	cid, err := ContainerID(spec.ID)
	if err != nil {
		return nil, err
	}
	if len(spec.Mounts) > 0 {
		return nil, errors.New("bundle: host mount grants are not supported by the gVisor backends in v1")
	}
	b.gcOnce.Do(func() { _ = b.GC(ctx) })
	b.mu.Lock()
	dup := b.live[cid]
	b.mu.Unlock()
	if dup {
		return nil, fmt.Errorf("bundle: sandbox %s already exists", spec.ID)
	}
	lower, ok := b.imagePath(spec.Image)
	if !ok {
		if err := b.Prepare(ctx, spec.Image, nil); err != nil {
			return nil, err
		}
		lower, _ = b.imagePath(spec.Image)
	}
	dir := b.cfg.StateDir + "/" + cid
	s := &Sandbox{
		b:       b,
		spec:    spec,
		cid:     cid,
		dir:     dir,
		user:    *b.cfg.User,
		runsc:   b.runsc("none", "dir="+dir+"/overlay"),
		created: b.cfg.Now(),
	}
	if spec.Limits.WallClock > 0 {
		s.deadline = s.created.Add(spec.Limits.WallClock)
	}
	b.setLive(cid, true)
	if err := s.create(ctx, lower); err != nil {
		_ = s.Destroy(ctx)
		return nil, err
	}
	if !s.deadline.IsZero() {
		s.timer = time.AfterFunc(s.deadline.Sub(b.cfg.Now()), s.expire)
	}
	return s, nil
}

func (s *Sandbox) create(ctx context.Context, lower string) error {
	b := s.b
	ws := s.dir + "/workspace"
	if err := b.run(ctx, MkdirArgv(s.dir, s.dir+"/proc", s.dir+"/overlay"), nil, nil); err != nil {
		return err
	}
	if err := b.run(ctx, MkdirModeArgv(0o755, ws), nil, nil); err != nil {
		return err
	}
	if err := b.run(ctx, ChownArgv(s.user, ws), nil, nil); err != nil {
		return err
	}
	netns := ""
	if s.spec.Egress.ProxyURL != "" {
		if err := s.setupNet(ctx); err != nil {
			return err
		}
		netns = s.net.NetnsPath()
		s.runsc.Network = "sandbox"
	}
	user := s.user
	cfg, err := OCIConfig(ConfigInput{
		Spec:        s.spec,
		Rootfs:      lower,
		Workspace:   ws,
		Netns:       netns,
		ProxyURL:    s.proxyURL,
		Init:        b.cfg.Init,
		Hostname:    "sandbox",
		CgroupsPath: b.cfg.CgroupParent + "/" + s.cid,
		User:        &user,
	})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := s.writePrivate(ctx, s.dir+"/config.json", data); err != nil {
		return err
	}
	if err := s.start(ctx); err != nil {
		return fmt.Errorf("bundle: runsc run: %w", err)
	}
	conf := "# names are resolved by the egress proxy\n"
	if s.net != nil && s.net.DNS.IsValid() {
		conf = "nameserver " + s.net.DNS.String() + "\n"
	}
	if err := s.guestAs(ctx, User{}, []string{"dd", "of=/etc/resolv.conf", "status=none"}, strings.NewReader(conf), nil); err != nil {
		return fmt.Errorf("bundle: write resolv.conf: %w", err)
	}
	return s.seed(ctx)
}

// start runs `runsc run --detach`. On Linux its stdio is the null device;
// on WSL it goes through DetachScript (setsid, stdio on /dev/null inside the
// distro) so the sandbox does not hold wsl.exe's pipes open.
func (s *Sandbox) start(ctx context.Context) error {
	b := s.b
	argv := s.runsc.Run(s.dir, s.cid)
	if b.cfg.Mode == ModeWSL {
		return b.run(ctx, DetachArgv(s.dir+"/runsc-run.log", argv), strings.NewReader(DetachScript), nil)
	}
	w, err := b.wrap(argv)
	if err != nil {
		return err
	}
	return b.cfg.Runner.Run(ctx, Cmd{Argv: w, Detached: true})
}

// writePrivate creates path with mode 0600 and then writes data into it.
func (s *Sandbox) writePrivate(ctx context.Context, p string, data []byte) error {
	if err := s.b.run(ctx, CreatePrivateArgv(p), nil, nil); err != nil {
		return err
	}
	return s.b.run(ctx, WriteFileArgv(p), bytes.NewReader(data), nil)
}

func (s *Sandbox) setupNet(ctx context.Context) error {
	b := s.b
	fw, err := b.detectFirewall(ctx)
	if err != nil {
		return err
	}
	var assigned map[netip.Addr]bool
	if out, err := b.out(ctx, AddrListArgv); err == nil {
		assigned = ParseAddrs(out)
	}
	slot, err := b.slots.Get(s.spec.ID, func(slot int) bool {
		n, err := NewNet(s.spec.ID, b.cfg.Pool, slot)
		return err != nil || assigned[n.HostAddr] || assigned[n.PeerAddr]
	})
	if err != nil {
		return err
	}
	n, err := NewNet(s.spec.ID, b.cfg.Pool, slot)
	if err != nil {
		b.slots.Release(s.spec.ID)
		return err
	}
	n.Firewall = fw
	s.net = &n // from here on Destroy tears the network down
	addr, err := b.proxyAddr(ctx, n)
	if err != nil {
		return err
	}
	if s.proxyURL, err = RewriteProxyURL(s.spec.Egress.ProxyURL, addr.String()); err != nil {
		return err
	}
	port, err := ProxyPort(s.proxyURL)
	if err != nil {
		return err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return fmt.Errorf("bundle: proxy port %q", port)
	}
	n.Proxy = netip.AddrPortFrom(addr, uint16(p))
	if b.cfg.AllowDNS {
		n.DNS = addr
	}
	if err := b.ensureHostRules(ctx, fw, uint16(p)); err != nil {
		return err
	}
	for _, argv := range n.SetupArgv() {
		if err := b.run(ctx, argv, nil, nil); err != nil {
			return fmt.Errorf("bundle: network setup: %w", err)
		}
	}
	return nil
}

// seed fills /workspace by copy from inside the sandbox, as the workload
// user, so a hostile tarball cannot write outside the sandbox's own files.
func (s *Sandbox) seed(ctx context.Context) error {
	if t := s.spec.Workspace.Tarball; t != "" {
		f, err := os.Open(t)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		br := bufio.NewReader(f)
		argv := []string{"tar", "-x", "--no-same-owner", "--no-same-permissions", "-C", WorkspaceDir, "-f", "-"}
		if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			argv = []string{"tar", "-x", "-z", "--no-same-owner", "--no-same-permissions", "-C", WorkspaceDir, "-f", "-"}
		}
		if err := s.guest(ctx, argv, br, nil); err != nil {
			return fmt.Errorf("bundle: seed workspace: %w", err)
		}
	}
	for _, in := range s.spec.Workspace.Inputs {
		if err := s.copyInFile(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sandbox) copyInFile(ctx context.Context, in sandbox.Input) error {
	f, err := os.Open(in.LocalPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return s.CopyIn(ctx, f, in.Path, 0o644)
}

func userString(u User) string { return fmt.Sprintf("%d:%d", u.UID, u.GID) }

// guest runs a helper command inside the sandbox in /workspace as the
// workload user. Helper binaries (tar, dd, find, stat, …) come from the
// image, so their output is untrusted data.
func (s *Sandbox) guest(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	return s.guestAs(ctx, s.user, argv, stdin, stdout)
}

func (s *Sandbox) guestAs(ctx context.Context, u User, argv []string, stdin io.Reader, stdout io.Writer) error {
	return s.b.run(ctx, s.runsc.ExecAs(s.cid, userString(u), argv...), stdin, stdout)
}

func (s *Sandbox) live() error {
	if s.destroyed.Load() {
		return fmt.Errorf("%w: sandbox %s destroyed", sandbox.ErrKilled, s.spec.ID)
	}
	if s.timedOut.Load() {
		return fmt.Errorf("%w: sandbox %s", sandbox.ErrTimeout, s.spec.ID)
	}
	return nil
}

// expire is the wall-clock timer: every process in the sandbox is killed and
// running Execs report ErrTimeout.
func (s *Sandbox) expire() {
	if s.destroyed.Load() {
		return
	}
	s.timedOut.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.b.run(ctx, s.runsc.KillAll(s.cid, "KILL"), nil, nil)
}

// ID implements sandbox.Sandbox.
func (s *Sandbox) ID() string { return s.spec.ID }

// Backend implements sandbox.Sandbox.
func (s *Sandbox) Backend() string { return s.b.cfg.Name }

// ProxyURL is the proxy URL as given to the sandbox (host rewritten).
func (s *Sandbox) ProxyURL() string { return s.proxyURL }

// CopyIn implements sandbox.Sandbox.
func (s *Sandbox) CopyIn(ctx context.Context, src io.Reader, p string, mode uint32) error {
	if err := s.live(); err != nil {
		return err
	}
	g, err := Confine(p, false)
	if err != nil {
		return err
	}
	if err := s.guest(ctx, []string{"mkdir", "-p", "--", path.Dir(g)}, nil, nil); err != nil {
		return err
	}
	if err := s.guest(ctx, []string{"dd", "of=" + g, "bs=1M", "status=none"}, src, nil); err != nil {
		return err
	}
	return s.guest(ctx, ChmodArgv(mode&0o777, g), nil, nil)
}

// CopyOut implements sandbox.Sandbox: a regular file comes back as its
// bytes, a directory as a tar stream of it (entries relative to its parent).
// The stream is produced by the image's own tar inside the sandbox and is
// untrusted: extract it without owners and without setuid/setgid bits
// (`--no-same-owner --no-same-permissions`), never as root into a shared
// location, and check entry names.
func (s *Sandbox) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := s.live(); err != nil {
		return nil, err
	}
	g, err := Confine(p, true)
	if err != nil {
		return nil, err
	}
	var kind bytes.Buffer
	if err := s.guest(ctx, []string{"stat", "-L", "-c", "%F", "--", g}, nil, &kind); err != nil {
		return nil, fmt.Errorf("bundle: copy out %s: %w", p, err)
	}
	var argv []string
	switch strings.TrimSpace(kind.String()) {
	case "directory":
		argv = []string{"tar", "-c", "-f", "-", "-C", path.Dir(g), "--", path.Base(g)}
	case "regular file", "regular empty file":
		argv = []string{"cat", "--", g}
	default:
		return nil, fmt.Errorf("bundle: copy out %s: not a file or directory", p)
	}
	w, err := s.b.wrap(s.runsc.ExecAs(s.cid, userString(s.user), argv...))
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	h, err := s.b.cfg.Runner.Start(cctx, Cmd{Argv: w, Stdout: pw})
	if err != nil {
		cancel()
		return nil, err
	}
	go func() { _ = pw.CloseWithError(h.Wait()) }()
	return &cancelReader{PipeReader: pr, cancel: cancel}, nil
}

// clearDir removes the contents of the node-local Spec.Dir.
func clearDir(dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

type cancelReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *cancelReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}

// Glob implements sandbox.Sandbox. The listing comes from the image's find
// inside the sandbox and is untrusted; names are only matched, never opened
// on the host.
func (s *Sandbox) Glob(ctx context.Context, patterns []string) ([]string, error) {
	if err := s.live(); err != nil {
		return nil, err
	}
	for _, p := range patterns {
		if err := ValidatePattern(p); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if err := s.guest(ctx, FindFilesArgv(), nil, &out); err != nil {
		return nil, err
	}
	return FilterGlob(out.Bytes(), patterns), nil
}

// Pause implements sandbox.Sandbox.
func (s *Sandbox) Pause(ctx context.Context) error {
	if err := s.live(); err != nil {
		return err
	}
	return s.b.run(ctx, s.runsc.Pause(s.cid), nil, nil)
}

// Resume implements sandbox.Sandbox.
func (s *Sandbox) Resume(ctx context.Context) error {
	if err := s.live(); err != nil {
		return err
	}
	return s.b.run(ctx, s.runsc.Resume(s.cid), nil, nil)
}

// Stats implements sandbox.Sandbox. CPUMilli is cumulative CPU time in
// milliseconds; DiskBytes is the size of the sandbox directory (overlay
// file and workspace). Egress bytes are counted by the proxy.
func (s *Sandbox) Stats(ctx context.Context) (sandbox.Stats, error) {
	if err := s.live(); err != nil {
		return sandbox.Stats{}, err
	}
	out, err := s.b.out(ctx, s.runsc.Stats(s.cid))
	if err != nil {
		return sandbox.Stats{}, err
	}
	ev, err := ParseStatsEvent(out)
	if err != nil {
		return sandbox.Stats{}, err
	}
	st := sandbox.Stats{
		CPUMilli:     int64(ev.CPU.Usage.Total / uint64(time.Millisecond)), // #nosec G115 -- ns/1e6 fits
		MemoryBytes:  int64(ev.Memory.Usage.Usage),                         // #nosec G115 -- bytes fit
		Processes:    int(ev.Pids.Current),                                 // #nosec G115 -- small
		RunningSince: s.created,
	}
	if du, err := s.b.out(ctx, DiskUsageArgv(s.dir)); err == nil {
		st.DiskBytes, _ = ParseDiskUsage(du)
	}
	return st, nil
}

// Destroy implements sandbox.Sandbox: kill every process, delete the
// container and confirm with `runsc state` that it is gone, remove the
// firewall rules, veth pair and namespace, then the sandbox directory and
// the contents of Spec.Dir. It ignores the caller's cancellation and runs
// under its own timeout. Errors that only say the object no longer exists
// are ignored; any other failure is returned and a later call retries
// everything. Once it succeeded, further calls do nothing.
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return nil
	}
	s.destroyed.Store(true)
	if s.timer != nil {
		s.timer.Stop()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), destroyTimeout)
	defer cancel()
	b := s.b
	var errs []error
	step := func(argv []string) bool {
		err := b.run(ctx, argv, nil, nil)
		if err != nil && !notExist(err) {
			errs = append(errs, err)
			return false
		}
		return true
	}
	step(s.runsc.KillAll(s.cid, "KILL"))
	step(s.runsc.Delete(s.cid))
	switch err := b.run(ctx, s.runsc.State(s.cid), nil, nil); {
	case err == nil:
		errs = append(errs, fmt.Errorf("bundle: container %s still present after delete", s.cid))
	case !notExist(err):
		errs = append(errs, err)
	}
	if s.net != nil {
		ok := true
		for _, argv := range s.net.TeardownArgv() {
			ok = step(argv) && ok
		}
		if ok {
			b.slots.Release(s.spec.ID)
		}
	}
	step(RemoveAllArgv(s.dir))
	if err := clearDir(s.spec.Dir); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("bundle: destroy %s: %w", s.spec.ID, errors.Join(errs...))
	}
	s.gone = true
	b.setLive(s.cid, false)
	return nil
}
