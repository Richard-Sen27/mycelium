package hub

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/richard-sen27/mycelium/internal/version"
	"github.com/richard-sen27/mycelium/internal/web"
)

// uiSettings are the browser-facing addresses (hub.yaml ui section with
// defaults).
type uiSettings struct {
	externalURL string
	rpID        string
	origins     []string
}

// resolveUISettings applies the defaults: the external URL is ui.external_url,
// else advertise_url, else the API listen address; the RP id is its host and
// the origins its origin.
func resolveUISettings(cfg *Config, apiAddr string) uiSettings {
	ext := strings.TrimRight(cfg.UI.ExternalURL, "/")
	if ext == "" {
		ext = strings.TrimRight(cfg.AdvertiseURL, "/")
	}
	if ext == "" {
		ext = "https://" + localAddr(apiAddr)
	}
	out := uiSettings{externalURL: ext, rpID: cfg.UI.RPID}
	u, err := url.Parse(ext)
	if err == nil && u.Host != "" {
		if out.rpID == "" {
			out.rpID = u.Hostname()
		}
		out.origins = []string{u.Scheme + "://" + u.Host}
	}
	if len(cfg.UI.Origins) > 0 {
		out.origins = nil
		for _, o := range cfg.UI.Origins {
			out.origins = append(out.origins, strings.TrimRight(o, "/"))
		}
	}
	return out
}

// uiAPIRoutes registers the phase 6 JSON routes and the single-page app.
func (s *server) uiAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ui-config", s.handleUIConfig)
	mux.HandleFunc("GET /api/auth/me", s.handleAuthMe)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /api/auth/logout-all", s.handleAuthLogoutAll)
	mux.HandleFunc("POST /api/auth/passkey/register/begin", s.handlePasskeyRegisterBegin)
	mux.HandleFunc("POST /api/auth/passkey/register/finish", s.handlePasskeyRegisterFinish)
	mux.HandleFunc("POST /api/auth/passkey/login/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("POST /api/auth/passkey/login/finish", s.handlePasskeyLoginFinish)
	mux.HandleFunc("DELETE /api/auth/passkey/{id}", s.handlePasskeyDelete)
	mux.HandleFunc("GET /api/push/vapid", s.handlePushVAPID)
	mux.HandleFunc("POST /api/push/subscriptions", s.handlePushSubscribe)
	mux.HandleFunc("DELETE /api/push/subscriptions", s.handlePushUnsubscribe)
	// Unknown API paths answer JSON, never the app.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not found")
	})
	// The app: "/" and every path not routed above (history fallback).
	// Without a build, "/" keeps the plain hub page.
	if !web.Built() {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			title := html.EscapeString("Mycelium hub " + string(s.td))
			_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body><h1>%s</h1></body></html>\n", title, title)
		})
	}
	mux.Handle("/", web.Handler())
}

func (s *server) handleUIConfig(w http.ResponseWriter, _ *http.Request) {
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"mode": "hub", "hub": s.uiCfg.externalURL, "rp_id": s.uiCfg.rpID,
		"passkeys": s.passkeys != nil, "push": true, "version": version.Version,
	})
}

// noStore marks API and Connect responses as not cacheable.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/mycel.v1.") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}
