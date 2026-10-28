package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// streamBuffer bounds buffered stdout/stderr per stream.
const streamBuffer = 4 << 20

// killGrace is how long Exec waits after asking runsc to kill a process
// tree before it also kills the local runsc exec client.
var killGrace = 5 * time.Second

// ErrTTY is returned for ExecOpts.TTY, which v1 does not support.
var ErrTTY = errors.New("bundle: TTY exec is not supported")

// signals that Signal accepts.
var signals = map[string]string{
	"TERM": "SIGTERM", "KILL": "SIGKILL", "INT": "SIGINT", "HUP": "SIGHUP",
	"QUIT": "SIGQUIT", "USR1": "SIGUSR1", "USR2": "SIGUSR2",
	"STOP": "SIGSTOP", "CONT": "SIGCONT",
}

// SignalName normalises "TERM", "sigterm" or "SIGTERM" to "SIGTERM".
func SignalName(sig string) (string, error) {
	k := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(sig)), "SIG")
	if n, ok := signals[k]; ok {
		return n, nil
	}
	return "", fmt.Errorf("bundle: unsupported signal %q", sig)
}

// PSEntry is one process of `runsc ps --format=json`.
type PSEntry struct {
	PID  int `json:"pid"`
	PPID int `json:"ppid"`
}

// ParsePS reads `runsc ps --format=json` output.
func ParsePS(out []byte) ([]PSEntry, error) {
	var ps []PSEntry
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, fmt.Errorf("bundle: parse runsc ps: %w", err)
	}
	return ps, nil
}

// Tree returns pid and all its descendants in ps, deepest first.
func Tree(ps []PSEntry, pid int) []int {
	children := map[int][]int{}
	for _, p := range ps {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	var out []int
	var walk func(int)
	seen := map[int]bool{}
	walk = func(p int) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, c := range children[p] {
			walk(c)
		}
		out = append(out, p)
	}
	walk(pid)
	return out
}

// ExecProcessSpec builds the process.json for one Exec: argv, the merged
// environment (spec env, then opts env, then proxy variables), cwd, the
// workload user, the same rlimits and no new privileges. Passing it as a
// file keeps secrets out of the host's process list.
func (s *Sandbox) ExecProcessSpec(argv []string, opts sandbox.ExecOpts) (*Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("bundle: empty argv")
	}
	if opts.TTY {
		return nil, ErrTTY
	}
	cwd := WorkspaceDir
	switch {
	case opts.Dir == "":
	case strings.HasPrefix(opts.Dir, "/"):
		cwd = opts.Dir
	default:
		c, err := Confine(opts.Dir, true)
		if err != nil {
			return nil, err
		}
		cwd = c
	}
	env := make(map[string]string, len(s.spec.Env)+len(opts.Env))
	for k, v := range s.spec.Env {
		env[k] = v
	}
	for k, v := range opts.Env {
		env[k] = v
	}
	caps := append([]string(nil), DefaultCaps...)
	return &Process{
		User: s.user,
		Args: append([]string(nil), argv...),
		Env:  Env(env, s.proxyURL, HomeFor(s.user)),
		Cwd:  cwd,
		Capabilities: &Capabilities{
			Bounding: caps, Effective: caps, Permitted: caps, Inheritable: []string{},
		},
		Rlimits:         Rlimits(s.spec.Limits),
		NoNewPrivileges: true,
	}, nil
}

// Exec implements sandbox.Sandbox. Cancelling ctx or sending KILL kills the
// process and all its descendants (by the process tree from `runsc ps`).
// The sandbox wall clock kills everything and Wait reports ErrTimeout.
func (s *Sandbox) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.Process, error) {
	if err := s.live(); err != nil {
		return nil, err
	}
	ps, err := s.ExecProcessSpec(argv, opts)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(ps)
	if err != nil {
		return nil, err
	}
	n := s.seq.Add(1)
	procFile := fmt.Sprintf("%s/proc/%d.json", s.dir, n)
	pidFile := fmt.Sprintf("%s/proc/%d.pid", s.dir, n)
	if err := s.writePrivate(ctx, procFile, data); err != nil {
		return nil, err
	}
	w, err := s.b.wrap(s.runsc.ExecProcess(s.cid, procFile, pidFile))
	if err != nil {
		return nil, err
	}

	// The local runsc exec client lives on its own context: cancellation
	// first kills the process tree inside the sandbox, then the client.
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &proc{
		s:       s,
		pidFile: pidFile,
		files:   []string{procFile, pidFile},
		stdout:  newBufPipe(streamBuffer),
		stderr:  newBufPipe(streamBuffer),
		done:    make(chan struct{}),
	}
	h, err := s.b.cfg.Runner.Start(runCtx, Cmd{Argv: w, Stdin: opts.Stdin, Stdout: p.stdout, Stderr: p.stderr})
	if err != nil {
		runCancel()
		return nil, err
	}
	s.running.Add(1)
	go p.watch(ctx, runCancel)
	go func() {
		err := h.Wait()
		p.finish(err)
		s.running.Add(-1)
		runCancel()
	}()
	return p, nil
}

type proc struct {
	s       *Sandbox
	pidFile string
	files   []string
	stdout  *bufPipe
	stderr  *bufPipe
	done    chan struct{}

	mu     sync.Mutex
	pid    int
	reason error // ErrKilled when we killed it
	code   int
	err    error
}

func (p *proc) Stdout() io.Reader { return p.stdout }
func (p *proc) Stderr() io.Reader { return p.stderr }

// watch kills the process tree when the caller's context ends.
func (p *proc) watch(ctx context.Context, runCancel context.CancelFunc) {
	select {
	case <-p.done:
		return
	case <-ctx.Done():
	}
	p.mu.Lock()
	p.reason = sandbox.ErrKilled
	p.mu.Unlock()
	_ = p.killTree()
	select {
	case <-p.done:
	case <-time.After(killGrace):
		runCancel()
	}
}

func (p *proc) finish(err error) {
	p.mu.Lock()
	switch {
	case err == nil:
		p.code = 0
	case exitCode(err) >= 0:
		p.code = exitCode(err)
	default:
		p.code = -1
		p.err = err
	}
	switch {
	case p.s.timedOut.Load():
		p.reason = sandbox.ErrTimeout
	case p.reason == nil && p.s.destroyed.Load():
		p.reason = sandbox.ErrKilled
	}
	if p.reason != nil {
		p.err = fmt.Errorf("%w: %s", p.reason, p.s.spec.ID)
	}
	p.mu.Unlock()
	p.stdout.CloseWrite(nil)
	p.stderr.CloseWrite(nil)
	if !p.s.destroyed.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for _, f := range p.files {
			_ = p.s.b.run(ctx, RemoveArgv(f), nil, nil)
		}
		cancel()
	}
	close(p.done)
}

// Wait implements sandbox.Process.
func (p *proc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.err
}

// Signal implements sandbox.Process through `runsc kill --pid`. KILL takes
// the whole process tree; other signals go to the process only.
func (p *proc) Signal(sig string) error {
	name, err := SignalName(sig)
	if err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	if name == "SIGKILL" {
		return p.killTree()
	}
	pid, err := p.readPid()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.s.b.run(ctx, p.s.runsc.KillPid(p.s.cid, pid, name), nil, nil)
}

// killTree sends SIGKILL to the process and every descendant, twice, to
// catch children forked between the listing and the kill. A descendant that
// already got re-parented to the container init escapes; Destroy and the
// wall clock kill everything.
func (p *proc) killTree() error {
	pid, err := p.readPid()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for round := 0; round < 2; round++ {
		pids := []int{pid}
		if out, err := p.s.b.out(ctx, p.s.runsc.PS(p.s.cid)); err == nil {
			if ps, err := ParsePS(out); err == nil {
				pids = Tree(ps, pid)
			}
		}
		for _, q := range pids {
			if err := p.s.b.run(ctx, p.s.runsc.KillPid(p.s.cid, q, "SIGKILL"), nil, nil); err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

// readPid reads the sandbox-internal pid runsc wrote, retrying briefly
// because the file appears only after the process started.
func (p *proc) readPid() (int, error) {
	p.mu.Lock()
	pid := p.pid
	p.mu.Unlock()
	if pid > 0 {
		return pid, nil
	}
	var lastErr error
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := p.s.b.out(ctx, CatArgv(p.pidFile))
		cancel()
		if err == nil {
			if pid, err = ParsePid(out); err == nil {
				p.mu.Lock()
				p.pid = pid
				p.mu.Unlock()
				return pid, nil
			}
		}
		lastErr = err
		select {
		case <-p.done:
			return 0, errors.New("bundle: process exited")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("bundle: no pid for process: %w", lastErr)
}
