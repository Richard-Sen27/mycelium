package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker/egress"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/profiles"
)

// JobsDir holds one directory per job under NodeDir:
//
//	jobs/<id>/in/            downloaded workspace tarball and inputs
//	jobs/<id>/work/          the sandbox's scratch directory (spec.Dir)
//	jobs/<id>/out/           collected outputs, kept for a while
//	jobs/<id>/summary.json   written when the job ended
const JobsDir = "jobs"

const (
	jobRPCTimeout     = 10 * time.Second
	jobStartedFirst   = 2 * time.Second // the synchronous job.started attempt
	jobStartedTries   = 5
	jobAcceptTimeout  = 5 * time.Second // backend selection while answering CreateSandbox
	jobInputTimeout   = 10 * time.Minute
	jobOutputTimeout  = 10 * time.Minute
	finishedMemory    = 24 * time.Hour // finished job ids are remembered this long
	finishedMemoryMax = 4096
)

// EnvEgressListen overrides the egress proxy's listen address
// (runConfig.egressListen), e.g. the WSL interface address on Windows.
const EnvEgressListen = "MYCEL_EGRESS_LISTEN"

var jobIDRE = regexp.MustCompile(`^job_[0-9A-Za-z]{1,64}$`)

// ---- job manager ----

// jobManager runs the jobs the hub sends with CreateSandbox.
type jobManager struct {
	n   *node
	log *slog.Logger
	dir string

	ctx    context.Context // ends on daemon shutdown
	cancel context.CancelFunc
	wg     sync.WaitGroup // job goroutines and their job.done reports

	mu       sync.Mutex
	closed   bool
	proxy    *egress.Proxy // nil while the proxy is down
	proxyErr string
	jobs     map[string]*runningJob // accepted, sandbox not yet cleaned up
	finished map[string]time.Time   // finished job ids (id-only memory)
	running  int                    // jobs holding a slot
	backends []string               // available backends (refreshed)
}

// runningJob is one accepted job.
type runningJob struct {
	id     string
	cancel context.CancelCauseFunc

	mu        sync.Mutex
	slot      bool           // holds a concurrency slot
	started   sync.WaitGroup // job.started retries
	sbxID     string         // set once registered with the egress proxy
	baseRules []egress.Rule  // the profile's egress rules
	sb        sandbox.Sandbox
	pauses    map[string]bool // reasons the sandbox must be paused (freeze, local_first)

	// pauseMu orders Pause and Resume calls on the sandbox.
	pauseMu   sync.Mutex
	pausedNow bool // the sandbox was last paused; guarded by pauseMu
	// mainStarted: the job's command was started (guarded by mu).
	mainStarted bool
}

// subjectJob is the grant subject of a job.
func subjectJob(id string) string { return "job:" + id }

// killCause is the cancel cause of KillSandbox (and of a hub that reports
// the job finished).
type killCause struct{ reason string }

func (k *killCause) Error() string { return "cancelled: " + k.reason }

func newJobManager(n *node) *jobManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &jobManager{
		n: n, log: n.log.With("component", "jobs"), dir: filepath.Join(n.dir, JobsDir),
		ctx: ctx, cancel: cancel,
		jobs: map[string]*runningJob{}, finished: map[string]time.Time{},
	}
	m.ensureProxy()
	// Leftovers of a previous run: unfinished job directories are stale.
	m.janitor(true)
	return m
}

// ensureProxy starts the egress proxy if it is not running.
func (m *jobManager) ensureProxy() {
	m.mu.Lock()
	up, closed := m.proxy != nil, m.closed
	m.mu.Unlock()
	if up || closed {
		return
	}
	listen := m.n.cfg.egressListen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	p, err := egress.Start(m.ctx, egress.Options{Listen: listen, Audit: m.audit, Logger: m.n.log.With("component", "egress")})
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.proxyErr == "" {
			m.log.Error("egress proxy did not start; jobs are refused until it does", "listen", listen, "err", err)
		}
		m.proxyErr = err.Error()
		return
	}
	if m.closed {
		_ = p.Close()
		return
	}
	if m.proxyErr != "" {
		m.log.Info("egress proxy started", "addr", p.Addr().String())
	}
	m.proxy, m.proxyErr = p, ""
}

func (m *jobManager) currentProxy() *egress.Proxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proxy
}

// proxyStatus is the status field egress_proxy: the listen address, or
// "unavailable: <reason>".
func (m *jobManager) proxyStatus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proxy != nil {
		return m.proxy.Addr().String()
	}
	if m.proxyErr != "" {
		return "unavailable: " + m.proxyErr
	}
	return "unavailable"
}

// audit logs the proxy's records; byte totals come from Unregister.
func (m *jobManager) audit(rec egress.Record) {
	m.log.Info("egress", "sandbox", rec.SandboxID, "host", rec.Host, "port", rec.Port,
		"allowed", rec.Allowed, "bytes", rec.BytesIn+rec.BytesOut)
}

// runningCount is the number of jobs holding a slot.
func (m *jobManager) runningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *jobManager) availableBackends() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.backends...)
}

// refreshBackends records which registered backends are available.
func (m *jobManager) refreshBackends(ctx context.Context) {
	list := []string{}
	for _, b := range sandbox.Backends() {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ok, _ := b.Available(actx)
		cancel()
		if ok {
			list = append(list, b.Name())
		}
	}
	m.mu.Lock()
	changed := !slices.Equal(m.backends, list) || m.backends == nil
	m.backends = list
	m.mu.Unlock()
	if changed {
		// The hub schedules on inventory.sandbox_backends: send the new set
		// in a full beat now rather than at the next probe.
		m.n.setInventoryBackends(list)
	}
}

// loop refreshes backends (and retries a failed proxy) and removes old job
// directories until ctx ends. It first replays unreported job.done.
func (m *jobManager) loop(ctx context.Context) {
	m.replayUnreported()
	m.refreshBackends(ctx)
	bt := time.NewTicker(m.n.cfg.backendsRefreshEvery)
	defer bt.Stop()
	jt := time.NewTicker(m.n.cfg.jobJanitorEvery)
	defer jt.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-bt.C:
			m.ensureProxy()
			m.refreshBackends(ctx)
		case <-jt.C:
			m.janitor(false)
		}
	}
}

// ctlError formats a ctl refusal with its connect code name.
func ctlError(code, format string, args ...any) string {
	return code + ": " + fmt.Sprintf(format, args...)
}

// checkOutputPattern accepts a glob relative to /workspace.
func checkOutputPattern(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) || filepath.IsAbs(p) {
		return fmt.Errorf("invalid output pattern %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("output pattern %q escapes /workspace", p)
		}
	}
	if _, err := path.Match(strings.ReplaceAll(p, "**", "*"), ""); err != nil {
		return fmt.Errorf("output pattern %q: %w", p, err)
	}
	return nil
}

// validateSpec checks everything about a job that can be checked before
// it runs; it returns the parsed egress rules.
func validateSpec(spec *mycelv1.JobSpec, prof *mycelv1.Profile) ([]egress.Rule, string) {
	if len(spec.GetCommand()) == 0 {
		return nil, ctlError("invalid_argument", "empty command")
	}
	if _, err := sandbox.ResolveLimits(prof.GetLimits(), nil); err != nil {
		return nil, ctlError("invalid_argument", "profile %s: %v", prof.GetName(), err)
	}
	if t := spec.GetTimeout(); t != "" {
		if d, err := time.ParseDuration(t); err != nil || d <= 0 {
			return nil, ctlError("invalid_argument", "invalid timeout %q", t)
		}
	}
	if err := sandbox.ValidateEnv(spec.GetEnv()); err != nil {
		return nil, ctlError("invalid_argument", "env: %v", err)
	}
	if spec.GetWorkspace().GetGit() != nil {
		return nil, ctlError("unimplemented", "git workspace seeds are not supported on this node yet")
	}
	if a := spec.GetWorkspace().GetArtifact(); a != "" && !artifactIDRE.MatchString(a) {
		return nil, ctlError("invalid_argument", "invalid workspace artifact id")
	}
	for _, o := range spec.GetOutputs() {
		if err := checkOutputPattern(o); err != nil {
			return nil, ctlError("invalid_argument", "%v", err)
		}
	}
	seen := map[string]bool{}
	for _, in := range spec.GetInputs() {
		if !artifactIDRE.MatchString(in.GetArtifact()) {
			return nil, ctlError("invalid_argument", "invalid input artifact id %q", in.GetArtifact())
		}
		p := in.GetPath()
		if p == "" {
			continue // inputs/<name>, known after download
		}
		if err := checkWorkspacePath(p); err != nil {
			return nil, ctlError("invalid_argument", "input %s: %v", in.GetArtifact(), err)
		}
		if c := path.Clean(p); seen[c] {
			return nil, ctlError("invalid_argument", "input path %q used twice", c)
		} else {
			seen[c] = true
		}
	}
	var rules []egress.Rule
	for _, r := range prof.GetEgress().GetAllow() {
		rule, err := egress.ParseRule(r)
		if err != nil {
			return nil, ctlError("invalid_argument", "profile %s: egress rule %q: %v", prof.GetName(), r, err)
		}
		rules = append(rules, rule)
	}
	return rules, ""
}

// create validates a CreateSandbox and starts the job asynchronously. It
// returns "" on acceptance or the refusal text. It never waits for the
// sandbox.
func (m *jobManager) create(cs *mycelv1.CreateSandbox) string {
	job := cs.GetJob()
	id := job.GetId()
	if !jobIDRE.MatchString(id) {
		return ctlError("invalid_argument", "invalid job id")
	}
	if job.GetNode() != m.n.name() {
		return ctlError("invalid_argument", "job %s is for node %q, not this node", id, job.GetNode())
	}
	m.mu.Lock()
	closed, proxyUp := m.closed, m.proxy != nil
	_, dup := m.jobs[id]
	_, done := m.finished[id]
	m.mu.Unlock()
	switch {
	case closed:
		return ctlError("unavailable", "node is shutting down")
	case dup:
		return "" // a retried CreateSandbox: the job is already running
	case done || m.hasSummary(id):
		return ctlError("failed_precondition", "job finished")
	case m.n.isFrozen():
		return ctlError("failed_precondition", "node frozen")
	case !m.n.sharing.Offers(offerSandbox) || !m.n.sharing.Active():
		return ctlError("failed_precondition", "sharing inactive")
	case !proxyUp:
		return ctlError("unavailable", "egress proxy is not running on this node")
	}
	spec := job.GetSpec()
	prof := cs.GetProfile()
	if prof == nil {
		p, err := profiles.Get(spec.GetProfile())
		if err != nil {
			return ctlError("invalid_argument", "unknown profile %q", spec.GetProfile())
		}
		prof = p
	}
	rules, refusal := validateSpec(spec, prof)
	if refusal != "" {
		return refusal
	}
	sctx, cancel := context.WithTimeout(m.ctx, jobAcceptTimeout)
	backend, err := sandbox.Select(sctx, prof)
	cancel()
	if err != nil {
		return ctlError("failed_precondition", "no backend for profile %s: %v", prof.GetName(), err)
	}

	jctx, jcancel := context.WithCancelCause(m.ctx)
	rj := &runningJob{id: id, cancel: jcancel, slot: true, pauses: map[string]bool{}}
	maxJobs := m.n.sharing.maxJobs()
	localFirst := m.n.sharing.localFirstPause()
	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		jcancel(nil)
		return ctlError("unavailable", "node is shutting down")
	case m.jobs[id] != nil:
		m.mu.Unlock()
		jcancel(nil)
		return ""
	case m.running >= maxJobs:
		running := m.running
		m.mu.Unlock()
		jcancel(nil)
		return ctlError("resource_exhausted", "node runs %d jobs (max %d)", running, maxJobs)
	}
	m.jobs[id] = rj
	m.running++
	m.wg.Add(1)
	// A freeze that began after the admission check finds the job in
	// m.jobs or sees the reason set here.
	if m.n.isFrozen() {
		rj.pauses[pauseFrozen] = true
	}
	if localFirst {
		rj.pauses[pauseLocalFirst] = true
	}
	m.mu.Unlock()

	m.log.Info("job accepted", "job", id, "profile", prof.GetName(), "backend", backend.Name())
	cs = proto.Clone(cs).(*mycelv1.CreateSandbox)
	prof = proto.Clone(prof).(*mycelv1.Profile)
	go func() {
		defer m.wg.Done()
		defer jcancel(nil)
		m.runJob(jctx, rj, cs, prof, rules, backend)
	}()
	return ""
}

// kill cancels a job; it returns "" or the refusal text. Killing a job
// that already finished is a no-op.
func (m *jobManager) kill(ks *mycelv1.KillSandbox) string {
	id := ks.GetJobId()
	m.mu.Lock()
	rj := m.jobs[id]
	_, finished := m.finished[id]
	m.mu.Unlock()
	if rj == nil {
		if finished || (jobIDRE.MatchString(id) && m.hasSummary(id)) {
			return ""
		}
		return ctlError("not_found", "unknown job %q", id)
	}
	reason := ks.GetReason()
	if reason == "" {
		reason = "killed by hub"
	}
	m.log.Info("job kill requested", "job", rj.id, "reason", reason)
	rj.cancel(&killCause{reason: reason})
	return ""
}

// releaseSlot frees rj's concurrency slot once.
func (m *jobManager) releaseSlot(rj *runningJob) {
	rj.mu.Lock()
	had := rj.slot
	rj.slot = false
	rj.mu.Unlock()
	if had {
		m.mu.Lock()
		m.running--
		m.mu.Unlock()
	}
}

// shutdown cancels every job and waits (bounded) until they reported.
func (m *jobManager) shutdown() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(m.n.cfg.jobKillGrace + 20*time.Second):
		m.log.Warn("jobs did not finish in time on shutdown")
	}
	if p := m.currentProxy(); p != nil {
		_ = p.Close()
	}
}

// ---- running one job ----

// jobOutcome is what job.done reports.
type jobOutcome struct {
	state     mycelv1.JobState
	exitCode  int32
	err       string
	artifacts []string
	stats     sandbox.Stats
}

func (m *jobManager) cancelError(ctx context.Context) string {
	var kc *killCause
	switch c := context.Cause(ctx); {
	case errors.As(c, &kc):
		return kc.Error()
	case m.ctx.Err() != nil:
		return "cancelled: node shutting down"
	}
	return "cancelled"
}

func (m *jobManager) runJob(ctx context.Context, rj *runningJob, cs *mycelv1.CreateSandbox, prof *mycelv1.Profile, rules []egress.Rule, backend sandbox.Backend) {
	job := cs.GetJob()
	spec := job.GetSpec()
	log := m.log.With("job", rj.id)
	started := time.Now().UTC()
	out := jobOutcome{state: mycelv1.JobState_JOB_STATE_FAILED, exitCode: -1}
	sbxID := "sbx:" + newULID(time.Now())
	jdir := filepath.Join(m.dir, rj.id)
	inDir, workDir, outDir := filepath.Join(jdir, "in"), filepath.Join(jdir, "work"), filepath.Join(jdir, "out")
	var (
		outputs        []summaryOutput
		collected      []sandbox.OutputFile
		createdBackend string
		proxy          *egress.Proxy
	)

	defer func() {
		// End of the job: slot, egress totals, scratch directories, summary,
		// then job.done (retried in the background).
		m.releaseSlot(rj)
		var egressBytes int64
		if proxy != nil {
			bin, bout := proxy.Unregister(sbxID)
			egressBytes = bin + bout
		}
		if len(collected) > 0 {
			outputs, out.artifacts = m.uploadOutputs(rj.id, workDir, outDir, collected, &out, log)
		}
		for _, d := range []string{inDir, workDir} {
			if err := os.RemoveAll(d); err != nil {
				log.Warn("remove job directory", "dir", d, "err", err)
			}
		}
		sum := &jobSummary{
			JobID: rj.id, State: out.state.String(), ExitCode: out.exitCode, Error: out.err, SandboxID: sbxID,
			Backend: createdBackend, Artifacts: append([]string{}, out.artifacts...), Outputs: outputs,
			StartedAt: started.Format(time.RFC3339Nano), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
			CPUMilli: out.stats.CPUMilli, MaxMemoryBytes: out.stats.MemoryBytes, EgressBytes: egressBytes,
		}
		m.writeSummary(sum, log)
		m.mu.Lock()
		delete(m.jobs, rj.id)
		m.rememberFinished(rj.id)
		m.mu.Unlock()
		m.prune()
		log.Info("job finished", "state", sum.State, "exit_code", sum.ExitCode, "artifacts", len(sum.Artifacts),
			"egress_bytes", egressBytes)
		// job.started retries end before job.done.
		rj.started.Wait()
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.deliverDone(sum, log)
		}()
	}()

	for _, d := range []string{jdir, inDir, workDir, outDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			out.err = "setup: " + err.Error()
			return
		}
	}
	m.prune()

	// Inputs.
	seed, err := m.fetchInputs(ctx, cs, inDir)
	if err != nil {
		if ctx.Err() != nil {
			out.state, out.err = mycelv1.JobState_JOB_STATE_CANCELLED, m.cancelError(ctx)
			return
		}
		out.err = "input: " + err.Error()
		return
	}

	// Egress.
	proxy = m.currentProxy()
	if proxy == nil {
		out.err = "egress: proxy not running"
		return
	}
	// Active grants of the job: environment, egress rules, mounts and the
	// proxy aliases of the brokers (refreshEgress sets the aliases).
	sg := m.n.grants.forSubject(subjectJob(rj.id))
	allRules := appendRules(slices.Clone(rules), sg.rules)
	allowText := make([]string, 0, len(allRules))
	for _, r := range allRules {
		allowText = append(allowText, string(r))
	}
	proxyURL, err := proxy.Register(sbxID, allRules)
	if err != nil {
		proxy = nil
		out.err = "egress: " + err.Error()
		return
	}
	rj.mu.Lock()
	rj.sbxID, rj.baseRules = sbxID, rules
	rj.mu.Unlock()
	// A grant attached or detached while registering is picked up here.
	m.refreshEgress(subjectJob(rj.id))
	env := maps.Clone(spec.GetEnv())
	if len(sg.env) > 0 {
		if env == nil {
			env = map[string]string{}
		}
		maps.Copy(env, sg.env)
	}

	limits, _ := sandbox.ResolveLimits(prof.GetLimits(), nil) // checked on accept
	sspec := sandbox.Spec{
		ID: sbxID, JobID: rj.id, Profile: prof, Image: prof.GetImage(), Limits: limits,
		Env: env, Workspace: seed,
		Egress: sandbox.Egress{ProxyURL: proxyURL, Allow: allowText},
		Mounts: sg.mounts,
		Dir:    workDir,
	}
	var timeout time.Duration
	if t := spec.GetTimeout(); t != "" {
		timeout, _ = time.ParseDuration(t) // checked on accept
	}

	// Run.
	hubFinished := func() {
		log.Info("hub reports the job finished; cancelling it")
		rj.cancel(&killCause{reason: "job finished on the hub"})
	}
	logs := newJobLog(rj.id, m.n.cfg.jobLogFlushEvery, m.n.cfg.jobLogMaxBuffer, m.sendLog,
		func(msg string, args ...any) { log.Warn(msg, args...) }, hubFinished)
	runner := &sandbox.Runner{
		Backend: &startedBackend{Backend: backend, m: m, rj: rj, onCreate: func(sb sandbox.Sandbox) {
			createdBackend = sb.Backend()
			m.reportStarted(rj, sb.ID(), sb.Backend(), hubFinished)
		}},
		Logs:          logs,
		KillGrace:     m.n.cfg.jobKillGrace,
		StatsInterval: m.n.cfg.jobStatsEvery,
	}
	res, runErr := runner.Run(ctx, sspec, spec.GetCommand(), spec.GetOutputs(), timeout)
	drain := time.Now().Add(m.n.cfg.jobLogDrain)
	if m.ctx.Err() != nil {
		drain = time.Now().Add(2 * time.Second)
	}
	logs.close(drain)

	out.exitCode = int32(res.ExitCode) //nolint:gosec // exit codes fit
	out.state, out.err, out.stats = res.State, res.Error, res.Stats
	if runErr != nil && ctx.Err() == nil {
		log.Warn("sandbox setup failed", "err", runErr)
	}
	// The runner's state stands: a command that exited just as a kill
	// arrived keeps its result. Only a cancelled run gets the reason.
	if out.state == mycelv1.JobState_JOB_STATE_CANCELLED {
		out.err = m.cancelError(ctx)
		return // a cancelled job uploads nothing
	}
	collected = res.Outputs
}

// startedBackend reports job.started once the runner created the sandbox
// and records the sandbox for pausing.
type startedBackend struct {
	sandbox.Backend
	m        *jobManager
	rj       *runningJob
	onCreate func(sandbox.Sandbox)
}

func (b *startedBackend) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	sb, err := b.Backend.Create(ctx, spec)
	if err != nil {
		return sb, err
	}
	b.rj.mu.Lock()
	b.rj.sb = sb
	b.rj.mu.Unlock()
	b.onCreate(sb)
	return &pausableSandbox{Sandbox: sb, m: b.m, rj: b.rj}, nil
}

// pausableSandbox applies the job's pause state to the processes it
// starts, so a command started while the job is paused (freeze,
// local_first) is stopped at once.
type pausableSandbox struct {
	sandbox.Sandbox
	m  *jobManager
	rj *runningJob
}

func (s *pausableSandbox) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.Process, error) {
	p, err := s.Sandbox.Exec(ctx, argv, opts)
	s.rj.mu.Lock()
	first := !s.rj.mainStarted
	s.rj.mainStarted = true
	s.rj.mu.Unlock()
	// Only the job's command is paused here; what the runner executes
	// after it ended (output collection) must run.
	if err == nil && first {
		s.m.applyPause(s.rj, true)
	}
	return p, err
}

// ---- pause, egress and grant subjects ----

// setPause sets or clears a pause reason on every running job.
func (m *jobManager) setPause(reason string, on bool) {
	m.mu.Lock()
	list := make([]*runningJob, 0, len(m.jobs))
	for _, rj := range m.jobs {
		list = append(list, rj)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, rj := range list {
		rj.mu.Lock()
		if on {
			rj.pauses[reason] = true
		} else {
			delete(rj.pauses, reason)
		}
		rj.mu.Unlock()
		wg.Go(func() { m.applyPause(rj, false) })
	}
	wg.Wait()
}

// applyLocalFirst pauses the running jobs while the owner is active and
// local_first is on, and resumes them otherwise.
func (m *jobManager) applyLocalFirst() {
	m.setPause(pauseLocalFirst, m.n.sharing.localFirstPause())
}

// applyPause brings the sandbox in line with the job's pause reasons. With
// force a pause is sent even when the sandbox is already paused (a new
// process was started). A failed pause or resume leaves pausedNow as it
// was, so the next evaluation (every idleSampleEvery, and every freeze or
// settings change) tries again.
func (m *jobManager) applyPause(rj *runningJob, force bool) {
	rj.pauseMu.Lock()
	defer rj.pauseMu.Unlock()
	rj.mu.Lock()
	want, sb := len(rj.pauses) > 0, rj.sb
	reasons := slices.Sorted(maps.Keys(rj.pauses))
	rj.mu.Unlock()
	if sb == nil || (want == rj.pausedNow && (!force || !want)) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if h := m.n.cfg.pauseHook; h != nil {
		err = h(rj.id, want)
	}
	if err == nil {
		if want {
			err = sb.Pause(ctx)
		} else {
			err = sb.Resume(ctx)
		}
	}
	if err != nil {
		m.log.Error("pause or resume of a job sandbox failed; retrying", "job", rj.id, "pause", want, "err", err)
		return
	}
	switch {
	case want && !rj.pausedNow:
		m.log.Info("job sandbox paused", "job", rj.id, "reasons", reasons)
	case !want && rj.pausedNow:
		m.log.Info("job sandbox resumed", "job", rj.id)
	}
	rj.pausedNow = want
}

// jobOfSubject returns the running job of a "job:<id>" subject.
func (m *jobManager) jobOfSubject(subject string) *runningJob {
	id, ok := strings.CutPrefix(subject, "job:")
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// sandboxOf returns the sandbox id of subject's running sandbox, or "".
func (m *jobManager) sandboxOf(subject string) string {
	rj := m.jobOfSubject(subject)
	if rj == nil {
		return ""
	}
	rj.mu.Lock()
	defer rj.mu.Unlock()
	return rj.sbxID
}

// refreshEgress replaces the allowlist of subject's running sandbox with
// its profile rules plus the rules of its active grants, and its proxy
// aliases with those of the brokers it may use.
func (m *jobManager) refreshEgress(subject string) {
	rj := m.jobOfSubject(subject)
	if rj == nil {
		return
	}
	proxy := m.currentProxy()
	rj.mu.Lock()
	id, base := rj.sbxID, slices.Clone(rj.baseRules)
	rj.mu.Unlock()
	if proxy == nil || id == "" {
		return
	}
	extra, aliases := m.n.grants.egressFor(subject)
	if err := proxy.Update(id, appendRules(base, extra)); err != nil {
		m.log.Debug("egress update", "job", rj.id, "err", err)
	}
	if err := proxy.SetAliases(id, aliases); err != nil {
		m.log.Warn("egress aliases", "job", rj.id, "err", err)
	}
}

// appendRules appends the extra rules not yet in rules.
func appendRules(rules, extra []egress.Rule) []egress.Rule {
	for _, r := range extra {
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules
}

// fetchInputs downloads the workspace tarball and the inputs into inDir.
func (m *jobManager) fetchInputs(ctx context.Context, cs *mycelv1.CreateSandbox, inDir string) (sandbox.WorkspaceSeed, error) {
	var seed sandbox.WorkspaceSeed
	spec := cs.GetJob().GetSpec()
	tar := spec.GetWorkspace().GetArtifact()
	if tar == "" && len(spec.GetInputs()) == 0 {
		return seed, nil
	}
	base, err := m.n.artifactBase(cs.GetInputBaseUrl())
	if err != nil {
		return seed, fmt.Errorf("input_base_url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, jobInputTimeout)
	defer cancel()
	hc := m.n.hubHTTP()
	defer hc.CloseIdleConnections()
	limit := m.n.cfg.jobArtifactMax
	if tar != "" {
		dst := filepath.Join(inDir, "workspace.tar")
		if _, err := m.n.downloadArtifact(ctx, hc, base, tar, dst, limit); err != nil {
			return seed, fmt.Errorf("workspace %s: %w", tar, err)
		}
		seed.Tarball = dst
	}
	seen := map[string]bool{}
	for i, in := range spec.GetInputs() {
		id := in.GetArtifact()
		dst := filepath.Join(inDir, fmt.Sprintf("%03d", i))
		name, err := m.n.downloadArtifact(ctx, hc, base, id, dst, limit)
		if err != nil {
			return seed, fmt.Errorf("%s: %w", id, err)
		}
		p := in.GetPath()
		if p == "" {
			if name == "" {
				name = id
			}
			p = "inputs/" + name
		}
		if err := checkWorkspacePath(p); err != nil {
			return seed, fmt.Errorf("%s: %w", id, err)
		}
		p = path.Clean(p)
		if seen[p] {
			return seed, fmt.Errorf("%s: path %q used twice", id, p)
		}
		seen[p] = true
		seed.Inputs = append(seed.Inputs, sandbox.Input{LocalPath: dst, Path: p})
	}
	return seed, nil
}

// checkWorkspacePath accepts a slash path relative to /workspace.
func checkWorkspacePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("invalid path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q escapes /workspace", p)
		}
	}
	if c := path.Clean(p); c == "." || !filepath.IsLocal(filepath.FromSlash(c)) {
		return fmt.Errorf("invalid path %q", p)
	}
	return nil
}

// uploadOutputs moves the collected outputs to out/ and uploads each one.
// An upload failure fails the job.
func (m *jobManager) uploadOutputs(jobID, workDir, outDir string, files []sandbox.OutputFile, out *jobOutcome, log *slog.Logger) ([]summaryOutput, []string) {
	src := filepath.Join(workDir, "outputs")
	if err := os.Remove(outDir); err == nil {
		if err := os.Rename(src, outDir); err != nil {
			_ = os.MkdirAll(outDir, 0o700)
			log.Warn("move outputs", "err", err)
		}
	}
	base, berr := m.n.artifactBase("")
	hc := m.n.hubHTTP()
	defer hc.CloseIdleConnections()
	var sums []summaryOutput
	var ids []string
	var errs []string
	for _, f := range files {
		so := summaryOutput{Path: f.Path, Size: f.Size}
		err := berr
		if err == nil && f.Size > m.n.cfg.jobArtifactMax {
			err = fmt.Errorf("%d bytes, more than the %d allowed", f.Size, m.n.cfg.jobArtifactMax)
		}
		if err == nil {
			var a *mycelv1.Artifact
			a, err = m.uploadFile(hc, base, jobID, f.Path, filepath.Join(outDir, filepath.FromSlash(f.Path)))
			if err == nil {
				so.Artifact = a.GetId()
				ids = append(ids, a.GetId())
			}
		}
		if err != nil {
			log.Warn("output upload failed", "path", f.Path, "err", err)
			errs = append(errs, f.Path+": "+err.Error())
		}
		sums = append(sums, so)
	}
	if len(errs) > 0 {
		msg := "output: " + strings.Join(errs, "; ")
		if out.err != "" {
			msg = out.err + "; " + msg
		}
		out.err = msg
		if out.state == mycelv1.JobState_JOB_STATE_SUCCEEDED {
			out.state = mycelv1.JobState_JOB_STATE_FAILED
		}
	}
	return sums, ids
}

func (m *jobManager) uploadFile(hc *http.Client, base, jobID, name, local string) (*mycelv1.Artifact, error) {
	f, err := os.Open(local) //nolint:gosec // G304: a file the runner collected under the job directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(m.ctx, jobOutputTimeout)
	defer cancel()
	return m.n.uploadArtifact(ctx, hc, base, url.Values{"name": {name}, "job": {jobID}}, f, st.Size())
}

// sendLog is jobLog's transport: one job.log RPC.
func (m *jobManager) sendLog(ctx context.Context, c *mycelv1.JobLogChunk) (*mycelv1.JobLogAck, error) {
	ctx, cancel := context.WithTimeout(ctx, jobRPCTimeout)
	defer cancel()
	ack := &mycelv1.JobLogAck{}
	if err := m.n.hubRPC(ctx, bus.RPCJobLog, c, ack); err != nil {
		return nil, err
	}
	return ack, nil
}

// reportStarted sends job.started: one short attempt in line, then retries
// in the background (tracked by rj.started) so a hub outage does not delay
// the command. A hub that says the job is finished cancels it.
func (m *jobManager) reportStarted(rj *runningJob, sandboxID, backend string, hubFinished func()) {
	req := &mycelv1.JobStartedRequest{JobId: rj.id, SandboxId: sandboxID, Backend: backend}
	// try reports whether to stop.
	try := func(timeout time.Duration) (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := m.n.hubRPC(ctx, bus.RPCJobStarted, req, &mycelv1.JobStartedResponse{})
		if err == nil {
			return true, nil
		}
		var he *hubError
		if errors.As(err, &he) {
			if hubSaysJobFinished(he.msg) {
				hubFinished()
				return true, nil
			}
			if !retryableHubCode(errCode(he.msg)) {
				return true, err
			}
		}
		return false, err
	}
	if stop, err := try(jobStartedFirst); stop {
		if err != nil {
			m.log.Warn("job.started refused", "job", rj.id, "err", err)
		}
		return
	}
	rj.started.Add(1)
	go func() {
		defer rj.started.Done()
		var err error
		for i := range jobStartedTries {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(time.Duration(i+1) * time.Second):
			}
			var stop bool
			if stop, err = try(jobRPCTimeout); stop {
				if err != nil {
					m.log.Warn("job.started refused", "job", rj.id, "err", err)
				}
				return
			}
		}
		m.log.Warn("job.started not delivered", "job", rj.id, "err", err)
	}()
}

// deliverDone sends job.done for sum and records in summary.json whether
// the hub acknowledged it.
func (m *jobManager) deliverDone(sum *jobSummary, log *slog.Logger) {
	if m.reportDone(sum.doneRequest(), log) {
		sum.Reported = true
		m.writeSummary(sum, log)
	}
}

// reportDone sends job.done until the hub acknowledges it, every
// jobDoneRetryEvery for jobDoneRetryFor; on shutdown it tries once.
func (m *jobManager) reportDone(req *mycelv1.JobDoneRequest, log *slog.Logger) bool {
	deadline := time.Now().Add(m.n.cfg.jobDoneRetryFor)
	for {
		timeout := jobRPCTimeout
		if m.ctx.Err() != nil {
			timeout = 3 * time.Second // shutting down: one quick attempt
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := m.n.hubRPC(ctx, bus.RPCJobDone, req, &mycelv1.JobDoneResponse{})
		cancel()
		if err == nil {
			return true
		}
		var he *hubError
		if errors.As(err, &he) && !retryableHubCode(errCode(he.msg)) {
			log.Warn("job.done refused by hub", "err", err)
			return false
		}
		if m.ctx.Err() != nil || time.Now().After(deadline) {
			log.Warn("job.done not delivered", "err", err)
			return false
		}
		select {
		case <-m.ctx.Done():
		case <-time.After(m.n.cfg.jobDoneRetryEvery):
		}
	}
}

// ---- ids ----

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a 26-char ULID (48-bit ms time, 80 random bits).
func newULID(now time.Time) string {
	var out [26]byte
	ms := uint64(now.UnixMilli()) & (1<<48 - 1) //nolint:gosec // masked to the 48-bit ULID time field
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&31]
		ms >>= 5
	}
	var rnd [10]byte
	_, _ = rand.Read(rnd[:])
	for i := range 16 {
		var v byte
		for j := range 5 {
			bit := i*5 + j
			v = v<<1 | (rnd[bit/8]>>(7-bit%8))&1
		}
		out[10+i] = crockford[v]
	}
	return string(out[:])
}
