package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const jobSummaryFile = "summary.json"

// jobSummary is jobs/<id>/summary.json. It holds everything job.done
// carries, so an unreported job.done can be sent again after a restart.
type jobSummary struct {
	JobID          string          `json:"job_id"`
	State          string          `json:"state"`
	ExitCode       int32           `json:"exit_code"`
	Error          string          `json:"error,omitempty"`
	SandboxID      string          `json:"sandbox_id,omitempty"`
	Backend        string          `json:"backend,omitempty"`
	Artifacts      []string        `json:"artifacts"`
	Outputs        []summaryOutput `json:"outputs"`
	StartedAt      string          `json:"started_at"`
	FinishedAt     string          `json:"finished_at"`
	CPUMilli       int64           `json:"cpu_milli"`
	MaxMemoryBytes int64           `json:"max_memory_bytes"`
	EgressBytes    int64           `json:"egress_bytes"`
	Reported       bool            `json:"reported"` // the hub acknowledged job.done
}

type summaryOutput struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Artifact string `json:"artifact,omitempty"`
}

func (s *jobSummary) doneRequest() *mycelv1.JobDoneRequest {
	return &mycelv1.JobDoneRequest{
		JobId: s.JobID, State: mycelv1.JobState(mycelv1.JobState_value[s.State]), ExitCode: s.ExitCode,
		Error: s.Error, Artifacts: s.Artifacts,
		Stats: &mycelv1.JobStats{
			StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, CpuSecondsMilli: s.CPUMilli,
			MaxMemoryBytes: s.MaxMemoryBytes, EgressBytes: s.EgressBytes,
		},
	}
}

func (m *jobManager) writeSummary(s *jobSummary, log *slog.Logger) {
	if s.Artifacts == nil {
		s.Artifacts = []string{}
	}
	if s.Outputs == nil {
		s.Outputs = []summaryOutput{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(m.dir, s.JobID, jobSummaryFile), b); err != nil {
		log.Warn("write job summary", "err", err)
	}
}

func (m *jobManager) readSummary(id string) (*jobSummary, error) {
	b, err := os.ReadFile(filepath.Join(m.dir, id, jobSummaryFile))
	if err != nil {
		return nil, err
	}
	s := &jobSummary{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	return s, nil
}

// rememberFinished records a finished job id; the caller holds m.mu. The
// memory outlives the job directory (bounded by age and count).
func (m *jobManager) rememberFinished(id string) {
	now := time.Now()
	m.finished[id] = now
	for fid, t := range m.finished {
		if now.Sub(t) > finishedMemory {
			delete(m.finished, fid)
		}
	}
	for len(m.finished) > finishedMemoryMax {
		oldest, ot := "", now
		for fid, t := range m.finished {
			if !t.After(ot) {
				oldest, ot = fid, t
			}
		}
		delete(m.finished, oldest)
	}
}

// replayUnreported sends job.done again for summaries the hub never
// acknowledged (the daemon stopped while retrying).
func (m *jobManager) replayUnreported() {
	for _, d := range m.listDirs() {
		if !d.summary {
			continue
		}
		s, err := m.readSummary(d.id)
		if err != nil || s.Reported || s.JobID != d.id {
			continue
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		m.rememberFinished(s.JobID)
		m.wg.Add(1)
		m.mu.Unlock()
		log := m.log.With("job", s.JobID)
		log.Info("sending job.done again after a restart", "state", s.State)
		go func() {
			defer m.wg.Done()
			m.deliverDone(s, log)
		}()
	}
}

func (m *jobManager) hasSummary(id string) bool {
	_, err := os.Stat(filepath.Join(m.dir, id, jobSummaryFile))
	return err == nil
}

type jobDirEntry struct {
	id      string
	t       time.Time
	summary bool
}

// listDirs returns the job directories not belonging to an active job.
func (m *jobManager) listDirs() []jobDirEntry {
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	active := make(map[string]bool, len(m.jobs))
	for id := range m.jobs {
		active[id] = true
	}
	m.mu.Unlock()
	var out []jobDirEntry
	for _, e := range ents {
		if !e.IsDir() || active[e.Name()] {
			continue
		}
		d := jobDirEntry{id: e.Name()}
		if st, err := os.Stat(filepath.Join(m.dir, e.Name(), jobSummaryFile)); err == nil {
			d.t, d.summary = st.ModTime(), true
		} else if st, err := os.Stat(filepath.Join(m.dir, e.Name())); err == nil {
			d.t = st.ModTime()
		}
		out = append(out, d)
	}
	return out
}

func (m *jobManager) removeDir(id string) {
	if err := os.RemoveAll(filepath.Join(m.dir, id)); err != nil {
		m.log.Warn("remove job directory", "job", id, "err", err)
	}
}

// janitor removes finished job directories older than jobKeep and, at
// startup, unfinished directories left by a previous run.
func (m *jobManager) janitor(startup bool) {
	now := time.Now()
	for _, d := range m.listDirs() {
		if (!d.summary && startup) || now.Sub(d.t) > m.n.cfg.jobKeep {
			m.removeDir(d.id)
		}
	}
}

// prune keeps at most jobsCap entries in the jobs directory, removing the
// oldest inactive ones.
func (m *jobManager) prune() {
	ents, err := os.ReadDir(m.dir)
	if err != nil || len(ents) <= m.n.cfg.jobsCap {
		return
	}
	dirs := m.listDirs()
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].t.Before(dirs[j].t) })
	excess := len(ents) - m.n.cfg.jobsCap
	for i := 0; i < excess && i < len(dirs); i++ {
		m.removeDir(dirs[i].id)
	}
}
