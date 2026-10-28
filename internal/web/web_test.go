package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerWithoutBuild(t *testing.T) {
	if Built() {
		t.Skip("a real build is embedded")
	}
	h := Handler()
	for _, p := range []string{"/", "/approvals", "/assets/x.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not built") {
			t.Fatalf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: csp %q", p, csp)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.keep", nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "not built") {
		t.Fatalf("hidden path: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHidden(t *testing.T) {
	for p, want := range map[string]bool{"/": false, "/assets/a.js": false, "/.git/config": true, "/a/.b": true} {
		if got := hidden(p); got != want {
			t.Errorf("hidden(%q) = %v", p, got)
		}
	}
}
