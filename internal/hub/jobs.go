package hub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// jobConfig holds the job timings and limits of one running hub (docs/
// decisions.md D021). A hub copies newJobConfig when it starts; tests
// change that variable before starting a hub to shorten timings or
// tighten limits.
type jobConfig struct {
	retry         time.Duration // a queued job without a node is retried this often
	cancelConfirm time.Duration // a cancelled job without job.done ends after this
	createTimeout time.Duration // the node's answer to CreateSandbox
	grace         time.Duration // an assigned job silent past its timeout plus this ends TIMED_OUT
	offlineGrace  time.Duration // jobs of a node offline this long end FAILED
	skipNode      time.Duration // a node that did not take a job is skipped for it this long
	retention     time.Duration // finished jobs are forgotten after this
	pruneEvery    time.Duration
	keepFinished  int // at most this many finished jobs are kept

	maxJobLog         uint64 // stored log per job; more is acknowledged and dropped
	maxArtifactBytes  int64  // one artifact
	ownerQuota        int64  // artifact bytes per owner
	nodeQuota         int64  // bytes of a node's uploads not tied to a job
	uploadsPerCaller  int    // concurrent uploads per user or node
	minFree           uint64 // uploads and log appends are refused below this free space
	maxActivePerOwner int    // unfinished jobs per owner
	submitsPerMinute  int32  // job submissions per owner
}

var newJobConfig = jobConfig{
	retry: 10 * time.Second, cancelConfirm: 30 * time.Second, createTimeout: 10 * time.Second,
	grace: 5 * time.Minute, offlineGrace: 2 * time.Minute, skipNode: time.Minute,
	retention: 7 * 24 * time.Hour, pruneEvery: time.Hour, keepFinished: 1000,

	maxJobLog: 256 << 20, maxArtifactBytes: 1 << 30, ownerQuota: 20 << 30, nodeQuota: 2 << 30,
	uploadsPerCaller: 4, minFree: 2 << 30, maxActivePerOwner: 64, submitsPerMinute: 60,
}

// Fixed job limits.
const (
	maxLogChunk          = 64 << 10 // one job.log chunk
	streamLogChunk       = 64 << 10 // StreamLogs message size
	defaultAgentLogBytes = 64 << 10
	maxAgentLogBytes     = 512 << 10
	maxLogWait           = 60 * time.Second

	maxJobArgs           = 1024
	maxJobArgBytes       = 256 << 10 // all argv bytes together
	maxJobEnv            = 64
	maxJobEnvValue       = 32 << 10
	maxJobInputs         = 64
	maxJobOutputs        = 64
	maxJobPathLen        = 1024
	maxJobName           = 128
	maxJobSpecBytes      = 512 << 10
	maxJobArtifacts      = 256
	maxJobError          = 4096
	maxLogStreamsPerUser = 16
	maxGitRef            = 256
	maxGitURL            = 2048
	maxResultError       = 512
	scheduleBatch        = 500
	maxNodeJobs          = 2 // running jobs per node when its CPU count is unknown, and at most

	rateKeyJobs = "*jobs" // rate limiter key of job submissions
)

// envKeyRE is the allowed shape of a job environment variable name.
var envKeyRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// jobState is the in-memory job machinery of a running hub.
type jobState struct {
	cfg jobConfig
	// mu serialises job state transitions that read before they write
	// (node reports, log appends, cancel, the per-owner cap) so they
	// cannot interleave.
	mu   sync.Mutex
	kick chan struct{}

	smu        sync.Mutex
	logStreams map[string]int                  // user id -> open follow streams
	uploads    map[string]int                  // caller -> uploads in progress
	reserved   map[string]int64                // quota key -> bytes being uploaded
	skip       map[string]map[string]time.Time // job -> node -> skipped until
	lastErr    map[string]string               // job -> why the last node did not take it

	// Scheduler goroutine only.
	offline   map[string]time.Time // node -> first seen offline with assigned jobs
	lastPrune time.Time
}

func newJobState(cfg jobConfig) *jobState {
	return &jobState{
		cfg: cfg, kick: make(chan struct{}, 1), logStreams: map[string]int{}, uploads: map[string]int{},
		reserved: map[string]int64{}, skip: map[string]map[string]time.Time{}, lastErr: map[string]string{},
		offline: map[string]time.Time{},
	}
}

func (s *server) kickScheduler() {
	select {
	case s.jobs.kick <- struct{}{}:
	default:
	}
}

// jobKey is the notifier key of a job's log and state waiters.
func jobKey(id string) string { return "job:" + id }

func (s *server) signalJob(id string) { s.ch.notify.signal(jobKey(id)) }

func nowNano() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseNano(v string) (time.Time, error) { return time.Parse(time.RFC3339Nano, v) }

// newULIDID returns prefix + a 26-char Crockford base32 ULID.
func newULIDID(prefix string) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var out [26]byte
	ms := uint64(time.Now().UnixMilli()) & (1<<48 - 1) //nolint:gosec // masked to the 48-bit ULID time field
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
	return prefix + string(out[:])
}

// validPrefixedID reports whether id is prefix + a 26-char ULID.
func validPrefixedID(id, prefix string) bool {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || len(rest) != 26 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if !strings.ContainsRune(crockford, rune(rest[i])) {
			return false
		}
	}
	return true
}

func validJobID(id string) bool      { return validPrefixedID(id, "job_") }
func validArtifactID(id string) bool { return validPrefixedID(id, "art_") }

func stateWord(st mycelv1.JobState) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "JOB_STATE_"))
}

// cleanText makes node-supplied text safe to store and show: valid UTF-8
// without C0/C1 controls (newline and tab kept), DEL, U+2028/U+2029 or bidi
// controls, at most n bytes.
func cleanText(v string, n int) string {
	v = strings.ToValidUTF8(v, "?")
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 || channels.IsBidiControl(r) {
			return -1
		}
		return r
	}, v)
	if len(v) > n {
		v = v[:n]
		for !utf8.ValidString(v) {
			v = v[:len(v)-1]
		}
	}
	return v
}

// ---- conversions ----

func (s *server) jobProto(ctx context.Context, j *jobRow) (*mycelv1.Job, error) {
	arts, err := s.st.jobArtifacts(ctx, j.ID)
	if err != nil {
		return nil, s.internalError("job artifacts", err)
	}
	out := &mycelv1.Job{
		Id: j.ID, Spec: j.Spec, State: j.State, OwnerId: j.OwnerID, SubmittedBy: j.SubmittedBy, Node: j.Node,
		Backend: j.Backend, SandboxId: j.SandboxID, ExitCode: j.ExitCode, Error: j.Error, Artifacts: arts,
		CreatedAt: j.CreatedAt, LogBytes: j.LogBytes,
	}
	stats := &mycelv1.JobStats{}
	if j.Stats != nil {
		stats = proto.CloneOf(j.Stats)
	}
	stats.StartedAt, stats.FinishedAt = j.StartedAt, j.FinishedAt
	if stats.StartedAt != "" || stats.FinishedAt != "" || j.Stats != nil {
		out.Stats = stats
	}
	return out, nil
}

// ---- visibility ----

// canReadChannel reports whether a may read channel name (missing
// channels are unreadable).
func (s *server) canReadChannel(ctx context.Context, a actor, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, s.internalError("get channel", err)
	}
	_, d, err := s.checkChannel(ctx, a, policy.ActionChannelRead, ch)
	if err != nil {
		return false, err
	}
	return d.Allow, nil
}

// agentMadeArtifact reports whether agent a created art or submitted the
// job that produced it.
func (s *server) agentMadeArtifact(ctx context.Context, a actor, art *artifactRow) (bool, error) {
	if art.CreatedBy == a.id {
		return true, nil
	}
	if art.JobID == "" {
		return false, nil
	}
	j, err := s.st.getJob(ctx, art.JobID)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, s.internalError("get job", err)
	}
	return j.SubmittedBy == a.id, nil
}

// artifactDecision asks the policy whether a may act on art. Member is set
// when the artifact is attached to a channel a may read (for agents: is a
// member of); Creator, for agents, when the agent made it.
func (s *server) artifactDecision(ctx context.Context, a actor, action policy.Action, art *artifactRow) (policy.Decision, error) {
	p := a.p
	if art.Channel != "" {
		ok, err := s.canReadChannel(ctx, a, art.Channel)
		if err != nil {
			return policy.Decision{}, err
		}
		p.Member = ok
	}
	if a.p.Kind == policy.KindAgent {
		ok, err := s.agentMadeArtifact(ctx, a, art)
		if err != nil {
			return policy.Decision{}, err
		}
		p.Creator = ok
	}
	return s.policy.Authorize(ctx, p, action, policy.Resource{Kind: "artifact", ID: art.ID, OwnerID: art.OwnerID}), nil
}

// ---- submission ----

// validRelPath: a relative slash path without "..", backslashes, a drive
// or control characters.
func validRelPath(p string) bool {
	if p == "" || len(p) > maxJobPathLen || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") ||
		!channels.IsPrintable(p) {
		return false
	}
	if len(p) >= 2 && p[1] == ':' { // a Windows drive
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func validOutputGlob(g string) bool {
	if !validRelPath(g) {
		return false
	}
	_, err := path.Match(g, "")
	return err == nil
}

// validGitURL accepts https URLs without credentials whose host is a
// public name: no localhost, IP literal or .local/.internal name.
func validGitURL(raw string) error {
	if raw == "" || len(raw) > maxGitURL || !channels.IsPrintable(raw) {
		return errors.New("invalid git url")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("git url must be an https URL")
	}
	if u.User != nil {
		return errors.New("git url must not carry credentials")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case host == "" || !strings.Contains(host, "."):
		return errors.New("git url needs a public host name")
	case net.ParseIP(strings.Trim(host, "[]")) != nil:
		return errors.New("git url host must not be an IP address")
	case host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal"):
		return errors.New("git url host must be a public name")
	}
	return nil
}

func validGitRef(ref string) bool {
	if ref == "" {
		return true
	}
	if len(ref) > maxGitRef || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._/-+", rune(c)) {
			return false
		}
	}
	return true
}

// isProxyVar reports environment names that would redirect sandbox
// traffic around the node's egress proxy.
func isProxyVar(k string) bool { return strings.HasSuffix(strings.ToUpper(k), "_PROXY") }

// jobRefusal is a submission refused for a reason worth auditing.
func (s *server) jobRefusal(ctx context.Context, a actor, code connect.Code, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	s.auditDenied(ctx, a.auditID(), string(policy.ActionJobRun), "job", map[string]any{"reason": msg})
	return connect.NewError(code, errors.New(msg))
}

// visibleArtifact loads an artifact referenced by a job spec and checks
// that the submitter may read it; an invisible one is reported missing.
func (s *server) visibleArtifact(ctx context.Context, a actor, id, what string) (*artifactRow, error) {
	if !validArtifactID(id) {
		return nil, invalidArg("%s: invalid artifact id %q", what, id)
	}
	missing := connect.NewError(connect.CodeNotFound, fmt.Errorf("%s: artifact %s not found", what, id))
	art, err := s.st.getArtifact(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, missing
	}
	if err != nil {
		return nil, s.internalError("get artifact", err)
	}
	d, err := s.artifactDecision(ctx, a, policy.ActionArtifactGet, art)
	if err != nil {
		return nil, err
	}
	if !d.Allow {
		s.auditDenied(ctx, a.auditID(), string(policy.ActionJobRun), "artifact:"+id, map[string]any{"reason": "artifact not visible"})
		return nil, missing
	}
	return art, nil
}

// validateJobSpec checks a submitted spec for a and returns the normalised
// spec (timeout and input paths filled in) and the resolved profile.
func (s *server) validateJobSpec(ctx context.Context, a actor, in *mycelv1.JobSpec) (*mycelv1.JobSpec, *mycelv1.Profile, error) {
	if in == nil {
		return nil, nil, invalidArg("missing job spec")
	}
	if proto.Size(in) > maxJobSpecBytes {
		return nil, nil, invalidArg("job spec exceeds %d bytes", maxJobSpecBytes)
	}
	spec := proto.CloneOf(in)
	prof, err := s.profile(ctx, spec.GetProfile())
	if errors.Is(err, errNotFound) {
		return nil, nil, s.jobRefusal(ctx, a, connect.CodeInvalidArgument, "unknown profile %q", spec.GetProfile())
	}
	if err != nil {
		return nil, nil, s.internalError("profile", err)
	}
	if len(spec.GetName()) > maxJobName || !channels.IsPrintable(spec.GetName()) {
		return nil, nil, invalidArg("invalid job name: up to %d printable bytes", maxJobName)
	}
	// command
	if len(spec.GetCommand()) == 0 || spec.GetCommand()[0] == "" {
		return nil, nil, invalidArg("command must be a non-empty argv")
	}
	if len(spec.GetCommand()) > maxJobArgs {
		return nil, nil, invalidArg("at most %d command arguments", maxJobArgs)
	}
	total := 0
	for _, arg := range spec.GetCommand() {
		total += len(arg)
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
			return nil, nil, invalidArg("command arguments must be valid UTF-8 without NUL bytes")
		}
	}
	if total > maxJobArgBytes {
		return nil, nil, invalidArg("command exceeds %d bytes", maxJobArgBytes)
	}
	// env
	if len(spec.GetEnv()) > maxJobEnv {
		return nil, nil, invalidArg("at most %d environment variables", maxJobEnv)
	}
	for k, v := range spec.GetEnv() {
		if strings.HasPrefix(strings.ToUpper(k), "MYCEL_") {
			return nil, nil, s.jobRefusal(ctx, a, connect.CodeInvalidArgument, "environment variable %s: MYCEL_* names are reserved", k)
		}
		if isProxyVar(k) {
			return nil, nil, s.jobRefusal(ctx, a, connect.CodeInvalidArgument, "environment variable %s: proxy settings are set by the node", k)
		}
		if !envKeyRE.MatchString(k) || len(k) > 128 {
			return nil, nil, invalidArg("invalid environment variable name %q: want [A-Z_][A-Z0-9_]*", k)
		}
		if len(v) > maxJobEnvValue || strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			return nil, nil, invalidArg("environment variable %s: value must be valid UTF-8 without NUL, at most %d bytes", k, maxJobEnvValue)
		}
	}
	// The sandbox package holds the one rule every backend enforces too.
	if err := sandbox.ValidateEnv(spec.GetEnv()); err != nil {
		return nil, nil, s.jobRefusal(ctx, a, connect.CodeInvalidArgument, "environment: %v", err)
	}
	// workspace
	switch src := spec.GetWorkspace().GetSource().(type) {
	case nil:
	case *mycelv1.WorkspaceSeed_Artifact:
		if _, err := s.visibleArtifact(ctx, a, src.Artifact, "workspace"); err != nil {
			return nil, nil, err
		}
	case *mycelv1.WorkspaceSeed_Git:
		if err := validGitURL(src.Git.GetUrl()); err != nil {
			return nil, nil, invalidArg("workspace: %v", err)
		}
		if !validGitRef(src.Git.GetRef()) {
			return nil, nil, invalidArg("workspace: invalid git ref %q", src.Git.GetRef())
		}
	}
	// inputs
	if len(spec.GetInputs()) > maxJobInputs {
		return nil, nil, invalidArg("at most %d inputs", maxJobInputs)
	}
	for _, in := range spec.GetInputs() {
		art, err := s.visibleArtifact(ctx, a, in.GetArtifact(), "input")
		if err != nil {
			return nil, nil, err
		}
		if in.GetPath() == "" {
			in.Path = "inputs/" + path.Base(art.Name)
		}
		if !validRelPath(in.GetPath()) {
			return nil, nil, invalidArg("input %s: path %q must be relative to /workspace without '..'", in.GetArtifact(), in.GetPath())
		}
	}
	// outputs
	if len(spec.GetOutputs()) > maxJobOutputs {
		return nil, nil, invalidArg("at most %d outputs", maxJobOutputs)
	}
	for _, g := range spec.GetOutputs() {
		if !validOutputGlob(g) {
			return nil, nil, invalidArg("output %q must be a relative glob without '..'", g)
		}
	}
	// timeout
	wall, err := profileWallClock(prof)
	if err != nil {
		return nil, nil, s.internalError("profile wall clock", err)
	}
	timeout := wall
	if v := spec.GetTimeout(); v != "" {
		timeout, err = time.ParseDuration(v)
		if err != nil || timeout <= 0 {
			return nil, nil, invalidArg("invalid timeout %q", v)
		}
		if timeout > wall {
			return nil, nil, invalidArg("timeout %s exceeds the %s profile's wall clock of %s", timeout, prof.GetName(), wall)
		}
	}
	spec.Timeout = timeout.String()
	// channel: the result message is posted for the submitter, so the
	// submitter must be allowed to post there.
	if name := spec.GetChannel(); name != "" {
		ch, err := s.loadChannel(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if err := s.postAllowed(ctx, a, ch); err != nil {
			return nil, nil, err
		}
	}
	// node pin
	if pin := spec.GetNode(); pin != "" {
		n, err := s.st.getNode(ctx, pin)
		if errors.Is(err, errNotFound) {
			return nil, nil, s.jobRefusal(ctx, a, connect.CodeNotFound, "node %q not found", pin)
		}
		if err != nil {
			return nil, nil, s.internalError("get node", err)
		}
		if ok, why := nodeFits(nodeCandidate(n), prof); !ok {
			return nil, nil, s.jobRefusal(ctx, a, connect.CodeFailedPrecondition, "node %s cannot run profile %s: %s", pin, prof.GetName(), why)
		}
	}
	return spec, prof, nil
}

// submitJob checks, stores and schedules a job for a (a user or an agent).
// The job's owner is the submitter's owner.
func (s *server) submitJob(ctx context.Context, a actor, in *mycelv1.JobSpec) (*mycelv1.Job, error) {
	cfg := s.jobs.cfg
	owner := a.p.OwnerID
	if owner == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("submitter has no owner"))
	}
	if d := s.policy.Authorize(ctx, a.p, policy.ActionJobRun, policy.Resource{Kind: "job", OwnerID: owner}); !d.Allow {
		s.auditDenied(ctx, a.auditID(), string(policy.ActionJobRun), "job", map[string]any{"reason": d.Reason})
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
	}
	if s.frozenAll() {
		return nil, s.jobRefusal(ctx, a, connect.CodeFailedPrecondition, "network frozen")
	}
	if !s.ch.limiter.Allow(owner, rateKeyJobs, cfg.submitsPerMinute) {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d job submissions per minute per owner", cfg.submitsPerMinute))
	}
	spec, prof, err := s.validateJobSpec(ctx, a, in)
	if err != nil {
		return nil, err
	}
	j := &jobRow{
		ID: newULIDID("job_"), Spec: spec, State: mycelv1.JobState_JOB_STATE_QUEUED, OwnerID: owner,
		SubmittedBy: a.id, CreatedAt: nowNano(), ProfileVersion: prof.GetVersion(),
	}
	// The cap is counted and the job inserted under one lock.
	s.jobs.mu.Lock()
	n, err := s.st.activeJobsOf(ctx, owner)
	if err == nil && n < cfg.maxActivePerOwner {
		err = s.st.insertJob(ctx, j)
	}
	s.jobs.mu.Unlock()
	if err != nil {
		return nil, s.internalError("insert job", err)
	}
	if n >= cfg.maxActivePerOwner {
		return nil, s.jobRefusal(ctx, a, connect.CodeResourceExhausted, "at most %d unfinished jobs per owner", cfg.maxActivePerOwner)
	}
	s.audit(ctx, a.auditID(), string(policy.ActionJobRun), "job:"+j.ID, outcomeOK, map[string]any{
		"profile": prof.GetName(), "profile_version": prof.GetVersion(), "owner_id": owner, "submitted_by": a.id,
		"argv0": spec.GetCommand()[0], "node_pin": spec.GetNode(), "channel": spec.GetChannel(), "timeout": spec.GetTimeout(),
	})
	s.kickScheduler()
	return s.jobProto(ctx, j)
}

// loadJob loads a job and checks action on it for a. A missing job is
// NotFound; a job the caller may not see is PermissionDenied.
func (s *server) loadJob(ctx context.Context, a actor, action policy.Action, id string) (*jobRow, error) {
	if !validJobID(id) {
		return nil, invalidArg("invalid job id %q", id)
	}
	j, err := s.st.getJob(ctx, id)
	if errors.Is(err, errNotFound) {
		if d := s.policy.Authorize(ctx, a.p, action, policy.Resource{Kind: "job", ID: id}); !d.Allow {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
		}
		return nil, notFound("job")
	}
	if err != nil {
		return nil, s.internalError("get job", err)
	}
	if d := s.policy.Authorize(ctx, a.p, action, policy.Resource{Kind: "job", ID: id, OwnerID: j.OwnerID}); !d.Allow {
		if action == policy.ActionJobCancel {
			s.auditDenied(ctx, a.auditID(), string(action), "job:"+id, map[string]any{"reason": d.Reason})
		}
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
	}
	return j, nil
}

// ---- scheduling ----

// nodeCand is what scheduling needs to know about a node.
type nodeCand struct {
	name     string
	sandbox  bool     // "sandbox" capability enabled
	backends []string // inventory sandbox_backends
	capacity int      // running jobs the node takes
}

func nodeCandidate(n *nodeRow) nodeCand {
	c := nodeCand{name: n.Name, sandbox: slices.Contains(n.CapsEnabled, "sandbox"), capacity: maxNodeJobs}
	if len(n.Inventory) > 0 {
		var inv mycelv1.Inventory
		if proto.Unmarshal(n.Inventory, &inv) == nil {
			c.backends = inv.GetSandboxBackends()
			if cpus := int(inv.GetCpuCount()); cpus > 0 {
				c.capacity = max(1, min(maxNodeJobs, cpus/2))
			}
		}
	}
	return c
}

// backendRank is the index in the profile's preference list of the first
// backend the node supports, or -1.
func backendRank(prof *mycelv1.Profile, backends []string) int {
	for i, b := range prof.GetBackends() {
		if slices.Contains(backends, b) {
			return i
		}
	}
	return -1
}

// nodeFits checks the static requirements: capability and backend.
func nodeFits(c nodeCand, prof *mycelv1.Profile) (bool, string) {
	if !c.sandbox {
		return false, "the sandbox capability is not enabled"
	}
	if backendRank(prof, c.backends) < 0 {
		return false, "no backend of [" + strings.Join(prof.GetBackends(), ", ") + "] available"
	}
	return true, ""
}

// onlineCandidates lists the online nodes, by name.
func (s *server) onlineCandidates(ctx context.Context) ([]nodeCand, error) {
	rows, err := s.st.listNodes(ctx)
	if err != nil {
		return nil, err
	}
	online := s.onlineSet()
	var out []nodeCand
	for _, n := range rows {
		if online[n.Name] {
			out = append(out, nodeCandidate(n))
		}
	}
	return out, nil
}

// pickNode chooses a node for prof among cands: sandbox enabled, a backend
// from the profile's list, below its capacity and not in skip; then fewest
// running jobs, then the earliest preferred backend, then name. pin
// restricts the choice to one node. "" when none fits.
func pickNode(cands []nodeCand, running map[string]int, prof *mycelv1.Profile, pin string, skip map[string]bool) string {
	best, bestRank, bestRun := "", 0, 0
	for _, c := range cands {
		if (pin != "" && c.name != pin) || skip[c.name] {
			continue
		}
		if ok, _ := nodeFits(c, prof); !ok {
			continue
		}
		rank, run := backendRank(prof, c.backends), running[c.name]
		if run >= c.capacity {
			continue
		}
		if best == "" || run < bestRun || (run == bestRun && (rank < bestRank || (rank == bestRank && c.name < best))) {
			best, bestRank, bestRun = c.name, rank, run
		}
	}
	return best
}

// jobTimeout is the job's normalised timeout.
func jobTimeout(j *jobRow) time.Duration {
	d, err := time.ParseDuration(j.Spec.GetTimeout())
	if err != nil || d <= 0 {
		return time.Minute
	}
	return d
}

// startScheduler requeues jobs whose sandbox request may never have left
// before the hub stopped, then runs the scheduling loop until the hub
// stops.
func (s *server) startScheduler(ctx context.Context) error {
	n, err := s.st.requeueUnstarted(ctx)
	if err != nil {
		return fmt.Errorf("hub: requeue jobs: %w", err)
	}
	if n > 0 {
		s.log.Info("requeued jobs that had not started", "jobs", n)
	}
	c := s.ch
	c.workMu.Lock()
	if c.closing {
		c.workMu.Unlock()
		return nil
	}
	c.work.Add(1)
	c.workMu.Unlock()
	go func() {
		defer c.work.Done()
		for {
			wait := s.scheduleOnce(ctx) // ctx never ends (WithoutCancel); stop ends the loop
			t := time.NewTimer(wait)
			select {
			case <-c.stop:
				t.Stop()
				return
			case <-s.jobs.kick:
			case <-t.C:
			}
			t.Stop()
		}
	}()
	return nil
}

// skipped returns the nodes a job must not be offered to right now.
func (s *server) skipped(id string) map[string]bool {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	out := map[string]bool{}
	now := time.Now()
	for node, until := range s.jobs.skip[id] {
		if now.Before(until) {
			out[node] = true
		} else {
			delete(s.jobs.skip[id], node)
		}
	}
	if len(s.jobs.skip[id]) == 0 {
		delete(s.jobs.skip, id)
	}
	return out
}

// forgetJob drops a finished job's scheduling memory.
func (s *server) forgetJob(id string) {
	s.jobs.smu.Lock()
	delete(s.jobs.skip, id)
	delete(s.jobs.lastErr, id)
	s.jobs.smu.Unlock()
}

// scheduleOnce assigns queued jobs to nodes, fails queued jobs whose
// timeout passed without a node, ends cancelled jobs whose node did not
// confirm, jobs of nodes that stayed offline and assigned jobs whose node
// went silent, and prunes old finished jobs. It returns how long to wait
// before the next pass.
func (s *server) scheduleOnce(ctx context.Context) time.Duration {
	cfg := s.jobs.cfg
	next := cfg.retry
	soon := func(at time.Time) {
		if d := time.Until(at); d < next {
			next = d
		}
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("scheduler panic", "panic", r)
		}
	}()
	if time.Since(s.jobs.lastPrune) >= cfg.pruneEvery {
		s.jobs.lastPrune = time.Now()
		s.pruneJobs(ctx)
	}
	// While the network is frozen nothing is dispatched or timed out; the
	// nodes have paused their sandboxes.
	if s.frozenAll() {
		return next
	}
	queued, err := s.st.jobsInState(ctx, mycelv1.JobState_JOB_STATE_QUEUED, scheduleBatch)
	if err != nil {
		s.log.Error("scheduler: queued jobs", "err", err)
		return next
	}
	if len(queued) > 0 {
		cands, err := s.onlineCandidates(ctx)
		if err != nil {
			s.log.Error("scheduler: nodes", "err", err)
			return next
		}
		running, err := s.st.runningPerNode(ctx)
		if err != nil {
			s.log.Error("scheduler: running jobs", "err", err)
			return next
		}
		for _, j := range queued {
			prof, err := s.st.getProfile(ctx, j.Spec.GetProfile(), j.ProfileVersion)
			if err != nil {
				s.failJob(ctx, j, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_QUEUED}, mycelv1.JobState_JOB_STATE_FAILED, "profile no longer available")
				continue
			}
			created, err := parseNano(j.CreatedAt)
			if err != nil {
				created = time.Now()
			}
			deadline := created.Add(jobTimeout(j))
			if !time.Now().Before(deadline) {
				reason := "no node available"
				s.jobs.smu.Lock()
				if last := s.jobs.lastErr[j.ID]; last != "" {
					reason += "; last attempt: " + last
				}
				s.jobs.smu.Unlock()
				s.failJob(ctx, j, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_QUEUED}, mycelv1.JobState_JOB_STATE_FAILED, reason)
				continue
			}
			skip := s.skipped(j.ID)
			if node := pickNode(cands, running, prof, j.Spec.GetNode(), skip); node != "" {
				if s.assign(ctx, j, prof, node) {
					running[node]++
				}
				continue
			}
			soon(deadline)
			if len(skip) > 0 {
				soon(time.Now().Add(time.Second))
			}
		}
	}
	assigned, err := s.st.assignedJobs(ctx)
	if err != nil {
		s.log.Error("scheduler: assigned jobs", "err", err)
		return next
	}
	online := s.onlineSet()
	busy := map[string]bool{}
	from := []mycelv1.JobState{mycelv1.JobState_JOB_STATE_STARTING, mycelv1.JobState_JOB_STATE_RUNNING}
	for _, j := range assigned {
		if j.CancelRequestedAt != "" {
			if at, err := parseNano(j.CancelRequestedAt); err == nil {
				if limit := at.Add(cfg.cancelConfirm); time.Now().Before(limit) {
					soon(limit)
				} else {
					s.failJob(ctx, j, from, mycelv1.JobState_JOB_STATE_CANCELLED, "node did not confirm")
					continue
				}
			}
		}
		if !online[j.Node] {
			busy[j.Node] = true
			since, ok := s.jobs.offline[j.Node]
			if !ok {
				since = time.Now()
				s.jobs.offline[j.Node] = since
			}
			if limit := since.Add(cfg.offlineGrace); time.Now().Before(limit) {
				soon(limit)
			} else {
				s.failJob(ctx, j, from, mycelv1.JobState_JOB_STATE_FAILED, "node offline")
				continue
			}
		}
		if at, err := parseNano(j.AssignedAt); err == nil {
			if limit := at.Add(jobTimeout(j) + cfg.grace); time.Now().Before(limit) {
				soon(limit)
			} else {
				s.killSandbox(j.Node, j.ID, "no result before the job timeout")
				s.failJob(ctx, j, from, mycelv1.JobState_JOB_STATE_TIMED_OUT, "no result from node "+j.Node)
			}
		}
	}
	for node := range s.jobs.offline {
		if !busy[node] {
			delete(s.jobs.offline, node)
		}
	}
	return max(next, 20*time.Millisecond)
}

// pruneJobs forgets finished jobs older than the retention or beyond the
// newest keepFinished, with their logs; their artifacts stay.
func (s *server) pruneJobs(ctx context.Context) {
	cfg := s.jobs.cfg
	cutoff := time.Now().Add(-cfg.retention).UTC().Format(time.RFC3339Nano)
	ids, err := s.st.prunableJobs(ctx, cutoff, cfg.keepFinished)
	if err != nil {
		s.log.Error("prune jobs", "err", err)
		return
	}
	for _, id := range ids {
		if err := s.st.deleteJob(ctx, id); err != nil {
			s.log.Error("prune job", "job", id, "err", err)
			return
		}
		if err := os.RemoveAll(filepath.Join(s.jobsDir(), id)); err != nil {
			s.log.Warn("remove job log", "job", id, "err", err)
		}
	}
	if len(ids) > 0 {
		s.log.Info("pruned finished jobs", "jobs", len(ids))
	}
}

// assign moves a queued job to STARTING on node and sends CreateSandbox
// through the node's control queue. It reports whether the job was taken.
func (s *server) assign(ctx context.Context, j *jobRow, prof *mycelv1.Profile, node string) bool {
	inc, err := s.st.nodeIncarnation(ctx, node)
	if err != nil {
		return false
	}
	now := nowNano()
	err = s.st.updateJob(ctx, j.ID, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_QUEUED},
		"state = ?, node = ?, node_incarnation = ?, assigned_at = ?", int32(mycelv1.JobState_JOB_STATE_STARTING), node, inc, now)
	if errors.Is(err, errStateChanged) {
		return false
	}
	if err != nil {
		s.log.Error("assign job", "job", j.ID, "err", err)
		return false
	}
	j.State, j.Node, j.NodeIncarnation, j.AssignedAt = mycelv1.JobState_JOB_STATE_STARTING, node, inc, now
	job, err := s.jobProto(ctx, j)
	if err != nil {
		return true
	}
	s.audit(ctx, hubActor.id, "job.schedule", "job:"+j.ID, outcomeOK, map[string]any{"node": node, "profile": prof.GetName()})
	id := j.ID
	s.enqueueCtl(node, ctlItem{
		req: &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_CreateSandbox{CreateSandbox: &mycelv1.CreateSandbox{
			Job: job, Profile: prof, InputBaseUrl: s.cfg.AdvertiseURL,
		}}},
		timeout: s.jobs.cfg.createTimeout,
		done:    func(resp *mycelv1.CtlResponse, err error) { s.createSandboxAnswered(id, node, now, resp, err) },
	})
	s.signalJob(id)
	return true
}

// createSandboxAnswered handles the node's answer to CreateSandbox. A
// refusal, no answer (timeout, no responder) or a dropped request puts the
// job back in the queue and keeps it away from that node for a while; it
// fails only when its queue deadline passes without a node.
func (s *server) createSandboxAnswered(id, node, assignedAt string, resp *mycelv1.CtlResponse, err error) {
	if err == nil && resp.GetOk() {
		return
	}
	reason := "node " + node + " did not answer the sandbox request"
	if err == nil {
		reason = "node " + node + ": " + cleanText(resp.GetError(), maxJobError)
		if resp.GetError() == "" {
			reason = "node " + node + " refused the job"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	s.jobs.mu.Lock()
	j, gerr := s.st.getJob(ctx, id)
	if gerr != nil || j.State != mycelv1.JobState_JOB_STATE_STARTING || j.Node != node || j.AssignedAt != assignedAt {
		s.jobs.mu.Unlock()
		return
	}
	uerr := s.st.updateJob(ctx, id, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_STARTING},
		"state = ?, node = '', node_incarnation = '', assigned_at = NULL", int32(mycelv1.JobState_JOB_STATE_QUEUED))
	s.jobs.mu.Unlock()
	if uerr != nil {
		return
	}
	if err != nil {
		// The node may still create the sandbox; make sure it goes away.
		s.killSandbox(node, id, "sandbox request not acknowledged")
	}
	s.jobs.smu.Lock()
	if s.jobs.skip[id] == nil {
		s.jobs.skip[id] = map[string]time.Time{}
	}
	s.jobs.skip[id][node] = time.Now().Add(s.jobs.cfg.skipNode)
	s.jobs.lastErr[id] = reason
	s.jobs.smu.Unlock()
	s.audit(ctx, hubActor.id, "job.requeue", "job:"+id, outcomeOK, map[string]any{"node": node, "reason": reason})
	s.signalJob(id)
	s.kickScheduler()
}

// killSandbox queues a KillSandbox for node.
func (s *server) killSandbox(node, id, reason string) {
	if node == "" {
		return
	}
	s.enqueueCtl(node, ctlItem{req: &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_KillSandbox{KillSandbox: &mycelv1.KillSandbox{
		JobId: id, Reason: reason,
	}}}})
}

// failJob ends a job decided by the hub (no node result) with state and
// errText, when it is still in one of from.
func (s *server) failJob(ctx context.Context, j *jobRow, from []mycelv1.JobState, state mycelv1.JobState, errText string) {
	s.jobs.mu.Lock()
	err := s.st.finishJob(ctx, j.ID, from, state, 0, errText, nil, nil)
	s.jobs.mu.Unlock()
	if errors.Is(err, errStateChanged) {
		return
	}
	if err != nil {
		s.log.Error("finish job", "job", j.ID, "err", err)
		return
	}
	s.jobFinished(ctx, hubActor.id, j.ID)
}

// jobFinished runs after a job reached its final state: audit, the result
// message (not for a job cancelled before it reached a node), wake log
// waiters and the scheduler.
func (s *server) jobFinished(ctx context.Context, actorID, id string) {
	ctx = context.WithoutCancel(ctx)
	s.forgetJob(id)
	j, err := s.st.getJob(ctx, id)
	if err != nil {
		s.log.Error("reload job", "job", id, "err", err)
		return
	}
	arts, err := s.st.jobArtifacts(ctx, id)
	if err != nil {
		s.log.Error("job artifacts", "job", id, "err", err)
	}
	s.audit(ctx, actorID, "job.finish", "job:"+id, outcomeOK, map[string]any{
		"state": stateWord(j.State), "exit_code": j.ExitCode, "error": j.Error, "node": j.Node, "artifacts": arts,
		"log_bytes": j.LogBytes,
	})
	// A finished job's grants end with it.
	s.endGrantsWhere(ctx, grantFilter{subject: "job:" + id}, hubActor.id, "job finished", true)
	cancelledInQueue := j.State == mycelv1.JobState_JOB_STATE_CANCELLED && j.Node == ""
	if j.Spec.GetChannel() != "" && !cancelledInQueue {
		s.postJobResult(ctx, j, arts)
	}
	s.signalJob(id)
	s.kickScheduler()
}

// submitterActor rebuilds the actor that submitted j; false when it may no
// longer act (user removed; agent revoked, stopped or paused).
func (s *server) submitterActor(ctx context.Context, j *jobRow) (actor, bool) {
	if uid, ok := strings.CutPrefix(j.SubmittedBy, "user:"); ok {
		u, err := s.st.getUser(ctx, uid)
		if err != nil {
			return actor{}, false
		}
		return userActor(u), true
	}
	ag, err := s.st.getAgent(ctx, j.SubmittedBy)
	if err != nil || ag.RevokedAt != "" || ag.State != stateRunning {
		return actor{}, false
	}
	return actor{
		id: ag.ID, p: policy.Principal{Kind: policy.KindAgent, ID: ag.ID, OwnerID: ag.OwnerID},
		origin: originAgent, node: ag.NodeName, sandbox: ag.SandboxID,
	}, true
}

// postJobResult posts a RESULT message from the hub to the job's channel,
// on behalf of the submitter: only while the submitter may still post
// there, within the submitter's rate limit, and marked untrusted because
// it carries submitter- and node-supplied text.
func (s *server) postJobResult(ctx context.Context, j *jobRow, arts []string) {
	skip := func(reason string) {
		s.log.Info("job result not posted", "job", j.ID, "channel", j.Spec.GetChannel(), "reason", reason)
	}
	ch, err := s.st.getChannel(ctx, j.Spec.GetChannel())
	if err != nil {
		skip("channel not found")
		return
	}
	sub, ok := s.submitterActor(ctx, j)
	if !ok {
		skip("submitter may no longer act")
		return
	}
	if err := s.postAllowed(ctx, sub, ch); err != nil {
		skip(err.Error())
		return
	}
	pol := channels.WithDefaults(ch.Policy, ch.Type)
	if !s.ch.limiter.Allow(sub.id, ch.Name, pol.GetRatePerMinute()) {
		skip("submitter rate limit")
		return
	}
	label := j.Spec.GetName()
	if label == "" {
		label = j.ID
	}
	text := fmt.Sprintf("job %s (%s): %s, exit code %d", label, j.ID, stateWord(j.State), j.ExitCode)
	if j.Error != "" {
		text += ": " + strings.ReplaceAll(cleanText(j.Error, maxResultError), "\n", " ")
	}
	var parts []*mycelv1.Part
	for _, id := range arts {
		if len(parts) == maxParts {
			text += fmt.Sprintf(" (%d more artifacts)", len(arts)-maxParts)
			break
		}
		name := ""
		if a, err := s.st.getArtifact(ctx, id); err == nil {
			name = a.Name
		}
		parts = append(parts, &mycelv1.Part{Kind: "artifact", Ref: id, Name: name})
	}
	now := time.Now()
	msg := &mycelv1.Message{
		Id: channels.NewMessageID(now), Channel: ch.Name, From: hubActor.id, Type: mycelv1.MessageType_MESSAGE_TYPE_RESULT,
		Payload: &mycelv1.Payload{Text: text, Parts: parts, Tags: []string{"job"}},
		Meta: &mycelv1.Meta{MaxHops: pol.GetMaxHops(), Provenance: &mycelv1.Provenance{
			Origin: originHub, Node: j.Node, Sandbox: j.SandboxID, Untrusted: true,
		}},
		Ts: formatTime(now),
	}
	msg.Meta.TraceId = msg.Id
	if err := s.commitMessage(ctx, ch, msg); err != nil {
		s.log.Warn("job result message", "job", j.ID, "err", err)
	}
}

// ---- cancel ----

// cancelJob cancels a job for a: a queued job ends at once; a starting or
// running one gets a KillSandbox and ends with the node's job.done, or
// after cancelConfirm.
func (s *server) cancelJob(ctx context.Context, a actor, id string) (*mycelv1.Job, error) {
	j, err := s.loadJob(ctx, a, policy.ActionJobCancel, id)
	if err != nil {
		return nil, err
	}
	if j.finished() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("job already %s", stateWord(j.State)))
	}
	reason := "cancelled by " + a.id
	s.jobs.mu.Lock()
	err = s.st.finishJob(ctx, id, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_QUEUED}, mycelv1.JobState_JOB_STATE_CANCELLED, 0, reason, nil, nil)
	if err == nil {
		s.jobs.mu.Unlock()
		s.audit(ctx, a.auditID(), string(policy.ActionJobCancel), "job:"+id, outcomeOK, map[string]any{"state": "queued"})
		s.jobFinished(ctx, a.auditID(), id)
		return s.reloadJob(ctx, id)
	}
	if !errors.Is(err, errStateChanged) {
		s.jobs.mu.Unlock()
		return nil, s.internalError("cancel job", err)
	}
	j, err = s.st.getJob(ctx, id)
	if err != nil {
		s.jobs.mu.Unlock()
		return nil, s.internalError("get job", err)
	}
	first := false
	if !j.finished() && j.CancelRequestedAt == "" {
		err = s.st.updateJob(ctx, id, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_STARTING, mycelv1.JobState_JOB_STATE_RUNNING},
			"cancel_requested_at = ?", nowNano())
		if err != nil && !errors.Is(err, errStateChanged) {
			s.jobs.mu.Unlock()
			return nil, s.internalError("cancel job", err)
		}
		first = err == nil
	}
	s.jobs.mu.Unlock()
	if first {
		s.killSandbox(j.Node, id, reason)
		s.audit(ctx, a.auditID(), string(policy.ActionJobCancel), "job:"+id, outcomeOK, map[string]any{"state": stateWord(j.State), "node": j.Node})
		s.kickScheduler()
	}
	return s.reloadJob(ctx, id)
}

func (s *server) reloadJob(ctx context.Context, id string) (*mycelv1.Job, error) {
	j, err := s.st.getJob(ctx, id)
	if err != nil {
		return nil, s.internalError("get job", err)
	}
	return s.jobProto(ctx, j)
}

// ---- node RPCs ----

func (s *server) jobsDir() string { return filepath.Join(s.dir, "jobs") }

func (s *server) jobLogPath(id string) string { return filepath.Join(s.jobsDir(), id, "log") }

// assignedTo reports whether j is assigned to this enrolment of node: a
// node re-enrolled under the same name never matches its predecessor's
// jobs.
func (s *server) assignedTo(ctx context.Context, j *jobRow, node string) (bool, error) {
	if j.Node != node || j.NodeIncarnation == "" {
		return false, nil
	}
	inc, err := s.st.nodeIncarnation(ctx, node)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return inc == j.NodeIncarnation, nil
}

// jobForNode loads a job reported on by node. A finished job of this node
// is returned too; callers answer "job finished".
func (s *server) jobForNode(ctx context.Context, node, method, id string) (*jobRow, error) {
	refuse := func(reason string) error {
		s.auditDenied(ctx, "node:"+node, "rpc."+method, "job:"+id, map[string]any{"reason": reason})
		return rpcErr(connect.CodePermissionDenied, "job not assigned to this node")
	}
	if !validJobID(id) {
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid job id")
	}
	j, err := s.st.getJob(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, refuse("unknown job")
	}
	if err != nil {
		return nil, s.internalError("get job", err)
	}
	ok, err := s.assignedTo(ctx, j, node)
	if err != nil {
		return nil, s.internalError("node incarnation", err)
	}
	if !ok {
		return nil, refuse("job assigned to another node")
	}
	return j, nil
}

// errJobFinished tells a node to stop work on a job (its daemon treats it
// as a kill).
func errJobFinished() error { return rpcErr(connect.CodeFailedPrecondition, "job finished") }

// serveJobRPC serves the node's job.started, job.log and job.done.
func (s *server) serveJobRPC(ctx context.Context, node, method string, data []byte) (proto.Message, error) {
	switch method {
	case bus.RPCJobStarted:
		var req mycelv1.JobStartedRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
		}
		return s.jobStarted(ctx, node, &req)
	case bus.RPCJobLog:
		var req mycelv1.JobLogChunk
		if err := proto.Unmarshal(data, &req); err != nil {
			return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
		}
		return s.jobLog(ctx, node, &req)
	case bus.RPCJobDone:
		var req mycelv1.JobDoneRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
		}
		return s.jobDone(ctx, node, &req)
	}
	return nil, rpcErr(connect.CodeNotFound, "unknown method %s", method)
}

func validBackendName(b string) bool { return b != "" && validHarness(b) }

func (s *server) jobStarted(ctx context.Context, node string, req *mycelv1.JobStartedRequest) (proto.Message, error) {
	sbx, backend := req.GetSandboxId(), req.GetBackend()
	if sbx == "" || !validSandboxID(sbx) {
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid sandbox id")
	}
	if !validBackendName(backend) {
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid backend")
	}
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	j, err := s.jobForNode(ctx, node, bus.RPCJobStarted, req.GetJobId())
	if err != nil {
		return nil, err
	}
	switch j.State {
	case mycelv1.JobState_JOB_STATE_RUNNING:
		if j.SandboxID == sbx && j.Backend == backend {
			return &mycelv1.JobStartedResponse{}, nil
		}
		return nil, rpcErr(connect.CodeFailedPrecondition, "job already running in %s", j.SandboxID)
	case mycelv1.JobState_JOB_STATE_STARTING:
	default:
		return nil, errJobFinished()
	}
	prof, err := s.st.getProfile(ctx, j.Spec.GetProfile(), j.ProfileVersion)
	if err != nil {
		return nil, s.internalError("get profile", err)
	}
	if !slices.Contains(prof.GetBackends(), backend) {
		return nil, rpcErr(connect.CodeInvalidArgument, "backend %s is not allowed by profile %s", backend, prof.GetName())
	}
	err = s.st.updateJob(ctx, j.ID, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_STARTING},
		"state = ?, sandbox_id = ?, backend = ?, started_at = ?", int32(mycelv1.JobState_JOB_STATE_RUNNING), sbx, backend, nowNano())
	if err != nil {
		return nil, s.internalError("start job", err)
	}
	s.audit(ctx, "node:"+node, "job.start", "job:"+j.ID, outcomeOK, map[string]any{"sandbox_id": sbx, "backend": backend})
	s.signalJob(j.ID)
	return &mycelv1.JobStartedResponse{}, nil
}

// lowDisk reports whether free space on the hub's volume is below the
// floor (unknown free space counts as enough).
func (s *server) lowDisk() bool {
	if s.jobs.cfg.minFree == 0 {
		return false
	}
	free, err := diskFree(s.dir)
	return err == nil && free < s.jobs.cfg.minFree
}

// jobLog stores one log chunk. Chunks are taken strictly in sequence: a
// duplicate or a gap is answered with the last stored sequence, so the
// node resends from there.
func (s *server) jobLog(ctx context.Context, node string, req *mycelv1.JobLogChunk) (proto.Message, error) {
	if len(req.GetData()) > maxLogChunk {
		return nil, rpcErr(connect.CodeInvalidArgument, "log chunk exceeds %d bytes", maxLogChunk)
	}
	if req.GetSeq() == 0 {
		return nil, rpcErr(connect.CodeInvalidArgument, "log sequence starts at 1")
	}
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	j, err := s.jobForNode(ctx, node, bus.RPCJobLog, req.GetJobId())
	if err != nil {
		return nil, err
	}
	if j.finished() {
		return nil, errJobFinished()
	}
	if req.GetSeq() != j.LogSeq+1 {
		return &mycelv1.JobLogAck{Seq: j.LogSeq}, nil
	}
	maxLog := s.jobs.cfg.maxJobLog
	data := req.GetData()
	if room := maxLog - min(j.LogBytes, maxLog); uint64(len(data)) > room {
		data = data[:room]
	}
	if len(data) > 0 {
		if s.lowDisk() {
			return nil, rpcErr(connect.CodeResourceExhausted, "hub disk space low; retry later")
		}
		if err := s.appendLog(j.ID, j.LogBytes, data); err != nil {
			return nil, s.internalError("append log", err)
		}
	}
	if err := s.st.setLog(ctx, j.ID, j.LogBytes+uint64(len(data)), req.GetSeq()); err != nil {
		return nil, s.internalError("store log position", err)
	}
	if len(data) > 0 {
		m := nats.NewMsg(bus.JobLogSubject(j.ID))
		m.Header.Set("Mycel-Offset", fmt.Sprint(j.LogBytes))
		m.Data = data
		if err := s.nc.PublishMsg(m); err != nil {
			s.log.Debug("job log publish", "job", j.ID, "err", err)
		}
		s.signalJob(j.ID)
	}
	return &mycelv1.JobLogAck{Seq: req.GetSeq()}, nil
}

// appendLog writes data at offset in the job's log file and syncs it before
// the caller records the new length (writing at the stored length heals a
// partial write before a crash).
func (s *server) appendLog(id string, offset uint64, data []byte) error {
	dir := filepath.Join(s.jobsDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, int64(offset)); err != nil { //nolint:gosec // bounded by the log cap
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readLog reads up to n bytes of a job's log from offset.
func (s *server) readLog(id string, offset uint64, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(s.jobLogPath(id))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, int64(offset)) //nolint:gosec // bounded by the log cap
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:got], nil
}

var finalStates = []mycelv1.JobState{
	mycelv1.JobState_JOB_STATE_SUCCEEDED, mycelv1.JobState_JOB_STATE_FAILED,
	mycelv1.JobState_JOB_STATE_CANCELLED, mycelv1.JobState_JOB_STATE_TIMED_OUT,
}

func (s *server) jobDone(ctx context.Context, node string, req *mycelv1.JobDoneRequest) (proto.Message, error) {
	if !slices.Contains(finalStates, req.GetState()) {
		return nil, rpcErr(connect.CodeInvalidArgument, "state must be succeeded, failed, cancelled or timed_out")
	}
	if len(req.GetArtifacts()) > maxJobArtifacts {
		return nil, rpcErr(connect.CodeInvalidArgument, "at most %d artifacts", maxJobArtifacts)
	}
	for _, a := range req.GetArtifacts() {
		if !validArtifactID(a) {
			return nil, rpcErr(connect.CodeInvalidArgument, "invalid artifact id")
		}
	}
	s.jobs.mu.Lock()
	j, err := s.jobForNode(ctx, node, bus.RPCJobDone, req.GetJobId())
	if err != nil {
		s.jobs.mu.Unlock()
		return nil, err
	}
	if j.finished() {
		// A repeated report (or one after the hub gave up waiting).
		s.jobs.mu.Unlock()
		return &mycelv1.JobDoneResponse{}, nil
	}
	arts := slices.Compact(slices.Sorted(slices.Values(req.GetArtifacts())))
	for _, id := range arts {
		a, err := s.st.getArtifact(ctx, id)
		if errors.Is(err, errNotFound) || (err == nil && a.JobID != j.ID) {
			s.jobs.mu.Unlock()
			return nil, rpcErr(connect.CodeInvalidArgument, "artifact %s does not belong to this job", id)
		}
		if err != nil {
			s.jobs.mu.Unlock()
			return nil, s.internalError("get artifact", err)
		}
	}
	state := req.GetState()
	errText := cleanText(req.GetError(), maxJobError)
	if j.CancelRequestedAt != "" {
		state = mycelv1.JobState_JOB_STATE_CANCELLED
		if errText == "" {
			errText = "cancelled"
		}
	}
	var stats *mycelv1.JobStats
	if st := req.GetStats(); st != nil {
		stats = &mycelv1.JobStats{
			CpuSecondsMilli: max(st.GetCpuSecondsMilli(), 0), MaxMemoryBytes: max(st.GetMaxMemoryBytes(), 0),
			EgressBytes: max(st.GetEgressBytes(), 0),
		}
	}
	err = s.st.finishJob(ctx, j.ID, []mycelv1.JobState{mycelv1.JobState_JOB_STATE_STARTING, mycelv1.JobState_JOB_STATE_RUNNING},
		state, req.GetExitCode(), errText, stats, arts)
	s.jobs.mu.Unlock()
	if errors.Is(err, errStateChanged) {
		return &mycelv1.JobDoneResponse{}, nil
	}
	if err != nil {
		return nil, s.internalError("finish job", err)
	}
	s.jobFinished(ctx, "node:"+node, j.ID)
	return &mycelv1.JobDoneResponse{}, nil
}

// ---- logs for agents ----

func (s *server) agentJobLogs(ctx context.Context, a actor, req *mycelv1.AgentJobLogsRequest) (*mycelv1.AgentJobLogsResponse, error) {
	j, err := s.loadJob(ctx, a, policy.ActionJobLogs, req.GetId())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetMaxBytes())
	if limit <= 0 {
		limit = defaultAgentLogBytes
	}
	limit = min(limit, maxAgentLogBytes)
	wait := min(max(time.Duration(req.GetWaitMs())*time.Millisecond, 0), maxLogWait)
	from := req.GetFromByte()
	if from > j.LogBytes {
		return nil, invalidArg("from_byte %d is beyond the end of the log (%d bytes)", from, j.LogBytes)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woke, unsub := s.ch.notify.subscribe(jobKey(j.ID))
		j, err = s.st.getJob(ctx, j.ID)
		if err != nil {
			unsub()
			return nil, s.internalError("get job", err)
		}
		if from < j.LogBytes {
			unsub()
			data, err := s.readLog(j.ID, from, int(min(uint64(limit), j.LogBytes-from))) //nolint:gosec // at most maxAgentLogBytes
			if err != nil {
				return nil, s.internalError("read log", err)
			}
			return &mycelv1.AgentJobLogsResponse{Data: data, NextByte: from + uint64(len(data)), State: j.State}, nil
		}
		if j.finished() || wait == 0 {
			unsub()
			return &mycelv1.AgentJobLogsResponse{NextByte: from, State: j.State}, nil
		}
		select {
		case <-woke:
			unsub()
		case <-deadline.C:
			unsub()
			wait = 0
		case <-ctx.Done():
			unsub()
			return &mycelv1.AgentJobLogsResponse{NextByte: from, State: j.State}, nil
		case <-s.ch.stop:
			unsub()
			return &mycelv1.AgentJobLogsResponse{NextByte: from, State: j.State}, nil
		}
	}
}

// ---- listing ----

// artifactVisibility is the SQL filter of what a may list; nil for admins.
// Users see what they own and what is attached to channels they may read;
// agents see artifacts in channels they are members of, created by them,
// or produced by jobs they submitted.
func (s *server) artifactVisibility(ctx context.Context, a actor) (*artifactFilter, error) {
	if a.isAdmin() {
		return nil, nil
	}
	if a.p.Kind == policy.KindAgent {
		ms, err := s.st.memberships(ctx, a.id)
		if err != nil {
			return nil, s.internalError("memberships", err)
		}
		f := &artifactFilter{creator: a.id}
		for _, m := range ms {
			f.channels = append(f.channels, m.Channel)
		}
		if f.jobs, err = s.st.jobIDsSubmittedBy(ctx, a.id); err != nil {
			return nil, s.internalError("jobs", err)
		}
		return f, nil
	}
	all, err := s.st.listChannels(ctx)
	if err != nil {
		return nil, s.internalError("list channels", err)
	}
	f := &artifactFilter{owner: a.p.ID}
	for _, ch := range all {
		_, d, err := s.checkChannel(ctx, a, policy.ActionChannelRead, ch)
		if err != nil {
			return nil, err
		}
		if d.Allow {
			f.channels = append(f.channels, ch.Name)
		}
	}
	return f, nil
}

// visibleArtifacts lists artifacts a may read, newest first.
func (s *server) visibleArtifacts(ctx context.Context, a actor, req *mycelv1.ListArtifactsRequest) ([]*mycelv1.Artifact, error) {
	if err := s.agentOrUserAllowed(ctx, a, policy.ActionArtifactList); err != nil {
		return nil, err
	}
	if id := req.GetJobId(); id != "" && !validJobID(id) {
		return nil, invalidArg("invalid job id %q", id)
	}
	if name := req.GetChannel(); name != "" && !channels.ValidName(name) && !channels.IsDirect(name) {
		return nil, invalidArg("invalid channel name %q", name)
	}
	vis, err := s.artifactVisibility(ctx, a)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.listArtifacts(ctx, req.GetJobId(), req.GetChannel(), vis, clampLimit(req.GetLimit(), defaultList, maxList))
	if err != nil {
		return nil, s.internalError("list artifacts", err)
	}
	out := []*mycelv1.Artifact{}
	for _, r := range rows {
		out = append(out, r.toProto())
	}
	return out, nil
}

// agentOrUserAllowed asks the policy about an action without a single
// target and returns PermissionDenied on a deny.
func (s *server) agentOrUserAllowed(ctx context.Context, a actor, action policy.Action) error {
	if d := s.policy.Authorize(ctx, a.p, action, policy.Resource{}); !d.Allow {
		return connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
	}
	return nil
}
