package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brutella/dnssd"
	dnssdlog "github.com/brutella/dnssd/log"
)

// ErrNotFound is returned by Find when no hub with the fingerprint answered.
var ErrNotFound = errors.New("discovery: no hub with that CA fingerprint answered on the local network")

// TXT record keys and the record version.
const (
	txtTrustDomain = "td"
	txtURL         = "url"
	txtBus         = "bus"
	txtFingerprint = "fp"
	txtVersion     = "v"
	recordVersion  = "1"

	domain       = "local"
	maxTXTValue  = 200 // one TXT string is at most 255 bytes including "key="
	closeTimeout = 3 * time.Second
)

// quiet silences the library's own logger, which writes to stdout by
// default (it would corrupt the CLI's --json output).
var quiet = sync.OnceFunc(func() {
	dnssdlog.Info.Disable()
	dnssdlog.Debug.Disable()
})

// lowerHex64 reports whether s is a SHA-256 fingerprint in lower hex.
func lowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// httpsURL reports whether s is an https URL with a host.
func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != ""
}

// txtFor validates h and returns its TXT record.
func txtFor(h Hub) (map[string]string, error) {
	fp := strings.ToLower(h.CAFingerprint)
	switch {
	case h.TrustDomain == "":
		return nil, errors.New("discovery: trust domain is required")
	case !httpsURL(h.URL):
		return nil, fmt.Errorf("discovery: advertise URL %q is not an https URL", h.URL)
	case !lowerHex64(fp):
		return nil, errors.New("discovery: CA fingerprint must be 64 hex characters")
	}
	txt := map[string]string{
		txtTrustDomain: h.TrustDomain, txtURL: h.URL, txtBus: h.BusURL, txtFingerprint: fp, txtVersion: recordVersion,
	}
	for k, v := range txt {
		if len(v) > maxTXTValue {
			return nil, fmt.Errorf("discovery: %s is longer than %d bytes", k, maxTXTValue)
		}
	}
	return txt, nil
}

// ---- browsing ----

type mdnsBrowser struct{}

func (mdnsBrowser) Browse(ctx context.Context, timeout time.Duration) ([]Hub, error) {
	return browseMDNS(ctx, timeout)
}

// hubFromTXT turns one browse answer into a Hub; answers that are not a
// valid version 1 record are ignored.
func hubFromTXT(name string, txt map[string]string, port int, ips []net.IP) (Hub, bool) {
	if txt[txtVersion] != recordVersion {
		return Hub{}, false
	}
	h := Hub{
		TrustDomain:   txt[txtTrustDomain],
		URL:           txt[txtURL],
		BusURL:        txt[txtBus],
		CAFingerprint: strings.ToLower(txt[txtFingerprint]),
		Port:          port,
	}
	if h.TrustDomain == "" {
		h.TrustDomain = name
	}
	if !httpsURL(h.URL) || !lowerHex64(h.CAFingerprint) {
		return Hub{}, false
	}
	for _, ip := range ips {
		h.Addresses = append(h.Addresses, ip.String())
	}
	return h, true
}

// collector merges answers per (fingerprint, URL): the same hub is seen
// once per interface.
type collector struct {
	mu   sync.Mutex
	hubs []Hub
}

func (c *collector) add(h Hub) Hub {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.hubs {
		e := &c.hubs[i]
		if e.CAFingerprint == h.CAFingerprint && e.URL == h.URL {
			for _, a := range h.Addresses {
				if !slices.Contains(e.Addresses, a) {
					e.Addresses = append(e.Addresses, a)
				}
			}
			return *e
		}
	}
	c.hubs = append(c.hubs, h)
	return h
}

func (c *collector) list() []Hub {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.hubs)
}

func browseMDNS(ctx context.Context, timeout time.Duration) ([]Hub, error) {
	quiet()
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var c collector
	add := func(e dnssd.BrowseEntry) {
		h, ok := hubFromTXT(e.Name, e.Text, e.Port, e.IPs)
		if !ok {
			return
		}
		c.add(h)
	}
	err := dnssd.LookupType(bctx, ServiceType+"."+domain+".", add, func(dnssd.BrowseEntry) {})
	if ctx.Err() != nil {
		return c.list(), ctx.Err()
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return c.list(), fmt.Errorf("discovery: browse: %w", err)
	}
	return c.list(), nil
}

func find(ctx context.Context, b Browser, caFingerprint string, timeout time.Duration) ([]Hub, error) {
	fp := strings.ToLower(caFingerprint)
	if !lowerHex64(fp) {
		return nil, errors.New("discovery: CA fingerprint must be 64 hex characters")
	}
	hubs, err := b.Browse(ctx, timeout)
	var out []Hub
	for _, h := range hubs {
		if strings.ToLower(h.CAFingerprint) == fp {
			out = append(out, h)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, ErrNotFound
}
