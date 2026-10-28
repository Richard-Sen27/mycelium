//go:build !windows

package discovery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brutella/dnssd"
)

// These tests run a fake "dns-sd" shell script in place of the system
// responder: nothing is announced on the network.

// fakeResponder writes an executable script named name that logs its
// start (to starts) and stderr, then runs body.
func fakeResponder(t *testing.T, name, body string) (path, starts string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, name)
	starts = filepath.Join(dir, "starts")
	script := "#!/bin/sh\necho start >> '" + starts + "'\necho \"responder says boom\" >&2\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	return path, starts
}

const loopForever = "while :; do sleep 1; done"

func useResponder(t *testing.T, os, path string) {
	t.Helper()
	g, lp := goos, lookPath
	t.Cleanup(func() { goos, lookPath = g, lp })
	goos = os
	lookPath = func(string) (string, error) { return path, nil }
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("responder %d still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPID(t *testing.T, file string) int {
	t.Helper()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(file) //nolint:gosec // test file
		if err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", file)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countLines(file string) int {
	b, _ := os.ReadFile(file) //nolint:gosec // test file
	return strings.Count(string(b), "\n")
}

var testHub = Hub{TrustDomain: "team", URL: "https://h:7777", BusURL: "tls://h:7778", CAFingerprint: strings.Repeat("e", 64)}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestSupervisorPIDFileAndClose(t *testing.T) {
	path, _ := fakeResponder(t, "dns-sd", loopForever)
	useResponder(t, "darwin", path)
	pidFile := filepath.Join(t.TempDir(), "mdns.pid")
	a, err := Advertise(context.Background(), testHub, AdvertiseOptions{PIDFile: pidFile})
	if err != nil {
		t.Fatal(err)
	}
	pid := readPID(t, pidFile)
	if cmd := processCommand(pid); !strings.Contains(cmd, "dns-sd") || !strings.Contains(cmd, ServiceType) {
		t.Fatalf("pid %d runs %q", pid, cmd)
	}
	_ = a.Close()
	waitGone(t, pid)
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pid file left after Close")
	}
}

// TestSupervisorKillsChildWhenHubPipeCloses: the hub dying closes the
// supervisor's stdin, and the responder goes with it.
func TestSupervisorKillsChildWhenHubPipeCloses(t *testing.T) {
	path, _ := fakeResponder(t, "dns-sd", loopForever)
	c, err := startChildOnce(path, []string{"-R", "team", ServiceType, "local", "7777"})
	if err != nil {
		t.Fatal(err)
	}
	if !alive(c.pid) {
		t.Fatalf("responder pid %d not running", c.pid)
	}
	_ = c.stdin.Close() // what the kernel does when the hub process dies
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit")
	}
	waitGone(t, c.pid)
}

func TestSupervisorArgumentsAreNotInterpolated(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	path := filepath.Join(dir, "dns-sd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+out+"'\n"+loopForever+"\n"), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	evil := []string{"-R", "$(touch " + filepath.Join(dir, "pwned") + ")", "`id`", "a b;c"}
	c, err := startChildOnce(path, evil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.stop()
	deadline := time.Now().Add(5 * time.Second)
	for countLines(out) < len(evil) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(out) //nolint:gosec // test file
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); strings.Join(got, "|") != strings.Join(evil, "|") {
		t.Fatalf("arguments %q, want %q (stderr %q)", got, evil, c.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("an argument was run by the shell")
	}
}

func TestSupervisorRestartsAndLogsStderr(t *testing.T) {
	setVarD(t, &restartMin, 50*time.Millisecond)
	setVarD(t, &restartMax, 100*time.Millisecond)
	path, starts := fakeResponder(t, "dns-sd", "exit 3")
	useResponder(t, "darwin", path)
	var logs syncBuffer
	a, err := Advertise(context.Background(), testHub, AdvertiseOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for countLines(starts) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := countLines(starts); n < 3 {
		t.Fatalf("responder started %d times, want restarts", n)
	}
	if l := logs.String(); !strings.Contains(l, "mDNS responder exited") || !strings.Contains(l, "responder says boom") {
		t.Fatalf("log lacks the exit and stderr: %s", l)
	}
}

type fakeAdv struct{ closed chan struct{} }

func (f *fakeAdv) Close() error { close(f.closed); return nil }

func TestAvahiQuickExitFallsBackToUserland(t *testing.T) {
	path, starts := fakeResponder(t, "avahi-publish-service", "exit 1")
	useResponder(t, "linux", path)
	used := make(chan dnssd.Service, 1)
	fa := &fakeAdv{closed: make(chan struct{})}
	old := startUserland
	startUserland = func(_ context.Context, srv dnssd.Service) (Advertiser, error) { used <- srv; return fa, nil }
	t.Cleanup(func() { startUserland = old })
	a, err := Advertise(context.Background(), testHub, AdvertiseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case srv := <-used:
		if !strings.HasPrefix(srvTarget(srv), "mycel-hub-") {
			t.Fatalf("fallback responder target %s", srvTarget(srv))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no fallback to the userland responder")
	}
	_ = a.Close()
	select {
	case <-fa.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback responder not closed")
	}
	if n := countLines(starts); n != 1 {
		t.Fatalf("avahi started %d times after falling back", n)
	}
}

func TestKillStaleResponder(t *testing.T) {
	path, _ := fakeResponder(t, "dns-sd", loopForever)
	stale := exec.Command(path, "-R", "team", ServiceType, "local", "7777") //nolint:gosec // test script
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = stale.Wait(); close(done) }()
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "mdns.pid")
	// A pid that runs something else is left alone.
	writePID(pidFile, other.Process.Pid, slog.New(slog.DiscardHandler))
	killStale(pidFile, slog.New(slog.DiscardHandler))
	if !alive(other.Process.Pid) {
		t.Fatal("killed an unrelated process")
	}
	writePID(pidFile, stale.Process.Pid, slog.New(slog.DiscardHandler))
	killStale(pidFile, slog.New(slog.DiscardHandler))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale responder not stopped")
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale pid file kept")
	}
}

func setVarD[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}
