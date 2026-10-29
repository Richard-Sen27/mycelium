package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
)

// fakeEngine imitates the parts of the Docker Engine API the broker uses.
type fakeEngine struct {
	srv *httptest.Server

	mu         sync.Mutex
	containers map[string]map[string]string // id → labels
	names      map[string]string            // name → id
	next       int
	reqs       []string // "METHOD path?query"
	lastCreate map[string]any
	paused     []string
	unpaused   []string
	removed    []string
	driver     string
	lastBody   map[string]string // path → last request body
}

// clearContainers drops every container but the foreign one; f.mu held.
func (f *fakeEngine) clearContainers() {
	for id := range f.containers {
		if id != "foreignid" {
			delete(f.containers, id)
		}
	}
}

var verPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{
		containers: map[string]map[string]string{"foreignid": {LabelGrant: "other"}},
		names:      map[string]string{"foreign": "foreignid"},
		lastBody:   map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// transport sends Engine requests (host "docker") to the fake server.
func (f *fakeEngine) transport() http.RoundTripper {
	u, _ := url.Parse(f.srv.URL)
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Host = u.Host
		r.Host = u.Host
		return http.DefaultTransport.RoundTrip(r)
	})
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *fakeEngine) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := verPrefix.ReplaceAllString(r.URL.Path, "")
	f.mu.Lock()
	defer f.mu.Unlock()
	line := r.Method + " " + p
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.reqs = append(f.reqs, line)
	f.lastBody[p] = string(body)
	segs := strings.Split(strings.Trim(p, "/"), "/")
	resolve := func(s string) string {
		if id, ok := f.names[s]; ok {
			return id
		}
		return s
	}
	switch {
	case p == "/_ping" || p == "/version":
		_, _ = io.WriteString(w, "OK")
	case p == "/info":
		_, _ = fmt.Fprintf(w, `{"CgroupDriver":%q}`, f.driver)
	case p == "/images/json":
		_, _ = io.WriteString(w, `[
			{"Id":"sha256:1","RepoTags":["alpine:3.20","alpine:latest"],"RepoDigests":["alpine@sha256:`+strings.Repeat("a", 64)+`"],"Size":1},
			{"Id":"sha256:2","RepoTags":["ghcr.io/org/tool:1"],"RepoDigests":[]},
			{"Id":"sha256:3","RepoTags":["nginx:latest"]},
			{"Id":"sha256:4","RepoTags":["ghcr.io/other/x:1"]}]`)
	case p == "/images/create":
		_, _ = io.WriteString(w, `{"status":"done"}`)
	case segs[0] == "images" && segs[len(segs)-1] == "json":
		_, _ = io.WriteString(w, `{"Id":"sha256:1"}`)
	case p == "/containers/create":
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.lastCreate = m
		labels := map[string]string{}
		if l, ok := m["Labels"].(map[string]any); ok {
			for k, v := range l {
				labels[k], _ = v.(string)
			}
		}
		f.next++
		id := fmt.Sprintf("c%d%s", f.next, strings.Repeat("0", 10))
		f.containers[id] = labels
		if n := r.URL.Query().Get("name"); n != "" {
			f.names[n] = id
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
	case p == "/containers/json":
		var filters map[string][]string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		out := []map[string]any{}
		for id, labels := range f.containers {
			ok := true
			for _, l := range filters["label"] {
				k, v, hasValue := strings.Cut(l, "=")
				if got, present := labels[k]; !present || (hasValue && got != v) {
					ok = false
				}
			}
			if ok {
				out = append(out, map[string]any{"Id": id, "Labels": labels})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case p == "/containers/prune":
		_, _ = io.WriteString(w, `{}`)
	case segs[0] == "containers" && len(segs) == 2 && r.Method == http.MethodDelete:
		id := resolve(segs[1])
		delete(f.containers, id)
		f.removed = append(f.removed, id)
		w.WriteHeader(http.StatusNoContent)
	case segs[0] == "containers" && len(segs) == 3:
		id := resolve(segs[1])
		labels, ok := f.containers[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container"}`)
			return
		}
		switch segs[2] {
		case "json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Labels": labels}})
		case "exec":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"exec1"}`)
		case "pause":
			f.paused = append(f.paused, id)
			w.WriteHeader(http.StatusNoContent)
		case "unpause":
			f.unpaused = append(f.unpaused, id)
			w.WriteHeader(http.StatusNoContent)
		case "logs":
			_, _ = io.WriteString(w, "log line\n")
			if r.URL.Query().Get("follow") == "1" {
				w.(http.Flusher).Flush()
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	case segs[0] == "exec":
		_, _ = io.WriteString(w, `{}`)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

type fakeCeiling struct{ offers, active atomic.Bool }

func (c *fakeCeiling) Offers(kind string) bool { return kind == OfferName && c.offers.Load() }
func (c *fakeCeiling) Active() bool            { return c.active.Load() }

type auditLog struct {
	mu   sync.Mutex
	recs []broker.Record
}

func (a *auditLog) add(r broker.Record) {
	a.mu.Lock()
	a.recs = append(a.recs, r)
	a.mu.Unlock()
}

func (a *auditLog) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.recs)
}

func (a *auditLog) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for audit record %d", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (a *auditLog) last() broker.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recs[len(a.recs)-1]
}

type env struct {
	t       *testing.T
	eng     *fakeEngine
	b       *Broker
	ceiling *fakeCeiling
	audit   *auditLog
	dir     string
	gpus    []string
}

func newEnv(t *testing.T, network string) *env {
	t.Helper()
	// A Windows node serves every grant over loopback TCP (the broker's
	// default there); a unix endpoint with a Windows path is not a URL.
	if network == "unix" && runtime.GOOS == "windows" {
		network = ""
	}
	// Unix socket paths are short on macOS (104 bytes); t.TempDir is too long.
	dir, err := os.MkdirTemp("", "dkb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := &env{t: t, eng: newFakeEngine(t), ceiling: &fakeCeiling{}, audit: &auditLog{}, dir: dir + "/s"}
	e.ceiling.offers.Store(true)
	e.ceiling.active.Store(true)
	b, err := New(Options{
		Engine: e.eng.transport(), SocketDir: e.dir, Network: network,
		Audit: e.audit.add, Ceiling: e.ceiling,
		GPUDevices: func(subject, _ string) []string { return e.gpus },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	e.b = b
	return e
}

// client is a Docker API client bound to one grant endpoint.
type client struct {
	t     *testing.T
	hc    *http.Client
	base  string
	audit *auditLog
}

func (e *env) attach(id string, spec *mycelv1.DockerHostGrant) *client {
	e.t.Helper()
	ep, err := e.b.Attach(context.Background(), broker.Attachment{
		Grant: &mycelv1.Grant{
			Id: id, Subject: "agent:" + id + "@node",
			Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: spec}},
		},
		SandboxID: "sbx_" + id,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	c := newClient(e.t, ep)
	c.audit = e.audit
	return c
}

func newClient(t *testing.T, ep string) *client {
	u, err := url.Parse(ep)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		c.hc = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
		c.base = "http://docker"
	case "tcp":
		c.hc = &http.Client{}
		c.base = "http://" + u.Host + u.Path
	default:
		t.Fatalf("endpoint %q", ep)
	}
	return c
}

func (c *client) do(method, path, body string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	before := 0
	if c.audit != nil {
		before = c.audit.count()
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if c.audit != nil {
		// The client can finish before the handler audits; every request
		// that reaches a grant listener is audited exactly once.
		c.audit.waitCount(c.t, before+1)
	}
	return resp.StatusCode, string(b)
}

func (c *client) create(body string) string {
	c.t.Helper()
	code, out := c.do(http.MethodPost, "/v1.43/containers/create", body)
	if code != http.StatusCreated {
		c.t.Fatalf("create: %d %s", code, out)
	}
	var r struct{ Id string }
	_ = json.Unmarshal([]byte(out), &r)
	return r.Id
}

func TestCreateRewrite(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}, Cpus: 1, MemoryBytes: 1 << 30})
	nc := e.attach("g2", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	gc := e.attach("g3", &mycelv1.DockerHostGrant{Images: []string{"alpine"}, Gpu: true})
	hcOf := func(m map[string]any) map[string]any { return m["HostConfig"].(map[string]any) }

	tests := []struct {
		name   string
		c      *client
		body   string
		status int
		check  func(t *testing.T, m map[string]any)
	}{
		{"plain", c, `{"Image":"alpine:3.20","Cmd":"echo hi","Labels":{"mine":"x","mycel.grant":"forged","mycel.sandbox":"forged"}}`, 201, func(t *testing.T, m map[string]any) {
			l := m["Labels"].(map[string]any)
			if l[LabelGrant] != "g1" || l[LabelSandbox] != "sbx_g1" || l["mine"] != "x" {
				t.Errorf("labels %v", l)
			}
			if fmt.Sprint(m["Cmd"]) != "[echo hi]" || m["Image"] != "alpine:3.20" {
				t.Errorf("config %v", m)
			}
			hc := hcOf(m)
			if hc["NanoCpus"] != float64(1e9) || hc["Memory"] != float64(1<<30) || hc["MemorySwap"] != float64(1<<30) ||
				hc["PidsLimit"] != float64(1024) || hc["CgroupParent"] != "mycel-g1" || hc["NetworkMode"] != "none" ||
				hc["OomKillDisable"] != false || fmt.Sprint(hc["RestartPolicy"]) != "map[MaximumRetryCount:0 Name:no]" {
				t.Errorf("host config %v", hc)
			}
			if fmt.Sprint(hc["LogConfig"]) != "map[Config:map[max-file:3 max-size:10m] Type:local]" {
				t.Errorf("log config %v", hc["LogConfig"])
			}
		}},
		{"lower limits kept", c, `{"Image":"alpine","HostConfig":{"NanoCpus":500000000,"Memory":1048576,"MemorySwap":-1,"PidsLimit":50}}`, 201, func(t *testing.T, m map[string]any) {
			hc := hcOf(m)
			if hc["NanoCpus"] != float64(5e8) || hc["Memory"] != float64(1<<20) || hc["MemorySwap"] != float64(1<<20) || hc["PidsLimit"] != float64(50) {
				t.Errorf("limits %v", hc)
			}
		}},
		{"higher limits capped", c, `{"Image":"alpine","HostConfig":{"NanoCpus":8000000000,"Memory":8589934592,"PidsLimit":-1,"ShmSize":1073741824}}`, 201, func(t *testing.T, m map[string]any) {
			hc := hcOf(m)
			if hc["NanoCpus"] != float64(1e9) || hc["Memory"] != float64(1<<30) || hc["PidsLimit"] != float64(1024) || hc["ShmSize"] != float64(256<<20) {
				t.Errorf("limits %v", hc)
			}
		}},
		{"node defaults", nc, `{"Image":"alpine","HostConfig":{"NanoCpus":8000000000}}`, 201, func(t *testing.T, m map[string]any) {
			hc := hcOf(m)
			if hc["NanoCpus"] != float64(2e9) || hc["Memory"] != float64(4<<30) || hc["CgroupParent"] != "mycel-g2" {
				t.Errorf("limits %v", hc)
			}
		}},
		{"clamped ulimits restart tmpfs", c, `{"Image":"alpine","HostConfig":{
			"Ulimits":[{"Name":"nofile","Soft":1000000,"Hard":1000000},{"Name":"nproc","Soft":10,"Hard":99999},{"Name":"memlock","Soft":-1,"Hard":-1}],
			"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":10},
			"Tmpfs":{"/run":"rw,noexec","/x":"size=64m"},
			"Mounts":[{"Type":"tmpfs","Target":"/tmp"}]}}`, 201, func(t *testing.T, m map[string]any) {
			hc := hcOf(m)
			if fmt.Sprint(hc["Ulimits"]) != "[map[Hard:65536 Name:nofile Soft:65536] map[Hard:1024 Name:nproc Soft:10] map[Hard:0 Name:memlock Soft:0]]" {
				t.Errorf("ulimits %v", hc["Ulimits"])
			}
			if fmt.Sprint(hc["RestartPolicy"]) != "map[MaximumRetryCount:3 Name:on-failure]" {
				t.Errorf("restart %v", hc["RestartPolicy"])
			}
			if fmt.Sprint(hc["Tmpfs"]) != "map[/run:rw,noexec,size=1g /x:size=64m]" {
				t.Errorf("tmpfs %v", hc["Tmpfs"])
			}
			if fmt.Sprint(hc["Mounts"]) != "[map[Target:/tmp TmpfsOptions:map[SizeBytes:1.073741824e+09] Type:tmpfs]]" {
				t.Errorf("mounts %v", hc["Mounts"])
			}
		}},
		{"network none", c, `{"Image":"alpine","HostConfig":{"NetworkMode":"none"},"NetworkingConfig":{"EndpointsConfig":{"none":{}}}}`, 201, nil},
		{"null fields", c, `{"Image":"alpine","HostConfig":{"Binds":null,"CapAdd":null,"Devices":[],"MaskedPaths":null,"Privileged":false,"Runtime":"","PortBindings":{}}}`, 201, nil},
		{"unknown fields dropped", c, `{"Image":"alpine","OnBuild":["x"],"HostConfig":{"CpuRealtimeRuntime":100,"Dns":["1.1.1.1"],"GroupAdd":["0"]}}`, 201, func(t *testing.T, m map[string]any) {
			hc := hcOf(m)
			for _, k := range []string{"CpuRealtimeRuntime", "Dns", "GroupAdd"} {
				if _, ok := hc[k]; ok {
					t.Errorf("%s forwarded", k)
				}
			}
			if _, ok := m["OnBuild"]; ok {
				t.Error("OnBuild forwarded")
			}
		}},
		{"gpu pinned", gc, `{"Image":"alpine","HostConfig":{"DeviceRequests":[{"Driver":"nvidia","Capabilities":[["gpu"]]}]}}`, 201, func(t *testing.T, m map[string]any) {
			dr := hcOf(m)["DeviceRequests"].([]any)[0].(map[string]any)
			if fmt.Sprint(dr["DeviceIDs"]) != "[GPU-1]" || dr["Driver"] != "nvidia" || dr["Count"] != nil {
				t.Errorf("device requests %v", dr)
			}
		}},

		// Case variants and duplicates are read the way the Engine reads them.
		{"lowercase privileged", c, `{"image":"alpine","hostConfig":{"privileged":true}}`, 400, nil},
		{"duplicate host config merges", c, `{"Image":"alpine","HostConfig":{"Privileged":true},"hostconfig":{"Memory":1}}`, 400, nil},
		{"lowercase volumes", c, `{"Image":"alpine","volumes":{"/d":{}}}`, 400, nil},
		{"duplicate image last wins", c, `{"Image":"alpine","IMAGE":"nginx"}`, 403, nil},
		{"lowercase labels overwritten", c, `{"image":"nginx","Image":"alpine","labels":{"mycel.grant":"g2"}}`, 201, func(t *testing.T, m map[string]any) {
			if m["Image"] != "alpine" || m["image"] != nil || m["labels"] != nil {
				t.Errorf("forwarded %v", m)
			}
			if l := m["Labels"].(map[string]any); l[LabelGrant] != "g1" {
				t.Errorf("labels %v", l)
			}
		}},
		{"lowercase network mode", c, `{"Image":"alpine","HostConfig":{"networkmode":"host"}}`, 400, nil},

		{"image not allowed", c, `{"Image":"nginx"}`, 403, nil},
		{"image id", c, `{"Image":"sha256:` + strings.Repeat("a", 64) + `"}`, 403, nil},
		{"no image", c, `{}`, 403, nil},
		{"privileged", c, `{"Image":"alpine","HostConfig":{"Privileged":true}}`, 400, nil},
		{"privileged as string", c, `{"Image":"alpine","HostConfig":{"Privileged":"true"}}`, 400, nil},
		{"cap add", c, `{"Image":"alpine","HostConfig":{"CapAdd":["SYS_ADMIN"]}}`, 400, nil},
		{"host network", c, `{"Image":"alpine","HostConfig":{"NetworkMode":"host"}}`, 400, nil},
		{"bridge network", c, `{"Image":"alpine","HostConfig":{"NetworkMode":"bridge"}}`, 400, nil},
		{"container network", c, `{"Image":"alpine","HostConfig":{"NetworkMode":"container:foreign"}}`, 400, nil},
		{"named network", c, `{"Image":"alpine","NetworkingConfig":{"EndpointsConfig":{"bridge":{}}}}`, 400, nil},
		{"port bindings", c, `{"Image":"alpine","HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}}}`, 400, nil},
		{"publish all", c, `{"Image":"alpine","HostConfig":{"PublishAllPorts":true}}`, 400, nil},
		{"host pid", c, `{"Image":"alpine","HostConfig":{"PidMode":"host"}}`, 400, nil},
		{"container pid", c, `{"Image":"alpine","HostConfig":{"PidMode":"container:foreign"}}`, 400, nil},
		{"host ipc", c, `{"Image":"alpine","HostConfig":{"IpcMode":"host"}}`, 400, nil},
		{"host uts", c, `{"Image":"alpine","HostConfig":{"UTSMode":"host"}}`, 400, nil},
		{"host userns", c, `{"Image":"alpine","HostConfig":{"UsernsMode":"host"}}`, 400, nil},
		{"host cgroupns", c, `{"Image":"alpine","HostConfig":{"CgroupnsMode":"host"}}`, 400, nil},
		{"binds", c, `{"Image":"alpine","HostConfig":{"Binds":["/:/host"]}}`, 400, nil},
		{"bind mount", c, `{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"bind","Source":"/","Target":"/host"}]}}`, 400, nil},
		{"volume mount", c, `{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"volume","Source":"data","Target":"/d"}]}}`, 400, nil},
		{"big tmpfs mount", c, `{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"tmpfs","Target":"/t","TmpfsOptions":{"SizeBytes":2147483648}}]}}`, 400, nil},
		{"big tmpfs", c, `{"Image":"alpine","HostConfig":{"Tmpfs":{"/t":"size=2g"}}}`, 400, nil},
		{"volumes", c, `{"Image":"alpine","Volumes":{"/data":{}}}`, 400, nil},
		{"volumes from", c, `{"Image":"alpine","HostConfig":{"VolumesFrom":["foreign"]}}`, 400, nil},
		{"devices", c, `{"Image":"alpine","HostConfig":{"Devices":[{"PathOnHost":"/dev/sda"}]}}`, 400, nil},
		{"device cgroup rules", c, `{"Image":"alpine","HostConfig":{"DeviceCgroupRules":["a *:* rwm"]}}`, 400, nil},
		{"gpu without grant", c, `{"Image":"alpine","HostConfig":{"DeviceRequests":[{"Driver":"nvidia"}]}}`, 400, nil},
		{"gpu count", gc, `{"Image":"alpine","HostConfig":{"DeviceRequests":[{"Count":-1}]}}`, 400, nil},
		{"gpu other driver", gc, `{"Image":"alpine","HostConfig":{"DeviceRequests":[{"Driver":"amd"}]}}`, 400, nil},
		{"security opt", c, `{"Image":"alpine","HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}`, 400, nil},
		{"sysctls", c, `{"Image":"alpine","HostConfig":{"Sysctls":{"net.ipv4.ip_forward":"1"}}}`, 400, nil},
		{"cgroup parent", c, `{"Image":"alpine","HostConfig":{"CgroupParent":"/"}}`, 400, nil},
		{"runtime", c, `{"Image":"alpine","HostConfig":{"Runtime":"runc-unsafe"}}`, 400, nil},
		{"storage opt", c, `{"Image":"alpine","HostConfig":{"StorageOpt":{"size":"1T"}}}`, 400, nil},
		{"annotations", c, `{"Image":"alpine","HostConfig":{"Annotations":{"a":"b"}}}`, 400, nil},
		{"isolation", c, `{"Image":"alpine","HostConfig":{"Isolation":"process"}}`, 400, nil},
		{"unmasked paths", c, `{"Image":"alpine","HostConfig":{"MaskedPaths":[]}}`, 400, nil},
		{"readonly paths", c, `{"Image":"alpine","HostConfig":{"ReadonlyPaths":[]}}`, 400, nil},
		{"cpu quota", c, `{"Image":"alpine","HostConfig":{"CpuQuota":100000}}`, 400, nil},
		{"links", c, `{"Image":"alpine","HostConfig":{"Links":["foreign:f"]}}`, 400, nil},
		{"oom score", c, `{"Image":"alpine","HostConfig":{"OomScoreAdj":-500}}`, 400, nil},
		{"oom kill disable", c, `{"Image":"alpine","HostConfig":{"OomKillDisable":true}}`, 400, nil},
		{"restart always", c, `{"Image":"alpine","HostConfig":{"RestartPolicy":{"Name":"always"}}}`, 400, nil},
		{"ulimit rtprio", c, `{"Image":"alpine","HostConfig":{"Ulimits":[{"Name":"rtprio","Soft":99,"Hard":99}]}}`, 400, nil},
		{"not json", c, `[`, 400, nil},
		{"bad host config", c, `{"Image":"alpine","HostConfig":"x"}`, 400, nil},
	}
	e.gpus = []string{"GPU-1"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.eng.mu.Lock()
			e.eng.lastCreate = nil
			e.eng.mu.Unlock()
			code, body := tt.c.do(http.MethodPost, "/v1.43/containers/create?name=x"+strings.ReplaceAll(tt.name, " ", "-"), tt.body)
			if code != tt.status {
				t.Fatalf("status %d, want %d: %s", code, tt.status, body)
			}
			e.eng.mu.Lock()
			m := e.eng.lastCreate
			e.eng.clearContainers() // keep under the per-grant container cap
			e.eng.mu.Unlock()
			if tt.status != 201 {
				if m != nil {
					t.Error("refused create reached the engine")
				}
				if rec := e.audit.last(); rec.Allowed || rec.Op != "POST containers.create" {
					t.Errorf("audit %+v", rec)
				}
				return
			}
			if rec := e.audit.last(); !rec.Allowed || !strings.HasPrefix(rec.Target, "docker.io/library/alpine:") ||
				!strings.Contains(rec.Reason, "caps nano_cpus=") {
				t.Errorf("audit %+v", rec)
			}
			if tt.check != nil {
				tt.check(t, m)
			}
		})
	}
	// No lease, no GPU: fail closed.
	e.gpus = nil
	if code, _ := gc.do(http.MethodPost, "/containers/create", `{"Image":"alpine","HostConfig":{"DeviceRequests":[{"Driver":"nvidia"}]}}`); code != 400 {
		t.Errorf("gpu without lease: %d", code)
	}
}

func TestCreateLimits(t *testing.T) {
	e := newEnv(t, "unix")
	e.eng.mu.Lock()
	e.eng.driver = "systemd"
	e.eng.mu.Unlock()
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	for i := 0; i < maxContainers; i++ {
		c.create(`{"Image":"alpine"}`)
	}
	e.eng.mu.Lock()
	parent := e.eng.lastCreate["HostConfig"].(map[string]any)["CgroupParent"]
	e.eng.mu.Unlock()
	if parent != "mycel-g1.slice" {
		t.Errorf("systemd cgroup parent %v", parent)
	}
	if code, body := c.do(http.MethodPost, "/containers/create", `{"Image":"alpine"}`); code != http.StatusConflict {
		t.Errorf("ninth container: %d %s", code, body)
	}
	// Without a GPUDevices hook a gpu grant still cannot request devices.
	b, err := New(Options{Engine: e.eng.transport(), SocketDir: e.dir + "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	ep, err := b.Attach(context.Background(), broker.Attachment{Grant: &mycelv1.Grant{Id: "g9",
		Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{Images: []string{"alpine"}, Gpu: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := newClient(t, ep).do(http.MethodPost, "/containers/create", `{"Image":"alpine","HostConfig":{"DeviceRequests":[{"Driver":"nvidia"}]}}`); code != 400 {
		t.Errorf("gpu without hook: %d", code)
	}
}

func TestPullRateLimit(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	for i := 0; i < pullsPerMinute; i++ {
		if code, _ := c.do(http.MethodPost, "/images/create?fromImage=alpine&tag=3", ""); code != 200 {
			t.Fatalf("pull %d: %d", i, code)
		}
	}
	if code, _ := c.do(http.MethodPost, "/images/create?fromImage=alpine&tag=3", ""); code != 429 {
		t.Errorf("over the pull rate: %d", code)
	}
}

func TestSocketDirChecks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets dir")
	}
	dir, err := os.MkdirTemp("", "dkl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Mkdir(dir+"/real", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"/real", dir+"/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Engine: rtFunc(nil), SocketDir: dir + "/link"}); err == nil {
		t.Error("symlinked socket dir accepted")
	}
	if err := os.WriteFile(dir+"/file", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Engine: rtFunc(nil), SocketDir: dir + "/file"}); err == nil {
		t.Error("file as socket dir accepted")
	}
}

func TestSweep(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	mine := c.create(`{"Image":"alpine"}`)
	e.eng.mu.Lock()
	e.eng.containers["stale1"] = map[string]string{LabelGrant: "gone"}
	e.eng.containers["unlabelled"] = map[string]string{}
	e.eng.mu.Unlock()
	if err := e.b.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.eng.mu.Lock()
	defer e.eng.mu.Unlock()
	if _, ok := e.eng.containers["stale1"]; ok {
		t.Error("orphan left")
	}
	if _, ok := e.eng.containers["foreignid"]; ok {
		t.Error("container of an unattached grant 'other' left")
	}
	for _, keep := range []string{mine, "unlabelled"} {
		if _, ok := e.eng.containers[keep]; !ok {
			t.Errorf("%s removed", keep)
		}
	}
}

func TestLabelScoping(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	other := e.attach("g2", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	id := c.create(`{"Image":"alpine"}`)
	theirs := other.create(`{"Image":"alpine"}`)

	if code, body := c.do(http.MethodGet, "/v1.43/containers/"+id+"/json", ""); code != 200 || !strings.Contains(body, id) {
		t.Errorf("inspect own: %d %s", code, body)
	}
	for _, foreign := range []string{"foreign", "foreignid", theirs, "missing"} {
		if code, _ := c.do(http.MethodGet, "/containers/"+foreign+"/json", ""); code != 404 {
			t.Errorf("inspect %s: %d", foreign, code)
		}
		if code, _ := c.do(http.MethodPost, "/containers/"+foreign+"/stop", ""); code != 404 {
			t.Errorf("stop %s: %d", foreign, code)
		}
		if code, _ := c.do(http.MethodDelete, "/containers/"+foreign+"?force=1", ""); code != 404 {
			t.Errorf("remove %s: %d", foreign, code)
		}
		if code, _ := c.do(http.MethodPost, "/containers/"+foreign+"/exec", `{"Cmd":["sh"]}`); code != 404 {
			t.Errorf("exec %s: %d", foreign, code)
		}
	}
	if rec := e.audit.last(); rec.Allowed || rec.Reason == "" {
		t.Errorf("audit %+v", rec)
	}

	// List: the engine is asked with this grant's label, and caller filters are kept.
	code, body := c.do(http.MethodGet, "/v1.43/containers/json?all=1&filters="+url.QueryEscape(`{"status":{"running":true}}`), "")
	if code != 200 || !strings.Contains(body, id) || strings.Contains(body, theirs) || strings.Contains(body, "foreignid") {
		t.Errorf("list: %d %s", code, body)
	}
	reqs := e.eng.requests()
	last := reqs[len(reqs)-1]
	u, _ := url.Parse(strings.TrimPrefix(last, "GET "))
	var f map[string][]string
	_ = json.Unmarshal([]byte(u.Query().Get("filters")), &f)
	if !slices.Contains(f["label"], "mycel.grant=g1") || !slices.Equal(f["status"], []string{"running"}) {
		t.Errorf("filters %v", f)
	}
	if code, _ := c.do(http.MethodGet, "/containers/json?filters=notjson", ""); code != 400 {
		t.Errorf("bad filters: %d", code)
	}
	if code, _ := c.do(http.MethodPost, "/containers/prune", ""); code != 200 {
		t.Errorf("prune: %d", code)
	}
	reqs = e.eng.requests()
	if last := reqs[len(reqs)-1]; !strings.Contains(last, url.QueryEscape("mycel.grant=g1")) {
		t.Errorf("prune not filtered: %s", last)
	}

	// Lifecycle on our own container; the path carries the resolved id.
	for _, a := range []string{"start", "stop", "kill", "wait", "attach", "resize"} {
		if code, body := c.do(http.MethodPost, "/v1.43/containers/"+id+"/"+a, ""); code >= 300 {
			t.Errorf("%s: %d %s", a, code, body)
		}
	}
	if code, body := c.do(http.MethodGet, "/v1.43/containers/"+id+"/logs?stdout=1", ""); code != 200 || body != "log line\n" {
		t.Errorf("logs: %d %q", code, body)
	}
	for _, a := range []string{"pause", "restart", "update", "rename", "archive", "export", "commit"} {
		if code, _ := c.do(http.MethodPost, "/containers/"+id+"/"+a, ""); code != 403 {
			t.Errorf("%s: %d", a, code)
		}
	}
	if code, _ := c.do(http.MethodGet, "/containers/"+id+"/archive?path=/", ""); code != 403 {
		t.Errorf("archive get: %d", code)
	}

	// Exec: created on our container, then usable by id; others are not.
	if code, _ := c.do(http.MethodPost, "/containers/"+id+"/exec", `{"Cmd":["sh"],"Privileged":true}`); code != 400 {
		t.Errorf("privileged exec: %d", code)
	}
	if code, body := c.do(http.MethodPost, "/v1.43/containers/"+id+"/exec", `{"Cmd":["sh"]}`); code != 201 || !strings.Contains(body, "exec1") {
		t.Fatalf("exec create: %d %s", code, body)
	}
	if code, _ := c.do(http.MethodPost, "/v1.43/exec/exec1/start", `{}`); code != 200 {
		t.Errorf("exec start: %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/exec/exec1/json", ""); code != 200 {
		t.Errorf("exec inspect: %d", code)
	}
	if code, _ := other.do(http.MethodPost, "/exec/exec1/start", `{}`); code != 404 {
		t.Errorf("exec of another grant: %d", code)
	}
	if code, _ := c.do(http.MethodPost, "/exec/exec1/delete", ``); code != 403 {
		t.Errorf("exec unknown action: %d", code)
	}

	if code, _ := c.do(http.MethodDelete, "/v1.43/containers/"+id+"?force=1", ""); code != 204 {
		t.Errorf("remove own: %d", code)
	}
	if rec := e.audit.last(); !rec.Allowed || rec.Op != "DELETE containers.remove" || rec.Target != id {
		t.Errorf("audit %+v", rec)
	}
}

func TestDeniedEndpoints(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	for _, r := range [][2]string{
		{"POST", "/build"}, {"POST", "/v1.43/build"}, {"GET", "/networks"}, {"POST", "/networks/create"},
		{"GET", "/volumes"}, {"POST", "/volumes/create"}, {"POST", "/swarm/init"}, {"GET", "/info"},
		{"GET", "/system/df"}, {"GET", "/events"}, {"GET", "/plugins"}, {"POST", "/plugins/pull"},
		{"POST", "/commit"}, {"POST", "/auth"}, {"GET", "/secrets"}, {"POST", "/session"},
		{"DELETE", "/images/alpine"}, {"POST", "/images/alpine/push"}, {"POST", "/images/alpine/tag"},
		{"POST", "/images/load"}, {"GET", "/images/get"}, {"POST", "/images/prune"}, {"GET", "/images/search"},
		{"POST", "/_ping"}, {"DELETE", "/containers/json"}, {"GET", "/containers/create"},
	} {
		if code, _ := c.do(r[0], r[1], ""); code != 403 {
			t.Errorf("%s %s: %d", r[0], r[1], code)
		}
	}
	for _, p := range []string{"/containers/x/../../build", "/containers//json", "/containers/%2e%2e/json"} {
		if code, _ := c.do("GET", p, ""); code < 400 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := c.do("GET", "/_ping", ""); code != 200 {
		t.Errorf("ping: %d", code)
	}
	if code, _ := c.do("GET", "/v1.43/version", ""); code != 200 {
		t.Errorf("version: %d", code)
	}
	for _, r := range e.eng.requests() {
		if !strings.HasPrefix(r, "GET /_ping") && !strings.HasPrefix(r, "GET /version") {
			t.Errorf("denied request reached the engine: %s", r)
		}
	}
}

func TestImageAllowlist(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine", "ghcr.io/org/", "library/busybox:1.36", "quay.io"}})

	pull := func(q string) int {
		e.b.mu.Lock()
		e.b.grants["g1"].pulls = bucket{} // the pull rate has its own test
		e.b.mu.Unlock()
		code, _ := c.do(http.MethodPost, "/v1.43/images/create?"+q, "")
		return code
	}
	for q, want := range map[string]int{
		"fromImage=alpine&tag=3.20":                              200,
		"fromImage=docker.io/library/alpine":                     200,
		"fromImage=index.docker.io/library/alpine&tag=3":         200,
		"fromImage=ghcr.io/org/tool&tag=1":                       200,
		"fromImage=ghcr.io/org/team/tool":                        200,
		"fromImage=quay.io/x/y&tag=2":                            200,
		"fromImage=busybox&tag=1.36":                             200,
		"fromImage=busybox&tag=latest":                           403,
		"fromImage=nginx":                                        403,
		"fromImage=ghcr.io/orgx/tool":                            403,
		"fromImage=ghcr.io/other/tool":                           403,
		"fromImage=evil.io/alpine":                               403,
		"fromImage=alpine/evil":                                  403,
		"fromSrc=http://x&repo=alpine":                           403,
		"fromImage=Bad%20Name":                                   400,
		"fromImage=alpine&tag=sha256:" + strings.Repeat("b", 64): 200,
	} {
		if got := pull(q); got != want {
			t.Errorf("pull %s: %d, want %d", q, got, want)
		}
	}
	// A pull without a tag is pinned to :latest instead of every tag.
	pull("fromImage=alpine")
	reqs := e.eng.requests()
	if last := reqs[len(reqs)-1]; !strings.Contains(last, "tag=latest") {
		t.Errorf("untagged pull forwarded as %s", last)
	}
	if rec := e.audit.last(); rec.Op != "POST images.pull" || rec.Target != "docker.io/library/alpine:latest" {
		t.Errorf("audit %+v", rec)
	}

	code, body := c.do(http.MethodGet, "/v1.43/images/json", "")
	if code != 200 || !strings.Contains(body, "alpine:3.20") || !strings.Contains(body, "ghcr.io/org/tool:1") ||
		strings.Contains(body, "nginx") || strings.Contains(body, "ghcr.io/other") || !strings.Contains(body, "alpine@sha256") {
		t.Errorf("images list: %d %s", code, body)
	}
	if code, _ := c.do(http.MethodGet, "/images/alpine:3.20/json", ""); code != 200 {
		t.Errorf("inspect allowed: %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/images/ghcr.io/org/tool:1/json", ""); code != 200 {
		t.Errorf("inspect registry path: %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/images/nginx/json", ""); code != 403 {
		t.Errorf("inspect other: %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/images/sha256:"+strings.Repeat("a", 64)+"/json", ""); code != 403 {
		t.Errorf("inspect by id: %d", code)
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":                     "docker.io/library/alpine:latest",
		"alpine:3.20":                "docker.io/library/alpine:3.20",
		"user/app":                   "docker.io/user/app:latest",
		"index.docker.io/user/app:1": "docker.io/user/app:1",
		"localhost:5000/app":         "localhost:5000/app:latest",
		"localhost/app:2":            "localhost/app:2",
		"ghcr.io/org/team/app:v1.2":  "ghcr.io/org/team/app:v1.2",
		"alpine@sha256:" + strings.Repeat("c", 64): "docker.io/library/alpine@sha256:" + strings.Repeat("c", 64),
	} {
		ref, _, ok := parseRef(in, true)
		if !ok || ref.String() != want {
			t.Errorf("parseRef(%q) = %q %v, want %q", in, ref.String(), ok, want)
		}
	}
	for _, bad := range []string{"", "x/UPPER", "a b", "alpine:", "alpine@sha256:zz", strings.Repeat("a", 64), "sha256:" + strings.Repeat("a", 64), "x//y"} {
		if _, _, ok := parseRef(bad, true); ok {
			t.Errorf("parseRef(%q) accepted", bad)
		}
	}
	if r, ok := parseImageRule("ghcr.io"); !ok || r.prefix != "ghcr.io/" {
		t.Errorf("bare registry %+v", r)
	}
	if r, ok := parseImageRule("myorg/"); !ok || r.prefix != "docker.io/myorg/" {
		t.Errorf("hub namespace %+v", r)
	}
	if r, ok := parseImageRule("alpine:3"); !ok || r.repo != "docker.io/library/alpine" || r.tag != "3" {
		t.Errorf("tagged %+v", r)
	}
}

func TestDetachCleanup(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	other := e.attach("g2", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	a := c.create(`{"Image":"alpine"}`)
	b := c.create(`{"Image":"alpine"}`)
	theirs := other.create(`{"Image":"alpine"}`)
	sock := e.dir + "/g1.sock"
	if runtime.GOOS != "windows" {
		st, err := os.Stat(sock)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("socket mode %v %v", st.Mode(), err)
		}
		if st, _ := os.Stat(e.dir); st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", st.Mode())
		}
	}

	if err := e.b.Detach(context.Background(), "g1", "expired"); err != nil {
		t.Fatal(err)
	}
	e.eng.mu.Lock()
	removed := slices.Clone(e.eng.removed)
	_, theirsLeft := e.eng.containers[theirs]
	e.eng.mu.Unlock()
	slices.Sort(removed)
	want := []string{a, b}
	slices.Sort(want)
	if !slices.Equal(removed, want) || !theirsLeft {
		t.Errorf("removed %v, want %v (theirs left %v)", removed, want, theirsLeft)
	}
	for _, r := range e.eng.requests() {
		if strings.HasPrefix(r, "DELETE") && !strings.Contains(r, "force=1") {
			t.Errorf("remove without force: %s", r)
		}
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if code, _ := c.do(http.MethodGet, "/_ping", ""); code != 0 {
		t.Errorf("detached grant still answers: %d", code)
	}
	if code, _ := other.do(http.MethodGet, "/_ping", ""); code != 200 {
		t.Errorf("other grant: %d", code)
	}
	// Usage: two creates on g1 (reported after detach), one on g2.
	got := map[string]int64{}
	for _, u := range e.b.Usage() {
		got[u.GrantID] = u.Requests
	}
	if got["g1"] != 2 || got["g2"] != 2 {
		t.Errorf("usage %v", got)
	}
}

func TestFreeze(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	id := c.create(`{"Image":"alpine"}`)
	e.b.Freeze("network frozen")
	code, body := c.do(http.MethodGet, "/containers/json", "")
	if code != 503 || !strings.Contains(body, "frozen") {
		t.Errorf("frozen: %d %s", code, body)
	}
	e.eng.mu.Lock()
	paused := slices.Clone(e.eng.paused)
	e.eng.mu.Unlock()
	if !slices.Equal(paused, []string{id}) {
		t.Errorf("paused %v", paused)
	}
	e.b.Unfreeze()
	e.eng.mu.Lock()
	unpaused := slices.Clone(e.eng.unpaused)
	e.eng.mu.Unlock()
	if !slices.Equal(unpaused, []string{id}) {
		t.Errorf("unpaused %v", unpaused)
	}
	if code, _ := c.do(http.MethodGet, "/containers/json", ""); code != 200 {
		t.Errorf("unfrozen: %d", code)
	}
}

func TestCeiling(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	e.ceiling.active.Store(false)
	if code, body := c.do(http.MethodGet, "/_ping", ""); code != 503 || !strings.Contains(body, "not active") {
		t.Errorf("inactive: %d %s", code, body)
	}
	e.ceiling.active.Store(true)
	e.ceiling.offers.Store(false)
	if code, body := c.do(http.MethodGet, "/_ping", ""); code != 503 || !strings.Contains(body, "switched off") {
		t.Errorf("not offered: %d %s", code, body)
	}
	_, err := e.b.Attach(context.Background(), broker.Attachment{Grant: &mycelv1.Grant{
		Id: "g2", Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{}}},
	}})
	if !errors.Is(err, broker.ErrCeiling) {
		t.Errorf("attach: %v", err)
	}
}

func TestTCPPrefix(t *testing.T) {
	e := newEnv(t, "tcp")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	if !strings.HasPrefix(c.base, "http://127.0.0.1:") {
		t.Fatalf("base %s", c.base)
	}
	if code, _ := c.do(http.MethodGet, "/v1.43/containers/json", ""); code != 200 {
		t.Errorf("with prefix: %d", code)
	}
	u, _ := url.Parse(c.base)
	bare := &client{t: t, hc: &http.Client{}, base: "http://" + u.Host}
	if code, _ := bare.do(http.MethodGet, "/v1.43/containers/json", ""); code != 403 {
		t.Errorf("without prefix: %d", code)
	}
	wrong := &client{t: t, hc: &http.Client{}, base: "http://" + u.Host + "/" + strings.Repeat("A", len(u.Path)-1)}
	if code, _ := wrong.do(http.MethodGet, "/_ping", ""); code != 403 {
		t.Errorf("wrong prefix: %d", code)
	}
	// Re-attaching keeps the endpoint.
	again := e.attach("g1", &mycelv1.DockerHostGrant{})
	if again.base != c.base {
		t.Errorf("endpoint changed: %s vs %s", again.base, c.base)
	}
	if code, _ := c.do(http.MethodPost, "/images/create?fromImage=alpine", ""); code != 403 {
		t.Errorf("replaced terms not applied: %d", code)
	}
}

func TestAttachValidation(t *testing.T) {
	e := newEnv(t, "unix")
	for _, g := range []*mycelv1.Grant{
		nil,
		{Id: "x"},
		{Id: "../x", Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{}}}},
	} {
		if _, err := e.b.Attach(context.Background(), broker.Attachment{Grant: g}); err == nil {
			t.Errorf("accepted %v", g)
		}
	}
	if ok, why := e.b.Available(context.Background()); !ok || e.b.Kind() != broker.KindDockerHost {
		t.Errorf("available %v %s", ok, why)
	}
}

func TestAddLabelFilter(t *testing.T) {
	got, err := addLabelFilter(`{"label":{"a=b":true,"c":false},"name":["x"]}`, "mycel.grant=g")
	if err != nil {
		t.Fatal(err)
	}
	var f map[string][]string
	_ = json.Unmarshal([]byte(got), &f)
	if !slices.Equal(f["label"], []string{"a=b", "mycel.grant=g"}) || !slices.Equal(f["name"], []string{"x"}) {
		t.Errorf("%s", got)
	}
	if _, err := addLabelFilter(`{"label":"x"}`, "l"); err == nil {
		t.Error("malformed accepted")
	}
}
