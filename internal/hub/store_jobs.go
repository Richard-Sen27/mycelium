package hub

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// jobSchema holds profiles, jobs and artifacts (phase 4). Job states are
// stored as the JobState enum number.
const jobSchema = `
CREATE TABLE IF NOT EXISTS profiles (
	name TEXT NOT NULL,
	version INTEGER NOT NULL,
	profile BLOB NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (name, version)
);
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	spec BLOB NOT NULL,
	state INTEGER NOT NULL,
	owner_id TEXT NOT NULL,
	submitted_by TEXT NOT NULL,
	node TEXT NOT NULL DEFAULT '',
	backend TEXT NOT NULL DEFAULT '',
	sandbox_id TEXT NOT NULL DEFAULT '',
	exit_code INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	stats BLOB,
	created_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT,
	log_bytes INTEGER NOT NULL DEFAULT 0,
	log_seq INTEGER NOT NULL DEFAULT 0,
	profile_version INTEGER NOT NULL DEFAULT 0,
	cancel_requested_at TEXT,
	assigned_at TEXT,
	pin TEXT NOT NULL DEFAULT '',
	node_incarnation TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state);
CREATE INDEX IF NOT EXISTS jobs_owner ON jobs(owner_id);
CREATE INDEX IF NOT EXISTS jobs_node ON jobs(node, state);
CREATE INDEX IF NOT EXISTS jobs_pin ON jobs(pin, state);
CREATE INDEX IF NOT EXISTS jobs_finished ON jobs(finished_at);
CREATE TABLE IF NOT EXISTS job_artifacts (
	job_id TEXT NOT NULL,
	artifact_id TEXT NOT NULL,
	PRIMARY KEY (job_id, artifact_id)
);
CREATE TABLE IF NOT EXISTS artifacts (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	media_type TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	created_by TEXT NOT NULL,
	job_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS artifacts_owner ON artifacts(owner_id);
CREATE INDEX IF NOT EXISTS artifacts_job ON artifacts(job_id);
CREATE INDEX IF NOT EXISTS artifacts_channel ON artifacts(channel);
`

// newIncarnation is a random id for one enrolment of a node name.
func newIncarnation() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// migrateNodes adds nodes.incarnation (phase 4) and gives every node one.
func migrateNodes(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(nodes)`)
	if err != nil {
		return fmt.Errorf("hub: inspect nodes: %w", err)
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
			return fmt.Errorf("hub: inspect nodes: %w", err)
		}
		have = have || name == "incarnation"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !have {
		if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN incarnation TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("hub: add nodes.incarnation: %w", err)
		}
	}
	_, err = db.ExecContext(ctx, `UPDATE nodes SET incarnation = lower(hex(randomblob(8))) WHERE incarnation = ''`)
	return err
}

// nodeIncarnation returns the current incarnation of a node name.
func (s *store) nodeIncarnation(ctx context.Context, name string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT incarnation FROM nodes WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNotFound
	}
	return v, err
}

// errStateChanged is returned by a conditional job update whose job is no
// longer in one of the expected states.
var errStateChanged = errors.New("job state changed")

// ---- profiles ----

// insertProfile stores a profile version unless that (name, version) is
// already stored; it reports whether it inserted.
func (s *store) insertProfile(ctx context.Context, p *mycelv1.Profile) (bool, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO profiles(name, version, profile, created_at) VALUES(?,?,?,?)`,
		p.GetName(), p.GetVersion(), b, nowString())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// latestProfiles returns the newest version of every stored profile, by name.
func (s *store) latestProfiles(ctx context.Context) ([]*mycelv1.Profile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.profile FROM profiles p
		JOIN (SELECT name, MAX(version) AS v FROM profiles GROUP BY name) m ON p.name = m.name AND p.version = m.v
		ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*mycelv1.Profile
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var p mycelv1.Profile
		if err := proto.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// getProfile returns one stored profile version.
func (s *store) getProfile(ctx context.Context, name string, version int32) (*mycelv1.Profile, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT profile FROM profiles WHERE name = ? AND version = ?`, name, version).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var p mycelv1.Profile
	if err := proto.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---- jobs ----

type jobRow struct {
	ID                string
	Spec              *mycelv1.JobSpec
	State             mycelv1.JobState
	OwnerID           string
	SubmittedBy       string
	Node              string
	Backend           string
	SandboxID         string
	ExitCode          int32
	Error             string
	Stats             *mycelv1.JobStats
	CreatedAt         string
	StartedAt         string
	FinishedAt        string
	LogBytes          uint64
	LogSeq            uint64
	ProfileVersion    int32
	CancelRequestedAt string
	AssignedAt        string // when the hub sent CreateSandbox
	NodeIncarnation   string // the assigned node's incarnation (nodes.incarnation)
}

// finished reports whether the job reached a final state.
func (j *jobRow) finished() bool { return jobFinal(j.State) }

func jobFinal(st mycelv1.JobState) bool {
	switch st {
	case mycelv1.JobState_JOB_STATE_SUCCEEDED, mycelv1.JobState_JOB_STATE_FAILED,
		mycelv1.JobState_JOB_STATE_CANCELLED, mycelv1.JobState_JOB_STATE_TIMED_OUT:
		return true
	}
	return false
}

const jobCols = `id, spec, state, owner_id, submitted_by, node, backend, sandbox_id, exit_code, error, stats,
	created_at, started_at, finished_at, log_bytes, log_seq, profile_version, cancel_requested_at, assigned_at, node_incarnation`

func scanJob(sc interface{ Scan(...any) error }) (*jobRow, error) {
	var (
		j                         jobRow
		spec, stats               []byte
		state                     int32
		started, finished, cancel sql.NullString
		assigned                  sql.NullString
		logBytes, logSeq          int64
	)
	if err := sc.Scan(&j.ID, &spec, &state, &j.OwnerID, &j.SubmittedBy, &j.Node, &j.Backend, &j.SandboxID,
		&j.ExitCode, &j.Error, &stats, &j.CreatedAt, &started, &finished, &logBytes, &logSeq,
		&j.ProfileVersion, &cancel, &assigned, &j.NodeIncarnation); err != nil {
		return nil, err
	}
	j.Spec = &mycelv1.JobSpec{}
	if err := proto.Unmarshal(spec, j.Spec); err != nil {
		return nil, err
	}
	if len(stats) > 0 {
		j.Stats = &mycelv1.JobStats{}
		if err := proto.Unmarshal(stats, j.Stats); err != nil {
			return nil, err
		}
	}
	j.State = mycelv1.JobState(state)
	j.StartedAt, j.FinishedAt, j.CancelRequestedAt, j.AssignedAt = started.String, finished.String, cancel.String, assigned.String
	j.LogBytes, j.LogSeq = uint64(max(logBytes, 0)), uint64(max(logSeq, 0)) //nolint:gosec // clamped to >= 0
	return &j, nil
}

func (s *store) insertJob(ctx context.Context, j *jobRow) error {
	spec, err := proto.Marshal(j.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO jobs(id, spec, state, owner_id, submitted_by, created_at, profile_version, pin)
		VALUES(?,?,?,?,?,?,?,?)`, j.ID, spec, int32(j.State), j.OwnerID, j.SubmittedBy, j.CreatedAt, j.ProfileVersion, j.Spec.GetNode())
	if isUnique(err) {
		return errConflict
	}
	return err
}

func (s *store) getJob(ctx context.Context, id string) (*jobRow, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return j, err
}

func (s *store) queryJobs(ctx context.Context, q string, args ...any) ([]*jobRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs `+q, args...) //nolint:gosec // q is a constant clause from this file; values are bound
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*jobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// activeStates are the states of a job that is not finished.
var activeStates = []mycelv1.JobState{
	mycelv1.JobState_JOB_STATE_QUEUED, mycelv1.JobState_JOB_STATE_STARTING, mycelv1.JobState_JOB_STATE_RUNNING,
}

// listJobs lists jobs newest first; owner "" means every owner.
func (s *store) listJobs(ctx context.Context, owner string, includeFinished bool, limit int) ([]*jobRow, error) {
	var where []string
	var args []any
	if owner != "" {
		where = append(where, "owner_id = ?")
		args = append(args, owner)
	}
	if !includeFinished {
		where = append(where, "state IN (?,?,?)")
		for _, st := range activeStates {
			args = append(args, int32(st))
		}
	}
	q := ""
	if len(where) > 0 {
		q = "WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)
	return s.queryJobs(ctx, q+" ORDER BY created_at DESC, id DESC LIMIT ?", args...)
}

// jobsInState lists jobs in one state, oldest first.
func (s *store) jobsInState(ctx context.Context, st mycelv1.JobState, limit int) ([]*jobRow, error) {
	return s.queryJobs(ctx, `WHERE state = ? ORDER BY created_at, id LIMIT ?`, int32(st), limit)
}

// jobIDsSubmittedBy lists the ids of jobs submitted by id (an agent).
func (s *store) jobIDsSubmittedBy(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE submitted_by = ?`, id)
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

// unfinishedJobs lists QUEUED, STARTING and RUNNING jobs.
func (s *store) unfinishedJobs(ctx context.Context) ([]*jobRow, error) {
	return s.queryJobs(ctx, `WHERE state IN (?,?,?) ORDER BY created_at, id`,
		int32(activeStates[0]), int32(activeStates[1]), int32(activeStates[2]))
}

// requeueUnstarted moves STARTING jobs that never started back to QUEUED
// (at hub start: their CreateSandbox may never have left).
func (s *store) requeueUnstarted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, node = '', node_incarnation = '', assigned_at = NULL
		WHERE state = ? AND started_at IS NULL`, int32(mycelv1.JobState_JOB_STATE_QUEUED), int32(mycelv1.JobState_JOB_STATE_STARTING))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// failNodeJobsTx ends every STARTING or RUNNING job on node and every
// queued job pinned to it with errText; it returns their ids.
func failNodeJobsTx(ctx context.Context, tx *sql.Tx, node, errText string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM jobs WHERE (node = ? AND state IN (?,?)) OR (pin = ? AND state = ?)`,
		node, int32(mycelv1.JobState_JOB_STATE_STARTING), int32(mycelv1.JobState_JOB_STATE_RUNNING),
		node, int32(mycelv1.JobState_JOB_STATE_QUEUED))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, error = ?, finished_at = ? WHERE id = ? AND state IN (?,?,?)`,
			int32(mycelv1.JobState_JOB_STATE_FAILED), errText, nowNano(), id,
			int32(activeStates[0]), int32(activeStates[1]), int32(activeStates[2])); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// prunableJobs lists finished jobs to forget, oldest first: finished
// before cutoff, or beyond the newest keep finished jobs.
func (s *store) prunableJobs(ctx context.Context, cutoff string, keep int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE finished_at IS NOT NULL AND (finished_at < ? OR id NOT IN
		(SELECT id FROM jobs WHERE finished_at IS NOT NULL ORDER BY finished_at DESC, id DESC LIMIT ?))
		ORDER BY finished_at, id LIMIT 1000`, cutoff, keep)
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

// deleteJob forgets a finished job (its artifacts stay).
func (s *store) deleteJob(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_artifacts WHERE job_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ? AND finished_at IS NOT NULL`, id)
		return err
	})
}

// assignedJobs lists STARTING and RUNNING jobs.
func (s *store) assignedJobs(ctx context.Context) ([]*jobRow, error) {
	return s.queryJobs(ctx, `WHERE state IN (?,?) ORDER BY created_at, id`,
		int32(mycelv1.JobState_JOB_STATE_STARTING), int32(mycelv1.JobState_JOB_STATE_RUNNING))
}

// runningPerNode counts STARTING and RUNNING jobs per node.
func (s *store) runningPerNode(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node, COUNT(*) FROM jobs WHERE state IN (?,?) GROUP BY node`,
		int32(mycelv1.JobState_JOB_STATE_STARTING), int32(mycelv1.JobState_JOB_STATE_RUNNING))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var n string
		var c int
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

// activeJobsOf counts the unfinished jobs of an owner.
func (s *store) activeJobsOf(ctx context.Context, owner string) (int, error) {
	var c int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE owner_id = ? AND state IN (?,?,?)`,
		owner, int32(activeStates[0]), int32(activeStates[1]), int32(activeStates[2])).Scan(&c)
	return c, err
}

// updateJob runs "UPDATE jobs SET <set>" with setArgs on job id, applied
// only while the job is in one of from; errStateChanged otherwise.
func (s *store) updateJob(ctx context.Context, id string, from []mycelv1.JobState, set string, setArgs ...any) error {
	ph := make([]string, len(from))
	args := append(append([]any{}, setArgs...), id)
	for i, st := range from {
		ph[i] = "?"
		args = append(args, int32(st))
	}
	//nolint:gosec // set is a constant clause from this file; values are bound
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET `+set+` WHERE id = ? AND state IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errStateChanged
	}
	return nil
}

// setLog records the stored log length and sequence of a job.
func (s *store) setLog(ctx context.Context, id string, bytes, seq uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET log_bytes = ?, log_seq = ? WHERE id = ?`, int64(bytes), int64(seq), id) //nolint:gosec // bounded by the log cap
	return err
}

// finishJob moves a job to a final state with its result and records its
// artifacts, in one transaction.
func (s *store) finishJob(ctx context.Context, id string, from []mycelv1.JobState, state mycelv1.JobState,
	exit int32, errText string, stats *mycelv1.JobStats, artifacts []string) error {
	var statsB []byte
	if stats != nil {
		b, err := proto.Marshal(stats)
		if err != nil {
			return err
		}
		statsB = b
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		ph := make([]string, len(from))
		args := []any{int32(state), exit, errText, statsB, nowNano(), id}
		for i, st := range from {
			ph[i] = "?"
			args = append(args, int32(st))
		}
		//nolint:gosec // only "?" placeholders are joined in; values are bound
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, exit_code = ?, error = ?, stats = ?, finished_at = ?
			WHERE id = ? AND state IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errStateChanged
		}
		for _, a := range artifacts {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO job_artifacts(job_id, artifact_id) VALUES(?,?)`, id, a); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *store) jobArtifacts(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT artifact_id FROM job_artifacts WHERE job_id = ? ORDER BY artifact_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- artifacts ----

type artifactRow struct {
	ID        string
	Name      string
	Size      int64
	SHA256    string
	MediaType string
	OwnerID   string
	CreatedBy string
	JobID     string
	Channel   string
	Path      string // relative to the hub directory
	CreatedAt string
}

const artifactCols = `id, name, size, sha256, media_type, owner_id, created_by, job_id, channel, path, created_at`

func scanArtifact(sc interface{ Scan(...any) error }) (*artifactRow, error) {
	var a artifactRow
	if err := sc.Scan(&a.ID, &a.Name, &a.Size, &a.SHA256, &a.MediaType, &a.OwnerID, &a.CreatedBy, &a.JobID,
		&a.Channel, &a.Path, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *artifactRow) toProto() *mycelv1.Artifact {
	return &mycelv1.Artifact{
		Id: a.ID, Name: a.Name, Size: a.Size, Sha256: a.SHA256, MediaType: a.MediaType, OwnerId: a.OwnerID,
		CreatedBy: a.CreatedBy, JobId: a.JobID, Channel: a.Channel, CreatedAt: a.CreatedAt,
	}
}

func (s *store) insertArtifact(ctx context.Context, a *artifactRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifacts(`+artifactCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Size, a.SHA256, a.MediaType, a.OwnerID, a.CreatedBy, a.JobID, a.Channel, a.Path, a.CreatedAt)
	if isUnique(err) {
		return errConflict
	}
	return err
}

func (s *store) getArtifact(ctx context.Context, id string) (*artifactRow, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return a, err
}

// listArtifacts lists artifacts newest first, optionally filtered by job
// and channel and, unless vis is nil, to what the caller may see.
func (s *store) listArtifacts(ctx context.Context, job, channel string, vis *artifactFilter, limit int) ([]*artifactRow, error) {
	var where []string
	var args []any
	if vis != nil {
		c, a := vis.clause()
		where = append(where, c)
		args = append(args, a...)
	}
	if job != "" {
		where = append(where, "job_id = ?")
		args = append(args, job)
	}
	if channel != "" {
		where = append(where, "channel = ?")
		args = append(args, channel)
	}
	q := `SELECT ` + artifactCols + ` FROM artifacts`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // constant conditions with bound values
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*artifactRow
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// artifactBytes sums the stored artifact bytes of an owner or, when node
// is set, of that node's uploads not tied to a job.
func (s *store) artifactBytes(ctx context.Context, owner, node string) (int64, error) {
	var n sql.NullInt64
	var err error
	if node != "" {
		err = s.db.QueryRowContext(ctx, `SELECT SUM(size) FROM artifacts WHERE created_by = ? AND job_id = ''`, "node:"+node).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT SUM(size) FROM artifacts WHERE owner_id = ?`, owner).Scan(&n)
	}
	return n.Int64, err
}

// jobArtifactCount counts the artifacts uploaded for a job.
func (s *store) jobArtifactCount(ctx context.Context, job string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE job_id = ?`, job).Scan(&n)
	return n, err
}

// artifactFilter restricts a listing for a caller that is not an admin to
// rows owned by owner (when set), attached to one of channels, created by
// creator, or produced by one of jobs.
type artifactFilter struct {
	owner    string
	channels []string
	creator  string
	jobs     []string
}

func (f *artifactFilter) clause() (string, []any) {
	var ors []string
	var args []any
	if f.owner != "" {
		ors = append(ors, "owner_id = ?")
		args = append(args, f.owner)
	}
	if f.creator != "" {
		ors = append(ors, "created_by = ?")
		args = append(args, f.creator)
	}
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ors = append(ors, col+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in("channel", f.channels)
	in("job_id", f.jobs)
	if len(ors) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

func (s *store) deleteArtifact(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errNotFound
	}
	return nil
}
