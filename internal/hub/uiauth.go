package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Phase 6 authentication of browser and node-UI requests (uiapi.go): the
// cookie session with its CSRF header, and the node read scope.

const (
	scopeHeader      = "Mycel-Scope"
	scopeNetworkRead = "network:read"
	csrfHeader       = "Mycel-Csrf"

	ctxReadScope ctxKey = 101 // node name of a read-scoped request
	ctxSession   ctxKey = 102 // session id of a cookie-authenticated request
)

// readOnlyMethods are the Connect procedures a node read scope may call
// (the daemon's /api/hub proxy keeps the same list, checked by a test).
var readOnlyMethods = map[string]bool{
	mycelv1connect.TokenServiceListJoinTokensProcedure:    true,
	mycelv1connect.NodeServiceListNodesProcedure:          true,
	mycelv1connect.NodeServiceGetNodeProcedure:            true,
	mycelv1connect.UserServiceListUsersProcedure:          true,
	mycelv1connect.UserServiceWhoamiProcedure:             true,
	mycelv1connect.AgentServiceListAgentsProcedure:        true,
	mycelv1connect.AgentServiceGetAgentProcedure:          true,
	mycelv1connect.AgentServiceGetChannelUsageProcedure:   true,
	mycelv1connect.ChannelServiceListChannelsProcedure:    true,
	mycelv1connect.ChannelServiceGetChannelProcedure:      true,
	mycelv1connect.ChannelServiceListMessagesProcedure:    true,
	mycelv1connect.ChannelServiceWatchProcedure:           true,
	mycelv1connect.ChannelServiceBlackboardGetProcedure:   true,
	mycelv1connect.GrantServiceListGrantsProcedure:        true,
	mycelv1connect.GrantServiceGetGrantProcedure:          true,
	mycelv1connect.ControlServiceGetNodeSharingProcedure:  true,
	mycelv1connect.ControlServiceFreezeStatusProcedure:    true,
	mycelv1connect.EventServiceWatchEventsProcedure:       true,
	mycelv1connect.EventServiceSearchProcedure:            true,
	mycelv1connect.EventServiceListAuditProcedure:         true,
	mycelv1connect.JobServiceGetJobProcedure:              true,
	mycelv1connect.JobServiceListJobsProcedure:            true,
	mycelv1connect.JobServiceStreamLogsProcedure:          true, // the inspector's logs tab
	mycelv1connect.JobServiceListProfilesProcedure:        true,
	mycelv1connect.JobServiceListNodesForProfileProcedure: true,
	mycelv1connect.ArtifactServiceListArtifactsProcedure:  true,
	mycelv1connect.ArtifactServiceGetArtifactProcedure:    true,
}

// readScopeOf returns the node name when ctx is a node read scope.
func readScopeOf(ctx context.Context) string {
	n, _ := ctx.Value(ctxReadScope).(string)
	return n
}

func sessionOf(ctx context.Context) string {
	id, _ := ctx.Value(ctxSession).(string)
	return id
}

// readScopeHTTPAllowed reports whether a read-scoped request may reach a
// non-Connect route: only artifact downloads and the public UI routes; the
// Connect procedures are checked by the scope interceptor.
func readScopeHTTPAllowed(r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/ui/") {
		return true
	}
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case get && strings.HasPrefix(p, "/api/artifacts/") && !strings.Contains(strings.TrimPrefix(p, "/api/artifacts/"), "/"):
		return true
	case get && p == "/api/ui-config":
		return true
	}
	return false
}

// readScopeUser resolves a node read scope: the node's owner as a member.
func (s *server) readScopeUser(ctx context.Context, np *nodePeer) *userRow {
	if s.st.IsRevoked(identity.SerialHex(np.Cert), identity.NodeSPIFFEID(s.td, np.Name)) {
		return nil
	}
	n, err := s.st.getNode(ctx, np.Name)
	if err != nil {
		if !errors.Is(err, errNotFound) {
			s.log.Error("read scope: get node", "err", err)
		}
		return nil
	}
	u, err := s.st.getUser(ctx, n.OwnerID)
	if err != nil {
		if !errors.Is(err, errNotFound) {
			s.log.Error("read scope: get user", "err", err)
		}
		return nil
	}
	cp := *u
	cp.Role = roleMember // the owner's visibility, never admin rights
	return &cp
}

// cookieUser resolves the session cookie of r into its user when the
// request passes the CSRF rules: plain GETs need the same-origin check,
// everything else also the session's token in Mycel-Csrf.
func (s *server) cookieUser(r *http.Request) (string, *userRow, bool) {
	if _, err := r.Cookie(uiSessionCookie); err != nil {
		return "", nil, false
	}
	if !s.originAllowed(r) {
		return "", nil, false
	}
	id, sess, u, ok := s.uiUser(r)
	if !ok {
		return "", nil, false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !tokensEqual(sess.csrf, r.Header.Get(csrfHeader)) {
		return "", nil, false
	}
	return id, u, true
}

// originAllowed is sameOrigin, plus the origins configured in hub.yaml.
func (s *server) originAllowed(r *http.Request) bool {
	if sameOrigin(r) {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" || strings.EqualFold(o, "null") {
		return false
	}
	for _, allowed := range s.uiCfg.origins {
		if strings.EqualFold(o, allowed) {
			return true
		}
	}
	return false
}

// scopeInterceptor refuses every Connect procedure outside readOnlyMethods
// to a node read scope.
type scopeInterceptor struct{ s *server }

func (si scopeInterceptor) check(ctx context.Context, procedure string) error {
	node := readScopeOf(ctx)
	if node == "" || readOnlyMethods[procedure] {
		return nil
	}
	si.s.auditDenied(ctx, "node:"+node, "scope.network_read", procedure, map[string]any{"reason": "node read scope"})
	return connect.NewError(connect.CodePermissionDenied, errors.New("node read scope"))
}

func (si scopeInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := si.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (si scopeInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (si scopeInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := si.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// ---- JSON helpers ----

func writeAPIJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	writeAPIJSON(w, code, map[string]string{"error": msg})
}

// readAPIJSON decodes a small JSON body into v (an empty body is {}).
func readAPIJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// sessionUser returns the user of a cookie-authenticated request, or
// answers 401.
func (s *server) sessionUser(w http.ResponseWriter, r *http.Request) (string, *userRow, bool) {
	id, u := sessionOf(r.Context()), userFrom(r.Context())
	if id == "" || u == nil {
		writeAPIError(w, http.StatusUnauthorized, "sign in first")
		return "", nil, false
	}
	return id, u, true
}

// ---- /api/auth ----

type passkeyInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
}

func (s *server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	id, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	sess, ok := s.ui.session(id)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "sign in first")
		return
	}
	rows, err := s.st.passkeysOf(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list passkeys", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	keys := []passkeyInfo{}
	for _, p := range rows {
		keys = append(keys, passkeyInfo{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt})
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"user_id": u.ID, "name": u.Name, "role": u.Role, "csrf": sess.csrf, "passkeys": keys,
	})
}

func (s *server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	id, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	s.ui.drop(id)
	clearSessionCookie(w)
	s.audit(r.Context(), u.ID, "ui.logout", "ui", outcomeOK, nil)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// startSession creates a session for u and sets its cookie (and drops a
// phase 5 cookie at /ui, which would otherwise shadow it there).
func (s *server) startSession(w http.ResponseWriter, u *userRow) *uiSession {
	id, sess := s.ui.newSession(u.ID)
	setCookie(w, uiSessionCookie, "", "/ui", 0)
	setCookie(w, uiSessionCookie, id, "/", uiSessionTTL)
	return sess
}

// handleAuthLogoutAll ends every session of the caller, on every device.
func (s *server) handleAuthLogoutAll(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	n := s.ui.dropUser(u.ID)
	clearSessionCookie(w)
	s.audit(r.Context(), u.ID, "ui.logout_all", "ui", outcomeOK, map[string]any{"sessions": n})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// clearSessionCookie removes the cookie at "/" and the phase 5 path.
func clearSessionCookie(w http.ResponseWriter) {
	setCookie(w, uiSessionCookie, "", "/", 0)
	setCookie(w, uiSessionCookie, "", "/ui", 0)
}
