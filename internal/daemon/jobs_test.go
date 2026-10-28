//go:build fakesandbox

package daemon

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/sandbox/fake"
	"github.com/richard-sen27/mycelium/profiles"
)

// TestJobHelperProcess is the job command: the test binary re-executed
// inside the fake sandbox with "-- <mode> ...".
func TestJobHelperProcess(t *testing.T) {
	args := helperArgs()
	if args == nil {
		t.Skip("helper process only")
	}
	switch args[0] {
	case "print":
		fmt.Println("out-1")
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "err-1")
		time.Sleep(300 * time.Millisecond)
		fmt.Println("out-2")
		p := os.Getenv("HTTPS_PROXY")
		fmt.Printf("proxy-set=%v\n", strings.HasPrefix(p, "http://") && strings.Contains(p, "@127.0.0.1:"))
		fmt.Printf("job-env=%s\n", os.Getenv("JOB_VALUE"))
		in, err := os.ReadFile(filepath.Join("inputs", "data.txt"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "no input:", err)
			os.Exit(5)
		}
		if err := os.WriteFile("result.txt", //nolint:gosec // G703: a fixed name in the sandbox's cwd
			append([]byte("result:"), in...), 0o600); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	case "sleep":
		fmt.Println("sleeping")
		time.Sleep(time.Hour)
	case "beat":
		// args[1]: a host file rewritten with a counter every 100 ms.
		for i := 0; ; i++ {
			_ = os.WriteFile(args[1], []byte(strconv.Itoa(i)), 0o600)
			time.Sleep(100 * time.Millisecond)
		}
	case "env":
		for _, k := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OLLAMA_HOST"} {
			fmt.Printf("%s=%s\n", k, os.Getenv(k))
		}
		// The broker by name, through the egress proxy like any client.
		req, _ := http.NewRequest(http.MethodGet, os.Getenv("OLLAMA_HOST")+"/api/tags", nil) //nolint:noctx,gosec // G704: the helper calls the node broker by design
		req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
		res, err := hc.Do(req) //nolint:gosec // G704: the node's broker through the proxy
		if err != nil {
			fmt.Printf("tags-err=%v\n", err)
			os.Exit(9)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		fmt.Printf("tags-status=%d\ntags-body=%s\n", res.StatusCode, strings.ReplaceAll(string(body), "\n", " "))
		os.Exit(0)
	case "egress":
		// args[1]: a host outside the allowlist; args[2]: an allowlisted
		// loopback TLS server.
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
		_, err := hc.Get(args[1])
		fmt.Printf("blocked=%v\n", err)
		// http.ProxyFromEnvironment never proxies loopback targets, so the
		// proxy is taken from the environment explicitly.
		pu, perr := url.Parse(os.Getenv("HTTPS_PROXY"))
		if perr != nil {
			os.Exit(7)
		}
		// The status the proxy gives a CONNECT outside the allowlist.
		if c, err := net.Dial("tcp", pu.Host); err == nil { //nolint:gosec // G704: the node's egress proxy

			bu, _ := url.Parse(args[1])
			pw, _ := pu.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pw))
			_, _ = fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nProxy-Authorization: Basic %s\r\n\r\n", bu.Host, bu.Host, auth)
			if res, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
				fmt.Printf("connect-status=%d\n", res.StatusCode)
			}
			_ = c.Close()
		}
		hc2 := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			Proxy:           http.ProxyURL(pu),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
		}}
		res, err := hc2.Get(args[2])
		if err != nil {
			fmt.Printf("allowed-err=%v\n", err)
			os.Exit(8)
		}
		_ = res.Body.Close()
		fmt.Printf("allowed=%d\n", res.StatusCode)
		os.Exit(0)
	}
	os.Exit(2)
}

func jobTweak(c *runConfig) {
	c.jobLogFlushEvery = 50 * time.Millisecond
	c.jobDoneRetryEvery = 100 * time.Millisecond
	c.jobKillGrace = time.Second
	c.jobStatsEvery = 200 * time.Millisecond
	c.jobLogDrain = 5 * time.Second
}

func startJobs(t *testing.T, tweak func(*runConfig)) *localEnv {
	t.Helper()
	t.Setenv(fake.EnvOK, "1")
	return startLocal(t, func(c *runConfig) {
		jobTweak(c)
		if tweak != nil {
			tweak(c)
		}
	})
}

func fakeProfile(t *testing.T) *mycelv1.Profile {
	t.Helper()
	p, err := profiles.Get("fake")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func helperJob(t *testing.T, e *localEnv, id string, args ...string) *mycelv1.CreateSandbox {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := append([]string{exe, "-test.run=^TestJobHelperProcess$", "--"}, args...)
	return &mycelv1.CreateSandbox{
		Job: &mycelv1.Job{Id: id, Node: "n1", Spec: &mycelv1.JobSpec{
			Profile: "fake", Command: cmd, Outputs: []string{"result.txt"},
		}},
		Profile:      fakeProfile(t),
		InputBaseUrl: e.h.http.URL,
	}
}

func sendCreate(t *testing.T, e *localEnv, cs *mycelv1.CreateSandbox) *mycelv1.CtlResponse {
	t.Helper()
	return ctl(t, e.h.hubConn(), "n1", createCtl(cs))
}

func mustCreate(t *testing.T, e *localEnv, cs *mycelv1.CreateSandbox) {
	t.Helper()
	if r := sendCreate(t, e, cs); !r.GetOk() {
		t.Fatalf("CreateSandbox %s refused: %s", cs.GetJob().GetId(), r.GetError())
	}
}

func sendKill(t *testing.T, e *localEnv, id string) *mycelv1.CtlResponse {
	t.Helper()
	return ctl(t, e.h.hubConn(), "n1", &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_KillSandbox{KillSandbox: &mycelv1.KillSandbox{JobId: id, Reason: "test"}}})
}

func waitDone(t *testing.T, e *localEnv, id string, d time.Duration) *mycelv1.JobDoneRequest {
	t.Helper()
	var done *mycelv1.JobDoneRequest
	waitFor(t, d, "job.done for "+id, func() bool {
		done = e.fa.jobs.doneFor(id)
		return done != nil
	})
	return done
}

func waitStarted(t *testing.T, e *localEnv, id string) {
	t.Helper()
	waitFor(t, 10*time.Second, "job.started for "+id, func() bool { return e.fa.jobs.startedFor(id) != nil })
}

func readSummary(t *testing.T, e *localEnv, id string) jobSummary {
	t.Helper()
	var s jobSummary
	waitFor(t, 5*time.Second, "summary of "+id, func() bool {
		b, err := os.ReadFile(filepath.Join(e.dir, JobsDir, id, jobSummaryFile))
		return err == nil && json.Unmarshal(b, &s) == nil && s.Reported // written again once job.done is acknowledged
	})
	return s
}

func TestJobRunsAndReports(t *testing.T) {
	e := startJobs(t, nil)
	e.fa.jobs.set(func(f *fakeJobs) { f.dropSeq = 2; f.doneFails = 1 })
	e.h.arts.add("art_IN1", "data.txt", []byte("hello input"))
	cs := helperJob(t, e, "job_A1", "print")
	cs.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_IN1"}}
	cs.Job.Spec.Env = map[string]string{"JOB_VALUE": "v42"}
	mustCreate(t, e, cs)

	done := waitDone(t, e, "job_A1", 30*time.Second)
	if done.GetState() != mycelv1.JobState_JOB_STATE_SUCCEEDED || done.GetExitCode() != 0 {
		logs, _ := e.fa.jobs.logFor("job_A1")
		t.Fatalf("done %+v\nlogs:\n%s", done, logs)
	}
	st := e.fa.jobs.startedFor("job_A1")
	if st == nil || !strings.HasPrefix(st.GetSandboxId(), "sbx:") || st.GetBackend() != "fake" {
		t.Fatalf("started %+v", st)
	}
	logs, recv := e.fa.jobs.logFor("job_A1")
	i1, i2, i3 := strings.Index(logs, "out-1"), strings.Index(logs, "err-1"), strings.Index(logs, "out-2")
	if i1 < 0 || i2 < i1 || i3 < i2 {
		t.Fatalf("logs out of order:\n%s", logs)
	}
	if !strings.Contains(logs, "proxy-set=true") || !strings.Contains(logs, "job-env=v42") {
		t.Fatalf("job environment:\n%s", logs)
	}
	if len(recv) == 0 || recv[0] != 1 || countSeq(recv, 2) != 2 {
		t.Fatalf("seqs %v: want first 1 and seq 2 resent after the gap ack", recv)
	}
	for i := 1; i < len(recv); i++ {
		if recv[i] > recv[i-1]+1 {
			t.Fatalf("seq jump in %v", recv)
		}
	}
	if len(done.GetArtifacts()) != 1 {
		t.Fatalf("artifacts %v", done.GetArtifacts())
	}
	art := e.h.arts.byIDGet(done.GetArtifacts()[0])
	if art == nil || art.a.GetName() != "result.txt" || art.a.GetJobId() != "job_A1" || string(art.data) != "result:hello input" {
		t.Fatalf("artifact %+v", art)
	}
	stats := done.GetStats()
	if stats.GetStartedAt() == "" || stats.GetFinishedAt() == "" {
		t.Fatalf("stats %+v", stats)
	}
	e.fa.jobs.mu.Lock()
	attempts := e.fa.jobs.doneAttempt
	e.fa.jobs.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("job.done attempts %d, want 2 (one retry)", attempts)
	}

	sum := readSummary(t, e, "job_A1")
	if sum.State != "JOB_STATE_SUCCEEDED" || !sum.Reported || len(sum.Outputs) != 1 || sum.Outputs[0].Artifact != art.a.GetId() {
		t.Fatalf("summary %+v", sum)
	}
	jdir := filepath.Join(e.dir, JobsDir, "job_A1")
	for _, d := range []string{"in", "work"} {
		if _, err := os.Stat(filepath.Join(jdir, d)); !os.IsNotExist(err) {
			t.Fatalf("%s not removed: %v", d, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(jdir, "out", "result.txt")); err != nil || string(b) != "result:hello input" {
		t.Fatalf("out/result.txt: %q %v", b, err)
	}
	waitFor(t, 5*time.Second, "no running jobs", func() bool { return getStatus(t, e.r.addr).Jobs == 0 })
	if s := getStatus(t, e.r.addr); !containsString(s.SandboxBackendsAvailable, "fake") {
		t.Fatalf("backends %v", s.SandboxBackendsAvailable)
	}

	// A kill for the finished job is a no-op.
	if r := sendKill(t, e, "job_A1"); !r.GetOk() {
		t.Fatalf("kill after finish: %+v", r)
	}
	// The same job again: the hub retried after it finished.
	if r := sendCreate(t, e, helperJob(t, e, "job_A1", "print")); r.GetOk() || !strings.HasPrefix(r.GetError(), "failed_precondition: job finished") {
		t.Fatalf("retry after finish: %+v", r)
	}
}

func TestJobInputMissingFails(t *testing.T) {
	e := startJobs(t, nil)
	cs := helperJob(t, e, "job_IM", "print")
	cs.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_NOPE"}}
	mustCreate(t, e, cs)
	done := waitDone(t, e, "job_IM", 20*time.Second)
	if done.GetState() != mycelv1.JobState_JOB_STATE_FAILED || !strings.HasPrefix(done.GetError(), "input: ") {
		t.Fatalf("done %+v", done)
	}
	if e.fa.jobs.startedFor("job_IM") != nil {
		t.Fatal("job.started sent for a job whose inputs failed")
	}
}

func TestJobInputTooLarge(t *testing.T) {
	e := startJobs(t, func(c *runConfig) { c.jobArtifactMax = 4 })
	e.h.arts.add("art_BIG", "data.txt", []byte("more than four bytes"))
	cs := helperJob(t, e, "job_IL", "print")
	cs.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_BIG"}}
	mustCreate(t, e, cs)
	done := waitDone(t, e, "job_IL", 20*time.Second)
	if done.GetState() != mycelv1.JobState_JOB_STATE_FAILED || !strings.Contains(done.GetError(), "allowed") {
		t.Fatalf("done %+v", done)
	}
}

func TestJobStartedHubDownDoesNotDelay(t *testing.T) {
	e := startJobs(t, nil)
	e.fa.jobs.set(func(f *fakeJobs) { f.startedErr = "unavailable: hub busy" })
	e.h.arts.add("art_IN1", "data.txt", []byte("x"))
	cs := helperJob(t, e, "job_SD", "print")
	cs.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_IN1"}}
	mustCreate(t, e, cs)
	// The command runs and logs while job.started is still being retried.
	waitFor(t, 10*time.Second, "output before job.started", func() bool {
		l, _ := e.fa.jobs.logFor("job_SD")
		return strings.Contains(l, "out-1")
	})
	e.fa.jobs.set(func(f *fakeJobs) { f.startedErr = "" })
	done := waitDone(t, e, "job_SD", 30*time.Second)
	if done.GetState() != mycelv1.JobState_JOB_STATE_SUCCEEDED || e.fa.jobs.startedFor("job_SD") == nil {
		t.Fatalf("done %+v, started %v", done, e.fa.jobs.startedFor("job_SD"))
	}
}

func TestJobTimeout(t *testing.T) {
	e := startJobs(t, nil)
	cs := helperJob(t, e, "job_T1", "sleep")
	cs.Job.Spec.Timeout = "1s"
	mustCreate(t, e, cs)
	done := waitDone(t, e, "job_T1", 20*time.Second)
	if done.GetState() != mycelv1.JobState_JOB_STATE_TIMED_OUT {
		t.Fatalf("done %+v", done)
	}
}

func TestJobKill(t *testing.T) {
	e := startJobs(t, nil)
	mustCreate(t, e, helperJob(t, e, "job_K1", "sleep"))
	waitStarted(t, e, "job_K1")
	waitFor(t, 5*time.Second, "output", func() bool {
		l, _ := e.fa.jobs.logFor("job_K1")
		return strings.Contains(l, "sleeping")
	})
	start := time.Now()
	if r := sendKill(t, e, "job_K1"); !r.GetOk() {
		t.Fatalf("kill: %+v", r)
	}
	done := waitDone(t, e, "job_K1", 10*time.Second)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("cancel took %v", took)
	}
	if done.GetState() != mycelv1.JobState_JOB_STATE_CANCELLED || !strings.Contains(done.GetError(), "test") {
		t.Fatalf("done %+v", done)
	}
}

func TestJobConcurrency(t *testing.T) {
	e := startJobs(t, nil)
	mustCreate(t, e, helperJob(t, e, "job_C1", "sleep"))
	mustCreate(t, e, helperJob(t, e, "job_C2", "sleep"))
	// A retry of a running job is accepted without a second run.
	mustCreate(t, e, helperJob(t, e, "job_C1", "sleep"))
	r := sendCreate(t, e, helperJob(t, e, "job_C3", "sleep"))
	if r.GetOk() || !strings.HasPrefix(r.GetError(), "resource_exhausted") {
		t.Fatalf("third job: %+v", r)
	}
	waitStarted(t, e, "job_C1")
	waitStarted(t, e, "job_C2")
	if n := getStatus(t, e.r.addr).Jobs; n != 2 {
		t.Fatalf("status jobs %d", n)
	}
	sendKill(t, e, "job_C1")
	sendKill(t, e, "job_C2")
	waitDone(t, e, "job_C1", 10*time.Second)
	waitDone(t, e, "job_C2", 10*time.Second)
	waitFor(t, 5*time.Second, "slots free", func() bool { return getStatus(t, e.r.addr).Jobs == 0 })
	mustCreate(t, e, helperJob(t, e, "job_C3", "sleep"))
	waitStarted(t, e, "job_C3")
	sendKill(t, e, "job_C3")
	waitDone(t, e, "job_C3", 10*time.Second)
}

func TestJobProfileWithoutBackend(t *testing.T) {
	e := startJobs(t, nil)
	cs := helperJob(t, e, "job_P1", "print")
	cs.Profile = proto.Clone(cs.Profile).(*mycelv1.Profile)
	cs.Profile.Backends = []string{"firecracker"}
	if r := sendCreate(t, e, cs); r.GetOk() || !strings.HasPrefix(r.GetError(), "failed_precondition: no backend for profile fake") {
		t.Fatalf("%+v", r)
	}
	for _, env := range []map[string]string{{"MYCEL_TOKEN": "x"}, {"HTTP_PROXY": "http://evil:1"}} {
		cs := helperJob(t, e, "job_P2", "print")
		cs.Job.Spec.Env = env
		if r := sendCreate(t, e, cs); r.GetOk() || !strings.HasPrefix(r.GetError(), "invalid_argument: env") {
			t.Fatalf("env %v: %+v", env, r)
		}
	}
}

func TestJobEgress(t *testing.T) {
	e := startJobs(t, nil)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cs := helperJob(t, e, "job_E1", "egress", "https://blocked.example.net/", srv.URL+"/")
	cs.Profile = proto.Clone(cs.Profile).(*mycelv1.Profile)
	cs.Profile.Egress = &mycelv1.EgressPolicy{Allow: []string{u.Host}}
	cs.Job.Spec.Outputs = nil
	mustCreate(t, e, cs)
	done := waitDone(t, e, "job_E1", 30*time.Second)
	logs, _ := e.fa.jobs.logFor("job_E1")
	if done.GetState() != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("done %+v\n%s", done, logs)
	}
	if !strings.Contains(logs, "blocked=") || !strings.Contains(logs, "connect-status=403") || !strings.Contains(logs, "allowed=200") {
		t.Fatalf("logs:\n%s", logs)
	}
	if done.GetStats().GetEgressBytes() <= 0 {
		t.Fatalf("egress bytes %d", done.GetStats().GetEgressBytes())
	}
}

func TestJobShutdownCancels(t *testing.T) {
	e := startJobs(t, nil)
	mustCreate(t, e, helperJob(t, e, "job_S1", "sleep"))
	waitStarted(t, e, "job_S1")
	e.r.cancel()
	_ = e.r.wait(t, 30*time.Second)
	done := e.fa.jobs.doneFor("job_S1")
	if done == nil || done.GetState() != mycelv1.JobState_JOB_STATE_CANCELLED || !strings.Contains(done.GetError(), "shutting down") {
		t.Fatalf("done %+v", done)
	}
}

func TestJobHubSaysFinished(t *testing.T) {
	e := startJobs(t, nil)
	e.fa.jobs.set(func(f *fakeJobs) { f.startedErr = "failed_precondition: job finished (cancelled)" })
	mustCreate(t, e, helperJob(t, e, "job_H1", "sleep"))
	done := waitDone(t, e, "job_H1", 10*time.Second)
	if done.GetState() != mycelv1.JobState_JOB_STATE_CANCELLED {
		t.Fatalf("done %+v", done)
	}
}

func TestJobDirectoryCleanup(t *testing.T) {
	e := startJobs(t, func(c *runConfig) {
		c.jobsCap = 2
		c.jobKeep = time.Hour
	})
	jobs := filepath.Join(e.dir, JobsDir)
	for i, id := range []string{"job_OLD1", "job_OLD2"} {
		d := filepath.Join(jobs, id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, jobSummaryFile)
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Duration(10-i) * time.Minute)
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	e.h.arts.add("art_IN1", "data.txt", []byte("x"))
	cs := helperJob(t, e, "job_N1", "print")
	cs.Job.Spec.Inputs = []*mycelv1.JobInput{{Artifact: "art_IN1"}}
	mustCreate(t, e, cs)
	waitDone(t, e, "job_N1", 30*time.Second)
	readSummary(t, e, "job_N1")
	ents, err := os.ReadDir(jobs)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	if len(names) != 2 || containsString(names, "job_OLD1") || !containsString(names, "job_N1") {
		t.Fatalf("jobs dir %v", names)
	}

	// Expiry: the janitor removes finished directories after jobKeep.
	e.r.cancel()
	_ = e.r.wait(t, 30*time.Second)
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	jobTweak(&cfg)
	cfg.jobKeep = 200 * time.Millisecond
	cfg.jobJanitorEvery = 50 * time.Millisecond
	// A directory without a summary is a leftover and goes at startup.
	if err := os.MkdirAll(filepath.Join(jobs, "job_STALE", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	startRunWith(t, e.dir, cfg, p, sl)
	waitFor(t, 5*time.Second, "jobs directory emptied", func() bool {
		ents, _ := os.ReadDir(jobs)
		return len(ents) == 0
	})
}

// ---- fake hub accessors used by the job tests ----

func (f *fakeJobs) set(fn func(*fakeJobs)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func (f *fakeJobs) doneFor(id string) *mycelv1.JobDoneRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.done[id]; len(d) > 0 {
		return d[0]
	}
	return nil
}

func (f *fakeJobs) startedFor(id string) *mycelv1.JobStartedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[id]
}

func (f *fakeJobs) logFor(id string) (string, []uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.logs[id]
	if l == nil {
		return "", nil
	}
	return string(l.data), append([]uint64(nil), l.recv...)
}

// Heartbeats report the backends that can run, not what is installed: the
// fake backend appears only while MYCEL_FAKE_SANDBOX_OK=1.
func TestHeartbeatReportsAvailableBackends(t *testing.T) {
	t.Setenv(fake.EnvOK, "")
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	sub, err := h.hubConn().SubscribeSync(bus.NodeTelemetry("n1"))
	if err != nil {
		t.Fatal(err)
	}
	p := newFakeProbe()
	installed := proto.Clone(p.inv.Load()).(*mycelv1.Inventory)
	installed.SandboxBackends = []string{"fake", "firecracker"} // what the probe found installed
	p.inv.Store(installed)
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	cfg.backendsRefreshEvery = 50 * time.Millisecond
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)

	// waitBeat waits for a full-inventory beat whose backends satisfy want.
	waitBeat := func(what string, want func([]string) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			hb := nextBeat(t, sub)
			if inv := hb.GetInventory(); inv != nil && want(inv.GetSandboxBackends()) {
				if hb.GetInventoryHash() == "" {
					t.Fatal("beat without inventory hash")
				}
				return
			}
		}
		t.Fatalf("no heartbeat with %s", what)
	}
	without := func(b []string) bool { return !containsString(b, "fake") && !containsString(b, "firecracker") }
	waitBeat("no fake backend", without)

	t.Setenv(fake.EnvOK, "1")
	waitBeat("the fake backend", func(b []string) bool { return containsString(b, "fake") && !containsString(b, "firecracker") })
	if s := getStatus(t, r.addr); !containsString(s.SandboxBackendsAvailable, "fake") {
		t.Fatalf("status backends %v", s.SandboxBackendsAvailable)
	}

	t.Setenv(fake.EnvOK, "0")
	waitBeat("the fake backend gone again", without)
}
