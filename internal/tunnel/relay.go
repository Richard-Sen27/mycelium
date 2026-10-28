package tunnel

import (
	"container/list"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// RelayOptions configures a relay.
type RelayOptions struct {
	// DataDir holds the relay certificate and tokens.json.
	DataDir string
	// Listen is the control listener address; default ":7790".
	Listen string
	// Public maps each public port (as named in a hub's hello) to its listen
	// address, e.g. {443: ":443", 7778: ":7778"}. Only these ports are ever
	// forwarded.
	Public map[int]string
	Logger *slog.Logger

	// Limits; zero selects the contract defaults (tests lower them).
	MaxConns          int
	MaxConnsPerSource int
	RatePerSource     int
	MaxPending        int // control connections waiting for their hello answer
	IdleTimeout       time.Duration
	HandshakeTimeout  time.Duration
	// TokenCheck is how often the attached hub's token is re-checked
	// against tokens.json (a revoked token drops the session); default 5 s.
	TokenCheck time.Duration
}

// RelayStats is a snapshot of the relay's counters.
type RelayStats struct {
	Hub      string // attached hub name, "" when none
	Active   int64  // forwarded connections open now
	Total    uint64 // forwarded connections since start
	Refused  uint64 // public connections refused (no hub, port, rate, cap)
	Sessions uint64 // hub sessions accepted since start
}

// Relay forwards public TCP connections to one hub over a yamux session.
// The hub dials the control listener and is the yamux client; the relay
// is the yamux server and opens one stream per public connection, so the
// hub only ever Accepts streams. The relay never Accepts hub-opened
// streams (they are closed at once).
type Relay struct {
	opts    RelayOptions
	log     *slog.Logger
	cert    tls.Certificate
	fp      string
	ctl     net.Listener
	pubs    map[int]net.Listener
	limit   *sourceLimiter // public connections
	ctlLim  *sourceLimiter // control connections
	pending chan struct{}  // control connections before their hello answer
	active  atomic.Int64

	total, refused, sessions atomic.Uint64

	srcMu  sync.Mutex
	perSrc map[string]int // open forwarded connections per source

	mu   sync.Mutex
	sess *relaySession
	wg   sync.WaitGroup
}

type relaySession struct {
	ys    *yamux.Session
	name  string
	hash  string
	ports map[int]bool
}

// NewRelay loads (or creates) the relay certificate and opens the control
// and public listeners. Call Serve to run it.
func NewRelay(opts RelayOptions) (*Relay, error) {
	if len(opts.Public) == 0 {
		return nil, errors.New("tunnel: the relay needs at least one public port")
	}
	if len(opts.Public) > maxPorts {
		return nil, fmt.Errorf("tunnel: at most %d public ports", maxPorts)
	}
	if opts.Listen == "" {
		opts.Listen = ":" + strconv.Itoa(DefaultControlPort)
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = MaxConns
	}
	if opts.MaxConnsPerSource <= 0 {
		opts.MaxConnsPerSource = MaxConnsPerSource
	}
	if opts.RatePerSource <= 0 {
		opts.RatePerSource = RatePerSource
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = MaxPendingHandshake
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = IdleTimeout
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = HandshakeTimeout
	}
	if opts.TokenCheck <= 0 {
		opts.TokenCheck = 5 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	cert, fp, err := LoadOrCreateRelayCert(opts.DataDir)
	if err != nil {
		return nil, err
	}
	r := &Relay{
		opts: opts, log: log.With("component", "relay"), cert: cert, fp: fp,
		pubs:    map[int]net.Listener{},
		limit:   newSourceLimiter(float64(opts.RatePerSource), maxSources),
		ctlLim:  newSourceLimiter(ctlRatePerSource, maxSources),
		pending: make(chan struct{}, opts.MaxPending),
		perSrc:  map[string]int{},
	}
	ok := false
	defer func() {
		if !ok {
			r.closeListeners()
		}
	}()
	if r.ctl, err = net.Listen("tcp", opts.Listen); err != nil {
		return nil, fmt.Errorf("tunnel: control listen: %w", err)
	}
	for port, addr := range opts.Public {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("tunnel: invalid public port %d", port)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("tunnel: public listen %d: %w", port, err)
		}
		r.pubs[port] = ln
	}
	ok = true
	return r, nil
}

// Fingerprint is the SHA-256 (hex) of the relay certificate.
func (r *Relay) Fingerprint() string { return r.fp }

// ControlAddr is the bound control address.
func (r *Relay) ControlAddr() string { return r.ctl.Addr().String() }

// PublicAddr is the bound address of a public port ("" if not public).
func (r *Relay) PublicAddr(port int) string {
	if ln, ok := r.pubs[port]; ok {
		return ln.Addr().String()
	}
	return ""
}

// Stats returns the current counters.
func (r *Relay) Stats() RelayStats {
	st := RelayStats{Active: r.active.Load(), Total: r.total.Load(), Refused: r.refused.Load(), Sessions: r.sessions.Load()}
	if s := r.current(); s != nil {
		st.Hub = s.name
	}
	return st
}

func (r *Relay) closeListeners() {
	if r.ctl != nil {
		_ = r.ctl.Close()
	}
	for _, ln := range r.pubs {
		_ = ln.Close()
	}
}

// Serve runs the relay until ctx is done.
func (r *Relay) Serve(ctx context.Context) error {
	ports := make([]string, 0, len(r.pubs))
	for p, ln := range r.pubs {
		ports = append(ports, fmt.Sprintf("%d=%s", p, ln.Addr()))
		r.wg.Add(1)
		go r.acceptPublic(p, ln)
	}
	sort.Strings(ports)
	r.log.Info("relay listening", "control", r.ControlAddr(), "public", ports, "fingerprint", r.fp)
	r.wg.Add(2)
	go r.acceptControl(ctx)
	go r.housekeeping(ctx)

	<-ctx.Done()
	r.closeListeners()
	r.mu.Lock()
	s := r.sess
	r.sess = nil
	r.mu.Unlock()
	if s != nil {
		_ = s.ys.Close()
	}
	r.wg.Wait()
	st := r.Stats()
	r.log.Info("relay stopped", "total", st.Total, "refused", st.Refused, "sessions", st.Sessions)
	return nil
}

func (r *Relay) current() *relaySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sess
}

// housekeeping drops a session whose token was revoked and prunes the rate
// limiters.
func (r *Relay) housekeeping(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.opts.TokenCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.limit.prune(now)
			r.ctlLim.prune(now)
			s := r.current()
			if s == nil {
				continue
			}
			hash, found, err := r.tokenHash(s.name)
			if err != nil {
				r.log.Warn("relay: read tokens", "err", err)
				continue
			}
			if !found || hash != s.hash {
				r.log.Warn("relay: hub token revoked; session closed", "hub", s.name)
				r.detach(s)
				_ = s.ys.Close()
			}
		}
	}
}

func (r *Relay) tokenHash(name string) (string, bool, error) {
	tf, err := readTokens(r.opts.DataDir)
	if err != nil {
		return "", false, err
	}
	for _, t := range tf.Tokens {
		if t.Name == name {
			return t.Hash, true, nil
		}
	}
	return "", false, nil
}

func (r *Relay) acceptControl(ctx context.Context) {
	defer r.wg.Done()
	for {
		c, err := r.ctl.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Warn("relay: control accept", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handleControl(ctx, c)
		}()
	}
}

// handleControl authenticates one hub and serves its session.
func (r *Relay) handleControl(ctx context.Context, raw net.Conn) {
	// Shutdown closes control connections in any state.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	setNoDelay(raw)
	remote := hostOf(raw.RemoteAddr())
	if !r.ctlLim.allow(sourceKey(raw.RemoteAddr()), time.Now()) {
		_ = raw.Close()
		return
	}
	// At most MaxPending connections between accept and the hello answer.
	select {
	case r.pending <- struct{}{}:
	default:
		r.log.Debug("relay: too many pending control connections", "remote", remote)
		_ = raw.Close()
		return
	}
	release := sync.OnceFunc(func() { <-r.pending })
	defer release()

	tc := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{r.cert}, MinVersion: tls.VersionTLS13})
	_ = tc.SetDeadline(time.Now().Add(r.opts.HandshakeTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		r.log.Debug("relay: control handshake failed", "remote", remote, "err", err)
		_ = tc.Close()
		return
	}
	line, err := readLine(tc, maxHelloBytes)
	if err != nil {
		r.log.Warn("relay: no hello", "remote", remote, "err", err)
		_ = tc.Close()
		return
	}
	s, reason := r.checkHello(line)
	if s == nil {
		r.log.Warn("relay: hello refused", "remote", remote, "reason", reason)
		_ = writeJSONLine(tc, answer{OK: false, Error: reason})
		_ = tc.Close()
		return
	}
	if err := writeJSONLine(tc, answer{OK: true}); err != nil {
		_ = tc.Close()
		return
	}
	release()
	_ = tc.SetDeadline(time.Time{})
	ys, err := yamux.Server(tc, yamuxConfig())
	if err != nil {
		_ = tc.Close()
		return
	}
	s.ys = ys
	if !r.attach(s, remote) {
		// Another hub attached between the check and now.
		_ = ys.Close()
		return
	}
	// The hub never opens streams; drop any it tries.
	go func() {
		for {
			st, err := ys.AcceptStream()
			if err != nil {
				return
			}
			_ = st.Close()
		}
	}()
	select {
	case <-ys.CloseChan():
	case <-ctx.Done():
		_ = ys.Close()
	}
	if r.detach(s) {
		r.log.Info("relay: hub detached", "hub", s.name, "active", r.active.Load(), "total", r.total.Load())
	}
}

// errOtherHub is the hello answer while another hub is attached.
const errOtherHub = "another hub is attached to this relay"

// checkHello validates a hello line; on success it returns the session
// (without its yamux side), otherwise a reason that is safe to send back.
func (r *Relay) checkHello(line []byte) (*relaySession, string) {
	var h hello
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, "malformed hello"
	}
	if h.V != protocolVersion {
		return nil, "unsupported protocol version"
	}
	hash, found, err := r.tokenHash(h.Name)
	if err != nil {
		r.log.Error("relay: read tokens", "err", err)
		return nil, "unauthorized"
	}
	if !found {
		// Same work as a known name, so the answer time says nothing.
		hash = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	got := hashSecret(h.Token)
	if subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1 || !found {
		return nil, "unauthorized"
	}
	// One hub per relay: only the attached hub's own name may replace its
	// session.
	if cur := r.current(); cur != nil && cur.name != h.Name {
		return nil, errOtherHub
	}
	if len(h.Ports) == 0 || len(h.Ports) > maxPorts {
		return nil, "no ports requested"
	}
	ports := map[int]bool{}
	for k, role := range h.Ports {
		p, err := strconv.Atoi(k)
		if err != nil {
			return nil, "invalid port " + strconv.Quote(k)
		}
		if role != RoleAPI && role != RoleBus {
			return nil, fmt.Sprintf("port %d: unknown role", p)
		}
		if _, ok := r.pubs[p]; !ok {
			return nil, fmt.Sprintf("port %d is not public on this relay", p)
		}
		ports[p] = true
	}
	return &relaySession{name: h.Name, hash: hash, ports: ports}, ""
}

// attach makes s the current session. A session of the same hub is
// replaced (and told so); a session of another hub wins and attach
// returns false.
func (r *Relay) attach(s *relaySession, remote string) bool {
	r.mu.Lock()
	old := r.sess
	if old != nil && old.name != s.name {
		r.mu.Unlock()
		r.log.Warn("relay: hello refused", "remote", remote, "reason", errOtherHub)
		return false
	}
	r.sess = s
	r.mu.Unlock()
	r.sessions.Add(1)
	if old != nil {
		r.log.Warn("relay: hub session replaced by a newer one", "hub", s.name, "remote", remote)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.closeReplaced(old)
		}()
	}
	r.log.Info("relay: hub attached", "hub", s.name, "remote", remote)
	return true
}

// closeReplaced tells a replaced session why and closes it.
func (r *Relay) closeReplaced(old *relaySession) {
	defer func() { _ = old.ys.Close() }()
	st, err := old.ys.OpenStream()
	if err != nil {
		return
	}
	_ = st.SetWriteDeadline(time.Now().Add(time.Second))
	if err := writeJSONLine(st, streamHeader{Close: closeReplaced}); err != nil {
		_ = st.Close()
		return
	}
	_ = st.Close()
	// Give the hub a moment to read it and close the session itself.
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-old.ys.CloseChan():
	case <-t.C:
	}
}

// detach clears s if it is still the current session.
func (r *Relay) detach(s *relaySession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == s {
		r.sess = nil
		return true
	}
	return false
}

func (r *Relay) acceptPublic(port int, ln net.Listener) {
	defer r.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Warn("relay: public accept", "port", port, "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handlePublic(port, c)
		}()
	}
}

func (r *Relay) refuse(c net.Conn) {
	r.refused.Add(1)
	_ = c.Close()
}

// takeSource counts one open connection for key; false at the per-source
// cap.
func (r *Relay) takeSource(key string) bool {
	r.srcMu.Lock()
	defer r.srcMu.Unlock()
	if r.perSrc[key] >= r.opts.MaxConnsPerSource {
		return false
	}
	r.perSrc[key]++
	return true
}

func (r *Relay) releaseSource(key string) {
	r.srcMu.Lock()
	defer r.srcMu.Unlock()
	if r.perSrc[key] <= 1 {
		delete(r.perSrc, key)
		return
	}
	r.perSrc[key]--
}

// handlePublic forwards one public connection over a new stream.
func (r *Relay) handlePublic(port int, c net.Conn) {
	key := sourceKey(c.RemoteAddr())
	if !r.limit.allow(key, time.Now()) {
		r.refuse(c)
		return
	}
	s := r.current()
	if s == nil || !s.ports[port] {
		r.refuse(c)
		return
	}
	if !r.takeSource(key) {
		r.refuse(c)
		return
	}
	defer r.releaseSource(key)
	if r.active.Add(1) > int64(r.opts.MaxConns) {
		r.active.Add(-1)
		r.refuse(c)
		return
	}
	defer r.active.Add(-1)
	setNoDelay(c)
	st, err := s.ys.OpenStream()
	if err != nil {
		r.refuse(c)
		return
	}
	_ = st.SetWriteDeadline(time.Now().Add(streamHeaderWait))
	if err := writeJSONLine(st, streamHeader{Port: port, Src: c.RemoteAddr().String()}); err != nil {
		_ = st.Close()
		r.refuse(c)
		return
	}
	_ = st.SetWriteDeadline(time.Time{})
	r.total.Add(1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		// A closed session must not leave an idle public peer waiting for
		// the idle timeout.
		select {
		case <-s.ys.CloseChan():
			_ = c.Close()
		case <-done:
		}
	}()
	pipe(c, st, r.opts.IdleTimeout)
}

func hostOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

// sourceLimiter is a token bucket per source key: rate new connections per
// second with a burst of rate. It holds at most max sources; beyond that
// the least recently seen source is forgotten (a flood of new sources
// cannot lock out a known one, at worst it resets that source's bucket).
type sourceLimiter struct {
	mu   sync.Mutex
	rate float64
	max  int
	m    map[string]*list.Element // of *bucket
	lru  *list.List               // front: most recently seen
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// ctlRatePerSource limits control connections (TLS handshakes and token
// checks) per source and second.
const ctlRatePerSource = 4

// maxSources bounds each limiter's memory.
const maxSources = 1 << 14

func newSourceLimiter(rate float64, max int) *sourceLimiter {
	return &sourceLimiter{rate: rate, max: max, m: map[string]*list.Element{}, lru: list.New()}
}

func (l *sourceLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if e := l.m[key]; e != nil {
		b = e.Value.(*bucket)
		l.lru.MoveToFront(e)
	} else {
		for len(l.m) >= l.max {
			oldest := l.lru.Back()
			l.lru.Remove(oldest)
			delete(l.m, oldest.Value.(*bucket).key)
		}
		b = &bucket{key: key, tokens: l.rate, last: now}
		l.m[key] = l.lru.PushFront(b)
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.rate, b.tokens+el*l.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets that have refilled completely. It walks from the
// least recently seen and stops at the first source seen within the last
// second (none after it can have refilled).
func (l *sourceLimiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for e := l.lru.Back(); e != nil; {
		b := e.Value.(*bucket)
		age := now.Sub(b.last)
		if age < time.Second {
			return
		}
		prev := e.Prev()
		if age > time.Second+time.Duration(float64(time.Second)*(l.rate-b.tokens)/l.rate) {
			l.lru.Remove(e)
			delete(l.m, b.key)
		}
		e = prev
	}
}
