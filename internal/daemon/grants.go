package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
	"github.com/richard-sen27/mycelium/internal/broker/docker"
	"github.com/richard-sen27/mycelium/internal/broker/egress"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/profiles"
)

// GrantsFile holds the grants attached on this node and the grant
// revocations not yet acknowledged by the hub (<NodeDir>/grants.json), so
// grants are re-attached after a restart before the hub resends them and a
// local revocation survives one.
const GrantsFile = "grants.json"

// Grant kinds that are not brokers.
const (
	kindMount  = "mount"
	kindEgress = "egress"
)

// Names sandboxes use for the node's brokers: the egress proxy maps them
// per sandbox to the broker listeners (Proxy.SetAliases).
const (
	ollamaHostName = "ollama.mycel.internal"
	ollamaAlias    = ollamaHostName + ":80"
	dockerHostName = "docker.mycel.internal"
	dockerAlias    = dockerHostName + ":2375"
)

const (
	revokedKeep      = 24 * time.Hour
	maxGrantRevoked  = 4096
	grantRevokedWait = 5 * time.Second
)

// Refusal texts and revocation reasons.
const (
	refusedByCeiling = "refused by local sharing settings"
	reasonCeiling    = "local ceiling"
	reasonLocal      = "revoked by the node owner"
	reasonRestart    = "not re-attached after restart: "
)

var grantIDRE = regexp.MustCompile(`^grt_[0-9A-Za-z]{1,64}$`)

// grantRec is one grant attached on this node.
type grantRec struct {
	g        *mycelv1.Grant
	kind     string
	expires  time.Time
	b        broker.Broker // broker kinds: the broker that holds it
	endpoint string        // broker kinds: what Attach returned
	mount    sandbox.Mount // mount: the resolved mount
	rules    []egress.Rule // egress: the parsed rules
	timer    *time.Timer   // local expiry
}

// revokedRec is a grant the node revoked or refused; the hub is told with
// grant.revoked until it acknowledges.
type revokedRec struct {
	ID        string    `json:"id"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
	At        time.Time `json:"at"`
	Reported  bool      `json:"reported"`
}

type grantsFile struct {
	Grants  []json.RawMessage `json:"grants"`
	Revoked []revokedRec      `json:"revoked,omitempty"`
}

// grantManager attaches the hub's grants to brokers and sandboxes.
type grantManager struct {
	n   *node
	log *slog.Logger
	b   *brokerSet

	attachMu sync.Mutex // one AttachGrant at a time

	mu        sync.Mutex
	closed    bool
	grants    map[string]*grantRec
	revoked   map[string]*revokedRec
	attaching string // id of the grant being attached, "" when none
	kick      chan struct{}
}

func newGrantManager(n *node) *grantManager {
	m := &grantManager{n: n, log: n.log.With("component", "grants"),
		grants: map[string]*grantRec{}, revoked: map[string]*revokedRec{}, kick: make(chan struct{}, 1)}
	m.b = startBrokers(n, n.sharing, m.gpuDevices)
	return m
}

func (m *grantManager) now() time.Time { return m.n.cfg.now() }

// grantKind is the spec kind of g ("" when unknown).
func grantKind(g *mycelv1.Grant) string {
	switch g.GetSpec().GetKind().(type) {
	case *mycelv1.GrantSpec_Ollama:
		return broker.KindOllama
	case *mycelv1.GrantSpec_Gpu:
		return broker.KindGPU
	case *mycelv1.GrantSpec_DockerHost:
		return broker.KindDockerHost
	case *mycelv1.GrantSpec_Mount:
		return kindMount
	case *mycelv1.GrantSpec_Egress:
		return kindEgress
	}
	return ""
}

// offerOf maps a grant kind to the sharing offer that gates it: mounts and
// egress rules only exist for sandboxes.
func offerOf(kind string) string {
	switch kind {
	case broker.KindDockerHost:
		return docker.OfferName
	case kindMount, kindEgress:
		return offerSandbox
	}
	return kind
}

func isBrokerKind(kind string) bool {
	return kind == broker.KindOllama || kind == broker.KindGPU || kind == broker.KindDockerHost
}

// ---- persistence ----

func (m *grantManager) path() string { return filepath.Join(m.n.dir, GrantsFile) }

// saveLocked writes grants.json; m.mu held.
func (m *grantManager) saveLocked() {
	f := grantsFile{Grants: []json.RawMessage{}}
	ids := make([]string, 0, len(m.grants))
	for id := range m.grants {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		b, err := protojson.Marshal(m.grants[id].g)
		if err != nil {
			continue
		}
		f.Grants = append(f.Grants, b)
	}
	for _, r := range m.revoked {
		f.Revoked = append(f.Revoked, *r)
	}
	slices.SortFunc(f.Revoked, func(a, b revokedRec) int { return strings.Compare(a.ID, b.ID) })
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = writeFileAtomic(m.path(), b)
	}
	if err != nil {
		m.log.Warn("store grants", "err", err)
	}
}

// restore re-attaches the grants of grants.json; called once at startup,
// before the bus connects. Broker availability comes from the startup
// probe. A grant that cannot be re-attached is reported with grant.revoked
// unless it expired; a schedule or idle requirement never revokes here.
func (m *grantManager) restore() {
	b, err := os.ReadFile(m.path())
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		m.log.Warn("read grants", "err", err)
		return
	}
	var f grantsFile
	if err := json.Unmarshal(b, &f); err != nil {
		m.log.Warn("grants file is invalid; ignoring it", "err", err)
		return
	}
	now := m.now()
	m.mu.Lock()
	for i := range f.Revoked {
		r := f.Revoked[i]
		if grantIDRE.MatchString(r.ID) && (now.Before(r.ExpiresAt.Add(revokedKeep)) || !r.Reported) {
			m.revoked[r.ID] = &r
		}
	}
	m.mu.Unlock()
	for _, raw := range f.Grants {
		g := &mycelv1.Grant{}
		if err := protojson.Unmarshal(raw, g); err != nil {
			m.log.Warn("grants file entry is invalid; skipping it")
			continue
		}
		e := m.attachGrant(g, true)
		if e == "" {
			m.log.Info("grant re-attached after restart", "grant", g.GetId())
			continue
		}
		m.log.Warn("grant not re-attached after restart", "grant", g.GetId(), "err", e)
		exp, perr := time.Parse(time.RFC3339Nano, g.GetExpiresAt())
		m.mu.Lock()
		_, known := m.revoked[g.GetId()]
		m.mu.Unlock()
		if grantIDRE.MatchString(g.GetId()) && perr == nil && now.Before(exp) && !known {
			m.markRevoked(g.GetId(), reasonRestart+e, exp)
		}
	}
	m.mu.Lock()
	m.saveLocked()
	m.mu.Unlock()
}

// sweepDocker removes brokered containers of grants that are no longer
// attached (left by a daemon that stopped without detaching). It runs once
// after restore, so containers of re-attached grants stay. Each Docker
// broker only knows its own grants, so with grants on both (unix and TCP)
// nothing is swept rather than removing the other broker's containers.
func (m *grantManager) sweepDocker(ctx context.Context) {
	m.mu.Lock()
	var onHost, onSbx bool
	for _, rec := range m.grants {
		switch {
		case rec.b == nil || rec.kind != broker.KindDockerHost:
		case rec.b == m.b.dockerSbx && rec.b != m.b.dockerHost:
			onSbx = true
		default:
			onHost = true
		}
	}
	m.mu.Unlock()
	target := m.b.dockerHost
	switch {
	case onHost && onSbx:
		m.log.Info("docker sweep skipped: grants on both docker brokers")
		return
	case onSbx:
		target = m.b.dockerSbx
	}
	db, ok := target.(*docker.Broker)
	if !ok {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if ok, _ := db.Available(sctx); !ok {
		return
	}
	if err := db.Sweep(sctx); err != nil {
		m.log.Warn("docker sweep", "err", err)
	}
}

// ---- attach and detach ----

// validate checks a grant from the hub; it returns the parsed kind data.
func (m *grantManager) validate(g *mycelv1.Grant) (*grantRec, string) {
	if !grantIDRE.MatchString(g.GetId()) {
		return nil, ctlError("invalid_argument", "invalid grant id")
	}
	if node := strings.TrimPrefix(g.GetNode(), "node:"); node != m.n.name() {
		return nil, ctlError("invalid_argument", "grant %s is for node %q, not this node", g.GetId(), g.GetNode())
	}
	if !m.validSubject(g.GetSubject()) {
		return nil, ctlError("invalid_argument", "grant %s: invalid subject %q", g.GetId(), g.GetSubject())
	}
	exp, err := time.Parse(time.RFC3339Nano, g.GetExpiresAt())
	if err != nil {
		return nil, ctlError("invalid_argument", "grant %s: invalid expires_at", g.GetId())
	}
	rec := &grantRec{g: proto.Clone(g).(*mycelv1.Grant), kind: grantKind(g), expires: exp}
	switch rec.kind {
	case "":
		return nil, ctlError("invalid_argument", "grant %s: unknown kind", g.GetId())
	case kindMount:
		mg := g.GetSpec().GetMount()
		mt, err := sandbox.ResolveMount(sandbox.Mount{HostPath: mg.GetHostPath(), GuestPath: mg.GetGuestPath(), ReadOnly: mg.GetReadOnly()})
		if err != nil {
			return nil, ctlError("invalid_argument", "grant %s: %v", g.GetId(), err)
		}
		rec.mount = mt
	case kindEgress:
		if len(g.GetSpec().GetEgress().GetAllow()) == 0 {
			return nil, ctlError("invalid_argument", "grant %s: no egress rules", g.GetId())
		}
		for _, s := range g.GetSpec().GetEgress().GetAllow() {
			r, err := egress.ParseRule(s)
			if err == nil {
				err = profiles.CheckEgressDestination(string(r))
			}
			if err != nil {
				return nil, ctlError("invalid_argument", "grant %s: %v", g.GetId(), err)
			}
			rec.rules = append(rec.rules, r)
		}
	}
	return rec, ""
}

func (m *grantManager) validSubject(s string) bool {
	if id, ok := strings.CutPrefix(s, "job:"); ok {
		return jobIDRE.MatchString(id)
	}
	_, node, ok := parseAgentID(s)
	return ok && node == m.n.name()
}

// attach handles AttachGrant: it returns "" or the ctl error text. A grant
// the local ceiling refuses is reported with grant.revoked.
func (m *grantManager) attach(g *mycelv1.Grant) string { return m.attachGrant(g, false) }

// attachGrant attaches g. Only the ceiling's offers refuse a grant here;
// schedules and idle-only settings are enforced per request by the brokers
// and at job admission. restoring (startup) skips the freeze check; the
// brokers are frozen right after.
func (m *grantManager) attachGrant(g *mycelv1.Grant, restoring bool) string {
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	rec, e := m.validate(g)
	if e != "" {
		return e
	}
	id, subject := rec.g.GetId(), rec.g.GetSubject()
	if !m.now().Before(rec.expires) {
		return ctlError("failed_precondition", "grant %s already expired", id)
	}
	m.mu.Lock()
	closed := m.closed
	old := m.grants[id]
	rv := m.revoked[id]
	if rv != nil && rv.Reported {
		rv.Reported = false // the hub still thinks it is active: tell it again
		m.saveLocked()
	}
	m.mu.Unlock()
	switch {
	case closed:
		return ctlError("unavailable", "node is shutting down")
	case rv != nil:
		m.wakeReporter()
		return ctlError("failed_precondition", "grant %s was revoked on this node: %s", id, rv.Reason)
	case old != nil && proto.Equal(old.g, rec.g):
		return "" // a resend, also while frozen
	case old != nil && old.kind != rec.kind:
		return ctlError("invalid_argument", "grant %s: the kind cannot change", id)
	case old != nil && old.g.GetSubject() != subject:
		return ctlError("invalid_argument", "grant %s: the subject cannot change", id)
	case !restoring && m.n.isFrozen():
		return ctlError("failed_precondition", "node frozen")
	}
	if !m.n.sharing.Offers(offerOf(rec.kind)) {
		m.refuse(rec, old)
		return ctlError("failed_precondition", refusedByCeiling)
	}

	m.mu.Lock()
	m.attaching = id
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.attaching = ""
		m.mu.Unlock()
	}()
	switch {
	case isBrokerKind(rec.kind):
		b := m.b.forGrant(rec.kind, subject)
		if ok, why := m.b.available(rec.kind); !ok {
			return ctlError("failed_precondition", "%s broker unavailable: %s", rec.kind, why)
		}
		actx, cancel := context.WithTimeout(context.Background(), brokerAvailWait)
		ep, err := b.Attach(actx, broker.Attachment{Grant: rec.g, SandboxID: m.n.jobs.sandboxOf(subject), ExpiresAt: rec.expires})
		cancel()
		switch {
		case errors.Is(err, broker.ErrCeiling):
			m.refuse(rec, old)
			return ctlError("failed_precondition", refusedByCeiling)
		case errors.Is(err, broker.ErrFrozen):
			return ctlError("failed_precondition", "node frozen")
		case err != nil:
			return ctlError("failed_precondition", "%s broker: %v", rec.kind, err)
		}
		rec.b, rec.endpoint = b, ep
	case rec.kind == kindMount:
		if m.n.jobs.sandboxOf(subject) != "" {
			return ctlError("failed_precondition", "mounts apply to new sandboxes")
		}
	}
	if h := m.n.cfg.grantAttachHook; h != nil {
		h(id)
	}

	// Re-check what may have changed while the broker attached: a local
	// revocation, the daemon closing, a tightened ceiling.
	m.mu.Lock()
	refusal := ""
	switch {
	case m.closed:
		refusal = ctlError("unavailable", "node is shutting down")
	case m.revoked[id] != nil:
		refusal = ctlError("failed_precondition", "grant %s was revoked on this node: %s", id, m.revoked[id].Reason)
	case !m.n.sharing.Offers(offerOf(rec.kind)):
		refusal = ctlError("failed_precondition", refusedByCeiling)
	}
	if refusal != "" {
		if cur := m.grants[id]; cur != nil {
			delete(m.grants, id)
			if cur.timer != nil {
				cur.timer.Stop()
			}
			m.saveLocked()
		}
		ceiling := m.revoked[id] == nil && !m.closed
		m.mu.Unlock()
		if rec.b != nil {
			ctx, cancel := context.WithTimeout(context.Background(), brokerDetachWait)
			_ = rec.b.Detach(ctx, id, "attach abandoned")
			cancel()
		}
		m.n.jobs.refreshEgress(subject)
		if ceiling {
			m.markRevoked(id, reasonCeiling, rec.expires)
		}
		m.log.Info("grant attach abandoned", "grant", id, "reason", refusal)
		return refusal
	}
	if old != nil && old.timer != nil {
		old.timer.Stop()
	}
	m.grants[id] = rec
	m.armLocked(rec)
	m.saveLocked()
	m.mu.Unlock()
	m.n.jobs.refreshEgress(subject)
	m.log.Info("grant attached", "grant", id, "kind", rec.kind, "subject", subject,
		"expires_at", rec.expires.UTC().Format(time.RFC3339), "replaced", old != nil)
	return ""
}

// armLocked starts the local expiry timer; m.mu held. The grants loop also
// checks expiry, so a timer delayed by a sleeping machine is not relied on.
func (m *grantManager) armLocked(rec *grantRec) {
	id := rec.g.GetId()
	rec.timer = time.AfterFunc(rec.expires.Sub(m.now()), func() {
		m.mu.Lock()
		cur := m.grants[id]
		m.mu.Unlock()
		if cur == rec {
			m.log.Info("grant expired on the node's clock", "grant", id)
			m.detach(id, "expired", false)
		}
	})
}

// expireDue detaches every grant whose expiry has passed.
func (m *grantManager) expireDue() {
	now := m.now()
	m.mu.Lock()
	var due []string
	for id, rec := range m.grants {
		if !now.Before(rec.expires) {
			due = append(due, id)
		}
	}
	m.mu.Unlock()
	for _, id := range due {
		m.log.Info("grant expired on the node's clock", "grant", id)
		m.detach(id, "expired", false)
	}
}

// refuse records a ceiling refusal: an attached version is detached and
// the hub is told with grant.revoked.
func (m *grantManager) refuse(rec, old *grantRec) {
	id := rec.g.GetId()
	m.log.Warn("grant refused by local sharing settings", "grant", id, "kind", rec.kind, "subject", rec.g.GetSubject())
	if old != nil {
		m.detach(id, reasonCeiling, true)
		return
	}
	m.markRevoked(id, reasonCeiling, rec.expires)
}

// markRevoked queues grant.revoked for id.
func (m *grantManager) markRevoked(id, reason string, expires time.Time) {
	m.mu.Lock()
	m.revoked[id] = &revokedRec{ID: id, Reason: reason, ExpiresAt: expires, At: m.now()}
	m.capRevokedLocked()
	m.saveLocked()
	m.mu.Unlock()
	m.wakeReporter()
}

// capRevokedLocked keeps at most maxGrantRevoked records: reported ones
// past their keep time go first, then the oldest; m.mu held.
func (m *grantManager) capRevokedLocked() {
	if len(m.revoked) <= maxGrantRevoked {
		return
	}
	m.pruneRevokedLocked()
	if len(m.revoked) <= maxGrantRevoked {
		return
	}
	list := make([]*revokedRec, 0, len(m.revoked))
	for _, r := range m.revoked {
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b *revokedRec) int { return a.At.Compare(b.At) })
	for _, r := range list[:len(list)-maxGrantRevoked] {
		m.log.Warn("grant revocation dropped: too many unreported revocations", "grant", r.ID)
		delete(m.revoked, r.ID)
	}
}

// detach removes a grant; report sends grant.revoked with the reason.
// Detaching an unknown grant is a no-op that reports false.
func (m *grantManager) detach(id, reason string, report bool) bool {
	m.mu.Lock()
	rec := m.grants[id]
	if rec == nil {
		m.mu.Unlock()
		return false
	}
	delete(m.grants, id)
	if rec.timer != nil {
		rec.timer.Stop()
	}
	m.saveLocked()
	m.mu.Unlock()
	subject := rec.g.GetSubject()
	if rec.b != nil {
		ctx, cancel := context.WithTimeout(context.Background(), brokerDetachWait)
		if err := rec.b.Detach(ctx, id, reason); err != nil {
			m.log.Warn("broker detach", "grant", id, "err", err)
		}
		cancel()
	}
	m.n.jobs.refreshEgress(subject)
	m.log.Info("grant detached", "grant", id, "kind", rec.kind, "subject", subject, "reason", reason)
	if report {
		m.markRevoked(id, reason, rec.expires)
	}
	return true
}

// revokeLocal handles DELETE /api/local/grants/{id}: an attached grant is
// detached; a grant being attached right now is marked revoked so the
// attach abandons it. It reports whether there was such a grant.
func (m *grantManager) revokeLocal(id string) bool {
	if m.detach(id, reasonLocal, true) {
		return true
	}
	m.mu.Lock()
	attaching := m.attaching == id
	m.mu.Unlock()
	if !attaching {
		return false
	}
	m.markRevoked(id, reasonLocal, m.now().Add(revokedKeep))
	m.log.Info("grant revoked locally while attaching", "grant", id)
	return true
}

// detachFromHub handles DetachGrant: the hub ended the grant, so a pending
// local revocation of it needs no report any more.
func (m *grantManager) detachFromHub(id, reason string) {
	m.detach(id, reason, false)
	m.mu.Lock()
	if r := m.revoked[id]; r != nil && !r.Reported {
		r.Reported = true
		m.saveLocked()
	}
	m.mu.Unlock()
}

// enforceCeiling detaches every grant the effective settings no longer
// offer and reports it.
func (m *grantManager) enforceCeiling(reason string) {
	m.mu.Lock()
	var drop []string
	for id, rec := range m.grants {
		if !m.n.sharing.Offers(offerOf(rec.kind)) {
			drop = append(drop, id)
		}
	}
	m.mu.Unlock()
	for _, id := range drop {
		m.detach(id, reason, true)
	}
}

// close stops the expiry timers and the brokers; grants.json stays.
func (m *grantManager) close() {
	m.mu.Lock()
	m.closed = true
	for _, rec := range m.grants {
		if rec.timer != nil {
			rec.timer.Stop()
		}
	}
	m.mu.Unlock()
	m.b.close()
}

func (m *grantManager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.grants)
}

// ---- grant.revoked reporting and expiry ----

func (m *grantManager) wakeReporter() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// loop detaches expired grants, sends pending grant.revoked reports
// (retried while the bus is down) and prunes old revocations.
func (m *grantManager) loop(ctx context.Context) {
	t := time.NewTicker(m.n.cfg.reportPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
		m.expireDue()
		m.reportRevoked(ctx)
	}
}

func (m *grantManager) reportRevoked(ctx context.Context) {
	if m.n.busConn() == nil {
		return
	}
	m.mu.Lock()
	var due []revokedRec
	for _, r := range m.revoked {
		if !r.Reported {
			due = append(due, *r)
		}
	}
	m.mu.Unlock()
	for _, r := range due {
		rctx, cancel := context.WithTimeout(ctx, grantRevokedWait)
		err := m.n.hubRPC(rctx, bus.RPCGrantRevoked, &mycelv1.GrantRevokedRequest{GrantId: r.ID, Reason: r.Reason}, &mycelv1.GrantRevokedResponse{})
		cancel()
		var he *hubError
		if err != nil && (!errors.As(err, &he) || retryableHubCode(errCode(he.msg))) {
			m.log.Debug("grant.revoked not delivered; retrying", "grant", r.ID, "err", err)
			return
		}
		if err != nil {
			m.log.Warn("grant.revoked refused by hub", "grant", r.ID, "err", err)
		} else {
			m.log.Info("grant revocation reported to the hub", "grant", r.ID, "reason", r.Reason)
		}
		m.mu.Lock()
		if cur := m.revoked[r.ID]; cur != nil && cur.Reason == r.Reason {
			cur.Reported = true
		}
		m.saveLocked()
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.pruneRevokedLocked()
	m.mu.Unlock()
}

// pruneRevokedLocked forgets reported revocations of grants expired for a
// while; m.mu held.
func (m *grantManager) pruneRevokedLocked() {
	now := m.now()
	for id, r := range m.revoked {
		if r.Reported && now.After(r.ExpiresAt.Add(revokedKeep)) {
			delete(m.revoked, id)
		}
	}
}

// ---- subjects ----

// subjectGrants is what the active grants of a subject add to a new sandbox.
type subjectGrants struct {
	env     map[string]string
	rules   []egress.Rule
	mounts  []sandbox.Mount
	aliases map[string]string // egress proxy aliases: "host:port" -> broker "ip:port"
}

// forSubject collects the active grants of subject for a new sandbox:
// environment, egress rules, mounts (resolved again now) and the proxy
// aliases that make the brokers reachable by name.
func (m *grantManager) forSubject(subject string) subjectGrants {
	out := subjectGrants{env: map[string]string{}}
	m.mu.Lock()
	recs := m.subjectLocked(subject)
	m.mu.Unlock()
	var gpus []string
	for _, rec := range recs {
		switch rec.kind {
		case broker.KindOllama:
			u, err := url.Parse(rec.endpoint)
			if err != nil || u.User == nil {
				continue
			}
			tok, _ := u.User.Password()
			out.env["OPENAI_BASE_URL"] = "http://" + ollamaHostName + "/v1"
			out.env["OPENAI_API_KEY"] = tok
			out.env["OLLAMA_HOST"] = "http://" + ollamaHostName
		case broker.KindDockerHost:
			// tcp://127.0.0.1:<port>/<prefix>: the prefix is the credential.
			if u, err := url.Parse(rec.endpoint); err == nil && u.Scheme == "tcp" {
				out.env["DOCKER_HOST"] = "tcp://" + dockerAlias + u.Path
			}
		case broker.KindGPU:
			if v, ok := strings.CutPrefix(rec.endpoint, "NVIDIA_VISIBLE_DEVICES="); ok && v != "" {
				gpus = append(gpus, v)
			}
		case kindMount:
			mt, err := sandbox.ResolveMount(sandbox.Mount{HostPath: rec.g.GetSpec().GetMount().GetHostPath(),
				GuestPath: rec.mount.GuestPath, ReadOnly: rec.mount.ReadOnly})
			if err != nil || mt.HostPath != rec.mount.HostPath {
				m.log.Warn("mount grant dropped: the host path no longer resolves as granted", "grant", rec.g.GetId(), "err", err)
				continue
			}
			out.mounts = append(out.mounts, mt)
		}
	}
	out.rules = rulesFor(recs)
	out.aliases = m.aliasesFor(recs)
	if len(gpus) > 0 {
		out.env["NVIDIA_VISIBLE_DEVICES"] = strings.Join(gpus, ",")
	}
	if len(out.env) == 0 {
		out.env = nil
	}
	return out
}

// egressFor is the egress allowlist and the proxy aliases the active
// grants of subject add to its running sandbox.
func (m *grantManager) egressFor(subject string) ([]egress.Rule, map[string]string) {
	m.mu.Lock()
	recs := m.subjectLocked(subject)
	m.mu.Unlock()
	return rulesFor(recs), m.aliasesFor(recs)
}

// rulesFor collects the egress grant rules.
func rulesFor(recs []*grantRec) []egress.Rule {
	var rules []egress.Rule
	for _, rec := range recs {
		if rec.kind == kindEgress {
			rules = append(rules, rec.rules...)
		}
	}
	return rules
}

// aliasesFor maps the broker names to the listeners: Ollama always (a
// request without a grant's credential is refused there), Docker when the
// subject holds a docker-host grant on the TCP broker.
func (m *grantManager) aliasesFor(recs []*grantRec) map[string]string {
	out := map[string]string{}
	if a := m.b.ollamaAddr; a != "" {
		out[ollamaAlias] = a
	}
	for _, rec := range recs {
		if rec.kind != broker.KindDockerHost {
			continue
		}
		if u, err := url.Parse(rec.endpoint); err == nil && u.Scheme == "tcp" {
			if _, _, err := net.SplitHostPort(u.Host); err == nil {
				out[dockerAlias] = u.Host
			}
		}
	}
	return out
}

// subjectLocked lists the grants of subject sorted by id; m.mu held.
func (m *grantManager) subjectLocked(subject string) []*grantRec {
	var out []*grantRec
	for _, rec := range m.grants {
		if rec.g.GetSubject() == subject {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b *grantRec) int { return strings.Compare(a.g.GetId(), b.g.GetId()) })
	return out
}

// gpuDevices is the docker broker's GPUDevices callback: the devices leased
// to subject by active gpu grants.
func (m *grantManager) gpuDevices(subject, _ string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, rec := range m.subjectLocked(subject) {
		if rec.kind != broker.KindGPU {
			continue
		}
		if v, ok := strings.CutPrefix(rec.endpoint, "NVIDIA_VISIBLE_DEVICES="); ok && v != "" {
			out = append(out, strings.Split(v, ",")...)
		}
	}
	return out
}

// ---- HTTP ----

// grantsReply is GET /api/local/grants and GET
// /api/local/agents/{session}/grants: the grants (protojson, proto field
// names) and the endpoint of each broker grant by grant id.
type grantsReply struct {
	Grants    []json.RawMessage `json:"grants"`
	Endpoints map[string]string `json:"endpoints"`
}

// list builds the reply for the grants matching keep. Without reveal the
// Ollama credential is removed from endpoints; only the subject's own
// session gets it.
func (m *grantManager) list(keep func(*grantRec) bool, reveal bool) grantsReply {
	m.mu.Lock()
	recs := make([]*grantRec, 0, len(m.grants))
	for _, rec := range m.grants {
		if keep(rec) {
			recs = append(recs, rec)
		}
	}
	m.mu.Unlock()
	slices.SortFunc(recs, func(a, b *grantRec) int { return strings.Compare(a.g.GetId(), b.g.GetId()) })
	out := grantsReply{Grants: []json.RawMessage{}, Endpoints: map[string]string{}}
	for _, rec := range recs {
		b, err := protoJSON.Marshal(rec.g)
		if err != nil {
			continue
		}
		out.Grants = append(out.Grants, b)
		if rec.endpoint == "" {
			continue
		}
		ep := rec.endpoint
		if !reveal {
			if u, err := url.Parse(ep); err == nil && u.User != nil {
				u.User = url.User(u.User.Username())
				ep = u.String()
			}
		}
		out.Endpoints[rec.g.GetId()] = ep
	}
	return out
}

func (l *localAPI) handleListGrants(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, l.n.grants.list(func(*grantRec) bool { return true }, false))
}

func (l *localAPI) handleAgentGrants(w http.ResponseWriter, r *http.Request) {
	s := l.session(r.PathValue("session"))
	if s == nil {
		writeJSONError(w, http.StatusNotFound, "unknown session")
		return
	}
	writeJSON(w, http.StatusOK, l.n.grants.list(func(g *grantRec) bool { return g.g.GetSubject() == s.agentID }, true))
}

func (l *localAPI) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !grantIDRE.MatchString(id) {
		writeJSONError(w, http.StatusNotFound, "unknown grant")
		return
	}
	if !l.n.grants.revokeLocal(id) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("grant %s is not attached on this node", id))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
