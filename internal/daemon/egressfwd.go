package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker/egress"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// Phase 6: the egress proxy's records go to the hub's audit log (the
// sandbox inspector's Egress tab). Each record (host, port, verdict,
// method, bytes; never bodies) is bounded (host at most 253 bytes without
// control characters, sandbox id 64, method 16; others are dropped and
// counted) and queued here; records with the same sandbox, host, port,
// method and verdict that wait for the same batch are coalesced into one
// with a count and summed bytes. Batches go on the node's telemetry
// subject as a Heartbeat: at most one per second, at most 256 records and
// 256 KiB per batch. Beyond a backlog of 4096 records new ones are dropped
// and counted, and the count travels with the next batch. A batch is kept
// for the next second only while the bus is disconnected; any other
// publish error drops it (counted), so one bad batch never wedges the
// queue. Records that failed proxy authentication carry no sandbox id
// (the client's claim is not trusted) and are bound to no job.

const (
	egressBatchEvery = time.Second
	egressBatchMax   = 256
	egressBatchBytes = 256 << 10
	egressBacklogMax = 4096
	// egressBindGrace keeps a finished sandbox's job id for records the
	// proxy reports after the sandbox was unregistered.
	egressBindGrace = time.Minute

	egressMaxHost      = 253
	egressMaxSandboxID = 64
	egressMaxMethod    = 16
)

// egressBatchLimit is egressBatchBytes; tests lower it.
var egressBatchLimit = egressBatchBytes

type egressBinding struct {
	job     string
	unbound time.Time // zero while the sandbox runs
}

// egressKey identifies records that may be coalesced.
type egressKey struct {
	sbx, host, method string
	port              uint32
	allowed, unauth   bool
}

func keyOf(r *mycelv1.EgressRecord) egressKey {
	return egressKey{sbx: r.GetSandboxId(), host: r.GetHost(), method: r.GetMethod(), port: r.GetPort(),
		allowed: r.GetAllowed(), unauth: r.GetUnauthenticated()}
}

// egressForwarder queues the proxy's records for the hub.
type egressForwarder struct {
	n       *node
	publish func([]*mycelv1.EgressRecord, uint64) error // n.publishEgress; tests replace it

	mu      sync.Mutex
	queue   []*mycelv1.EgressRecord
	pending map[egressKey]*mycelv1.EgressRecord // queued records by key (coalescing)
	dropped uint64
	sbx     map[string]*egressBinding // sandbox id -> job
}

func newEgressForwarder(n *node) *egressForwarder {
	f := &egressForwarder{n: n, sbx: map[string]*egressBinding{}, pending: map[egressKey]*mycelv1.EgressRecord{}}
	if n != nil {
		f.publish = n.publishEgress
	}
	return f
}

// bind names the job a sandbox runs (before it is registered with the proxy).
func (f *egressForwarder) bind(sbxID, jobID string) {
	f.mu.Lock()
	f.sbx[sbxID] = &egressBinding{job: jobID}
	f.mu.Unlock()
}

// unbind starts the grace period after which the binding is forgotten.
func (f *egressForwarder) unbind(sbxID string) {
	f.mu.Lock()
	if b := f.sbx[sbxID]; b != nil {
		b.unbound = time.Now()
	}
	f.mu.Unlock()
}

// validEgressHost: at most 253 bytes, no control characters or spaces.
func validEgressHost(h string) bool {
	if h == "" || len(h) > egressMaxHost {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func nonNeg(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// add queues one proxy record (the proxy's Audit callback).
func (f *egressForwarder) add(rec egress.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !validEgressHost(rec.Host) {
		f.dropped++
		return
	}
	r := &mycelv1.EgressRecord{
		Host: rec.Host, Allowed: rec.Allowed, Method: clip(rec.Method, egressMaxMethod),
		BytesIn: nonNeg(rec.BytesIn), BytesOut: nonNeg(rec.BytesOut), Count: 1,
		Unauthenticated: rec.Unauthenticated,
	}
	if rec.Port > 0 && rec.Port <= 65535 {
		r.Port = uint32(rec.Port) //nolint:gosec // range checked
	}
	if !rec.Unauthenticated {
		r.SandboxId = clip(rec.SandboxID, egressMaxSandboxID)
		if b := f.sbx[rec.SandboxID]; b != nil {
			r.JobId = b.job
		}
	}
	k := keyOf(r)
	if q := f.pending[k]; q != nil {
		q.Count++
		q.BytesIn += r.BytesIn
		q.BytesOut += r.BytesOut
		return
	}
	if len(f.queue) >= egressBacklogMax {
		f.dropped++
		return
	}
	f.queue = append(f.queue, r)
	f.pending[k] = r
}

// take removes up to egressBatchMax records (and at most egressBatchBytes)
// and the dropped count.
func (f *egressForwarder) take() ([]*mycelv1.EgressRecord, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for id, b := range f.sbx {
		if !b.unbound.IsZero() && now.Sub(b.unbound) > egressBindGrace {
			delete(f.sbx, id)
		}
	}
	k, size := 0, 0
	for k < len(f.queue) && k < egressBatchMax {
		sz := proto.Size(f.queue[k]) + 4 // field tag and length
		if k > 0 && size+sz > egressBatchLimit {
			break
		}
		size += sz
		k++
	}
	batch := f.queue[:k:k]
	for _, r := range batch {
		if f.pending[keyOf(r)] == r {
			delete(f.pending, keyOf(r))
		}
	}
	f.queue = append([]*mycelv1.EgressRecord(nil), f.queue[k:]...)
	d := f.dropped
	f.dropped = 0
	return batch, d
}

// requeue puts a batch that could not be sent (bus down) back in front;
// the backlog stays bounded and its records are no longer coalesced.
func (f *egressForwarder) requeue(batch []*mycelv1.EgressRecord, dropped uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped += dropped
	q := append(batch, f.queue...)
	if over := len(q) - egressBacklogMax; over > 0 {
		for _, r := range q[egressBacklogMax:] {
			f.dropped += max(r.GetCount(), 1)
			if f.pending[keyOf(r)] == r {
				delete(f.pending, keyOf(r))
			}
		}
		q = q[:egressBacklogMax]
	}
	f.queue = q
}

// discard counts a batch that will never be sent.
func (f *egressForwarder) discard(batch []*mycelv1.EgressRecord, dropped uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped += dropped
	for _, r := range batch {
		f.dropped += max(r.GetCount(), 1)
	}
}

// loop sends one batch per second until ctx ends.
func (f *egressForwarder) loop(ctx context.Context) {
	t := time.NewTicker(egressBatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f.flush()
	}
}

// flush sends one batch.
func (f *egressForwarder) flush() {
	batch, dropped := f.take()
	if len(batch) == 0 && dropped == 0 {
		return
	}
	switch err := f.publish(batch, dropped); {
	case err == nil:
	case errors.Is(err, errEgressBusDown):
		f.requeue(batch, dropped)
	default:
		if f.n != nil {
			f.n.log.Warn("egress batch dropped", "records", len(batch), "err", err)
		}
		f.discard(batch, dropped)
	}
}

// errEgressBusDown: the batch can be retried once the bus is back.
var errEgressBusDown = errors.New("bus not connected")

// publishEgress sends a batch as a light heartbeat on the telemetry
// subject. It returns errEgressBusDown while the connection is down; every other
// error means the batch can never be sent.
func (n *node) publishEgress(batch []*mycelv1.EgressRecord, dropped uint64) error {
	n.mu.Lock()
	nc, hash, name := n.nc, n.invHash, n.st.Name
	n.mu.Unlock()
	if nc == nil || !nc.IsConnected() {
		return errEgressBusDown
	}
	now := time.Now()
	b, err := proto.Marshal(&mycelv1.Heartbeat{
		NodeName: name, InventoryHash: hash,
		UptimeSeconds: uint64(max(now.Sub(n.start), 0).Seconds()),
		SentAt:        now.UTC().Format(time.RFC3339Nano),
		Egress:        batch, EgressDropped: dropped,
	})
	if err != nil {
		return err
	}
	if err := nc.Publish(bus.NodeTelemetry(name), b); err != nil {
		if errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionDraining) ||
			errors.Is(err, nats.ErrConnectionReconnecting) || errors.Is(err, nats.ErrDisconnected) {
			return errEgressBusDown
		}
		return err // e.g. nats.ErrMaxPayload: never retried
	}
	return nil
}
