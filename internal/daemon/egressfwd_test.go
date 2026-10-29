package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker/egress"
)

func TestEgressForwarderBatches(t *testing.T) {
	f := newEgressForwarder(nil)
	f.bind("sbx:1", "job_1")
	// Distinct hosts: no coalescing; the backlog caps the queue.
	for i := range egressBacklogMax + 10 {
		f.add(egress.Record{SandboxID: "sbx:1", Host: fmt.Sprintf("h%d.example.com", i), Port: 443, Allowed: true, Method: "CONNECT", BytesIn: 5, BytesOut: -1})
	}
	batch, dropped := f.take()
	if len(batch) != egressBatchMax || dropped != 10 {
		t.Fatalf("batch %d dropped %d", len(batch), dropped)
	}
	r := batch[0]
	if r.GetJobId() != "job_1" || r.GetPort() != 443 || r.GetBytesIn() != 5 || r.GetBytesOut() != 0 || r.GetCount() != 1 {
		t.Fatalf("record %v", r)
	}
	// A batch kept back (bus down) goes back in front.
	f.requeue(batch, dropped)
	batch2, dropped2 := f.take()
	if batch2[0] != r || dropped2 != 10 {
		t.Fatalf("requeue: %v %d", batch2[0], dropped2)
	}
	f.unbind("sbx:1")
	f.add(egress.Record{SandboxID: "sbx:1", Host: "late.example.com"})
	f.mu.Lock()
	last := f.queue[len(f.queue)-1]
	f.mu.Unlock()
	if last.GetJobId() != "job_1" {
		t.Fatalf("a record within the grace period lost its job: %v", last)
	}
}

func TestEgressForwarderBoundsAndCoalesces(t *testing.T) {
	f := newEgressForwarder(nil)
	f.bind("sbx:1", "job_1")
	// Oversized or malformed hosts are dropped and counted.
	f.add(egress.Record{SandboxID: "sbx:1", Host: strings.Repeat("a", 60<<10), Port: 443})
	f.add(egress.Record{SandboxID: "sbx:1", Host: "evil\r\nhost", Port: 443})
	f.add(egress.Record{SandboxID: "sbx:1", Host: "", Port: 443})
	// Long fields are cut.
	f.add(egress.Record{SandboxID: "sbx:1" + strings.Repeat("x", 500), Host: "a.example", Method: strings.Repeat("M", 100)})
	// Same key: one record with a count and summed bytes.
	for range 5 {
		f.add(egress.Record{SandboxID: "sbx:1", Host: "example.com", Port: 443, Allowed: true, Method: "CONNECT", BytesIn: 10, BytesOut: 2})
	}
	f.add(egress.Record{SandboxID: "sbx:1", Host: "example.com", Port: 443, Allowed: false, Method: "CONNECT"})
	// Unauthenticated: the claimed sandbox is not kept or bound.
	f.add(egress.Record{SandboxID: "sbx:1", Host: "example.com", Port: 443, Method: "CONNECT", Unauthenticated: true})
	batch, dropped := f.take()
	if dropped != 3 || len(batch) != 4 {
		t.Fatalf("batch %v dropped %d", batch, dropped)
	}
	if len(batch[0].GetSandboxId()) != egressMaxSandboxID || len(batch[0].GetMethod()) != egressMaxMethod || batch[0].GetJobId() != "" {
		t.Fatalf("cut record %v", batch[0])
	}
	if c := batch[1]; c.GetCount() != 5 || c.GetBytesIn() != 50 || c.GetBytesOut() != 10 || c.GetJobId() != "job_1" {
		t.Fatalf("coalesced record %v", c)
	}
	if u := batch[3]; !u.GetUnauthenticated() || u.GetSandboxId() != "" || u.GetJobId() != "" {
		t.Fatalf("unauthenticated record %v", u)
	}
	// After a take, the same key starts a new record.
	f.add(egress.Record{SandboxID: "sbx:1", Host: "example.com", Port: 443, Allowed: true, Method: "CONNECT"})
	if b, _ := f.take(); len(b) != 1 || b[0].GetCount() != 1 {
		t.Fatalf("after take: %v", b)
	}
}

func TestEgressForwarderSizeCapAndErrors(t *testing.T) {
	f := newEgressForwarder(nil)
	egressBatchLimit = 32 << 10
	t.Cleanup(func() { egressBatchLimit = egressBatchBytes })
	host := strings.Repeat("h", egressMaxHost)
	for i := range egressBatchMax {
		f.add(egress.Record{SandboxID: fmt.Sprintf("sbx:%04d%s", i, strings.Repeat("s", 50)), Host: host, Port: 443, Method: "CONNECT"})
	}
	var sent [][]*mycelv1.EgressRecord
	f.publish = func(b []*mycelv1.EgressRecord, _ uint64) error {
		sent = append(sent, b)
		size := 0
		for _, r := range b {
			size += proto.Size(r)
		}
		if size > egressBatchLimit {
			t.Errorf("batch of %d bytes", size)
		}
		return nil
	}
	f.flush()
	if len(sent) != 1 || len(sent[0]) == 0 || len(sent[0]) >= egressBatchMax {
		t.Fatalf("sent %d batches", len(sent))
	}
	t.Logf("first batch: %d records", len(sent[0]))
	// A payload error drops the batch (counted) instead of retrying it forever.
	f.publish = func([]*mycelv1.EgressRecord, uint64) error { return nats.ErrMaxPayload }
	left := len(f.queue)
	f.flush()
	if f.dropped == 0 || len(f.queue) >= left {
		t.Fatalf("after ErrMaxPayload: queue %d (was %d), dropped %d", len(f.queue), left, f.dropped)
	}
	// The bus being down keeps the batch.
	f.publish = func([]*mycelv1.EgressRecord, uint64) error { return errEgressBusDown }
	left, dropped := len(f.queue), f.dropped
	f.flush()
	if len(f.queue) != left || f.dropped != dropped {
		t.Fatalf("after a disconnect: queue %d (was %d), dropped %d (was %d)", len(f.queue), left, f.dropped, dropped)
	}
}
