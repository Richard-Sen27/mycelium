package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// sealBytes encrypts data in the export format.
func sealBytes(t *testing.T, pass, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	sw, err := newSealWriter(&buf, pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func openBytes(data, pass []byte) ([]byte, error) {
	r, err := newOpenReader(bytes.NewReader(data), pass)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestSegmentedExportFormat(t *testing.T) {
	pass := []byte("correct horse battery")
	if _, err := newSealWriter(io.Discard, []byte("short")); err == nil {
		t.Fatal("short passphrase accepted")
	}
	big := make([]byte, 2*exportSegment+12345)
	_, _ = rand.Read(big)
	for _, n := range []int{0, 1, exportSegment - 1, exportSegment, exportSegment + 1, len(big)} {
		plain := big[:n]
		data := sealBytes(t, pass, plain)
		segs := n/exportSegment + 1 // a full last buffer is the last segment itself
		if n > 0 && n%exportSegment == 0 {
			segs = n / exportSegment
		}
		if !bytes.HasPrefix(data, []byte("MYCELHUB2")) || len(data) != exportHdrLen+n+segs*exportTagLen {
			t.Fatalf("n=%d: %d bytes, want %d", n, len(data), exportHdrLen+n+segs*exportTagLen)
		}
		got, err := openBytes(data, pass)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: round trip: %v", n, err)
		}
	}
	data := sealBytes(t, pass, big)
	if bytes.Contains(data, big[100:140]) {
		t.Fatal("plaintext visible")
	}
	if _, err := openBytes(data, []byte("wrong passphrase!!")); !errors.Is(err, ErrExportDecrypt) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	seg := exportSegment + exportTagLen
	tamper := func(name string, bad []byte) {
		t.Helper()
		if _, err := openBytes(bad, pass); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, i := range []int{3, len(exportMagic) + 1, len(exportMagic) + exportSaltLen + 1, exportHdrLen + 5, exportHdrLen + seg + 5, len(data) - 1} {
		bad := bytes.Clone(data)
		bad[i] ^= 1
		tamper("flipped byte", bad)
	}
	// Cut at a segment boundary: the remaining last segment was not sealed as last.
	tamper("truncated at a boundary", data[:exportHdrLen+seg])
	tamper("truncated mid-segment", data[:exportHdrLen+seg+100])
	tamper("header only", data[:exportHdrLen])
	// Segments swapped.
	sw := bytes.Clone(data)
	copy(sw[exportHdrLen:], data[exportHdrLen+seg:exportHdrLen+2*seg])
	copy(sw[exportHdrLen+seg:], data[exportHdrLen:exportHdrLen+seg])
	tamper("swapped segments", sw)
	tamper("trailing data", append(bytes.Clone(data), 0))
	if _, err := openBytes([]byte("MYCELHUB1"+strings.Repeat("x", 60)), pass); err == nil {
		t.Fatal("old format accepted")
	}
}

func TestImportTargetChecks(t *testing.T) {
	root := t.TempDir()
	full := filepath.Join(root, "full")
	if err := os.MkdirAll(full, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(full, "stray"), "x")
	file := filepath.Join(root, "file")
	writeTestFile(t, file, "x")
	cases := map[string]string{full: "already holds", file: "not a directory"}
	if runtime.GOOS != "windows" {
		real := filepath.Join(root, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		cases[link] = "symbolic link"
		cases[filepath.Join(link, "hub")] = "symbolic link"
	}
	for dir, want := range cases {
		_, err := Import(context.Background(), ImportOptions{HubDir: dir, Source: strings.NewReader("x"), Passphrase: []byte("a long export passphrase"), AdvertiseURL: "https://new:7777"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", dir, err, want)
		}
	}
}

func writeTestFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestImportRollbackRemovesEverything: a file damaged late in the archive
// fails after jetstream/ and other files were written; nothing remains.
func TestImportRollbackRemovesEverything(t *testing.T) {
	opts, _ := initTestHub(t)
	pass := []byte("a long export passphrase")
	// Bulk the archive with an artifact so it spans several segments.
	if err := os.MkdirAll(filepath.Join(opts.HubDir, artifactsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, 3*exportSegment)
	_, _ = rand.Read(noise)
	writeTestFile(t, filepath.Join(opts.HubDir, artifactsSubdir, "blob"), string(noise))
	var file bytes.Buffer
	if err := Export(context.Background(), ExportOptions{HubDir: opts.HubDir, Passphrase: pass, Logger: quietLogger()}, &file); err != nil {
		t.Fatal(err)
	}
	data := file.Bytes()
	data[len(data)-20] ^= 1
	root := t.TempDir()
	dir := filepath.Join(root, "hub")
	_, err := Import(context.Background(), ImportOptions{HubDir: dir, Source: bytes.NewReader(data), Passphrase: pass, AdvertiseURL: "https://new:7777", Logger: quietLogger()})
	if !errors.Is(err, ErrExportDecrypt) {
		t.Fatalf("damaged export: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestExcludeArtifacts(t *testing.T) {
	opts, _ := initTestHub(t)
	if err := os.MkdirAll(filepath.Join(opts.HubDir, artifactsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(opts.HubDir, artifactsSubdir, "blob"), "artifact bytes")
	pass := []byte("a long export passphrase")
	var file bytes.Buffer
	if err := Export(context.Background(), ExportOptions{HubDir: opts.HubDir, Passphrase: pass, ExcludeArtifacts: true}, &file); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "hub")
	if _, err := Import(context.Background(), ImportOptions{HubDir: dir, Source: &file, Passphrase: pass, AdvertiseURL: "https://127.0.0.1:7777", Logger: quietLogger()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, artifactsSubdir, "blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("artifact exported despite --exclude-artifacts")
	}
}

func TestHubLockGuardsOfflineOperations(t *testing.T) {
	opts, res := initTestHub(t)
	stale := filepath.Join(opts.HubDir, ".export-123")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	_, stop := runTestHub(t, opts, res, false)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale export directory not cleaned at start")
	}
	pass := []byte("a long export passphrase")
	if err := Export(context.Background(), ExportOptions{HubDir: opts.HubDir, Passphrase: pass}, io.Discard); !errors.Is(err, errHubLocked) {
		t.Fatalf("offline export of a running hub: %v", err)
	}
	if _, err := Redirect(context.Background(), RedirectOptions{HubDir: opts.HubDir, HubURL: "https://new.example.com:7777"}); !errors.Is(err, errHubLocked) {
		t.Fatalf("offline redirect of a running hub: %v", err)
	}
	// A second hub on the same directory refuses to start.
	if err := Run(context.Background(), Options{HubDir: opts.HubDir, APIListen: "127.0.0.1:0", BusListen: "127.0.0.1:0", Logger: quietLogger()}, nil); !errors.Is(err, errHubLocked) {
		t.Fatalf("second hub: %v", err)
	}
	stop()
	if err := Export(context.Background(), ExportOptions{HubDir: opts.HubDir, Passphrase: pass}, io.Discard); err != nil {
		t.Fatalf("offline export once stopped: %v", err)
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n -= len(p); f.n < 0 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestOfflineExportAuditedWithOutcome(t *testing.T) {
	opts, _ := initTestHub(t)
	pass := []byte("a long export passphrase")
	if err := Export(context.Background(), ExportOptions{HubDir: opts.HubDir, Passphrase: pass}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Export(context.Background(), ExportOptions{HubDir: opts.HubDir, Passphrase: pass}, &failWriter{n: 100}); err == nil {
		t.Fatal("export to a failing writer succeeded")
	}
	st, err := openStore(filepath.Join(opts.HubDir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	entries, err := st.listAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, a := range entries {
		if a.Action == "hub.export" {
			outcomes = append(outcomes, a.Outcome)
		}
	}
	if strings.Join(outcomes, ",") != outcomeOK+","+outcomeError && strings.Join(outcomes, ",") != outcomeError+","+outcomeOK {
		t.Fatalf("export audit outcomes %v", outcomes)
	}
}

func TestOneLiveExportAtATime(t *testing.T) {
	var srv *server
	setVar(t, &testHookServer, func(s *server) { srv = s })
	opts, res := initTestHub(t)
	h, _ := runTestHub(t, opts, res, false)
	srv.move.exporting.Store(true)
	if r := adminPostTest(t, h, "/api/admin/export", res.AdminAPIKey, struct{}{}); r.StatusCode != http.StatusConflict {
		t.Fatalf("second export: %d", r.StatusCode)
	}
	srv.move.exporting.Store(false)
	r := adminPostTest(t, h, "/api/admin/export", res.AdminAPIKey, struct{}{})
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK || srv.move.exporting.Load() {
		t.Fatalf("export: %d, still busy %v", r.StatusCode, srv.move.exporting.Load())
	}
}

func TestNextIssuedClockJump(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if got := nextIssued(now.Unix()-10, now, nil); got != now.Unix() {
		t.Fatalf("past: %d", got)
	}
	if got := nextIssued(now.Unix()+10, now, nil); got != now.Unix()+11 {
		t.Fatalf("slightly ahead: %d", got)
	}
	if got := nextIssued(now.Unix()+identity.RedirectMaxFuture+1, now, quietLogger()); got != now.Unix() {
		t.Fatalf("clock jumped back: %d", got)
	}
	if fresh(now.Unix()+identity.RedirectMaxFuture+1, now) || !fresh(now.Unix()-60, now) || fresh(now.Unix()-3600, now) {
		t.Fatal("fresh")
	}

	// A stored redirect dated far ahead is signed again with the current time.
	opts, res := initTestHub(t)
	ca, err := loadHubCA(opts.HubDir)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour).Unix()
	r, err := signRedirect(ca, "https://moved.example.com:7777", "tls://moved.example.com:7778", future)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistMoved(opts.HubDir, r); err != nil {
		t.Fatal(err)
	}
	h, _ := runTestHub(t, opts, res, false)
	_, err = h.clients(res.AdminAPIKey, nil).users.Whoami(context.Background(), connect.NewRequest(&mycelv1.WhoamiRequest{}))
	got := daemon.RedirectFromError(err)
	if got == nil || got.GetIssuedAt() > time.Now().Unix()+identity.RedirectMaxFuture || daemon.VerifyRedirect(res.CACertPEM, got, time.Now()) != nil {
		t.Fatalf("redirect after a clock jump: %+v", got)
	}
}

func TestMovedRedirectResentOnHeartbeat(t *testing.T) {
	var srv *server
	setVar(t, &testHookServer, func(s *server) { srv = s })
	opts, res := initTestHub(t)
	h, _ := runTestHub(t, opts, res, false)
	admin := h.clients(res.AdminAPIKey, nil)
	e := enrol(t, admin, createToken(t, admin, "n1", nil, ""), "n1")
	sn := h.connectNode(t, e)
	if r := adminPostTest(t, h, "/api/admin/redirect", res.AdminAPIKey, map[string]string{"hub_url": "https://new.example.com:7777"}); r.StatusCode != http.StatusOK {
		t.Fatalf("redirect: %d", r.StatusCode)
	}
	waitRedirect := func(what string) {
		t.Helper()
		eventually(t, 5*time.Second, what, func() bool {
			select {
			case req := <-sn.ctl:
				return req.GetRedirect() != nil
			default:
				return false
			}
		})
	}
	waitRedirect("first redirect")
	time.Sleep(300 * time.Millisecond) // the connect-time sync may queue one more
	for len(sn.ctl) > 0 {
		<-sn.ctl
	}
	srv.move.mu.Lock()
	srv.move.sent["n1"] = time.Now()
	srv.move.mu.Unlock()
	beat := func() {
		b, _ := proto.Marshal(&mycelv1.Heartbeat{NodeName: "n1", Inventory: &mycelv1.Inventory{Os: "linux"}})
		if err := sn.nc.Publish(bus.NodeTelemetry("n1"), b); err != nil {
			t.Fatal(err)
		}
		_ = sn.nc.Flush()
	}
	// The first heartbeat carries a new inventory, whose sync re-queues
	// the redirect too; drain that.
	beat()
	time.Sleep(300 * time.Millisecond)
	for len(sn.ctl) > 0 {
		<-sn.ctl
	}
	srv.move.mu.Lock()
	srv.move.sent["n1"] = time.Now()
	srv.move.mu.Unlock()
	// Within redirectResend an unchanged heartbeat queues nothing new.
	beat()
	time.Sleep(300 * time.Millisecond)
	select {
	case req := <-sn.ctl:
		t.Fatalf("re-sent too soon: %v", req)
	default:
	}
	// Once redirectResend has passed, the next heartbeat re-queues it.
	srv.move.mu.Lock()
	srv.move.sent["n1"] = time.Now().Add(-redirectResend - time.Second)
	srv.move.mu.Unlock()
	beat()
	waitRedirect("redirect re-sent on heartbeat")
}

func TestMovedStreamingRPCCarriesRedirect(t *testing.T) {
	opts, res := initTestHub(t)
	h, _ := runTestHub(t, opts, res, false)
	if r := adminPostTest(t, h, "/api/admin/redirect", res.AdminAPIKey, map[string]string{"hub_url": "https://new.example.com:7777"}); r.StatusCode != http.StatusOK {
		t.Fatalf("redirect: %d", r.StatusCode)
	}
	ch := mycelv1connect.NewChannelServiceClient(h.httpClient(nil), h.ready.HubURL, bearer(res.AdminAPIKey))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := ch.Watch(ctx, connect.NewRequest(&mycelv1.WatchRequest{}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
		_ = stream.Close()
	}
	wantCode(t, err, connect.CodeFailedPrecondition)
	r := daemon.RedirectFromError(err)
	if r == nil || daemon.VerifyRedirect(res.CACertPEM, r, time.Now()) != nil || r.GetHubUrl() != "https://new.example.com:7777" {
		t.Fatalf("streaming moved error %v, redirect %+v", err, r)
	}
}
