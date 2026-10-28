package daemon

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// fakeClock is time.Now moved by an offset.
type fakeClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

func sharingCtl(s *mycelv1.SharingSettings) *mycelv1.CtlRequest {
	return &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Sharing{Sharing: s}}
}

func TestPutSharingTightenOnlyAgainstHubMaxJobs(t *testing.T) {
	e, _ := startGrants(t, nil)
	hub := &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"sandbox", "ollama"}}, When: "always", MaxJobs: 1, LocalFirst: true}
	if r := ctl(t, e.h.hubConn(), "n1", sharingCtl(hub)); !r.GetOk() {
		t.Fatalf("sharing ctl: %+v", r)
	}
	two := &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"sandbox"}}, When: "always", MaxJobs: 2, LocalFirst: true, IdleAfterMinutes: 10}
	if code, b := e.c.putSharing(two); code != http.StatusBadRequest {
		t.Fatalf("max_jobs 2 over the hub's 1: %d %s", code, b)
	}
	two.MaxJobs = 1
	if code, b := e.c.putSharing(two); code != http.StatusOK {
		t.Fatalf("max_jobs 1: %d %s", code, b)
	}
}

func TestGrantKindChangeRefused(t *testing.T) {
	e, _ := startGrants(t, nil)
	coder := e.c.mustRegister("coder")
	mustAttach(t, e, ollamaGrant("grt_KIND1", coder.AgentID, time.Hour))
	g := ollamaGrant("grt_KIND1", coder.AgentID, time.Hour)
	g.Spec = &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"pypi.org"}}}}
	if r := attachCtl(t, e, g); r.GetOk() || !strings.HasPrefix(r.GetError(), "invalid_argument") {
		t.Fatalf("kind change: %+v", r)
	}
}

func TestAttachRevokedWhileAttaching(t *testing.T) {
	var c atomic.Pointer[localClient]
	var once sync.Once
	revoked := make(chan int, 1)
	e, _ := startGrants(t, func(cfg *runConfig) {
		cfg.grantAttachHook = func(id string) {
			if id != "grt_RACE1" {
				return
			}
			once.Do(func() { code, _ := c.Load().do(http.MethodDelete, "/api/local/grants/"+id, ""); revoked <- code })
		}
	})
	c.Store(e.c)
	coder := e.c.mustRegister("coder")
	r := attachCtl(t, e, ollamaGrant("grt_RACE1", coder.AgentID, time.Hour))
	if revokeCode := <-revoked; revokeCode != http.StatusNoContent {
		t.Fatalf("DELETE during attach: %d", revokeCode)
	}
	if r.GetOk() || !strings.Contains(r.GetError(), "revoked on this node") {
		t.Fatalf("attach: %+v", r)
	}
	if ids := e.c.grants("/api/local/grants").ids(t); len(ids) != 0 {
		t.Fatalf("grant recorded after the revoke: %v", ids)
	}
	waitFor(t, 5*time.Second, "grant.revoked", func() bool {
		rv := e.fa.p5.revokedFor("grt_RACE1")
		return rv != nil && rv.GetReason() == reasonLocal
	})
}

func TestRestoreOutsideScheduleKeepsGrants(t *testing.T) {
	up := newFakeOllama(t)
	h := newFakeHub(t)
	fa := newFakeAgents(h)
	dir := joinNode(t, h, "n1")
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	grantTweak(up.URL)(&cfg)
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	e := &localEnv{h: h, fa: fa, dir: dir, r: r, c: newLocalClient(t, r, dir)}

	// Sharing only on a day two days from now: inactive today.
	day := strings.ToLower(time.Now().Add(48 * time.Hour).Weekday().String()[:3])
	hub := &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"sandbox", "ollama"}}, When: "schedule",
		Schedule: []*mycelv1.ScheduleWindow{{Days: []string{day}, From: "00:00", To: "01:00"}}, MaxJobs: 2, LocalFirst: true}
	if resp := ctl(t, h.hubConn(), "n1", sharingCtl(hub)); !resp.GetOk() {
		t.Fatalf("sharing ctl: %+v", resp)
	}
	if getStatus(t, r.addr).SharingActive {
		t.Fatal("sharing active outside the window")
	}
	// Offered, so attached; requests are refused until the window opens.
	mustAttach(t, e, ollamaGrant("grt_SCH1", "job:job_S1", time.Hour))

	r.cancel()
	_ = r.wait(t, 10*time.Second)
	r2 := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r2.addr)
	c2 := newLocalClient(t, r2, dir)
	if ids := c2.grants("/api/local/grants").ids(t); len(ids) != 1 {
		t.Fatalf("after restart outside the window: %v", ids)
	}
	time.Sleep(300 * time.Millisecond)
	if fa.p5.revokedFor("grt_SCH1") != nil {
		t.Fatal("restore revoked a grant for inactivity")
	}
}

func TestGrantExpiryIsSleepSafe(t *testing.T) {
	clk := &fakeClock{}
	e, _ := startGrants(t, func(c *runConfig) { c.now = clk.now })
	coder := e.c.mustRegister("coder")
	mustAttach(t, e, ollamaGrant("grt_SLP1", coder.AgentID, time.Hour))
	// The machine "slept" past the expiry: the timer has not fired, the
	// grants loop catches it.
	clk.advance(2 * time.Hour)
	waitFor(t, 5*time.Second, "expired after sleep", func() bool { return len(e.c.grants("/api/local/grants").ids(t)) == 0 })
	if e.fa.p5.revokedFor("grt_SLP1") != nil {
		t.Fatal("expiry reported as a revocation")
	}
}

func TestReattachFailureReported(t *testing.T) {
	up := newFakeOllama(t)
	h := newFakeHub(t)
	fa := newFakeAgents(h)
	dir := joinNode(t, h, "n1")
	p := newFakeProbe()
	sl := &stateLog{}
	cfg := testRunConfig(p, sl)
	grantTweak(up.URL)(&cfg)
	r := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r.addr)
	e := &localEnv{h: h, fa: fa, dir: dir, r: r, c: newLocalClient(t, r, dir)}
	mustAttach(t, e, ollamaGrant("grt_RA1", "job:job_RA", time.Hour))
	r.cancel()
	_ = r.wait(t, 10*time.Second)

	// The model endpoint is gone at restart: the grant cannot come back.
	up.Close()
	r2 := startRunWith(t, dir, cfg, p, sl)
	waitConnected(t, r2.addr)
	if ids := newLocalClient(t, r2, dir).grants("/api/local/grants").ids(t); len(ids) != 0 {
		t.Fatalf("grants %v", ids)
	}
	waitFor(t, 5*time.Second, "grant.revoked for the lost grant", func() bool {
		rv := fa.p5.revokedFor("grt_RA1")
		return rv != nil && strings.HasPrefix(rv.GetReason(), reasonRestart)
	})
}
