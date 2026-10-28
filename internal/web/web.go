// Package web embeds the built single-page app and serves it.
//
// The UI lives in web/ (React, TypeScript, Vite). `make web` builds it into
// internal/web/dist, which is embedded here and served by the hub (admin
// UI, over its HTTPS listener) and by every node daemon (node UI, on the
// loopback local address). A tree without a build still compiles: the
// handler then answers every page with a plain "UI not built" notice.
//
// Contract (phase 6):
//   - GET /assets/<hashed file>: the file, Cache-Control immutable (one year).
//   - GET /manifest.webmanifest, /sw.js, /icons/*, /favicon.*: served as files,
//     Cache-Control no-cache (so service-worker updates are picked up).
//   - Every other GET path: index.html with Cache-Control no-store
//     (history-API fallback for client-side routes). Non-GET: 405.
//   - Every response carries the headers set by Headers. The content
//     security policy allows only same-origin scripts, styles, fonts,
//     images (plus data: and blob: for canvases and generated icons),
//     workers and connections; no inline scripts; frame-ancestors none.
//   - Paths are cleaned; "..", absolute and hidden (dot) paths yield 404.
//   - The 3D scene must be a separate chunk that index.html does not load
//     eagerly (P6 acceptance: initial load under 1 MB without it).
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Built reports whether a real build is embedded: dist/index.html exists.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Headers sets the security headers every UI response carries.
func Headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data: blob:; connect-src 'self'; worker-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// Handler serves the embedded app as described in the package comment.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded; Sub fails only on a bad path
	}
	files := http.FS(sub)
	fileServer := http.FileServer(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Headers(w)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if hidden(p) {
			http.NotFound(w, r)
			return
		}
		if !Built() {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Mycelium UI not built: run `make web` and rebuild the binary", http.StatusNotFound)
			return
		}
		if p != "/" && p != "/index.html" {
			if f, err := sub.Open(strings.TrimPrefix(p, "/")); err == nil {
				st, serr := f.Stat()
				_ = f.Close()
				if serr == nil && !st.IsDir() {
					if strings.HasPrefix(p, "/assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "no-cache")
					}
					r2 := r.Clone(r.Context())
					r2.URL.Path = p
					fileServer.ServeHTTP(w, r2)
					return
				}
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

// hidden reports paths with a dot-prefixed segment ("/.git", "/assets/.x").
func hidden(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
