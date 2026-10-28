package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/service"
)

type fakeServiceManager struct {
	calls   []string
	st      service.Status
	failErr error
}

func (f *fakeServiceManager) Install(role string) error {
	f.calls = append(f.calls, "install "+role)
	f.st = service.Status{Installed: true, Running: true, PID: 42, Path: "/x/io.mycel.daemon.plist"}
	return f.failErr
}
func (f *fakeServiceManager) Uninstall() error {
	f.calls = append(f.calls, "uninstall")
	return f.failErr
}
func (f *fakeServiceManager) Start() error { f.calls = append(f.calls, "start"); return f.failErr }
func (f *fakeServiceManager) Stop() error  { f.calls = append(f.calls, "stop"); return f.failErr }
func (f *fakeServiceManager) Status() (service.Status, error) {
	return f.st, nil
}

func useFakeService(t *testing.T) *fakeServiceManager {
	t.Helper()
	f := &fakeServiceManager{}
	old := newServiceManager
	newServiceManager = func(string) (service.Manager, error) { return f, nil }
	t.Cleanup(func() { newServiceManager = old })
	return f
}

func TestServiceInstall(t *testing.T) {
	tests := []struct {
		name          string
		hub, node     bool
		args          []string
		wantCode      int
		wantCall      string
		wantErrSubstr string
	}{
		{name: "nothing initialised", args: []string{"service", "install"}, wantCode: 1, wantErrSubstr: "mycel join <token>"},
		{name: "node", node: true, args: []string{"service", "install"}, wantCall: "install node"},
		{name: "hub machine defaults to hub", hub: true, node: true, args: []string{"service", "install"}, wantCall: "install hub"},
		{name: "explicit node on a hub machine", hub: true, node: true, args: []string{"service", "install", "--role", "node"}, wantCall: "install node"},
		{name: "hub role without hub", node: true, args: []string{"service", "install", "--role", "hub"}, wantCode: 1, wantErrSubstr: "mycel hub init"},
		{name: "bad role", node: true, args: []string{"service", "install", "--role", "relay"}, wantCode: 2, wantErrSubstr: "--role must be node or hub"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			initRoles(t, env.home, tt.hub, tt.node)
			f := useFakeService(t)
			res := newTestApp(t, "").run(tt.args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d: %+v", res.code, tt.wantCode, res)
			}
			if tt.wantErrSubstr != "" && !strings.Contains(res.stderr, tt.wantErrSubstr) {
				t.Errorf("stderr %q lacks %q", res.stderr, tt.wantErrSubstr)
			}
			if tt.wantCall == "" {
				if len(f.calls) != 0 {
					t.Errorf("calls = %v, want none", f.calls)
				}
				return
			}
			if len(f.calls) != 1 || f.calls[0] != tt.wantCall {
				t.Errorf("calls = %v, want [%s]", f.calls, tt.wantCall)
			}
			if !strings.Contains(res.stdout, filepath.Join(env.home, "logs", "daemon.log")) {
				t.Errorf("install output lacks the log file: %s", res.stdout)
			}
		})
	}
}

func TestServiceStatusAndActions(t *testing.T) {
	env := newTestEnv(t)
	f := useFakeService(t)
	f.st = service.Status{Installed: true, Running: true, PID: 7, Path: "/u/mycel.service"}
	res := newTestApp(t, "").run("service", "status", "--json")
	if res.code != 0 {
		t.Fatal(res)
	}
	var got serviceStatusJSON
	if err := json.Unmarshal([]byte(res.stdout), &got); err != nil {
		t.Fatal(err)
	}
	want := serviceStatusJSON{Installed: true, Running: true, PID: 7, Path: "/u/mycel.service", LogFile: filepath.Join(env.home, "logs", "daemon.log")}
	if got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	res = newTestApp(t, "").run("service", "status")
	if !strings.Contains(res.stdout, "running:   yes") || !strings.Contains(res.stdout, "pid:       7") {
		t.Errorf("status text: %s", res.stdout)
	}
	for _, c := range []string{"start", "stop", "uninstall"} {
		if res := newTestApp(t, "").run("service", c); res.code != 0 {
			t.Errorf("service %s: %+v", c, res)
		}
	}
	if strings.Join(f.calls, ",") != "start,stop,uninstall" {
		t.Errorf("calls = %v", f.calls)
	}
	f.failErr = service.ErrNotInstalled
	if res := newTestApp(t, "").run("service", "stop"); res.code != 1 || !strings.Contains(res.stderr, "not installed") {
		t.Errorf("stop when not installed: %+v", res)
	}
	if res := newTestApp(t, "").run("service", "uninstall"); res.code != 0 || !strings.Contains(res.stdout, "Not installed") {
		t.Errorf("uninstall when not installed: %+v", res)
	}
}

func TestServiceLogs(t *testing.T) {
	env := newTestEnv(t)
	res := newTestApp(t, "").run("service", "logs")
	if res.code != 1 || !strings.Contains(res.stderr, "no log yet") {
		t.Errorf("logs without a file: %+v", res)
	}
	writeFile(t, filepath.Join(env.home, "logs", "daemon.log"), "one\ntwo\nthree\n")
	res = newTestApp(t, "").run("service", "logs", "-n", "2")
	if res.code != 0 || res.stdout != "two\nthree\n" {
		t.Errorf("logs -n 2: %+v", res)
	}
}

func TestDaemonLogFile(t *testing.T) {
	env := newTestEnv(t)
	oldDefault, oldRedirect := slog.Default(), redirectStderr
	redirectStderr = func(string) error { return nil }
	t.Cleanup(func() { slog.SetDefault(oldDefault); redirectStderr = oldRedirect })
	initRoles(t, env.home, false, true)
	ta := newTestApp(t, "")
	ta.daemonRun = func(ctx context.Context, o daemon.Options, r chan<- daemon.Ready) error {
		o.Logger.Info("hello from the node")
		return errors.New("stop here")
	}
	logFile := filepath.Join(env.home, "logs", "daemon.log")
	res := ta.run("daemon", "--log-file", logFile)
	if res.code != 1 {
		t.Fatalf("daemon: %+v", res)
	}
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "hello from the node") || !strings.Contains(string(b), "role stopped") {
		t.Errorf("log file:\n%s", b)
	}
	if strings.Contains(res.stderr, "hello from the node") {
		t.Errorf("log also went to stderr: %s", res.stderr)
	}
}
