package daemon

import (
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
)

// Status is the JSON body of GET /api/status.
type Status struct {
	Node            string          `json:"node"`
	HubURL          string          `json:"hub_url"`
	State           string          `json:"state"`
	Connected       bool            `json:"connected"`
	LastHeartbeatAt *string         `json:"last_heartbeat_at"`
	CertNotAfter    string          `json:"cert_not_after"`
	Caps            []string        `json:"caps"`
	Inventory       json.RawMessage `json:"inventory"`
	LocalAgents     int             `json:"local_agents"`
	Workers         int             `json:"workers"` // running or paused local workers
}

func (n *node) status() Status {
	agents, workers := 0, 0
	if n.local != nil {
		agents = n.local.count()
		workers = n.local.runningWorkers()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	s := Status{
		Node:         n.st.Name,
		HubURL:       n.st.HubURL,
		State:        n.state,
		Connected:    n.connected,
		CertNotAfter: n.leaf.NotAfter.UTC().Format(time.RFC3339),
		Caps:         append([]string{}, n.st.Caps...),
		Inventory:    json.RawMessage("null"),
		LocalAgents:  agents,
		Workers:      workers,
	}
	if n.expired && n.state != StateRevoked {
		s.State = StateExpired
	}
	if !n.lastBeat.IsZero() {
		t := n.lastBeat.UTC().Format(time.RFC3339Nano)
		s.LastHeartbeatAt = &t
	}
	if n.inv != nil {
		if b, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(n.inv); err == nil {
			s.Inventory = b
		}
	}
	return s
}

func (n *node) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(n.status())
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		s := n.status()
		inv := ""
		if string(s.Inventory) != "null" {
			inv = string(s.Inventory)
		}
		last := "never"
		if s.LastHeartbeatAt != nil {
			last = *s.LastHeartbeatAt
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = statusPage.Execute(w, map[string]any{
			"S":    s,
			"Caps": strings.Join(s.Caps, ", "),
			"Last": last,
			"Inv":  inv,
		})
	})
	if n.local != nil {
		n.local.register(mux)
	}
	return securityHeaders(mux)
}

// securityHeaders refuses requests whose Host is not a loopback name (DNS
// rebinding) and sets conservative headers on every local response.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		h.ServeHTTP(w, r)
	})
}

var statusPage = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>mycel node {{.S.Node}}</title>
<style>body{font-family:ui-monospace,monospace;background:#0b0d10;color:#d8dde3;margin:2rem}th{text-align:left;padding-right:1.5rem;color:#8a939c;font-weight:normal}pre{white-space:pre-wrap}</style>
</head><body>
<h1>node {{.S.Node}}</h1>
<table>
<tr><th>hub</th><td>{{.S.HubURL}}</td></tr>
<tr><th>state</th><td>{{.S.State}}</td></tr>
<tr><th>connected</th><td>{{.S.Connected}}</td></tr>
<tr><th>last heartbeat</th><td>{{.Last}}</td></tr>
<tr><th>certificate valid until</th><td>{{.S.CertNotAfter}}</td></tr>
<tr><th>caps</th><td>{{.Caps}}</td></tr>
</table>
<h2>inventory</h2>
<pre>{{if .Inv}}{{.Inv}}{{else}}not probed yet{{end}}</pre>
</body></html>
`))

// loopbackHost reports whether a Host header names this machine's loopback.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isWindows() bool { return runtime.GOOS == "windows" }
