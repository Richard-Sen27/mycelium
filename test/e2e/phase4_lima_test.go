//go:build e2e

package e2e

import (
	"os"
	"os/user"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Phase 4 isolation on the real macOS backend (applevf: Lima with
// Virtualization.framework). Opt-in, macOS only:
//
//	MYCEL_TEST_LIMA=1 go test -tags e2e -count=1 -timeout 40m -run TestJobIsolationLima ./test/e2e/
//
// The hub's own node runs the jobs without MYCEL_FAKE_SANDBOX_OK, so the
// daemon picks applevf for the default and dev profiles. Each job boots a
// VM (about 1-2 minutes with the Debian image in Lima's cache).

// limaJobWait bounds one VM job from submission to its end.
const limaJobWait = 15 * time.Minute

// limaRoot is a short hub root: Lima's instance sockets live under
// <MYCEL_HOME>/node/lima and must fit the 104-byte unix socket path limit,
// which t.TempDir() paths on macOS exceed.
func limaRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("keeping %s for inspection", dir)
			return
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

// sections splits output written as "==name" marker lines followed by the
// section's lines.
func sections(out string) map[string]string {
	m := map[string]string{}
	cur := ""
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if name, ok := strings.CutPrefix(line, "=="); ok {
			cur = strings.TrimSpace(name)
			m[cur] = ""
			continue
		}
		if cur != "" {
			m[cur] += line + "\n"
		}
	}
	for k, v := range m {
		m[k] = strings.TrimSpace(v)
	}
	return m
}

func TestJobIsolationLima(t *testing.T) {
	if os.Getenv("MYCEL_TEST_LIMA") != "1" || runtime.GOOS != "darwin" {
		t.Skip("set MYCEL_TEST_LIMA=1 on macOS to run the applevf (Lima) isolation suite")
	}
	h := startHubWith(t, hubOptions{Root: limaRoot})
	hubNode := &Node{t: t, Name: "hub-node", Home: h.Home, LocalAddr: h.LocalAddr, LocalURL: "http://" + h.LocalAddr}
	if !waitFor(t, 60*time.Second, func() bool { return slices.Contains(availableBackends(t, hubNode), "applevf") }) {
		t.Fatalf("hub-node does not have applevf available: %v", hubNode.Status(t))
	}
	waitBackends(t, h, "applevf", hubNode) // and the hub knows it
	localFirstOff(t, h, hubNode)

	// default: workspace only, no egress, no host files, own kernel.
	start := time.Now()
	j := runJob(t, h, "", "--profile", "default", "--node", "hub-node", "--timeout", "10m", "--",
		"sh", "-c", "echo ==users; ls -A /Users 2>&1; echo ==host; ls -A /host 2>&1; "+
			"echo ==pid1; ps -p 1 -o comm= 2>/dev/null || cat /proc/1/comm; echo ==uid; id -u; echo ==os; head -1 /etc/os-release")
	j = waitFinished(t, h, j.ID, limaJobWait)
	logs := jobLogs(t, h, j.ID)
	t.Logf("default job %s: %s in %s\n%s", j.ID, j.State, time.Since(start).Round(time.Second), logs)
	if j.State != jobSucceeded || j.Backend != "applevf" {
		t.Fatalf("default job: %+v; want %s on applevf", j, jobSucceeded)
	}
	s := sections(logs)
	if u := s["users"]; u != "" && !strings.Contains(u, "No such file") && !strings.Contains(u, "cannot access") {
		t.Errorf("/Users in the sandbox lists %q; want it empty or missing", u)
	}
	if me, err := user.Current(); err == nil && me.Username != "" && strings.Contains(s["users"], me.Username) {
		t.Errorf("/Users in the sandbox shows the host user %s", me.Username)
	}
	if hst := s["host"]; !strings.Contains(hst, "No such file") && !strings.Contains(hst, "cannot access") {
		t.Errorf("/host in the sandbox: %q; want it missing", hst)
	}
	if p := s["pid1"]; p == "" || strings.Contains(p, "launchd") {
		t.Errorf("PID 1 in the sandbox is %q; want a Linux init, not launchd", p)
	}
	if uid := s["uid"]; uid == "" || uid == "0" {
		t.Errorf("uid in the sandbox is %q; want a non-root uid", uid)
	}
	if osl := s["os"]; !strings.Contains(osl, "Debian") {
		t.Errorf("os-release in the sandbox starts %q; want Debian", osl)
	}

	// dev: egress through the proxy's allowlist only.
	start = time.Now()
	curl := "curl -sS -o /dev/null --max-time 60 -w 'code=%{http_code} connect=%{http_connect}\\n' "
	j = runJob(t, h, "", "--profile", "dev", "--node", "hub-node", "--timeout", "10m", "--",
		"sh", "-c", "echo ==allowed; "+curl+"https://github.com 2>&1; echo ==denied; "+curl+"https://example.org 2>&1; true")
	j = waitFinished(t, h, j.ID, limaJobWait)
	logs = jobLogs(t, h, j.ID)
	t.Logf("dev job %s: %s in %s\n%s", j.ID, j.State, time.Since(start).Round(time.Second), logs)
	if j.State != jobSucceeded || j.Backend != "applevf" {
		t.Fatalf("dev job: %+v; want %s on applevf", j, jobSucceeded)
	}
	s = sections(logs)
	if !strings.Contains(s["allowed"], "code=200") {
		t.Errorf("curl https://github.com in dev: %q; want 200", s["allowed"])
	}
	if d := s["denied"]; !strings.Contains(d, "connect=403") && !strings.Contains(d, "code=403") {
		t.Errorf("curl https://example.org in dev: %q; want 403 from the proxy", d)
	}
}
