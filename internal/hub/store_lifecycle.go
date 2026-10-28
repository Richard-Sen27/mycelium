package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Agent states.
const (
	stateRunning = "running"
	statePaused  = "paused"
	stateStopped = "stopped"
)

// lifecycleSchema holds the phase 3 tables. The agents columns are added by
// migrateAgents because the table predates them.
const lifecycleSchema = `
CREATE TABLE IF NOT EXISTS channel_usage (
	channel TEXT PRIMARY KEY,
	used_tokens INTEGER NOT NULL DEFAULT 0,
	used_usd REAL NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
`

// agentColumns are the phase 3 columns of agents with their definitions.
var agentColumns = []struct{ name, def string }{
	{"parent", "TEXT NOT NULL DEFAULT ''"},
	{"home_channel", "TEXT NOT NULL DEFAULT ''"},
	{"budget_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"budget_usd", "REAL NOT NULL DEFAULT 0"},
	{"used_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"used_usd", "REAL NOT NULL DEFAULT 0"},
	{"turns", "INTEGER NOT NULL DEFAULT 0"},
	{"paused_reason", "TEXT NOT NULL DEFAULT ''"},
	{"exited_at", "TEXT"},
	{"exit_code", "INTEGER NOT NULL DEFAULT 0"},
	{"workdir", "TEXT NOT NULL DEFAULT ''"},
	{"budget_set_by", "TEXT NOT NULL DEFAULT ''"},
}

// migrateAgents adds the phase 3 columns that are missing (idempotent).
func migrateAgents(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(agents)`)
	if err != nil {
		return fmt.Errorf("hub: inspect agents: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("hub: inspect agents: %w", err)
		}
		have[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range agentColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE agents ADD COLUMN `+c.name+` `+c.def); err != nil {
			return fmt.Errorf("hub: add agents.%s: %w", c.name, err)
		}
	}
	return nil
}

var errAgentNotLive = errors.New("agent not live")

// usageRow is a channel's accumulated usage.
type usageRow struct {
	Channel    string
	UsedTokens int64
	UsedUSD    float64
}

// addUsage adds increments to an agent reported by node and to its home
// channel, in one transaction. It returns the updated agent and, when the
// agent has a home channel, the channel's usage. errNotFound when the agent
// is unknown or belongs to another node, errAgentNotLive when it is revoked.
func (s *store) addUsage(ctx context.Context, node, id string, tokens int64, usd float64, turns int64) (*agentRow, *usageRow, error) {
	var (
		a *agentRow
		u *usageRow
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		a, err = getAgentQ(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.NodeName != node {
			return errNotFound
		}
		if a.RevokedAt != "" {
			return errAgentNotLive
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET used_tokens = used_tokens + ?, used_usd = used_usd + ?,
			turns = turns + ? WHERE id = ?`, tokens, usd, turns, id); err != nil {
			return err
		}
		a.UsedTokens += tokens
		a.UsedUSD += usd
		a.Turns += turns
		if a.HomeChannel == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO channel_usage(channel, used_tokens, used_usd, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(channel) DO UPDATE SET used_tokens = used_tokens + excluded.used_tokens,
			used_usd = used_usd + excluded.used_usd, updated_at = excluded.updated_at`,
			a.HomeChannel, tokens, usd, nowString()); err != nil {
			return err
		}
		u, err = channelUsageQ(ctx, tx, a.HomeChannel)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return a, u, nil
}

func channelUsageQ(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, channel string) (*usageRow, error) {
	u := &usageRow{Channel: channel}
	err := q.QueryRowContext(ctx, `SELECT used_tokens, used_usd FROM channel_usage WHERE channel = ?`, channel).
		Scan(&u.UsedTokens, &u.UsedUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return u, nil
	}
	return u, err
}

// channelUsage returns a channel's usage (zero when nothing was reported).
func (s *store) channelUsage(ctx context.Context, channel string) (*usageRow, error) {
	return channelUsageQ(ctx, s.db, channel)
}

// resetChannelUsage zeroes a channel's counters.
func (s *store) resetChannelUsage(ctx context.Context, channel string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_usage WHERE channel = ?`, channel)
	return err
}

// setAgentState sets state and paused reason of an unrevoked agent in one
// statement. It reports whether a row changed (false once revoked).
func (s *store) setAgentState(ctx context.Context, id, state, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET state = ?, paused_reason = ? WHERE id = ? AND revoked_at IS NULL`,
		state, reason, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// setAgentBudget stores an operator-set budget.
func (s *store) setAgentBudget(ctx context.Context, id string, tokens int64, usd float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET budget_tokens = ?, budget_usd = ?, budget_set_by = 'operator'
		WHERE id = ? AND revoked_at IS NULL`, tokens, usd, id)
	return err
}

// setAgentExited records the end of an agent's run: stopped, with any pause
// reason kept so that a pause survives the next registration.
func (s *store) setAgentExited(ctx context.Context, id string, code int32) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET state = 'stopped', exited_at = ?, exit_code = ?
		WHERE id = ? AND revoked_at IS NULL`, nowString(), code, id)
	return err
}

// agentsByHome lists the unrevoked agents whose home channel is channel.
func (s *store) agentsByHome(ctx context.Context, channel string) ([]*agentRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE home_channel = ? AND revoked_at IS NULL
		ORDER BY id`, channel)
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
