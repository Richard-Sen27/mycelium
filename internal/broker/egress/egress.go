// Package egress is the node's egress proxy (spec: "Filesystem, network,
// secrets → Egress"): every sandbox's outbound traffic goes through it, it
// allows only the sandbox's allowlisted hosts, and it logs domain and bytes
// per sandbox to the audit stream.
//
// Contract (owned by the orchestrator): exported names are fixed.
//
// Protocol: HTTP forward proxy with CONNECT for TLS (clients set
// HTTP_PROXY/HTTPS_PROXY). Plain HTTP requests are forwarded with the Host
// header checked against the allowlist; CONNECT targets are checked by
// host:port. No TLS interception. Each sandbox authenticates to the proxy
// with a per-sandbox token in the Proxy-Authorization header (Basic, user =
// sandbox id, password = token), so sandboxes cannot use each other's
// allowlists; backends that give each sandbox its own network may instead
// bind one listener per sandbox and skip the token.
package egress

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("egress: not implemented")

// Rule is one allowlist entry: "host", "*.suffix", "host:port"; ports default
// to 80 and 443; no CIDRs in v1; IP literals are allowed when listed.
type Rule string

// Audit receives one record per connection or request.
type Audit func(rec Record)

// Record is what gets audited.
type Record struct {
	SandboxID string
	Host      string
	Port      int
	Allowed   bool
	BytesIn   int64
	BytesOut  int64
	Method    string // "CONNECT" or the HTTP method
	// Addr is the dialled upstream address (ip:port); empty when nothing
	// was dialled.
	Addr string
}

const (
	defaultMaxConns = 64
	dialTimeout     = 10 * time.Second
	headerTimeout   = 10 * time.Second
	maxHeaderBytes  = 64 << 10
	tokenBytes      = 24
)

// tuning holds the knobs tests shorten or replace.
type tuning struct {
	idle          time.Duration // tunnel and forwarded-request idle timeout
	httpIdle      time.Duration // keep-alive idle timeout on proxy connections
	maxConnsTotal int           // accepted connections, all clients
	maxConnsPerIP int           // accepted connections per remote IP
	denyRate      float64       // denials per second per sandbox
	denyBurst     float64
	ifaceRefresh  time.Duration
	lookup        func(ctx context.Context, name string) ([]netip.Addr, error)
	ifaceAddrs    func() ([]netip.Addr, error)
}

func defaultTuning() tuning {
	return tuning{
		idle:          5 * time.Minute,
		httpIdle:      60 * time.Second,
		maxConnsTotal: 1024,
		maxConnsPerIP: 128,
		denyRate:      20,
		denyBurst:     40,
		ifaceRefresh:  30 * time.Second,
		lookup: func(ctx context.Context, name string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
		},
		ifaceAddrs: systemInterfaceAddrs,
	}
}

// Proxy is the running egress proxy for one node.
type Proxy struct {
	ln        net.Listener
	srv       *http.Server
	audit     Audit
	log       *slog.Logger
	maxConns  int
	tune      tuning
	serveDone chan struct{}
	local     localAddrs

	mu          sync.Mutex
	closed      bool
	sandboxes   map[string]*sandbox
	unknownDeny bucket         // failed authentication with an unknown id
	wg          sync.WaitGroup // every running handler
}

// sandbox is one registered sandbox. Fields are guarded by Proxy.mu.
type sandbox struct {
	id     string
	token  string
	rules  []rule // replaced wholesale, never mutated in place
	active int
	conns  map[*liveConn]struct{}
	deny   bucket
}

// Options configures the proxy.
type Options struct {
	// Listen is the address sandboxes reach, e.g. "127.0.0.1:0" for the fake
	// backend or a vmnet/bridge address for VM backends.
	Listen string
	Audit  Audit
	Logger *slog.Logger
	// MaxConnsPerSandbox caps concurrent connections; default 64.
	MaxConnsPerSandbox int
}

// Start listens and serves until Close.
func Start(ctx context.Context, opts Options) (*Proxy, error) {
	return start(ctx, opts, defaultTuning())
}

func start(ctx context.Context, opts Options, tune tuning) (*Proxy, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	maxConns := opts.MaxConnsPerSandbox
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("egress: listen %s: %w", opts.Listen, err)
	}
	p := &Proxy{
		audit:     opts.Audit,
		log:       logger.With("component", "egress"),
		maxConns:  maxConns,
		tune:      tune,
		serveDone: make(chan struct{}),
		sandboxes: make(map[string]*sandbox),
		local:     localAddrs{load: tune.ifaceAddrs, every: tune.ifaceRefresh},
	}
	p.ln = &limitListener{
		Listener: ln,
		maxTotal: tune.maxConnsTotal,
		maxPerIP: tune.maxConnsPerIP,
		perIP:    make(map[netip.Addr]int),
		log:      p.log,
	}
	p.srv = &http.Server{
		Handler:                      p,
		ReadHeaderTimeout:            headerTimeout,
		IdleTimeout:                  tune.httpIdle,
		MaxHeaderBytes:               maxHeaderBytes,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
	}
	go func() {
		defer close(p.serveDone)
		if err := p.srv.Serve(p.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.Error("egress proxy stopped", "err", err)
		}
	}()
	p.log.Info("egress proxy listening", "addr", ln.Addr().String())
	return p, nil
}

// Addr is the bound listener address.
func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// Register adds a sandbox with its allowlist and returns the proxy URL to put
// into the sandbox environment (http://<sandbox id>:<token>@host:port).
// Registering an existing id replaces its rules and token and closes its
// live connections.
func (p *Proxy) Register(sandboxID string, allow []Rule) (proxyURL string, err error) {
	if sandboxID == "" || strings.ContainsAny(sandboxID, ":\r\n") {
		return "", fmt.Errorf("egress: invalid sandbox id %q", sandboxID)
	}
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("egress: token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	rules := parseRules(allow)

	var live []*liveConn
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", errors.New("egress: proxy closed")
	}
	if sb, ok := p.sandboxes[sandboxID]; ok {
		sb.token = token
		sb.rules = rules
		live = takeConns(sb)
	} else {
		p.sandboxes[sandboxID] = &sandbox{
			id:    sandboxID,
			token: token,
			rules: rules,
			conns: make(map[*liveConn]struct{}),
		}
	}
	p.mu.Unlock()
	for _, lc := range live {
		lc.close()
	}

	p.log.Info("egress sandbox registered", "sandbox", sandboxID,
		"rules", len(allow), "valid_rules", len(rules), "closed_conns", len(live))
	u := url.URL{
		Scheme: "http",
		User:   url.UserPassword(sandboxID, token),
		Host:   p.ln.Addr().String(),
	}
	return u.String(), nil
}

// Update replaces a sandbox's allowlist (live grants). Live connections whose
// target the new allowlist no longer permits are closed.
func (p *Proxy) Update(sandboxID string, allow []Rule) error {
	rules := parseRules(allow)
	var revoked []*liveConn
	p.mu.Lock()
	sb, ok := p.sandboxes[sandboxID]
	if ok {
		sb.rules = rules
		for lc := range sb.conns {
			if !matchRules(rules, lc.host, lc.port) {
				delete(sb.conns, lc)
				sb.active--
				revoked = append(revoked, lc)
			}
		}
	}
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("egress: unknown sandbox %q", sandboxID)
	}
	for _, lc := range revoked {
		lc.close()
	}
	p.log.Info("egress allowlist updated", "sandbox", sandboxID,
		"rules", len(allow), "valid_rules", len(rules), "closed_conns", len(revoked))
	return nil
}

// Unregister removes a sandbox and closes its connections.
func (p *Proxy) Unregister(sandboxID string) {
	p.mu.Lock()
	sb, ok := p.sandboxes[sandboxID]
	var live []*liveConn
	if ok {
		delete(p.sandboxes, sandboxID)
		live = takeConns(sb)
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	for _, lc := range live {
		lc.close()
	}
	p.log.Info("egress sandbox unregistered", "sandbox", sandboxID, "closed_conns", len(live))
}

// Close stops the proxy and waits for every handler to return.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var live []*liveConn
	for _, sb := range p.sandboxes {
		live = append(live, takeConns(sb)...)
	}
	p.sandboxes = map[string]*sandbox{}
	p.mu.Unlock()

	err := p.srv.Close()
	for _, lc := range live {
		lc.close()
	}
	p.wg.Wait()
	<-p.serveDone
	return err
}

// Match reports whether host:port is allowed by the rules.
func Match(rules []Rule, host string, port int) bool {
	return matchRules(parseRules(rules), host, port)
}

// enter registers a running handler; it fails once the proxy is closed.
func (p *Proxy) enter() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.wg.Add(1)
	return true
}

// takeConns detaches a sandbox's live connections; the caller holds p.mu and
// closes them after unlocking.
func takeConns(sb *sandbox) []*liveConn {
	out := make([]*liveConn, 0, len(sb.conns))
	for lc := range sb.conns {
		out = append(out, lc)
	}
	clear(sb.conns)
	sb.active = 0
	return out
}

// liveConn is one tracked tunnel or forwarded request that Unregister,
// Update and Close can tear down.
type liveConn struct {
	host string // normalised target host
	port int

	mu      sync.Mutex
	closed  bool
	closers []func()
}

// add registers a closer; if the connection was already torn down, f runs
// immediately and add reports false.
func (l *liveConn) add(f func()) bool {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		f()
		return false
	}
	l.closers = append(l.closers, f)
	l.mu.Unlock()
	return true
}

func (l *liveConn) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *liveConn) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	fs := l.closers
	l.closers = nil
	l.mu.Unlock()
	for _, f := range fs {
		f()
	}
}

var (
	errGone = errors.New("sandbox gone")
	errBusy = errors.New("too many connections")
)

// acquire reserves a connection slot for sb.
func (p *Proxy) acquire(sb *sandbox, host string, port int) (*liveConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.sandboxes[sb.id] != sb {
		return nil, errGone
	}
	if sb.active >= p.maxConns {
		return nil, errBusy
	}
	lc := &liveConn{host: host, port: port}
	sb.active++
	sb.conns[lc] = struct{}{}
	return lc, nil
}

// release frees a slot taken by acquire.
func (p *Proxy) release(sb *sandbox, lc *liveConn) {
	p.mu.Lock()
	if _, ok := sb.conns[lc]; ok {
		delete(sb.conns, lc)
		sb.active--
	}
	p.mu.Unlock()
	lc.close()
}

func (p *Proxy) emit(rec Record) {
	p.log.Info("egress",
		"sandbox", rec.SandboxID, "method", rec.Method, "host", rec.Host, "port", rec.Port,
		"addr", rec.Addr, "allowed", rec.Allowed, "bytes_in", rec.BytesIn, "bytes_out", rec.BytesOut)
	if p.audit != nil {
		p.audit(rec)
	}
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
