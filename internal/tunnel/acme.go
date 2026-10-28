package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// ACMECacheDir is the directory under the hub directory that holds the ACME
// account key and certificates.
const ACMECacheDir = "acme"

// Issuer timing: retries start at ACMEMinRetry and double up to
// ACMEMaxRetry; after any failure nothing asks the CA again for
// ACMEFailureHold (so probes cannot burn the CA's validation limits).
const (
	ACMEMinRetry    = time.Minute
	ACMEMaxRetry    = time.Hour
	ACMEFailureHold = 10 * time.Minute
)

// certSource is the part of autocert.Manager the hub uses (tests inject one).
type certSource interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// ACME serves a Let's Encrypt (or other ACME CA) certificate for the hub's
// advertise host to browsers and leaves everything else to the CA-issued
// certificate. A nil *ACME is valid and does nothing.
//
// Issuance never happens inside a normal client hello: a background issuer
// (Start) obtains the certificate and sets the ready flag; until then, and
// after a failure, hellos get the CA-issued certificate. Only TLS-ALPN-01
// challenge hellos ("acme-tls/1") and ready-state hellos reach the manager.
type ACME struct {
	host string
	src  certSource
	log  *slog.Logger

	// Issuer timing; newACME sets the defaults (tests lower them).
	minRetry, maxRetry, failHold time.Duration

	ready atomic.Bool
	kick  chan struct{} // a ready-state hello failed
	start sync.Once

	mu        sync.Mutex
	lastWarn  time.Time
	holdUntil time.Time
}

// NewACME returns the hub's ACME support, or nil when cfg is disabled.
// host is the advertise host; hubDir holds the cache. Call Start once the
// hub is reachable.
func NewACME(cfg ACMEConfig, hubDir, host string, log *slog.Logger) (*ACME, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if err := validatePublicHost(host); err != nil {
		return nil, err
	}
	if cfg.Email != "" {
		if err := validateEmail(cfg.Email); err != nil {
			return nil, err
		}
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(filepath.Join(hubDir, ACMECacheDir)),
		HostPolicy: autocert.HostWhitelist(host),
		Email:      cfg.Email,
	}
	if cfg.Directory != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.Directory}
	}
	return newACME(host, m, log), nil
}

func newACME(host string, src certSource, log *slog.Logger) *ACME {
	if log == nil {
		log = slog.Default()
	}
	return &ACME{
		host: host, src: src, log: log.With("component", "acme"),
		minRetry: ACMEMinRetry, maxRetry: ACMEMaxRetry, failHold: ACMEFailureHold,
		kick: make(chan struct{}, 1),
	}
}

// Host is the name the ACME certificate is for.
func (a *ACME) Host() string {
	if a == nil {
		return ""
	}
	return a.host
}

// Ready reports whether the ACME certificate is being served.
func (a *ACME) Ready() bool {
	return a != nil && a.ready.Load()
}

// GetCertificate is a tls.Config.GetCertificate. (nil, nil) means "use
// Config.Certificates", the CA-issued certificate.
//
//   - Another name than the advertise host: the CA certificate.
//   - A TLS-ALPN-01 challenge hello ("acme-tls/1"): the manager's challenge
//     certificate (or its error).
//   - A hello listing identity.HubCAALPN (nodes and CLIs): the CA
//     certificate, always.
//   - Otherwise (browsers): the ACME certificate once the issuer has one,
//     the CA certificate before that or after a failure.
func (a *ACME) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if a == nil {
		return nil, nil
	}
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if name != a.host {
		return nil, nil
	}
	if slices.Contains(hello.SupportedProtos, acme.ALPNProto) {
		return a.src.GetCertificate(hello)
	}
	if slices.Contains(hello.SupportedProtos, identity.HubCAALPN) || !a.ready.Load() {
		return nil, nil
	}
	cert, err := a.src.GetCertificate(hello)
	if err != nil || cert == nil {
		a.failed(err)
		select {
		case a.kick <- struct{}{}:
		default:
		}
		return nil, nil
	}
	return cert, nil
}

// failed records a failure: the CA certificate is served and nothing asks
// the CA again for failHold.
func (a *ACME) failed(err error) {
	a.ready.Store(false)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holdUntil = time.Now().Add(a.failHold)
	if time.Since(a.lastWarn) < time.Minute {
		return
	}
	a.lastWarn = time.Now()
	a.log.Warn("acme: no certificate for the advertise host; serving the hub CA certificate", "host", a.host, "err", err)
}

// Configure sets GetCertificate on c and adds the ALPN protocols an HTTPS
// listener needs ("h2", "http/1.1" and "acme-tls/1" for the challenge).
// identity.HubCAALPN is never added: it is a marker, not a protocol.
func (a *ACME) Configure(c *tls.Config, https bool) {
	if a == nil {
		return
	}
	c.GetCertificate = a.GetCertificate
	if https {
		for _, p := range []string{"h2", "http/1.1", acme.ALPNProto} {
			if !slices.Contains(c.NextProtos, p) {
				c.NextProtos = append(c.NextProtos, p)
			}
		}
	}
}

// Start runs the background issuer until ctx is done (once; later calls do
// nothing). The hub calls it when its API is reachable on port 443: once
// it serves, or with a tunnel after the first attach to the relay.
func (a *ACME) Start(ctx context.Context) {
	if a == nil {
		return
	}
	a.start.Do(func() { go a.issue(ctx) })
}

func (a *ACME) issue(ctx context.Context) {
	backoff := a.minRetry
	for {
		a.mu.Lock()
		hold := time.Until(a.holdUntil)
		a.mu.Unlock()
		if !sleepCtx(ctx, hold) {
			return
		}
		cert, err := a.src.GetCertificate(&tls.ClientHelloInfo{
			ServerName:        a.host,
			SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			SupportedCurves:   []tls.CurveID{tls.CurveP256},
			CipherSuites:      []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
		})
		if err == nil && cert != nil {
			if !a.ready.Swap(true) {
				a.log.Info("acme: certificate ready", "host", a.host)
			}
			backoff = a.minRetry
			select {
			case <-ctx.Done():
				return
			case <-a.kick:
			}
			continue
		}
		if err == nil {
			err = errors.New("no certificate")
		}
		a.failed(err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, a.maxRetry)
	}
}

// sleepCtx waits d (nothing when d <= 0); false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ValidateACME checks that a hub can use ACME (TLS-ALPN-01): the advertise
// URL's host must be a public DNS name reachable on port 443, either
// directly or through the relay's port 443 forwarded to the API (tun
// non-nil when the hub uses a tunnel). email may be empty.
func ValidateACME(advertiseURL, email string, tun *Config) error {
	u, err := url.Parse(advertiseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("tunnel: acme needs an https advertise URL")
	}
	if err := validatePublicHost(strings.ToLower(u.Hostname())); err != nil {
		return err
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("tunnel: acme (TLS-ALPN-01) needs the hub on public port 443; the advertise URL uses port %s "+
			"(advertise https://<host> and forward public port 443 to the hub's api_listen port)", p)
	}
	if tun != nil {
		if tun.PortMap()[443] != RoleAPI {
			return errors.New("tunnel: acme through a relay needs the relay's port 443 forwarded to the hub API")
		}
	}
	if email != "" {
		return validateEmail(email)
	}
	return nil
}

func validateEmail(e string) error {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e {
		return fmt.Errorf("tunnel: invalid acme email %q", e)
	}
	return nil
}

// validatePublicHost accepts a DNS name a public CA can issue for: not an IP
// address, at least two labels, not a local or reserved suffix.
func validatePublicHost(h string) error {
	if h == "" {
		return errors.New("tunnel: acme needs a host name")
	}
	if net.ParseIP(h) != nil {
		return fmt.Errorf("tunnel: acme needs a public DNS name, not the IP address %s", h)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 || len(h) > 253 {
		return fmt.Errorf("tunnel: acme needs a public DNS name, got %q", h)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("tunnel: invalid DNS name %q", h)
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("tunnel: invalid DNS name %q", h)
			}
		}
	}
	for _, s := range []string{"localhost", "local", "internal", "lan", "home", "corp", "test", "example", "invalid", "home.arpa", "onion"} {
		if h == s || strings.HasSuffix(h, "."+s) {
			return fmt.Errorf("tunnel: acme needs a public DNS name; %q is not publicly resolvable", h)
		}
	}
	return nil
}
