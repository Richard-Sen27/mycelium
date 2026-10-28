package docker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/broker"
)

// OfferName is the sharing-settings offer that gates this broker.
const OfferName = "docker"

const (
	engineHost     = "docker" // placeholder host in URLs sent to the Engine transport
	headerTimeout  = 10 * time.Second
	maxBodyBytes   = 1 << 20
	maxListBytes   = 16 << 20
	engineTimeout  = 30 * time.Second
	prefixBytes    = 24
	freezeDeadline = 5 * time.Second
	pullsPerMinute = 5
)

var grantIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Options configures the broker.
type Options struct {
	// Engine reaches the host Docker Engine API; requests use the URL host
	// "docker". Default: DOCKER_HOST (unix:// or tcp://) or the standard
	// socket (/var/run/docker.sock, then ~/.docker/run/docker.sock).
	Engine http.RoundTripper
	// SocketDir holds the per-grant unix sockets (0700 directory, 0600
	// sockets). Default: <tmp>/mycel-docker-<uid>.
	SocketDir string
	// Network is "unix" (default) or "tcp" (default on Windows): one
	// loopback listener per grant whose URL path prefix is the credential.
	Network string
	// GPUDevices, when set, returns the GPU device ids leased to a subject
	// (from the gpu broker); container DeviceRequests are pinned to them.
	GPUDevices func(subject, sandboxID string) []string
	Audit      broker.Audit
	Ceiling    broker.Ceiling
	Logger     *slog.Logger
}

// Broker is the brokered host Docker. It implements broker.Broker.
type Broker struct {
	engine     http.RoundTripper
	dir        string
	network    string
	gpuDevices func(subject, sandboxID string) []string
	audit      broker.Audit
	ceiling    broker.Ceiling
	log        *slog.Logger
	now        func() time.Time
	proxy      *httputil.ReverseProxy

	mu      sync.Mutex
	closed  bool
	frozen  bool
	reason  string
	paused  map[string]string // containers paused by Freeze → grant id
	grants  map[string]*grant
	pending map[string]*broker.Usage
	driver  string // the Engine's cgroup driver, once known
}

var _ broker.Broker = (*Broker)(nil)

// grant is one attached grant with its own listener.
type grant struct {
	id        string
	ln        net.Listener
	srv       *http.Server
	prefix    string // tcp only: "/<random>"
	endpoint  string
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	sockPath  string
	wg        sync.WaitGroup  // handlers in flight
	createMu  sync.Mutex      // serialises creates so the container cap holds
	execs     map[string]bool // exec ids created through this grant; guarded by Broker.mu from here on
	subject   string
	sandboxID string
	expires   time.Time
	terms     terms
	dReq      int64
	stopping  bool
	nextReq   uint64
	inflight  map[uint64]context.CancelFunc
	pulls     bucket
}

// terms are the grant's limits.
type terms struct {
	images   []imageRule
	gpu      bool
	nanoCPUs int64
	memory   int64
}

// New prepares the broker; listeners are created per grant by Attach.
func New(opts Options) (*Broker, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	network := opts.Network
	if network == "" {
		network = "unix"
		if runtime.GOOS == "windows" {
			network = "tcp"
		}
	}
	if network != "unix" && network != "tcp" {
		return nil, fmt.Errorf("docker: unknown network %q", network)
	}
	b := &Broker{
		engine:     opts.Engine,
		dir:        opts.SocketDir,
		network:    network,
		gpuDevices: opts.GPUDevices,
		audit:      opts.Audit,
		ceiling:    opts.Ceiling,
		log:        logger.With("component", "broker", "broker", broker.KindDockerHost),
		now:        time.Now,
		grants:     make(map[string]*grant),
		pending:    make(map[string]*broker.Usage),
		paused:     make(map[string]string),
	}
	if b.engine == nil {
		b.engine = defaultEngine()
	}
	if network == "unix" {
		if b.dir == "" {
			b.dir = filepath.Join(os.TempDir(), fmt.Sprintf("mycel-docker-%d", os.Getuid()))
		}
		if err := os.MkdirAll(b.dir, 0o700); err != nil {
			return nil, fmt.Errorf("docker: socket dir: %w", err)
		}
		if err := checkSocketDir(b.dir); err != nil {
			return nil, err
		}
		if err := os.Chmod(b.dir, 0o700); err != nil { //nolint:gosec // a directory needs the search bit; owner only
			return nil, fmt.Errorf("docker: socket dir: %w", err)
		}
	}
	b.proxy = &httputil.ReverseProxy{
		Rewrite:        b.rewrite,
		Transport:      b.engine,
		FlushInterval:  -1,
		ModifyResponse: b.modifyResponse,
		ErrorHandler:   b.proxyError,
		ErrorLog:       slog.NewLogLogger(b.log.Handler(), slog.LevelDebug),
	}
	return b, nil
}

// Kind implements broker.Broker.
func (b *Broker) Kind() string { return broker.KindDockerHost }

// Available pings the Engine.
func (b *Broker) Available(ctx context.Context) (bool, string) {
	resp, err := b.engineDo(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return false, "docker engine unreachable: " + err.Error()
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("docker engine ping: status %d", resp.StatusCode)
	}
	return true, ""
}

// Attach implements broker.Broker: it opens the grant's socket (or loopback
// listener) and returns unix:///<dir>/<grant>.sock or
// tcp://127.0.0.1:<port>/<prefix>. Re-attaching keeps the endpoint and
// replaces the terms.
func (b *Broker) Attach(ctx context.Context, a broker.Attachment) (string, error) {
	g := a.Grant
	spec := g.GetSpec().GetDockerHost()
	if g.GetId() == "" || spec == nil {
		return "", fmt.Errorf("docker: %w: not a docker-host grant", broker.ErrNotAllowed)
	}
	if !grantIDRe.MatchString(g.GetId()) {
		return "", fmt.Errorf("docker: invalid grant id %q", g.GetId())
	}
	if b.ceiling != nil && !b.ceiling.Offers(OfferName) {
		return "", broker.ErrCeiling
	}
	if !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(b.now()) {
		return "", fmt.Errorf("docker: grant %s already expired", g.GetId())
	}
	t := terms{
		images:   parseImageRules(spec.GetImages()),
		gpu:      spec.GetGpu(),
		nanoCPUs: int64(spec.GetCpus()) * 1e9,
		memory:   spec.GetMemoryBytes(),
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return "", errors.New("docker: broker closed")
	}
	if gr, ok := b.grants[g.GetId()]; ok {
		gr.subject, gr.sandboxID, gr.expires, gr.terms = g.GetSubject(), a.SandboxID, a.ExpiresAt, t
		b.mu.Unlock()
		b.log.Info("docker grant re-attached", "grant", gr.id, "images", len(t.images))
		return gr.endpoint, nil
	}
	b.mu.Unlock()

	gr, err := b.listen(ctx, g.GetId())
	if err != nil {
		return "", err
	}
	gr.subject, gr.sandboxID, gr.expires, gr.terms = g.GetSubject(), a.SandboxID, a.ExpiresAt, t

	b.mu.Lock()
	if b.closed || b.grants[gr.id] != nil {
		b.mu.Unlock()
		b.stopGrant(gr)
		return "", errors.New("docker: grant attached concurrently or broker closed")
	}
	b.grants[gr.id] = gr
	b.mu.Unlock()
	go func() {
		defer close(gr.done)
		if err := gr.srv.Serve(gr.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.log.Error("docker grant listener stopped", "grant", gr.id, "err", err)
		}
	}()
	b.log.Info("docker grant attached", "grant", gr.id, "subject", gr.subject, "sandbox", gr.sandboxID,
		"images", len(t.images), "gpu", t.gpu, "nano_cpus", t.nanoCPUs, "memory", t.memory)
	return gr.endpoint, nil
}

// listen creates the grant's listener and server.
func (b *Broker) listen(ctx context.Context, id string) (*grant, error) {
	gctx, cancel := context.WithCancel(context.Background())
	gr := &grant{id: id, ctx: gctx, cancel: cancel, done: make(chan struct{}), execs: map[string]bool{},
		inflight: map[uint64]context.CancelFunc{}}
	var lc net.ListenConfig
	switch b.network {
	case "unix":
		gr.sockPath = filepath.Join(b.dir, id+".sock")
		_ = os.Remove(gr.sockPath)
		ln, err := lc.Listen(ctx, "unix", gr.sockPath)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("docker: listen: %w", err)
		}
		if err := os.Chmod(gr.sockPath, 0o600); err != nil {
			_ = ln.Close()
			cancel()
			return nil, fmt.Errorf("docker: socket mode: %w", err)
		}
		gr.ln = ln
		gr.endpoint = "unix://" + gr.sockPath
	default:
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			cancel()
			return nil, fmt.Errorf("docker: listen: %w", err)
		}
		buf := make([]byte, prefixBytes)
		if _, err := rand.Read(buf); err != nil {
			_ = ln.Close()
			cancel()
			return nil, fmt.Errorf("docker: prefix: %w", err)
		}
		gr.ln = ln
		gr.prefix = "/" + base64.RawURLEncoding.EncodeToString(buf)
		gr.endpoint = "tcp://" + ln.Addr().String() + gr.prefix
	}
	gr.srv = &http.Server{
		Handler:                      http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b.serve(gr, w, r) }),
		ReadHeaderTimeout:            headerTimeout,
		MaxHeaderBytes:               64 << 10,
		DisableGeneralOptionsHandler: true,
		BaseContext:                  func(net.Listener) context.Context { return gctx },
		ErrorLog:                     slog.NewLogLogger(b.log.Handler(), slog.LevelDebug),
	}
	return gr, nil
}

// stopGrant closes a grant's listener and in-flight requests.
func (b *Broker) stopGrant(gr *grant) {
	gr.cancel()
	_ = gr.srv.Close()
	if gr.sockPath != "" {
		_ = os.Remove(gr.sockPath)
	}
}

// Detach implements broker.Broker: the listener is closed, in-flight
// requests are cancelled and every container labelled with the grant is
// force-removed. Detaching an unknown grant still removes its containers.
func (b *Broker) Detach(ctx context.Context, grantID, reason string) error {
	b.mu.Lock()
	gr, ok := b.grants[grantID]
	subject, sandboxID := "", ""
	var cancels []context.CancelFunc
	if ok {
		delete(b.grants, grantID)
		gr.stopping = true
		subject, sandboxID = gr.subject, gr.sandboxID
		cancels = takeCancels(gr)
		b.keepPending(gr)
	}
	b.mu.Unlock()
	if ok {
		for _, c := range cancels {
			c()
		}
		b.stopGrant(gr)
		// Every handler that passed admission must finish before the
		// containers are listed, or a create in flight could outlive us.
		waitGroup(&gr.wg, freezeDeadline)
		select {
		case <-gr.done:
		case <-time.After(freezeDeadline):
		}
	}
	return b.removeContainers(ctx, grantID, subject, sandboxID, "detach: "+reason)
}

// removeContainers force-removes every container labelled with the grant.
func (b *Broker) removeContainers(ctx context.Context, grantID, subject, sandboxID, why string) error {
	ids, err := b.grantContainers(ctx, grantID, false)
	if err != nil {
		b.emit(broker.Record{Broker: broker.KindDockerHost, GrantID: grantID, Subject: subject, SandboxID: sandboxID,
			Op: "GET containers.list", Reason: why + ": list failed: " + err.Error()})
		b.log.Warn("docker: cannot list containers of a grant", "grant", grantID, "err", err)
		return fmt.Errorf("docker: list containers of %s: %w", grantID, err)
	}
	var failed error
	for _, id := range ids {
		rec := broker.Record{Broker: broker.KindDockerHost, GrantID: grantID, Subject: subject,
			SandboxID: sandboxID, Op: "DELETE containers.remove", Target: id, Allowed: true, Reason: why}
		resp, err := b.engineDo(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=1&v=1", nil)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		if err != nil {
			rec.Allowed, rec.Reason = false, why+": remove failed: "+err.Error()
			b.log.Warn("docker: cannot remove container", "grant", grantID, "container", id, "err", err)
			failed = errors.Join(failed, err)
		}
		b.emit(rec)
	}
	b.mu.Lock()
	for _, id := range ids {
		delete(b.paused, id)
	}
	b.mu.Unlock()
	b.log.Info("docker grant containers removed", "grant", grantID, "reason", why, "containers", len(ids))
	return failed
}

// Sweep removes containers labelled mycel.grant whose grant is not attached,
// e.g. left over from a daemon that stopped without detaching. The daemon
// calls it at start, before re-attaching grants.
func (b *Broker) Sweep(ctx context.Context) error {
	raw, _ := json.Marshal(map[string][]string{"label": {LabelGrant}})
	q := url.Values{"all": {"1"}, "filters": {string(raw)}}
	resp, err := b.engineDo(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("docker: sweep: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return fmt.Errorf("docker: sweep: status %d", resp.StatusCode)
	}
	var list []struct {
		Labels map[string]string `json:"Labels"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&list)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("docker: sweep: %w", err)
	}
	orphans := map[string]bool{}
	b.mu.Lock()
	for _, c := range list {
		if id := c.Labels[LabelGrant]; id != "" && b.grants[id] == nil {
			orphans[id] = true
		}
	}
	b.mu.Unlock()
	var failed error
	for id := range orphans {
		failed = errors.Join(failed, b.removeContainers(ctx, id, "", "", "sweep: grant not attached"))
	}
	return failed
}

func takeCancels(g *grant) []context.CancelFunc {
	out := make([]context.CancelFunc, 0, len(g.inflight))
	for _, c := range g.inflight {
		out = append(out, c)
	}
	clear(g.inflight)
	return out
}

// waitGroup waits for wg up to d.
func waitGroup(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// grantContainers lists the ids of containers labelled with the grant.
func (b *Broker) grantContainers(ctx context.Context, grantID string, runningOnly bool) ([]string, error) {
	f := map[string][]string{"label": {LabelGrant + "=" + grantID}}
	if runningOnly {
		f["status"] = []string{"running"}
	}
	raw, _ := json.Marshal(f)
	q := url.Values{"all": {"1"}, "filters": {string(raw)}}
	resp, err := b.engineDo(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var list []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		// The engine filtered by label; checking again keeps a misbehaving
		// engine from making us remove someone else's container.
		if c.ID != "" && c.Labels[LabelGrant] == grantID {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// Freeze implements broker.Broker: every request is refused, open streams
// (attach, logs, exec) are cut and running brokered containers are paused.
func (b *Broker) Freeze(reason string) {
	b.mu.Lock()
	b.frozen, b.reason = true, reason
	grants := make([]*grant, 0, len(b.grants))
	var cancels []context.CancelFunc
	for _, g := range b.grants {
		grants = append(grants, g)
		cancels = append(cancels, takeCancels(g)...)
	}
	b.mu.Unlock()
	for _, c := range cancels {
		c()
	}

	ctx, cancel := context.WithTimeout(context.Background(), freezeDeadline)
	defer cancel()
	paused := 0
	for _, g := range grants {
		ids, err := b.grantContainers(ctx, g.id, true)
		if err != nil {
			b.log.Warn("docker: freeze cannot list containers", "grant", g.id, "err", err)
			b.emit(broker.Record{Broker: broker.KindDockerHost, GrantID: g.id, Op: "POST containers.pause",
				Reason: "freeze: list failed: " + err.Error()})
			continue
		}
		for _, id := range ids {
			ok := b.engineSimple(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/pause")
			if ok {
				paused++
				b.mu.Lock()
				b.paused[id] = g.id
				b.mu.Unlock()
			}
			b.emit(broker.Record{Broker: broker.KindDockerHost, GrantID: g.id, Op: "POST containers.pause",
				Target: id, Allowed: ok, Reason: "freeze: " + reason})
		}
	}
	b.log.Warn("docker broker frozen", "reason", reason, "paused", paused, "cancelled", len(cancels))
}

// Unfreeze implements broker.Broker: containers paused by Freeze resume.
func (b *Broker) Unfreeze() {
	b.mu.Lock()
	b.frozen, b.reason = false, ""
	paused := b.paused
	b.paused = make(map[string]string)
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), freezeDeadline)
	defer cancel()
	for id, grantID := range paused {
		ok := b.engineSimple(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/unpause")
		b.emit(broker.Record{Broker: broker.KindDockerHost, GrantID: grantID, Op: "POST containers.unpause",
			Target: id, Allowed: ok, Reason: "unfreeze"})
	}
	b.log.Info("docker broker unfrozen", "unpaused", len(paused))
}

// cgroupParent returns the cgroup parent for a grant's containers in the
// form the Engine's cgroup driver expects.
func (b *Broker) cgroupParent(ctx context.Context, grantID string) (string, error) {
	b.mu.Lock()
	driver := b.driver
	b.mu.Unlock()
	if driver == "" {
		resp, err := b.engineDo(ctx, http.MethodGet, "/info", nil)
		if err != nil {
			return "", err
		}
		var info struct {
			CgroupDriver string `json:"CgroupDriver"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&info)
		_ = resp.Body.Close()
		if err != nil {
			return "", err
		}
		driver = info.CgroupDriver
		if driver == "" {
			driver = "cgroupfs"
		}
		b.mu.Lock()
		b.driver = driver
		b.mu.Unlock()
	}
	if driver == "systemd" {
		return cgroupParentBase + grantID + ".slice", nil
	}
	return cgroupParentBase + grantID, nil
}

// bucket is a token bucket; the zero value starts full on first use.
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) allow(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// engineSimple sends a body-less request and reports success.
func (b *Broker) engineSimple(ctx context.Context, method, path string) bool {
	resp, err := b.engineDo(ctx, method, path, nil)
	if err != nil {
		b.log.Warn("docker engine request failed", "path", path, "err", err)
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		b.log.Warn("docker engine request refused", "path", path, "status", resp.StatusCode)
		return false
	}
	return true
}

// Usage implements broker.Broker: requests per grant since the last call.
func (b *Broker) Usage() []broker.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, g := range b.grants {
		b.keepPending(g)
	}
	out := make([]broker.Usage, 0, len(b.pending))
	for _, u := range b.pending {
		out = append(out, *u)
	}
	clear(b.pending)
	return out
}

// keepPending moves a grant's request count into pending; b.mu held.
func (b *Broker) keepPending(g *grant) {
	if g.dReq == 0 {
		return
	}
	u := b.pending[g.id]
	if u == nil {
		u = &broker.Usage{GrantID: g.id, Subject: g.subject, Broker: broker.KindDockerHost}
		b.pending[g.id] = u
	}
	u.Requests += g.dReq
	g.dReq = 0
}

// Close stops every listener. Containers stay; the daemon detaches grants.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	grants := b.grants
	b.grants = map[string]*grant{}
	b.mu.Unlock()
	for _, g := range grants {
		b.stopGrant(g)
		<-g.done
	}
	return nil
}

// engineDo sends one request to the Engine.
func (b *Broker) engineDo(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	req, err := http.NewRequestWithContext(ctx, method, "http://"+engineHost+path, body) //nolint:gosec // G704: fixed placeholder host; the transport dials the local Engine
	if err != nil {
		cancel()
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.engine.RoundTrip(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (b *Broker) emit(rec broker.Record) {
	if b.audit != nil {
		b.audit(rec)
	}
}
