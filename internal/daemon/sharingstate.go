package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
	"github.com/richard-sen27/mycelium/internal/broker/docker"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/sharing"
)

// SharingHubFile holds the owner's hub-side sharing settings as last
// received from the hub (SharingSettings ctl or the node.sharing reply), so
// the effective settings survive a restart while the hub is unreachable.
const SharingHubFile = "sharing-hub.json"

const (
	// ownerActiveWithin: the owner counts as active at the machine while the
	// idle detector reports less than this (local_first).
	ownerActiveWithin = 2 * time.Minute
	maxSharingBody    = 64 << 10
	sharingRPCTimeout = 5 * time.Second
	reportRetryAfter  = 5 * time.Second
)

// maxIdleFailures transient detector errors keep the last sample.
const maxIdleFailures = 3

// Offer names of the sharing settings.
const offerSandbox = "sandbox"

// sharingState holds the hub-side settings, the local ceiling and their
// conjunction, plus the owner's idle state. It implements broker.Ceiling.
//
// local_first: while the owner is active at the machine (idle detector below
// two minutes), running remote sandboxes (every job) are paused with the
// runner's Pause and resumed once the owner is idle again. The idle state is
// sampled every idleSampleEvery (30 s), so a pause or resume follows the
// owner's activity within that period; a change of the settings applies at
// once.
type sharingState struct {
	n   *node
	log *slog.Logger
	det sharing.IdleDetector

	mu      sync.Mutex
	hub     *mycelv1.SharingSettings // nil until the hub sent settings
	local   *mycelv1.SharingSettings // nil = no local ceiling
	eff     *mycelv1.SharingSettings
	idleFor time.Duration // last known sample
	// idleKnown is false while the detector cannot tell (or failed more
	// than maxIdleFailures times in a row): when=idle is then inactive and
	// local_first pauses nothing.
	idleKnown bool
	idleFails int
	sampled   bool

	// node.sharing reporting: the report is due while pending is set or the
	// connection changed since the last successful one.
	repMu      sync.Mutex
	pending    bool
	repNC      *nats.Conn
	repRC      uint64
	retryAfter time.Time
	kick       chan struct{}
}

var _ broker.Ceiling = (*sharingState)(nil)

func newSharingState(n *node) *sharingState {
	s := &sharingState{n: n, log: n.log.With("component", "sharing"), det: n.cfg.idleDetector,
		pending: true, kick: make(chan struct{}, 1)}
	if s.det == nil {
		s.det = sharing.NewIdleDetector()
	}
	local, err := sharing.Load(n.dir)
	if err != nil {
		// Fail closed: a ceiling the owner wrote but that cannot be read
		// offers nothing until it is fixed or replaced with PUT.
		s.log.Error("local sharing settings unreadable; offering nothing until fixed", "err", err)
		local = &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{}}}
	}
	s.local = local
	s.hub = s.loadHub()
	s.eff = sharing.Effective(s.hub, s.local)
	return s
}

func (s *sharingState) hubPath() string { return filepath.Join(s.n.dir, SharingHubFile) }

func (s *sharingState) loadHub() *mycelv1.SharingSettings {
	b, err := os.ReadFile(s.hubPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.log.Warn("read hub sharing settings", "err", err)
		return nil
	}
	h := &mycelv1.SharingSettings{}
	if err := protojson.Unmarshal(b, h); err != nil || sharing.Validate(h) != nil {
		s.log.Warn("hub sharing settings file is invalid; ignoring it", "file", SharingHubFile)
		return nil
	}
	return h
}

// Offers implements broker.Ceiling.
func (s *sharingState) Offers(name string) bool {
	if name == broker.KindDockerHost {
		name = docker.OfferName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.eff.GetOffer().GetNames(), name)
}

// Active implements broker.Ceiling: the effective settings are active now
// given the last idle sample. An unknown idle state never counts as idle.
func (s *sharingState) Active() bool {
	s.mu.Lock()
	eff, idle := s.eff, s.idleFor
	if !s.idleKnown {
		idle = 0
	}
	s.mu.Unlock()
	return sharing.ActiveAt(eff, time.Now(), idle)
}

// effective returns the current effective settings (never mutated).
func (s *sharingState) effective() *mycelv1.SharingSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eff
}

func (s *sharingState) maxJobs() int {
	m := int(s.effective().GetMaxJobs())
	if m <= 0 {
		m = sharing.DefaultMaxJobs
	}
	return m
}

// localFirstPause reports whether remote sandboxes should be paused now.
func (s *sharingState) localFirstPause() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eff.GetLocalFirst() && s.idleKnown && s.idleFor < ownerActiveWithin
}

func (s *sharingState) snapshot() (hub, local, eff *mycelv1.SharingSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hub, s.local, s.eff
}

// sample reads the idle detector once. sharing.ErrUnknownIdle makes the
// idle state unknown at once; other errors keep the last sample for up to
// maxIdleFailures samples, then count as unknown.
func (s *sharingState) sample() {
	d, err := s.det.IdleFor()
	s.mu.Lock()
	was := s.idleKnown
	switch {
	case errors.Is(err, sharing.ErrUnknownIdle):
		s.idleKnown, s.idleFails = false, 0
	case err != nil:
		s.idleFails++
		if s.idleFails > maxIdleFailures {
			s.idleKnown = false
		}
	default:
		s.idleKnown, s.idleFails, s.idleFor = true, 0, d
	}
	now, first := s.idleKnown, !s.sampled
	s.sampled = true
	s.mu.Unlock()
	if err != nil && !errors.Is(err, sharing.ErrUnknownIdle) {
		s.log.Warn("idle detector", "err", err)
	}
	switch {
	case now && !was && !first:
		s.log.Info("owner idle state known again")
	case !now && (was || first):
		s.log.Warn("owner idle state unknown: when=idle sharing stays inactive and local_first pauses nothing", "err", err)
	}
}

// idleText is the idle state for the status and GET /api/local/sharing:
// "unknown" or the time since the owner's last input.
func (s *sharingState) idleText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.idleKnown {
		return "unknown"
	}
	return s.idleFor.Truncate(time.Second).String()
}

// loop samples the idle state and applies local_first every
// idleSampleEvery, and reports node.sharing when due.
func (s *sharingState) loop(ctx context.Context) {
	idle := time.NewTicker(s.n.cfg.idleSampleEvery)
	defer idle.Stop()
	rep := time.NewTicker(s.n.cfg.reportPollEvery)
	defer rep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			s.sample()
			s.n.jobs.applyLocalFirst()
		case <-rep.C:
			s.report(ctx)
		case <-s.kick:
			s.report(ctx)
		}
	}
}

// requestReport marks node.sharing due and wakes the loop.
func (s *sharingState) requestReport() {
	s.repMu.Lock()
	s.pending = true
	s.retryAfter = time.Time{}
	s.repMu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// report sends node.sharing when it is due: after a local change, at
// startup and on every new or re-established bus connection. A transport
// failure is retried; a refusal is dropped until the next change.
func (s *sharingState) report(ctx context.Context) {
	nc := s.n.busConn()
	if nc == nil {
		return
	}
	rc := nc.Stats().Reconnects
	s.repMu.Lock()
	due := s.pending || nc != s.repNC || rc != s.repRC
	if !due || time.Now().Before(s.retryAfter) {
		s.repMu.Unlock()
		return
	}
	s.repMu.Unlock()

	_, local, _ := s.snapshot()
	req := &mycelv1.NodeSharingRequest{Local: local}
	resp := &mycelv1.NodeSharingResponse{}
	rctx, cancel := context.WithTimeout(ctx, sharingRPCTimeout)
	err := s.n.hubRPC(rctx, bus.RPCNodeSharing, req, resp)
	cancel()
	var he *hubError
	retry := err != nil && (!errors.As(err, &he) || retryableHubCode(errCode(he.msg)))
	s.repMu.Lock()
	if retry {
		s.retryAfter = time.Now().Add(reportRetryAfter)
		s.repMu.Unlock()
		s.log.Debug("node.sharing not delivered; retrying", "err", err)
		return
	}
	s.pending, s.repNC, s.repRC = false, nc, rc
	s.repMu.Unlock()
	if err != nil {
		s.log.Warn("node.sharing refused by hub", "err", err)
		return
	}
	s.log.Info("local sharing settings reported to the hub")
	if h := resp.GetHub(); h != nil {
		if e := s.setHub(h, false); e != "" {
			s.log.Warn("hub sharing settings from node.sharing reply ignored", "err", e)
		}
	}
}

// setHub stores new hub-side settings (SharingSettings ctl or the
// node.sharing reply) and applies the new effective settings. It returns
// the ctl error text or "".
func (s *sharingState) setHub(h *mycelv1.SharingSettings, fromCtl bool) string {
	if err := sharing.Validate(h); err != nil {
		return ctlError("invalid_argument", "%v", err)
	}
	h = proto.Clone(h).(*mycelv1.SharingSettings)
	s.mu.Lock()
	same := proto.Equal(s.hub, h)
	s.mu.Unlock()
	if same {
		return ""
	}
	b, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(h)
	if err != nil {
		return ctlError("internal", "encode settings")
	}
	if err := writeFileAtomic(s.hubPath(), b); err != nil {
		s.log.Warn("store hub sharing settings", "err", err)
		if fromCtl {
			return ctlError("internal", "could not store the settings")
		}
	}
	s.mu.Lock()
	s.hub = h
	s.eff = sharing.Effective(s.hub, s.local)
	s.mu.Unlock()
	s.log.Info("hub sharing settings updated")
	s.n.sharingChanged("sharing settings")
	return ""
}

// putLocal validates and stores a new local ceiling. It must tighten the
// hub-side settings last received (their defaults when none were).
func (s *sharingState) putLocal(c *mycelv1.SharingSettings) (*mycelv1.SharingSettings, int, error) {
	if err := sharing.Validate(c); err != nil {
		return nil, http.StatusBadRequest, err
	}
	s.mu.Lock()
	hub := s.hub
	s.mu.Unlock()
	if !sharing.Tightens(sharing.Effective(hub, nil), c) {
		return nil, http.StatusBadRequest, fmt.Errorf("%w: the local ceiling may not widen the hub-side settings (offers, schedule, idle-only, local_first, max_jobs, idle_after_minutes); widen from the hub", sharing.ErrWider)
	}
	c = proto.Clone(c).(*mycelv1.SharingSettings)
	if err := sharing.Save(s.n.dir, c); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.mu.Lock()
	s.local = c
	s.eff = sharing.Effective(s.hub, s.local)
	eff := s.eff
	s.mu.Unlock()
	s.log.Info("local sharing ceiling changed", "offer", eff.GetOffer().GetNames(), "when", eff.GetWhen(),
		"local_first", eff.GetLocalFirst(), "max_jobs", eff.GetMaxJobs())
	s.n.sharingChanged("local ceiling")
	s.requestReport()
	return eff, http.StatusOK, nil
}

// sharingChanged applies new effective settings: grants no longer offered
// are detached and reported, and the job runner re-evaluates local_first.
func (n *node) sharingChanged(reason string) {
	if n.grants != nil {
		n.grants.enforceCeiling(reason)
	}
	if n.jobs != nil {
		n.jobs.applyLocalFirst()
	}
}

// ---- HTTP ----

var protoJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}

func writeProto(w http.ResponseWriter, code int, m proto.Message) {
	b, err := protoJSON.Marshal(m)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (l *localAPI) handleGetSharing(w http.ResponseWriter, _ *http.Request) {
	hub, local, eff := l.n.sharing.snapshot()
	if hub == nil {
		hub = sharing.Effective(nil, nil) // the defaults until the hub sends settings
	}
	// GetNodeSharingResponse plus "idle": "unknown" or "<duration>".
	b, err := protoJSON.Marshal(&mycelv1.GetNodeSharingResponse{Hub: hub, Local: local, Effective: eff})
	out := map[string]json.RawMessage{}
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	idle, _ := json.Marshal(l.n.sharing.idleText())
	out["idle"] = idle
	writeJSON(w, http.StatusOK, out)
}

func (l *localAPI) handlePutSharing(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r, maxSharingBody)
	if err != nil {
		writeBodyError(w, err, "could not read request body")
		return
	}
	c := &mycelv1.SharingSettings{}
	if err := protojson.Unmarshal(body, c); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid SharingSettings JSON")
		return
	}
	eff, code, err := l.n.sharing.putLocal(c)
	if err != nil {
		writeJSONError(w, code, err.Error())
		return
	}
	writeProto(w, http.StatusOK, eff)
}

// readBody reads at most limit bytes of the request body.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}
