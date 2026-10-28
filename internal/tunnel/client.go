package tunnel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// Reconnect backoff of the hub client. The backoff is reset only after a
// session that lasted StableSession, so a relay that accepts and drops the
// hub at once is not hammered.
const (
	MinBackoff    = time.Second
	MaxBackoff    = 30 * time.Second
	StableSession = 30 * time.Second
)

// maxPendingStreams caps the streams the hub holds while waiting for their
// header; more are closed at once.
const maxPendingStreams = 64

// ErrReplaced is the session error when the relay replaced this hub's
// session with a newer one for the same hub name (two hubs sharing one
// relay URL, or a restart racing the old session).
var ErrReplaced = errors.New("relay replaced this session with a newer one for the same hub")

// ClientOptions configures the hub side of a tunnel.
type ClientOptions struct {
	URL *RelayURL
	// Ports maps public relay ports to roles (Config.PortMap()).
	Ports map[int]string
	// BusAddr is the local bus listener (host:port) that "bus" streams are
	// forwarded to.
	BusAddr string
	Logger  *slog.Logger
	// OnConnect, when set, is called after every accepted hello (the hub
	// starts ACME issuance on the first one).
	OnConnect func()

	// Zero selects the defaults (tests lower them).
	MinBackoff, MaxBackoff time.Duration
	StableSession          time.Duration
	IdleTimeout            time.Duration
}

// Status is the hub client's state for `mycel hub tunnel status`. It never
// contains the token.
type Status struct {
	Relay     string    `json:"relay"` // redacted relay URL
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`    // when the current state began
	Connects  uint64    `json:"connects"` // successful sessions since start
	Active    int64     `json:"active"`   // forwarded connections open now
	Total     uint64    `json:"total"`    // forwarded connections since start
	LastError string    `json:"last_error,omitempty"`
	Replaced  uint64    `json:"replaced,omitempty"` // sessions the relay replaced with a newer one
}

// Client keeps the hub's session with its relay. API streams are handed to
// the hub's HTTP server through APIListener (the hub's own TLS config
// terminates TLS on them); bus streams are forwarded to BusAddr.
type Client struct {
	opts    ClientOptions
	log     *slog.Logger
	api     *streamListener
	pending chan struct{} // streams waiting for their header

	active                    atomic.Int64
	total, connects, replaced atomic.Uint64

	mu        sync.Mutex
	connected bool
	since     time.Time
	lastErr   string
	notice    error // why the relay closed the current session
}

// NewClient validates opts and returns a client; Run starts it.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.URL == nil {
		return nil, errors.New("tunnel: relay URL is required")
	}
	if err := validatePorts(opts.Ports); err != nil {
		return nil, err
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = MinBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = MaxBackoff
	}
	if opts.StableSession <= 0 {
		opts.StableSession = StableSession
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = IdleTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		opts: opts, log: log.With("component", "tunnel", "relay", opts.URL.Redacted()),
		api: newStreamListener(srcAddr(opts.URL.Redacted())), since: time.Now(),
		pending: make(chan struct{}, maxPendingStreams),
	}, nil
}

// APIListener yields the forwarded API connections. Closing it makes later
// API streams fail; Run keeps running.
func (c *Client) APIListener() net.Listener { return c.api }

// Status returns a snapshot.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{
		Relay: c.opts.URL.Redacted(), Connected: c.connected, Since: c.since,
		Connects: c.connects.Load(), Active: c.active.Load(), Total: c.total.Load(), LastError: c.lastErr,
		Replaced: c.replaced.Load(),
	}
}

func (c *Client) setState(connected bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected != connected {
		c.connected = connected
		c.since = time.Now()
	}
	switch {
	case connected:
		c.lastErr = ""
	case err != nil:
		c.lastErr = err.Error()
	}
}

// Run keeps a session with the relay until ctx is done, reconnecting with
// exponential backoff. It closes the API listener when it returns.
func (c *Client) Run(ctx context.Context) {
	defer func() { _ = c.api.Close() }()
	backoff := c.opts.MinBackoff
	for {
		start := time.Now()
		attached, err := c.runOnce(ctx)
		c.setState(false, err)
		if ctx.Err() != nil {
			return
		}
		if attached && time.Since(start) >= c.opts.StableSession {
			backoff = c.opts.MinBackoff
		}
		if errors.Is(err, ErrReplaced) {
			c.log.Error("tunnel: the relay replaced this hub's session; is another hub using the same relay URL?", "err", err, "retry_in", backoff)
		} else {
			c.log.Warn("tunnel: disconnected from relay", "err", err, "retry_in", backoff)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, c.opts.MaxBackoff)
	}
}

// runOnce dials, authenticates and serves one session. attached reports
// whether the relay accepted the hello.
func (c *Client) runOnce(ctx context.Context) (attached bool, err error) {
	u := c.opts.URL
	d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", u.Addr())
	if err != nil {
		return false, fmt.Errorf("dial relay: %w", err)
	}
	setNoDelay(raw)
	tc := tls.Client(raw, pinnedTLS(u.Fingerprint))
	_ = tc.SetDeadline(time.Now().Add(HandshakeTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = tc.Close()
		return false, fmt.Errorf("relay handshake: %w", err)
	}
	ports := make(map[string]string, len(c.opts.Ports))
	for p, role := range c.opts.Ports {
		ports[strconv.Itoa(p)] = role
	}
	if err := writeJSONLine(tc, hello{V: protocolVersion, Name: u.Name, Token: u.Token, Ports: ports}); err != nil {
		_ = tc.Close()
		return false, fmt.Errorf("relay hello: %w", err)
	}
	line, err := readLine(tc, maxHelloBytes)
	if err != nil {
		_ = tc.Close()
		return false, fmt.Errorf("relay answer: %w", err)
	}
	var a answer
	if err := json.Unmarshal(line, &a); err != nil {
		_ = tc.Close()
		return false, errors.New("relay answer: malformed")
	}
	if !a.OK {
		_ = tc.Close()
		// The reason comes from the relay: never log or show it raw.
		return false, fmt.Errorf("relay refused the hub: %s", cleanRemoteText(a.Error))
	}
	_ = tc.SetDeadline(time.Time{})
	ys, err := yamux.Client(tc, yamuxConfig())
	if err != nil {
		_ = tc.Close()
		return true, err
	}
	defer func() { _ = ys.Close() }()
	c.mu.Lock()
	c.notice = nil
	c.mu.Unlock()
	c.connects.Add(1)
	c.setState(true, nil)
	c.log.Info("tunnel: connected to relay")
	if c.opts.OnConnect != nil {
		c.opts.OnConnect()
	}
	stop := context.AfterFunc(ctx, func() { _ = ys.Close() })
	defer stop()
	for {
		st, err := ys.AcceptStream()
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			c.mu.Lock()
			notice := c.notice
			c.mu.Unlock()
			if notice != nil {
				return true, notice
			}
			return true, fmt.Errorf("relay session: %w", err)
		}
		select {
		case c.pending <- struct{}{}:
			go c.handleStream(ys, st)
		default:
			_ = st.Close()
		}
	}
}

// pinnedTLS trusts exactly the relay certificate with the given fingerprint.
func pinnedTLS(fp string) *tls.Config {
	want, _ := hex.DecodeString(fp)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The relay certificate is self-signed: VerifyConnection pins it by
		// fingerprint instead of a chain (it also runs on resumed sessions).
		InsecureSkipVerify: true, //nolint:gosec // G402: pinned below by SHA-256 fingerprint
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("tunnel: relay sent no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if len(want) != len(sum) || subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("tunnel: relay certificate does not match the fingerprint in the relay URL")
			}
			return nil
		},
	}
}

// handleStream reads the stream header and dispatches by role. It holds a
// c.pending slot until the header is read.
func (c *Client) handleStream(ys *yamux.Session, st *yamux.Stream) {
	_ = st.SetReadDeadline(time.Now().Add(streamHeaderWait))
	line, err := readLine(st, maxHeaderBytes)
	<-c.pending
	if err != nil {
		_ = st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	var h streamHeader
	if err := json.Unmarshal(line, &h); err != nil {
		_ = st.Close()
		return
	}
	if h.Close != "" {
		// The relay ends the session and says why.
		var notice error
		if h.Close == closeReplaced {
			notice = ErrReplaced
			c.replaced.Add(1)
		} else {
			notice = fmt.Errorf("relay closed the session: %s", cleanRemoteText(h.Close))
		}
		c.mu.Lock()
		c.notice = notice
		c.mu.Unlock()
		_ = st.Close()
		_ = ys.Close()
		return
	}
	c.total.Add(1)
	switch c.opts.Ports[h.Port] {
	case RoleAPI:
		c.active.Add(1)
		conn := &streamConn{Stream: st, remote: parseSrc(h.Src), onClose: func() { c.active.Add(-1) }}
		if !c.api.push(conn) {
			_ = conn.Close()
		}
	case RoleBus:
		c.active.Add(1)
		defer c.active.Add(-1)
		d := net.Dialer{Timeout: 5 * time.Second}
		local, err := d.Dial("tcp", c.opts.BusAddr)
		if err != nil {
			c.log.Warn("tunnel: dial local bus", "err", err)
			_ = st.Close()
			return
		}
		setNoDelay(local)
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ys.CloseChan():
				_ = local.Close()
			case <-done:
			}
		}()
		pipe(local, st, c.opts.IdleTimeout)
	default:
		_ = st.Close()
	}
}

// streamConn is an API stream as the HTTP server sees it: the relay-reported
// source as RemoteAddr, and an idle limit through the server's deadlines.
type streamConn struct {
	*yamux.Stream
	remote  net.Addr
	once    sync.Once
	onClose func()
}

func (s *streamConn) RemoteAddr() net.Addr { return s.remote }

func (s *streamConn) Close() error {
	s.once.Do(s.onClose)
	// Wake a reader blocked on a half-closed stream.
	_ = s.SetReadDeadline(time.Now())
	return s.Stream.Close()
}

// ViaRelay reports whether c, or a connection it wraps (a *tls.Conn), came
// through the relay tunnel. Its RemoteAddr is then the source the relay
// reported, which the hub trusts the relay for.
func ViaRelay(c net.Conn) bool {
	for range 4 {
		switch v := c.(type) {
		case *streamConn:
			return true
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return false
		}
	}
	return false
}

// streamListener is a net.Listener fed with tunnel streams.
type streamListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func newStreamListener(addr net.Addr) *streamListener {
	return &streamListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: addr}
}

func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *streamListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *streamListener) Addr() net.Addr { return l.addr }

// push hands c to Accept; false once the listener is closed.
func (l *streamListener) push(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	}
}
