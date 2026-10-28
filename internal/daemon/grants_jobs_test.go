//go:build fakesandbox

package daemon

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/sharing"
)

func startGrantJobs(t *testing.T, tweak func(*runConfig)) *localEnv {
	t.Helper()
	up := newFakeOllama(t)
	return startJobs(t, func(c *runConfig) {
		grantTweak(up.URL)(c)
		if tweak != nil {
			tweak(c)
		}
	})
}

func (f *fakeIdle) set(d time.Duration) {
	f.mu.Lock()
	f.idle = d
	f.mu.Unlock()
}

func readBeat(path string) string {
	b, _ := os.ReadFile(path) //nolint:gosec // test file
	return string(b)
}

// beating reports whether the heartbeat file changes within 500 ms.
func beating(path string) bool {
	a := readBeat(path)
	time.Sleep(500 * time.Millisecond)
	return readBeat(path) != a
}

func waitBeating(t *testing.T, path string, want bool, d time.Duration, what string) {
	t.Helper()
	waitFor(t, d, what, func() bool { return beating(path) == want })
}

func freezeCtl(scope string) *mycelv1.CtlRequest {
	return &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Freeze{Freeze: &mycelv1.Freeze{Scope: scope, Reason: "test freeze"}}}
}

func unfreezeCtl() *mycelv1.CtlRequest {
	return &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Unfreeze{Unfreeze: &mycelv1.Unfreeze{Scope: "all"}}}
}

func TestJobGetsOllamaGrantEnv(t *testing.T) {
	e := startGrantJobs(t, nil)
	mustAttach(t, e, ollamaGrant("grt_JOB1", "job:job_G1", time.Hour))
	cs := helperJob(t, e, "job_G1", "env")
	cs.Job.Spec.Outputs = nil
	mustCreate(t, e, cs)
	done := waitDone(t, e, "job_G1", 30*time.Second)
	logs, _ := e.fa.jobs.logFor("job_G1")
	if done.GetState() != mycelv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("done %+v\n%s", done, logs)
	}
	vals := map[string]string{}
	for _, line := range strings.Split(logs, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			vals[k] = v
		}
	}
	key := vals["OPENAI_API_KEY"]
	if key == "" || vals["OPENAI_BASE_URL"] != "http://ollama.mycel.internal/v1" || vals["OLLAMA_HOST"] != "http://ollama.mycel.internal" {
		t.Fatalf("job environment %v", vals)
	}
	// End to end: the job reached the broker by name through the egress
	// proxy alias, with its key, and got the fake upstream's tags.
	if vals["tags-status"] != "200" || !strings.Contains(vals["tags-body"], "llama3") {
		t.Fatalf("broker through the proxy: %v\n%s", vals, logs)
	}
	// The key is the grant's credential at the broker.
	ep := e.c.grants("/api/local/grants").Endpoints["grt_JOB1"]
	u, err := url.Parse(ep)
	if err != nil {
		t.Fatal(err)
	}
	if code := brokerGet(t, "http://grant:"+key+"@"+u.Host, "/api/tags"); code != http.StatusOK {
		t.Fatalf("job key refused by the broker: %d", code)
	}
}

func TestUnknownIdleNeitherIdleNorActive(t *testing.T) {
	fi := &fakeIdle{err: sharing.ErrUnknownIdle}
	e := startGrantJobs(t, func(c *runConfig) {
		c.idleDetector = fi
		c.idleSampleEvery = 100 * time.Millisecond
	})
	if s := getStatus(t, e.r.addr); s.Idle != "unknown" || !s.SharingActive {
		t.Fatalf("status %+v", s)
	}
	code, b := e.c.do(http.MethodGet, "/api/local/sharing", "")
	if code != http.StatusOK || !strings.Contains(string(b), `"idle":"unknown"`) {
		t.Fatalf("GET sharing: %d %s", code, b)
	}
	// Default settings (local_first on): the job runs, nothing pauses it.
	beat := filepath.Join(t.TempDir(), "beat")
	cs := helperJob(t, e, "job_U1", "beat", beat)
	cs.Job.Spec.Outputs = nil
	mustCreate(t, e, cs)
	waitBeating(t, beat, true, 10*time.Second, "job heartbeat")
	time.Sleep(300 * time.Millisecond) // a few idle samples
	if !beating(beat) {
		t.Fatal("unknown idle state paused the job")
	}
	sendKill(t, e, "job_U1")
	waitDone(t, e, "job_U1", 10*time.Second)

	// when=idle with an unknown idle state is inactive: admission refuses.
	idle := proto.Clone(e.c.sharing().GetEffective()).(*mycelv1.SharingSettings)
	idle.When = "idle"
	if code, b := e.c.putSharing(idle); code != http.StatusOK {
		t.Fatalf("PUT idle: %d %s", code, b)
	}
	if r := sendCreate(t, e, helperJob(t, e, "job_U2", "sleep")); r.GetOk() || r.GetError() != "failed_precondition: sharing inactive" {
		t.Fatalf("create with unknown idle: %+v", r)
	}
}

func TestFailedPauseIsRetried(t *testing.T) {
	var mu sync.Mutex
	fails := 2
	e := startGrantJobs(t, func(c *runConfig) {
		c.idleSampleEvery = 100 * time.Millisecond
		c.pauseHook = func(string, bool) error {
			mu.Lock()
			defer mu.Unlock()
			if fails > 0 {
				fails--
				return errors.New("injected pause failure")
			}
			return nil
		}
	})
	beat := filepath.Join(t.TempDir(), "beat")
	cs := helperJob(t, e, "job_P1", "beat", beat)
	cs.Job.Spec.Outputs = nil
	mustCreate(t, e, cs)
	waitBeating(t, beat, true, 10*time.Second, "job heartbeat")
	if r := ctl(t, e.h.hubConn(), "n1", freezeCtl("all")); !r.GetOk() {
		t.Fatalf("freeze: %+v", r)
	}
	waitBeating(t, beat, false, 3*time.Second, "job paused after retries")
	mu.Lock()
	left := fails
	mu.Unlock()
	if left != 0 {
		t.Fatalf("failures left %d", left)
	}
	if r := ctl(t, e.h.hubConn(), "n1", unfreezeCtl()); !r.GetOk() {
		t.Fatalf("unfreeze: %+v", r)
	}
	waitBeating(t, beat, true, 5*time.Second, "job resumed")
	sendKill(t, e, "job_P1")
	waitDone(t, e, "job_P1", 10*time.Second)
}

func TestJobRefusedWhenSandboxNotShared(t *testing.T) {
	e := startGrantJobs(t, nil)
	if code, b := e.c.putSharing(e.c.withoutOffer("sandbox")); code != http.StatusOK {
		t.Fatalf("PUT sharing: %d %s", code, b)
	}
	r := sendCreate(t, e, helperJob(t, e, "job_NS1", "sleep"))
	if r.GetOk() || r.GetError() != "failed_precondition: sharing inactive" {
		t.Fatalf("create: %+v", r)
	}
}

func TestFreezePausesJobsAndBrokers(t *testing.T) {
	e := startGrantJobs(t, nil)
	coder := e.c.mustRegister("coder")
	mustAttach(t, e, ollamaGrant("grt_FRZ1", coder.AgentID, time.Hour))
	ep := e.c.grants("/api/local/agents/" + coder.Session + "/grants").Endpoints["grt_FRZ1"]

	beat := filepath.Join(t.TempDir(), "beat")
	cs := helperJob(t, e, "job_F1", "beat", beat)
	cs.Job.Spec.Outputs = nil
	mustCreate(t, e, cs)
	waitBeating(t, beat, true, 10*time.Second, "job heartbeat")

	start := time.Now()
	if r := ctl(t, e.h.hubConn(), "n1", freezeCtl("all")); !r.GetOk() {
		t.Fatalf("freeze: %+v", r)
	}
	waitBeating(t, beat, false, 2*time.Second, "job paused by the freeze")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("freeze took %v", d)
	}
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusServiceUnavailable {
		t.Fatalf("frozen broker answered %d", code)
	}
	if r := sendCreate(t, e, helperJob(t, e, "job_F2", "sleep")); r.GetOk() || r.GetError() != "failed_precondition: node frozen" {
		t.Fatalf("create while frozen: %+v", r)
	}
	if r := attachCtl(t, e, ollamaGrant("grt_FRZ2", coder.AgentID, time.Hour)); r.GetOk() {
		t.Fatal("attach while frozen accepted")
	}
	// The hub resending an attached grant is still acknowledged.
	g := e.c.grants("/api/local/grants")
	resend := &mycelv1.Grant{}
	if err := protojson.Unmarshal(g.Grants[0], resend); err != nil {
		t.Fatal(err)
	}
	if r := attachCtl(t, e, resend); !r.GetOk() {
		t.Fatalf("resend while frozen: %+v", r)
	}
	if code, _ := e.c.spawn(coder.Session, spawnReq("test-echo", nil)); code != http.StatusLocked {
		t.Fatalf("spawn while frozen: %d", code)
	}
	if s := getStatus(t, e.r.addr); !s.Frozen {
		t.Fatalf("status %+v", s)
	}
	// Still paused a little later.
	if beating(beat) {
		t.Fatal("job runs while frozen")
	}

	if r := ctl(t, e.h.hubConn(), "n1", unfreezeCtl()); !r.GetOk() {
		t.Fatalf("unfreeze: %+v", r)
	}
	waitBeating(t, beat, true, 5*time.Second, "job resumed")
	if code := brokerGet(t, ep, "/api/tags"); code != http.StatusOK {
		t.Fatalf("broker after unfreeze: %d", code)
	}
	sendKill(t, e, "job_F1")
	waitDone(t, e, "job_F1", 10*time.Second)
}

func TestLocalFirstPausesJobWhileOwnerActive(t *testing.T) {
	fi := &fakeIdle{idle: time.Hour}
	e := startGrantJobs(t, func(c *runConfig) {
		c.idleDetector = fi
		c.idleSampleEvery = 100 * time.Millisecond
	})
	if !e.c.sharing().GetEffective().GetLocalFirst() {
		t.Fatal("local_first is not the default")
	}
	beat := filepath.Join(t.TempDir(), "beat")
	cs := helperJob(t, e, "job_L1", "beat", beat)
	cs.Job.Spec.Outputs = nil
	mustCreate(t, e, cs)
	waitBeating(t, beat, true, 10*time.Second, "job heartbeat")

	fi.set(10 * time.Second) // the owner is at the machine
	waitBeating(t, beat, false, 3*time.Second, "job paused while the owner is active")
	fi.set(time.Hour)
	waitBeating(t, beat, true, 3*time.Second, "job resumed when idle")

	// A freeze and local_first overlap: unfreezing keeps the job paused
	// while the owner is active.
	fi.set(0)
	waitBeating(t, beat, false, 3*time.Second, "paused again")
	if r := ctl(t, e.h.hubConn(), "n1", freezeCtl("all")); !r.GetOk() {
		t.Fatalf("freeze: %+v", r)
	}
	if r := ctl(t, e.h.hubConn(), "n1", unfreezeCtl()); !r.GetOk() {
		t.Fatalf("unfreeze: %+v", r)
	}
	if beating(beat) {
		t.Fatal("unfreeze resumed a job local_first holds")
	}
	fi.set(time.Hour)
	waitBeating(t, beat, true, 3*time.Second, "resumed at last")
	sendKill(t, e, "job_L1")
	waitDone(t, e, "job_L1", 10*time.Second)
}
