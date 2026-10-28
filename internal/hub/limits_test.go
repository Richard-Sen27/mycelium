package hub

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/identity"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), dbFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestStoreEnrolRaceOneWinner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	tok := &tokenRow{ID: "t1", SecretHash: identity.HashSecret([]byte("s")), NodeName: "n", Caps: nil, OwnerID: "usr_x",
		CreatedAt: formatTime(now), ExpiresAt: formatTime(now.Add(time.Hour))}
	if err := st.createJoinToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("node-%d", i)
			errs[i] = st.enrolNode(ctx, "t1", &nodeRow{
				Name: name, SPIFFEID: "spiffe://td/node/" + name, OwnerID: "usr_x",
				CertSerial: fmt.Sprintf("%x", i+1), CertNotAfter: formatTime(now.Add(time.Hour)), CreatedAt: formatTime(now),
			})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, errTokenSpent):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d enrolments succeeded, want 1", ok)
	}
	nodes, _ := st.listNodes(ctx)
	if len(nodes) != 1 {
		t.Fatalf("%d nodes stored", len(nodes))
	}
}

func TestStoreAgentCap(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	reg := func(name string) error {
		_, _, err := st.upsertAgent(ctx, &agentRow{ID: "agent:" + name + "@n", Name: name, NodeName: "n", OwnerID: "usr_x",
			State: "running", CreatedAt: nowString()})
		return err
	}
	for i := range maxAgentsPerNode {
		if err := reg(fmt.Sprintf("a%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg("one-more"); !errors.Is(err, errTooManyAgents) {
		t.Fatalf("agent %d: %v", maxAgentsPerNode+1, err)
	}
	// Re-registering an existing agent is not a new agent.
	if err := reg("a0"); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	// Revoked agents do not count.
	if err := st.revokeAgent(ctx, "agent:a1@n"); err != nil {
		t.Fatal(err)
	}
	if !st.agentRevoked("agent:a1@n") {
		t.Fatal("revoked agent not in memory set")
	}
	if err := reg("one-more"); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
	if err := reg("a1"); !errors.Is(err, errTokenSpent) {
		t.Fatalf("revoked agent re-registered: %v", err)
	}
	// Stopped agents do not count either, but starting one again does.
	if err := st.setAgentExited(ctx, "agent:a2@n", 0); err != nil {
		t.Fatal(err)
	}
	if err := reg("two-more"); err != nil {
		t.Fatalf("after exit: %v", err)
	}
	if err := reg("a2"); !errors.Is(err, errTooManyAgents) {
		t.Fatalf("stopped agent restarted past the cap: %v", err)
	}
	// The revoked set survives a reopen.
	st2, err := openStore(st.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	if !st2.agentRevoked("agent:a1@n") {
		t.Fatal("revoked agent not loaded")
	}
}

func TestStorePrunesExpiredRevocations(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	past, future := formatTime(time.Now().Add(-time.Hour)), formatTime(time.Now().Add(time.Hour))
	for serial, na := range map[string]string{"aa": past, "bb": future} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO revoked_certs(serial, spiffe_id, revoked_at, not_after) VALUES(?,?,?,?)`,
			serial, "spiffe://td/node/x", nowString(), na); err != nil {
			t.Fatal(err)
		}
	}
	st2, err := openStore(st.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	if st2.IsRevoked("aa", "") || !st2.IsRevoked("bb", "") {
		t.Fatal("expired serial not pruned or live one lost")
	}
	var n int
	_ = st2.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revoked_certs`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d revoked rows", n)
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter(10, 10)
	l.now = func() time.Time { return now }
	for i := range 10 {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d refused", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("11th request allowed")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("other IP refused")
	}
	now = now.Add(6 * time.Second) // one token per 6 s
	if !l.allow("1.2.3.4") || l.allow("1.2.3.4") {
		t.Fatal("refill wrong")
	}
	now = now.Add(2 * time.Minute)
	l.allow("9.9.9.9")
	if _, ok := l.buckets["5.6.7.8"]; ok {
		t.Fatal("idle bucket not pruned")
	}
}

func TestEnrolRateLimited(t *testing.T) {
	h := startTestHub(t)
	anon := h.clients("", nil)
	limited := false
	for i := range enrolBurst + 2 {
		_, err := tryEnrol(anon, "mcl1_garbage", "x")
		switch connect.CodeOf(err) {
		case connect.CodeResourceExhausted:
			if i < enrolBurst {
				t.Fatalf("limited after %d requests", i)
			}
			limited = true
		case connect.CodePermissionDenied:
		default:
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if !limited {
		t.Fatal("never rate limited")
	}
}

func TestEnrolConcurrentOneToken(t *testing.T) {
	h := startTestHub(t)
	admin := h.clients(h.init.AdminAPIKey, nil)
	anon := h.clients("", nil)
	tok := createToken(t, admin, "racer", nil, "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = tryEnrol(anon, tok, "racer")
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 2 concurrent enrolments succeeded", ok)
	}
}

func TestEnrolBadCSRDoesNotRevealToken(t *testing.T) {
	h := startTestHub(t)
	admin := h.clients(h.init.AdminAPIKey, nil)
	anon := h.clients("", nil)
	tok := createToken(t, admin, "csr-node", nil, "")
	for _, csr := range [][]byte{nil, []byte("garbage"), make([]byte, 5000)} {
		_, err := anon.enrol.Enrol(context.Background(), connect.NewRequest(&mycelv1.EnrolRequest{Token: tok, CsrDer: csr}))
		wantCode(t, err, connect.CodePermissionDenied)
		if err.Error() != invalidToken().Error() {
			t.Fatalf("message differs: %v", err)
		}
	}
	// The token was not burned by the bad attempts.
	enrol(t, anon, tok, "csr-node")
}

func TestRunReadyWithoutReceiver(t *testing.T) {
	opts, _ := initTestHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{HubDir: opts.HubDir, APIListen: "127.0.0.1:0", BusListen: "127.0.0.1:0", Logger: quietLogger()}, make(chan Ready))
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run blocked on an unread ready channel")
	}
}
