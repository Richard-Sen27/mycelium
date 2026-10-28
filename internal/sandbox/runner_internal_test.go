package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// mockProc is a scripted Process: wait decides how it ends, given the
// signals received so far.
type mockProc struct {
	stdout io.Reader
	mu     sync.Mutex
	sigs   []string
	sigCh  chan string
	wait   func(sigs <-chan string) (int, error)
}

func (p *mockProc) Stdout() io.Reader { return p.stdout }
func (p *mockProc) Stderr() io.Reader { return nil }
func (p *mockProc) Wait() (int, error) {
	return p.wait(p.sigCh)
}

func (p *mockProc) Signal(sig string) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, sig)
	p.mu.Unlock()
	select {
	case p.sigCh <- sig:
	default:
	}
	return nil
}

func (p *mockProc) signals() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.sigs, ",")
}

type mockSandbox struct {
	proc      *mockProc
	mu        sync.Mutex
	destroyed int
}

func (s *mockSandbox) ID() string      { return "sbx:mock" }
func (s *mockSandbox) Backend() string { return "mock" }
func (s *mockSandbox) Exec(context.Context, []string, ExecOpts) (Process, error) {
	return s.proc, nil
}
func (s *mockSandbox) CopyIn(context.Context, io.Reader, string, uint32) error { return nil }
func (s *mockSandbox) CopyOut(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("none")
}
func (s *mockSandbox) Glob(context.Context, []string) ([]string, error) { return nil, nil }
func (s *mockSandbox) Pause(context.Context) error                      { return nil }
func (s *mockSandbox) Resume(context.Context) error                     { return nil }
func (s *mockSandbox) Stats(context.Context) (Stats, error)             { return Stats{}, nil }
func (s *mockSandbox) Destroy(context.Context) error {
	s.mu.Lock()
	s.destroyed++
	s.mu.Unlock()
	return nil
}

func (s *mockSandbox) destroys() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.destroyed
}

type mockBackend struct{ sb *mockSandbox }

func (b mockBackend) Name() string                                        { return "mock" }
func (b mockBackend) Available(context.Context) (bool, string)            { return true, "" }
func (b mockBackend) Capabilities() Capabilities                          { return Capabilities{} }
func (b mockBackend) Prepare(context.Context, string, func(string)) error { return nil }
func (b mockBackend) Create(context.Context, Spec) (Sandbox, error)       { return b.sb, nil }

func mockRun(t *testing.T, p *mockProc, timeout time.Duration) (Result, *mockSandbox, *bytes.Buffer, error) {
	t.Helper()
	if p.sigCh == nil {
		p.sigCh = make(chan string, 8)
	}
	if p.stdout == nil {
		p.stdout = strings.NewReader("")
	}
	sb := &mockSandbox{proc: p}
	var logs bytes.Buffer
	r := &Runner{Backend: mockBackend{sb}, Logs: &lockedWriter{w: &logs}, KillGrace: 100 * time.Millisecond, StatsInterval: time.Hour}
	res, err := r.Run(context.Background(), Spec{Dir: t.TempDir()}, []string{"x"}, nil, timeout)
	return res, sb, &logs, err
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestRunnerBackendTimeout(t *testing.T) {
	p := &mockProc{wait: func(<-chan string) (int, error) { return -1, ErrTimeout }}
	res, _, _, err := mockRun(t, p, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestRunnerKillsGroupAfterNormalExit(t *testing.T) {
	p := &mockProc{wait: func(<-chan string) (int, error) { return 0, nil }}
	res, _, _, err := mockRun(t, p, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if p.signals() != "KILL" {
		t.Fatalf("signals = %q, want KILL", p.signals())
	}
}

func TestRunnerKeepsKillTimerWhenLeaderExits(t *testing.T) {
	// The leader dies on TERM; its children (not modelled) must still get
	// KILL once the grace period is over.
	p := &mockProc{wait: func(sigs <-chan string) (int, error) {
		for s := range sigs {
			if s == "TERM" {
				return -1, ErrKilled
			}
		}
		return 0, nil
	}}
	start := time.Now()
	res, _, _, err := mockRun(t, p, 50*time.Millisecond)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if p.signals() != "TERM,KILL" {
		t.Fatalf("signals = %q", p.signals())
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("KILL came before the grace period: %s", d)
	}
}

func TestRunnerKillFailed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := &mockProc{wait: func(<-chan string) (int, error) { <-release; return 0, nil }}
	res, sb, _, err := mockRun(t, p, 50*time.Millisecond)
	if !errors.Is(err, ErrKillFailed) || res.State != mycelv1.JobState_JOB_STATE_FAILED || !strings.Contains(res.Error, "kill failed") {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if p.signals() != "TERM,KILL" || sb.destroys() == 0 {
		t.Fatalf("signals %q, destroys %d", p.signals(), sb.destroys())
	}
}

func TestRunnerDrainTimeout(t *testing.T) {
	saved := drainTimeout
	drainTimeout = 50 * time.Millisecond
	defer func() { drainTimeout = saved }()

	pr, pw := io.Pipe() // a reader that never ends, even after Destroy
	defer func() { _ = pw.Close() }()
	p := &mockProc{stdout: pr, wait: func(<-chan string) (int, error) { return 0, nil }}
	go func() { _, _ = pw.Write([]byte("before\n")) }()
	start := time.Now()
	res, sb, logs, err := mockRun(t, p, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("drain not bounded: %s", d)
	}
	if sb.destroys() < 2 { // once to unblock the pipes, once deferred
		t.Fatalf("destroys = %d", sb.destroys())
	}
	// Writes after Run returned never reach the caller's writer.
	done := make(chan struct{})
	go func() { _, _ = pw.Write([]byte("after\n")); close(done) }()
	<-done
	time.Sleep(20 * time.Millisecond)
	if strings.Contains(logs.String(), "after") {
		t.Fatalf("log written after Run returned: %q", logs.String())
	}
}
