package hub

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker/egress"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// Grants (spec: "Grant", "Runtime escalation", "Approvals"; docs/decisions.md
// D024). A grant is requested by a member for a subject it owns or by an
// agent for itself, validated against the node it attaches to (the
// subject's node), and then either auto-approved by a rule from hub.yaml
// (policy.GrantDecider) or left PENDING with an approval card in
// #approvals. An ACTIVE grant is sent to its node as AttachGrant through
// the node's control queue; the grant loop below expires grants, and
// syncNode brings a node that (re)connects up to date.

// Grant kinds as named in rules and errors.
const (
	kindOllama     = "ollama"
	kindGPU        = "gpu"
	kindDockerHost = "docker-host"
	kindMount      = "mount"
	kindEgress     = "egress"
)

var grantKinds = []string{kindOllama, kindGPU, kindDockerHost, kindMount, kindEgress}

// grantConfig holds the grant timings of one running hub. pendingTTL comes
// from hub.yaml (grants.pending_ttl).
type grantConfig struct {
	maxWait      time.Duration // longest sleep of the grant loop
	pendingTTL   time.Duration // a PENDING grant without a decision expires after this
	pruneEvery   time.Duration
	retention    time.Duration // finished grants are forgotten after this
	keepFinished int           // at most this many finished grants are kept
}

var defaultGrantConfig = grantConfig{
	maxWait: 30 * time.Second, pendingTTL: 24 * time.Hour, pruneEvery: time.Hour,
	retention: 7 * 24 * time.Hour, keepFinished: 2000,
}

// Grant limits.
const (
	defaultGrantTTL        = time.Hour
	maxGrantReason         = 1024 // raw request bytes; stored cleaned, at most maxReasonLen
	maxPendingPerSubject   = 16
	maxOpenGrantsPerOwner  = 128 // pending + active
	grantRequestsPerMinute = 30  // per owner
	maxGrantList           = 500
	maxOllamaModels        = 64
	maxOllamaRPM           = 100_000
	maxDockerImages        = 64
	maxImageRef            = 256
	maxDockerCPUs          = 1024
	maxEgressRules         = 64
	maxMountPath           = 4096
	maxCapString           = 256 // list entries and other short strings
	maxUsageRequests       = 1_000_000
	maxUsageGPUSeconds     = 7 * 24 * 3600
	rateKeyGrants          = "*grants"
)

// grantState is the in-memory grant machinery of a running hub.
type grantState struct {
	cfg   grantConfig
	rules []policy.GrantRule
	// mu serialises grant transitions that read before they write (the
	// count checks of a request, decisions, expiry, revocation).
	mu   sync.Mutex
	kick chan struct{}

	lastPrune time.Time // grant loop only
}

func newGrantState(cfg grantConfig, rules []policy.GrantRule) *grantState {
	return &grantState{cfg: cfg, rules: rules, kick: make(chan struct{}, 1)}
}

func (s *server) kickGrants() {
	select {
	case s.grants.kick <- struct{}{}:
	default:
	}
}

func validGrantID(id string) bool { return validPrefixedID(id, "grt_") }

func grantKind(spec *mycelv1.GrantSpec) string {
	switch spec.GetKind().(type) {
	case *mycelv1.GrantSpec_Ollama:
		return kindOllama
	case *mycelv1.GrantSpec_Gpu:
		return kindGPU
	case *mycelv1.GrantSpec_DockerHost:
		return kindDockerHost
	case *mycelv1.GrantSpec_Mount:
		return kindMount
	case *mycelv1.GrantSpec_Egress:
		return kindEgress
	}
	return ""
}

// kindOffer is the sharing offer a node must make for a grant kind. Mounts
// and egress rules attach to sandboxes.
func kindOffer(kind string) string {
	switch kind {
	case kindOllama:
		return "ollama"
	case kindGPU:
		return "gpu"
	case kindDockerHost:
		return "docker"
	default:
		return "sandbox"
	}
}

func grantStateWord(st mycelv1.GrantState) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "GRANT_STATE_"))
}

// grantOpen reports whether a grant is still pending or active.
func grantOpen(g *mycelv1.Grant) bool {
	return g.GetState() == mycelv1.GrantState_GRANT_STATE_PENDING || g.GetState() == mycelv1.GrantState_GRANT_STATE_ACTIVE
}

var openStates = []mycelv1.GrantState{mycelv1.GrantState_GRANT_STATE_PENDING, mycelv1.GrantState_GRANT_STATE_ACTIVE}

// parseGrantTTL parses a requested duration: default 1h, at most 24h.
func parseGrantTTL(v string) (time.Duration, error) {
	if v == "" {
		return defaultGrantTTL, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, invalidArg("invalid duration %q", v)
	}
	if d > policy.MaxGrantTTL {
		return 0, invalidArg("duration %s exceeds the maximum of %s", d, policy.MaxGrantTTL)
	}
	return d, nil
}

// ---- spec validation ----

var (
	ollamaModelRE = regexp.MustCompile(`^[a-z0-9._:/-]{1,128}$`)
	migSliceRE    = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	registryRE    = regexp.MustCompile(`^(localhost|[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+|[A-Za-z0-9-]+:[0-9]{1,5}|[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+:[0-9]{1,5})$`)
	imagePathRE   = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	imageTagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imageDigestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// validImageRef accepts an OCI image reference ("[registry/]path[:tag][@digest]")
// or a registry/repository prefix ending in "/".
func validImageRef(ref string) bool {
	if ref == "" || len(ref) > maxImageRef {
		return false
	}
	prefix := strings.HasSuffix(ref, "/")
	rest := strings.TrimSuffix(ref, "/")
	digest := ""
	if i := strings.Index(rest, "@"); i >= 0 {
		rest, digest = rest[:i], rest[i+1:]
		if prefix || !imageDigestRE.MatchString(digest) {
			return false
		}
	}
	comps := strings.Split(rest, "/")
	if len(comps) == 0 || comps[0] == "" {
		return false
	}
	// A first component with a dot, a port or "localhost" is a registry.
	if len(comps) > 1 || prefix {
		if c := comps[0]; strings.ContainsAny(c, ".:") || c == "localhost" {
			if !registryRE.MatchString(c) {
				return false
			}
			comps = comps[1:]
		}
	}
	if len(comps) == 0 {
		return prefix // "ghcr.io/" allows the whole registry
	}
	last := len(comps) - 1
	for i, c := range comps {
		if i == last && !prefix {
			if name, tag, ok := strings.Cut(c, ":"); ok {
				if !imageTagRE.MatchString(tag) {
					return false
				}
				c = name
			}
		}
		if !imagePathRE.MatchString(c) {
			return false
		}
	}
	return true
}

// nodeInventory decodes a node's stored inventory (nil when none).
func nodeInventory(n *nodeRow) *mycelv1.Inventory {
	if len(n.Inventory) == 0 {
		return nil
	}
	var inv mycelv1.Inventory
	if proto.Unmarshal(n.Inventory, &inv) != nil {
		return nil
	}
	return &inv
}

// validGuestPath applies the guest-side mount rules lexically (the same
// rules sandbox.ValidateMount applies once the host path exists).
func validGuestPath(g string) error {
	if g == "" || !path.IsAbs(g) || len(g) > maxMountPath || !channels.IsPrintable(g) || strings.ContainsAny(g, "\\\x00") {
		return fmt.Errorf("guest path %q must be an absolute path", g)
	}
	g = path.Clean(g)
	if g == "/" {
		return errors.New("guest path must not be /")
	}
	if g == sandbox.WorkspaceGuestPath || strings.HasPrefix(g, sandbox.WorkspaceGuestPath+"/") {
		return fmt.Errorf("guest path %q is under %s", g, sandbox.WorkspaceGuestPath)
	}
	top := strings.SplitN(strings.TrimPrefix(g, "/"), "/", 2)[0]
	switch {
	case top == "proc", top == "sys", top == "dev", top == "etc", top == "usr", top == "bin", top == "sbin",
		top == "run", top == "boot", strings.HasPrefix(top, "lib"):
		return fmt.Errorf("guest path %q shadows the system directory /%s", g, top)
	}
	return nil
}

// Host paths a mount may never name (lower-cased), as roots or trees, and
// path segments that are never mounted (credential folders). The hub
// checks these lexically only: it cannot see the node's file system, home
// directory or symlinks; the node resolves and re-validates every mount
// (sandbox.ResolveMount) before it attaches one.
var (
	mountForbiddenExact = []string{
		"/", "/var", "/library", "/private", "/private/var", "/users", "/home", "/opt", "/applications",
		"/tmp", "/private/tmp", "/var/tmp", "/volumes", "/mnt", "/media",
	}
	mountForbiddenTree = []string{
		"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/boot", "/dev", "/proc", "/sys",
		"/run", "/var/run", "/var/lib", "/var/root", "/root", "/private/etc", "/private/var/run", "/private/var/db",
		"/private/var/root", "/system", "/library/keychains", "/windows", "/program files", "/program files (x86)",
		"/programdata",
	}
	mountForbiddenSegments = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".colima", ".orbstack", ".lima", "gcloud", "keychains"}
)

// validMountHost checks a mount's host path lexically for a node running
// nodeOS ("windows" or a POSIX system).
func validMountHost(m *mycelv1.MountGrant, nodeOS string) error {
	host := m.GetHostPath()
	if host == "" || len(host) > maxMountPath || !channels.IsPrintable(host) || strings.ContainsRune(host, 0) {
		return errors.New("host path must be a non-empty absolute path")
	}
	segs := strings.FieldsFunc(host, func(r rune) bool { return r == '/' || r == '\\' })
	for _, seg := range segs {
		if seg == ".." || seg == "." {
			return fmt.Errorf("host path %q has a . or .. component", host)
		}
		if slices.Contains(mountForbiddenSegments, strings.ToLower(seg)) {
			return fmt.Errorf("host path %q names a credential folder", host)
		}
	}
	var p string // lower-case, slash-separated, without a drive letter
	if nodeOS == "windows" {
		if len(host) < 3 || host[1] != ':' || host[2] != '\\' {
			return fmt.Errorf("host path %q must be a drive path like C:\\dir", host)
		}
		p = "/" + strings.ToLower(strings.Join(segs[1:], "/"))
	} else {
		if !strings.HasPrefix(host, "/") || strings.Contains(host, "\\") {
			return fmt.Errorf("host path %q must be an absolute path", host)
		}
		p = "/" + strings.ToLower(strings.Join(segs, "/"))
	}
	if slices.Contains(mountForbiddenExact, p) {
		return fmt.Errorf("host path %q is a system root", host)
	}
	for _, t := range mountForbiddenTree {
		if p == t || strings.HasPrefix(p, t+"/") {
			return fmt.Errorf("host path %q is inside the system tree %s", host, t)
		}
	}
	// A home directory itself (/Users/<name>, /home/<name>) is never mounted.
	if len(segs) == 2 && (p == "/users/"+strings.ToLower(segs[1]) || p == "/home/"+strings.ToLower(segs[1])) {
		return fmt.Errorf("host path %q is a home directory", host)
	}
	return nil
}

// validateGrantSpec checks spec for node and returns the normalised spec.
func validateGrantSpec(in *mycelv1.GrantSpec, n *nodeRow) (*mycelv1.GrantSpec, error) {
	spec := proto.CloneOf(in)
	switch k := spec.GetKind().(type) {
	case *mycelv1.GrantSpec_Ollama:
		o := k.Ollama
		if o == nil {
			return nil, invalidArg("ollama: missing spec")
		}
		if len(o.GetModels()) > maxOllamaModels {
			return nil, invalidArg("ollama: at most %d models", maxOllamaModels)
		}
		for _, m := range o.GetModels() {
			if !ollamaModelRE.MatchString(m) {
				return nil, invalidArg("ollama: invalid model name %q: 1-128 chars of a-z, 0-9, '.', '_', ':', '/' and '-'", m)
			}
		}
		if o.GetRequestsPerMinute() < 0 || o.GetRequestsPerMinute() > maxOllamaRPM {
			return nil, invalidArg("ollama: requests_per_minute must be between 0 and %d", maxOllamaRPM)
		}
		if o.GetMaxTokens() < 0 {
			return nil, invalidArg("ollama: max_tokens must not be negative")
		}
	case *mycelv1.GrantSpec_Gpu:
		g := k.Gpu
		if g == nil {
			return nil, invalidArg("gpu: missing spec")
		}
		if g.GetMigSlice() != "" && !migSliceRE.MatchString(g.GetMigSlice()) {
			return nil, invalidArg("gpu: invalid mig_slice")
		}
		inv := nodeInventory(n)
		found := false
		for _, d := range inv.GetGpus() {
			if d.GetIndex() == g.GetDevice() {
				found = true
				break
			}
		}
		if !found {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("gpu: node %s reports no GPU with index %d", n.Name, g.GetDevice()))
		}
	case *mycelv1.GrantSpec_DockerHost:
		d := k.DockerHost
		if d == nil || len(d.GetImages()) == 0 {
			return nil, invalidArg("docker-host: at least one allowed image or registry prefix is required")
		}
		if len(d.GetImages()) > maxDockerImages {
			return nil, invalidArg("docker-host: at most %d images", maxDockerImages)
		}
		for _, img := range d.GetImages() {
			if !validImageRef(img) {
				return nil, invalidArg("docker-host: %q is not an image reference or a registry prefix ending in '/'", img)
			}
		}
		if d.GetCpus() < 0 || d.GetCpus() > maxDockerCPUs {
			return nil, invalidArg("docker-host: cpus must be between 0 and %d", maxDockerCPUs)
		}
		if d.GetMemoryBytes() < 0 {
			return nil, invalidArg("docker-host: memory_bytes must not be negative")
		}
	case *mycelv1.GrantSpec_Mount:
		m := k.Mount
		if m == nil {
			return nil, invalidArg("mount: missing spec")
		}
		guest := m.GetGuestPath()
		if guest == "" {
			base := path.Base(strings.ReplaceAll(m.GetHostPath(), "\\", "/"))
			if base == "" || base == "/" || base == "." || !imagePathRE.MatchString(strings.ToLower(base)) {
				base = "data"
			}
			guest = "/mnt/" + strings.ToLower(base)
		}
		if err := validGuestPath(guest); err != nil {
			return nil, invalidArg("mount: %v", err)
		}
		m.GuestPath = path.Clean(guest)
		nodeOS := ""
		if inv := nodeInventory(n); inv != nil {
			nodeOS = inv.GetOs()
		}
		if err := validMountHost(m, nodeOS); err != nil {
			return nil, invalidArg("mount: %v", err)
		}
	case *mycelv1.GrantSpec_Egress:
		e := k.Egress
		if e == nil || len(e.GetAllow()) == 0 {
			return nil, invalidArg("egress: at least one allow rule is required")
		}
		if len(e.GetAllow()) > maxEgressRules {
			return nil, invalidArg("egress: at most %d rules", maxEgressRules)
		}
		for i, r := range e.GetAllow() {
			if len(r) > maxCapString || strings.ContainsRune(r, 0) {
				return nil, invalidArg("egress: rules are at most %d bytes", maxCapString)
			}
			norm, err := egress.ParseRule(r)
			if err != nil {
				return nil, invalidArg("egress: rule %q: %v", r, err)
			}
			e.Allow[i] = string(norm)
		}
	default:
		return nil, invalidArg("grant spec needs one of ollama, gpu, docker_host, mount, egress")
	}
	return spec, nil
}

// ---- subjects and visibility ----

// grantSubject is the resolved holder of a grant.
type grantSubject struct {
	id         string // "agent:<name>@<node>" or "job:<id>"
	owner      string
	node       string
	agent      *agentRow // set for agents
	jobChannel string    // set for jobs: the channel the job was submitted for
}

// resolveSubject loads a subject and the node its grants attach to.
func (s *server) resolveSubject(ctx context.Context, id string) (*grantSubject, error) {
	switch {
	case strings.HasPrefix(id, "agent:"):
		if !channels.ValidParticipant(id) {
			return nil, invalidArg("invalid subject %q", id)
		}
		a, err := s.st.getAgent(ctx, id)
		if errors.Is(err, errNotFound) || (err == nil && a.RevokedAt != "") {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %s not found", id))
		}
		if err != nil {
			return nil, s.internalError("get agent", err)
		}
		return &grantSubject{id: id, owner: a.OwnerID, node: a.NodeName, agent: a}, nil
	case strings.HasPrefix(id, "job:"):
		jid := strings.TrimPrefix(id, "job:")
		if !validJobID(jid) {
			return nil, invalidArg("invalid subject %q", id)
		}
		j, err := s.st.getJob(ctx, jid)
		if errors.Is(err, errNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("job %s not found", jid))
		}
		if err != nil {
			return nil, s.internalError("get job", err)
		}
		switch j.State {
		case mycelv1.JobState_JOB_STATE_STARTING, mycelv1.JobState_JOB_STATE_RUNNING:
		case mycelv1.JobState_JOB_STATE_QUEUED:
			// The node fixes a sandbox's environment, and with it the
			// broker credentials, when it creates the sandbox: a job that
			// needs a grant from its first command gets it while it waits
			// in the queue, which works once it is pinned to its node.
			if pin := j.Spec.GetNode(); pin != "" {
				return &grantSubject{id: id, owner: j.OwnerID, node: pin, jobChannel: j.Spec.GetChannel()}, nil
			}
			return &grantSubject{id: id, owner: j.OwnerID}, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("job %s is not on a node yet; pin it to a node or request the grant once it is starting or running", jid))
		default:
			return &grantSubject{id: id, owner: j.OwnerID}, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("job %s has finished", jid))
		}
		return &grantSubject{id: id, owner: j.OwnerID, node: j.Node, jobChannel: j.Spec.GetChannel()}, nil
	case id == "":
		return nil, invalidArg("a grant needs a subject: an agent or a job you own")
	default:
		return nil, invalidArg("invalid subject %q: want agent:<name>@<node> or job:<id>", id)
	}
}

// nodeOwner returns the owner of a node, "" when it is gone.
func (s *server) nodeOwner(ctx context.Context, node string) (string, error) {
	n, err := s.st.getNode(ctx, node)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	if err != nil {
		return "", s.internalError("get node", err)
	}
	return n.OwnerID, nil
}

// userGrantAllowed reports whether u may do action on g as the subject's
// owner or as the owner of g's node (admins always).
func (s *server) userGrantAllowed(ctx context.Context, u *userRow, action policy.Action, g *mycelv1.Grant) (bool, string, error) {
	p := principalOf(u)
	d := s.policy.Authorize(ctx, p, action, policy.Resource{Kind: "grant", ID: g.GetId(), OwnerID: g.GetOwnerId()})
	if d.Allow {
		return true, "", nil
	}
	owner, err := s.nodeOwner(ctx, g.GetNode())
	if err != nil {
		return false, "", err
	}
	// The owner of the node may see and revoke what attaches to it.
	nodeAction := action
	if action == policy.ActionGrantGet {
		nodeAction = policy.ActionGrantApprove
	}
	if owner != "" {
		if d2 := s.policy.Authorize(ctx, p, nodeAction, policy.Resource{Kind: "grant", ID: g.GetId(), OwnerID: owner}); d2.Allow {
			return true, "", nil
		}
	}
	return false, d.Reason, nil
}

// ---- requests ----

// grantRequest is a request after decoding, from a user or an agent.
type grantRequest struct {
	subject  string
	spec     *mycelv1.GrantSpec
	duration string
	reason   string
	channel  string
}

// grantRefusal is a request refused for a reason worth auditing.
func (s *server) grantRefusal(ctx context.Context, a actor, err error) error {
	reason := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		reason = ce.Message()
	}
	s.auditDenied(ctx, a.auditID(), string(policy.ActionGrantRequest), "grant", map[string]any{"reason": reason})
	return err
}

// requestGrant checks, stores and decides a grant request from a.
func (s *server) requestGrant(ctx context.Context, a actor, req grantRequest) (*mycelv1.Grant, error) {
	if s.frozenAll() {
		return nil, s.grantRefusal(ctx, a, connect.NewError(connect.CodeFailedPrecondition, errors.New("network frozen")))
	}
	if req.spec == nil {
		return nil, invalidArg("missing grant spec")
	}
	kind := grantKind(req.spec)
	if kind == "" {
		return nil, invalidArg("grant spec needs one of ollama, gpu, docker_host, mount, egress")
	}
	ttl, err := parseGrantTTL(req.duration)
	if err != nil {
		return nil, err
	}
	if len(req.reason) > maxGrantReason || strings.ContainsRune(req.reason, 0) {
		return nil, invalidArg("reason must be at most %d bytes without NUL", maxGrantReason)
	}
	if len(req.subject) > maxCapString || strings.ContainsRune(req.subject, 0) {
		return nil, invalidArg("invalid subject")
	}
	reason := cleanReason(req.reason)
	subj, err := s.resolveSubject(ctx, req.subject)
	// Authorise before revealing anything about the subject's state.
	owner := ""
	if subj != nil {
		owner = subj.owner
	}
	res := policy.Resource{Kind: "grant", ID: req.subject, OwnerID: owner}
	if d := s.policy.Authorize(ctx, a.p, policy.ActionGrantRequest, res); !d.Allow {
		s.auditDenied(ctx, a.auditID(), string(policy.ActionGrantRequest), "grant", map[string]any{"reason": d.Reason, "subject": req.subject})
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
	}
	if err != nil {
		return nil, err
	}
	if !s.ch.limiter.Allow(subj.owner, rateKeyGrants, grantRequestsPerMinute) {
		return nil, s.grantRefusal(ctx, a, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("at most %d grant requests per minute per owner", grantRequestsPerMinute)))
	}
	if subj.agent != nil && subj.agent.HomeChannel != "" && s.frozenChannel(subj.agent.HomeChannel) {
		return nil, s.grantRefusal(ctx, a, connect.NewError(connect.CodeFailedPrecondition, errors.New("channel frozen")))
	}
	channel, err := s.grantChannel(ctx, subj, req.channel)
	if err != nil {
		return nil, s.grantRefusal(ctx, a, err)
	}
	n, err := s.st.getNode(ctx, subj.node)
	if errors.Is(err, errNotFound) {
		return nil, s.grantRefusal(ctx, a, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("node %s not found", subj.node)))
	}
	if err != nil {
		return nil, s.internalError("get node", err)
	}
	offers, err := s.effectiveOffers(ctx, n)
	if err != nil {
		return nil, err
	}
	if offer := kindOffer(kind); !slices.Contains(offers, offer) {
		return nil, s.grantRefusal(ctx, a, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("node %s does not offer %s (offers: %s)", n.Name, offer, strings.Join(offers, ", "))))
	}
	spec, err := validateGrantSpec(req.spec, n)
	if err != nil {
		return nil, s.grantRefusal(ctx, a, err)
	}
	now := time.Now()
	g := &mycelv1.Grant{
		Id: newULIDID("grt_"), Spec: spec, Subject: subj.id, Node: n.Name, OwnerId: subj.owner, RequestedBy: a.auditID(),
		Reason: reason, Duration: ttl.String(), State: mycelv1.GrantState_GRANT_STATE_PENDING, CreatedAt: formatNano(now),
		Channel: channel, RequiresApproval: true,
	}
	var rule *policy.GrantRule
	s.grants.mu.Lock()
	err = func() error {
		open, err := s.st.listGrants(ctx, grantFilter{states: openStates, owner: subj.owner})
		if err != nil {
			return s.internalError("list grants", err)
		}
		if len(open) >= maxOpenGrantsPerOwner {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d pending or active grants per owner", maxOpenGrantsPerOwner))
		}
		pending, active := 0, 0
		for _, o := range open {
			if o.GetSubject() != subj.id {
				continue
			}
			if o.GetState() == mycelv1.GrantState_GRANT_STATE_PENDING {
				pending++
			} else if grantKind(o.GetSpec()) == kind {
				active++
			}
		}
		if pending >= maxPendingPerSubject {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d pending grants per subject", maxPendingPerSubject))
		}
		needs := true
		var r *policy.GrantRule
		if dec, ok := s.policy.(policy.GrantDecider); ok {
			needs, _, r = dec.NeedsApproval(policy.GrantRequestInfo{
				Channel: channel, Kind: kind, Node: n.Name, TTL: ttl.String(),
				ReadOnlyMount: spec.GetMount().GetReadOnly(), ActiveOfKind: active, Rules: s.grants.rules,
			})
		}
		if !needs {
			rule = r
			g.State, g.RequiresApproval, g.DecidedBy = mycelv1.GrantState_GRANT_STATE_ACTIVE, false, "policy"
			g.ActivatedAt, g.ExpiresAt = formatNano(now), formatNano(now.Add(ttl))
			g.DecisionNote = "auto-approved by rule " + describeRule(r)
		}
		if err := s.st.insertGrant(ctx, g); err != nil {
			return s.internalError("insert grant", err)
		}
		if !needs {
			s.attachGrant(g)
		}
		return nil
	}()
	s.grants.mu.Unlock()
	if err != nil {
		return nil, err
	}
	details := map[string]any{
		"kind": kind, "subject": g.Subject, "node": g.Node, "owner_id": g.OwnerId, "duration": g.Duration,
		"channel": g.Channel, "state": grantStateWord(g.State),
	}
	if rule != nil {
		details["rule"] = describeRule(rule)
	}
	s.audit(ctx, a.auditID(), string(policy.ActionGrantRequest), "grant:"+g.Id, outcomeOK, details)
	if g.State == mycelv1.GrantState_GRANT_STATE_PENDING {
		s.postApprovalRequest(ctx, g)
	}
	s.kickGrants() // a new expiry, active or pending

	return proto.CloneOf(g), nil
}

// describeRule renders a rule for notes and audit entries.
func describeRule(r *policy.GrantRule) string {
	if r == nil {
		return ""
	}
	out := fmt.Sprintf("channel=%s kind=%s node=%s", r.Channel, r.Kind, r.Node)
	if r.MaxTTL != "" {
		out += " max_ttl=" + r.MaxTTL
	}
	return out
}

// maxGrantChannel bounds the channel name of a grant request.
const maxGrantChannel = 128

// grantChannel resolves the request's channel. It is policy context
// supplied by the requester (auto rules match on it), so it counts only
// when someone with authority put the subject there: the agent's home
// channel, a membership added by the channel owner, an admin or the
// spawner (never a self-join), or the channel a job was submitted for.
// Otherwise the request is treated as having no channel. A frozen channel
// takes no requests.
func (s *server) grantChannel(ctx context.Context, subj *grantSubject, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if len(name) > maxGrantChannel || strings.ContainsRune(name, 0) || !channels.ValidName(name) {
		return "", invalidArg("invalid channel name")
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	if err != nil {
		return "", s.internalError("get channel", err)
	}
	if ch.ArchivedAt != "" {
		return "", nil
	}
	belongs := false
	switch {
	case subj.agent != nil:
		if subj.agent.HomeChannel == name {
			belongs = true
		} else if m, err := s.st.getMember(ctx, name, subj.agent.ID); err == nil {
			belongs = m.AddedBy != "" && m.AddedBy != addedBySelf
		} else if !errors.Is(err, errNotFound) {
			return "", s.internalError("get member", err)
		}
	case subj.jobChannel != "":
		belongs = subj.jobChannel == name
	}
	if !belongs {
		return "", nil
	}
	if s.frozenChannel(name) {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("channel frozen"))
	}
	return name, nil
}

// decisionCheck re-checks a pending grant before it becomes active: no
// freeze applies (network, the grant's channel, the subject's home
// channel), the subject still exists and is running on the grant's node,
// and the node still offers the kind and accepts the spec.
func (s *server) decisionCheck(ctx context.Context, g *mycelv1.Grant) error {
	precondition := func(format string, args ...any) error {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
	}
	if s.frozenAll() {
		return precondition("network frozen")
	}
	if ch := g.GetChannel(); ch != "" && s.frozenChannel(ch) {
		return precondition("channel frozen")
	}
	subj, err := s.resolveSubject(ctx, g.GetSubject())
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Code() != connect.CodeInternal {
			return precondition("subject %s is gone: %s", g.GetSubject(), ce.Message())
		}
		return err
	}
	if a := subj.agent; a != nil {
		if a.State != stateRunning {
			return precondition("agent %s is %s", a.ID, a.State)
		}
		if a.HomeChannel != "" && s.frozenChannel(a.HomeChannel) {
			return precondition("channel frozen")
		}
	}
	if subj.node != g.GetNode() {
		return precondition("subject %s is no longer on node %s", g.GetSubject(), g.GetNode())
	}
	n, err := s.st.getNode(ctx, g.GetNode())
	if errors.Is(err, errNotFound) {
		return precondition("node %s not found", g.GetNode())
	}
	if err != nil {
		return s.internalError("get node", err)
	}
	offers, err := s.effectiveOffers(ctx, n)
	if err != nil {
		return err
	}
	if offer := kindOffer(grantKind(g.GetSpec())); !slices.Contains(offers, offer) {
		return precondition("node %s no longer offers %s", n.Name, offer)
	}
	if _, err := validateGrantSpec(g.GetSpec(), n); err != nil {
		return err
	}
	return nil
}

func formatNano(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// attachGrant queues AttachGrant for an active grant's node.
func (s *server) attachGrant(g *mycelv1.Grant) {
	gid, node := g.GetId(), g.GetNode()
	s.enqueueCtl(node, ctlItem{
		req:    &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_AttachGrant{AttachGrant: &mycelv1.AttachGrant{Grant: proto.CloneOf(g)}}},
		dedupe: "attach:" + gid,
		done: func(resp *mycelv1.CtlResponse, err error) {
			if err != nil || resp.GetOk() {
				return // not answered: the next sync of the node sends it again
			}
			// The node refused it (its local ceiling, a missing device, ...).
			why := cleanReason(resp.GetError())
			if why == "" {
				why = "refused by the node"
			}
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			by := "node:" + node
			rg, ferr := s.finishGrantNoDetach(ctx, gid, mycelv1.GrantState_GRANT_STATE_REVOKED, by, why, mycelv1.GrantState_GRANT_STATE_ACTIVE)
			if ferr != nil {
				return
			}
			s.audit(ctx, by, string(policy.ActionGrantRevoke), "grant:"+gid, outcomeOK, map[string]any{
				"subject": rg.GetSubject(), "reason": why, "via": "attach refused",
			})
			s.announceRevoke(ctx, rg, by, why)
		},
	})
}

// detachGrant queues DetachGrant for a grant's node.
func (s *server) detachGrant(g *mycelv1.Grant, reason string) {
	s.enqueueCtl(g.GetNode(), ctlItem{critical: true, req: &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_DetachGrant{
		DetachGrant: &mycelv1.DetachGrant{GrantId: g.GetId(), Reason: reason},
	}}})
}

// grantSummary is the one-line description used in messages.
func grantSummary(g *mycelv1.Grant) string {
	kind := grantKind(g.GetSpec())
	detail := ""
	switch k := g.GetSpec().GetKind().(type) {
	case *mycelv1.GrantSpec_Ollama:
		if len(k.Ollama.GetModels()) > 0 {
			detail = " models " + strings.Join(k.Ollama.GetModels(), ",")
		}
	case *mycelv1.GrantSpec_Gpu:
		detail = fmt.Sprintf(" device %d", k.Gpu.GetDevice())
		if k.Gpu.GetExclusive() {
			detail += " exclusive"
		}
	case *mycelv1.GrantSpec_DockerHost:
		detail = " images " + strings.Join(k.DockerHost.GetImages(), ",")
	case *mycelv1.GrantSpec_Mount:
		mode := "read-write"
		if k.Mount.GetReadOnly() {
			mode = "read-only"
		}
		detail = fmt.Sprintf(" %s -> %s %s", k.Mount.GetHostPath(), k.Mount.GetGuestPath(), mode)
	case *mycelv1.GrantSpec_Egress:
		detail = " allow " + strings.Join(k.Egress.GetAllow(), ",")
	}
	return channels.SanitizeLine(kind + detail)
}

// postApprovalRequest posts the approval card for a pending grant in
// #approvals and an EVENT in the request's channel. The card carries only
// hub-checked fields (id, kind, subject, node, duration, requester); the
// reason, paths and lists are on the approvals page and in GetGrant.
func (s *server) postApprovalRequest(ctx context.Context, g *mycelv1.Grant) {
	text := fmt.Sprintf("grant %s requires approval\nkind: %s\nsubject: %s\nnode: %s\nduration: %s\nrequested by: %s\nrequires_approval: true",
		g.GetId(), grantKind(g.GetSpec()), g.GetSubject(), g.GetNode(), g.GetDuration(), g.GetRequestedBy())
	s.hubMessage(ctx, channels.Approvals, hubMsg{
		typ: mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_REQUEST, text: text, node: g.GetNode(),
		parts: []*mycelv1.Part{{Kind: "grant", Ref: g.GetId()}}, tags: []string{"grant", "requires-approval"},
	})
	if ch := g.GetChannel(); ch != "" && s.ch.limiter.Allow(rateKeyHub, ch, hubEventsPerMinute) {
		s.hubEvent(ctx, ch, "", fmt.Sprintf("grant %s (%s for %s on %s) is waiting for approval in %s",
			g.GetId(), grantKind(g.GetSpec()), g.GetSubject(), g.GetNode(), channels.Approvals), 0)
	}
}

// hubMsg is a message the hub posts outside the event path.
type hubMsg struct {
	typ       mycelv1.MessageType
	text      string
	to        []string
	parts     []*mycelv1.Part
	tags      []string
	untrusted bool
	node      string
}

// hubMessage posts m in channel from the hub; failures are logged.
func (s *server) hubMessage(ctx context.Context, channel string, m hubMsg) {
	ctx = context.WithoutCancel(ctx)
	ch, err := s.st.getChannel(ctx, channel)
	if err != nil {
		s.log.Warn("hub message: channel", "channel", channel, "err", err)
		return
	}
	if ch.ArchivedAt != "" {
		return
	}
	pol := channels.WithDefaults(ch.Policy, ch.Type)
	now := time.Now()
	text := m.text
	if len(text) > channels.MaxTextBytes {
		text = cleanText(text, channels.MaxTextBytes)
	}
	msg := &mycelv1.Message{
		Id: channels.NewMessageID(now), Channel: ch.Name, From: hubActor.id, To: m.to, Type: m.typ,
		Payload: &mycelv1.Payload{Text: text, Parts: m.parts, Tags: m.tags},
		Meta: &mycelv1.Meta{MaxHops: pol.GetMaxHops(), Provenance: &mycelv1.Provenance{
			Origin: originHub, Untrusted: m.untrusted, Node: m.node,
		}},
		Ts: formatTime(now),
	}
	msg.Meta.TraceId = msg.Id
	if err := s.commitMessage(ctx, ch, msg); err != nil {
		s.log.Warn("hub message: store", "channel", channel, "err", err)
	}
}

// notifyAgent sends a direct message from the hub to an agent, in the
// direct channel between the agent and its owner, so the agent's inbox and
// its owner both see it.
func (s *server) notifyAgent(ctx context.Context, agentID string, typ mycelv1.MessageType, text string, untrusted bool, g *mycelv1.Grant) {
	ctx = context.WithoutCancel(ctx)
	a, err := s.st.getAgent(ctx, agentID)
	if err != nil || a.RevokedAt != "" {
		return
	}
	user := userMemberID(a.OwnerID)
	dm := channels.DirectName(a.ID, user)
	if !channels.IsDirect(dm) {
		return
	}
	row := &channelRow{
		Name: dm, Type: mycelv1.ChannelType_CHANNEL_TYPE_DIRECT, Creator: hubActor.id,
		Policy: &mycelv1.ChannelPolicy{}, CreatedAt: nowString(),
	}
	if _, err := s.st.ensureChannel(ctx, row, []memberRow{{Member: a.ID, Role: memberAgent, AddedBy: hubActor.id}, {Member: user, Role: memberHuman, AddedBy: hubActor.id}}); err != nil {
		s.log.Warn("notify agent: direct channel", "agent", a.ID, "err", err)
		return
	}
	s.hubMessage(ctx, dm, hubMsg{
		typ: typ, text: text, to: []string{a.ID}, untrusted: untrusted, node: g.GetNode(),
		parts: []*mycelv1.Part{{Kind: "grant", Ref: g.GetId()}}, tags: []string{"grant"},
	})
}

// ---- decisions ----

// loadGrantFor loads a grant for a user decision on it.
func (s *server) loadGrant(ctx context.Context, u *userRow, action policy.Action, id string) (*mycelv1.Grant, error) {
	if !validGrantID(id) {
		return nil, invalidArg("invalid grant id %q", id)
	}
	g, err := s.st.getGrant(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, s.notFoundAfterAuth(ctx, u, action, "grant", id)
	}
	if err != nil {
		return nil, s.internalError("get grant", err)
	}
	return g, nil
}

// authorizeDecision checks approve or deny on g for u: admins and the
// owner of the node g attaches to.
func (s *server) authorizeDecision(ctx context.Context, u *userRow, action policy.Action, g *mycelv1.Grant) error {
	owner, err := s.nodeOwner(ctx, g.GetNode())
	if err != nil {
		return err
	}
	return s.authorize(ctx, u, action, policy.Resource{Kind: "grant", ID: g.GetId(), OwnerID: owner}, true)
}

// approveGrant activates a pending grant; duration may shorten it.
func (s *server) approveGrant(ctx context.Context, u *userRow, id, duration, note string) (*mycelv1.Grant, error) {
	g, err := s.loadGrant(ctx, u, policy.ActionGrantApprove, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDecision(ctx, u, policy.ActionGrantApprove, g); err != nil {
		return nil, err
	}
	requested, err := time.ParseDuration(g.GetDuration())
	if err != nil || requested <= 0 {
		requested = defaultGrantTTL
	}
	ttl := requested
	if duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil || d <= 0 {
			return nil, invalidArg("invalid duration %q", duration)
		}
		if d > requested {
			return nil, invalidArg("an approval may only shorten the requested duration of %s", requested)
		}
		ttl = d
	}
	note = cleanReason(note)
	s.grants.mu.Lock()
	err = func() error {
		cur, err := s.st.getGrant(ctx, id)
		if err != nil {
			return s.internalError("get grant", err)
		}
		if cur.GetState() != mycelv1.GrantState_GRANT_STATE_PENDING {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("grant is %s", grantStateWord(cur.GetState())))
		}
		if err := s.decisionCheck(ctx, cur); err != nil {
			reason := err.Error()
			var ce *connect.Error
			if errors.As(err, &ce) {
				reason = ce.Message()
			}
			s.audit(ctx, u.ID, string(policy.ActionGrantApprove), "grant:"+id, outcomeError, map[string]any{"reason": reason})
			return err
		}
		now := time.Now()
		cur.State, cur.Duration, cur.DecidedBy, cur.DecisionNote = mycelv1.GrantState_GRANT_STATE_ACTIVE, ttl.String(), u.ID, note
		cur.ActivatedAt, cur.ExpiresAt = formatNano(now), formatNano(now.Add(ttl))
		if err := s.st.updateGrant(ctx, cur, mycelv1.GrantState_GRANT_STATE_PENDING); err != nil {
			if errors.Is(err, errStateChanged) {
				return connect.NewError(connect.CodeFailedPrecondition, errors.New("grant is no longer pending"))
			}
			return s.internalError("approve grant", err)
		}
		g = cur
		s.attachGrant(g)
		return nil
	}()
	s.grants.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.kickGrants()
	s.audit(ctx, u.ID, string(policy.ActionGrantApprove), "grant:"+id, outcomeOK, map[string]any{
		"subject": g.Subject, "node": g.Node, "duration": g.Duration, "expires_at": g.ExpiresAt, "note": note,
	})
	s.announceDecision(ctx, g, "approved", u.ID, note)
	return proto.CloneOf(g), nil
}

// denyGrant refuses a pending grant.
func (s *server) denyGrant(ctx context.Context, u *userRow, id, note string) (*mycelv1.Grant, error) {
	g, err := s.loadGrant(ctx, u, policy.ActionGrantDeny, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDecision(ctx, u, policy.ActionGrantDeny, g); err != nil {
		return nil, err
	}
	note = cleanReason(note)
	g, err = s.finishGrant(ctx, id, mycelv1.GrantState_GRANT_STATE_DENIED, u.ID, note, mycelv1.GrantState_GRANT_STATE_PENDING)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, u.ID, string(policy.ActionGrantDeny), "grant:"+id, outcomeOK, map[string]any{
		"subject": g.Subject, "node": g.Node, "note": note,
	})
	s.announceDecision(ctx, g, "denied", u.ID, note)
	return proto.CloneOf(g), nil
}

// finishGrant moves a grant in one of from to state (DENIED, REVOKED,
// EXPIRED), sending DetachGrant when it was active.
func (s *server) finishGrant(ctx context.Context, id string, state mycelv1.GrantState, by, note string,
	from ...mycelv1.GrantState) (*mycelv1.Grant, error) {
	return s.finishGrantDetach(ctx, id, state, by, note, true, from...)
}

// finishGrantNoDetach is finishGrant for grants the node already dropped.
func (s *server) finishGrantNoDetach(ctx context.Context, id string, state mycelv1.GrantState, by, note string,
	from ...mycelv1.GrantState) (*mycelv1.Grant, error) {
	return s.finishGrantDetach(ctx, id, state, by, note, false, from...)
}

func (s *server) finishGrantDetach(ctx context.Context, id string, state mycelv1.GrantState, by, note string, detach bool,
	from ...mycelv1.GrantState) (*mycelv1.Grant, error) {
	s.grants.mu.Lock()
	defer s.grants.mu.Unlock()
	cur, err := s.st.getGrant(ctx, id)
	if err != nil {
		return nil, s.internalError("get grant", err)
	}
	if !slices.Contains(from, cur.GetState()) {
		return cur, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("grant is %s", grantStateWord(cur.GetState())))
	}
	was := cur.GetState()
	cur.State, cur.DecidedBy, cur.DecisionNote = state, by, note
	if err := s.st.updateGrant(ctx, cur, was); err != nil {
		if errors.Is(err, errStateChanged) {
			return cur, connect.NewError(connect.CodeFailedPrecondition, errors.New("grant changed meanwhile"))
		}
		return nil, s.internalError("update grant", err)
	}
	if detach && was == mycelv1.GrantState_GRANT_STATE_ACTIVE {
		s.detachGrant(cur, detachReason(state))
	}
	return cur, nil
}

func detachReason(state mycelv1.GrantState) string {
	if state == mycelv1.GrantState_GRANT_STATE_EXPIRED {
		return "expired"
	}
	return "revoked"
}

// announceDecision posts an approval_decision in #approvals and, when an
// agent requested the grant, sends it a direct message. The note (free
// text) goes only into the direct message.
func (s *server) announceDecision(ctx context.Context, g *mycelv1.Grant, verb, by, note string) {
	text := fmt.Sprintf("grant %s %s by %s (%s for %s on %s)", g.GetId(), verb, by, grantKind(g.GetSpec()), g.GetSubject(), g.GetNode())
	if g.GetState() == mycelv1.GrantState_GRANT_STATE_ACTIVE {
		text += fmt.Sprintf("; active until %s", g.GetExpiresAt())
	}
	s.hubMessage(ctx, channels.Approvals, hubMsg{
		typ: mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION, text: text, node: g.GetNode(),
		parts: []*mycelv1.Part{{Kind: "grant", Ref: g.GetId()}}, tags: []string{"grant"},
	})
	if strings.HasPrefix(g.GetRequestedBy(), "agent:") {
		dm := text
		if note != "" {
			dm += "\nnote: " + note
		}
		s.notifyAgent(ctx, g.GetRequestedBy(), mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION, dm, note != "", g)
	}
}

// revokeGrant revokes a pending or active grant for u (subject owner, node
// owner or admin).
func (s *server) revokeGrant(ctx context.Context, u *userRow, id, note string) (*mycelv1.Grant, error) {
	g, err := s.loadGrant(ctx, u, policy.ActionGrantRevoke, id)
	if err != nil {
		return nil, err
	}
	ok, reason, err := s.userGrantAllowed(ctx, u, policy.ActionGrantRevoke, g)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.audit(ctx, u.ID, string(policy.ActionGrantRevoke), "grant:"+id, outcomeDenied, map[string]any{"reason": reason})
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(reason))
	}
	note = cleanReason(note)
	g, err = s.finishGrant(ctx, id, mycelv1.GrantState_GRANT_STATE_REVOKED, u.ID, note, openStates...)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, u.ID, string(policy.ActionGrantRevoke), "grant:"+id, outcomeOK, map[string]any{
		"subject": g.Subject, "node": g.Node, "note": note,
	})
	s.announceRevoke(ctx, g, u.ID, note)
	return proto.CloneOf(g), nil
}

// announceRevoke tells a requesting agent that its grant ended.
func (s *server) announceRevoke(ctx context.Context, g *mycelv1.Grant, by, note string) {
	if !strings.HasPrefix(g.GetRequestedBy(), "agent:") {
		return
	}
	text := fmt.Sprintf("grant %s revoked by %s (%s on %s)", g.GetId(), by, grantKind(g.GetSpec()), g.GetNode())
	if note != "" {
		text += "\nnote: " + note
	}
	s.notifyAgent(ctx, g.GetRequestedBy(), mycelv1.MessageType_MESSAGE_TYPE_EVENT, text, note != "", g)
}

// endGrantsWhere revokes every open grant matching f (and pick, when
// given): node removed, agent revoked, job finished, offer withdrawn. It
// reports how many ended.
func (s *server) endGrantsWhere(ctx context.Context, f grantFilter, by, note string, detach bool, pick ...func(*mycelv1.Grant) bool) int {
	ctx = context.WithoutCancel(ctx)
	f.states = openStates
	gs, err := s.st.listGrants(ctx, f)
	if err != nil {
		s.log.Error("list grants to end", "err", err)
		return 0
	}
	n := 0
	for _, g := range gs {
		if len(pick) > 0 && !pick[0](g) {
			continue
		}
		eg, err := s.finishGrantDetach(ctx, g.GetId(), mycelv1.GrantState_GRANT_STATE_REVOKED, by, note, detach, openStates...)
		if err != nil {
			continue
		}
		n++
		s.audit(ctx, by, string(policy.ActionGrantRevoke), "grant:"+eg.GetId(), outcomeOK, map[string]any{
			"subject": eg.GetSubject(), "node": eg.GetNode(), "note": note,
		})
		s.announceRevoke(ctx, eg, by, note)
	}
	return n
}

// ---- the grant loop: expiry ----

// startGrantLoop runs expiry and pruning until the hub stops.
func (s *server) startGrantLoop(ctx context.Context) {
	s.background(func() {
		for {
			wait := s.expireOnce(ctx)
			t := time.NewTimer(wait)
			select {
			case <-s.ch.stop:
				t.Stop()
				return
			case <-s.grants.kick:
			case <-t.C:
			}
			t.Stop()
		}
	})
}

// background runs fn on a goroutine the hub waits for when it stops; false
// when the hub is stopping.
func (s *server) background(fn func()) bool {
	c := s.ch
	c.workMu.Lock()
	if c.closing {
		c.workMu.Unlock()
		return false
	}
	c.work.Add(1)
	c.workMu.Unlock()
	go func() {
		defer c.work.Done()
		fn()
	}()
	return true
}

// expireOnce expires due active grants and stale pending ones, prunes old
// finished grants, and returns how long to wait for the next expiry.
func (s *server) expireOnce(ctx context.Context) time.Duration {
	cfg := s.grants.cfg
	next := cfg.maxWait
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("grant loop panic", "panic", r)
		}
	}()
	if time.Since(s.grants.lastPrune) >= cfg.pruneEvery {
		s.grants.lastPrune = time.Now()
		cutoff := formatNano(time.Now().Add(-cfg.retention))
		if n, err := s.st.pruneGrants(ctx, cutoff, cfg.keepFinished); err != nil {
			s.log.Error("prune grants", "err", err)
		} else if n > 0 {
			s.log.Info("pruned finished grants", "grants", n)
		}
	}
	gs, err := s.st.listGrants(ctx, grantFilter{states: openStates})
	if err != nil {
		s.log.Error("grant loop: list", "err", err)
		return next
	}
	now := time.Now()
	for _, g := range gs {
		var due time.Time
		if g.GetState() == mycelv1.GrantState_GRANT_STATE_ACTIVE {
			due, err = time.Parse(time.RFC3339Nano, g.GetExpiresAt())
		} else {
			due, err = time.Parse(time.RFC3339Nano, g.GetCreatedAt())
			due = due.Add(cfg.pendingTTL)
		}
		if err != nil {
			due = now
		}
		if due.After(now) {
			next = min(next, due.Sub(now))
			continue
		}
		note := ""
		if g.GetState() == mycelv1.GrantState_GRANT_STATE_PENDING {
			note = "no decision within " + cfg.pendingTTL.String()
		}
		eg, err := s.finishGrant(ctx, g.GetId(), mycelv1.GrantState_GRANT_STATE_EXPIRED, hubActor.id, note, g.GetState())
		if err != nil {
			continue
		}
		s.audit(ctx, hubActor.id, "grant.expire", "grant:"+eg.GetId(), outcomeOK, map[string]any{
			"subject": eg.GetSubject(), "node": eg.GetNode(), "expires_at": eg.GetExpiresAt(), "was_pending": note != "",
		})
		s.announceDecision(ctx, eg, "expired", hubActor.id, note)
	}
	return max(next, 10*time.Millisecond)
}

// ---- node sync ----

// syncState coalesces syncs per node: at most one runs, one more may wait.
type syncState struct {
	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
}

func newSyncState() *syncState {
	return &syncState{running: map[string]bool{}, again: map[string]bool{}}
}

// requestSync syncs node in the background (see syncNode). Every full
// inventory heartbeat, which a node sends after every (re)connect, asks
// for one.
func (s *server) requestSync(node string) {
	ss := s.syncs
	ss.mu.Lock()
	if ss.running[node] {
		ss.again[node] = true
		ss.mu.Unlock()
		return
	}
	ss.running[node] = true
	ss.mu.Unlock()
	ok := s.background(func() {
		for {
			s.syncNode(node)
			ss.mu.Lock()
			if !ss.again[node] {
				delete(ss.running, node)
				ss.mu.Unlock()
				return
			}
			delete(ss.again, node)
			ss.mu.Unlock()
		}
	})
	if !ok {
		ss.mu.Lock()
		delete(ss.running, node)
		ss.mu.Unlock()
	}
}

// forgetSync drops a removed node's sync state.
func (s *server) forgetSync(node string) {
	s.syncs.mu.Lock()
	delete(s.syncs.again, node)
	s.syncs.mu.Unlock()
}

// syncNode brings a node that (re)connected up to date: first (at the head
// of its control queue) the network freeze state, Freeze or Unfreeze; then
// AttachGrant for each active grant and DetachGrant for each grant that
// was attached once and ended before its expiry (the node may have missed
// it). Nodes apply all of them idempotently.
func (s *server) syncNode(node string) {
	// Under the freeze lock, so no freeze change queues between the read
	// and the enqueue.
	s.freeze.op.Lock()
	it := freezeCtl(false, "")
	if f := s.freezeAll(); f != nil {
		it = freezeCtl(true, f.Reason)
	}
	it.priority = true
	s.enqueueCtl(node, it)
	s.freeze.op.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	gs, err := s.st.listGrants(ctx, grantFilter{node: node, activated: true})
	if err != nil {
		s.log.Warn("sync node grants", "node", node, "err", err)
		return
	}
	now := time.Now()
	attached, detached := 0, 0
	for _, g := range gs {
		exp, err := time.Parse(time.RFC3339Nano, g.GetExpiresAt())
		if err != nil || !exp.After(now) {
			continue // past its expiry: the node drops it on its own clock
		}
		switch g.GetState() {
		case mycelv1.GrantState_GRANT_STATE_ACTIVE:
			s.attachGrant(g)
			attached++
		case mycelv1.GrantState_GRANT_STATE_REVOKED, mycelv1.GrantState_GRANT_STATE_EXPIRED, mycelv1.GrantState_GRANT_STATE_DENIED:
			s.detachGrant(g, detachReason(g.GetState()))
			detached++
		}
	}
	if attached > 0 || detached > 0 {
		s.audit(ctx, hubActor.id, "grant.reattach", "node:"+node, outcomeOK, map[string]any{"attached": attached, "detached": detached})
	}
}

// ---- node RPCs ----

// serveGrantRPC serves the phase 5 node-scoped RPCs.
func (s *server) serveGrantRPC(ctx context.Context, node, method string, data []byte) (proto.Message, error) {
	switch method {
	case "grant.revoked":
		var req mycelv1.GrantRevokedRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
		}
		return s.nodeGrantRevoked(ctx, node, &req)
	case "broker.usage":
		var req mycelv1.BrokerUsageRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
		}
		return s.brokerUsage(ctx, node, &req)
	case "node.sharing":
		var req mycelv1.NodeSharingRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return nil, rpcErr(connect.CodeInvalidArgument, "malformed request")
		}
		return s.nodeSharingReport(ctx, node, &req)
	}
	return nil, rpcErr(connect.CodeNotFound, "unknown method %s", method)
}

// nodeGrant loads a grant attached to node; grants of other nodes and
// unknown ids get the same answer.
func (s *server) nodeGrant(ctx context.Context, node, method, id string) (*mycelv1.Grant, error) {
	if !validGrantID(id) {
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid grant id")
	}
	g, err := s.st.getGrant(ctx, id)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, s.internalError("get grant", err)
	}
	if err != nil || g.GetNode() != node {
		s.auditDenied(ctx, "node:"+node, method, "grant:"+id, map[string]any{"reason": "grant not on this node"})
		return nil, rpcErr(connect.CodePermissionDenied, "grant not on this node")
	}
	return g, nil
}

// nodeGrantRevoked serves grant.revoked: the node's owner revoked a grant
// locally, or the node's ceiling rejected it.
func (s *server) nodeGrantRevoked(ctx context.Context, node string, req *mycelv1.GrantRevokedRequest) (proto.Message, error) {
	g, err := s.nodeGrant(ctx, node, "grant.revoked", req.GetGrantId())
	if err != nil {
		return nil, err
	}
	reason := cleanReason(req.GetReason())
	if !grantOpen(g) {
		return &mycelv1.GrantRevokedResponse{}, nil
	}
	by := "node:" + node
	// The node has detached it already; no DetachGrant goes back.
	s.grants.mu.Lock()
	was := g.GetState()
	g.State, g.DecidedBy, g.DecisionNote = mycelv1.GrantState_GRANT_STATE_REVOKED, by, reason
	err = s.st.updateGrant(ctx, g, was)
	s.grants.mu.Unlock()
	if errors.Is(err, errStateChanged) {
		return &mycelv1.GrantRevokedResponse{}, nil
	}
	if err != nil {
		return nil, s.internalError("revoke grant", err)
	}
	s.audit(ctx, by, string(policy.ActionGrantRevoke), "grant:"+g.GetId(), outcomeOK, map[string]any{
		"subject": g.GetSubject(), "reason": reason,
	})
	s.announceRevoke(ctx, g, by, reason)
	return &mycelv1.GrantRevokedResponse{}, nil
}

// brokerUsage serves broker.usage: usage increments for a grant on this
// node. The reply tells the broker to stop serving the grant when it is no
// longer active or a freeze applies.
func (s *server) brokerUsage(ctx context.Context, node string, req *mycelv1.BrokerUsageRequest) (proto.Message, error) {
	broker := req.GetBroker()
	if broker != kindOllama && broker != kindGPU && broker != kindDockerHost {
		return nil, rpcErr(connect.CodeInvalidArgument, "broker must be ollama, gpu or docker-host")
	}
	tokens, requests, gpu := req.GetTokens(), req.GetRequests(), req.GetGpuSeconds()
	if tokens < 0 || requests < 0 || gpu < 0 {
		return nil, rpcErr(connect.CodeInvalidArgument, "usage increments must not be negative")
	}
	if tokens > maxUsageTokens || requests > maxUsageRequests || gpu > maxUsageGPUSeconds {
		return nil, rpcErr(connect.CodeInvalidArgument, "usage report too large")
	}
	if len(req.GetModel()) > 128 || !channels.IsPrintable(req.GetModel()) {
		return nil, rpcErr(connect.CodeInvalidArgument, "invalid model")
	}
	g, err := s.nodeGrant(ctx, node, "broker.usage", req.GetGrantId())
	if err != nil {
		return nil, err
	}
	if sub := req.GetSubject(); sub != "" && sub != g.GetSubject() {
		s.auditDenied(ctx, "node:"+node, "broker.usage", "grant:"+g.GetId(), map[string]any{"reason": "subject mismatch", "subject": sub})
		return nil, rpcErr(connect.CodeInvalidArgument, "subject does not hold this grant")
	}
	if tokens > 0 || requests > 0 || gpu > 0 {
		if err := s.st.addGrantUsage(ctx, g.GetId(), g.GetSubject(), broker, tokens, requests, gpu); err != nil {
			return nil, s.internalError("grant usage", err)
		}
	}
	deny, why := false, ""
	switch {
	case g.GetState() != mycelv1.GrantState_GRANT_STATE_ACTIVE:
		deny, why = true, "grant "+grantStateWord(g.GetState())
	case s.grantExpired(g):
		deny, why = true, "grant expired"
	default:
		if f := s.grantFreeze(ctx, g); f != "" {
			deny, why = true, f
		} else if w, err := s.usageBlock(ctx, g); err != nil {
			return nil, err
		} else if w != "" {
			deny, why = true, w
		}
	}
	details := map[string]any{"broker": broker, "subject": g.GetSubject(), "tokens": tokens, "requests": requests,
		"gpu_seconds": gpu, "deny": deny}
	if req.GetModel() != "" {
		details["model"] = req.GetModel()
	}
	if why != "" {
		details["reason"] = why
	}
	s.audit(ctx, "node:"+node, "broker.usage", "grant:"+g.GetId(), outcomeOK, details)
	return &mycelv1.BrokerUsageResponse{Deny: deny}, nil
}

// usageBlock returns why a grant's broker must stop serving its subject,
// or "": the subject is paused, stopped, revoked or over budget (agents),
// no longer running (jobs), or the grant's token ceiling is used up.
func (s *server) usageBlock(ctx context.Context, g *mycelv1.Grant) (string, error) {
	switch sub := g.GetSubject(); {
	case strings.HasPrefix(sub, "agent:"):
		a, err := s.st.getAgent(ctx, sub)
		if errors.Is(err, errNotFound) {
			return "agent gone", nil
		}
		if err != nil {
			return "", s.internalError("get agent", err)
		}
		if a.RevokedAt != "" {
			return "agent revoked", nil
		}
		if a.State != stateRunning {
			return "agent " + a.State, nil
		}
		block, err := s.budgetBlock(ctx, a)
		if err != nil {
			return "", s.internalError("budget check", err)
		}
		if block != "" {
			return block, nil
		}
	case strings.HasPrefix(sub, "job:"):
		j, err := s.st.getJob(ctx, strings.TrimPrefix(sub, "job:"))
		if errors.Is(err, errNotFound) {
			return "job gone", nil
		}
		if err != nil {
			return "", s.internalError("get job", err)
		}
		switch j.State {
		case mycelv1.JobState_JOB_STATE_STARTING, mycelv1.JobState_JOB_STATE_RUNNING:
		case mycelv1.JobState_JOB_STATE_QUEUED:
			// Granted while queued for its pinned node (resolveSubject).
			if j.Spec.GetNode() != g.GetNode() {
				return "job " + stateWord(j.State), nil
			}
		default:
			return "job " + stateWord(j.State), nil
		}
	}
	if limit := g.GetSpec().GetOllama().GetMaxTokens(); limit > 0 {
		rows, err := s.st.grantUsage(ctx, g.GetId())
		if err != nil {
			return "", s.internalError("grant usage", err)
		}
		var used int64
		for _, r := range rows {
			if r.Broker == kindOllama {
				used += r.Tokens
			}
		}
		if used >= limit {
			return "token ceiling reached", nil
		}
	}
	return "", nil
}

func (s *server) grantExpired(g *mycelv1.Grant) bool {
	exp, err := time.Parse(time.RFC3339Nano, g.GetExpiresAt())
	return err != nil || !exp.After(time.Now())
}

// grantFreeze returns the freeze that stops g's brokers, or "": the
// network freeze, a freeze of the grant's request channel, or of its agent
// subject's home channel.
func (s *server) grantFreeze(ctx context.Context, g *mycelv1.Grant) string {
	if s.frozenAll() {
		return "network frozen"
	}
	if ch := g.GetChannel(); ch != "" && s.frozenChannel(ch) {
		return "channel frozen"
	}
	if strings.HasPrefix(g.GetSubject(), "agent:") {
		if a, err := s.st.getAgent(ctx, g.GetSubject()); err == nil && a.HomeChannel != "" && s.frozenChannel(a.HomeChannel) {
			return "channel frozen"
		}
	}
	return ""
}

// ---- agent RPCs ----

func (s *server) agentRequestGrant(ctx context.Context, a actor, r *mycelv1.AgentRequestGrantRequest) (*mycelv1.RequestGrantResponse, error) {
	g, err := s.requestGrant(ctx, a, grantRequest{
		subject: a.id, spec: r.GetSpec(), duration: r.GetDuration(), reason: r.GetReason(), channel: r.GetChannel(),
	})
	if err != nil {
		return nil, err
	}
	return &mycelv1.RequestGrantResponse{Grant: g}, nil
}

func (s *server) agentListGrants(ctx context.Context, a actor) (*mycelv1.ListGrantsResponse, error) {
	if err := s.agentAllowed(ctx, a, policy.ActionGrantList, policy.Resource{Kind: "grant", ID: a.id}); err != nil {
		return nil, err
	}
	gs, err := s.st.listGrants(ctx, grantFilter{states: openStates, subject: a.id, limit: maxGrantList})
	if err != nil {
		return nil, s.internalError("list grants", err)
	}
	return &mycelv1.ListGrantsResponse{Grants: gs}, nil
}
