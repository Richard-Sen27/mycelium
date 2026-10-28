package tunnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/yamux"
)

// Roles a forwarded port can have on the hub.
const (
	RoleAPI = "api"
	RoleBus = "bus"
)

// Limits (contract: internal/tunnel/doc.go). A source is an IPv4 address or
// an IPv6 /64.
const (
	// StreamWindow is yamux's per-stream receive window: the most a stream
	// buffers when its reader stalls. yamux v0.1.2 refuses anything below
	// 256 KiB, so memory is bounded by StreamMemoryBudget instead.
	StreamWindow       = 256 << 10
	StreamMemoryBudget = 256 << 20 // stream windows per session, each side
	// MaxConns is the forwarded connections per hub: the memory budget
	// divided by the window (1024).
	MaxConns            = StreamMemoryBudget / StreamWindow
	MaxConnsPerSource   = 64               // concurrent forwarded connections per source
	RatePerSource       = 64               // new connections per second per source
	MaxPendingHandshake = 64               // control connections before their hello is answered
	IdleTimeout         = 10 * time.Minute // per forwarded connection
	HandshakeTimeout    = 10 * time.Second // control listener: TLS handshake and hello
	StreamBufferSize    = 32 << 10         // read buffer per stream
	maxPorts            = 8                // forwarded ports per hub
	maxHelloBytes       = 4 << 10
	maxHeaderBytes      = 256
	streamHeaderWait    = 10 * time.Second
	protocolVersion     = 1
	keepAliveInterval   = 15 * time.Second
	streamCloseTimeout  = 30 * time.Second
	// maxRemoteText bounds a relay-supplied error string.
	maxRemoteText = 200
)

// closeReplaced is the close reason the relay sends to a session it
// replaces with a newer one of the same hub.
const closeReplaced = "replaced"

// DefaultPorts returns the default port map: 443 → api, 7778 → bus.
func DefaultPorts() map[int]string { return map[int]string{443: RoleAPI, 7778: RoleBus} }

// PortMap returns the configured ports, or the defaults when none are set.
func (c Config) PortMap() map[int]string {
	if len(c.Ports) == 0 {
		return DefaultPorts()
	}
	return c.Ports
}

// Validate checks the tunnel section.
func (c Config) Validate() error {
	return validatePorts(c.PortMap())
}

// PublicPort returns the lowest public port forwarded to role.
func (c Config) PublicPort(role string) (int, bool) {
	ports := c.PortMap()
	keys := make([]int, 0, len(ports))
	for p, r := range ports {
		if r == role {
			keys = append(keys, p)
		}
	}
	if len(keys) == 0 {
		return 0, false
	}
	return slices.Min(keys), true
}

func validatePorts(ports map[int]string) error {
	if len(ports) == 0 || len(ports) > maxPorts {
		return fmt.Errorf("tunnel: between 1 and %d forwarded ports", maxPorts)
	}
	for p, role := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("tunnel: invalid port %d", p)
		}
		if role != RoleAPI && role != RoleBus {
			return fmt.Errorf("tunnel: port %d: role must be %q or %q, got %q", p, RoleAPI, RoleBus, role)
		}
	}
	return nil
}

// hello is the first line the hub sends on the control connection.
type hello struct {
	V     int               `json:"v"`
	Name  string            `json:"name"`
	Token string            `json:"token"`
	Ports map[string]string `json:"ports"`
}

// answer is the relay's reply to a hello.
type answer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// streamHeader is the first line the relay writes on every stream it opens:
// which public port the connection arrived on and its source address. A
// header with Close set carries no connection: the relay is ending the
// session and says why ("replaced").
type streamHeader struct {
	Port  int    `json:"port,omitempty"`
	Src   string `json:"src,omitempty"`
	Close string `json:"close,omitempty"`
}

// cleanRemoteText makes a string from the other end safe to log, store and
// print: control characters (and other non-printing runes) become spaces
// and it is cut to maxRemoteText runes.
func cleanRemoteText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxRemoteText {
			b.WriteString("...")
			break
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// sourceKey is the key per-source limits use: the IPv4 address, or the /64
// of an IPv6 address (one host usually holds a whole /64).
func sourceKey(a net.Addr) string {
	h := hostOf(a)
	ip := net.ParseIP(h)
	if ip == nil {
		return h
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// readLine reads one '\n'-terminated line of at most max bytes without
// reading past it (the bytes that follow belong to someone else).
func readLine(r io.Reader, max int) ([]byte, error) {
	var buf bytes.Buffer
	b := make([]byte, 1)
	for buf.Len() <= max {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return buf.Bytes(), nil
			}
			buf.WriteByte(b[0])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return nil, errors.New("tunnel: line too long")
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// yamuxConfig is the session configuration on both ends.
func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = keepAliveInterval
	c.AcceptBacklog = 1024
	c.MaxStreamWindowSize = StreamWindow
	c.StreamCloseTimeout = streamCloseTimeout
	c.LogOutput = io.Discard
	return c
}

// setNoDelay enables TCP_NODELAY when c is a TCP connection.
func setNoDelay(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

// closeWrite half-closes c: a FIN on TCP, a yamux stream FIN, a TLS
// close_notify. Connections without half-close are closed.
func closeWrite(c net.Conn) {
	switch v := c.(type) {
	case interface{ CloseWrite() error }:
		_ = v.CloseWrite()
	case *yamux.Stream:
		_ = v.Close() // yamux Close is a half-close: reads continue
	default:
		_ = c.Close()
	}
}

// pipe copies a↔b with StreamBufferSize buffers and half-close semantics
// until both directions finish, an error occurs, or nothing moves for idle.
// It closes both connections before returning.
func pipe(a, b net.Conn, idle time.Duration) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			// A deadline wakes readers that Close alone would not (a yamux
			// stream stays readable after a local close).
			now := time.Now()
			_ = a.SetDeadline(now)
			_ = b.SetDeadline(now)
			_ = a.Close()
			_ = b.Close()
		})
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, StreamBufferSize)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					closeBoth()
					return
				}
				last.Store(time.Now().UnixNano())
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					closeWrite(dst)
				} else {
					closeBoth()
				}
				return
			}
		}
	}
	go cp(a, b)
	go cp(b, a)
	go func() { wg.Wait(); close(done) }()

	tick := idle / 4
	if tick > 30*time.Second {
		tick = 30 * time.Second
	}
	if tick <= 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-done:
			closeBoth()
			return
		case <-t.C:
			if time.Since(time.Unix(0, last.Load())) >= idle {
				closeBoth()
				<-done
				return
			}
		}
	}
}

// srcAddr is a net.Addr for a source address reported by the relay.
type srcAddr string

func (srcAddr) Network() string  { return "tcp" }
func (a srcAddr) String() string { return string(a) }

// parseSrc returns the relay-reported source as a *net.TCPAddr when it parses.
func parseSrc(s string) net.Addr {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return srcAddr("relay")
	}
	ip := net.ParseIP(host)
	p, err := strconv.Atoi(port)
	if ip == nil || err != nil {
		return srcAddr("relay")
	}
	return &net.TCPAddr{IP: ip, Port: p}
}
