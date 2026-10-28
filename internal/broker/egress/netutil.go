package egress

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// refusedPrefixes are ranges a resolved name may never point into, beyond
// the classes netip.Addr reports directly.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, includes 255.255.255.255
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
}

var teredoPrefix = netip.MustParsePrefix("2001::/32")

// refusedAddr reports addresses a resolved name may not point to: loopback,
// link-local, multicast, unspecified, private and the reserved ranges above,
// plus Teredo addresses whose embedded IPv4 server or client is refused.
func refusedAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return true
	}
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || a.IsPrivate() {
		return true
	}
	for _, p := range refusedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if teredoPrefix.Contains(a) {
		b := a.As16()
		server := netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]})
		client := netip.AddrFrom4([4]byte{b[12] ^ 0xff, b[13] ^ 0xff, b[14] ^ 0xff, b[15] ^ 0xff})
		return refusedAddr(server) || refusedAddr(client)
	}
	return false
}

// systemInterfaceAddrs lists the addresses on the node's interfaces.
func systemInterfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if na, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, na.Unmap())
		}
	}
	return out, nil
}

// localAddrs caches the node's own addresses, refreshed at most every
// `every`.
type localAddrs struct {
	load  func() ([]netip.Addr, error)
	every time.Duration

	mu     sync.Mutex
	set    map[netip.Addr]struct{}
	loaded time.Time
}

// contains reports whether a is one of the node's addresses. A failed
// refresh keeps the previous set.
func (l *localAddrs) contains(a netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set == nil || time.Since(l.loaded) >= l.every {
		if addrs, err := l.load(); err == nil {
			set := make(map[netip.Addr]struct{}, len(addrs))
			for _, x := range addrs {
				set[x.Unmap().WithZone("")] = struct{}{}
			}
			l.set = set
			l.loaded = time.Now()
		}
	}
	_, ok := l.set[a.Unmap().WithZone("")]
	return ok
}

// limitListener closes accepted connections beyond a global and a per-IP
// cap.
type limitListener struct {
	net.Listener
	maxTotal int
	maxPerIP int
	log      *slog.Logger

	mu    sync.Mutex
	total int
	perIP map[netip.Addr]int
}

func remoteIP(c net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	return netip.Addr{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := remoteIP(c)
		l.mu.Lock()
		if l.total >= l.maxTotal || l.perIP[ip] >= l.maxPerIP {
			l.mu.Unlock()
			l.log.Debug("egress connection limit reached", "remote", ip.String())
			_ = c.Close()
			continue
		}
		l.total++
		l.perIP[ip]++
		l.mu.Unlock()
		return &limitConn{Conn: c, l: l, ip: ip}, nil
	}
}

func (l *limitListener) done(ip netip.Addr) {
	l.mu.Lock()
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
	l.mu.Unlock()
}

type limitConn struct {
	net.Conn
	l    *limitListener
	ip   netip.Addr
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.done(c.ip) })
	return err
}

func (c *limitConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// meteredConn renews an idle deadline before every read and write and
// counts wire bytes.
type meteredConn struct {
	net.Conn
	idle    time.Duration
	in, out *atomic.Int64
}

func (c *meteredConn) Read(b []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	n, err := c.Conn.Read(b)
	c.in.Add(int64(n))
	return n, err
}

func (c *meteredConn) Write(b []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.idle))
	n, err := c.Conn.Write(b)
	c.out.Add(int64(n))
	return n, err
}
