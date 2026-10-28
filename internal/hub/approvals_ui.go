package hub

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// The minimal approvals page (docs/decisions.md D024): server-rendered
// HTML without JavaScript so that "an admin approves it in the hub UI"
// works before the phase 6 UI. Sign-in takes an API key and sets a session
// cookie; every form carries a CSRF token bound to the session; decisions
// go through the same code (and policy) as the GrantService RPCs.

const (
	uiSessionCookie = "mycel_session"
	uiLoginCookie   = "mycel_login"
	uiSessionTTL    = 24 * time.Hour
	uiLoginTTL      = 15 * time.Minute
	maxUISessions   = 1024
	maxUserSessions = 8
	maxUIForm       = 8 << 10
	uiLoginsPerMin  = 10
	uiLoginBurst    = 10
)

type uiSession struct {
	userID  string
	csrf    string
	expires time.Time
	flash   string
}

// uiState holds the page's sessions (in memory: a hub restart signs
// everyone out).
type uiState struct {
	mu       sync.Mutex
	sessions map[string]*uiSession
	logins   *rateLimiter
}

func newUIState() *uiState {
	return &uiState{sessions: map[string]*uiSession{}, logins: newRateLimiter(uiLoginsPerMin, uiLoginBurst)}
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokensEqual(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// newSession stores a session for user and returns its id.
func (u *uiState) newSession(userID string) (string, *uiSession) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	for id, s := range u.sessions {
		if now.After(s.expires) {
			delete(u.sessions, id)
		}
	}
	// Drop the session closest to expiry: the user's own past the per-user
	// cap, anyone's past the global cap.
	dropOldest := func(match func(*uiSession) bool) {
		var oldest string
		for id, s := range u.sessions {
			if match(s) && (oldest == "" || s.expires.Before(u.sessions[oldest].expires)) {
				oldest = id
			}
		}
		delete(u.sessions, oldest)
	}
	mine := 0
	for _, s := range u.sessions {
		if s.userID == userID {
			mine++
		}
	}
	if mine >= maxUserSessions {
		dropOldest(func(s *uiSession) bool { return s.userID == userID })
	}
	if len(u.sessions) >= maxUISessions {
		dropOldest(func(*uiSession) bool { return true })
	}
	id := randomToken()
	s := &uiSession{userID: userID, csrf: randomToken(), expires: now.Add(uiSessionTTL)}
	u.sessions[id] = s
	return id, s
}

// session returns a copy of the live session for id.
func (u *uiState) session(id string) (uiSession, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.sessions[id]
	if !ok {
		return uiSession{}, false
	}
	if time.Now().After(s.expires) {
		delete(u.sessions, id)
		return uiSession{}, false
	}
	return *s, true
}

func (u *uiState) setFlash(id, msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.sessions[id]; ok {
		s.flash = msg
	}
}

func (u *uiState) takeFlash(id string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.sessions[id]
	if !ok {
		return ""
	}
	f := s.flash
	s.flash = ""
	return f
}

// dropUser ends every session of a user and reports how many there were.
func (u *uiState) dropUser(userID string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for id, s := range u.sessions {
		if s.userID == userID {
			delete(u.sessions, id)
			n++
		}
	}
	return n
}

func (u *uiState) drop(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.sessions, id)
}

func (s *server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/login", s.uiLoginPage)
	mux.HandleFunc("POST /ui/login", s.uiLogin)
	mux.HandleFunc("POST /ui/logout", s.uiLogout)
	mux.HandleFunc("GET /ui/approvals", s.uiApprovals)
	mux.HandleFunc("POST /ui/approvals/{id}/{action}", s.uiDecide)
}

// uiHeaders sets the page's security headers: no scripts, no framing, no
// caching, forms only to this origin.
func uiHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
	if ttl > 0 {
		c.MaxAge = int(ttl / time.Second)
	} else {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// sameOrigin refuses cross-site form posts when the browser says where
// they came from (defence in depth next to SameSite and the CSRF token).
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		// The full origin (scheme and host); "null" (sandboxed frames,
		// some redirects) is cross-site.
		return o != "null" && strings.EqualFold(o, "https://"+r.Host)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	return true
}

func parseUIForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxUIForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return false
	}
	return true
}

var uiTemplates = template.Must(template.New("ui").Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem;background:#0d0f12;color:#d8dde3}
a{color:#8ab4f8}table{border-collapse:collapse;width:100%}td,th{border-bottom:1px solid #2a2f36;padding:.5rem;text-align:left;vertical-align:top}
input,button{font:inherit;padding:.3rem .5rem;background:#161a1f;color:#d8dde3;border:1px solid #39404a}
.msg{border:1px solid #39404a;padding:.5rem;margin:1rem 0}.mono{font-family:ui-monospace,monospace;font-size:.9em}
form.inline{display:inline-block;margin:.2rem 0}</style></head><body>{{end}}
{{define "login"}}{{template "head" .}}<h1>Mycelium hub {{.Hub}}</h1>
{{if .Flash}}<p class="msg">{{.Flash}}</p>{{end}}
<form method="post" action="/ui/login"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label>API key <input type="password" name="api_key" autocomplete="off" required></label> <button type="submit">Sign in</button></form>
</body></html>{{end}}
{{define "approvals"}}{{template "head" .}}<h1>Approvals</h1>
<p>Signed in as {{.User}} ({{.Role}}). <form class="inline" method="post" action="/ui/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Sign out</button></form></p>
{{if .Flash}}<p class="msg">{{.Flash}}</p>{{end}}
{{if .Grants}}<table><tr><th>Grant</th><th>Request</th><th>Decision</th></tr>
{{range .Grants}}<tr><td class="mono">{{.ID}}<br>{{.Created}}</td>
<td><b>{{.Kind}}</b><br>subject {{.Subject}}<br>node {{.Node}}<br>duration {{.Duration}}<br>requested by {{.RequestedBy}}{{if .Channel}} in {{.Channel}}{{end}}<br>reason: {{.Reason}}</td>
<td><form method="post" action="/ui/approvals/{{.ID}}/approve"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<input name="duration" placeholder="{{.Duration}}" size="6" aria-label="duration"> <input name="note" placeholder="note" size="14" aria-label="note"> <button type="submit">Approve</button></form>
<form method="post" action="/ui/approvals/{{.ID}}/deny"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<input name="note" placeholder="note" size="14" aria-label="note"> <button type="submit">Deny</button></form></td></tr>
{{end}}</table>{{else}}<p>No pending grants that you can decide.</p>{{end}}
</body></html>{{end}}
`))

type uiGrant struct {
	ID, Created, Kind, Subject, Node, Duration, RequestedBy, Channel, Reason string
}

func (s *server) uiLoginPage(w http.ResponseWriter, _ *http.Request) {
	tok := randomToken()
	setCookie(w, uiLoginCookie, tok, "/ui/login", uiLoginTTL)
	s.renderUI(w, http.StatusOK, "login", map[string]any{"Title": "Sign in", "Hub": string(s.td), "CSRF": tok})
}

func (s *server) renderUI(w http.ResponseWriter, code int, name string, data map[string]any) {
	uiHeaders(w)
	w.WriteHeader(code)
	if err := uiTemplates.ExecuteTemplate(w, name, data); err != nil {
		s.log.Warn("render ui", "page", name, "err", err)
	}
}

// uiLogin checks an API key and starts a session. The login form is bound
// to a short-lived cookie (double submit) so another site cannot sign a
// browser in under someone else's key.
func (s *server) uiLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if !parseUIForm(w, r) {
		return
	}
	c, err := r.Cookie(uiLoginCookie)
	if err != nil || !tokensEqual(c.Value, r.PostForm.Get("csrf")) {
		http.Error(w, "form expired; reload the sign-in page", http.StatusForbidden)
		return
	}
	retry := func(code int, msg string) {
		tok := randomToken()
		setCookie(w, uiLoginCookie, tok, "/ui/login", uiLoginTTL)
		s.renderUI(w, code, "login", map[string]any{"Title": "Sign in", "Hub": string(s.td), "CSRF": tok, "Flash": msg})
	}
	if !s.ui.logins.allow(remoteIP(r.RemoteAddr)) {
		s.auditDenied(r.Context(), "ui", "ui.login", "ui", map[string]any{"reason": "rate limited", "remote": remoteIP(r.RemoteAddr)})
		retry(http.StatusTooManyRequests, "Too many sign-in attempts; wait a minute.")
		return
	}
	u, _, err := s.st.lookupAPIKey(r.Context(), strings.TrimSpace(r.PostForm.Get("api_key")))
	if err != nil {
		if !errors.Is(err, errNotFound) {
			s.log.Error("ui login: api key lookup", "err", err)
		}
		s.audit(r.Context(), "ui", "ui.login", "ui", outcomeDenied, map[string]any{"remote": remoteIP(r.RemoteAddr)})
		retry(http.StatusUnauthorized, "That API key is not valid.")
		return
	}
	setCookie(w, uiLoginCookie, "", "/ui/login", 0)
	s.startSession(w, u) // one session for /ui/* and the app
	s.audit(r.Context(), u.ID, "ui.login", "ui", outcomeOK, map[string]any{"remote": remoteIP(r.RemoteAddr)})
	http.Redirect(w, r, "/ui/approvals", http.StatusSeeOther)
}

// uiUser resolves the session of r and its (current) user; ok is false
// when the caller must sign in.
func (s *server) uiUser(r *http.Request) (string, uiSession, *userRow, bool) {
	c, err := r.Cookie(uiSessionCookie)
	if err != nil {
		return "", uiSession{}, nil, false
	}
	sess, ok := s.ui.session(c.Value)
	if !ok {
		return "", uiSession{}, nil, false
	}
	u, err := s.st.getUser(r.Context(), sess.userID)
	if err != nil {
		s.ui.drop(c.Value)
		return "", uiSession{}, nil, false
	}
	return c.Value, sess, u, true
}

func (s *server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if !parseUIForm(w, r) {
		return
	}
	if id, sess, u, ok := s.uiUser(r); ok {
		if !tokensEqual(sess.csrf, r.PostForm.Get("csrf")) {
			http.Error(w, "invalid form token", http.StatusForbidden)
			return
		}
		s.ui.drop(id)
		s.audit(r.Context(), u.ID, "ui.logout", "ui", outcomeOK, nil)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// decidable reports whether u may approve or deny g (admin or the owner
// of g's node); it asks the policy without auditing.
func (s *server) decidable(ctx context.Context, u *userRow, g *mycelv1.Grant, owners map[string]string) bool {
	owner, ok := owners[g.GetNode()]
	if !ok {
		var err error
		if owner, err = s.nodeOwner(ctx, g.GetNode()); err != nil {
			return false
		}
		owners[g.GetNode()] = owner
	}
	return s.policy.Authorize(ctx, principalOf(u), policy.ActionGrantApprove,
		policy.Resource{Kind: "grant", ID: g.GetId(), OwnerID: owner}).Allow
}

func (s *server) uiApprovals(w http.ResponseWriter, r *http.Request) {
	id, sess, u, ok := s.uiUser(r)
	if !ok {
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	f := grantFilter{states: []mycelv1.GrantState{mycelv1.GrantState_GRANT_STATE_PENDING}, limit: maxGrantList}
	if u.Role != roleAdmin {
		f.nodeOwner = u.ID // only grants on the member's own nodes are theirs to decide
	}
	rows, err := s.st.listGrants(ctx, f)
	if err != nil {
		s.log.Error("ui: list grants", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	owners := map[string]string{}
	var list []uiGrant
	for _, g := range rows {
		if !s.decidable(ctx, u, g, owners) {
			continue
		}
		reason := g.GetReason()
		if reason == "" {
			reason = "(none)"
		}
		list = append(list, uiGrant{
			ID: g.GetId(), Created: g.GetCreatedAt(), Kind: grantSummary(g), Subject: g.GetSubject(), Node: g.GetNode(),
			Duration: g.GetDuration(), RequestedBy: g.GetRequestedBy(), Channel: g.GetChannel(), Reason: reason,
		})
	}
	s.renderUI(w, http.StatusOK, "approvals", map[string]any{
		"Title": "Approvals", "User": u.Name, "Role": u.Role, "CSRF": sess.csrf, "Grants": list, "Flash": s.ui.takeFlash(id),
	})
}

func (s *server) uiDecide(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	id, sess, u, ok := s.uiUser(r)
	if !ok {
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
		return
	}
	if !parseUIForm(w, r) {
		return
	}
	if !tokensEqual(sess.csrf, r.PostForm.Get("csrf")) {
		s.auditDenied(r.Context(), u.ID, "ui.decide", "ui", map[string]any{"reason": "invalid csrf token"})
		http.Error(w, "invalid form token; reload the page", http.StatusForbidden)
		return
	}
	gid := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "approve":
		_, err = s.approveGrant(r.Context(), u, gid, strings.TrimSpace(r.PostForm.Get("duration")), r.PostForm.Get("note"))
	case "deny":
		_, err = s.denyGrant(r.Context(), u, gid, r.PostForm.Get("note"))
	default:
		http.NotFound(w, r)
		return
	}
	if !validGrantID(gid) {
		gid = "the grant"
	}
	if err == nil {
		past := map[string]string{"approve": "approved", "deny": "denied"}[r.PathValue("action")]
		s.ui.setFlash(id, "Grant "+gid+" "+past+".")
	} else {
		msg := "internal error"
		var ce *connect.Error
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		s.ui.setFlash(id, "Could not "+r.PathValue("action")+" "+gid+": "+msg)
	}
	http.Redirect(w, r, "/ui/approvals", http.StatusSeeOther)
}
