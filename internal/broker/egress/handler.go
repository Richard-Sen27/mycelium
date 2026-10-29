package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errRefused marks a destination that resolved to a refused address.
var errRefused = errors.New("destination address refused")

// dummyToken keeps the credential check's timing independent of whether the
// sandbox id exists.
const dummyToken = "________________________________"

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.enter() {
		http.Error(w, "egress proxy closed", http.StatusServiceUnavailable)
		return
	}
	defer p.wg.Done()
	switch {
	case r.Method == http.MethodConnect:
		p.handleConnect(w, r)
	case r.URL != nil && r.URL.IsAbs() && r.URL.Scheme == "http" && r.URL.Host != "":
		p.handleForward(w, r)
	default:
		http.Error(w, "egress proxy: only CONNECT and absolute http:// requests are supported", http.StatusBadRequest)
	}
}

// authenticate checks Proxy-Authorization and returns the claimed id, the
// sandbox and a snapshot of its rules.
func (p *Proxy) authenticate(r *http.Request) (string, *sandbox, []rule, bool) {
	user, pass, ok := parseBasic(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return user, nil, nil, false
	}
	p.mu.Lock()
	sb := p.byUser[user]
	var token string
	var rules []rule
	if sb != nil {
		token, rules = sb.token, sb.rules
	}
	p.mu.Unlock()
	want := token
	if sb == nil {
		want = dummyToken
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 || sb == nil {
		return user, nil, nil, false
	}
	return user, sb, rules, true
}

func parseBasic(h string) (user, pass string, ok bool) {
	const prefix = "basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(raw), ":")
	if !ok || user == "" {
		return "", "", false
	}
	return user, pass, true
}

func proxyAuthRequired(w http.ResponseWriter) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="mycel"`)
	http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
}

// allowDenial takes a token from the denial bucket of the sandbox with this
// id, or from the shared bucket when no such sandbox exists.
func (p *Proxy) allowDenial(id string) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	b := &p.unknownDeny
	if sb, ok := p.sandboxes[id]; ok {
		b = &sb.deny
	}
	return b.allow(now, p.tune.denyRate, p.tune.denyBurst)
}

// deny answers a denied request: within the sandbox's denial budget it audits
// and sends status; beyond it, it sends 429 without auditing.
func (p *Proxy) deny(w http.ResponseWriter, rec Record, status int, msg string) {
	rec.Allowed = false
	if !p.allowDenial(rec.SandboxID) {
		p.log.Debug("egress denial rate limited", "sandbox", rec.SandboxID, "host", rec.Host, "port", rec.Port)
		http.Error(w, "too many denied requests", http.StatusTooManyRequests)
		return
	}
	p.emit(rec)
	http.Error(w, msg, status)
}

// admit runs the checks shared by CONNECT and forwarding; host is
// normalised. On failure it has written the response.
func (p *Proxy) admit(w http.ResponseWriter, r *http.Request, host string, port int) (*sandbox, *liveConn, bool) {
	claimed, sb, rules, ok := p.authenticate(r)
	if !ok {
		// Failed authentication is audited through the denial limiter but
		// always answered with 407 so clients can still authenticate.
		if p.allowDenial(claimed) {
			p.log.Warn("egress authentication failed", "remote", r.RemoteAddr, "claimed_id", claimed,
				"method", r.Method, "host", host, "port", port)
			p.emit(Record{SandboxID: claimed, Host: host, Port: port, Method: r.Method, Unauthenticated: true})
		}
		proxyAuthRequired(w)
		return nil, nil, false
	}
	rec := Record{SandboxID: sb.id, Host: host, Port: port, Method: r.Method}
	p.mu.Lock()
	alias := sb.aliases[net.JoinHostPort(host, strconv.Itoa(port))]
	p.mu.Unlock()
	if alias == "" && !matchRules(rules, host, port) {
		p.deny(w, rec, http.StatusForbidden, "destination not in the sandbox allowlist")
		return nil, nil, false
	}
	lc, err := p.acquire(sb, host, port)
	if err == nil {
		lc.alias = alias
	}
	switch {
	case errors.Is(err, errBusy):
		p.deny(w, rec, http.StatusTooManyRequests, "too many connections for this sandbox")
		return nil, nil, false
	case err != nil:
		http.Error(w, "sandbox no longer registered", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	return sb, lc, true
}

// splitTarget splits host[:port] and normalises the host. Brackets are only
// accepted around IPv6 literals.
func splitTarget(hostport string, defPort int) (string, int, bool) {
	var host string
	port := defPort
	if h, ps, err := net.SplitHostPort(hostport); err == nil {
		n, ok := parsePort(ps)
		if !ok {
			return "", 0, false
		}
		host, port = h, n
		if strings.HasPrefix(hostport, "[") {
			host = "[" + h + "]"
		}
	} else {
		if defPort == 0 {
			return "", 0, false
		}
		host = hostport
		if !strings.HasPrefix(host, "[") && strings.Contains(host, ":") {
			return "", 0, false
		}
	}
	h, _ := normalizeHost(host)
	if h == "" {
		return "", 0, false
	}
	return h, port, true
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, ok := splitTarget(r.Host, 0)
	if !ok {
		http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	sb, lc, ok := p.admit(w, r, host, port)
	if !ok {
		return
	}
	defer p.release(sb, lc)
	rec := Record{SandboxID: sb.id, Host: host, Port: port, Method: http.MethodConnect, Allowed: true}

	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	lc.add(cancel)
	upstream, err := p.dialTarget(ctx, lc, host, port)
	cancel()
	if err != nil {
		p.dialFailed(w, lc, rec, err)
		return
	}
	rec.Addr = upstream.RemoteAddr().String()
	if !lc.add(func() { _ = upstream.Close() }) {
		p.emit(rec)
		http.Error(w, "sandbox connection revoked", http.StatusServiceUnavailable)
		return
	}

	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = upstream.Close()
		p.log.Error("egress hijack failed", "sandbox", sb.id, "err", err)
		p.emit(rec)
		http.Error(w, "cannot tunnel on this connection", http.StatusInternalServerError)
		return
	}
	lc.add(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Time{})

	var pending []byte
	if n := brw.Reader.Buffered(); n > 0 {
		pending, _ = brw.Peek(n)
		pending = append([]byte(nil), pending...)
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		p.emit(rec)
		return
	}
	rec.BytesIn, rec.BytesOut = p.splice(client, pending, upstream)
	p.emit(rec)
}

// dialFailed answers a failed upstream dial and audits it.
func (p *Proxy) dialFailed(w http.ResponseWriter, lc *liveConn, rec Record, err error) {
	switch {
	case errors.Is(err, errRefused):
		p.deny(w, rec, http.StatusForbidden, "destination address refused")
	case lc.isClosed():
		p.emit(rec)
		http.Error(w, "sandbox connection revoked", http.StatusServiceUnavailable)
	default:
		p.log.Debug("egress dial failed", "sandbox", rec.SandboxID, "host", rec.Host, "port", rec.Port, "err", err)
		p.emit(rec)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
	}
}

// addrAllowed reports whether a name may resolve to a: not a refused class
// and not one of the node's own addresses.
func (p *Proxy) addrAllowed(a netip.Addr) bool {
	return !refusedAddr(a) && !p.local.contains(a)
}

// dialChecked resolves host (normalised), refuses forbidden addresses and
// dials. An IP literal reaches here only when a rule lists it literally,
// which permits exactly that address; a name is refused if any resolved
// address is refused.
// dialTarget dials an alias target directly or the checked destination.
func (p *Proxy) dialTarget(ctx context.Context, lc *liveConn, host string, port int) (net.Conn, error) {
	if lc.alias != "" {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", lc.alias)
	}
	return p.dialChecked(ctx, host, port)
}

func (p *Proxy) dialChecked(ctx context.Context, host string, port int) (net.Conn, error) {
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip.Unmap()}
	} else {
		// The trailing dot defeats search-domain expansion.
		resolved, err := p.tune.lookup(ctx, host+".")
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("resolve %s: no addresses", host)
		}
		for _, a := range resolved {
			if !p.addrAllowed(a) {
				return nil, fmt.Errorf("%s resolves to %s: %w", host, a, errRefused)
			}
		}
		addrs = resolved
	}
	d := net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String()) //nolint:gosec // port validated to 1..65535
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// splice copies both directions until both end, an error occurs, or the
// tunnel is idle for the idle timeout. It returns bytes upstream→client (in)
// and client→upstream (out), and closes both connections.
func (p *Proxy) splice(client net.Conn, pending []byte, upstream net.Conn) (in, out int64) {
	defer func() { _ = client.Close(); _ = upstream.Close() }()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	if len(pending) > 0 {
		_ = upstream.SetWriteDeadline(time.Now().Add(p.tune.idle))
		n, err := upstream.Write(pending)
		out += int64(n)
		if err != nil {
			return 0, out
		}
	}
	done := make(chan int64, 1)
	go func() {
		done <- p.copyIdle(client, upstream, &last)
	}()
	out += p.copyIdle(upstream, client, &last)
	in = <-done
	return in, out
}

// copyIdle copies src to dst; on EOF it half-closes dst, on any other end it
// closes both so the opposite direction stops too.
func (p *Proxy) copyIdle(dst, src net.Conn, last *atomic.Int64) int64 {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		_ = src.SetReadDeadline(time.Now().Add(p.tune.idle))
		n, err := src.Read(buf)
		if n > 0 {
			last.Store(time.Now().UnixNano())
			_ = dst.SetWriteDeadline(time.Now().Add(p.tune.idle))
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				_ = src.Close()
				_ = dst.Close()
				return total
			}
		}
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < p.tune.idle {
			continue
		}
		if errors.Is(err, io.EOF) {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
				return total
			}
		}
		_ = src.Close()
		_ = dst.Close()
		return total
	}
}

// hopHeaders are removed in both directions when forwarding.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for f := range strings.SplitSeq(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// deadlineBody renews the client read deadline before each read of the
// request body and clears it once the body ends.
type deadlineBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration

	mu      sync.Mutex
	aborted bool
}

// abort stops the body: a blocked read returns at once and later reads fail.
func (b *deadlineBody) abort() {
	b.mu.Lock()
	b.aborted = true
	_ = b.rc.SetReadDeadline(time.Now())
	b.mu.Unlock()
}

func (b *deadlineBody) Read(buf []byte) (int, error) {
	b.mu.Lock()
	if b.aborted {
		b.mu.Unlock()
		return 0, errors.New("request body aborted")
	}
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(buf)
	if err != nil {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// flushWriter renews the client write deadline before each write and
// flushes after it.
type flushWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	idle time.Duration
}

func (f flushWriter) Write(b []byte) (int, error) {
	_ = f.rc.SetWriteDeadline(time.Now().Add(f.idle))
	n, err := f.w.Write(b)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}

// maxResponseHeaderBytes bounds what the forward path reads before the
// final response header is parsed.
const maxResponseHeaderBytes = 1 << 20

var errHeaderTooLarge = errors.New("upstream response header too large")

// headerCapture records what is read until stop is set, so the raw response
// header can be inspected after http.ReadResponse has consumed it.
type headerCapture struct {
	r    io.Reader
	buf  bytes.Buffer
	stop bool
}

func (h *headerCapture) Read(b []byte) (int, error) {
	n, err := h.r.Read(b)
	if !h.stop {
		h.buf.Write(b[:n])
		if h.buf.Len() > maxResponseHeaderBytes {
			return n, errHeaderTooLarge
		}
	}
	return n, err
}

// connectionTokens returns the Connection header values of the final
// (non-1xx) response in raw. http.ReadResponse drops the Connection header
// when it says "close", which would hide the hop-by-hop names it lists.
func connectionTokens(raw []byte) []string {
	tr := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		line, err := tr.ReadLine()
		if err != nil {
			return nil
		}
		hdr, err := tr.ReadMIMEHeader()
		if err != nil && len(hdr) == 0 {
			return nil
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if code, err := strconv.Atoi(fields[1]); err == nil && code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
				continue
			}
		}
		return hdr.Values("Connection")
	}
}

func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	host, port, ok := splitTarget(r.URL.Host, 80)
	if !ok {
		http.Error(w, "bad request host", http.StatusBadRequest)
		return
	}
	sb, lc, ok := p.admit(w, r, host, port)
	if !ok {
		return
	}
	defer p.release(sb, lc)
	rec := Record{SandboxID: sb.id, Host: host, Port: port, Method: r.Method, Allowed: true}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	lc.add(cancel)

	dctx, dcancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := p.dialTarget(dctx, lc, host, port)
	dcancel()
	if err != nil {
		p.dialFailed(w, lc, rec, err)
		return
	}
	rec.Addr = conn.RemoteAddr().String()
	if !lc.add(func() { _ = conn.Close() }) {
		p.emit(rec)
		http.Error(w, "sandbox connection revoked", http.StatusServiceUnavailable)
		return
	}
	stopAfter := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopAfter()
	var wireIn, wireOut atomic.Int64
	upstream := &meteredConn{Conn: conn, idle: p.tune.idle, in: &wireIn, out: &wireOut}

	rc := http.NewResponseController(w)
	defer func() {
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
	}()
	out := r.Clone(ctx)
	out.RequestURI = ""
	out.Host = ""
	out.Close = true // one request per upstream connection
	removeHopHeaders(out.Header)
	var body *deadlineBody
	if r.Body != nil && r.Body != http.NoBody {
		body = &deadlineBody{ReadCloser: r.Body, rc: rc, idle: p.tune.idle}
		out.Body = body
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- out.Write(upstream) }()
	finish := func() {
		lc.close() // closes the upstream connection, so the counts settle
		var werr error
		select {
		case werr = <-writeDone:
		default:
			// The writer may be blocked reading the client's body.
			if body != nil {
				body.abort()
			}
			werr = <-writeDone
		}
		if werr != nil {
			p.log.Debug("egress forward request write ended", "sandbox", sb.id, "host", host, "err", werr)
		}
		rec.BytesIn, rec.BytesOut = wireIn.Load(), wireOut.Load()
		p.emit(rec)
	}

	capture := &headerCapture{r: upstream}
	br := bufio.NewReader(capture)
	var resp *http.Response
	for {
		resp, err = http.ReadResponse(br, out)
		if err != nil || resp.StatusCode >= 200 {
			break
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			err = errors.New("protocol upgrades are not proxied")
			break
		}
		_ = resp.Body.Close() // interim 1xx response
	}
	capture.stop = true
	if err != nil {
		p.log.Debug("egress forward failed", "sandbox", sb.id, "host", host, "port", port, "err", err)
		finish()
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	for _, v := range connectionTokens(capture.buf.Bytes()) {
		for f := range strings.SplitSeq(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				resp.Header.Del(f)
			}
		}
	}
	capture.buf = bytes.Buffer{}
	removeHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	_ = rc.SetWriteDeadline(time.Now().Add(p.tune.idle))
	w.WriteHeader(resp.StatusCode)
	fw := flushWriter{w: w, rc: rc, idle: p.tune.idle}
	buf := make([]byte, 32*1024)
	_, _ = io.CopyBuffer(fw, resp.Body, buf)
	_ = rc.Flush()
	_ = resp.Body.Close()
	finish()
}
