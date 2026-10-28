package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// grantSchema holds the phase 5 tables: grants, broker usage per grant,
// node sharing settings (hub-side and the node's last reported local
// ceiling) and active freezes. Grant states are stored as the GrantState
// enum number; spec, hub and local are protobuf blobs.
const grantSchema = `
CREATE TABLE IF NOT EXISTS grants (
	id TEXT PRIMARY KEY,
	spec BLOB NOT NULL,
	subject TEXT NOT NULL,
	node TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	requested_by TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	duration TEXT NOT NULL,
	state INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	activated_at TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL DEFAULT '',
	decided_by TEXT NOT NULL DEFAULT '',
	decision_note TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	requires_approval INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS grants_state ON grants(state);
CREATE INDEX IF NOT EXISTS grants_subject ON grants(subject, state);
CREATE INDEX IF NOT EXISTS grants_node ON grants(node, state);
CREATE INDEX IF NOT EXISTS grants_owner ON grants(owner_id, state);
CREATE TABLE IF NOT EXISTS grant_usage (
	grant_id TEXT NOT NULL,
	subject TEXT NOT NULL,
	broker TEXT NOT NULL,
	tokens INTEGER NOT NULL DEFAULT 0,
	requests INTEGER NOT NULL DEFAULT 0,
	gpu_seconds INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (grant_id, broker)
);
CREATE TABLE IF NOT EXISTS node_sharing (
	node TEXT PRIMARY KEY,
	hub BLOB,
	local BLOB,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS freezes (
	scope TEXT PRIMARY KEY,
	reason TEXT NOT NULL DEFAULT '',
	by TEXT NOT NULL,
	since TEXT NOT NULL
);
`

// migrateMembers adds channel_members.added_by (phase 5) when missing.
func migrateMembers(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(channel_members)`)
	if err != nil {
		return fmt.Errorf("hub: inspect channel_members: %w", err)
	}
	have := false
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("hub: inspect channel_members: %w", err)
		}
		have = have || name == "added_by"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if have {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE channel_members ADD COLUMN added_by TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("hub: add channel_members.added_by: %w", err)
	}
	return nil
}

const grantCols = `id, spec, subject, node, owner_id, requested_by, reason, duration, state, created_at,
	activated_at, expires_at, decided_by, decision_note, channel, requires_approval`

func scanGrant(sc interface{ Scan(...any) error }) (*mycelv1.Grant, error) {
	var (
		g    mycelv1.Grant
		spec []byte
		st   int32
	)
	if err := sc.Scan(&g.Id, &spec, &g.Subject, &g.Node, &g.OwnerId, &g.RequestedBy, &g.Reason, &g.Duration, &st,
		&g.CreatedAt, &g.ActivatedAt, &g.ExpiresAt, &g.DecidedBy, &g.DecisionNote, &g.Channel, &g.RequiresApproval); err != nil {
		return nil, err
	}
	g.State = mycelv1.GrantState(st)
	g.Spec = &mycelv1.GrantSpec{}
	if err := proto.Unmarshal(spec, g.Spec); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *store) insertGrant(ctx context.Context, g *mycelv1.Grant) error {
	spec, err := proto.Marshal(g.GetSpec())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO grants(`+grantCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.GetId(), spec, g.GetSubject(), g.GetNode(), g.GetOwnerId(), g.GetRequestedBy(), g.GetReason(), g.GetDuration(),
		int32(g.GetState()), g.GetCreatedAt(), g.GetActivatedAt(), g.GetExpiresAt(), g.GetDecidedBy(), g.GetDecisionNote(),
		g.GetChannel(), g.GetRequiresApproval())
	if isUnique(err) {
		return errConflict
	}
	if err == nil {
		s.changed(kindGrant, opCreated, g.GetId(), nil)
	}
	return err
}

func (s *store) getGrant(ctx context.Context, id string) (*mycelv1.Grant, error) {
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantCols+` FROM grants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return g, err
}

// grantFilter selects grants; empty fields match everything.
type grantFilter struct {
	states  []mycelv1.GrantState
	node    string
	subject string
	owner   string
	// visibleTo keeps the grants a member may see: subject owned by the
	// user or attached to a node the user owns.
	visibleTo string
	// nodeOwner keeps the grants attached to nodes this user owns.
	nodeOwner string
	activated bool // only grants that were active once
	limit     int
}

func (s *store) listGrants(ctx context.Context, f grantFilter) ([]*mycelv1.Grant, error) {
	var (
		where []string
		args  []any
	)
	if len(f.states) > 0 {
		ph := make([]string, len(f.states))
		for i, st := range f.states {
			ph[i] = "?"
			args = append(args, int32(st))
		}
		where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	}
	for _, c := range []struct{ col, v string }{{"node", f.node}, {"subject", f.subject}, {"owner_id", f.owner}} {
		if c.v != "" {
			where = append(where, c.col+" = ?")
			args = append(args, c.v)
		}
	}
	if f.visibleTo != "" {
		where = append(where, "(owner_id = ? OR node IN (SELECT name FROM nodes WHERE owner_id = ?))")
		args = append(args, f.visibleTo, f.visibleTo)
	}
	if f.nodeOwner != "" {
		where = append(where, "node IN (SELECT name FROM nodes WHERE owner_id = ?)")
		args = append(args, f.nodeOwner)
	}
	if f.activated {
		where = append(where, "activated_at != ''")
	}
	q := `SELECT ` + grantCols + ` FROM grants`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // constant conditions with bound values
	}
	q += " ORDER BY created_at DESC, id DESC"
	if f.limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*mycelv1.Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// updateGrant stores g's state, duration, times and decision when the
// stored state is one of from; errStateChanged otherwise.
func (s *store) updateGrant(ctx context.Context, g *mycelv1.Grant, from ...mycelv1.GrantState) error {
	ph := make([]string, len(from))
	args := []any{int32(g.GetState()), g.GetDuration(), g.GetActivatedAt(), g.GetExpiresAt(), g.GetDecidedBy(),
		g.GetDecisionNote(), g.GetId()}
	for i, st := range from {
		ph[i] = "?"
		args = append(args, int32(st))
	}
	//nolint:gosec // only "?" placeholders are joined in; values are bound
	res, err := s.db.ExecContext(ctx, `UPDATE grants SET state = ?, duration = ?, activated_at = ?, expires_at = ?,
		decided_by = ?, decision_note = ? WHERE id = ? AND state IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errStateChanged
	}
	s.changed(kindGrant, opUpdated, g.GetId(), nil)
	return nil
}

// pruneGrants deletes finished grants (and their usage) created before
// cutoff or beyond the newest keep finished ones, and reports how many.
func (s *store) pruneGrants(ctx context.Context, cutoff string, keep int) (int, error) {
	n := 0
	var gone []*mycelv1.Grant
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		gone = gone[:0]
		const finished = `state NOT IN (?, ?)`
		open := []any{int32(mycelv1.GrantState_GRANT_STATE_PENDING), int32(mycelv1.GrantState_GRANT_STATE_ACTIVE)}
		sel := `SELECT id, owner_id, node FROM grants WHERE ` + finished + ` AND (created_at < ? OR id NOT IN
			(SELECT id FROM grants WHERE ` + finished + ` ORDER BY created_at DESC, id DESC LIMIT ?))`
		rows, err := tx.QueryContext(ctx, sel, append(append(open, cutoff), append(open, keep)...)...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			g := &mycelv1.Grant{}
			if err := rows.Scan(&g.Id, &g.OwnerId, &g.Node); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, g.Id)
			gone = append(gone, g)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM grant_usage WHERE grant_id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM grants WHERE id = ?`, id); err != nil {
				return err
			}
		}
		n = len(ids)
		return nil
	})
	if err == nil {
		for _, g := range gone {
			s.changed(kindGrant, opDeleted, g.GetId(), g)
		}
	}
	return n, err
}

// addGrantUsage adds broker usage increments for a grant.
func (s *store) addGrantUsage(ctx context.Context, grantID, subject, broker string, tokens, requests, gpuSeconds int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO grant_usage(grant_id, subject, broker, tokens, requests, gpu_seconds, updated_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(grant_id, broker) DO UPDATE SET tokens = tokens + excluded.tokens,
		requests = requests + excluded.requests, gpu_seconds = gpu_seconds + excluded.gpu_seconds,
		updated_at = excluded.updated_at`, grantID, subject, broker, tokens, requests, gpuSeconds, nowNano())
	return s.changedOn(kindGrant, grantID, err)
}

// grantUsageRow is one grant_usage row.
type grantUsageRow struct {
	Subject    string
	Broker     string
	Tokens     int64
	Requests   int64
	GPUSeconds int64
}

func (s *store) grantUsage(ctx context.Context, grantID string) ([]grantUsageRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, broker, tokens, requests, gpu_seconds FROM grant_usage
		WHERE grant_id = ? ORDER BY broker`, grantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []grantUsageRow
	for rows.Next() {
		var u grantUsageRow
		if err := rows.Scan(&u.Subject, &u.Broker, &u.Tokens, &u.Requests, &u.GPUSeconds); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- sharing ----

// nodeSharing returns the stored hub-side and local settings of a node;
// either is nil when not set.
func (s *store) nodeSharing(ctx context.Context, node string) (hub, local *mycelv1.SharingSettings, err error) {
	var hb, lb []byte
	err = s.db.QueryRowContext(ctx, `SELECT hub, local FROM node_sharing WHERE node = ?`, node).Scan(&hb, &lb)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dec := func(b []byte) (*mycelv1.SharingSettings, error) {
		if b == nil {
			return nil, nil
		}
		var v mycelv1.SharingSettings
		if err := proto.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		return &v, nil
	}
	if hub, err = dec(hb); err != nil {
		return nil, nil, err
	}
	if local, err = dec(lb); err != nil {
		return nil, nil, err
	}
	return hub, local, nil
}

// setNodeSharing stores one side ("hub" or "local") of a node's settings.
func (s *store) setNodeSharing(ctx context.Context, node, side string, v *mycelv1.SharingSettings) error {
	if side != "hub" && side != "local" {
		return errors.New("bad sharing side")
	}
	var b any // nil (SQL NULL) clears the side
	if v != nil {
		enc, err := proto.Marshal(v)
		if err != nil {
			return err
		}
		if enc == nil {
			enc = []byte{}
		}
		b = enc
	}
	//nolint:gosec // side is "hub" or "local", checked above; values are bound
	_, err := s.db.ExecContext(ctx, `INSERT INTO node_sharing(node, `+side+`, updated_at) VALUES(?,?,?)
		ON CONFLICT(node) DO UPDATE SET `+side+` = excluded.`+side+`, updated_at = excluded.updated_at`, node, b, nowString())
	return s.changedOn(kindNode, node, err)
}

func (s *store) deleteNodeSharing(ctx context.Context, node string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM node_sharing WHERE node = ?`, node)
	return err
}

// ---- freezes ----

type freezeRow struct {
	Scope  string // "all" or "channel:<name>"
	Reason string
	By     string
	Since  string
}

func (s *store) listFreezes(ctx context.Context) ([]freezeRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope, reason, by, since FROM freezes ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []freezeRow
	for rows.Next() {
		var f freezeRow
		if err := rows.Scan(&f.Scope, &f.Reason, &f.By, &f.Since); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// insertFreeze stores a freeze; it reports false when the scope was
// already frozen (the stored row is kept).
func (s *store) insertFreeze(ctx context.Context, f freezeRow) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO freezes(scope, reason, by, since) VALUES(?,?,?,?)`,
		f.Scope, f.Reason, f.By, f.Since)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// deleteFreeze removes a freeze; it reports false when there was none.
func (s *store) deleteFreeze(ctx context.Context, scope string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM freezes WHERE scope = ?`, scope)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
