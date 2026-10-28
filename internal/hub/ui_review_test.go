package hub

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	webpush "github.com/SherClockHolmes/webpush-go"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
)

// captureServer starts a grant environment and returns its running server.
func captureServer(t *testing.T) (*grantEnv, *server) {
	t.Helper()
	var srv *server
	setVar(t, &testHookServer, func(s *server) { srv = s })
	e := newGrantEnv(t)
	return e, srv
}

func TestOpeningHeartbeatSeq(t *testing.T) {
	var srv *server
	var once sync.Once
	setVar(t, &testHookServer, func(s *server) { srv = s })
	// An event appended after the stream fixed its cursor but before the
	// opening heartbeat went out must still reach the client.
	setVar(t, &testHookEventsStarted, func() {
		once.Do(func() {
			srv.events.append([]ringEntry{{ev: &mycelv1.Event{Kind: kindNode, Op: opUpdated, Id: "between"}}}, false)
		})
	})
	e := newGrantEnv(t)
	s := watch(t, e.events(keyHeaders(e.bobKey), nil), &mycelv1.WatchEventsRequest{})
	ev := s.wait(t, "event appended before the heartbeat", func(ev *mycelv1.Event) bool { return ev.GetId() == "between" })
	s.mu.Lock()
	open, bad := s.first, s.bad
	s.mu.Unlock()
	if bad || open == 0 || ev.GetSeq() <= open {
		t.Fatalf("heartbeat seq %d (bad %v), event seq %d: a client skipping seq <= heartbeat would lose it", open, bad, ev.GetSeq())
	}
}

func TestViewerCacheQueries(t *testing.T) {
	e, srv := captureServer(t)
	ctx := context.Background()
	u, err := srv.st.getUser(ctx, e.aliceID)
	if err != nil {
		t.Fatal(err)
	}
	v := newViewer(u)
	msg := &mycelv1.Event{Kind: kindMessage, Body: &mycelv1.Event_Message{Message: &mycelv1.Message{Channel: "#general"}}}
	grant := &mycelv1.Event{Kind: kindGrant, Body: &mycelv1.Event_Grant{Grant: &mycelv1.Grant{OwnerId: "usr_other", Node: "gpu-box"}}}
	agent := &mycelv1.Event{Kind: kindAgent, Body: &mycelv1.Event_Agent{Agent: &mycelv1.Agent{OwnerId: "usr_other"}}}
	for range 100 {
		if !srv.visible(ctx, v, msg, "") || !srv.visible(ctx, v, grant, "") || !srv.visible(ctx, v, agent, "") {
			t.Fatal("not visible")
		}
	}
	if v.queries != 2 {
		t.Fatalf("%d queries for 300 events, want 2 (one channel, one node owner)", v.queries)
	}
	v.invalidate(&mycelv1.Event{Kind: kindChannel, Id: "#general"})
	v.invalidate(&mycelv1.Event{Kind: kindNode, Id: "gpu-box"})
	srv.visible(ctx, v, msg, "")
	srv.visible(ctx, v, grant, "")
	if v.queries != 4 {
		t.Fatalf("%d queries after invalidation, want 4", v.queries)
	}
	v.reset()
	srv.visible(ctx, v, msg, "")
	if v.queries != 5 {
		t.Fatalf("%d queries after reset, want 5", v.queries)
	}
	// Audit entries by the row's owner; no query.
	audit := &mycelv1.Event{Kind: kindAudit, Body: &mycelv1.Event_Audit{Audit: &mycelv1.AuditEntry{Actor: "node:gpu-box"}}}
	if !srv.visible(ctx, v, audit, e.aliceID) || srv.visible(ctx, v, audit, e.bobID) || srv.visible(ctx, v, audit, "") || v.queries != 5 {
		t.Fatalf("audit visibility (queries %d)", v.queries)
	}
}

func TestStreamsEndWithTheirRights(t *testing.T) {
	setVar(t, &eventHeartbeat, 150*time.Millisecond)
	e := newGrantEnv(t)
	ctx := context.Background()
	b := e.browser(t)
	b.login(e.aliceKey)
	_, me := b.me()
	other := e.browser(t)
	other.login(e.aliceKey)
	_, otherMe := other.me()
	cookieEvents := mycelv1connect.NewEventServiceClient(b.c, b.base, connect.WithInterceptors(headers{csrfHeader: me.CSRF}))
	sess := watch(t, cookieEvents, &mycelv1.WatchEventsRequest{})
	scope := watch(t, e.events(scopedHeaders(), &e.node.cert), &mycelv1.WatchEventsRequest{})
	keyed := watch(t, e.events(keyHeaders(e.aliceKey), nil), &mycelv1.WatchEventsRequest{})
	eventually(t, 5*time.Second, "heartbeats", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.beats >= 1
	})
	// Signing out everywhere (from the other browser) ends the cookie stream.
	if code, _, _ := other.jsonDo("POST", "/api/auth/logout-all", nil, map[string]string{csrfHeader: otherMe.CSRF}); code != http.StatusNoContent {
		t.Fatalf("logout-all: %d", code)
	}
	wantCode(t, sess.waitErr(t), connect.CodeUnauthenticated)
	if code, _ := b.me(); code != http.StatusUnauthorized {
		t.Fatalf("me after logout-all: %d", code)
	}
	// Removing the node ends its read-scope stream.
	if _, err := e.adminCl.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "gpu-box"})); err != nil {
		t.Fatal(err)
	}
	wantCode(t, scope.waitErr(t), connect.CodeUnauthenticated)
	// The API-key stream lives on.
	select {
	case <-keyed.done:
		t.Fatalf("api key stream ended: %v", keyed.err)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestSearchDMsWindowsAndRate(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "@user:" + e.bobID, Text: "dm secret"})); err != nil {
		t.Fatal(err)
	}
	search := func(key string, q string) ([]*mycelv1.SearchHit, error) {
		resp, err := e.events(keyHeaders(key), nil).Search(ctx, connect.NewRequest(&mycelv1.SearchRequest{Query: q, Kinds: []mycelv1.EventKind{kindMessage}}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetHits(), nil
	}
	if hits, err := search(e.res.AdminAPIKey, "dm secret"); err != nil || len(hits) != 0 {
		t.Fatalf("admin search found a DM: %v %v", hits, err)
	}
	if hits, err := search(e.bobKey, "dm secret"); err != nil || len(hits) != 1 {
		t.Fatalf("bob: %v %v", hits, err)
	}
	// Windows: with one-seq windows and two of them only the newest two
	// messages are examined.
	for i := range 4 {
		if _, err := e.aliceCh.Post(ctx, connect.NewRequest(&mycelv1.PostRequest{Channel: "#general", Text: "window needle " + string(rune('a'+i))})); err != nil {
			t.Fatal(err)
		}
	}
	setVar(t, &searchWindow, 1)
	setVar(t, &searchWindows, 2)
	hits, err := search(e.aliceKey, "window needle")
	if err != nil || len(hits) == 0 || len(hits) > 2 {
		t.Fatalf("windowed search: %v %v", hits, err)
	}
	// At most 10 searches per second per user.
	limited := false
	for range 15 {
		if _, err := search(e.bobKey, "x"); connect.CodeOf(err) == connect.CodeResourceExhausted {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("search is not rate limited")
	}
}

func TestOriginStrict(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	b := e.browser(t)
	b.login(e.aliceKey)
	_, me := b.me()
	host := strings.TrimPrefix(e.h.ready.HubURL, "https://")
	for _, origin := range []string{"null", "http://" + host, "https://" + host + ".evil.example"} {
		_, err := b.users(headers{csrfHeader: me.CSRF, "Origin": origin}).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{}))
		wantCode(t, err, connect.CodeUnauthenticated)
	}
	if _, err := b.users(headers{csrfHeader: me.CSRF, "Origin": "https://" + strings.ToUpper(host)}).Whoami(ctx, connect.NewRequest(&mycelv1.WhoamiRequest{})); err != nil {
		t.Fatalf("same origin refused: %v", err)
	}
	// The passkey login ceremony belongs to the browser that began it.
	other := e.browser(t)
	if code, _, _ := b.jsonDo("POST", "/api/auth/passkey/login/begin", nil, nil); code != http.StatusOK {
		t.Fatalf("begin: %d", code)
	}
	if code, data, _ := other.jsonDo("POST", "/api/auth/passkey/login/finish", []byte("{}"), nil); code != http.StatusBadRequest ||
		!strings.Contains(string(data), "no passkey sign-in in progress") {
		t.Fatalf("finish from another browser: %d %s", code, data)
	}
}

func TestPushOwnershipAndAddresses(t *testing.T) {
	setVar[webpush.HTTPClient](t, &pushHTTPClient, &pushRecorder{hits: map[string]int{}})
	e := newGrantEnv(t)
	alice, bob := e.browser(t), e.browser(t)
	alice.login(e.aliceKey)
	bob.login(e.bobKey)
	_, am := alice.me()
	_, bm := bob.me()
	sub := subscription(t, "https://push.example.com/shared")
	if code, _, _ := alice.jsonDo("POST", "/api/push/subscriptions", sub, map[string]string{csrfHeader: am.CSRF}); code != http.StatusCreated {
		t.Fatalf("alice: %d", code)
	}
	if code, _, _ := bob.jsonDo("POST", "/api/push/subscriptions", sub, map[string]string{csrfHeader: bm.CSRF}); code != http.StatusConflict {
		t.Fatalf("bob took alice's endpoint: %d", code)
	}
	for addr, ok := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true, "100.64.1.1": false, "198.18.0.1": false, "192.0.0.8": false,
		"0.1.2.3": false, "64:ff9b::808:808": false, "10.0.0.1": false, "127.0.0.1": false, "::1": false,
		"169.254.169.254": false, "::ffff:10.0.0.1": false, "fd00::1": false,
	} {
		if publicAddr(net.ParseIP(addr)) != ok {
			t.Errorf("publicAddr(%s) = %v", addr, !ok)
		}
	}
}

func TestAuditOwnerMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE audit (seq INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, actor TEXT NOT NULL,
		action TEXT NOT NULL, resource TEXT NOT NULL, outcome TEXT NOT NULL, details TEXT NOT NULL);
		INSERT INTO audit(ts, actor, action, resource, outcome, details) VALUES('2026-01-01T00:00:00Z','usr_old','x','y','ok','{}')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	u, _, _, err := st.createUser(ctx, "dora", roleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.appendAudit(ctx, u.ID, "post", "channel:#x", outcomeOK, nil); err != nil {
		t.Fatal(err)
	}
	all, err := st.queryAudit(ctx, auditQuery{limit: 10})
	if err != nil || len(all) != 2 || all[0].OwnerID != u.ID || all[1].OwnerID != "" {
		t.Fatalf("all: %+v %v", all, err)
	}
	for _, member := range []string{u.ID, "usr_old"} {
		got, err := st.queryAudit(ctx, auditQuery{limit: 10, member: member})
		want := map[string]int{u.ID: 1, "usr_old": 0}[member] // old rows are admin-only
		if err != nil || len(got) != want {
			t.Fatalf("member %s: %+v %v", member, got, err)
		}
	}
}

func TestPruneGrantsEmitsDeletes(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var got []change
	st.onChange = func(c change) { got = append(got, c) }
	ctx := context.Background()
	g := &mycelv1.Grant{Id: "gr_old", Spec: gpuSpec(0), Subject: "agent:x@n", Node: "n", OwnerId: "usr_a", RequestedBy: "agent:x@n",
		Duration: "1h", State: mycelv1.GrantState_GRANT_STATE_REVOKED, CreatedAt: "2000-01-01T00:00:00Z"}
	if err := st.insertGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	got = nil
	if n, err := st.pruneGrants(ctx, nowString(), 0); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if len(got) != 1 || got[0].op != opDeleted || got[0].id != "gr_old" {
		t.Fatalf("changes %+v", got)
	}
	if body, ok := got[0].body.(*mycelv1.Grant); !ok || body.GetOwnerId() != "usr_a" || body.GetNode() != "n" {
		t.Fatalf("body %+v", got[0].body)
	}
}
