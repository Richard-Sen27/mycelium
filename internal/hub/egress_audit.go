package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Phase 6: a node's egress proxy records (internal/daemon/egressfwd.go)
// arrive with its heartbeats and become audit rows: actor "node:<name>",
// action "egress", resource "job:<id>" (or "sandbox:<id>" for a sandbox
// without a job), outcome ok or denied, owner the job's owner (else the
// node's owner), details host, port, allowed, method, bytes, count. A job
// id is only taken when the job is assigned to the sending node; otherwise
// the record is filed under its sandbox. Records that failed proxy
// authentication go under "sandbox:unauth", owned by the node's owner.
// The sandbox inspector's Egress tab reads them with ListAudit(resource
// "job:<id>").
//
// Bounds: at most 256 records per batch, a token bucket of 256 rows per
// second (burst 1024) per node, at most 4 batches written at once; every
// record over a bound is counted into the node's next "egress.dropped" row
// (with the drops the node reported itself). One transaction per batch.

const (
	actionEgress        = "egress"
	actionEgressDropped = "egress.dropped"
	maxEgressBatch      = 256
	maxEgressHost       = 253
	maxEgressMethod     = 16
	maxEgressSandboxID  = 64
	egressRowsPerSecond = 256
	egressRowsBurst     = 1024
	// maxEgressWriters bounds the batches written at once; further batches
	// are dropped (and counted) rather than blocking the telemetry callback.
	maxEgressWriters = 4
	// unauthSandbox is the resource of records that failed proxy authentication.
	unauthSandbox = "sandbox:unauth"
)

// egressState bounds the egress audit writes per node.
type egressState struct {
	sem   chan struct{}
	limit *rateLimiter // rows per node

	mu      sync.Mutex
	dropped map[string]uint64 // per node, reported with the next batch
}

func newEgressState() *egressState {
	return &egressState{
		sem:     make(chan struct{}, maxEgressWriters),
		limit:   newRateLimiter(egressRowsPerSecond*60, egressRowsBurst),
		dropped: map[string]uint64{},
	}
}

func (e *egressState) addDropped(node string, n uint64) {
	if n == 0 {
		return
	}
	e.mu.Lock()
	e.dropped[node] += n
	e.mu.Unlock()
}

func (e *egressState) takeDropped(node string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := e.dropped[node]
	delete(e.dropped, node)
	return n
}

// handleEgress queues a heartbeat's egress batch for the audit log.
func (s *server) handleEgress(node string, hb *mycelv1.Heartbeat) {
	recs, dropped := hb.GetEgress(), hb.GetEgressDropped()
	if len(recs) == 0 && dropped == 0 {
		return
	}
	e := s.egress
	if e == nil {
		return
	}
	if len(recs) > maxEgressBatch {
		for _, r := range recs[maxEgressBatch:] {
			dropped += max(r.GetCount(), 1)
		}
		recs = recs[:maxEgressBatch]
	}
	select {
	case e.sem <- struct{}{}:
	default:
		for _, r := range recs {
			dropped += max(r.GetCount(), 1)
		}
		e.addDropped(node, dropped)
		s.log.Debug("egress records dropped: audit writer busy", "node", node, "records", len(recs))
		return
	}
	go func() {
		defer func() { <-e.sem }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*rpcTimeout)
		defer cancel()
		s.writeEgress(ctx, node, recs, dropped)
	}()
}

// egressRow is one audit row to append.
type egressRow struct {
	resource, outcome, owner string
	details                  map[string]any
}

func (s *server) writeEgress(ctx context.Context, node string, recs []*mycelv1.EgressRecord, dropped uint64) {
	actor := "node:" + node
	nodeOwner := s.st.auditOwner(ctx, actor)
	jobs := map[string]*jobRow{} // nil value: not this node's job
	rows := make([]egressRow, 0, len(recs))
	for _, r := range recs {
		count := max(r.GetCount(), 1)
		host := r.GetHost()
		if host == "" || len(host) > maxEgressHost || strings.ContainsFunc(host, func(c rune) bool { return c <= ' ' || c == 0x7f }) {
			dropped += count
			continue
		}
		if !s.egress.limit.allow(node) {
			dropped += count
			continue
		}
		method := r.GetMethod()
		if len(method) > maxEgressMethod {
			method = method[:maxEgressMethod]
		}
		sbx := r.GetSandboxId()
		if len(sbx) > maxEgressSandboxID {
			sbx = sbx[:maxEgressSandboxID]
		}
		resource, owner := "sandbox:"+sbx, nodeOwner
		switch {
		case r.GetUnauthenticated():
			// The claimed sandbox is the client's word: bind it to nothing.
			resource, sbx = unauthSandbox, ""
		case r.GetJobId() != "":
			id := r.GetJobId()
			j, seen := jobs[id]
			if !seen {
				row, err := s.st.getJob(ctx, id)
				if err != nil && !errors.Is(err, errNotFound) {
					s.log.Warn("egress audit: job lookup", "job", id, "err", err)
				}
				if err == nil && row.Node == node {
					j = row
				}
				jobs[id] = j
			}
			if j != nil {
				resource, owner = "job:"+j.ID, j.OwnerID
			}
		}
		outcome := outcomeOK
		if !r.GetAllowed() {
			outcome = outcomeDenied
		}
		details := map[string]any{
			"host": host, "port": r.GetPort(), "allowed": r.GetAllowed(), "method": method, "count": count,
			"bytes": r.GetBytesIn() + r.GetBytesOut(), "bytes_in": r.GetBytesIn(), "bytes_out": r.GetBytesOut(),
		}
		if sbx != "" {
			details["sandbox"] = sbx
		}
		if r.GetUnauthenticated() {
			details["unauthenticated"] = true
		}
		rows = append(rows, egressRow{resource: resource, outcome: outcome, owner: owner, details: details})
	}
	if err := s.st.appendAuditRows(ctx, actor, actionEgress, rows); err != nil {
		s.log.Error("audit write failed", "err", err, "action", actionEgress, "rows", len(rows))
		for _, r := range rows {
			dropped += r.details["count"].(uint64) //nolint:forcetypeassert // set above
		}
		rows = nil
	}
	for _, r := range rows {
		s.publishAudit(actor, actionEgress, r.resource, r.outcome, r.details)
	}
	if dropped += s.egress.takeDropped(node); dropped > 0 {
		s.log.Warn("egress records dropped", "node", node, "count", dropped)
		s.audit(ctx, actor, actionEgressDropped, actor, outcomeError, map[string]any{"count": dropped})
	}
}

// appendAuditRows appends rows of one actor and action in one transaction
// (each with its own owner: the user whose members' view lists it; "" =
// admins only) and reports them to the event stream after the commit.
func (s *store) appendAuditRows(ctx context.Context, actor, action string, rows []egressRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO audit(ts, actor, action, resource, outcome, details, owner_id) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	entries := make([]auditEntry, 0, len(rows))
	for _, r := range rows {
		b, err := json.Marshal(r.details)
		if err != nil {
			return err
		}
		ts := nowString()
		res, err := stmt.ExecContext(ctx, ts, actor, action, r.resource, r.outcome, string(b), nullable(r.owner))
		if err != nil {
			return err
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return err
		}
		entries = append(entries, auditEntry{Seq: seq, TS: ts, Actor: actor, Action: action, Resource: r.resource,
			Outcome: r.outcome, Details: string(b), OwnerID: r.owner})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, e := range entries {
		s.changed(kindAudit, opCreated, strconv.FormatInt(e.Seq, 10), auditBody{entry: e.proto(), owner: e.OwnerID})
	}
	return nil
}
