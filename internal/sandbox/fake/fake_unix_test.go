//go:build fakesandbox && unix

package fake

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/internal/sbxtest"
)

// gone waits up to 3 s for pid to disappear (it may need reaping by init).
func gone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// spawnChild runs "spawn MODE exit" and returns the leader process after it
// exited and the background child's pid.
func spawnChild(t *testing.T, s *Sandbox, mode string) (sandbox.Process, int) {
	t.Helper()
	p, err := s.Exec(context.Background(), sbxtest.Argv("spawn", mode, "exit"), sandbox.ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid := sbxtest.ChildPID(line)
	if pid == 0 {
		t.Fatalf("no child pid in %q", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("leader: %d %v", code, err)
	}
	if !alive(pid) {
		t.Fatal("child died with the leader")
	}
	return p, pid
}

func TestDestroyKillsBackgroundChild(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	_, pid := spawnChild(t, s, "ignoreterm")
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !gone(pid) {
		t.Fatalf("child %d survived Destroy", pid)
	}
}

func TestKillAfterLeaderExitReachesGroup(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	p, pid := spawnChild(t, s, "sleep")
	if err := p.Signal("KILL"); err != nil {
		t.Fatal(err)
	}
	if !gone(pid) {
		t.Fatalf("child %d survived KILL after the leader exited", pid)
	}
	// The group is gone now: signalling it again is still fine.
	if err := p.Signal("KILL"); err != nil {
		t.Fatalf("KILL on a gone group = %v", err)
	}
}

func TestFIFORefused(t *testing.T) {
	s := mustCreate(t, sandbox.Spec{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.CopyIn(ctx, strings.NewReader("x"), "out/real.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(s.WorkspaceDir(), "out", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Glob(ctx, []string{"out/*"})
	if err != nil || strings.Join(got, ",") != "out/real.txt" {
		t.Fatalf("Glob = %v, %v", got, err)
	}
	// As if out/real.txt had been swapped for a FIFO after Glob: CopyOut
	// must neither block nor return it.
	done := make(chan error, 1)
	go func() {
		_, err := s.CopyOut(ctx, "out/pipe")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("CopyOut(fifo) = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("CopyOut blocked on a FIFO")
	}
	// The directory tar skips it.
	rc, err := s.CopyOut(ctx, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "real.txt" {
		t.Fatalf("tar = %v", names)
	}
	// A cancelled context stops a read.
	cctx, ccancel := context.WithCancel(context.Background())
	rc2, err := s.CopyOut(cctx, "out/real.txt")
	if err != nil {
		t.Fatal(err)
	}
	ccancel()
	if _, err := io.ReadAll(rc2); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v", err)
	}
	_ = rc2.Close()
}
