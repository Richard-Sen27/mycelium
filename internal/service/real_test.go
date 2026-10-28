package service_test

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/service"
)

// TestRealService installs the daemon with the real service manager of this
// OS, checks that it runs, stops, starts and uninstalls. It changes the
// user's launchd / systemd / Task Scheduler state, so it is opt-in:
//
//	MYCEL_TEST_SERVICE=1 go test -run TestRealService -v ./internal/service/
//
// It refuses to run when a mycel service is already installed.
func TestRealService(t *testing.T) {
	if os.Getenv("MYCEL_TEST_SERVICE") != "1" {
		t.Skip("set MYCEL_TEST_SERVICE=1 to install a real per-user service")
	}
	if m, err := service.New(""); err != nil {
		t.Fatal(err)
	} else if st, err := m.Status(); err != nil {
		t.Fatal(err)
	} else if st.Installed {
		t.Skipf("a mycel service is already installed (%s); not touching it", st.Path)
	}

	// The unit refuses a binary in a temp dir: build into the user cache.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(cache, "mycel-service-test")
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	exe := filepath.Join(binDir, "mycel")
	if os.PathSeparator == '\\' {
		exe += ".exe"
	}
	build := exec.Command("go", "build", "-o", exe, "github.com/richard-sen27/mycelium/cmd/mycel") //nolint:gosec // test builds into its own directory
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	home := t.TempDir()
	mycel := func(args ...string) (string, error) {
		cmd := exec.Command(exe, args...) //nolint:gosec // the binary this test built
		cmd.Env = append(os.Environ(), "MYCEL_HOME="+home)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := mycel(args...)
		if err != nil {
			t.Fatalf("mycel %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	api, bus := freePort(t), freePort(t)
	must("hub", "init", "--advertise", "https://127.0.0.1:"+api, "--api-listen", "127.0.0.1:"+api,
		"--bus-listen", "127.0.0.1:"+bus, "--trust-domain", "svc-test", "--node-name", "svc-test", "--json")

	must("service", "install")
	t.Cleanup(func() {
		if out, err := mycel("service", "uninstall"); err != nil && !strings.Contains(out, "not installed") {
			t.Errorf("cleanup uninstall: %v\n%s", err, out)
		}
	})
	type status struct {
		Installed bool   `json:"installed"`
		Running   bool   `json:"running"`
		PID       int    `json:"pid"`
		LogFile   string `json:"log_file"`
	}
	waitStatus := func(want func(status) bool) status {
		t.Helper()
		var st status
		deadline := time.Now().Add(30 * time.Second)
		for {
			if err := json.Unmarshal([]byte(must("service", "status", "--json")), &st); err != nil {
				t.Fatal(err)
			}
			if want(st) {
				return st
			}
			if time.Now().After(deadline) {
				logs, _ := mycel("service", "logs")
				t.Fatalf("service status stuck at %+v\nlogs:\n%s", st, logs)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	st := waitStatus(func(s status) bool { return s.Installed && s.Running && s.PID > 0 })
	t.Logf("running: pid %d, log %s", st.PID, st.LogFile)

	deadline := time.Now().Add(30 * time.Second)
	for {
		logs, _ := mycel("service", "logs", "-n", "1000")
		if strings.Contains(logs, "node ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never logged \"node ready\":\n%s", logs)
		}
		time.Sleep(250 * time.Millisecond)
	}

	must("service", "stop")
	waitStatus(func(s status) bool { return s.Installed && !s.Running })
	must("service", "start")
	waitStatus(func(s status) bool { return s.Running && s.PID > 0 })
	must("service", "uninstall")
	waitStatus(func(s status) bool { return !s.Installed && !s.Running })
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}
