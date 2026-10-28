//go:build fakesandbox

package fake

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// EnvOK is the variable that must be "1" for the backend to run.
const EnvOK = "MYCEL_FAKE_SANDBOX_OK"

// MaxTarEntries caps the number of entries in a seed tarball.
const MaxTarEntries = 100_000

// tarEntryCost is what every tar entry counts toward the disk limit on top
// of its size, so directories, links and empty files are not free.
const tarEntryCost = 512

// ErrRefused is returned by Prepare and Create when EnvOK is not set.
var ErrRefused = errors.New("fake sandbox: refused: " + EnvOK + " is not 1")

// ErrEscape is wrapped by every path that would leave the workspace.
var ErrEscape = errors.New("fake sandbox: path escapes the workspace")

func init() { sandbox.Register(Backend{}) }

// Backend is the fake backend. The zero value is ready to use.
type Backend struct{}

// Name returns "fake".
func (Backend) Name() string { return sandbox.BackendFake }

// Available is true only when MYCEL_FAKE_SANDBOX_OK=1.
func (Backend) Available(context.Context) (bool, string) {
	if os.Getenv(EnvOK) != "1" {
		return false, "fake backend refuses to run unless " + EnvOK + "=1 (CI only)"
	}
	return true, ""
}

// Capabilities reports Egress true so profiles with an egress allowlist
// (the fake profile) are accepted: the proxy variables are set in every
// command's environment, but nothing stops a process from connecting
// directly. Isolation "none" is the marker that this backend is unsafe.
func (Backend) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Egress: true, Isolation: "none"}
}

// Prepare has no image to fetch; it only checks that the backend may run.
func (b Backend) Prepare(ctx context.Context, _ string, _ func(string)) error {
	if ok, _ := b.Available(ctx); !ok {
		return ErrRefused
	}
	return nil
}

// Create requires spec.Dir to be new or empty, makes <Dir>/workspace,
// <Dir>/home and <Dir>/tmp and seeds the workspace from the tarball and
// inputs, within Limits.DiskBytes (default sandbox.DefaultDiskBytes).
func (b Backend) Create(ctx context.Context, spec sandbox.Spec) (_ sandbox.Sandbox, err error) {
	if ok, _ := b.Available(ctx); !ok {
		return nil, ErrRefused
	}
	if spec.Dir == "" || !filepath.IsAbs(spec.Dir) {
		return nil, fmt.Errorf("fake sandbox: Dir %q must be an absolute path", spec.Dir)
	}
	if err := sandbox.ValidateEnv(spec.Env); err != nil {
		return nil, err
	}
	s := &Sandbox{
		id:      spec.ID,
		dir:     spec.Dir,
		ws:      filepath.Join(spec.Dir, "workspace"),
		home:    filepath.Join(spec.Dir, "home"),
		tmp:     filepath.Join(spec.Dir, "tmp"),
		env:     spec.Env,
		proxy:   spec.Egress.ProxyURL,
		disk:    spec.Limits.DiskBytes,
		created: time.Now(),
	}
	if s.disk <= 0 {
		s.disk = sandbox.DefaultDiskBytes
	}
	ents, err := os.ReadDir(spec.Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
			return nil, err
		}
		s.ownsDir = true
	case err != nil:
		return nil, err
	case len(ents) > 0:
		return nil, fmt.Errorf("fake sandbox: Dir %s is not empty", spec.Dir)
	}
	defer func() {
		if err != nil {
			_ = s.Destroy(context.Background())
		}
	}()
	for _, d := range []string{s.ws, s.home, s.tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	if s.root, err = os.OpenRoot(s.ws); err != nil {
		return nil, err
	}
	if spec.Workspace.Tarball != "" {
		if err := extractTarball(ctx, s.root, spec.Workspace.Tarball, s.disk); err != nil {
			return nil, fmt.Errorf("fake sandbox: seed: %w", err)
		}
	}
	for _, in := range spec.Workspace.Inputs {
		if err := s.copyInput(ctx, in); err != nil {
			return nil, fmt.Errorf("fake sandbox: input %s: %w", in.Path, err)
		}
	}
	return s, nil
}

// Sandbox is one fake sandbox: a directory tree on the host.
type Sandbox struct {
	id, dir, ws, home, tmp string
	ownsDir                bool
	env                    map[string]string
	proxy                  string
	disk                   int64
	created                time.Time

	mu        sync.Mutex
	root      *os.Root
	procs     []*Process
	destroyed bool
}

// ID returns the sandbox id from the spec.
func (s *Sandbox) ID() string { return s.id }

// Backend returns "fake".
func (s *Sandbox) Backend() string { return sandbox.BackendFake }

// WorkspaceDir is the host directory standing in for /workspace (tests).
func (s *Sandbox) WorkspaceDir() string { return s.ws }

// Exec starts argv with a scrubbed environment in the workspace (or
// opts.Dir inside it) as the leader of a new process group. argv[0] is
// resolved with the job's PATH.
func (s *Sandbox) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("fake sandbox: empty command")
	}
	if err := sandbox.ValidateEnv(opts.Env); err != nil {
		return nil, err
	}
	root, err := s.liveRoot()
	if err != nil {
		return nil, err
	}
	dir, err := s.execDir(root, opts.Dir)
	if err != nil {
		return nil, err
	}
	env := s.environ(opts.Env)
	bin, err := lookPath(argv[0], env["PATH"], dir)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return nil, errors.New("fake sandbox: destroyed")
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	p := &Process{stdout: outR, stderr: errR, done: make(chan struct{})}
	// Running the job's argv is the purpose of this backend.
	cmd := exec.CommandContext(ctx, bin, argv[1:]...) //nolint:gosec // G204: the job's own command
	cmd.Args[0] = argv[0]
	cmd.Dir = dir
	cmd.Env = flatten(env)
	cmd.Stdin = opts.Stdin
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.WaitDelay = time.Second
	setProcAttr(cmd)
	cmd.Cancel = func() error { return p.kill() }
	p.cmd = cmd
	err = cmd.Start()
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		_ = outR.Close()
		_ = errR.Close()
		return nil, err
	}
	p.proc = cmd.Process
	p.pgid = cmd.Process.Pid // Setpgid: the leader's pid is the group id
	p.started = time.Now()
	s.procs = append(s.procs, p)
	return p, nil
}

// execDir maps a guest working directory to the host, resolving it through
// the workspace root so a symlink cannot lead out.
func (s *Sandbox) execDir(root *os.Root, d string) (string, error) {
	rel := ""
	switch {
	case d == "" || d == sandbox.WorkspaceGuestPath:
	case strings.HasPrefix(d, sandbox.WorkspaceGuestPath+"/"):
		rel = strings.TrimPrefix(d, sandbox.WorkspaceGuestPath+"/")
	case !strings.HasPrefix(d, "/"):
		rel = d
	default:
		return "", fmt.Errorf("%w: working directory %q", ErrEscape, d)
	}
	if rel == "" {
		return s.ws, nil
	}
	r, err := relPath(rel)
	if err != nil {
		return "", err
	}
	st, err := root.Stat(r)
	if err != nil {
		return "", confineErr(err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("fake sandbox: working directory %q is not a directory", d)
	}
	return filepath.Join(s.ws, r), nil
}

// environ is the whole environment of a command: nothing from the host but
// PATH (and SystemRoot/ComSpec on windows), then the job's variables, then
// the proxy settings, which nothing overrides.
func (s *Sandbox) environ(extra map[string]string) map[string]string {
	env := map[string]string{
		"PATH":   os.Getenv("PATH"),
		"HOME":   s.home,
		"TMPDIR": s.tmp,
		"TMP":    s.tmp,
		"TEMP":   s.tmp,
	}
	if runtime.GOOS == "windows" {
		env["SystemRoot"] = os.Getenv("SystemRoot")
		env["ComSpec"] = os.Getenv("ComSpec")
		env["USERPROFILE"] = s.home
	}
	for k, v := range s.env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	for k := range env {
		if sandbox.IsProxyEnvKey(k) {
			delete(env, k)
		}
	}
	if s.proxy != "" {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env[k] = s.proxy
		}
	}
	env["NO_PROXY"], env["no_proxy"] = "", ""
	return env
}

func flatten(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// lookPath resolves name like a shell would with the job's PATH: names with
// a separator are taken relative to dir; otherwise only absolute PATH
// entries are searched.
func lookPath(name, pathEnv, dir string) (string, error) {
	if strings.ContainsAny(name, `/\`) || filepath.IsAbs(name) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if f, ok := executable(p); ok {
			return f, nil
		}
		return "", fmt.Errorf("fake sandbox: %q is not an executable file", name)
	}
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		if f, ok := executable(filepath.Join(d, name)); ok {
			return f, nil
		}
	}
	return "", fmt.Errorf("fake sandbox: %q not found in the job's PATH", name)
}

// CopyIn writes src to path inside the workspace, within the disk limit.
func (s *Sandbox) CopyIn(ctx context.Context, src io.Reader, p string, mode uint32) error {
	rel, err := relPath(p)
	if err != nil {
		return err
	}
	root, err := s.liveRoot()
	if err != nil {
		return err
	}
	remaining := s.disk - diskUsage(s.dir)
	if remaining < 0 {
		remaining = 0
	}
	return writeFile(root, rel, &ctxReader{ctx: ctx, r: src}, fs.FileMode(mode)&0o777, remaining)
}

// CopyOut returns a regular file's bytes, or a tar stream for a directory.
// Paths that leave the workspace, symlinks and special files are refused.
func (s *Sandbox) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	rel, err := relPath(p)
	if err != nil {
		return nil, err
	}
	root, err := s.liveRoot()
	if err != nil {
		return nil, err
	}
	lst, err := root.Lstat(rel)
	if err != nil {
		return nil, confineErr(err)
	}
	if lst.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", ErrEscape, p)
	}
	if lst.IsDir() {
		sub, err := root.OpenRoot(rel)
		if err != nil {
			return nil, confineErr(err)
		}
		pr, pw := io.Pipe()
		go func() {
			defer func() { _ = sub.Close() }()
			pw.CloseWithError(writeTar(ctx, sub, pw))
		}()
		return pr, nil
	}
	f, err := openRegular(root, rel)
	if err != nil {
		return nil, err
	}
	return &ctxReadCloser{ctxReader: ctxReader{ctx: ctx, r: f}, c: f}, nil
}

// Glob returns workspace-relative slash paths of regular files matching any
// pattern. "**" matches any number of directories; a pattern naming a
// directory selects every file below it. Symlinks are never followed or
// returned.
func (s *Sandbox) Glob(ctx context.Context, patterns []string) ([]string, error) {
	for _, p := range patterns {
		if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			return nil, fmt.Errorf("%w: pattern %q", ErrEscape, p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return nil, fmt.Errorf("%w: pattern %q", ErrEscape, p)
			}
		}
		if _, err := path.Match(strings.ReplaceAll(p, "**", "*"), ""); err != nil {
			return nil, fmt.Errorf("fake sandbox: pattern %q: %w", p, err)
		}
	}
	root, err := s.liveRoot()
	if err != nil {
		return nil, err
	}
	var out []string
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		for _, p := range patterns {
			if matchPath(p, name) {
				out = append(out, name)
				break
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Pause stops every process group with SIGSTOP (no-op on windows).
func (s *Sandbox) Pause(context.Context) error { return s.signalAll("STOP") }

// Resume continues every process group with SIGCONT (no-op on windows).
func (s *Sandbox) Resume(context.Context) error { return s.signalAll("CONT") }

func (s *Sandbox) signalAll(sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, p := range s.procs {
		errs = append(errs, signalGroup(p.proc, p.pgid, sig))
	}
	return errors.Join(errs...)
}

// Stats reports running leaders, disk use of the sandbox directory, and CPU
// time and peak memory of processes that have exited (from rusage; zero
// where the platform has none).
func (s *Sandbox) Stats(context.Context) (sandbox.Stats, error) {
	s.mu.Lock()
	procs := append([]*Process(nil), s.procs...)
	s.mu.Unlock()
	st := sandbox.Stats{RunningSince: s.created}
	for _, p := range procs {
		if !p.exited() {
			st.Processes++
			continue
		}
		cpu, rss := usage(p.cmd.ProcessState)
		st.CPUMilli += cpu
		if rss > st.MemoryBytes {
			st.MemoryBytes = rss
		}
	}
	if len(procs) > 0 {
		st.RunningSince = procs[0].started
	}
	st.DiskBytes = diskUsage(s.dir)
	return st, nil
}

// Destroy kills every process group (whether or not its leader exited),
// closes the pipes and removes what Create made: the workspace, home and tmp
// directories, and Dir itself when Create created it. Idempotent.
func (s *Sandbox) Destroy(context.Context) error {
	s.mu.Lock()
	s.destroyed = true
	procs := s.procs
	s.procs = nil
	root := s.root
	s.root = nil
	s.mu.Unlock()
	for _, p := range procs {
		_ = p.kill()
		select {
		case <-p.waitAsync():
		case <-time.After(5 * time.Second):
		}
		_ = p.stdout.Close()
		_ = p.stderr.Close()
	}
	if root != nil {
		_ = root.Close()
	}
	var errs []error
	for _, d := range []string{s.ws, s.home, s.tmp} {
		errs = append(errs, os.RemoveAll(d))
	}
	if s.ownsDir {
		if err := os.Remove(s.dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Sandbox) liveRoot() (*os.Root, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil, errors.New("fake sandbox: destroyed")
	}
	return s.root, nil
}

func (s *Sandbox) copyInput(ctx context.Context, in sandbox.Input) error {
	f, err := os.Open(in.LocalPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", in.LocalPath)
	}
	return s.CopyIn(ctx, f, in.Path, uint32(st.Mode().Perm()))
}

// Process is a command started by Exec, leader of its own process group.
type Process struct {
	cmd            *exec.Cmd
	proc           *os.Process
	pgid           int
	stdout, stderr *os.File
	started        time.Time
	killed         atomic.Bool

	once sync.Once
	done chan struct{}
	code int
	err  error
}

// Stdout is the command's standard output.
func (p *Process) Stdout() io.Reader { return p.stdout }

// Stderr is the command's standard error.
func (p *Process) Stderr() io.Reader { return p.stderr }

// Wait returns the exit code; a death by signal (or a kill on windows)
// returns -1 and an error wrapping sandbox.ErrKilled.
func (p *Process) Wait() (int, error) {
	<-p.waitAsync()
	return p.code, p.err
}

func (p *Process) waitAsync() <-chan struct{} {
	p.once.Do(func() {
		go func() {
			defer close(p.done)
			err := p.cmd.Wait()
			ps := p.cmd.ProcessState
			switch {
			case ps == nil:
				p.code, p.err = -1, err
			case ps.ExitCode() == -1 || p.killed.Load():
				p.code, p.err = -1, fmt.Errorf("%w: %s", sandbox.ErrKilled, ps.String())
			default:
				p.code = ps.ExitCode()
			}
		}()
	})
	return p.done
}

func (p *Process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Signal sends "TERM" or "KILL" to the whole process group, also after the
// leader exited (its children may still run).
func (p *Process) Signal(sig string) error {
	switch sig {
	case "KILL":
		return p.kill()
	case "TERM":
		if termIsKill && !p.exited() {
			p.killed.Store(true)
		}
		return signalGroup(p.proc, p.pgid, "TERM")
	default:
		return fmt.Errorf("fake sandbox: unsupported signal %q", sig)
	}
}

// kill continues and kills the whole group; a group that is already gone
// counts as success.
func (p *Process) kill() error {
	if !p.exited() {
		p.killed.Store(true)
	}
	_ = signalGroup(p.proc, p.pgid, "CONT")
	return signalGroup(p.proc, p.pgid, "KILL")
}

// relPath turns a /workspace-relative (or /workspace/…) slash path into a
// local OS path, refusing anything that leaves the workspace.
func relPath(p string) (string, error) {
	p = strings.TrimPrefix(p, sandbox.WorkspaceGuestPath+"/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("%w: %q", ErrEscape, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q", ErrEscape, p)
		}
	}
	l := filepath.FromSlash(path.Clean(p))
	if !filepath.IsLocal(l) {
		return "", fmt.Errorf("%w: %q", ErrEscape, p)
	}
	return l, nil
}

func confineErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "escapes from parent") {
		return fmt.Errorf("%w: %v", ErrEscape, err)
	}
	return err
}

// openRegular opens rel for reading without following a final symlink or
// blocking on a FIFO, and refuses anything but a regular file.
func openRegular(root *os.Root, rel string) (*os.File, error) {
	f, err := root.OpenFile(rel, openReadFlags, 0)
	if err != nil {
		return nil, confineErr(err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("fake sandbox: %s is not a regular file", rel)
	}
	return f, nil
}

// ctxReader fails reads once its context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type ctxReadCloser struct {
	ctxReader
	c io.Closer
}

func (c *ctxReadCloser) Close() error { return c.c.Close() }

// diskUsage sums the sizes of regular files below dir.
func diskUsage(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// checkParents refuses rel when any existing prefix of it (including rel
// itself) is a symlink.
func checkParents(root *os.Root, rel string) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i <= len(parts); i++ {
		prefix := filepath.FromSlash(strings.Join(parts[:i], "/"))
		st, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return confineErr(err)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s passes through the symlink %s", ErrEscape, rel, prefix)
		}
	}
	return nil
}

// writeFile creates rel inside root from src, refusing more than max bytes.
func writeFile(root *os.Root, rel string, src io.Reader, mode fs.FileMode, max int64) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return confineErr(err)
		}
	}
	if mode == 0 {
		mode = 0o644
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return confineErr(err)
	}
	n, err := io.CopyN(f, src, max+1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fmt.Errorf("%w: disk limit reached writing %s", sandbox.ErrLimit, rel)
	}
	if err != nil {
		_ = root.Remove(rel)
	}
	return err
}

// extractTarball unpacks a tar or tar.gz into root. Absolute names, ".."
// anywhere in names or link targets, and entries that pass through an
// existing symlink are refused; devices and FIFOs are skipped. The total
// (sizes plus tarEntryCost per entry) is capped by maxBytes and the entry
// count by MaxTarEntries.
func extractTarball(ctx context.Context, root *os.Root, file string, maxBytes int64) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	tr := tar.NewReader(&ctxReader{ctx: ctx, r: r})
	var total int64
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if entries >= MaxTarEntries {
			return fmt.Errorf("%w: tarball has more than %d entries", sandbox.ErrLimit, MaxTarEntries)
		}
		name, err := tarName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		size := hdr.Size
		if hdr.Typeflag != tar.TypeReg || size < 0 {
			size = 0
		}
		total += tarEntryCost + size
		if total > maxBytes {
			return fmt.Errorf("%w: tarball exceeds the disk limit of %d bytes", sandbox.ErrLimit, maxBytes)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
		default:
			continue // devices, FIFOs and other specials are not seeded
		}
		if err := checkParents(root, name); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return confineErr(err)
			}
		case tar.TypeReg:
			if err := writeFile(root, name, tr, fs.FileMode(hdr.Mode)&0o777, hdr.Size); err != nil { //nolint:gosec // G115: masked to permission bits
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			target, err := linkTarget(hdr.Linkname)
			if err != nil {
				return fmt.Errorf("%w (entry %s)", err, hdr.Name)
			}
			if dir := filepath.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return confineErr(err)
				}
			}
			if hdr.Typeflag == tar.TypeSymlink {
				err = root.Symlink(filepath.FromSlash(target), name)
			} else {
				if err := checkParents(root, filepath.FromSlash(target)); err != nil {
					return err
				}
				err = root.Link(filepath.FromSlash(target), name)
			}
			if err != nil {
				return confineErr(err)
			}
		}
	}
}

// linkTarget accepts only relative targets without any ".." component, so
// a link can only point down from its own directory.
func linkTarget(t string) (string, error) {
	if t == "" || strings.HasPrefix(t, "/") || strings.Contains(t, "\\") || filepath.VolumeName(t) != "" {
		return "", fmt.Errorf("%w: link target %q", ErrEscape, t)
	}
	for _, seg := range strings.Split(t, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: link target %q", ErrEscape, t)
		}
	}
	return path.Clean(t), nil
}

// tarName validates an entry name and returns it as a local path ("" for
// the archive root).
func tarName(n string) (string, error) {
	if strings.HasPrefix(n, "/") || strings.Contains(n, "\\") || filepath.IsAbs(n) || filepath.VolumeName(n) != "" {
		return "", fmt.Errorf("%w: absolute entry %q", ErrEscape, n)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: entry %q", ErrEscape, n)
		}
	}
	c := path.Clean(n)
	if c == "." {
		return "", nil
	}
	return filepath.FromSlash(c), nil
}

// writeTar streams the tree under root as a tar archive of regular files
// and directories; symlinks and special files are skipped.
func writeTar(ctx context.Context, root *os.Root, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." || (!d.IsDir() && !d.Type().IsRegular()) {
			return nil
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name + "/"
			return tw.WriteHeader(hdr)
		}
		f, err := openRegular(root, filepath.FromSlash(name))
		if err != nil {
			return nil // swapped for something else meanwhile: skip
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.CopyN(tw, &ctxReader{ctx: ctx, r: f}, info.Size())
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// matchPath matches a slash path against a pattern where "**" spans any
// number of segments and a match on a directory prefix selects the files
// below it.
func matchPath(pattern, name string) bool {
	ps := strings.Split(pattern, "/")
	ns := strings.Split(name, "/")
	for i := len(ns); i >= 1; i-- {
		if matchSegs(ps, ns[:i]) {
			return true
		}
	}
	return false
}

func matchSegs(ps, ns []string) bool {
	if len(ps) == 0 {
		return len(ns) == 0
	}
	if ps[0] == "**" {
		for i := 0; i <= len(ns); i++ {
			if matchSegs(ps[1:], ns[i:]) {
				return true
			}
		}
		return false
	}
	if len(ns) == 0 {
		return false
	}
	ok, err := path.Match(ps[0], ns[0])
	return err == nil && ok && matchSegs(ps[1:], ns[1:])
}
