package ollama

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/broker"
)

const (
	defaultUpstream = "http://127.0.0.1:11434"
	defaultListen   = "127.0.0.1:0"
	defaultRPM      = 60
	maxBodyBytes    = 1 << 20
	requestTimeout  = 10 * time.Minute
	tagsTTL         = time.Minute
	tagsTimeout     = 10 * time.Second
	headerTimeout   = 10 * time.Second
	tokenBytes      = 32
	credentialUser  = "grant"
	bodyReadTimeout = 30 * time.Second
	maxInflight     = 8   // concurrent requests per grant
	maxConns        = 256 // accepted connections, all grants
	maxOpLen        = 64
	// Refused requests without a valid credential are audited at most
	// this often, so a scanner cannot flood the audit stream.
	unauthAuditRate  = 1.0
	unauthAuditBurst = 10.0
)

// Options configures the broker.
type Options struct {
	// Upstream is the Ollama or OpenAI-compatible base URL; default
	// http://127.0.0.1:11434.
	Upstream string
	// Listen is the address sandboxes and agents reach; default 127.0.0.1:0.
	Listen  string
	Audit   broker.Audit
	Ceiling broker.Ceiling
	Logger  *slog.Logger
}

// Broker is the model-endpoint broker. It implements broker.Broker.
type Broker struct {
	upstream *url.URL
	ln       net.Listener
	srv      *http.Server
	proxy    *httputil.ReverseProxy
	client   *http.Client // for /api/tags discovery
	audit    broker.Audit
	ceiling  broker.Ceiling
	log      *slog.Logger
	now      func() time.Time
	done     chan struct{}

	mu           sync.Mutex
	closed       bool
	frozen       bool
	freezeReason string
	grants       map[string]*grant        // by grant id
	byKey        map[[32]byte]*grant      // by sha256 of the credential
	pending      map[string]*broker.Usage // usage of detached grants not reported yet
	tagsMu       sync.Mutex               // serialises upstream model discovery
	tags         map[string]bool          // normalised names present upstream
	tagsAt       time.Time
	unauth       bucket // samples audits of unauthenticated requests
}

var _ broker.Broker = (*Broker)(nil)

// grant is one attached grant. Fields are guarded by Broker.mu.
type grant struct {
	id        string
	subject   string
	sandboxID string
	token     string
	expires   time.Time
	terms     terms
	bucket    bucket
	used      int64 // generated tokens over the grant's lifetime on this node
	reserved  int64 // tokens reserved by requests in flight
	dReq      int64 // usage since the last Usage call
	dTok      int64
	model     string
	nextReq   uint64
	inflight  map[uint64]context.CancelFunc
}

// terms are the grant's limits, replaced wholesale on re-attach.
type terms struct {
	models      map[string]bool // normalised; empty = any model present upstream
	allowPull   bool
	allowDelete bool
	rpm         int
	maxTokens   int64
}

// New listens and serves until Close.
func New(opts Options) (*Broker, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	raw := opts.Upstream
	if raw == "" {
		raw = defaultUpstream
	}
	up, err := url.Parse(raw)
	if err != nil || (up.Scheme != "http" && up.Scheme != "https") || up.Host == "" {
		return nil, fmt.Errorf("ollama: invalid upstream %q", raw)
	}
	up.User = nil
	listen := opts.Listen
	if listen == "" {
		listen = defaultListen
	}
	var lc net.ListenConfig
	raw0, err := lc.Listen(context.Background(), "tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("ollama: listen %s: %w", listen, err)
	}
	ln := limitListener(raw0, maxConns)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // the upstream is local; never route it through an environment proxy
	b := &Broker{
		upstream: up,
		ln:       ln,
		client:   &http.Client{Transport: transport, Timeout: tagsTimeout},
		audit:    opts.Audit,
		ceiling:  opts.Ceiling,
		log:      logger.With("component", "broker", "broker", broker.KindOllama),
		now:      time.Now,
		done:     make(chan struct{}),
		grants:   make(map[string]*grant),
		byKey:    make(map[[32]byte]*grant),
		pending:  make(map[string]*broker.Usage),
	}
	b.proxy = &httputil.ReverseProxy{
		Rewrite:        b.rewrite,
		Transport:      transport,
		FlushInterval:  -1,
		ModifyResponse: b.modifyResponse,
		ErrorHandler:   b.proxyError,
		ErrorLog:       slog.NewLogLogger(b.log.Handler(), slog.LevelDebug),
	}
	b.srv = &http.Server{
		Handler:                      b,
		ReadHeaderTimeout:            headerTimeout,
		IdleTimeout:                  2 * time.Minute,
		MaxHeaderBytes:               64 << 10,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(b.log.Handler(), slog.LevelDebug),
	}
	go func() {
		defer close(b.done)
		if err := b.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.log.Error("ollama broker stopped", "err", err)
		}
	}()
	b.log.Info("ollama broker listening", "addr", ln.Addr().String(), "upstream", up.Redacted())
	return b, nil
}

// Addr is the bound listener address.
func (b *Broker) Addr() net.Addr { return b.ln.Addr() }

// Kind implements broker.Broker.
func (b *Broker) Kind() string { return broker.KindOllama }

// Available reports whether the upstream answers /api/tags.
func (b *Broker) Available(ctx context.Context) (bool, string) {
	if _, err := b.fetchTags(ctx); err != nil {
		return false, "model endpoint unreachable: " + err.Error()
	}
	return true, ""
}

// Attach implements broker.Broker. The endpoint is
// http://grant:<token>@<host>:<port>; re-attaching keeps the credential and
// replaces the terms.
func (b *Broker) Attach(_ context.Context, a broker.Attachment) (string, error) {
	g := a.Grant
	spec := g.GetSpec().GetOllama()
	if g.GetId() == "" || spec == nil {
		return "", fmt.Errorf("ollama: %w: not an ollama grant", broker.ErrNotAllowed)
	}
	if b.ceiling != nil && !b.ceiling.Offers(broker.KindOllama) {
		return "", broker.ErrCeiling
	}
	if !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(b.now()) {
		return "", fmt.Errorf("ollama: grant %s already expired", g.GetId())
	}
	t := terms{
		models:      make(map[string]bool, len(spec.GetModels())),
		allowPull:   spec.GetAllowPull(),
		allowDelete: spec.GetAllowDelete(),
		rpm:         int(spec.GetRequestsPerMinute()),
		maxTokens:   spec.GetMaxTokens(),
	}
	for _, m := range spec.GetModels() {
		if n := normModel(m); n != "" {
			t.models[n] = true
		}
	}
	if t.rpm <= 0 {
		t.rpm = defaultRPM
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", errors.New("ollama: broker closed")
	}
	gr, ok := b.grants[g.GetId()]
	if !ok {
		token, err := newToken()
		if err != nil {
			return "", err
		}
		gr = &grant{id: g.GetId(), token: token, inflight: make(map[uint64]context.CancelFunc)}
		b.grants[gr.id] = gr
		b.byKey[sha256.Sum256([]byte(token))] = gr
	}
	if gr.terms.rpm != t.rpm {
		gr.bucket = bucket{} // a new rate starts full; an unchanged one keeps its state
	}
	gr.subject, gr.sandboxID, gr.expires, gr.terms = g.GetSubject(), a.SandboxID, a.ExpiresAt, t
	b.log.Info("ollama grant attached", "grant", gr.id, "subject", gr.subject, "sandbox", gr.sandboxID,
		"models", len(t.models), "allow_pull", t.allowPull, "allow_delete", t.allowDelete,
		"rpm", t.rpm, "max_tokens", t.maxTokens, "replaced", ok)
	u := url.URL{Scheme: "http", User: url.UserPassword(credentialUser, gr.token), Host: b.ln.Addr().String()}
	return u.String(), nil
}

// Detach implements broker.Broker: the credential is forgotten and the
// grant's in-flight requests are cancelled. Detaching an unknown grant is a
// no-op.
func (b *Broker) Detach(_ context.Context, grantID, reason string) error {
	b.mu.Lock()
	g, ok := b.grants[grantID]
	var cancels []context.CancelFunc
	if ok {
		delete(b.grants, grantID)
		delete(b.byKey, sha256.Sum256([]byte(g.token)))
		cancels = takeCancels(g)
		b.keepPending(g)
	}
	b.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	if ok {
		b.log.Info("ollama grant detached", "grant", grantID, "reason", reason, "cancelled", len(cancels))
	}
	return nil
}

// keepPending moves a detached grant's unreported usage aside; b.mu held.
func (b *Broker) keepPending(g *grant) {
	if g.dReq == 0 && g.dTok == 0 {
		return
	}
	u := b.pending[g.id]
	if u == nil {
		u = &broker.Usage{GrantID: g.id, Subject: g.subject, Broker: broker.KindOllama}
		b.pending[g.id] = u
	}
	u.Requests += g.dReq
	u.Tokens += g.dTok
	if g.model != "" {
		u.Model = g.model
	}
	g.dReq, g.dTok = 0, 0
}

func takeCancels(g *grant) []context.CancelFunc {
	out := make([]context.CancelFunc, 0, len(g.inflight))
	for _, c := range g.inflight {
		out = append(out, c)
	}
	clear(g.inflight)
	return out
}

// Freeze implements broker.Broker: every request is refused with 503 and
// in-flight requests are cancelled.
func (b *Broker) Freeze(reason string) {
	b.mu.Lock()
	b.frozen, b.freezeReason = true, reason
	var cancels []context.CancelFunc
	for _, g := range b.grants {
		cancels = append(cancels, takeCancels(g)...)
	}
	b.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	b.log.Warn("ollama broker frozen", "reason", reason, "cancelled", len(cancels))
}

// Unfreeze implements broker.Broker.
func (b *Broker) Unfreeze() {
	b.mu.Lock()
	b.frozen, b.freezeReason = false, ""
	b.mu.Unlock()
	b.log.Info("ollama broker unfrozen")
}

// Usage implements broker.Broker: per-grant requests and generated tokens
// since the last call, including grants detached since then.
func (b *Broker) Usage() []broker.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]broker.Usage, 0, len(b.pending)+len(b.grants))
	for _, u := range b.pending {
		out = append(out, *u)
	}
	clear(b.pending)
	for _, g := range b.grants {
		if g.dReq == 0 && g.dTok == 0 {
			continue
		}
		out = append(out, broker.Usage{
			GrantID: g.id, Subject: g.subject, Broker: broker.KindOllama,
			Requests: g.dReq, Tokens: g.dTok, Model: g.model,
		})
		g.dReq, g.dTok = 0, 0
	}
	return out
}

// Close stops the listener and cancels every in-flight request.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	var cancels []context.CancelFunc
	for _, g := range b.grants {
		cancels = append(cancels, takeCancels(g)...)
	}
	b.grants = map[string]*grant{}
	b.byKey = map[[32]byte]*grant{}
	b.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	err := b.srv.Close()
	<-b.done
	return err
}

func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ollama: token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// lookup returns the grant for a presented credential, or nil.
func (b *Broker) lookup(token string) *grant {
	if token == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.byKey[sha256.Sum256([]byte(token))]
	if g == nil || subtle.ConstantTimeCompare([]byte(g.token), []byte(token)) != 1 {
		return nil
	}
	return g
}

// credential extracts the grant token: Bearer, Basic grant:<token>, or the
// token query parameter.
func credential(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, rest, _ := strings.Cut(h, " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToLower(scheme) {
		case "bearer":
			return rest
		case "basic":
			raw, err := base64.StdEncoding.DecodeString(rest)
			if err != nil {
				return ""
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok || user != credentialUser {
				return ""
			}
			return pass
		}
		return ""
	}
	return r.URL.Query().Get("token")
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

// limitListener caps the number of open accepted connections; Accept blocks
// while the cap is reached.
func limitListener(ln net.Listener, n int) net.Listener {
	return &limitedListener{Listener: ln, sem: make(chan struct{}, n), done: make(chan struct{})}
}

type limitedListener struct {
	net.Listener
	sem       chan struct{}
	closeOnce sync.Once
	done      chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitedConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
