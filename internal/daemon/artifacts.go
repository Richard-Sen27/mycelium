package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Artifact transfer limits (defaults of runConfig.jobArtifactMax and
// runConfig.localArtifactMax).
const (
	maxJobArtifactBytes   = 1 << 30   // job inputs and outputs
	maxLocalArtifactBytes = 256 << 20 // trusted-host agent exports and fetches
	localArtifactTimeout  = 15 * time.Minute
	maxArtifactReply      = 1 << 20
	// AgentHeader names the agent on whose behalf the daemon fetches an
	// artifact, so the hub can apply that agent's visibility.
	agentHeader = "Mycel-Agent"
)

var artifactIDRE = regexp.MustCompile(`^art_[0-9A-Za-z_-]{1,128}$`)

// hubHTTP returns an HTTPS client for the hub's artifact endpoints: pinned
// hub CA, the node's current certificate, no overall timeout (callers bound
// transfers by context) and no redirects (a redirect is answered as the
// hub's response, never followed with the node certificate).
func (n *node) hubHTTP() *http.Client {
	hc := pinnedClient(n.pool, func() *tls.Certificate {
		n.mu.Lock()
		defer n.mu.Unlock()
		c := n.cert
		return &c
	})
	hc.Timeout = 0
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

// artifactBase returns base (or the node's hub URL when base is empty)
// without a trailing slash; it must be https.
func (n *node) artifactBase(base string) (string, error) {
	if base == "" {
		n.mu.Lock()
		base = n.st.HubURL
		n.mu.Unlock()
	}
	if err := checkHubURL(base); err != nil {
		return "", err
	}
	return strings.TrimRight(base, "/"), nil
}

// hubStatusError is a non-2xx answer of the hub's artifact endpoints.
type hubStatusError struct {
	code int
	msg  string
}

func (e *hubStatusError) Error() string { return fmt.Sprintf("hub answered %d: %s", e.code, e.msg) }

func readHubError(res *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(b, &e) == nil {
		msg = e.Error
		if msg == "" {
			msg = e.Message
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if len(msg) > 512 {
		msg = msg[:512]
	}
	if msg == "" {
		msg = http.StatusText(res.StatusCode)
	}
	return &hubStatusError{code: res.StatusCode, msg: msg}
}

// uploadArtifact streams body (size bytes, -1 unknown) to PUT
// <base>/api/artifacts?<q> and returns the Artifact the hub created.
func (n *node) uploadArtifact(ctx context.Context, hc *http.Client, base string, q url.Values, body io.Reader, size int64) (*mycelv1.Artifact, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/artifacts?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, readHubError(res)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxArtifactReply))
	if err != nil {
		return nil, err
	}
	a := &mycelv1.Artifact{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, a); err != nil {
		return nil, errors.New("malformed artifact reply from hub")
	}
	if !artifactIDRE.MatchString(a.GetId()) {
		return nil, errors.New("hub returned no valid artifact id")
	}
	return a, nil
}

// openArtifact starts GET <base>/api/artifacts/{id}; the caller closes the
// body of the 2xx response.
func (n *node) openArtifact(ctx context.Context, hc *http.Client, base, id string, hdr http.Header) (*http.Response, error) {
	if !artifactIDRE.MatchString(id) {
		return nil, fmt.Errorf("invalid artifact id %q", id)
	}
	// base is the hub's https URL, verified against the pinned hub CA.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/artifacts/"+url.PathEscape(id), nil) //nolint:gosec // G704: see above
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	res, err := hc.Do(req) //nolint:gosec // G704: the pinned hub
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer func() { _ = res.Body.Close() }()
		return nil, readHubError(res)
	}
	return res, nil
}

// downloadArtifact writes artifact id to dst (0600), at most limit bytes,
// and returns the file name the hub gave it (Content-Disposition, may be "").
func (n *node) downloadArtifact(ctx context.Context, hc *http.Client, base, id, dst string, limit int64) (string, error) {
	res, err := n.openArtifact(ctx, hc, base, id, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.ContentLength > limit {
		return "", fmt.Errorf("%d bytes, more than the %d allowed", res.ContentLength, limit)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	nb, err := io.Copy(f, io.LimitReader(res.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if nb > limit {
		return "", fmt.Errorf("more than the %d bytes allowed", limit)
	}
	return dispositionName(res.Header.Get("Content-Disposition")), nil
}

// dispositionName returns a safe base file name from a Content-Disposition
// header, or "".
func dispositionName(h string) string {
	if h == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(h)
	if err != nil {
		return ""
	}
	name := params["filename"]
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "." || name == ".." || name == "/" || name == "" || len(name) > 255 {
		return ""
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return name
}

// ---- local artifact endpoints (trusted-host agents) ----

func (l *localAPI) artifactSession(w http.ResponseWriter, r *http.Request) *agentSession {
	s := l.session(r.PathValue("session"))
	if s == nil {
		writeJSONError(w, http.StatusNotFound, "unknown session")
		return nil
	}
	return s
}

// writeHubStatusError relays a hub refusal; 401 from the hub becomes 403
// (401 is reserved for the local token).
func writeHubStatusError(w http.ResponseWriter, err error) {
	var hs *hubStatusError
	if errors.As(err, &hs) {
		code := hs.code
		if code == http.StatusUnauthorized {
			code = http.StatusForbidden
		}
		if code < 400 || code > 599 {
			code = http.StatusBadGateway
		}
		writeJSONError(w, code, hs.msg)
		return
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "artifact too large")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSONError(w, http.StatusGatewayTimeout, "hub did not answer in time")
		return
	}
	if errors.Is(err, context.Canceled) {
		writeJSONError(w, http.StatusServiceUnavailable, "request cancelled")
		return
	}
	writeJSONError(w, http.StatusBadGateway, "hub unreachable")
}

// handleExportArtifact: POST /api/local/agents/{session}/artifacts.
func (l *localAPI) handleExportArtifact(w http.ResponseWriter, r *http.Request) {
	s := l.artifactSession(w, r)
	if s == nil {
		return
	}
	if paused, reason := s.pauseState(); paused {
		writeJSONError(w, http.StatusLocked, "agent paused: "+reason)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || len(name) > 255 || strings.ContainsRune(name, 0) {
		writeJSONError(w, http.StatusBadRequest, "name is required (at most 255 bytes)")
		return
	}
	limit := l.n.cfg.localArtifactMax
	if r.ContentLength > limit {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "artifact too large")
		return
	}
	base, err := l.n.artifactBase("")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "no usable hub URL")
		return
	}
	q := url.Values{"name": {name}, "created_by": {s.agentID}}
	if ch := r.URL.Query().Get("channel"); ch != "" {
		q.Set("channel", ch)
	}
	ctx, cancel := l.requestContext(r.Context(), localArtifactTimeout)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, limit)
	hc := l.n.hubHTTP()
	defer hc.CloseIdleConnections()
	a, err := l.n.uploadArtifact(ctx, hc, base, q, body, r.ContentLength)
	if err != nil {
		// A body over the cap surfaces as the upload's error.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "artifact too large")
			return
		}
		l.log.Info("artifact export failed", "agent", s.agentID, "err", err)
		writeHubStatusError(w, err)
		return
	}
	b, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(a)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	l.log.Info("artifact exported", "agent", s.agentID, "artifact", a.GetId(), "size", a.GetSize())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(b)
}

// handleFetchArtifact: GET /api/local/agents/{session}/artifacts/{id}.
func (l *localAPI) handleFetchArtifact(w http.ResponseWriter, r *http.Request) {
	s := l.artifactSession(w, r)
	if s == nil {
		return
	}
	id := r.PathValue("id")
	if !artifactIDRE.MatchString(id) {
		writeJSONError(w, http.StatusBadRequest, "invalid artifact id")
		return
	}
	base, err := l.n.artifactBase("")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "no usable hub URL")
		return
	}
	ctx, cancel := l.requestContext(r.Context(), localArtifactTimeout)
	defer cancel()
	hc := l.n.hubHTTP()
	defer hc.CloseIdleConnections()
	res, err := l.n.openArtifact(ctx, hc, base, id, http.Header{agentHeader: {s.agentID}})
	if err != nil {
		writeHubStatusError(w, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	limit := l.n.cfg.localArtifactMax
	if res.ContentLength > limit {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "artifact too large to fetch through the node")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		if name := dispositionName(cd); name != "" {
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		}
	}
	if res.ContentLength >= 0 {
		h.Set("Content-Length", fmt.Sprint(res.ContentLength))
	}
	w.WriteHeader(http.StatusOK)
	// Without a length the copy stops at the cap; the client sees a short,
	// failed body rather than more than the cap.
	if n, _ := io.Copy(w, io.LimitReader(res.Body, limit+1)); n > limit {
		panic(http.ErrAbortHandler)
	}
}
