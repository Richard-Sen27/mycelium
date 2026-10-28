package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// Artifact transfer constants.
const (
	maxArtifactName    = 256
	artifactIOTimeout  = time.Hour // read or write deadline of one transfer
	artifactsSubdir    = "artifacts"
	artifactSniffBytes = 512
	uploadPrefix       = ".upload-"
)

func (s *server) artifactsDir() string { return filepath.Join(s.dir, artifactsSubdir) }

// sweepUploads removes temporary upload files left by a hub that stopped
// mid-upload.
func (s *server) sweepUploads() {
	leftovers, _ := filepath.Glob(filepath.Join(s.artifactsDir(), uploadPrefix+"*"))
	for _, p := range leftovers {
		if err := os.Remove(p); err != nil {
			s.log.Warn("remove upload leftover", "path", p, "err", err)
		}
	}
}

// artCaller is who moves artifact bytes: a user (API key) or a node
// (client certificate). A request with both is treated as the user.
type artCaller struct {
	user *userRow
	node *nodeRow
}

func (c *artCaller) id() string {
	if c.user != nil {
		return c.user.ID
	}
	return "node:" + c.node.Name
}

// artifactCaller resolves the caller; a node certificate counts only while
// it is not revoked and the node still exists.
func (s *server) artifactCaller(ctx context.Context) (*artCaller, int, string) {
	if u := userFrom(ctx); u != nil {
		return &artCaller{user: u}, 0, ""
	}
	peer := nodeFrom(ctx)
	if peer == nil {
		return nil, http.StatusUnauthorized, "an API key or a node certificate is required"
	}
	if s.st.IsRevoked(identity.SerialHex(peer.Cert), identity.NodeSPIFFEID(s.td, peer.Name)) {
		return nil, http.StatusForbidden, "certificate revoked"
	}
	n, err := s.st.getNode(ctx, peer.Name)
	if errors.Is(err, errNotFound) {
		return nil, http.StatusForbidden, "node removed"
	}
	if err != nil {
		s.log.Error("get node", "err", err)
		return nil, http.StatusInternalServerError, "internal error"
	}
	return &artCaller{node: n}, 0, ""
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, msg, code)
}

// validArtifactName: a plain file name of 1-256 printable bytes, without
// path separators and not starting with a dot.
func validArtifactName(n string) bool {
	if n == "" || len(n) > maxArtifactName || !channels.IsPrintable(n) || strings.Contains(n, "\\") {
		return false
	}
	// A relative slash path (job outputs keep their directories): no empty,
	// "." or ".." components and none starting with a dot.
	for _, c := range strings.Split(n, "/") {
		if c == "" || strings.HasPrefix(c, ".") {
			return false
		}
	}
	return true
}

// liveNodeAgent resolves an agent id named by a node in a request: it must be
// agent:<name>@<node> for this node and name a live (registered, not
// revoked, not stopped) agent. The returned actor applies the agent's own
// visibility.
func (s *server) liveNodeAgent(ctx context.Context, node, id string) (actor, bool, error) {
	if !strings.HasPrefix(id, "agent:") || !strings.HasSuffix(id, "@"+node) || !channels.ValidParticipant(id) {
		return actor{}, false, nil
	}
	ag, err := s.st.getAgent(ctx, id)
	if errors.Is(err, errNotFound) {
		return actor{}, false, nil
	}
	if err != nil {
		return actor{}, false, err
	}
	if ag.NodeName != node || ag.RevokedAt != "" || ag.State == stateStopped {
		return actor{}, false, nil
	}
	return actor{
		id: ag.ID, p: policy.Principal{Kind: policy.KindAgent, ID: ag.ID, OwnerID: ag.OwnerID},
		origin: originAgent, node: ag.NodeName, sandbox: ag.SandboxID,
	}, true, nil
}

// ---- upload slots and quota reservations ----

func (s *server) acquireUpload(caller string) bool {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	if s.jobs.uploads[caller] >= s.jobs.cfg.uploadsPerCaller {
		return false
	}
	s.jobs.uploads[caller]++
	return true
}

func (s *server) releaseUpload(caller string) {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	if s.jobs.uploads[caller]--; s.jobs.uploads[caller] <= 0 {
		delete(s.jobs.uploads, caller)
	}
}

// quota is one byte budget an upload counts against.
type quota struct {
	key   string // reservation key
	used  int64  // stored bytes when the upload started
	limit int64
}

// reserveQuota adds n bytes to every quota's reservation unless one would be
// exceeded.
func (s *server) reserveQuota(qs []quota, n int64) bool {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	for _, q := range qs {
		if q.used+s.jobs.reserved[q.key]+n > q.limit {
			return false
		}
	}
	for _, q := range qs {
		s.jobs.reserved[q.key] += n
	}
	return true
}

func (s *server) unreserveQuota(qs []quota, n int64) {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	for _, q := range qs {
		if s.jobs.reserved[q.key] -= n; s.jobs.reserved[q.key] <= 0 {
			delete(s.jobs.reserved, q.key)
		}
	}
}

var (
	errArtifactTooLarge = errors.New("artifact too large")
	errQuotaExceeded    = errors.New("artifact quota exceeded")
)

// handleArtifactPut stores an uploaded artifact:
// PUT /api/artifacts?name=<name>[&job=<id>][&channel=<name>].
func (s *server) handleArtifactPut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.jobs.cfg
	c, code, msg := s.artifactCaller(ctx)
	if c == nil {
		httpError(w, code, msg)
		return
	}
	q := r.URL.Query()
	name, jobID, channel := q.Get("name"), q.Get("job"), q.Get("channel")
	deny := func(code int, reason string) {
		s.auditDenied(ctx, c.id(), string(policy.ActionArtifactPut), "artifact", map[string]any{"reason": reason, "job": jobID, "channel": channel})
		httpError(w, code, reason)
	}
	if !validArtifactName(name) {
		httpError(w, http.StatusBadRequest, "invalid name: a relative path of 1-256 printable bytes, no '\\', '.' or '..' components or components starting with '.'")
		return
	}
	createdBy := q.Get("created_by")
	var agentActor *actor
	if createdBy != "" {
		if c.node == nil {
			httpError(w, http.StatusBadRequest, "created_by is only for node uploads on behalf of an agent")
			return
		}
		a, ok, err := s.liveNodeAgent(ctx, c.node.Name, createdBy)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			deny(http.StatusForbidden, "created_by must name a live agent on this node")
			return
		}
		agentActor = &a
	}
	if jobID != "" && !validJobID(jobID) {
		httpError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	if channel != "" && !channels.ValidName(channel) && !channels.IsDirect(channel) {
		httpError(w, http.StatusBadRequest, "invalid channel name")
		return
	}
	if r.ContentLength > cfg.maxArtifactBytes {
		httpError(w, http.StatusRequestEntityTooLarge, "artifact exceeds the size limit")
		return
	}
	if !s.acquireUpload(c.id()) {
		httpError(w, http.StatusTooManyRequests, "too many uploads in progress")
		return
	}
	defer s.releaseUpload(c.id())
	if s.lowDisk() {
		httpError(w, http.StatusInsufficientStorage, "hub disk space low")
		return
	}

	row := &artifactRow{Name: name}
	var nodeQuotaKey string
	if c.user != nil {
		a := userActor(c.user)
		row.OwnerID, row.CreatedBy = c.user.ID, "user:"+c.user.ID
		if d := s.policy.Authorize(ctx, a.p, policy.ActionArtifactPut, policy.Resource{Kind: "artifact", OwnerID: c.user.ID}); !d.Allow {
			deny(http.StatusForbidden, d.Reason)
			return
		}
		if jobID != "" {
			j, err := s.st.getJob(ctx, jobID)
			if err != nil || j.OwnerID != c.user.ID {
				deny(http.StatusForbidden, "only the job's owner may attach uploads to it")
				return
			}
		}
		if channel != "" {
			ok, err := s.canReadChannel(ctx, a, channel)
			if err != nil {
				httpError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if !ok {
				deny(http.StatusForbidden, "channel not readable by you")
				return
			}
		}
	} else {
		row.OwnerID, row.CreatedBy = c.node.OwnerID, "node:"+c.node.Name
		if agentActor != nil {
			// The daemon exports on behalf of one of its agents.
			row.OwnerID, row.CreatedBy = agentActor.p.OwnerID, agentActor.id
		}
		if jobID != "" {
			// Job outputs: the job runs on this enrolment of the node right
			// now; they belong to the job's owner and go to its channel.
			j, err := s.st.getJob(ctx, jobID)
			ok := false
			if err == nil {
				ok, err = s.assignedTo(ctx, j, c.node.Name)
			}
			if err != nil && !errors.Is(err, errNotFound) {
				httpError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if !ok {
				deny(http.StatusForbidden, "job not assigned to this node")
				return
			}
			if j.State != mycelv1.JobState_JOB_STATE_RUNNING {
				deny(http.StatusConflict, "job is "+stateWord(j.State))
				return
			}
			if channel != "" && channel != j.Spec.GetChannel() {
				deny(http.StatusForbidden, "job outputs may only be attached to the job's channel")
				return
			}
			row.OwnerID, channel = j.OwnerID, j.Spec.GetChannel()
		} else {
			// Not tied to a job: counts against the node's quota too.
			nodeQuotaKey = c.node.Name
			if channel != "" {
				// The agent's own read access, or the node owner's.
				reader := agentActor
				if reader == nil {
					owner, err := s.st.getUser(ctx, c.node.OwnerID)
					if err != nil {
						deny(http.StatusForbidden, "node owner not found")
						return
					}
					ua := userActor(owner)
					reader = &ua
				}
				ok, err := s.canReadChannel(ctx, *reader, channel)
				if err != nil {
					httpError(w, http.StatusInternalServerError, "internal error")
					return
				}
				if !ok {
					deny(http.StatusForbidden, "channel not readable by the node's owner")
					return
				}
			}
		}
	}
	row.JobID, row.Channel = jobID, channel
	if jobID != "" {
		n, err := s.st.jobArtifactCount(ctx, jobID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if n >= maxJobArtifacts {
			deny(http.StatusConflict, "the job has the maximum number of artifacts")
			return
		}
	}

	// Quotas: the owner's bytes, and a node's bytes not tied to a job.
	used, err := s.st.artifactBytes(ctx, row.OwnerID, "")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	qs := []quota{{key: "owner:" + row.OwnerID, used: used, limit: cfg.ownerQuota}}
	if nodeQuotaKey != "" {
		nused, err := s.st.artifactBytes(ctx, "", nodeQuotaKey)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		qs = append(qs, quota{key: "node:" + nodeQuotaKey, used: nused, limit: cfg.nodeQuota})
	}
	var reserved int64
	defer func() { s.unreserveQuota(qs, reserved) }()

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(artifactIOTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(artifactIOTimeout))
	err = s.storeArtifactBody(r.Body, row, cfg.maxArtifactBytes, func(n int64) error {
		if !s.reserveQuota(qs, n) {
			return errQuotaExceeded
		}
		reserved += n
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errArtifactTooLarge):
			httpError(w, http.StatusRequestEntityTooLarge, "artifact exceeds the size limit")
		case errors.Is(err, errQuotaExceeded):
			deny(http.StatusInsufficientStorage, "artifact quota exceeded")
		default:
			s.log.Warn("artifact upload", "caller", c.id(), "err", err)
			httpError(w, http.StatusBadRequest, "upload failed")
		}
		return
	}
	if err := s.st.insertArtifact(context.WithoutCancel(ctx), row); err != nil {
		_ = os.Remove(filepath.Join(s.dir, row.Path))
		s.log.Error("insert artifact", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.audit(ctx, c.id(), string(policy.ActionArtifactPut), "artifact:"+row.ID, outcomeOK, map[string]any{
		"name": row.Name, "size": row.Size, "sha256": row.SHA256, "owner_id": row.OwnerID, "job": row.JobID, "channel": row.Channel,
	})
	b, err := protojson.Marshal(row.toProto())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(b)
}

// storeArtifactBody streams body into the artifact store, calling account
// for every chunk before it is written, and fills the row's id, path,
// size, sha256, media type and creation time. The file is synced before
// it gets its final name.
func (s *server) storeArtifactBody(body io.Reader, row *artifactRow, limit int64, account func(int64) error) (err error) {
	dir := s.artifactsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, uploadPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			return err
		}
	}
	h := sha256.New()
	sniff := make([]byte, 0, artifactSniffBytes)
	buf := make([]byte, 256<<10)
	var size int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > limit {
				return errArtifactTooLarge
			}
			if err := account(int64(n)); err != nil {
				return err
			}
			if len(sniff) < artifactSniffBytes {
				sniff = append(sniff, buf[:min(n, artifactSniffBytes-len(sniff))]...)
			}
			h.Write(buf[:n])
			if _, err := tmp.Write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	row.ID = newULIDID("art_")
	row.Path = filepath.ToSlash(filepath.Join(artifactsSubdir, row.ID))
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, row.Path)); err != nil {
		return err
	}
	row.Size, row.SHA256 = size, hex.EncodeToString(h.Sum(nil))
	row.MediaType = http.DetectContentType(sniff)
	row.CreatedAt = nowString()
	return nil
}

// specUses reports whether a job spec references the artifact as its
// workspace or an input.
func specUses(spec *mycelv1.JobSpec, artID string) bool {
	return spec.GetWorkspace().GetArtifact() == artID ||
		slices.ContainsFunc(spec.GetInputs(), func(in *mycelv1.JobInput) bool { return in.GetArtifact() == artID })
}

// nodeMayFetch reports whether this enrolment of node runs a job (starting
// or running) whose spec references the artifact.
func (s *server) nodeMayFetch(ctx context.Context, node, artID string) (bool, error) {
	jobs, err := s.st.assignedJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if !specUses(j.Spec, artID) {
			continue
		}
		ok, err := s.assignedTo(ctx, j, node)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// handleArtifactGet serves GET /api/artifacts/{id}. Bytes are always sent
// as an attachment of type application/octet-stream so that uploaded
// content never renders in the hub's origin. An artifact the caller may
// not see is reported missing.
func (s *server) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, code, msg := s.artifactCaller(ctx)
	if c == nil {
		httpError(w, code, msg)
		return
	}
	id := r.PathValue("id")
	if !validArtifactID(id) {
		httpError(w, http.StatusBadRequest, "invalid artifact id")
		return
	}
	art, err := s.st.getArtifact(ctx, id)
	if errors.Is(err, errNotFound) {
		httpError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		s.log.Error("get artifact", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	allowed, reason := false, ""
	if c.user != nil {
		d, err := s.artifactDecision(ctx, userActor(c.user), policy.ActionArtifactGet, art)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		allowed, reason = d.Allow, d.Reason
	} else if hdr := r.Header.Get("Mycel-Agent"); hdr != "" {
		// The daemon fetches for one of its agents: the agent's visibility.
		a, ok, err := s.liveNodeAgent(ctx, c.node.Name, hdr)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			s.auditDenied(ctx, c.id(), string(policy.ActionArtifactGet), "artifact:"+id, map[string]any{"reason": "Mycel-Agent is not a live agent of this node", "agent": hdr})
			httpError(w, http.StatusForbidden, "Mycel-Agent must name a live agent on this node")
			return
		}
		d, err := s.artifactDecision(ctx, a, policy.ActionArtifactGet, art)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		allowed, reason = d.Allow, d.Reason
	} else {
		allowed, err = s.nodeMayFetch(ctx, c.node.Name, id)
		if err != nil {
			s.log.Error("node artifact check", "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		reason = "no job on this node uses the artifact"
	}
	if !allowed {
		s.auditDenied(ctx, c.id(), string(policy.ActionArtifactGet), "artifact:"+id, map[string]any{"reason": reason})
		httpError(w, http.StatusNotFound, "artifact not found")
		return
	}
	f, err := os.Open(filepath.Join(s.dir, filepath.FromSlash(art.Path)))
	if err != nil {
		s.log.Error("open artifact", "artifact", id, "err", err)
		httpError(w, http.StatusNotFound, "artifact file missing")
		return
	}
	defer func() { _ = f.Close() }()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(artifactIOTimeout))
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("ETag", `"`+art.SHA256+`"`)
	h.Set("Mycel-Artifact-Sha256", art.SHA256)
	h.Set("Mycel-Media-Type", art.MediaType)
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(art.Name)}); cd != "" {
		h.Set("Content-Disposition", cd)
	} else {
		h.Set("Content-Disposition", "attachment")
	}
	modified, _ := parseTime(art.CreatedAt)
	http.ServeContent(w, r, "", modified, f)
}
