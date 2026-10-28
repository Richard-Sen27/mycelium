package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-sen27/mycelium/internal/web"
)

func TestUIConfigAndApp(t *testing.T) {
	env := startLocal(t, nil)
	code, body := env.c.doWith("", "GET", "/api/ui-config", "")
	var cfg map[string]string
	if code != http.StatusOK || json.Unmarshal(body, &cfg) != nil {
		t.Fatalf("ui-config: %d %s", code, body)
	}
	if cfg["mode"] != "node" || cfg["node"] != "n1" || cfg["hub"] != env.h.http.URL || cfg["version"] == "" {
		t.Fatalf("ui-config: %v", cfg)
	}
	if code, body := env.c.do("GET", "/api/nope", ""); code != http.StatusNotFound || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("unknown api path: %d %s", code, body)
	}
	resp, err := http.Get("http://" + env.r.addr + "/channels/general")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("app headers: %v", resp.Header)
	}
	if want := map[bool]int{true: http.StatusOK, false: http.StatusNotFound}[web.Built()]; resp.StatusCode != want {
		t.Fatalf("app route: %d", resp.StatusCode)
	}
}

type proxySeen struct {
	path, scope, auth, cookie, agent, ctype, body string
	cert                                          bool
}

func TestHubProxy(t *testing.T) {
	setVarDaemon(t, &proxyIdle, 500*time.Millisecond)
	env := startLocal(t, nil)
	var (
		mu      sync.Mutex
		seen    []proxySeen
		release = make(chan struct{})
	)
	env.h.connect.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, proxySeen{
			path: r.URL.Path, scope: r.Header.Get("Mycel-Scope"), auth: r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"), agent: r.Header.Get("Mycel-Agent"), ctype: r.Header.Get("Content-Type"),
			body: string(b), cert: r.TLS != nil && len(r.TLS.PeerCertificates) == 1,
		})
		mu.Unlock()
		switch r.URL.Path {
		case "/mycel.v1.NodeService/ListNodes":
			w.Header().Set("Set-Cookie", "hub=1")
			w.Header().Set("X-Internal", "1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"nodes":[]}`))
		case "/mycel.v1.EventService/WatchEvents":
			w.Header().Set("Content-Type", "application/connect+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("chunk1\n"))
			w.(http.Flusher).Flush()
			select {
			case <-release:
				_, _ = w.Write([]byte("chunk2\n"))
			case <-r.Context().Done():
			}
		case "/mycel.v1.EventService/Search":
			// Silent after the headers: the proxy's idle timeout ends it.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	})))
	hubPost := func(token, path, body string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://"+env.r.addr+"/api/hub"+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	read := func(resp *http.Response) string {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b)
	}

	// The local token is required, and only read-only methods pass.
	if resp := hubPost("", "/mycel.v1.NodeService/ListNodes", "{}", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without token: %d", resp.StatusCode)
	}
	for _, p := range []string{"/mycel.v1.NodeService/RemoveNode", "/mycel.v1.GrantService/ApproveGrant", "/api/artifacts/art_1", "/mycel.v1.EnrolService/Renew", "/ui/login"} {
		if resp := hubPost(env.c.token, p, "{}", nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, read(resp))
		}
	}
	if code, _ := env.c.do("GET", "/api/hub/mycel.v1.NodeService/ListNodes", ""); code != http.StatusForbidden {
		t.Fatalf("GET of a procedure: %d", code)
	}
	mu.Lock()
	if len(seen) != 0 {
		t.Fatalf("refused requests reached the hub: %v", seen)
	}
	mu.Unlock()

	// A read: the scope header and the node certificate go out, the
	// browser's credentials do not; hub cookies do not come back.
	resp := hubPost(env.c.token, "/mycel.v1.NodeService/ListNodes", "{}", map[string]string{
		"Content-Type": "application/json", "Connect-Protocol-Version": "1", "Cookie": "mycel_session=s3cret",
		"Mycel-Agent": "agent:x@n1", "Mycel-Scope": "network:write",
	})
	if body := read(resp); resp.StatusCode != http.StatusOK || body != `{"nodes":[]}` {
		t.Fatalf("ListNodes: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Internal") != "" || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response headers: %v", resp.Header)
	}
	mu.Lock()
	got := seen[0]
	mu.Unlock()
	if got.scope != "network:read" || got.auth != "" || got.cookie != "" || got.agent != "" || !got.cert ||
		got.ctype != "application/json" || got.body != "{}" {
		t.Fatalf("forwarded request: %+v", got)
	}

	// Streams are forwarded chunk by chunk.
	resp = hubPost(env.c.token, "/mycel.v1.EventService/WatchEvents", "x", map[string]string{"Content-Type": "application/connect+json"})
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "chunk1\n" {
		t.Fatalf("first chunk: %q %v", line, err)
	}
	close(release)
	if line, err = br.ReadString('\n'); err != nil || line != "chunk2\n" {
		t.Fatalf("second chunk: %q %v", line, err)
	}
	_ = resp.Body.Close()

	// A silent stream ends after the idle timeout.
	start := time.Now()
	resp = hubPost(env.c.token, "/mycel.v1.EventService/Search", "{}", nil)
	_ = read(resp)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("idle stream lasted %v", d)
	}

	// Request bodies are bounded.
	if resp := hubPost(env.c.token, "/mycel.v1.NodeService/ListNodes", strings.Repeat("x", proxyMaxRequest+10), nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body: %d %s", resp.StatusCode, read(resp))
	}

	// The hub is gone: 502.
	env.h.http.Close()
	resp = hubPost(env.c.token, "/mycel.v1.NodeService/ListNodes", "{}", nil)
	if body := read(resp); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "hub unreachable") {
		t.Fatalf("hub down: %d %s", resp.StatusCode, body)
	}
}

func TestProxyAllowed(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		ok           bool
	}{
		{"POST", "/mycel.v1.EventService/WatchEvents", true},
		{"POST", "/mycel.v1.ChannelService/Post", false},
		{"GET", "/api/artifacts/art_0123456789ABCDEFGHJKMNPQRS", true},
		{"HEAD", "/api/artifacts/art_x", true},
		{"GET", "/api/artifacts/art_x/extra", false},
		{"PUT", "/api/artifacts", false},
		{"DELETE", "/mycel.v1.NodeService/ListNodes", false},
		{"GET", "/api/auth/me", false},
	} {
		if got := proxyAllowed(tc.method, tc.path); got != tc.ok {
			t.Errorf("%s %s: %v", tc.method, tc.path, got)
		}
	}
}

func setVarDaemon[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}
