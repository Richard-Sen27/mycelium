//go:build darwin

package applevf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// runner runs limactl. Every limactl invocation of the backend goes through
// it so unit tests can replace it with a recording fake. Arguments are always
// an argv; nothing is ever passed through a host shell.
type runner interface {
	// Run runs limactl args to completion with stdin (may be nil) and
	// returns its output. A non-zero exit is returned as an error that
	// exitCode understands.
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout, stderr []byte, err error)
	// Start starts a long-running limactl (Exec, CopyOut). The command is
	// not tied to a context: the caller kills it.
	Start(stdin io.Reader, args ...string) (runningCmd, error)
}

// runningCmd is a started limactl process. Its Stdout and Stderr must be
// drained concurrently; each keeps up to maxBufferedOutput bytes in memory
// and spills the rest to a file in the sandbox directory, so neither an
// unread stream nor a slow reader blocks the process or Wait.
type runningCmd interface {
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait waits for exit; it returns the exit code (-1 if the process did
	// not exit normally).
	Wait() (int, error)
	Kill()
}

// limactlRunner runs the real limactl binary with a scrubbed environment
// (env) in its own process group.
type limactlRunner struct {
	path     string   // limactl executable
	env      []string // complete environment of every call
	spillDir string   // where large outputs of started commands spill ("" = block)
}

func (r limactlRunner) command(args []string) *exec.Cmd {
	cmd := exec.Command(r.path, args...) //nolint:gosec // argv only, binary from LookPath
	cmd.Env = r.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	return cmd
}

// killGroup kills the process group of a started command.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func (r limactlRunner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	cmd := r.command(args)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = killGroup(cmd)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%w (%w)", ctx.Err(), err)
	}
	return out.Bytes(), errb.Bytes(), err
}

func (r limactlRunner) Start(stdin io.Reader, args ...string) (runningCmd, error) {
	cmd := r.command(args)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	rc := &limactlCmd{cmd: cmd, stdout: newBufPipe(maxBufferedOutput, r.spillDir), stderr: newBufPipe(maxBufferedOutput, r.spillDir)}
	cmd.Stdout = rc.stdout
	cmd.Stderr = rc.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return rc, nil
}

type limactlCmd struct {
	cmd    *exec.Cmd
	stdout *bufPipe
	stderr *bufPipe
}

func (c *limactlCmd) Stdout() io.Reader { return c.stdout }
func (c *limactlCmd) Stderr() io.Reader { return c.stderr }
func (c *limactlCmd) Kill()             { _ = killGroup(c.cmd) }

func (c *limactlCmd) Wait() (int, error) {
	err := c.cmd.Wait()
	c.stdout.CloseWrite()
	c.stderr.CloseWrite()
	code := -1
	if c.cmd.ProcessState != nil {
		code = c.cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code < 0 { // limactl itself died from a signal
			return code, err
		}
		return code, nil
	}
	// ErrWaitDelay: the process exited but a caller-supplied stdin was still
	// open; the exit code is valid.
	if errors.Is(err, exec.ErrWaitDelay) && code >= 0 {
		return code, nil
	}
	return code, err
}

// exitCode extracts the exit code of a failed Run (any error with an
// ExitCode method, such as *exec.ExitError), or -1.
func exitCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// cmdError formats a failed limactl call with the tail of its stderr.
func cmdError(what string, err error, stderr []byte) error {
	tail := strings.TrimSpace(string(stderr))
	if len(tail) > 2000 {
		tail = "…" + tail[len(tail)-2000:]
	}
	if tail == "" {
		return fmt.Errorf("applevf: %s: %w", what, err)
	}
	return fmt.Errorf("applevf: %s: %w: %s", what, err, tail)
}

// bufPipe is an in-memory pipe that never blocks its writer: up to limit
// bytes stay in memory, beyond that the writer appends to a spill file in
// dir that the reader consumes in order. With dir == "" (or if the spill file
// cannot be created) the writer blocks while limit bytes are unread.
type bufPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	mem    bytes.Buffer
	limit  int
	dir    string
	spill  *os.File
	spillW int64 // bytes written to spill
	spillR int64 // bytes read from spill
	closed bool  // writer side closed
	rdone  bool  // reader gave up; writes are discarded
}

func newBufPipe(limit int, dir string) *bufPipe {
	p := &bufPipe{limit: limit, dir: dir}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *bufPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for len(b) > 0 {
		if p.rdone {
			return n + len(b), nil
		}
		if p.closed {
			return n, io.ErrClosedPipe
		}
		if p.spill != nil {
			k, err := p.spill.WriteAt(b, p.spillW)
			p.spillW += int64(k)
			n += k
			b = b[k:]
			p.cond.Broadcast()
			if err != nil {
				return n, err
			}
			continue
		}
		if room := p.limit - p.mem.Len(); room > 0 {
			k := min(room, len(b))
			p.mem.Write(b[:k])
			b = b[k:]
			n += k
			p.cond.Broadcast()
			continue
		}
		if p.dir != "" {
			f, err := os.CreateTemp(p.dir, ".stream-*")
			if err == nil {
				_ = os.Remove(f.Name()) // unlinked: gone when closed, whatever happens
				p.spill, p.spillW, p.spillR = f, 0, 0
				continue
			}
			p.dir = "" // cannot spill: fall back to blocking
		}
		p.cond.Wait()
	}
	return n, nil
}

func (p *bufPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.mem.Len() > 0 {
			n, _ := p.mem.Read(b)
			p.cond.Broadcast()
			return n, nil
		}
		if p.spill != nil {
			if p.spillR < p.spillW {
				want := min(int64(len(b)), p.spillW-p.spillR)
				n, err := p.spill.ReadAt(b[:want], p.spillR)
				p.spillR += int64(n)
				if n > 0 {
					return n, nil
				}
				if err != nil {
					return 0, err
				}
				continue
			}
			p.dropSpill() // drained: back to memory
			continue
		}
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}
}

func (p *bufPipe) dropSpill() {
	if p.spill != nil {
		_ = p.spill.Close()
		p.spill = nil
	}
}

// CloseWrite ends the stream; readers get EOF after the buffered data.
func (p *bufPipe) CloseWrite() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// CloseRead discards further output and unblocks the writer.
func (p *bufPipe) CloseRead() {
	p.mu.Lock()
	p.rdone = true
	p.mem.Reset()
	p.dropSpill()
	p.cond.Broadcast()
	p.mu.Unlock()
}
