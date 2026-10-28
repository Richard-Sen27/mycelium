package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/version"
	"github.com/richard-sen27/mycelium/internal/web"
)

const (
	uiCodeTTL      = 60 * time.Second
	maxUICodes     = 8
	maxUISessionIn = 4 << 10
)

// uiRoutes adds the phase 6 node UI to the local mux (localapi.go): the
// public UI config, the sign-in code exchange, the hub proxy for the network
// view, a JSON 404 for unknown API paths and the single-page app for
// everything else.
func (n *node) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ui-config", n.handleUIConfig)
	if l := n.local; l != nil {
		mux.Handle("POST /api/local/ui-code", l.logRequests(l.requireToken(http.HandlerFunc(l.handleUICode))))
		mux.Handle("POST /api/local/ui-session", l.logRequests(http.HandlerFunc(l.handleUISession)))
		mux.Handle("/api/hub/", l.logRequests(l.requireToken(http.HandlerFunc(l.handleHubProxy))))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/", web.Handler())
}

func (n *node) handleUIConfig(w http.ResponseWriter, _ *http.Request) {
	n.mu.Lock()
	name, hub := n.st.Name, n.st.HubURL
	n.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"mode": "node", "node": name, "hub": hub, "version": version.Version})
}

// serveApp answers "/" with the app once it is built; reports false when
// the plain status page should be served instead.
func serveApp(w http.ResponseWriter, r *http.Request) bool {
	if !web.Built() {
		return false
	}
	web.Handler().ServeHTTP(w, r)
	return true
}

// uiCodeStore holds the single-use sign-in codes `mycel ui` puts in the URL
// instead of the local token.
type uiCodeStore struct {
	mu    sync.Mutex
	codes map[string]time.Time // code -> expiry
	now   func() time.Time
}

var errTooManyCodes = errors.New("too many outstanding sign-in codes")

func (c *uiCodeStore) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *uiCodeStore) prune(now time.Time) {
	for k, exp := range c.codes {
		if !now.Before(exp) {
			delete(c.codes, k)
		}
	}
}

// issue makes a new code (at most maxUICodes outstanding).
func (c *uiCodeStore) issue() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.codes == nil {
		c.codes = map[string]time.Time{}
	}
	c.prune(now)
	if len(c.codes) >= maxUICodes {
		return "", errTooManyCodes
	}
	code, err := randomB64(32)
	if err != nil {
		return "", err
	}
	c.codes[code] = now.Add(uiCodeTTL)
	return code, nil
}

// redeem uses a code once; it reports false for unknown, used or expired
// codes. Codes are compared in constant time.
func (c *uiCodeStore) redeem(code string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(c.clock())
	for k := range c.codes {
		if subtle.ConstantTimeCompare([]byte(k), []byte(code)) == 1 {
			delete(c.codes, k)
			return true
		}
	}
	return false
}

func (l *localAPI) handleUICode(w http.ResponseWriter, _ *http.Request) {
	code, err := l.uiCodes.issue()
	if errors.Is(err, errTooManyCodes) {
		writeJSONError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_in": int(uiCodeTTL / time.Second)})
}

// handleUISession exchanges a code for the local token (no auth: the code
// is the credential).
func (l *localAPI) handleUISession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUISessionIn))
	if err != nil || json.Unmarshal(b, &body) != nil || body.Code == "" {
		writeJSONError(w, http.StatusBadRequest, `want {"code":"..."}`)
		return
	}
	if !l.uiCodes.redeem(body.Code) {
		writeJSONError(w, http.StatusNotFound, "unknown, used or expired code; run `mycel ui` again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": l.token})
}
