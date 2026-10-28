//go:build fakesandbox

package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/fake"
	"github.com/richard-sen27/mycelium/internal/sandbox/internal/sbxtest"
)

func TestMain(m *testing.M) {
	sbxtest.MaybeRun()
	os.Exit(m.Run())
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newRunner(t *testing.T) (*sandbox.Runner, *syncBuf, sandbox.Spec) {
	t.Helper()
	t.Setenv(fake.EnvOK, "1")
	logs := &syncBuf{}
	r := &sandbox.Runner{Backend: fake.Backend{}, Logs: logs, StatsInterval: 20 * time.Millisecond, KillGrace: 300 * time.Millisecond}
	spec := sandbox.Spec{ID: "sbx:test", Image: "none", Env: sbxtest.Env(), Dir: t.TempDir()}
	return r, logs, spec
}

func TestSelectFake(t *testing.T) {
	p := &mycelv1.Profile{Name: "fake", Backends: []string{sandbox.BackendFake}}
	t.Setenv(fake.EnvOK, "")
	if _, err := sandbox.Select(context.Background(), p); !errors.Is(err, sandbox.ErrUnavailable) || !strings.Contains(err.Error(), fake.EnvOK) {
		t.Fatalf("Select without env = %v", err)
	}
	t.Setenv(fake.EnvOK, "1")
	b, err := sandbox.Select(context.Background(), p)
	if err != nil || b.Name() != sandbox.BackendFake {
		t.Fatalf("Select = %v, %v", b, err)
	}
	// A profile that does not list fake never gets it.
	if _, err := sandbox.Select(context.Background(), &mycelv1.Profile{Name: "d", Backends: []string{sandbox.BackendGVisor}}); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("Select(gvisor) = %v", err)
	}
}

func TestRunSucceededWithLogsAndOutputs(t *testing.T) {
	r, logs, spec := newRunner(t)
	var samples int
	var mu sync.Mutex
	r.OnStats = func(sandbox.Stats) { mu.Lock(); samples++; mu.Unlock() }

	// Write two outputs, then echo.
	seed := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(seed, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Workspace.Inputs = []sandbox.Input{{LocalPath: seed, Path: "in.txt"}}
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("write", "out/result.txt", "forty", "two"), []string{"out/**", "nothing/*"}, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED || res.ExitCode != 0 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Path != "out/result.txt" || res.Outputs[0].Size != int64(len("forty two")) {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	want := filepath.Join(spec.Dir, "outputs", "out", "result.txt")
	if res.Outputs[0].LocalPath != want {
		t.Fatalf("LocalPath = %s, want %s", res.Outputs[0].LocalPath, want)
	}
	if b, _ := os.ReadFile(want); string(b) != "forty two" {
		t.Fatalf("output content %q", b)
	}
	// The sandbox's own directory is gone; outputs survive.
	if _, err := os.Stat(filepath.Join(spec.Dir, "sandbox")); !os.IsNotExist(err) {
		t.Fatalf("sandbox dir not removed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if samples == 0 {
		t.Fatal("no stats samples")
	}
	if res.Stats.RunningSince.IsZero() {
		t.Fatal("no RunningSince")
	}
	_ = logs

	// Logs: stdout and stderr both arrive.
	r2, logs2, spec2 := newRunner(t)
	res, err = r2.Run(context.Background(), spec2, sbxtest.Argv("echo", "to-out\n", "to-err\n"), nil, 0)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if s := logs2.String(); !strings.Contains(s, "to-out\n") || !strings.Contains(s, "to-err\n") {
		t.Fatalf("logs = %q", s)
	}
}

func TestRunFailed(t *testing.T) {
	r, _, spec := newRunner(t)
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("exit", "3"), nil, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_FAILED || res.ExitCode != 3 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestRunWorkingDirectory(t *testing.T) {
	r, logs, spec := newRunner(t)
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("pwd"), nil, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	got := strings.TrimSpace(logs.String())
	want := filepath.Join(spec.Dir, "sandbox", "workspace")
	gr, _ := filepath.EvalSymlinks(filepath.Dir(got))
	wr, _ := filepath.EvalSymlinks(filepath.Dir(want))
	if filepath.Base(got) != "workspace" || gr != wr {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
}

func TestRunTimeout(t *testing.T) {
	r, _, spec := newRunner(t)
	start := time.Now()
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("sleep"), nil, 200*time.Millisecond)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestRunTimeoutIgnoringTerm(t *testing.T) {
	r, logs, spec := newRunner(t)
	start := time.Now()
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("ignoreterm"), nil, 500*time.Millisecond)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT || res.ExitCode != -1 {
		t.Fatalf("Run = %+v, %v (logs %q)", res, err, logs.String())
	}
	if !strings.Contains(logs.String(), "ready") {
		t.Fatalf("helper never got ready (TERM may have hit before signal.Ignore): %q", logs.String())
	}
	if d := time.Since(start); d < 500*time.Millisecond+r.KillGrace {
		t.Fatalf("KILL came before the grace period: %s", d)
	}
}

func TestRunOutputsOnTimeout(t *testing.T) {
	r, _, spec := newRunner(t)
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("writesleep", "partial.txt", "half"), []string{"*.txt"}, 500*time.Millisecond)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Path != "partial.txt" || res.Outputs[0].Size != 4 {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
}

func TestRunOutputLimitAndStaleOutputs(t *testing.T) {
	r, _, spec := newRunner(t)
	stale := filepath.Join(spec.Dir, "outputs", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.MaxOutputBytes = 5
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("write", "big.txt", "0123456789"), []string{"*.txt"}, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(res.Outputs) != 0 || !strings.Contains(res.Error, "size limit") {
		t.Fatalf("outputs %+v, error %q", res.Outputs, res.Error)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale output not cleared")
	}
	if _, err := os.Stat(filepath.Join(spec.Dir, "outputs", "big.txt")); !os.IsNotExist(err) {
		t.Fatal("oversized output left behind")
	}
}

func TestRunDefaultWallClock(t *testing.T) {
	r, _, spec := newRunner(t)
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("exit", "0"), nil, 0)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestRunWallClockCapsTimeout(t *testing.T) {
	r, _, spec := newRunner(t)
	spec.Limits.WallClock = 200 * time.Millisecond
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("sleep"), nil, time.Hour)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestRunCancel(t *testing.T) {
	r, _, spec := newRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	res, err := r.Run(ctx, spec, sbxtest.Argv("sleep"), []string{"**"}, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_CANCELLED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(spec.Dir, "sandbox")); !os.IsNotExist(err) {
		t.Fatalf("sandbox dir not removed after cancel: %v", err)
	}
}

func TestRunLogCap(t *testing.T) {
	r, logs, spec := newRunner(t)
	r.MaxLogBytes = 1000
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("flood", "100000"), nil, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	s := logs.String()
	if !strings.HasSuffix(s, sandbox.LogTruncatedMarker) || len(s) != 1000+len(sandbox.LogTruncatedMarker) {
		t.Fatalf("log len %d, suffix %q", len(s), s[max(0, len(s)-60):])
	}
}

func TestRunSetupErrors(t *testing.T) {
	r, _, spec := newRunner(t)
	if res, err := r.Run(context.Background(), spec, nil, nil, 0); err == nil || res.State != mycelv1.JobState_JOB_STATE_FAILED {
		t.Fatalf("empty argv: %+v, %v", res, err)
	}
	if _, err := r.Run(context.Background(), spec, sbxtest.Argv("exit", "0"), []string{"../x"}, 0); err == nil {
		t.Fatal("escaping output pattern accepted")
	}
	bad := spec
	bad.Dir = "relative"
	if _, err := r.Run(context.Background(), bad, sbxtest.Argv("exit", "0"), nil, 0); err == nil {
		t.Fatal("relative Dir accepted")
	}
	if _, err := r.Run(context.Background(), spec, []string{filepath.Join(t.TempDir(), "no-such-binary")}, nil, 0); err == nil {
		t.Fatal("exec of a missing binary succeeded")
	}
	t.Setenv(fake.EnvOK, "")
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("exit", "0"), nil, 0)
	if !errors.Is(err, fake.ErrRefused) || res.State != mycelv1.JobState_JOB_STATE_FAILED {
		t.Fatalf("refused: %+v, %v", res, err)
	}
}
