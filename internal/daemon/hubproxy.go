package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// The node UI's network view (localapi.go, phase 6): /api/hub/{path...}
// forwards read-only hub calls over the node's mTLS certificate with the
// read scope header. The hub address is the node's configured hub, never
// taken from the request; only the read-only methods are forwarded (the hub
// checks them again); the browser's credentials never leave the machine.

const (
	hubScopeHeader      = "Mycel-Scope"
	hubScopeNetworkRead = "network:read"

	proxyMaxRequest    = 1 << 20
	proxyHeaderTimeout = 30 * time.Second
	// proxyIdleTimeout ends a forwarded stream that stays silent this long
	// (the hub sends a heartbeat every 15 s on WatchEvents); it never bounds
	// the total duration of a stream.
	proxyIdleTimeout = 60 * time.Second
	proxyChunk       = 32 << 10
)

// proxyIdle is proxyIdleTimeout; tests shorten it.
var proxyIdle = proxyIdleTimeout

// HubReadOnlyProcedures are the hub's Connect procedures a node read scope
// may call: the same list as internal/hub (readOnlyMethods), checked by a
// hub test.
var HubReadOnlyProcedures = []string{
	"/mycel.v1.TokenService/ListJoinTokens",
	"/mycel.v1.NodeService/ListNodes",
	"/mycel.v1.NodeService/GetNode",
	"/mycel.v1.UserService/ListUsers",
	"/mycel.v1.UserService/Whoami",
	"/mycel.v1.AgentService/ListAgents",
	"/mycel.v1.AgentService/GetAgent",
	"/mycel.v1.AgentService/GetChannelUsage",
	"/mycel.v1.ChannelService/ListChannels",
	"/mycel.v1.ChannelService/GetChannel",
	"/mycel.v1.ChannelService/ListMessages",
	"/mycel.v1.ChannelService/Watch",
	"/mycel.v1.ChannelService/BlackboardGet",
	"/mycel.v1.GrantService/ListGrants",
	"/mycel.v1.GrantService/GetGrant",
	"/mycel.v1.ControlService/GetNodeSharing",
	"/mycel.v1.ControlService/FreezeStatus",
	"/mycel.v1.EventService/WatchEvents",
	"/mycel.v1.EventService/Search",
	"/mycel.v1.EventService/ListAudit",
	"/mycel.v1.JobService/GetJob",
	"/mycel.v1.JobService/ListJobs",
	"/mycel.v1.JobService/ListProfiles",
	"/mycel.v1.JobService/ListNodesForProfile",
	"/mycel.v1.ArtifactService/ListArtifacts",
	"/mycel.v1.ArtifactService/GetArtifact",
}

var hubReadOnly = func() map[string]bool {
	m := map[string]bool{}
	for _, p := range HubReadOnlyProcedures {
		m[p] = true
	}
	return m
}()

// Request headers forwarded to the hub; everything else (cookies,
// Authorization, Mycel-Agent, ...) stays behind.
var proxyRequestHeaders = []string{
	"Content-Type", "Accept", "Connect-Protocol-Version", "Connect-Timeout-Ms",
	"Connect-Accept-Encoding", "Connect-Content-Encoding", "Content-Encoding",
}

// Response headers passed back; Set-Cookie and the like never are.
var proxyResponseHeaders = []string{
	"Content-Type", "Content-Encoding", "Content-Length", "Content-Disposition",
	"Connect-Content-Encoding", "Connect-Accept-Encoding", "Grpc-Status", "Grpc-Message",
}

// proxyAllowed reports whether method and hub path may go through.
func proxyAllowed(method, path string) bool {
	switch method {
	case http.MethodPost:
		return hubReadOnly[path]
	case http.MethodGet, http.MethodHead:
		id, ok := strings.CutPrefix(path, "/api/artifacts/")
		return ok && artifactIDRE.MatchString(id)
	}
	return false
}

func (l *localAPI) proxyClient() *http.Client {
	l.proxyOnce.Do(func() { l.proxyHC = l.n.hubHTTP() })
	return l.proxyHC
}

func (l *localAPI) handleHubProxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/hub")
	if !proxyAllowed(r.Method, path) {
		writeJSONError(w, http.StatusForbidden, "not allowed through the node's read scope")
		return
	}
	l.n.mu.Lock()
	base := l.n.st.HubURL
	l.n.mu.Unlock()
	base, err := l.n.artifactBase(base)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "hub unreachable")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var body io.Reader
	if r.Method == http.MethodPost {
		body = http.MaxBytesReader(w, r.Body, proxyMaxRequest)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, base+path, body) //nolint:gosec // base is the node's configured hub; path passed the allowlist
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	if r.Method == http.MethodPost && r.ContentLength >= 0 {
		req.ContentLength = r.ContentLength
	}
	for _, h := range proxyRequestHeaders {
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[http.CanonicalHeaderKey(h)] = v
		}
	}
	req.Header.Set(hubScopeHeader, hubScopeNetworkRead)

	headerTimer := time.AfterFunc(proxyHeaderTimeout, cancel)
	resp, err := l.proxyClient().Do(req) //nolint:gosec // see above: fixed hub, allowlisted path
	headerTimer.Stop()
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		l.log.Debug("hub proxy", "path", path, "err", err)
		writeJSONError(w, http.StatusBadGateway, "hub unreachable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range proxyResponseHeaders {
		if v := resp.Header.Values(h); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(h)] = v
		}
	}
	for k, v := range resp.Header {
		if strings.HasPrefix(k, "Trailer-") {
			w.Header()[k] = v
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	idle := time.AfterFunc(proxyIdle, cancel)
	defer idle.Stop()
	buf := make([]byte, proxyChunk)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(proxyIdle)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}
