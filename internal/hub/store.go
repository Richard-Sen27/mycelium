package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/richard-sen27/mycelium/internal/identity"
)

// Store errors.
var (
	errNotFound      = errors.New("not found")
	errConflict      = errors.New("already exists")
	errLastAdmin     = errors.New("cannot remove the last admin")
	errOwnsNodes     = errors.New("user still owns nodes; remove the user's nodes first")
	errTokenSpent    = errors.New("token already used or revoked")
	errTooManyAgents = errors.New("too many agents on this node")
)

// apiKeyPrefix marks an API key string.
const apiKeyPrefix = "mck_"

// maxAgentsPerNode caps the live agents one node may register.
const maxAgentsPerNode = 256

// apiKeyTouchInterval bounds how often last_used_at is written.
const apiKeyTouchInterval = time.Minute

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	key_hash TEXT NOT NULL UNIQUE,
	scopes TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_used_at TEXT
);
CREATE TABLE IF NOT EXISTS join_tokens (
	id TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL UNIQUE,
	node_name TEXT NOT NULL,
	caps TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at TEXT,
	revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS nodes (
	name TEXT PRIMARY KEY,
	spiffe_id TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	caps_ceiling TEXT NOT NULL,
	caps_enabled TEXT NOT NULL,
	inventory BLOB NULL,
	inventory_hash TEXT,
	last_seen_at TEXT,
	cert_serial TEXT NOT NULL,
	cert_not_after TEXT NOT NULL,
	created_at TEXT NOT NULL
);
-- Every certificate ever issued to a node, so that removing a node can revoke
-- all of its still-valid certificates (renewal does not revoke the old one).
CREATE TABLE IF NOT EXISTS node_certs (
	serial TEXT PRIMARY KEY,
	node_name TEXT NOT NULL,
	spiffe_id TEXT NOT NULL,
	not_after TEXT NOT NULL,
	issued_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS node_certs_node ON node_certs(node_name);
CREATE TABLE IF NOT EXISTS revoked_certs (
	serial TEXT PRIMARY KEY,
	spiffe_id TEXT NOT NULL,
	revoked_at TEXT NOT NULL,
	not_after TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	node_name TEXT NOT NULL,
	sandbox_id TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	harness TEXT NOT NULL,
	state TEXT NOT NULL,
	created_at TEXT NOT NULL,
	token_expires_at TEXT,
	revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS audit (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	resource TEXT NOT NULL,
	outcome TEXT NOT NULL,
	details TEXT NOT NULL
);
`

// store is the hub's SQLite state. It also implements bus.Revocation from an
// in-memory set so the bus authentication path never touches the database.
type store struct {
	db   *sql.DB
	path string

	mu         sync.RWMutex
	revSerials map[string]bool // revoked certificate serials (lowercase hex)
	revIDs     map[string]bool // SPIFFE IDs of removed nodes whose name is not re-enrolled
	revAgents  map[string]bool // revoked agent ids

	// seqMu sequences node-changing transactions (insert, enrol, remove)
	// together with their in-memory revocation updates, so a remove racing a
	// re-enrol of the same name cannot leave the sets inconsistent.
	seqMu sync.Mutex

	touchMu  sync.Mutex
	keyTouch map[string]time.Time // api key id -> last last_used_at write

	// onChange, when set, hears about every row change the UI's event
	// stream reports (events.go). It must not block or touch the store.
	onChange func(change)
}

func openStore(path string) (*store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("hub: open db: %w", err)
	}
	// One connection serialises all access. The hub's write rate is tiny and
	// this rules out SQLITE_BUSY on transaction upgrades.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + channelSchema + lifecycleSchema + jobSchema + grantSchema + uiSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hub: apply schema: %w", err)
	}
	if err := migrateAgents(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateMembers(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateNodes(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateAudit(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := backfillMessageSearch(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("hub: chmod db: %w", err)
		}
	}
	s := &store{
		db: db, path: path, revSerials: map[string]bool{}, revIDs: map[string]bool{}, revAgents: map[string]bool{},
		keyTouch: map[string]time.Time{},
	}
	if err := s.loadRevocations(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) Close() error { return s.db.Close() }

// loadRevocations prunes expired entries and fills the in-memory sets.
func (s *store) loadRevocations(ctx context.Context) error {
	now := nowString()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM revoked_certs WHERE not_after < ?`, now); err != nil {
		return fmt.Errorf("hub: prune revocations: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM node_certs WHERE not_after < ?`, now); err != nil {
		return fmt.Errorf("hub: prune node certs: %w", err)
	}
	if err := s.loadRevokedAgents(ctx); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT serial, spiffe_id, spiffe_id IN (SELECT spiffe_id FROM nodes) FROM revoked_certs`)
	if err != nil {
		return fmt.Errorf("hub: load revocations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var serial, id string
		var live bool
		if err := rows.Scan(&serial, &id, &live); err != nil {
			return err
		}
		s.revSerials[strings.ToLower(serial)] = true
		if !live {
			s.revIDs[id] = true
		}
	}
	return rows.Err()
}

func (s *store) loadRevokedAgents(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM agents WHERE revoked_at IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("hub: load revoked agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.revAgents[id] = true
	}
	return rows.Err()
}

// agentRevoked reports whether an agent id has been revoked (memory only).
func (s *store) agentRevoked(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revAgents[id]
}

// IsRevoked implements bus.Revocation.
func (s *store) IsRevoked(serialHex, spiffeID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revSerials[strings.ToLower(serialHex)] || s.revIDs[spiffeID]
}

// ---- helpers ----

func nowString() string { return formatTime(time.Now()) }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

func newID(prefix string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func encodeList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeList(s string) []string {
	var v []string
	if s == "" || json.Unmarshal([]byte(s), &v) != nil {
		return []string{}
	}
	return v
}

// nullable maps "" to SQL NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- users and API keys ----

type userRow struct {
	ID        string
	Name      string
	Role      string
	CreatedAt string
}

// createUser inserts a user and their first API key in one transaction and
// returns the key secret, which is never stored.
func (s *store) createUser(ctx context.Context, name, role string) (*userRow, *apiKeyRow, string, error) {
	id, err := newID("usr_")
	if err != nil {
		return nil, nil, "", err
	}
	u := &userRow{ID: id, Name: name, Role: role, CreatedAt: nowString()}
	var (
		k      *apiKeyRow
		secret string
	)
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO users(id, name, role, created_at) VALUES(?,?,?,?)`,
			u.ID, u.Name, u.Role, u.CreatedAt)
		if isUnique(err) {
			return errConflict
		}
		if err != nil {
			return err
		}
		k, secret, err = insertAPIKey(ctx, tx, u.ID)
		return err
	})
	if err != nil {
		return nil, nil, "", err
	}
	return u, k, secret, nil
}

func (s *store) getUser(ctx context.Context, id string) (*userRow, error) {
	var u userRow
	err := s.db.QueryRowContext(ctx, `SELECT id, name, role, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *store) listUsers(ctx context.Context) ([]*userRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, role, created_at FROM users ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// removeUser deletes a user and their API keys, and revokes their unused join
// tokens. It refuses the last admin and users that still own nodes.
func (s *store) removeUser(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, id).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if role == roleAdmin {
			var admins int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ?`, roleAdmin).Scan(&admins); err != nil {
				return err
			}
			if admins <= 1 {
				return errLastAdmin
			}
		}
		var nodes int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE owner_id = ?`, id).Scan(&nodes); err != nil {
			return err
		}
		if nodes > 0 {
			return errOwnsNodes
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE join_tokens SET revoked_at = ? WHERE owner_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
			nowString(), id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		return err
	})
}

type apiKeyRow struct {
	ID         string
	UserID     string
	Scopes     []string
	CreatedAt  string
	LastUsedAt string
}

func hashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// createAPIKey creates a key for userID and returns it with its secret, which
// is never stored.
func (s *store) createAPIKey(ctx context.Context, userID string) (*apiKeyRow, string, error) {
	return insertAPIKey(ctx, s.db, userID)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertAPIKey(ctx context.Context, db execer, userID string) (*apiKeyRow, string, error) {
	id, err := newID("key_")
	if err != nil {
		return nil, "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	secret := apiKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	k := &apiKeyRow{ID: id, UserID: userID, Scopes: []string{"user"}, CreatedAt: nowString()}
	_, err = db.ExecContext(ctx,
		`INSERT INTO api_keys(id, user_id, key_hash, scopes, created_at) VALUES(?,?,?,?,?)`,
		k.ID, k.UserID, hashAPIKey(secret), encodeList(k.Scopes), k.CreatedAt)
	if err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

// lookupAPIKey resolves a key secret to its user. Unknown keys return errNotFound.
func (s *store) lookupAPIKey(ctx context.Context, secret string) (*userRow, *apiKeyRow, error) {
	if !strings.HasPrefix(secret, apiKeyPrefix) || len(secret) > 128 {
		return nil, nil, errNotFound
	}
	h := hashAPIKey(secret)
	var (
		k      apiKeyRow
		stored string
		scopes string
		last   sql.NullString
		u      userRow
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT k.id, k.user_id, k.key_hash, k.scopes, k.created_at, k.last_used_at,
		       u.id, u.name, u.role, u.created_at
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`, h).
		Scan(&k.ID, &k.UserID, &stored, &scopes, &k.CreatedAt, &last, &u.ID, &u.Name, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, errNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if !identity.ConstantTimeEqualHex(stored, h) {
		return nil, nil, errNotFound
	}
	k.Scopes = decodeList(scopes)
	k.LastUsedAt = last.String
	s.touchAPIKey(ctx, k.ID)
	return &u, &k, nil
}

func (s *store) touchAPIKey(ctx context.Context, id string) {
	now := time.Now()
	s.touchMu.Lock()
	if t, ok := s.keyTouch[id]; ok && now.Sub(t) < apiKeyTouchInterval {
		s.touchMu.Unlock()
		return
	}
	s.keyTouch[id] = now
	s.touchMu.Unlock()
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, formatTime(now), id)
}

// ---- join tokens ----

type tokenRow struct {
	ID         string
	SecretHash string
	NodeName   string
	Caps       []string
	OwnerID    string
	CreatedAt  string
	ExpiresAt  string
	UsedAt     string
	RevokedAt  string
}

const tokenCols = `id, secret_hash, node_name, caps, owner_id, created_at, expires_at, used_at, revoked_at` //nolint:gosec // column list, not a credential

func scanToken(sc interface{ Scan(...any) error }) (*tokenRow, error) {
	var (
		t          tokenRow
		caps       string
		used, revk sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.SecretHash, &t.NodeName, &caps, &t.OwnerID, &t.CreatedAt, &t.ExpiresAt, &used, &revk); err != nil {
		return nil, err
	}
	t.Caps = decodeList(caps)
	t.UsedAt = used.String
	t.RevokedAt = revk.String
	return &t, nil
}

func (s *store) createJoinToken(ctx context.Context, t *tokenRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO join_tokens(`+tokenCols+`) VALUES(?,?,?,?,?,?,?,NULL,NULL)`,
		t.ID, t.SecretHash, t.NodeName, encodeList(t.Caps), t.OwnerID, t.CreatedAt, t.ExpiresAt)
	if isUnique(err) {
		return errConflict
	}
	return err
}

func (s *store) getJoinToken(ctx context.Context, id string) (*tokenRow, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM join_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return t, err
}

func (s *store) joinTokenBySecretHash(ctx context.Context, h string) (*tokenRow, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM join_tokens WHERE secret_hash = ?`, h))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if !identity.ConstantTimeEqualHex(t.SecretHash, h) {
		return nil, errNotFound
	}
	return t, nil
}

func (s *store) listJoinTokens(ctx context.Context) ([]*tokenRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM join_tokens ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*tokenRow
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *store) revokeJoinToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, nowString(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.getJoinToken(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// ---- nodes ----

type nodeRow struct {
	Name          string
	SPIFFEID      string
	OwnerID       string
	CapsCeiling   []string
	CapsEnabled   []string
	Inventory     []byte
	InventoryHash string
	LastSeenAt    string
	CertSerial    string
	CertNotAfter  string
	CreatedAt     string
}

const nodeCols = `name, spiffe_id, owner_id, caps_ceiling, caps_enabled, inventory, inventory_hash,
	last_seen_at, cert_serial, cert_not_after, created_at`

func scanNode(sc interface{ Scan(...any) error }) (*nodeRow, error) {
	var (
		n             nodeRow
		ceil, enabled string
		invHash, seen sql.NullString
	)
	if err := sc.Scan(&n.Name, &n.SPIFFEID, &n.OwnerID, &ceil, &enabled, &n.Inventory, &invHash,
		&seen, &n.CertSerial, &n.CertNotAfter, &n.CreatedAt); err != nil {
		return nil, err
	}
	n.CapsCeiling = decodeList(ceil)
	n.CapsEnabled = decodeList(enabled)
	n.InventoryHash = invHash.String
	n.LastSeenAt = seen.String
	return &n, nil
}

func insertNodeTx(ctx context.Context, tx *sql.Tx, n *nodeRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO nodes(`+nodeCols+`) VALUES(?,?,?,?,?,NULL,NULL,NULL,?,?,?)`,
		n.Name, n.SPIFFEID, n.OwnerID, encodeList(n.CapsCeiling), encodeList(n.CapsEnabled),
		n.CertSerial, n.CertNotAfter, n.CreatedAt)
	if isUnique(err) {
		return errConflict
	}
	if err != nil {
		return err
	}
	// A fresh incarnation: jobs assigned to an earlier node of this name
	// never match it (see jobForNode).
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET incarnation = ? WHERE name = ?`, newIncarnation(), n.Name); err != nil {
		return err
	}
	return recordCertTx(ctx, tx, n.Name, n.SPIFFEID, n.CertSerial, n.CertNotAfter)
}

func recordCertTx(ctx context.Context, tx *sql.Tx, name, spiffeID, serial, notAfter string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO node_certs(serial, node_name, spiffe_id, not_after, issued_at) VALUES(?,?,?,?,?)`,
		strings.ToLower(serial), name, spiffeID, notAfter, nowString())
	return err
}

// insertNode adds a node (used by Init for the local node).
func (s *store) insertNode(ctx context.Context, n *nodeRow) error {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if err := s.inTx(ctx, func(tx *sql.Tx) error { return insertNodeTx(ctx, tx, n) }); err != nil {
		return err
	}
	s.unrevokeID(n.SPIFFEID)
	s.changed(kindNode, opCreated, n.Name, nil)
	return nil
}

// enrolNode burns the join token and inserts the node in one transaction.
// The burn is conditional, so two concurrent enrolments with one token cannot
// both succeed.
func (s *store) enrolNode(ctx context.Context, tokenID string, n *nodeRow) error {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE join_tokens SET used_at = ?
			WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
			nowString(), tokenID, nowString())
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c != 1 {
			return errTokenSpent
		}
		return insertNodeTx(ctx, tx, n)
	})
	if err != nil {
		return err
	}
	s.unrevokeID(n.SPIFFEID)
	s.changed(kindNode, opCreated, n.Name, nil)
	return nil
}

// unrevokeID clears the SPIFFE-ID revocation when a name is enrolled again.
// The certificates of the earlier node with that name stay revoked by serial.
func (s *store) unrevokeID(id string) {
	s.mu.Lock()
	delete(s.revIDs, id)
	s.mu.Unlock()
}

func (s *store) getNode(ctx context.Context, name string) (*nodeRow, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return n, err
}

func (s *store) nodeExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *store) listNodes(ctx context.Context) ([]*nodeRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*nodeRow
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// removeNode deletes the node and its agents, revokes every certificate
// ever issued to it that has not expired yet, and fails its jobs (assigned
// ones and queued ones pinned to it); it returns the failed job ids.
func (s *store) removeNode(ctx context.Context, name string) (*nodeRow, []string, error) {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	var n *nodeRow
	var serials, agents, jobs []string
	now := time.Now()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE name = ?`, name))
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		certs := map[string]string{strings.ToLower(n.CertSerial): n.CertNotAfter}
		rows, err := tx.QueryContext(ctx, `SELECT serial, not_after FROM node_certs WHERE node_name = ?`, name)
		if err != nil {
			return err
		}
		for rows.Next() {
			var serial, notAfter string
			if err := rows.Scan(&serial, &notAfter); err != nil {
				_ = rows.Close()
				return err
			}
			certs[strings.ToLower(serial)] = notAfter
		}
		if err := rows.Close(); err != nil {
			return err
		}
		revokedAt := formatTime(now)
		for serial, notAfter := range certs {
			if t, err := parseTime(notAfter); err == nil && t.Before(now) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO revoked_certs(serial, spiffe_id, revoked_at, not_after)
				VALUES(?,?,?,?)`, serial, n.SPIFFEID, revokedAt, notAfter); err != nil {
				return err
			}
			serials = append(serials, serial)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM node_certs WHERE node_name = ?`, name); err != nil {
			return err
		}
		// Agents are kept, marked revoked, so their ids stay refused.
		arows, err := tx.QueryContext(ctx, `SELECT id FROM agents WHERE node_name = ?`, name)
		if err != nil {
			return err
		}
		for arows.Next() {
			var id string
			if err := arows.Scan(&id); err != nil {
				_ = arows.Close()
				return err
			}
			agents = append(agents, id)
		}
		if err := arows.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET revoked_at = COALESCE(revoked_at, ?), state = 'stopped',
			paused_reason = '' WHERE node_name = ?`, revokedAt, name); err != nil {
			return err
		}
		if jobs, err = failNodeJobsTx(ctx, tx, name, "node removed"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM nodes WHERE name = ?`, name)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	for _, serial := range serials {
		s.revSerials[serial] = true
	}
	s.revIDs[n.SPIFFEID] = true
	for _, id := range agents {
		s.revAgents[id] = true
	}
	s.mu.Unlock()
	s.changed(kindNode, opDeleted, n.Name, n)
	for _, id := range agents {
		s.changed(kindAgent, opUpdated, id, nil)
	}
	for _, id := range jobs {
		s.changed(kindJob, opUpdated, id, nil)
	}
	return n, jobs, nil
}

func (s *store) setNodeCaps(ctx context.Context, name string, caps []string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET caps_enabled = ? WHERE name = ?`, encodeList(caps), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	s.changed(kindNode, opUpdated, name, nil)
	return nil
}

func (s *store) renewNodeCert(ctx context.Context, name, serial, notAfter string) error {
	return s.changedOn(kindNode, name, s.inTx(ctx, func(tx *sql.Tx) error {
		var spiffeID string
		err := tx.QueryRowContext(ctx, `SELECT spiffe_id FROM nodes WHERE name = ?`, name).Scan(&spiffeID)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET cert_serial = ?, cert_not_after = ? WHERE name = ?`,
			strings.ToLower(serial), notAfter, name); err != nil {
			return err
		}
		return recordCertTx(ctx, tx, name, spiffeID, serial, notAfter)
	}))
}

// touchNode records a heartbeat. inv is nil when the inventory is unchanged.
func (s *store) touchNode(ctx context.Context, name, seen string, inv []byte, invHash string) error {
	if inv != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE nodes SET last_seen_at = ?, inventory = ?, inventory_hash = ? WHERE name = ?`,
			seen, inv, invHash, name)
		return s.changedOn(kindNode, name, err)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET last_seen_at = ? WHERE name = ?`, seen, name)
	return err
}

// ---- agents ----

type agentRow struct {
	ID             string
	Name           string
	NodeName       string
	SandboxID      string
	OwnerID        string
	Harness        string
	State          string
	CreatedAt      string
	TokenExpiresAt string
	RevokedAt      string
	// Phase 3.
	Parent       string
	HomeChannel  string
	BudgetTokens int64
	BudgetUSD    float64
	UsedTokens   int64
	UsedUSD      float64
	Turns        int64
	PausedReason string
	ExitedAt     string
	ExitCode     int64
	Workdir      string
	BudgetSetBy  string // "node" or "operator"
}

const agentCols = `id, name, node_name, sandbox_id, owner_id, harness, state, created_at, token_expires_at, revoked_at,
	parent, home_channel, budget_tokens, budget_usd, used_tokens, used_usd, turns, paused_reason, exited_at, exit_code, workdir,
	budget_set_by`

func scanAgent(sc interface{ Scan(...any) error }) (*agentRow, error) {
	var (
		a                  agentRow
		exp, revkd, exited sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.Name, &a.NodeName, &a.SandboxID, &a.OwnerID, &a.Harness, &a.State,
		&a.CreatedAt, &exp, &revkd, &a.Parent, &a.HomeChannel, &a.BudgetTokens, &a.BudgetUSD,
		&a.UsedTokens, &a.UsedUSD, &a.Turns, &a.PausedReason, &exited, &a.ExitCode, &a.Workdir, &a.BudgetSetBy); err != nil {
		return nil, err
	}
	a.TokenExpiresAt = exp.String
	a.RevokedAt = revkd.String
	a.ExitedAt = exited.String
	return &a, nil
}

func (s *store) getAgent(ctx context.Context, id string) (*agentRow, error) {
	return getAgentQ(ctx, s.db, id)
}

func getAgentQ(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*agentRow, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return a, err
}

// upsertAgent registers an agent or re-registers an existing, unrevoked one.
// A revoked agent id stays revoked. A re-registration starts a new run
// (exit fields cleared) and keeps the usage counters, the parent and the
// home channel; a stored pause reason makes the agent paused again; the
// stored budget stays unless the node asks for a stricter one. Only running
// and paused agents count towards the per-node cap. It reports whether the
// agent was new and returns the stored row.
func (s *store) upsertAgent(ctx context.Context, a *agentRow) (bool, *agentRow, error) {
	created := false
	var out *agentRow
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		capCheck := func() error {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE node_name = ? AND revoked_at IS NULL
				AND state IN ('running', 'paused')`, a.NodeName).Scan(&count); err != nil {
				return err
			}
			if count >= maxAgentsPerNode {
				return errTooManyAgents
			}
			return nil
		}
		prev, err := getAgentQ(ctx, tx, a.ID)
		switch {
		case errors.Is(err, errNotFound):
			if err := capCheck(); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO agents(`+agentCols+`) VALUES(?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,0,0,0,?,NULL,0,?,'node')`,
				a.ID, a.Name, a.NodeName, a.SandboxID, a.OwnerID, a.Harness, a.State, a.CreatedAt, nullable(a.TokenExpiresAt),
				a.Parent, a.HomeChannel, a.BudgetTokens, a.BudgetUSD, a.PausedReason, a.Workdir)
			if err != nil {
				return err
			}
			created = true
		case err != nil:
			return err
		case prev.RevokedAt != "":
			return errTokenSpent
		default:
			if prev.State == stateStopped {
				if err := capCheck(); err != nil {
					return err
				}
			}
			state, reason := a.State, a.PausedReason
			if prev.PausedReason != "" {
				state, reason = statePaused, prev.PausedReason
			}
			bt, bu := stricterTokens(prev.BudgetTokens, a.BudgetTokens), stricterUSD(prev.BudgetUSD, a.BudgetUSD)
			setBy := prev.BudgetSetBy
			if bt != prev.BudgetTokens || bu != prev.BudgetUSD {
				setBy = "node"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET sandbox_id = ?, owner_id = ?, harness = ?, state = ?,
				paused_reason = ?, token_expires_at = ?, budget_tokens = ?, budget_usd = ?, budget_set_by = ?, workdir = ?,
				exited_at = NULL, exit_code = 0 WHERE id = ?`,
				a.SandboxID, a.OwnerID, a.Harness, state, reason, nullable(a.TokenExpiresAt), bt, bu, setBy, a.Workdir, a.ID); err != nil {
				return err
			}
		}
		out, err = getAgentQ(ctx, tx, a.ID)
		return err
	})
	if err == nil {
		op := opUpdated
		if created {
			op = opCreated
		}
		s.changed(kindAgent, op, a.ID, nil)
	}
	return created, out, err
}

// stricterTokens is the stricter of two token ceilings (0 = none).
func stricterTokens(old, req int64) int64 {
	if req <= 0 {
		return old
	}
	if old <= 0 {
		return req
	}
	return min(old, req)
}

// stricterUSD is the stricter of two USD ceilings (0 = none).
func stricterUSD(old, req float64) float64 {
	if req <= 0 {
		return old
	}
	if old <= 0 {
		return req
	}
	return min(old, req)
}

func (s *store) setAgentTokenExpiry(ctx context.Context, id, exp string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET token_expires_at = ? WHERE id = ?`, exp, id)
	return s.changedOn(kindAgent, id, err)
}

func (s *store) listAgents(ctx context.Context) ([]*agentRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY node_name, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*agentRow
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *store) revokeAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET revoked_at = COALESCE(revoked_at, ?), state = 'stopped', paused_reason = ''
		WHERE id = ?`,
		nowString(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	s.mu.Lock()
	s.revAgents[id] = true
	s.mu.Unlock()
	s.changed(kindAgent, opUpdated, id, nil)
	return nil
}

// ---- audit ----

type auditEntry struct {
	Seq      int64
	TS       string
	Actor    string
	Action   string
	Resource string
	Outcome  string
	Details  string
	OwnerID  string // the user who owns the actor; "" = admins only (phase 6)
}

func (s *store) appendAudit(ctx context.Context, actor, action, resource, outcome string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	ts := nowString()
	owner := s.auditOwner(ctx, actor)
	res, err := s.db.ExecContext(ctx, `INSERT INTO audit(ts, actor, action, resource, outcome, details, owner_id) VALUES(?,?,?,?,?,?,?)`,
		ts, actor, action, resource, outcome, string(b), nullable(owner))
	if err != nil {
		return err
	}
	if seq, err := res.LastInsertId(); err == nil {
		e := auditEntry{Seq: seq, TS: ts, Actor: actor, Action: action, Resource: resource, Outcome: outcome, Details: string(b), OwnerID: owner}
		s.changed(kindAudit, opCreated, strconv.FormatInt(seq, 10), auditBody{entry: e.proto(), owner: owner})
	}
	return nil
}

func (s *store) listAudit(ctx context.Context) ([]auditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, ts, actor, action, resource, outcome, details FROM audit ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		if err := rows.Scan(&e.Seq, &e.TS, &e.Actor, &e.Action, &e.Resource, &e.Outcome, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
