package hub

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Channel store errors.
var (
	errVersionConflict = errors.New("blackboard version conflict")
	errBlackboardFull  = errors.New("blackboard is full")
	errBlackboardQuota = errors.New("blackboard quota exceeded")
	errChannelCap      = errors.New("too many channels")
)

// Store caps.
const (
	// maxBlackboardKeys caps the number of keys per channel blackboard.
	maxBlackboardKeys = 1024
	// maxBlackboardBytesPerOwner caps key+value bytes written by one owner's
	// principals (the user and their agents) across all blackboards.
	maxBlackboardBytesPerOwner = 16 << 20
	// maxChannelsPerAgent caps channels created by one agent.
	maxChannelsPerAgent = 64
	// maxChannelsPerOwner caps task and direct channels per owning user.
	maxChannelsPerOwner = 256
)

// channelSchema is applied after schema. Messages carry a hub-wide sequence
// (the autoincrement seq) that inbox cursors use; the msg blob is the
// protobuf Message without seq, which is filled from the column on read.
const channelSchema = `
CREATE TABLE IF NOT EXISTS channels (
	name TEXT PRIMARY KEY,
	type INTEGER NOT NULL,
	owner_id TEXT NOT NULL,
	creator TEXT NOT NULL,
	policy BLOB NOT NULL,
	created_at TEXT NOT NULL,
	archived_at TEXT,
	last_seq INTEGER NOT NULL DEFAULT 0,
	-- creator_owner: the user who owns the creator (the user itself for
	-- humans); per-owner channel caps count by it. owner_id is empty for
	-- direct channels.
	creator_owner TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS channels_creator ON channels(creator);
CREATE INDEX IF NOT EXISTS channels_creator_owner ON channels(creator_owner);-- joined_seq: the hub-wide seq at join time; inboxes only deliver later
-- messages (history is available through ListMessages and the summary).
CREATE TABLE IF NOT EXISTS channel_members (
	channel TEXT NOT NULL REFERENCES channels(name) ON DELETE CASCADE,
	member TEXT NOT NULL,
	role TEXT NOT NULL,
	joined_at TEXT NOT NULL,
	joined_seq INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(channel, member)
);
CREATE INDEX IF NOT EXISTS channel_members_member ON channel_members(member);
CREATE TABLE IF NOT EXISTS subscriptions (
	channel TEXT NOT NULL,
	agent TEXT NOT NULL,
	sub BLOB NOT NULL,
	PRIMARY KEY(channel, agent)
);
CREATE TABLE IF NOT EXISTS messages (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	channel TEXT NOT NULL,
	thread TEXT NOT NULL,
	sender TEXT NOT NULL,
	msg BLOB NOT NULL,
	ts TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_channel_seq ON messages(channel, seq);
CREATE INDEX IF NOT EXISTS messages_thread_seq ON messages(thread, seq);
CREATE TABLE IF NOT EXISTS blackboard (
	channel TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	updated_by TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	version INTEGER NOT NULL,
	owner TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(channel, key)
);
CREATE INDEX IF NOT EXISTS blackboard_owner ON blackboard(owner);
`

type channelRow struct {
	Name    string
	Type    mycelv1.ChannelType
	OwnerID string // user id
	Creator string // "user:<id>", agent id or "hub"
	// CreatorOwner is the user owning the creator; set on insert only.
	CreatorOwner string
	Policy       *mycelv1.ChannelPolicy
	CreatedAt    string
	ArchivedAt   string
	LastSeq      uint64
}

type memberRow struct {
	Channel   string
	Member    string
	Role      string
	JoinedAt  string
	JoinedSeq uint64
}

const channelCols = `name, type, owner_id, creator, policy, created_at, archived_at, last_seq`

func scanChannel(sc interface{ Scan(...any) error }) (*channelRow, error) {
	var (
		c        channelRow
		typ      int32
		pol      []byte
		archived sql.NullString
	)
	if err := sc.Scan(&c.Name, &typ, &c.OwnerID, &c.Creator, &pol, &c.CreatedAt, &archived, &c.LastSeq); err != nil {
		return nil, err
	}
	c.Type = mycelv1.ChannelType(typ)
	c.ArchivedAt = archived.String
	c.Policy = &mycelv1.ChannelPolicy{}
	if err := proto.Unmarshal(pol, c.Policy); err != nil {
		return nil, err
	}
	return &c, nil
}

func maxSeqTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (uint64, error) {
	var seq uint64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM messages`).Scan(&seq)
	return seq, err
}

// maxSeq returns the highest message seq on the hub.
func (s *store) maxSeq(ctx context.Context) (uint64, error) { return maxSeqTx(ctx, s.db) }

func insertChannelTx(ctx context.Context, tx *sql.Tx, c *channelRow, orIgnore bool) (bool, error) {
	pol, err := proto.Marshal(c.Policy)
	if err != nil {
		return false, err
	}
	verb := "INSERT"
	if orIgnore {
		verb = "INSERT OR IGNORE"
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels WHERE name = ?`, c.Name).Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists > 0 {
		if orIgnore {
			return false, nil
		}
		return false, errConflict
	}
	if err := checkChannelCapsTx(ctx, tx, c); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, verb+` INTO channels(`+channelCols+`, creator_owner) VALUES(?,?,?,?,?,?,NULL,0,?)`,
		c.Name, int32(c.Type), c.OwnerID, c.Creator, pol, c.CreatedAt, c.CreatorOwner)
	if isUnique(err) {
		return false, errConflict
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// checkChannelCapsTx enforces the creation caps: maxChannelsPerAgent
// channels created by one agent, maxChannelsPerOwner task and direct
// channels per owning user.
func checkChannelCapsTx(ctx context.Context, tx *sql.Tx, c *channelRow) error {
	if strings.HasPrefix(c.Creator, "agent:") {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels WHERE creator = ?`, c.Creator).Scan(&n); err != nil {
			return err
		}
		if n >= maxChannelsPerAgent {
			return errChannelCap
		}
	}
	if c.CreatorOwner != "" && (c.Type == mycelv1.ChannelType_CHANNEL_TYPE_TASK || c.Type == mycelv1.ChannelType_CHANNEL_TYPE_DIRECT) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels WHERE creator_owner = ? AND type IN (?, ?)`, c.CreatorOwner,
			int32(mycelv1.ChannelType_CHANNEL_TYPE_TASK), int32(mycelv1.ChannelType_CHANNEL_TYPE_DIRECT)).Scan(&n); err != nil {
			return err
		}
		if n >= maxChannelsPerOwner {
			return errChannelCap
		}
	}
	return nil
}

// hubEventInThread reports whether the hub already posted in a thread of a
// channel (hub messages in threads are hop-limit events).
func (s *store) hubEventInThread(ctx context.Context, channel, thread string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE thread = ? AND channel = ? AND sender = 'hub'`,
		thread, channel).Scan(&n)
	return n > 0, err
}

func addMemberTx(ctx context.Context, tx *sql.Tx, channel, member, role string) error {
	seq, err := maxSeqTx(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO channel_members(channel, member, role, joined_at, joined_seq) VALUES(?,?,?,?,?)
		ON CONFLICT(channel, member) DO UPDATE SET role = excluded.role`, channel, member, role, nowString(), seq)
	return err
}

// createChannel inserts a channel with its first members. errConflict if the
// name exists.
func (s *store) createChannel(ctx context.Context, c *channelRow, members []memberRow) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := insertChannelTx(ctx, tx, c, false); err != nil {
			return err
		}
		for _, m := range members {
			if err := addMemberTx(ctx, tx, c.Name, m.Member, m.Role); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureChannel creates a channel (and members) unless the name exists and
// reports whether it did.
func (s *store) ensureChannel(ctx context.Context, c *channelRow, members []memberRow) (bool, error) {
	created := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		created, err = insertChannelTx(ctx, tx, c, true)
		if err != nil || !created {
			return err
		}
		for _, m := range members {
			if err := addMemberTx(ctx, tx, c.Name, m.Member, m.Role); err != nil {
				return err
			}
		}
		return nil
	})
	return created, err
}

func (s *store) getChannel(ctx context.Context, name string) (*channelRow, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return c, err
}

func (s *store) listChannels(ctx context.Context) ([]*channelRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelCols+` FROM channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*channelRow
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *store) setChannelPolicy(ctx context.Context, name string, p *mycelv1.ChannelPolicy) error {
	b, err := proto.Marshal(p)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE channels SET policy = ? WHERE name = ?`, b, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *store) archiveChannel(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE channels SET archived_at = COALESCE(archived_at, ?) WHERE name = ?`, nowString(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func scanMember(sc interface{ Scan(...any) error }) (*memberRow, error) {
	var m memberRow
	if err := sc.Scan(&m.Channel, &m.Member, &m.Role, &m.JoinedAt, &m.JoinedSeq); err != nil {
		return nil, err
	}
	return &m, nil
}

const memberCols = `channel, member, role, joined_at, joined_seq`

func (s *store) channelMembers(ctx context.Context, channel string) ([]*memberRow, error) {
	return s.queryMembers(ctx, `SELECT `+memberCols+` FROM channel_members WHERE channel = ? ORDER BY joined_at, member`, channel)
}

// memberships lists the channels member belongs to.
func (s *store) memberships(ctx context.Context, member string) ([]*memberRow, error) {
	return s.queryMembers(ctx, `SELECT `+memberCols+` FROM channel_members WHERE member = ? ORDER BY channel`, member)
}

func (s *store) queryMembers(ctx context.Context, q string, arg string) ([]*memberRow, error) {
	rows, err := s.db.QueryContext(ctx, q, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*memberRow
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) getMember(ctx context.Context, channel, member string) (*memberRow, error) {
	m, err := scanMember(s.db.QueryRowContext(ctx, `SELECT `+memberCols+` FROM channel_members WHERE channel = ? AND member = ?`, channel, member))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return m, err
}

// addMember adds a member, or changes the role of an existing one.
func (s *store) addMember(ctx context.Context, channel, member, role string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return addMemberTx(ctx, tx, channel, member, role) })
}

// removeMember drops a member and its subscription; errNotFound if absent.
func (s *store) removeMember(ctx context.Context, channel, member string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM channel_members WHERE channel = ? AND member = ?`, channel, member)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE channel = ? AND agent = ?`, channel, member)
		return err
	})
}

func (s *store) setSubscription(ctx context.Context, channel, agent string, sub *mycelv1.Subscription) error {
	if sub == nil {
		_, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE channel = ? AND agent = ?`, channel, agent)
		return err
	}
	b, err := proto.Marshal(sub)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO subscriptions(channel, agent, sub) VALUES(?,?,?)
		ON CONFLICT(channel, agent) DO UPDATE SET sub = excluded.sub`, channel, agent, b)
	return err
}

// subscriptions returns the agent's subscription per channel.
func (s *store) subscriptions(ctx context.Context, agent string) (map[string]*mycelv1.Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel, sub FROM subscriptions WHERE agent = ?`, agent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*mycelv1.Subscription{}
	for rows.Next() {
		var (
			ch string
			b  []byte
		)
		if err := rows.Scan(&ch, &b); err != nil {
			return nil, err
		}
		sub := &mycelv1.Subscription{}
		if err := proto.Unmarshal(b, sub); err != nil {
			return nil, err
		}
		out[ch] = sub
	}
	return out, rows.Err()
}

// ---- messages ----

// insertMessage stores m, sets m.Seq and advances the channel's last_seq.
func (s *store) insertMessage(ctx context.Context, m *mycelv1.Message) error {
	m.Seq = 0
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO messages(id, channel, thread, sender, msg, ts) VALUES(?,?,?,?,?,?)`,
			m.GetId(), m.GetChannel(), m.GetThread(), m.GetFrom(), b, m.GetTs())
		if isUnique(err) {
			return errConflict
		}
		if err != nil {
			return err
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET last_seq = ? WHERE name = ?`, seq, m.GetChannel()); err != nil {
			return err
		}
		m.Seq = uint64(seq) //nolint:gosec // autoincrement is positive
		return nil
	})
}

func decodeMessage(seq uint64, b []byte) (*mycelv1.Message, error) {
	m := &mycelv1.Message{}
	if err := proto.Unmarshal(b, m); err != nil {
		return nil, err
	}
	m.Seq = seq
	return m, nil
}

func (s *store) queryMessages(ctx context.Context, q string, args ...any) ([]*mycelv1.Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*mycelv1.Message
	for rows.Next() {
		var (
			seq uint64
			b   []byte
		)
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, err
		}
		m, err := decodeMessage(seq, b)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) getMessage(ctx context.Context, id string) (*mycelv1.Message, error) {
	out, err := s.queryMessages(ctx, `SELECT seq, msg FROM messages WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errNotFound
	}
	return out[0], nil
}

// listMessages pages through a channel in seq order. With after > 0 it
// returns the first limit messages after it; otherwise the last limit
// messages (before before, when set). thread filters when non-empty.
func (s *store) listMessages(ctx context.Context, channel, thread string, after, before uint64, limit int) ([]*mycelv1.Message, error) {
	q := `SELECT seq, msg FROM messages WHERE channel = ?`
	args := []any{channel}
	if thread != "" {
		// A thread named after its first message includes that message.
		q += ` AND (thread = ? OR id = ?)`
		args = append(args, thread, thread)
	}
	if after > 0 {
		q += ` AND seq > ?`
		args = append(args, after)
	}
	if before > 0 {
		q += ` AND seq < ?`
		args = append(args, before)
	}
	if after > 0 {
		return s.queryMessages(ctx, q+` ORDER BY seq LIMIT ?`, append(args, limit)...)
	}
	out, err := s.queryMessages(ctx, q+` ORDER BY seq DESC LIMIT ?`, append(args, limit)...)
	slices.Reverse(out)
	return out, err
}

// messagesAfter returns up to limit messages of any channel with seq in
// (after, upto], in seq order.
func (s *store) messagesAfter(ctx context.Context, after, upto uint64, limit int) ([]*mycelv1.Message, error) {
	return s.queryMessages(ctx, `SELECT seq, msg FROM messages WHERE seq > ? AND seq <= ? ORDER BY seq LIMIT ?`, after, upto, limit)
}

// inboxCandidates returns up to limit messages with seq in (after, upto]
// from channels agent is a member of, posted after it joined and not sent
// by it. The caller applies the subscription filter.
func (s *store) inboxCandidates(ctx context.Context, agent string, after, upto uint64, limit int) ([]*mycelv1.Message, error) {
	return s.queryMessages(ctx, `SELECT m.seq, m.msg FROM messages m
		JOIN channel_members cm ON cm.channel = m.channel AND cm.member = ?
		WHERE m.seq > ? AND m.seq <= ? AND m.seq > cm.joined_seq AND m.sender != ?
		ORDER BY m.seq LIMIT ?`, agent, after, upto, agent, limit)
}

// ---- blackboard ----

const bbCols = `channel, key, value, updated_by, updated_at, version`

func scanBB(sc interface{ Scan(...any) error }) (*mycelv1.BlackboardEntry, error) {
	var e mycelv1.BlackboardEntry
	if err := sc.Scan(&e.Channel, &e.Key, &e.Value, &e.UpdatedBy, &e.UpdatedAt, &e.Version); err != nil {
		return nil, err
	}
	return &e, nil
}

// bbGet returns one entry (key set) or all entries of a channel, by key.
func (s *store) bbGet(ctx context.Context, channel, key string) ([]*mycelv1.BlackboardEntry, error) {
	q := `SELECT ` + bbCols + ` FROM blackboard WHERE channel = ?`
	args := []any{channel}
	if key != "" {
		q += ` AND key = ?`
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY key`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*mycelv1.BlackboardEntry
	for rows.Next() {
		e, err := scanBB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// bbPut writes an entry. expect > 0 requires the current version to equal
// it (a missing key has version 0, so expect must be 0 to create). It
// returns errVersionConflict on mismatch.
//
// owner is the user owning the writer; the bytes of all entries last written
// by an owner's principals stay within maxBlackboardBytesPerOwner
// (errBlackboardQuota).
func (s *store) bbPut(ctx context.Context, channel, key, value, by, owner string, expect uint64) (*mycelv1.BlackboardEntry, error) {
	var out *mycelv1.BlackboardEntry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var cur uint64
		err := tx.QueryRowContext(ctx, `SELECT version FROM blackboard WHERE channel = ? AND key = ?`, channel, key).Scan(&cur)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		if expect != 0 && expect != cur {
			return errVersionConflict
		}
		if missing {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blackboard WHERE channel = ?`, channel).Scan(&n); err != nil {
				return err
			}
			if n >= maxBlackboardKeys {
				return errBlackboardFull
			}
		}
		if owner != "" {
			var used int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB))), 0)
				FROM blackboard WHERE owner = ? AND NOT (channel = ? AND key = ?)`, owner, channel, key).Scan(&used); err != nil {
				return err
			}
			if used+int64(len(key)+len(value)) > maxBlackboardBytesPerOwner {
				return errBlackboardQuota
			}
		}
		out = &mycelv1.BlackboardEntry{Channel: channel, Key: key, Value: value, UpdatedBy: by, UpdatedAt: nowString(), Version: cur + 1}
		_, err = tx.ExecContext(ctx, `INSERT INTO blackboard(`+bbCols+`, owner) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(channel, key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by,
			updated_at = excluded.updated_at, version = excluded.version, owner = excluded.owner`,
			out.Channel, out.Key, out.Value, out.UpdatedBy, out.UpdatedAt, out.Version, owner)
		return err
	})
	return out, err
}

func (s *store) bbDelete(ctx context.Context, channel, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM blackboard WHERE channel = ? AND key = ?`, channel, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}
