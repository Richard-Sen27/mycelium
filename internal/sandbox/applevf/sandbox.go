//go:build darwin

package applevf

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// limaSandbox is one Lima VM.
//
// Every guest operation runs `limactl shell --shell <mycel shell> --workdir
// … <instance> -- /usr/local/libexec/mycel/helper <op> <args>` as the
// unprivileged guest user; the shell is bash without profile or rc files, so
// nothing the sandbox writes into its home affects helper calls. String
// arguments are passed as "b"+base64, so neither the host nor the guest shell
// ever interprets user-supplied text, whatever quoting `limactl shell`
// applies. Exec requests (argv and environment, which may hold secrets) never
// appear in any argv: they go over stdin into a 0600 file in the guest.
type limaSandbox struct {
	id, inst, dir string
	home          string // LIMA_HOME, protected from removal
	r             runner
	log           *slog.Logger
	env           map[string]string // Spec.Env (the proxy variables live in the guest's /etc/environment)
	wall          time.Duration     // wall clock, measured from the end of Create
	created       time.Time

	mu        sync.Mutex
	procs     map[*process]struct{}
	destroyed bool
}

var _ sandbox.Sandbox = (*limaSandbox)(nil)

func (s *limaSandbox) ID() string      { return s.id }
func (s *limaSandbox) Backend() string { return sandbox.BackendAppleVF }

// helperArgs is the limactl argv that runs the guest helper.
func (s *limaSandbox) helperArgs(op string, args ...string) []string {
	return append([]string{"shell", "--tty=false", "--shell=" + guestShell, "--workdir=" + guestWorkspace, s.inst, "--", guestHelper, op}, args...)
}

// enc encodes one string argument for the guest helper.
func enc(v string) string { return "b" + base64.StdEncoding.EncodeToString([]byte(v)) }

func (s *limaSandbox) alive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return errDestroyed
	}
	return nil
}

// run runs one short helper call with a timeout.
func (s *limaSandbox) run(ctx context.Context, stdin io.Reader, op string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, shortCallTimeout)
	defer cancel()
	return s.runCtx(ctx, stdin, op, args...)
}

// runCtx runs one helper call bounded only by ctx (transfers whose duration
// depends on their size: CopyIn, untar, Glob).
func (s *limaSandbox) runCtx(ctx context.Context, stdin io.Reader, op string, args ...string) ([]byte, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	out, stderr, err := s.r.Run(ctx, stdin, s.helperArgs(op, args...)...)
	if err != nil {
		return out, cmdError(op, err, stderr)
	}
	return out, nil
}

// workspaceRel confines p to /workspace and returns it relative to it
// ("." for /workspace itself). Absolute paths are accepted only inside
// /workspace; ".." may not leave it.
func workspaceRel(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("applevf: invalid path %q", p)
	}
	if path.IsAbs(p) {
		c := path.Clean(p)
		switch {
		case c == guestWorkspace:
			return ".", nil
		case strings.HasPrefix(c, guestWorkspace+"/"):
			p = strings.TrimPrefix(c, guestWorkspace+"/")
		default:
			return "", fmt.Errorf("applevf: path %q is outside /workspace", p)
		}
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("applevf: path %q escapes /workspace", p)
	}
	return c, nil
}

// globPattern validates a glob pattern: relative, no "..", no NUL.
func globPattern(p string) error {
	if p == "" || strings.ContainsRune(p, 0) || path.IsAbs(p) {
		return fmt.Errorf("applevf: invalid glob pattern %q", p)
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("applevf: glob pattern %q escapes /workspace", p)
	}
	return nil
}

// newPidID returns the random name of a process's request and pid files in
// the guest.
func newPidID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// execPayload is the NUL-separated exec request: dir, env count, NAME=value
// pairs (sorted), argv.
func execPayload(dir string, env map[string]string, argv []string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	fields := []string{dir, strconv.Itoa(len(keys))}
	for _, k := range keys {
		fields = append(fields, k+"="+env[k])
	}
	fields = append(fields, argv...)
	return strings.Join(fields, "\x00") + "\x00"
}

// Exec implements sandbox.Sandbox. The request (dir, environment, argv) is
// first stored in the guest at /tmp/.mycel-exec/<id>.req (0600, over stdin);
// the second call runs it: the helper reads and deletes the request and runs
// the command as the guest user in its own session (setsid), so its process
// group id is its pid, written to /tmp/.mycel-exec/<id>.pid for Signal and
// removed after exit. Cancelling ctx or reaching the sandbox wall clock kills
// the group. ExecOpts.TTY is ignored: commands never get a terminal.
//
// Stdout and Stderr must be drained concurrently; output beyond 8 MiB per
// stream that is not read yet spills to a file in the sandbox directory.
func (s *limaSandbox) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.Process, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	var deadline time.Time
	if s.wall > 0 {
		deadline = s.created.Add(s.wall)
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("applevf: exec: %w", sandbox.ErrTimeout)
		}
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("applevf: empty argv")
	}
	for _, a := range argv {
		if strings.ContainsRune(a, 0) {
			return nil, errors.New("applevf: argv contains a NUL byte")
		}
	}
	dir := "."
	if opts.Dir != "" {
		d, err := workspaceRel(opts.Dir)
		if err != nil {
			return nil, err
		}
		dir = d
	}
	env, err := mergeEnv(s.env, opts.Env)
	if err != nil {
		return nil, err
	}
	pidID := newPidID()
	if _, err := s.run(ctx, strings.NewReader(execPayload(dir, env, argv)), "putreq", pidID); err != nil {
		return nil, fmt.Errorf("applevf: exec: %w", err)
	}
	rc, err := s.r.Start(opts.Stdin, s.helperArgs("exec", pidID)...)
	if err != nil {
		return nil, fmt.Errorf("applevf: exec: %w", err)
	}
	p := &process{s: s, rc: rc, pidID: pidID, done: make(chan struct{})}
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		rc.Kill()
		_, _ = rc.Wait()
		return nil, errDestroyed
	}
	s.procs[p] = struct{}{}
	s.mu.Unlock()

	go func() {
		p.code, p.err = rc.Wait()
		close(p.done)
		s.mu.Lock()
		delete(s.procs, p)
		s.mu.Unlock()
	}()
	go p.watch(ctx, deadline)
	return p, nil
}

// process is one Exec.
type process struct {
	s     *limaSandbox
	rc    runningCmd
	pidID string
	done  chan struct{}
	code  int
	err   error

	mu     sync.Mutex
	reason error // why we killed it, if we did
}

func (p *process) Stdout() io.Reader { return p.rc.Stdout() }
func (p *process) Stderr() io.Reader { return p.rc.Stderr() }

func (p *process) watch(ctx context.Context, deadline time.Time) {
	var expire <-chan time.Time
	if !deadline.IsZero() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		expire = t.C
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			p.kill(sandbox.ErrTimeout)
		} else {
			p.kill(sandbox.ErrKilled)
		}
	case <-expire:
		p.kill(sandbox.ErrTimeout)
	}
}

// kill kills the process group in the guest, then the local limactl.
func (p *process) kill(reason error) {
	p.mu.Lock()
	if p.reason == nil {
		p.reason = reason
	}
	p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
	}
	if p.s.alive() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := p.s.run(ctx, nil, "signal", p.pidID, "KILL"); err != nil {
			p.s.log.Debug("applevf: kill in guest", "err", err)
		}
		cancel()
	}
	p.rc.Kill()
}

// Wait implements sandbox.Process.
func (p *process) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	reason := p.reason
	p.mu.Unlock()
	if reason != nil {
		return p.code, fmt.Errorf("applevf: process: %w", reason)
	}
	if p.err != nil {
		return p.code, fmt.Errorf("applevf: process: %w", p.err)
	}
	return p.code, nil
}

// Signal implements sandbox.Process: the signal goes to the whole process
// group in the guest. "KILL" marks the process as killed for Wait and, if the
// guest cannot be reached, kills the local limactl.
func (p *process) Signal(sig string) error {
	switch sig {
	case "TERM", "KILL", "INT", "HUP", "QUIT", "USR1", "USR2":
	default:
		return fmt.Errorf("applevf: unsupported signal %q", sig)
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	if sig == "KILL" {
		p.mu.Lock()
		if p.reason == nil {
			p.reason = sandbox.ErrKilled
		}
		p.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := p.s.run(ctx, nil, "signal", p.pidID, sig)
	if err != nil && sig == "KILL" {
		p.rc.Kill()
	}
	return err
}

// CopyIn implements sandbox.Sandbox: the bytes stream over limactl's stdin
// into `cat` in the guest, followed by chmod; parent directories are created.
func (s *limaSandbox) CopyIn(ctx context.Context, src io.Reader, p string, mode uint32) error {
	rel, err := workspaceRel(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return errors.New("applevf: copy-in target is /workspace itself")
	}
	if mode == 0 {
		mode = 0o644
	}
	if src == nil {
		src = bytes.NewReader(nil)
	}
	_, err = s.runCtx(ctx, src, "copyin", enc(rel), strconv.FormatUint(uint64(mode&0o7777), 8))
	return err
}

// CopyOut implements sandbox.Sandbox: a tar stream of the file or directory
// (paths in the archive are relative to /workspace). The stream is produced
// by the guest and is untrusted: callers must treat member names, link
// targets, modes and sizes as hostile (no extraction without their own path
// confinement and limits).
func (s *limaSandbox) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	rel, err := workspaceRel(p)
	if err != nil {
		return nil, err
	}
	rc, err := s.r.Start(nil, s.helperArgs("copyout", enc(rel))...)
	if err != nil {
		return nil, fmt.Errorf("applevf: copy-out: %w", err)
	}
	t := &tarStream{rc: rc, done: make(chan struct{})}
	go func() {
		// Wait first: stderr is buffered and only reaches EOF after Wait.
		t.code, t.err = rc.Wait()
		var buf bytes.Buffer
		_, _ = io.Copy(&limitedWriter{w: &buf, n: 64 << 10}, rc.Stderr())
		t.stderr = buf.Bytes()
		close(t.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			rc.Kill()
		case <-t.done:
		}
	}()
	return t, nil
}

// tarStream reports a failed tar at EOF instead of a silently short stream.
type tarStream struct {
	rc     runningCmd
	done   chan struct{}
	code   int
	err    error
	stderr []byte
	once   sync.Once
}

func (t *tarStream) Read(b []byte) (int, error) {
	n, err := t.rc.Stdout().Read(b)
	if err == io.EOF {
		<-t.done
		if t.err != nil {
			return n, fmt.Errorf("applevf: copy-out: %w", t.err)
		}
		if t.code != 0 {
			return n, cmdError("copy-out", fmt.Errorf("exit status %d", t.code), t.stderr)
		}
	}
	return n, err
}

func (t *tarStream) Close() error {
	t.once.Do(func() {
		select {
		case <-t.done:
		default:
			t.rc.Kill()
		}
		if bp, ok := t.rc.Stdout().(interface{ CloseRead() }); ok {
			bp.CloseRead()
		}
		<-t.done
	})
	return nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if l.n > 0 {
		k := min(l.n, len(b))
		_, _ = l.w.Write(b[:k])
		l.n -= k
	}
	return len(b), nil
}

// Glob implements sandbox.Sandbox: patterns are expanded by bash in the guest
// (globstar on, so "**" matches across directories), relative to /workspace.
func (s *limaSandbox) Glob(ctx context.Context, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	args := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if err := globPattern(p); err != nil {
			return nil, err
		}
		args = append(args, enc(p))
	}
	out, err := s.runCtx(ctx, nil, "glob", args...)
	if err != nil {
		return nil, err
	}
	// The guest's answer is untrusted: keep only confined relative paths.
	var res []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || path.IsAbs(f) {
			continue
		}
		if rel, err := workspaceRel(f); err == nil && rel != "." && rel == f {
			res = append(res, rel)
		}
	}
	slices.Sort(res)
	return slices.Compact(res), nil
}

// Pause implements sandbox.Sandbox. v1 pauses at the process level inside the
// VM: `docker pause` for nested containers, then SIGSTOP by pid to every
// process of the guest user except the helper's own chain (its ssh session)
// and the systemd user manager, repeated until a pass finds no new process.
// The VM itself keeps running (limactl has no suspend).
func (s *limaSandbox) Pause(ctx context.Context) error {
	_, err := s.run(ctx, nil, "pause")
	return err
}

// Resume implements sandbox.Sandbox (SIGCONT, `docker unpause`).
func (s *limaSandbox) Resume(ctx context.Context) error {
	_, err := s.run(ctx, nil, "resume")
	return err
}

// Stats implements sandbox.Sandbox from /proc/loadavg, /proc/meminfo and df
// in the guest. CPUMilli is the 1-minute load average × 1000. Egress bytes
// are accounted by the egress proxy, not here.
func (s *limaSandbox) Stats(ctx context.Context) (sandbox.Stats, error) {
	out, err := s.run(ctx, nil, "stats")
	if err != nil {
		return sandbox.Stats{}, err
	}
	st := parseStats(out)
	st.RunningSince = s.created
	return st, nil
}

func parseStats(out []byte) sandbox.Stats {
	var st sandbox.Stats
	var memTotal, memAvail int64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		switch k {
		case "load":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				st.CPUMilli = int64(f * 1000)
			}
		case "memtotal":
			memTotal, _ = strconv.ParseInt(v, 10, 64)
		case "memavail":
			memAvail, _ = strconv.ParseInt(v, 10, 64)
		case "disk":
			st.DiskBytes, _ = strconv.ParseInt(v, 10, 64)
		case "procs":
			n, _ := strconv.Atoi(v)
			st.Processes = n
		}
	}
	if memTotal > memAvail {
		st.MemoryBytes = (memTotal - memAvail) * 1024
	}
	return st
}

// seed fills /workspace from the tarball and the input files.
func (s *limaSandbox) seed(ctx context.Context, w sandbox.WorkspaceSeed) error {
	if w.Tarball != "" {
		if err := s.untar(ctx, w.Tarball); err != nil {
			return err
		}
	}
	for _, in := range w.Inputs {
		if err := s.copyInFile(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

func (s *limaSandbox) untar(ctx context.Context, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("applevf: workspace tarball: %w", err)
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	format := "plain"
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		format = "gz"
	}
	if _, err := s.runCtx(ctx, br, "untar", format); err != nil {
		return fmt.Errorf("applevf: seed workspace: %w", err)
	}
	return nil
}

func (s *limaSandbox) copyInFile(ctx context.Context, in sandbox.Input) error {
	f, err := os.Open(in.LocalPath)
	if err != nil {
		return fmt.Errorf("applevf: input %s: %w", in.Path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("applevf: input %s: %w", in.Path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("applevf: input %s is not a regular file", in.Path)
	}
	return s.CopyIn(ctx, f, in.Path, uint32(fi.Mode().Perm()))
}

// Destroy implements sandbox.Sandbox: kill running commands, `limactl stop
// --force`, `limactl delete --force`, remove the sandbox dir. Idempotent; a
// missing instance counts as deleted.
func (s *limaSandbox) Destroy(ctx context.Context) error {
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return nil
	}
	procs := make([]*process, 0, len(s.procs))
	for p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	for _, p := range procs {
		p.mu.Lock()
		if p.reason == nil {
			p.reason = sandbox.ErrKilled
		}
		p.mu.Unlock()
		p.rc.Kill()
	}

	sctx, cancel := context.WithTimeout(ctx, stopTimeout)
	if _, stderr, err := s.r.Run(sctx, nil, "stop", "--tty=false", "--force", s.inst); err != nil {
		s.log.Debug("applevf: limactl stop", "err", cmdError("stop", err, stderr))
	}
	cancel()
	dctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	_, stderr, err := s.r.Run(dctx, nil, "delete", "--tty=false", "--force", s.inst)
	cancel()
	if err != nil {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		exists, lerr := s.instanceExists(lctx)
		cancel()
		if lerr != nil || exists {
			return cmdError("limactl delete", err, stderr)
		}
	}
	if err := checkRemovable(s.dir, s.home); err != nil {
		return err
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("applevf: remove sandbox dir: %w", err)
	}
	s.mu.Lock()
	s.destroyed = true
	s.mu.Unlock()
	s.log.Info("applevf: sandbox destroyed")
	return nil
}

func (s *limaSandbox) instanceExists(ctx context.Context) (bool, error) {
	out, stderr, err := s.r.Run(ctx, nil, "list", "--tty=false", "--quiet")
	if err != nil {
		return false, cmdError("limactl list", err, stderr)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == s.inst {
			return true, nil
		}
	}
	return false, nil
}
