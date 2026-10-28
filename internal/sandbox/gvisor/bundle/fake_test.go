package bundle

import (
	"context"
	"io"
	"strings"
	"sync"
)

// call is one recorded command.
type call struct {
	argv  []string
	stdin string
}

func (c call) String() string { return strings.Join(c.argv, " ") }

// reply is what the fake returns for a command.
type reply struct {
	stdout string
	err    error
	// block, when set, keeps a started command running until closed.
	block chan struct{}
}

// fakeRunner records every command and answers by longest matching prefix
// of the space-joined argv.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	replies map[string]reply
}

func newFake() *fakeRunner { return &fakeRunner{replies: map[string]reply{}} }

func (f *fakeRunner) on(prefix string, r reply) { f.mu.Lock(); f.replies[prefix] = r; f.mu.Unlock() }

func (f *fakeRunner) lookup(argv []string) reply {
	line := strings.Join(argv, " ")
	best, bestLen := reply{}, -1
	for p, r := range f.replies {
		// "*text" matches anywhere in the line.
		hit := strings.HasPrefix(line, p)
		if strings.HasPrefix(p, "*") {
			hit = strings.Contains(line, p[1:])
		}
		if hit && len(p) > bestLen {
			best, bestLen = r, len(p)
		}
	}
	return best
}

func (f *fakeRunner) record(c Cmd) reply {
	var in string
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{argv: append([]string(nil), c.Argv...), stdin: in})
	r := f.lookup(c.Argv)
	f.mu.Unlock()
	return r
}

func (f *fakeRunner) Run(ctx context.Context, c Cmd) error {
	h, err := f.Start(ctx, c)
	if err != nil {
		return err
	}
	return h.Wait()
}

func (f *fakeRunner) Start(ctx context.Context, c Cmd) (Handle, error) {
	// Stdin is drained synchronously so the order of recorded calls is
	// deterministic, except for blocking commands whose stdin may stream.
	f.mu.Lock()
	r := f.lookup(c.Argv)
	f.mu.Unlock()
	if r.block != nil {
		f.mu.Lock()
		f.calls = append(f.calls, call{argv: append([]string(nil), c.Argv...)})
		f.mu.Unlock()
		h := &fakeHandle{done: make(chan struct{})}
		go func() {
			select {
			case <-r.block:
			case <-ctx.Done():
			}
			if c.Stdout != nil && r.stdout != "" {
				_, _ = io.WriteString(c.Stdout, r.stdout)
			}
			h.err = r.err
			close(h.done)
		}()
		return h, nil
	}
	if pr, ok := c.Stdin.(*io.PipeReader); ok {
		// A streaming consumer: record now, drain in the background.
		f.mu.Lock()
		idx := len(f.calls)
		f.calls = append(f.calls, call{argv: append([]string(nil), c.Argv...)})
		f.mu.Unlock()
		h := &fakeHandle{done: make(chan struct{}), err: r.err}
		go func() {
			b, _ := io.ReadAll(pr)
			f.mu.Lock()
			f.calls[idx].stdin = string(b)
			f.mu.Unlock()
			close(h.done)
		}()
		return h, nil
	}
	r = f.record(c)
	h := &fakeHandle{done: make(chan struct{}), err: r.err}
	if pw, ok := c.Stdout.(*io.PipeWriter); ok {
		// A streaming producer: write in the background like a process.
		go func() {
			if r.stdout != "" {
				_, _ = io.WriteString(pw, r.stdout)
			}
			close(h.done)
		}()
		return h, nil
	}
	if c.Stdout != nil && r.stdout != "" {
		_, _ = io.WriteString(c.Stdout, r.stdout)
	}
	close(h.done)
	return h, nil
}

type fakeHandle struct {
	done chan struct{}
	err  error
}

func (h *fakeHandle) Wait() error { <-h.done; return h.err }

func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.String()
	}
	return out
}

func (f *fakeRunner) find(prefix string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c.String(), prefix) {
			return c, true
		}
	}
	return call{}, false
}

func (f *fakeRunner) reset() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

// failed is an exit status 1.
func failed(argv ...string) error { return &ExitError{Argv: argv, Code: 1} }
