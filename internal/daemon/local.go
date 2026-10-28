package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Local API limits and timings.
const (
	localRPCTimeout   = 10 * time.Second
	inboxMaxWait      = 60 * time.Second
	inboxExtraTimeout = 5 * time.Second
	maxLocalBody      = 1 << 20 // the bus MaxPayload
	maxRegisterBody   = 4 << 10
	minRefreshDelay   = time.Second
)

// errAgentRevoked ends a session: the hub revoked the agent.
var errAgentRevoked = errors.New("agent revoked by hub")

// agentMethod describes one proxied agent RPC.
type agentMethod struct {
	subject string // bus method, e.g. bus.RPCAgentPost
	req     func() proto.Message
	resp    func() proto.Message
}

// agentMethods maps the URL method name (the bus method without "agent.")
// to its request and response types.
var agentMethods = func() map[string]agentMethod {
	list := []agentMethod{
		{bus.RPCAgentPost, func() proto.Message { return &mycelv1.AgentPostRequest{} }, func() proto.Message { return &mycelv1.PostResponse{} }},
		{bus.RPCAgentJoinChannel, func() proto.Message { return &mycelv1.AgentJoinChannelRequest{} }, func() proto.Message { return &mycelv1.AgentJoinChannelResponse{} }},
		{bus.RPCAgentLeaveChannel, func() proto.Message { return &mycelv1.AgentLeaveChannelRequest{} }, func() proto.Message { return &mycelv1.AgentLeaveChannelResponse{} }},
		{bus.RPCAgentListChannels, func() proto.Message { return &mycelv1.AgentListChannelsRequest{} }, func() proto.Message { return &mycelv1.ListChannelsResponse{} }},
		{bus.RPCAgentListAgents, func() proto.Message { return &mycelv1.AgentListAgentsRequest{} }, func() proto.Message { return &mycelv1.AgentListAgentsResponse{} }},
		{bus.RPCAgentWhoami, func() proto.Message { return &mycelv1.AgentWhoamiRequest{} }, func() proto.Message { return &mycelv1.AgentWhoamiResponse{} }},
		{bus.RPCAgentInboxPull, func() proto.Message { return &mycelv1.InboxPullRequest{} }, func() proto.Message { return &mycelv1.InboxPullResponse{} }},
		{bus.RPCAgentBBGet, func() proto.Message { return &mycelv1.AgentBlackboardGetRequest{} }, func() proto.Message { return &mycelv1.BlackboardGetResponse{} }},
		{bus.RPCAgentBBPut, func() proto.Message { return &mycelv1.AgentBlackboardPutRequest{} }, func() proto.Message { return &mycelv1.BlackboardPutResponse{} }},
		{bus.RPCAgentListMessages, func() proto.Message { return &mycelv1.AgentListMessagesRequest{} }, func() proto.Message { return &mycelv1.ListMessagesResponse{} }},
	}
	m := make(map[string]agentMethod, len(list))
	for _, am := range list {
		m[strings.TrimPrefix(am.subject, "agent.")] = am
	}
	return m
}()

// callField is the AgentCall field every agent request carries.
const callField protoreflect.Name = "call"

// agentSession is a registered local agent. The token never leaves the daemon.
type agentSession struct {
	id      string
	name    string
	agentID string
	harness string
	since   time.Time
	cancel  context.CancelFunc
	kick    chan struct{} // asks the refresh loop to refresh now

	mu        sync.Mutex
	token     string
	issued    time.Time // local receipt time of token
	expiresAt time.Time
}

func (s *agentSession) setToken(token, expiresAt string) {
	now := time.Now()
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		exp = now.Add(identity.AgentTokenLifetime)
	}
	s.mu.Lock()
	s.token, s.issued, s.expiresAt = token, now, exp
	s.mu.Unlock()
}

// kickRefresh asks the refresh loop to renew the token now (or end the
// session if the agent was revoked).
func (s *agentSession) kickRefresh() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *agentSession) currentToken() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.expiresAt
}

// refreshDelay is the time until 2/3 of the token's lifetime has passed.
func (s *agentSession) refreshDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	life := s.expiresAt.Sub(s.issued)
	if life <= 0 {
		// Clock skew between hub and node: fall back to the default lifetime.
		life = identity.AgentTokenLifetime
	}
	return max(time.Until(s.issued.Add(life*2/3)), minRefreshDelay)
}

// localAPI serves /api/local/* and owns the agent sessions.
type localAPI struct {
	n         *node
	log       *slog.Logger
	token     string
	tokenPath string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	regLocks  keyedLock    // per agent name: a name registers once
	pullLocks keyedLock    // per agent name: cursor read, pull and advance
	busWaits  atomic.Int32 // requests currently waiting for the bus (tests)

	mu       sync.Mutex
	closed   bool
	sessions map[string]*agentSession // by session id
	byName   map[string]*agentSession

	cursorMu sync.Mutex // guards cursor read-modify-write
}

// keyedLock is a set of per-key locks that can be abandoned on ctx done.
type keyedLock struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	ch   chan struct{} // holds one token while locked
	refs int
}

// lock acquires the lock for key; it returns ctx.Err() if ctx ends first.
func (k *keyedLock) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{ch: make(chan struct{}, 1)}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	release := func() {
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
	select {
	case e.ch <- struct{}{}:
		return func() { <-e.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// newLocalAPI creates a fresh local sign-in token and writes it to
// <NodeDir>/local.token (0600).
func newLocalAPI(n *node) (*localAPI, error) {
	tok, err := randomB64(32)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(n.dir, LocalTokenFile)
	if err := writeFileAtomic(path, []byte(tok)); err != nil {
		return nil, fmt.Errorf("write %s: %w", LocalTokenFile, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &localAPI{
		n: n, log: n.log.With("component", "local"), token: tok, tokenPath: path,
		ctx: ctx, cancel: cancel,
		sessions: map[string]*agentSession{}, byName: map[string]*agentSession{},
	}, nil
}

// close ends every session, stops the refresh loops and removes the token file.
func (l *localAPI) close() {
	l.mu.Lock()
	l.closed = true
	clear(l.sessions)
	clear(l.byName)
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
	if err := os.Remove(l.tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		l.log.Warn("remove local token file", "err", err)
	}
}

func (l *localAPI) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions)
}

func (l *localAPI) session(id string) *agentSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[id]
}

// endSession removes s (if still current) and stops its refresh loop.
func (l *localAPI) endSession(s *agentSession) bool {
	l.mu.Lock()
	cur, ok := l.sessions[s.id]
	if ok && cur == s {
		delete(l.sessions, s.id)
		if l.byName[s.name] == s {
			delete(l.byName, s.name)
		}
	}
	l.mu.Unlock()
	s.cancel()
	return ok && cur == s
}

// ---- HTTP ----

func (l *localAPI) register(mux *http.ServeMux) {
	lm := http.NewServeMux()
	lm.HandleFunc("POST /api/local/agents", l.handleRegister)
	lm.HandleFunc("GET /api/local/agents", l.handleList)
	lm.HandleFunc("DELETE /api/local/agents/{session}", l.handleDelete)
	lm.HandleFunc("POST /api/local/agents/{session}/rpc/{method}", l.handleRPC)
	lm.HandleFunc("/api/local/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/api/local/", l.logRequests(l.requireToken(lm)))
}

// requireToken checks the local sign-in token in constant time.
func (l *localAPI) requireToken(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(l.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mycel-local"`)
			writeJSONError(w, http.StatusUnauthorized, "missing or wrong local token")
			return
		}
		h.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests logs each local request at debug. Only the method, the route
// pattern (never the session id) and the status are logged.
func (l *localAPI) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(rec, r)
		if l.log.Enabled(r.Context(), slog.LevelDebug) {
			route := r.Pattern
			if m := r.PathValue("method"); m != "" {
				route += " " + m
			}
			l.log.Debug("local request", "method", r.Method, "route", route, "status", rec.code,
				"ms", time.Since(start).Milliseconds())
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// validHarness: 1-64 chars of [a-z0-9-].
func validHarness(h string) bool {
	if len(h) < 1 || len(h) > 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

type registerBody struct {
	Name    string `json:"name"`
	Harness string `json:"harness"`
}

type registerReply struct {
	AgentID string `json:"agent_id"`
	Session string `json:"session"`
}

func (l *localAPI) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body registerBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeBodyError(w, err, "invalid JSON body")
		return
	}
	if !identity.ValidNodeName(body.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid agent name: 1-63 chars of a-z, 0-9 and '-', not starting or ending with '-'")
		return
	}
	if !validHarness(body.Harness) {
		writeJSONError(w, http.StatusBadRequest, "invalid harness: 1-64 chars of a-z, 0-9 and '-'")
		return
	}

	ctx, cancel := l.requestContext(r.Context(), localRPCTimeout)
	defer cancel()
	unlock, err := l.regLocks.lock(ctx, body.Name)
	if err != nil {
		writeRPCError(w, err)
		return
	}
	defer unlock()
	l.mu.Lock()
	existing, closed := l.byName[body.Name], l.closed
	l.mu.Unlock()
	if closed {
		writeJSONError(w, http.StatusServiceUnavailable, "daemon is shutting down")
		return
	}
	if existing != nil {
		if existing.harness != body.Harness {
			writeJSONError(w, http.StatusConflict, "agent "+body.Name+" already has a session with harness "+existing.harness)
			return
		}
		writeJSON(w, http.StatusCreated, registerReply{AgentID: existing.agentID, Session: existing.id})
		return
	}

	resp := &mycelv1.RegisterAgentResponse{}
	err = l.n.hubRPC(ctx, bus.RPCRegisterAgent,
		&mycelv1.RegisterAgentRequest{AgentName: body.Name, SandboxId: "", Harness: body.Harness}, resp)
	if err != nil {
		writeRPCError(w, err)
		return
	}
	if resp.GetAgentId() != "agent:"+body.Name+"@"+l.n.name() || resp.GetToken() == "" {
		writeJSONError(w, http.StatusBadGateway, errBadReply.Error())
		return
	}
	sid, err := randomB64(24)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	sctx, scancel := context.WithCancel(l.ctx)
	s := &agentSession{
		id: sid, name: body.Name, agentID: resp.GetAgentId(), harness: body.Harness,
		since: time.Now().UTC(), cancel: scancel, kick: make(chan struct{}, 1),
	}
	s.setToken(resp.GetToken(), resp.GetExpiresAt())

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		scancel()
		writeJSONError(w, http.StatusServiceUnavailable, "daemon is shutting down")
		return
	}
	l.sessions[sid] = s
	l.byName[s.name] = s
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		l.refreshLoop(sctx, s)
	}()
	l.log.Info("local agent registered", "agent", s.agentID, "harness", s.harness)
	writeJSON(w, http.StatusCreated, registerReply{AgentID: s.agentID, Session: sid})
}

type agentListEntry struct {
	AgentID string `json:"agent_id"`
	Session string `json:"session"`
	Harness string `json:"harness"`
	Since   string `json:"since"`
}

func (l *localAPI) handleList(w http.ResponseWriter, _ *http.Request) {
	l.mu.Lock()
	list := make([]*agentSession, 0, len(l.sessions))
	for _, s := range l.sessions {
		list = append(list, s)
	}
	l.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	out := make([]agentListEntry, 0, len(list))
	for _, s := range list {
		out = append(out, agentListEntry{AgentID: s.agentID, Session: s.id, Harness: s.harness, Since: s.since.Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (l *localAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	s := l.session(r.PathValue("session"))
	if s == nil || !l.endSession(s) {
		writeJSONError(w, http.StatusNotFound, "unknown session")
		return
	}
	l.log.Info("local agent session ended", "agent", s.agentID)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// requestContext derives a context that ends with the request, the timeout
// or the local API's shutdown.
func (l *localAPI) requestContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, d)
	stop := context.AfterFunc(l.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (l *localAPI) handleRPC(w http.ResponseWriter, r *http.Request) {
	// Both the short name ("post") and the bus constant ("agent.post") work.
	am, ok := agentMethods[strings.TrimPrefix(r.PathValue("method"), "agent.")]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown method")
		return
	}
	s := l.session(r.PathValue("session"))
	if s == nil {
		writeJSONError(w, http.StatusNotFound, "unknown session")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLocalBody))
	if err != nil {
		writeBodyError(w, err, "could not read request body")
		return
	}
	req := am.req()
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := protojson.Unmarshal(body, req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body for "+am.subject)
			return
		}
	}
	m := req.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(callField)
	if fd == nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if m.Has(fd) {
		writeJSONError(w, http.StatusBadRequest, "the call field is set by the daemon")
		return
	}

	timeout := localRPCTimeout
	pull, isPull := req.(*mycelv1.InboxPullRequest)
	var wait time.Duration
	if isPull {
		pull.WaitMs = min(max(pull.GetWaitMs(), 0), int32(inboxMaxWait/time.Millisecond))
		wait = time.Duration(pull.GetWaitMs()) * time.Millisecond
	}

	if l.n.busConn() == nil {
		if wait <= 0 {
			writeRPCError(w, errBusDown)
			return
		}
		start := time.Now()
		wctx, wcancel := l.requestContext(r.Context(), wait)
		l.busWaits.Add(1)
		nc := l.n.waitBus(wctx, wait)
		l.busWaits.Add(-1)
		cancelled := wctx.Err() != nil && !errors.Is(wctx.Err(), context.DeadlineExceeded)
		wcancel()
		if nc == nil {
			if cancelled {
				writeRPCError(w, context.Canceled)
			} else {
				writeRPCError(w, errBusDown)
			}
			return
		}
		wait = max(wait-time.Since(start), 0)
		pull.WaitMs = int32(min(wait, inboxMaxWait).Milliseconds()) // #nosec G115 -- at most 60000
	}
	if isPull {
		timeout = wait + inboxExtraTimeout
	}
	ctx, cancel := l.requestContext(r.Context(), timeout)
	defer cancel()
	if isPull && pull.GetAfterSeq() == 0 {
		// The daemon's cursor: one pull at a time per agent, from reading the
		// cursor to advancing it, so two clients never get the same batch.
		unlock, err := l.pullLocks.lock(ctx, s.name)
		if err != nil {
			writeRPCError(w, err)
			return
		}
		defer unlock()
		pull.AfterSeq = l.readCursor(s.name)
	}

	token, _ := s.currentToken()
	m.Set(fd, protoreflect.ValueOfMessage((&mycelv1.AgentCall{Token: token}).ProtoReflect()))

	resp := am.resp()
	if err := l.n.hubRPC(ctx, am.subject, req, resp); err != nil {
		var he *hubError
		if errors.As(err, &he) && agentRefused(he.msg) {
			s.kickRefresh()
		}
		writeRPCError(w, err)
		return
	}
	if isPull {
		if err := l.advanceCursor(s.name, resp.(*mycelv1.InboxPullResponse).GetNextSeq()); err != nil {
			l.log.Warn("store inbox cursor", "agent", s.agentID, "err", err)
		}
	}
	b, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(resp)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// codeStatus maps a hub error's leading connect code name to an HTTP status.
var codeStatus = map[string]int{
	"invalid_argument":   http.StatusBadRequest,
	"unauthenticated":    http.StatusForbidden, // 401 is reserved for the local token
	"permission_denied":  http.StatusForbidden,
	"not_found":          http.StatusNotFound,
	"resource_exhausted": http.StatusTooManyRequests,
}

// writeBodyError answers 413 for an oversized body and 400 otherwise.
func writeBodyError(w http.ResponseWriter, err error, msg string) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeJSONError(w, http.StatusBadRequest, msg)
}

// errCode returns the leading connect code name of a hub error text.
func errCode(msg string) string {
	name, _, ok := strings.Cut(msg, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

func writeRPCError(w http.ResponseWriter, err error) {
	var he *hubError
	switch {
	case errors.As(err, &he):
		code := http.StatusInternalServerError
		if c, ok := codeStatus[errCode(he.msg)]; ok {
			code = c
		}
		writeJSONError(w, code, he.msg)
	case errors.Is(err, context.Canceled):
		writeJSONError(w, http.StatusServiceUnavailable, "request cancelled")
	case errors.Is(err, errBusDown), errors.Is(err, errNoResponder):
		writeJSONError(w, http.StatusServiceUnavailable, errBusDown.Error())
	case errors.Is(err, errHubTimeout), errors.Is(err, context.DeadlineExceeded):
		writeJSONError(w, http.StatusGatewayTimeout, errHubTimeout.Error())
		writeJSONError(w, http.StatusGatewayTimeout, err.Error())
	case errors.Is(err, errBadReply):
		writeJSONError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// ---- token refresh ----

// refreshLoop refreshes s's token at 2/3 of its lifetime until ctx ends or
// the hub revokes the agent.
func (l *localAPI) refreshLoop(ctx context.Context, s *agentSession) {
	delay := s.refreshDelay()
	for {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-s.kick:
			t.Stop()
		}
		err := l.refreshOnce(ctx, s)
		switch {
		case err == nil:
			delay = s.refreshDelay()
		case errors.Is(err, errAgentRevoked):
			if l.endSession(s) {
				l.log.Warn("local agent revoked by hub; session ended", "agent", s.agentID)
			}
			return
		case ctx.Err() != nil:
			return
		default:
			l.log.Warn("agent token refresh failed; will retry", "agent", s.agentID, "err", err, "in", l.n.cfg.agentRetryEvery)
			delay = l.n.cfg.agentRetryEvery
		}
	}
}

// revokedPrefix is how the hub answers for a revoked agent.
const revokedPrefix = "permission_denied: agent revoked"

// isRevokedMsg reports whether a hub error says the agent is revoked: the
// hub's prefix first, any "revoked" as fallback for older hubs.
func isRevokedMsg(msg string) bool {
	return strings.HasPrefix(msg, revokedPrefix) || strings.Contains(strings.ToLower(msg), "revoked")
}

// agentRefused reports whether an agent RPC error means the token may be
// bad or the agent revoked; the refresh loop is then kicked. An ordinary
// policy refusal (permission_denied on a channel) does not count.
func agentRefused(msg string) bool {
	c := errCode(msg)
	return c == "unauthenticated" || strings.HasPrefix(msg, revokedPrefix) || strings.Contains(strings.ToLower(msg), "agent revoked")
}

// tokenRefused reports whether a refresh refusal means the token itself is
// no good (invalid or expired), so registering again can help.
func tokenRefused(msg string) bool {
	l := strings.ToLower(msg)
	return errCode(msg) == "unauthenticated" || strings.Contains(l, "invalid agent token") ||
		strings.Contains(l, "invalid token") || strings.Contains(l, "expired")
}

// refreshOnce renews s's token with agent.refresh. When the hub refuses the
// token itself (invalid or expired, for example because it expired while the
// hub was unreachable) it registers the agent again under the same name;
// other refusals are retried later.
func (l *localAPI) refreshOnce(ctx context.Context, s *agentSession) error {
	token, exp := s.currentToken()
	if time.Now().Before(exp) {
		rctx, cancel := context.WithTimeout(ctx, localRPCTimeout)
		resp := &mycelv1.RefreshAgentTokenResponse{}
		err := l.n.hubRPC(rctx, bus.RPCRefreshAgentToken, &mycelv1.RefreshAgentTokenRequest{AgentId: s.agentID, Token: token}, resp)
		cancel()
		if err == nil {
			if resp.GetToken() == "" {
				return errBadReply
			}
			s.setToken(resp.GetToken(), resp.GetExpiresAt())
			return nil
		}
		var he *hubError
		if !errors.As(err, &he) {
			return err
		}
		if isRevokedMsg(he.msg) {
			return errAgentRevoked
		}
		if !tokenRefused(he.msg) {
			return err
		}
	}
	rctx, cancel := context.WithTimeout(ctx, localRPCTimeout)
	defer cancel()
	resp := &mycelv1.RegisterAgentResponse{}
	err := l.n.hubRPC(rctx, bus.RPCRegisterAgent,
		&mycelv1.RegisterAgentRequest{AgentName: s.name, SandboxId: "", Harness: s.harness}, resp)
	if err != nil {
		var he *hubError
		if errors.As(err, &he) && isRevokedMsg(he.msg) {
			return errAgentRevoked
		}
		return err
	}
	if resp.GetAgentId() != s.agentID || resp.GetToken() == "" {
		return errBadReply
	}
	s.setToken(resp.GetToken(), resp.GetExpiresAt())
	return nil
}

// ---- inbox cursors ----

type cursorFile struct {
	Cursor uint64 `json:"cursor"`
}

func (l *localAPI) cursorPath(name string) string {
	return filepath.Join(l.n.dir, AgentsDir, name+".json")
}

func (l *localAPI) loadCursor(name string) uint64 {
	b, err := os.ReadFile(l.cursorPath(name))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			l.log.Warn("read inbox cursor", "agent", name, "err", err)
		}
		return 0
	}
	var c cursorFile
	if err := json.Unmarshal(b, &c); err != nil {
		l.log.Warn("parse inbox cursor; starting from 0", "agent", name, "err", err)
		return 0
	}
	return c.Cursor
}

func (l *localAPI) readCursor(name string) uint64 {
	l.cursorMu.Lock()
	defer l.cursorMu.Unlock()
	return l.loadCursor(name)
}

// advanceCursor stores max(stored, next): the cursor never moves backwards.
func (l *localAPI) advanceCursor(name string, next uint64) error {
	l.cursorMu.Lock()
	defer l.cursorMu.Unlock()
	if next <= l.loadCursor(name) {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(l.n.dir, AgentsDir), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cursorFile{Cursor: next})
	if err != nil {
		return err
	}
	return writeFileAtomic(l.cursorPath(name), b)
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
