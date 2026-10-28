package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Cmd is one external command. Argv[0] is the program; there is no shell.
// Nil Stdin reads from the null device, nil Stdout discards output.
type Cmd struct {
	Argv   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Detached runs with all stdio on the null device and no capture, for
	// commands that leave a daemon holding inherited descriptors
	// (runsc run --detach).
	Detached bool
}

// Handle is a started command.
type Handle interface {
	// Wait blocks until the command exits; a non-zero exit is an *ExitError.
	Wait() error
}

// Runner starts external commands. Tests replace it with a recording fake.
type Runner interface {
	// Run starts the command and waits for it.
	Run(ctx context.Context, c Cmd) error
	// Start starts the command and returns without waiting.
	Start(ctx context.Context, c Cmd) (Handle, error)
}

// ExitError is a command that ran and exited non-zero.
type ExitError struct {
	Argv   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s: exit status %d", strings.Join(e.Argv, " "), e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, c Cmd) error {
	h, err := r.Start(ctx, c)
	if err != nil {
		return err
	}
	return h.Wait()
}

// Start implements Runner.
func (ExecRunner) Start(ctx context.Context, c Cmd) (Handle, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("bundle: empty argv")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) // #nosec G204 -- argv built by this package, never a shell
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	var errBuf *limitedBuffer
	if c.Detached {
		cmd.Stdin, cmd.Stdout = nil, nil
	} else if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	} else {
		errBuf = &limitedBuffer{max: 4096}
		cmd.Stderr = errBuf
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execHandle{cmd: cmd, errBuf: errBuf}, nil
}

type execHandle struct {
	cmd    *exec.Cmd
	errBuf *limitedBuffer
}

func (h *execHandle) Wait() error {
	err := h.cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		x := &ExitError{Argv: h.cmd.Args, Code: ee.ExitCode()}
		if h.errBuf != nil {
			x.Stderr = h.errBuf.String()
		}
		return x
	}
	return err
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// output runs argv and returns its stdout.
func output(ctx context.Context, r Runner, argv []string) ([]byte, error) {
	var out bytes.Buffer
	err := r.Run(ctx, Cmd{Argv: argv, Stdout: &out})
	return out.Bytes(), err
}

// exitCode returns the exit code carried by err, or -1.
func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}
