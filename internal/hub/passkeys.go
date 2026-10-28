package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/richard-sen27/mycelium/internal/channels"
)

// Passkeys (uiapi.go): WebAuthn registration for a signed-in user and
// discoverable login that starts a cookie session. Ceremony state stays on
// the hub, one pending ceremony per session (registration) or per login
// ceremony cookie (a random id in a short-lived HttpOnly cookie), for two
// minutes and used once; the login rate limit stays per remote IP.

const (
	passkeyChallengeTTL   = 2 * time.Minute
	maxPasskeysPerUser    = 16
	maxPendingCeremonies  = 4096
	maxPasskeyBody        = 64 << 10
	maxPasskeyName        = 64
	passkeyCeremonyCookie = "mycel_pk"
	passkeyCookiePath     = "/api/auth/passkey/login" //nolint:gosec // a cookie path, not a credential
)

type ceremony struct {
	data    webauthn.SessionData
	userID  string
	name    string
	expires time.Time
}

type passkeyState struct {
	wa      *webauthn.WebAuthn
	mu      sync.Mutex
	pending map[string]*ceremony
	now     func() time.Time
}

// newPasskeyState configures WebAuthn from the UI settings; it fails when
// the RP id is not a domain (for example a bare IP address).
func newPasskeyState(cfg uiSettings) (*passkeyState, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID: cfg.rpID, RPDisplayName: "Mycelium", RPOrigins: cfg.origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification: protocol.VerificationPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyChallengeTTL, TimeoutUVD: passkeyChallengeTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyChallengeTTL, TimeoutUVD: passkeyChallengeTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	if err := protocol.ValidateRPID(cfg.rpID); err != nil {
		return nil, err
	}
	return &passkeyState{wa: wa, pending: map[string]*ceremony{}, now: time.Now}, nil
}

// put stores the ceremony for key, replacing an earlier one.
func (p *passkeyState) put(key string, c *ceremony) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, v := range p.pending {
		if now.After(v.expires) {
			delete(p.pending, k)
		}
	}
	if _, ok := p.pending[key]; !ok && len(p.pending) >= maxPendingCeremonies {
		return false
	}
	c.expires = now.Add(passkeyChallengeTTL)
	p.pending[key] = c
	return true
}

// take removes and returns the live ceremony for key (single use).
func (p *passkeyState) take(key string) (*ceremony, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.pending[key]
	delete(p.pending, key)
	if !ok || p.now().After(c.expires) {
		return nil, false
	}
	return c, true
}

// waUser adapts a user and their credentials to webauthn.User. The user
// handle is the user id.
type waUser struct {
	u     *userRow
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return []byte(w.u.ID) }
func (w *waUser) WebAuthnName() string                       { return w.u.Name }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.Name }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *server) loadWAUser(ctx context.Context, u *userRow) (*waUser, []*passkeyRow, error) {
	rows, err := s.st.passkeysOf(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	w := &waUser{u: u}
	for _, r := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(r.Credential), &c); err != nil {
			s.log.Warn("stored passkey does not decode", "passkey", r.ID, "err", err)
			continue
		}
		w.creds = append(w.creds, c)
	}
	return w, rows, nil
}

func credentialID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

func (s *server) passkeysAvailable(w http.ResponseWriter) bool {
	if s.passkeys == nil {
		writeAPIError(w, http.StatusNotFound, "passkeys need a domain name: set ui.rp_id or ui.external_url in hub.yaml")
		return false
	}
	return true
}

func validPasskeyName(n string) bool {
	return n != "" && len(n) <= maxPasskeyName && channels.IsPrintable(n)
}

// readBody reads a bounded request body.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	return b, true
}

func (s *server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	sid, u, ok := s.sessionUser(w, r)
	if !ok || !s.passkeysAvailable(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !readAPIJSON(w, r, &body, maxPasskeyBody) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		body.Name = "passkey"
	}
	if !validPasskeyName(body.Name) {
		writeAPIError(w, http.StatusBadRequest, "name: 1-64 printable characters")
		return
	}
	wu, rows, err := s.loadWAUser(r.Context(), u)
	if err != nil {
		s.log.Error("load passkeys", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(rows) >= maxPasskeysPerUser {
		writeAPIError(w, http.StatusConflict, "too many passkeys; delete one first")
		return
	}
	creation, data, err := s.passkeys.wa.BeginRegistration(wu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(wu.creds).CredentialDescriptors()))
	if err != nil {
		s.log.Error("passkey registration begin", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !s.passkeys.put("reg:"+sid, &ceremony{data: *data, userID: u.ID, name: body.Name}) {
		writeAPIError(w, http.StatusServiceUnavailable, "too many sign-in ceremonies in progress; try again")
		return
	}
	writeAPIJSON(w, http.StatusOK, creation)
}

func (s *server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	sid, u, ok := s.sessionUser(w, r)
	if !ok || !s.passkeysAvailable(w) {
		return
	}
	b, ok := readBody(w, r, maxPasskeyBody)
	if !ok {
		return
	}
	c, ok := s.passkeys.take("reg:" + sid)
	if !ok || c.userID != u.ID {
		writeAPIError(w, http.StatusBadRequest, "no passkey registration in progress (it lasts two minutes)")
		return
	}
	fail := func(reason string) {
		s.audit(r.Context(), u.ID, "passkey.register", "user:"+u.ID, outcomeDenied, map[string]any{"reason": reason})
		writeAPIError(w, http.StatusBadRequest, "passkey registration failed: "+reason)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(b))
	if err != nil {
		fail("invalid credential")
		return
	}
	wu, _, err := s.loadWAUser(r.Context(), u)
	if err != nil {
		s.log.Error("load passkeys", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	cred, err := s.passkeys.wa.CreateCredential(wu, c.data, parsed)
	if err != nil {
		fail(webauthnReason(err))
		return
	}
	enc, err := json.Marshal(cred)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	row := &passkeyRow{ID: credentialID(cred.ID), UserID: u.ID, Name: c.name, Credential: string(enc), CreatedAt: nowString()}
	switch err := s.st.insertPasskey(r.Context(), row, maxPasskeysPerUser); {
	case errors.Is(err, errTooMany):
		writeAPIError(w, http.StatusConflict, "too many passkeys; delete one first")
		return
	case errors.Is(err, errConflict):
		fail("credential already registered")
		return
	case err != nil:
		s.log.Error("store passkey", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.audit(r.Context(), u.ID, "passkey.register", "user:"+u.ID, outcomeOK, map[string]any{"passkey": row.ID, "name": row.Name})
	writeAPIJSON(w, http.StatusCreated, map[string]string{"id": row.ID})
}

// webauthnReason is a short, secret-free reason for a failed ceremony.
func webauthnReason(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.Details != "" {
		d := pe.Details
		if len(d) > 200 {
			d = d[:200]
		}
		return d
	}
	return "verification failed"
}

// loginAllowed applies the sign-in rate limit of /ui/login.
func (s *server) loginAllowed(w http.ResponseWriter, r *http.Request) bool {
	ip := remoteIP(r.RemoteAddr)
	if s.ui.logins.allow(ip) {
		return true
	}
	s.auditDenied(r.Context(), "ui", "passkey.login", "ui", map[string]any{"reason": "rate limited", "remote": ip})
	writeAPIError(w, http.StatusTooManyRequests, "too many sign-in attempts; wait a minute")
	return false
}

func (s *server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.passkeysAvailable(w) || !s.loginAllowed(w, r) {
		return
	}
	var body struct {
		User string `json:"user"`
	}
	if !readAPIJSON(w, r, &body, maxPasskeyBody) {
		return
	}
	// The user hint is accepted but not used: discoverable credentials name
	// the user, and answering differently for unknown names would reveal
	// which names exist.
	assertion, data, err := s.passkeys.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		s.log.Error("passkey login begin", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	cid := randomToken()
	if !s.passkeys.put("login:"+cid, &ceremony{data: *data}) {
		writeAPIError(w, http.StatusServiceUnavailable, "too many sign-in ceremonies in progress; try again")
		return
	}
	setCookie(w, passkeyCeremonyCookie, cid, passkeyCookiePath, passkeyChallengeTTL)
	writeAPIJSON(w, http.StatusOK, assertion)
}

func (s *server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.passkeysAvailable(w) || !s.loginAllowed(w, r) {
		return
	}
	b, ok := readBody(w, r, maxPasskeyBody)
	if !ok {
		return
	}
	ip := remoteIP(r.RemoteAddr)
	var c *ceremony
	if ck, err := r.Cookie(passkeyCeremonyCookie); err == nil && ck.Value != "" {
		c, ok = s.passkeys.take("login:" + ck.Value)
	} else {
		ok = false
	}
	setCookie(w, passkeyCeremonyCookie, "", passkeyCookiePath, 0)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "no passkey sign-in in progress (it lasts two minutes)")
		return
	}
	ctx := r.Context()
	fail := func(actor, reason string) {
		s.audit(ctx, actor, "passkey.login", "ui", outcomeDenied, map[string]any{"reason": reason, "remote": ip})
		writeAPIError(w, http.StatusUnauthorized, "passkey sign-in failed")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(b))
	if err != nil {
		fail("ui", "invalid assertion")
		return
	}
	var row *passkeyRow
	var user *userRow
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		p, err := s.st.getPasskey(ctx, credentialID(rawID))
		if err != nil {
			return nil, errors.New("unknown credential")
		}
		if p.UserID != string(userHandle) {
			return nil, errors.New("user handle does not match the credential")
		}
		u, err := s.st.getUser(ctx, p.UserID)
		if err != nil {
			return nil, errors.New("unknown user")
		}
		wu, _, err := s.loadWAUser(ctx, u)
		if err != nil {
			return nil, err
		}
		row, user = p, u
		return wu, nil
	}
	_, cred, err := s.passkeys.wa.ValidatePasskeyLogin(handler, c.data, parsed)
	actor := "ui"
	if user != nil {
		actor = user.ID
	}
	if err != nil {
		fail(actor, webauthnReason(err))
		return
	}
	if cred.Authenticator.CloneWarning {
		fail(actor, "signature counter did not increase (possible cloned authenticator)")
		return
	}
	enc, err := json.Marshal(cred)
	if err == nil {
		err = s.st.usePasskey(ctx, row.ID, string(enc))
	}
	if err != nil {
		s.log.Error("update passkey", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	sess := s.startSession(w, user)
	s.audit(ctx, user.ID, "passkey.login", "ui", outcomeOK, map[string]any{"passkey": row.ID, "remote": ip})
	writeAPIJSON(w, http.StatusOK, map[string]string{"user_id": user.ID, "csrf": sess.csrf})
}

func (s *server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 1024 {
		writeAPIError(w, http.StatusBadRequest, "invalid passkey id")
		return
	}
	switch err := s.st.deletePasskey(r.Context(), u.ID, id); {
	case errors.Is(err, errNotFound):
		writeAPIError(w, http.StatusNotFound, "passkey not found")
		return
	case errors.Is(err, errLastCredential):
		writeAPIError(w, http.StatusConflict, "this is your last passkey and you have no API key; create an API key first")
		return
	case err != nil:
		s.log.Error("delete passkey", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.audit(r.Context(), u.ID, "passkey.delete", "user:"+u.ID, outcomeOK, map[string]any{"passkey": id})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
