package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/daemon"
)

func publishEgress(t *testing.T, e *grantEnv, dropped uint64, recs ...*mycelv1.EgressRecord) {
	t.Helper()
	b, err := proto.Marshal(&mycelv1.Heartbeat{NodeName: "gpu-box", EgressDropped: dropped, Egress: recs})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.sn.nc.Publish(bus.NodeTelemetry("gpu-box"), b); err != nil {
		t.Fatal(err)
	}
	if err := e.sn.nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

func listAuditRes(t *testing.T, e *grantEnv, key, resource string) []*mycelv1.AuditEntry {
	t.Helper()
	var out []*mycelv1.AuditEntry
	page := ""
	for {
		resp, err := e.events(keyHeaders(key), nil).ListAudit(context.Background(),
			connect.NewRequest(&mycelv1.ListAuditRequest{Resource: resource, Limit: 1000, PageToken: page}))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, resp.Msg.GetEntries()...)
		if page = resp.Msg.GetNextPageToken(); page == "" {
			return out
		}
	}
}

// droppedCount sums the egress.dropped rows of gpu-box.
func droppedCount(t *testing.T, e *grantEnv) uint64 {
	t.Helper()
	var n uint64
	for _, r := range listAuditRes(t, e, e.res.AdminAPIKey, "node:gpu-box") {
		if r.GetAction() != actionEgressDropped {
			continue
		}
		var d struct {
			Count uint64 `json:"count"`
		}
		if err := json.Unmarshal([]byte(r.GetDetailsJson()), &d); err != nil {
			t.Fatal(err)
		}
		n += d.Count
	}
	return n
}

// A node's egress records become audit rows owned by the node's owner; a
// job id that is not the node's own job, and any unauthenticated record,
// is not trusted; bad hosts and drops are counted.
func TestEgressRecordsAudited(t *testing.T) {
	e := newGrantEnv(t)
	publishEgress(t, e, 3,
		&mycelv1.EgressRecord{SandboxId: "sbx:1", Host: "example.com", Port: 443, Allowed: true, Method: "CONNECT", BytesIn: 100, BytesOut: 20, Count: 4},
		&mycelv1.EgressRecord{SandboxId: "sbx:1", Host: "github.com", Port: 443, Allowed: false, Method: "CONNECT"},
		&mycelv1.EgressRecord{SandboxId: "sbx:2", JobId: "job_not_mine", Host: "example.org", Port: 80, Allowed: true, Method: "GET"},
		&mycelv1.EgressRecord{SandboxId: "sbx:1", JobId: "job_x", Host: "example.net", Port: 443, Method: "CONNECT", Unauthenticated: true},
		&mycelv1.EgressRecord{SandboxId: "sbx:1", Host: strings.Repeat("h", 300), Port: 443},
	)
	var rows []*mycelv1.AuditEntry
	eventually(t, 5*time.Second, "egress rows", func() bool {
		rows = listAuditRes(t, e, e.aliceKey, "sandbox:sbx:1")
		return len(rows) == 2
	})
	byHost := map[string]*mycelv1.AuditEntry{}
	for _, r := range rows {
		var d struct {
			Host    string `json:"host"`
			Port    int    `json:"port"`
			Allowed bool   `json:"allowed"`
			Bytes   int    `json:"bytes"`
			Count   int    `json:"count"`
		}
		if err := json.Unmarshal([]byte(r.GetDetailsJson()), &d); err != nil {
			t.Fatal(err)
		}
		if r.GetActor() != "node:gpu-box" || r.GetAction() != "egress" || d.Port != 443 {
			t.Fatalf("row %v", r)
		}
		byHost[d.Host] = r
		if d.Host == "example.com" && (!d.Allowed || d.Bytes != 120 || d.Count != 4 || r.GetOutcome() != "allowed") {
			t.Fatalf("allowed row %v", r)
		}
		if d.Host == "github.com" && (d.Allowed || d.Count != 1 || r.GetOutcome() != "denied") {
			t.Fatalf("denied row %v", r)
		}
	}
	if len(byHost) != 2 {
		t.Fatalf("rows %v", rows)
	}
	// A job id the node does not run is filed under the sandbox.
	eventually(t, 5*time.Second, "unbound job row", func() bool { return len(listAuditRes(t, e, e.res.AdminAPIKey, "sandbox:sbx:2")) == 1 })
	if got := listAuditRes(t, e, e.res.AdminAPIKey, "job:job_not_mine"); len(got) != 0 {
		t.Fatalf("spoofed job row: %v", got)
	}
	// Unauthenticated: under sandbox:unauth for the node's owner, no sandbox or job.
	un := listAuditRes(t, e, e.aliceKey, unauthSandbox)
	if len(un) != 1 || strings.Contains(un[0].GetDetailsJson(), "sbx:1") || !strings.Contains(un[0].GetDetailsJson(), `"unauthenticated":true`) {
		t.Fatalf("unauthenticated rows %v", un)
	}
	// The node's 3 drops plus the oversized host.
	if n := droppedCount(t, e); n != 4 {
		t.Fatalf("dropped %d, want 4", n)
	}
	// Bob does not own gpu-box.
	if got := listAuditRes(t, e, e.bobKey, "sandbox:sbx:1"); len(got) != 0 {
		t.Fatalf("bob sees %v", got)
	}
}

// The per-node row budget counts the excess into egress.dropped.
func TestEgressRateLimited(t *testing.T) {
	e := newGrantEnv(t)
	const batches = 6
	for i := range batches {
		recs := make([]*mycelv1.EgressRecord, maxEgressBatch)
		for j := range recs {
			recs[j] = &mycelv1.EgressRecord{SandboxId: "sbx:rl", Host: fmt.Sprintf("h%d-%d.example", i, j), Port: 443, Method: "CONNECT"}
		}
		publishEgress(t, e, 0, recs...)
	}
	total := uint64(batches * maxEgressBatch)
	var rows, dropped uint64
	eventually(t, 10*time.Second, "all records accounted for", func() bool {
		rows = uint64(len(listAuditRes(t, e, e.res.AdminAPIKey, "sandbox:sbx:rl")))
		dropped = droppedCount(t, e)
		return rows+dropped == total
	})
	if dropped == 0 || rows < egressRowsBurst {
		t.Fatalf("rows %d dropped %d of %d", rows, dropped, total)
	}
	t.Logf("%d rows written, %d dropped", rows, dropped)
}

// The daemon's proxy keeps the hub's read-only list, and every entry is a read.
func TestReadOnlyListIsReads(t *testing.T) {
	hub := map[string]bool{}
	for p := range readOnlyMethods {
		hub[p] = true
	}
	d := map[string]bool{}
	for _, p := range daemon.HubReadOnlyProcedures {
		d[p] = true
	}
	if len(hub) != len(d) {
		t.Fatalf("hub %d procedures, daemon %d", len(hub), len(d))
	}
	for p := range d {
		if !hub[p] {
			t.Errorf("daemon allows %s, hub does not", p)
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if !slices.ContainsFunc([]string{"List", "Get", "Watch", "Whoami", "Search", "FreezeStatus", "StreamLogs", "BlackboardGet"},
			func(pre string) bool { return strings.HasPrefix(name, pre) }) {
			t.Errorf("%s is on the read-only list but is not a read", p)
		}
	}
}
