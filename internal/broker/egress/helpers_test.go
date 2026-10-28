package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type auditLog struct {
	mu   sync.Mutex
	recs []Record
	ch   chan Record
}

func newAuditLog() *auditLog { return &auditLog{ch: make(chan Record, 1024)} }

func (a *auditLog) add(r Record) {
	a.mu.Lock()
	a.recs = append(a.recs, r)
	a.mu.Unlock()
	select {
	case a.ch <- r:
	default:
	}
}

func (a *auditLog) all() []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Record(nil), a.recs...)
}

// wait returns the next record satisfying f.
func (a *auditLog) wait(t *testing.T, f func(Record) bool) Record {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case r := <-a.ch:
			if f(r) {
				return r
			}
		case <-timeout:
			t.Fatal("timed out waiting for an audit record")
			return Record{}
		}
	}
}

func startTuned(t *testing.T, opts Options, mod func(*tuning)) (*Proxy, *auditLog) {
	t.Helper()
	al := newAuditLog()
	opts.Listen = "127.0.0.1:0"
	opts.Audit = al.add
	tune := defaultTuning()
	if mod != nil {
		mod(&tune)
	}
	p, err := start(context.Background(), opts, tune)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, al
}

func startProxy(t *testing.T, maxConns int) (*Proxy, *auditLog) {
	t.Helper()
	return startTuned(t, Options{MaxConnsPerSandbox: maxConns}, nil)
}

func register(t *testing.T, p *Proxy, id string, rules ...Rule) *url.URL {
	t.Helper()
	raw, err := p.Register(id, rules)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(ps)
	return n
}

func clientVia(ts *httptest.Server, proxy *url.URL) *http.Client {
	var tlsConf *tls.Config
	if ts != nil {
		if tr, ok := ts.Client().Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
			tlsConf = tr.TLSClientConfig.Clone()
		}
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: tlsConf, DisableKeepAlives: true},
		Timeout:   10 * time.Second,
	}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// tryConnect sends a CONNECT (plus extra bytes right after it) and returns
// the response and the connection.
func tryConnect(proxyAddr, target, user, pass, extra string) (*http.Response, net.Conn, *bufio.Reader, error) {
	c, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if user != "" {
		req += "Proxy-Authorization: " + basic(user, pass) + "\r\n"
	}
	req += "\r\n" + extra
	if _, err := io.WriteString(c, req); err != nil {
		_ = c.Close()
		return nil, nil, nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = c.Close()
		return nil, nil, nil, err
	}
	_ = c.SetReadDeadline(time.Time{})
	return resp, c, br, nil
}

func rawConnect(t *testing.T, proxyAddr, target, user, pass string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	resp, c, br, err := tryConnect(proxyAddr, target, user, pass, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return resp, c, br
}

func creds(u *url.URL) (string, string) {
	pw, _ := u.User.Password()
	return u.User.Username(), pw
}

// echoServer is a TCP echo server on addr.
func echoServerOn(t *testing.T, network, addr string) string {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("listen %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func echoServer(t *testing.T) string {
	t.Helper()
	return echoServerOn(t, "tcp", "127.0.0.1:0")
}

func echoOnce(t *testing.T, c net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	readExactly(t, c, br, msg)
}

func readExactly(t *testing.T, c net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	buf := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if string(buf) != msg {
		t.Fatalf("read %q, want %q", buf, msg)
	}
}

// expectClosed fails unless the peer closes c within 5 s.
func expectClosed(t *testing.T, c net.Conn, r io.Reader, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, r)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("%s: connection not closed", what)
	}
}

// countingConn counts bytes read.
type countingConn struct {
	net.Conn
	n atomic.Int64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

// rawUpstream serves one request per connection with a fixed raw response,
// reporting the request it saw and the exact bytes it read.
type rawUpstream struct {
	addr string
	reqs chan *http.Request
	read chan int64
}

func newRawUpstream(t *testing.T, response string) *rawUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	u := &rawUpstream{addr: ln.Addr().String(), reqs: make(chan *http.Request, 16), read: make(chan int64, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				cc := &countingConn{Conn: c}
				req, err := http.ReadRequest(bufio.NewReader(cc))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				u.reqs <- req
				u.read <- cc.n.Load()
				_, _ = io.WriteString(c, response)
			}()
		}
	}()
	return u
}

// syncBuffer is a goroutine-safe bytes.Buffer for captured logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func debugLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
