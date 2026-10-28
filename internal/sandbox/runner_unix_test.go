//go:build fakesandbox && unix

package sandbox_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/sandbox/internal/sbxtest"
)

func goneWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// The leader dies on TERM, its background child ignores TERM: the KILL that
// follows the grace period must still reach the child.
func TestRunTimeoutKillsChildIgnoringTerm(t *testing.T) {
	r, logs, spec := newRunner(t)
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("spawn", "ignoreterm", "sleep"), nil, 500*time.Millisecond)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	pid := sbxtest.ChildPID(logs.String())
	if pid == 0 {
		t.Fatalf("no child pid in logs %q", logs.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !goneWithin(pid, 3*time.Second) {
		t.Fatalf("child %d survived the timeout", pid)
	}
}

// A job that exits normally but leaves a background child behind: the child
// is killed and Run does not hang on its open stdout.
func TestRunKillsLeftoverChild(t *testing.T) {
	r, logs, spec := newRunner(t)
	start := time.Now()
	res, err := r.Run(context.Background(), spec, sbxtest.Argv("spawn", "sleep", "exit"), nil, time.Minute)
	if err != nil || res.State != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Run took %s", d)
	}
	pid := sbxtest.ChildPID(logs.String())
	if pid == 0 {
		t.Fatalf("no child pid in logs %q", logs.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !goneWithin(pid, 3*time.Second) {
		t.Fatalf("leftover child %d survived", pid)
	}
}
