package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Phase 6 tables: passkeys, push subscriptions, the message search index
// and audit lookup indexes.
const uiSchema = `
CREATE TABLE IF NOT EXISTS passkeys (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	credential TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_used_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS passkeys_user ON passkeys(user_id);
CREATE TABLE IF NOT EXISTS push_subscriptions (
	endpoint TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	p256dh TEXT NOT NULL,
	auth TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS push_subscriptions_user ON push_subscriptions(user_id);
CREATE TABLE IF NOT EXISTS message_search (
	seq INTEGER PRIMARY KEY,
	channel TEXT NOT NULL,
	body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_actor ON audit(actor, seq);
-- audit.owner_id (migrateAudit) gets its index there.
CREATE INDEX IF NOT EXISTS audit_resource ON audit(resource, seq);
CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts);
`

// messageText is the searchable text of a message: its body.
func messageText(m *mycelv1.Message) string { return m.GetPayload().GetText() }

func indexMessageTx(ctx context.Context, tx *sql.Tx, seq int64, m *mycelv1.Message) error {
	_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO message_search(seq, channel, body) VALUES(?,?,?)`,
		seq, m.GetChannel(), messageText(m))
	return err
}

// backfillMessageSearch indexes messages stored before the index existed.
func backfillMessageSearch(ctx context.Context, db *sql.DB) error {
	for {
		rows, err := db.QueryContext(ctx, `SELECT seq, msg FROM messages
			WHERE seq > (SELECT COALESCE(MAX(seq), 0) FROM message_search) ORDER BY seq LIMIT 500`)
		if err != nil {
			return err
		}
		type item struct {
			seq int64
			m   *mycelv1.Message
		}
		var batch []item
		for rows.Next() {
			var seq int64
			var b []byte
			if err := rows.Scan(&seq, &b); err != nil {
				_ = rows.Close()
				return err
			}
			m, err := decodeMessage(uint64(seq), b) //nolint:gosec // autoincrement is positive
			if err != nil {
				m = &mycelv1.Message{}
			}
			batch = append(batch, item{seq, m})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, it := range batch {
			if err := indexMessageTx(ctx, tx, it.seq, it.m); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

// migrateAudit adds audit.owner_id: the user who owns the actor (the user
// itself, a node's owner, an agent's owner). Rows written before it stay
// NULL and are visible to admins only.
func migrateAudit(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(audit)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		has = has || name == "owner_id"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !has {
		if _, err := db.ExecContext(ctx, `ALTER TABLE audit ADD COLUMN owner_id TEXT`); err != nil {
			return fmt.Errorf("hub: migrate audit: %w", err)
		}
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS audit_owner ON audit(owner_id, seq)`)
	return err
}

// auditOwner resolves the user who owns an audit actor ("" when none).
func (s *store) auditOwner(ctx context.Context, actor string) string {
	var q string
	switch {
	case strings.HasPrefix(actor, "usr_"):
		return actor
	case strings.HasPrefix(actor, "node:"):
		q, actor = `SELECT owner_id FROM nodes WHERE name = ?`, strings.TrimPrefix(actor, "node:")
	case strings.HasPrefix(actor, "agent:"):
		q = `SELECT owner_id FROM agents WHERE id = ?`
	default:
		return ""
	}
	var owner string
	if err := s.db.QueryRowContext(ctx, q, actor).Scan(&owner); err != nil {
		return ""
	}
	return owner
}

// changed reports a row change to the event stream (nil-safe).
func (s *store) changed(kind mycelv1.EventKind, op mycelv1.EventOp, id string, body any) {
	if s.onChange != nil {
		s.onChange(change{kind: kind, op: op, id: id, body: body})
	}
}

// changedOn reports an update of id when err is nil and returns err.
func (s *store) changedOn(kind mycelv1.EventKind, id string, err error) error {
	if err == nil {
		s.changed(kind, opUpdated, id, nil)
	}
	return err
}

// ---- audit ----

// auditOutcome maps a stored outcome to the API's wording.
func auditOutcome(o string) string {
	if o == outcomeOK {
		return "allowed"
	}
	return o
}

func (e auditEntry) proto() *mycelv1.AuditEntry {
	trace := ""
	if strings.Contains(e.Details, "trace_id") {
		var d struct {
			TraceID string `json:"trace_id"`
		}
		if json.Unmarshal([]byte(e.Details), &d) == nil {
			trace = d.TraceID
		}
	}
	return &mycelv1.AuditEntry{
		Id: strconv.FormatInt(e.Seq, 10), At: e.TS, Actor: e.Actor, Action: e.Action, Resource: e.Resource,
		Outcome: auditOutcome(e.Outcome), TraceId: trace, DetailsJson: e.Details,
	}
}

// auditQuery selects audit rows, newest first.
type auditQuery struct {
	traceID, actor, resource string
	since, until             string // RFC 3339 (inclusive, exclusive)
	before                   int64  // seq < before when > 0 (paging)
	member                   string // restrict to entries whose actor is this user, their nodes or agents
	limit                    int
}

func (s *store) queryAudit(ctx context.Context, q auditQuery) ([]auditEntry, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, a ...any) { where = append(where, cond); args = append(args, a...) }
	if q.traceID != "" {
		add(`json_extract(details, '$.trace_id') = ?`, q.traceID)
	}
	if q.actor != "" {
		add(`actor = ?`, q.actor)
	}
	if q.resource != "" {
		add(`resource = ?`, q.resource)
	}
	if q.since != "" {
		add(`ts >= ?`, q.since)
	}
	if q.until != "" {
		add(`ts < ?`, q.until)
	}
	if q.before > 0 {
		add(`seq < ?`, q.before)
	}
	if q.member != "" {
		add(`owner_id = ?`, q.member)
	}
	stmt := `SELECT seq, ts, actor, action, resource, outcome, details, COALESCE(owner_id, '') FROM audit`
	if len(where) > 0 {
		stmt += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // constant conditions with bound values
	}
	stmt += " ORDER BY seq DESC LIMIT ?"
	args = append(args, q.limit)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		if err := rows.Scan(&e.Seq, &e.TS, &e.Actor, &e.Action, &e.Resource, &e.Outcome, &e.Details, &e.OwnerID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- search ----

// likePattern makes a LIKE pattern matching q anywhere ('\' escapes).
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// searchNames returns up to limit values of col in table matching q.
func (s *store) searchNames(ctx context.Context, table, col, q string, limit int) ([]string, error) {
	//nolint:gosec // table and col are constants from search.go; the pattern is bound
	rows, err := s.db.QueryContext(ctx, `SELECT `+col+` FROM `+table+` WHERE `+col+` LIKE ? ESCAPE '\' ORDER BY `+col+` LIMIT ?`,
		likePattern(q), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// searchMessages returns the messages with seq in (hi-window, hi] whose
// body matches q, newest first: the caller walks windows downwards so one
// query never examines more than window rows of the index.
func (s *store) searchMessages(ctx context.Context, q string, hi, window int64) ([]*mycelv1.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.seq, m.msg FROM message_search x JOIN messages m ON m.seq = x.seq
		WHERE x.seq <= ? AND x.seq > ? AND x.body LIKE ? ESCAPE '\' ORDER BY x.seq DESC`, hi, hi-window, likePattern(q))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*mycelv1.Message
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, err
		}
		m, err := decodeMessage(uint64(seq), b) //nolint:gosec // autoincrement is positive
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// maxMessageSeq is the newest indexed message sequence (0 when none).
func (s *store) maxMessageSeq(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM message_search`).Scan(&n)
	return n, err
}

// ---- passkeys ----

type passkeyRow struct {
	ID         string // base64url credential id
	UserID     string
	Name       string
	Credential string // go-webauthn Credential as JSON
	CreatedAt  string
	LastUsedAt string
}

const passkeyCols = `id, user_id, name, credential, created_at, last_used_at` //nolint:gosec // column names, not a credential

func scanPasskey(sc interface{ Scan(...any) error }) (*passkeyRow, error) {
	var p passkeyRow
	if err := sc.Scan(&p.ID, &p.UserID, &p.Name, &p.Credential, &p.CreatedAt, &p.LastUsedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *store) passkeysOf(ctx context.Context, userID string) ([]*passkeyRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+passkeyCols+` FROM passkeys WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*passkeyRow
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) getPasskey(ctx context.Context, id string) (*passkeyRow, error) {
	p, err := scanPasskey(s.db.QueryRowContext(ctx, `SELECT `+passkeyCols+` FROM passkeys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return p, err
}

// insertPasskey stores a credential unless the user already has max.
func (s *store) insertPasskey(ctx context.Context, p *passkeyRow, maxPerUser int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkeys WHERE user_id = ?`, p.UserID).Scan(&n); err != nil {
			return err
		}
		if n >= maxPerUser {
			return errTooMany
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO passkeys(`+passkeyCols+`) VALUES(?,?,?,?,?,?)`,
			p.ID, p.UserID, p.Name, p.Credential, p.CreatedAt, p.LastUsedAt)
		if isUnique(err) {
			return errConflict
		}
		return err
	})
}

// usePasskey stores the credential after a login (sign count, flags).
func (s *store) usePasskey(ctx context.Context, id, credential string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE passkeys SET credential = ?, last_used_at = ? WHERE id = ?`, credential, nowString(), id)
	return err
}

// deletePasskey removes a user's passkey. It refuses the last one while the
// user has no API key (errLastCredential).
func (s *store) deletePasskey(ctx context.Context, userID, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n, keys int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkeys WHERE user_id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE user_id = ?`, userID).Scan(&keys); err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM passkeys WHERE id = ? AND user_id = ?`, id, userID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if n <= 1 && keys == 0 {
			return errLastCredential
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM passkeys WHERE id = ?`, id)
		return err
	})
}

// ---- push subscriptions ----

type pushSubRow struct {
	Endpoint, UserID, P256dh, Auth, CreatedAt string
}

// upsertPushSub stores a subscription for userID; past maxPerUser the
// user's oldest subscription is dropped.
func (s *store) upsertPushSub(ctx context.Context, p *pushSubRow, maxPerUser int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM push_subscriptions WHERE endpoint = ?`, p.Endpoint).Scan(&owner)
		if err == nil && owner != p.UserID {
			return errConflict // another user's subscription
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, p.Endpoint); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint IN (
			SELECT endpoint FROM push_subscriptions WHERE user_id = ? ORDER BY created_at DESC, endpoint DESC LIMIT -1 OFFSET ?)`,
			p.UserID, maxPerUser-1); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO push_subscriptions(endpoint, user_id, p256dh, auth, created_at) VALUES(?,?,?,?,?)`,
			p.Endpoint, p.UserID, p.P256dh, p.Auth, p.CreatedAt)
		return err
	})
}

func (s *store) deletePushSub(ctx context.Context, userID, endpoint string) (bool, error) {
	q, args := `DELETE FROM push_subscriptions WHERE endpoint = ?`, []any{endpoint}
	if userID != "" {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// pushSubsFor lists the subscriptions of the given users.
func (s *store) pushSubsFor(ctx context.Context, userIDs []string) ([]*pushSubRow, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	ph := make([]string, len(userIDs))
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		ph[i], args[i] = "?", id
	}
	//nolint:gosec // only "?" placeholders are joined in
	rows, err := s.db.QueryContext(ctx, `SELECT endpoint, user_id, p256dh, auth, created_at FROM push_subscriptions
		WHERE user_id IN (`+strings.Join(ph, ",")+`) ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*pushSubRow
	for rows.Next() {
		var p pushSubRow
		if err := rows.Scan(&p.Endpoint, &p.UserID, &p.P256dh, &p.Auth, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *store) countPushSubs(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// adminIDs lists the ids of all admins.
func (s *store) adminIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM users WHERE role = ? ORDER BY id`, roleAdmin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- grant usage ----

// grantUsageSums sums the broker usage rows of the given grants, one query
// per 500 ids; grants without reports are absent from the map.
func (s *store) grantUsageSums(ctx context.Context, ids []string) (map[string]*mycelv1.GrantUsage, error) {
	out := map[string]*mycelv1.GrantUsage{}
	for len(ids) > 0 {
		n := min(len(ids), 500)
		chunk := ids[:n]
		ids = ids[n:]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			ph[i], args[i] = "?", id
		}
		//nolint:gosec // only "?" placeholders are joined in
		rows, err := s.db.QueryContext(ctx, `SELECT grant_id, SUM(tokens), SUM(requests), SUM(gpu_seconds), MAX(updated_at)
			FROM grant_usage WHERE grant_id IN (`+strings.Join(ph, ",")+`) GROUP BY grant_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			u := &mycelv1.GrantUsage{}
			if err := rows.Scan(&id, &u.Tokens, &u.Requests, &u.GpuSeconds, &u.UpdatedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = u
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// withUsage fills Grant.usage from the summed broker reports (one query
// for the whole list).
func (s *server) withUsage(ctx context.Context, gs ...*mycelv1.Grant) error {
	ids := make([]string, 0, len(gs))
	for _, g := range gs {
		if g != nil {
			ids = append(ids, g.GetId())
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sums, err := s.st.grantUsageSums(ctx, ids)
	if err != nil {
		return s.internalError("grant usage", err)
	}
	for _, g := range gs {
		if g != nil {
			g.Usage = sums[g.GetId()]
		}
	}
	return nil
}

var (
	errTooMany        = errors.New("limit reached")
	errLastCredential = errors.New("last credential")
)
