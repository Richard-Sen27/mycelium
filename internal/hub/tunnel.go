package hub

// Phase 7 (P7-relay): the hub side of the relay tunnel and ACME
// (contract: internal/tunnel/doc.go).
//
//   - `tunnel.enabled` with <hub dir>/relay.url: the hub keeps its local
//     listeners and also dials the relay; API streams are served by the same
//     http.Server (same TLS config, so the relay sees ciphertext only) and
//     bus streams are forwarded to the local bus listener.
//   - `acme.enabled`: the API and bus listeners serve a Let's Encrypt
//     certificate for the advertise host and the CA-issued one otherwise.
//   - GET /api/hub/tunnel (admins): the tunnel and ACME status for
//     `mycel hub tunnel status`.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

// tunnelState is the running tunnel client (nil fields when disabled).
type tunnelState struct {
	mu     sync.Mutex
	client *tunnel.Client
	err    string // why the tunnel is not running although enabled
	acme   *tunnel.ACME
}

// newHubACME returns the ACME support of a hub with `acme.enabled`, nil
// otherwise.
func newHubACME(dir string, cfg *Config, log *slog.Logger) (*tunnel.ACME, error) {
	if !cfg.ACME.Enabled {
		return nil, nil
	}
	u, err := url.Parse(cfg.AdvertiseURL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("hub: acme needs advertise_url with a host, got %q", cfg.AdvertiseURL)
	}
	a, err := tunnel.NewACME(cfg.ACME, dir, u.Hostname(), log)
	if err != nil {
		return nil, fmt.Errorf("hub: acme: %w", err)
	}
	return a, nil
}

// startTunnel starts the relay client when hub.yaml enables the tunnel. A
// missing or invalid relay.url does not stop the hub: it runs locally and
// the status says why the tunnel is down. It is called once the API
// server is serving, and starts ACME issuance then (without a tunnel) or
// after the first attach to the relay (with one; the advertise host is
// only reachable on port 443 through the relay).
func (s *server) startTunnel(ctx context.Context, httpSrv *http.Server, busAddr string) (stop func()) {
	acmeCerts := s.tun.acme
	if !s.cfg.Tunnel.Enabled {
		acmeCerts.Start(ctx)
		return func() {}
	}
	fail := func(msg string) func() {
		s.tun.mu.Lock()
		s.tun.err = msg
		s.tun.mu.Unlock()
		s.log.Error("tunnel not started", "reason", msg)
		if acmeCerts != nil {
			s.log.Warn("acme: not requesting a certificate while the tunnel is down", "host", acmeCerts.Host())
		}
		return func() {}
	}
	b, err := os.ReadFile(filepath.Join(s.dir, tunnel.RelayURLFile))
	if errors.Is(err, fs.ErrNotExist) {
		return fail(tunnel.RelayURLFile + " is missing; run `mycel hub tunnel set -` and paste the relay URL")
	}
	if err != nil {
		return fail(err.Error())
	}
	ru, err := tunnel.ParseRelayURL(string(b))
	if err != nil {
		return fail(err.Error())
	}
	if err := s.cfg.Tunnel.Validate(); err != nil {
		return fail(err.Error())
	}
	c, err := tunnel.NewClient(tunnel.ClientOptions{
		URL: ru, Ports: s.cfg.Tunnel.PortMap(), BusAddr: busAddr, Logger: s.log,
		OnConnect: func() { acmeCerts.Start(ctx) },
	})
	if err != nil {
		return fail(err.Error())
	}
	s.tun.mu.Lock()
	s.tun.client = c
	s.tun.mu.Unlock()
	tctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.Run(tctx)
	}()
	go func() {
		defer wg.Done()
		if err := httpSrv.ServeTLS(c.APIListener(), "", ""); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			s.log.Warn("tunnel: api server", "err", err)
		}
	}()
	s.log.Info("tunnel enabled", "relay", ru.Redacted())
	return func() {
		cancel()
		_ = c.APIListener().Close()
		wg.Wait()
	}
}

// viaRelayKey marks a request context whose connection came through the
// relay tunnel.
type viaRelayKey struct{}

// relayConnContext is the API server's ConnContext: it marks connections
// that came through the relay, whose RemoteAddr is the source the relay
// reported.
func relayConnContext(ctx context.Context, c net.Conn) context.Context {
	if tunnel.ViaRelay(c) {
		return context.WithValue(ctx, viaRelayKey{}, true)
	}
	return ctx
}

// markRelayedRemote adds remote_via_relay to audit details that record a
// remote address ("remote" or "peer") taken from a relayed connection: that
// address is what the relay reported, so it is only as trustworthy as the
// relay. details is copied, never modified.
func markRelayedRemote(ctx context.Context, details map[string]any) map[string]any {
	if v, _ := ctx.Value(viaRelayKey{}).(bool); !v {
		return details
	}
	_, remote := details["remote"]
	_, peer := details["peer"]
	if !remote && !peer {
		return details
	}
	out := make(map[string]any, len(details)+1)
	for k, v := range details {
		out[k] = v
	}
	out["remote_via_relay"] = true
	return out
}

// busGetCertificate is the bus's GetCertificate: nil without ACME.
func busGetCertificate(a *tunnel.ACME) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if a == nil {
		return nil
	}
	return a.GetCertificate
}

// tunnelStatusJSON is GET /api/hub/tunnel.
type tunnelStatusJSON struct {
	Enabled bool              `json:"enabled"`
	Ports   map[string]string `json:"ports,omitempty"`
	Error   string            `json:"error,omitempty"`
	*tunnel.Status
	ACME struct {
		Enabled bool   `json:"enabled"`
		Host    string `json:"host,omitempty"`
		// Ready: browsers get the ACME certificate (before that, and after
		// a failure, the CA-issued one).
		Ready bool `json:"ready"`
	} `json:"acme"`
}

func (s *server) tunnelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hub/tunnel", s.handleTunnelStatus)
}

func (s *server) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u == nil {
		writeAPIError(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	if u.Role != roleAdmin {
		writeAPIError(w, http.StatusForbidden, "admins only")
		return
	}
	out := tunnelStatusJSON{Enabled: s.cfg.Tunnel.Enabled}
	if out.Enabled {
		out.Ports = map[string]string{}
		for p, role := range s.cfg.Tunnel.PortMap() {
			out.Ports[strconv.Itoa(p)] = role
		}
	}
	if t := s.tun; t != nil {
		t.mu.Lock()
		c, msg, a := t.client, t.err, t.acme
		t.mu.Unlock()
		out.Error = msg
		if c != nil {
			st := c.Status()
			out.Status = &st
		}
		out.ACME.Enabled, out.ACME.Host, out.ACME.Ready = a != nil, a.Host(), a.Ready()
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// ---- hub.yaml and relay.url (offline, `mycel hub init` / `hub tunnel`) ----

// TunnelSettings is what SetTunnel configured.
type TunnelSettings struct {
	Relay        string // redacted relay URL
	AdvertiseURL string
	BusAdvertise string
	// CertReissued is true when the server certificate was re-issued to
	// cover the relay host; a running hub must be restarted.
	CertReissued bool
}

// TunnelAdvertise returns the advertise URL and bus address of a hub
// reached through the relay at host with the given port map.
func TunnelAdvertise(host string, ports map[int]string) (advertiseURL, busAdvertise string, err error) {
	cfg := tunnel.Config{Ports: ports}
	if err := cfg.Validate(); err != nil {
		return "", "", err
	}
	apiPort, ok := cfg.PublicPort(tunnel.RoleAPI)
	if !ok {
		return "", "", errors.New("hub: the tunnel forwards no port to the API")
	}
	advertiseURL = "https://" + host
	if strings.Contains(host, ":") {
		advertiseURL = "https://[" + host + "]"
	}
	if apiPort != 443 {
		advertiseURL = "https://" + net.JoinHostPort(host, strconv.Itoa(apiPort))
	}
	if busPort, ok := cfg.PublicPort(tunnel.RoleBus); ok {
		busAdvertise = "tls://" + net.JoinHostPort(host, strconv.Itoa(busPort))
	}
	return advertiseURL, busAdvertise, nil
}

// SetTunnel stores relayURL in <hub dir>/relay.url (0600), enables the
// tunnel in hub.yaml with ports (nil: the defaults), points advertise_url
// and bus_advertise at the relay host, and re-issues the server
// certificate if it does not cover that host. The hub picks it up on its
// next start.
func SetTunnel(hubDir, relayURL string, ports map[int]string) (*TunnelSettings, error) {
	ru, err := tunnel.ParseRelayURL(relayURL)
	if err != nil {
		return nil, err
	}
	cfg, err := ReadConfig(hubDir)
	if err != nil {
		return nil, fmt.Errorf("hub: read config: %w", err)
	}
	tc := tunnel.Config{Enabled: true, Ports: ports}
	if len(ports) == 0 {
		tc.Ports = tunnel.DefaultPorts()
	}
	if err := tc.Validate(); err != nil {
		return nil, err
	}
	adv, busAdv, err := TunnelAdvertise(ru.Host, tc.Ports)
	if err != nil {
		return nil, err
	}
	if cfg.ACME.Enabled {
		if err := tunnel.ValidateACME(adv, cfg.ACME.Email, &tc); err != nil {
			return nil, err
		}
	}
	reissued, err := ensureServerCertHost(hubDir, ru.Host)
	if err != nil {
		return nil, err
	}
	if err := tunnel.WriteFileAtomic(filepath.Join(hubDir, tunnel.RelayURLFile), []byte(ru.String()+"\n")); err != nil {
		return nil, err
	}
	cfg.Tunnel = tc
	cfg.AdvertiseURL = adv
	if busAdv != "" {
		cfg.BusAdvertise = busAdv
	}
	if err := writeConfig(hubDir, cfg); err != nil {
		return nil, err
	}
	return &TunnelSettings{Relay: ru.Redacted(), AdvertiseURL: adv, BusAdvertise: cfg.BusAdvertise, CertReissued: reissued}, nil
}

// UnsetTunnel removes relay.url and disables the tunnel. advertise_url is
// left as it is.
func UnsetTunnel(hubDir string) error {
	cfg, err := ReadConfig(hubDir)
	if err != nil {
		return fmt.Errorf("hub: read config: %w", err)
	}
	if err := os.Remove(filepath.Join(hubDir, tunnel.RelayURLFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cfg.Tunnel = tunnel.Config{}
	return writeConfig(hubDir, cfg)
}

// SetACME writes the acme section of hub.yaml after validating it against
// the advertise URL and the tunnel.
func SetACME(hubDir string, ac tunnel.ACMEConfig) error {
	cfg, err := ReadConfig(hubDir)
	if err != nil {
		return fmt.Errorf("hub: read config: %w", err)
	}
	if ac.Enabled {
		var tc *tunnel.Config
		if cfg.Tunnel.Enabled {
			tc = &cfg.Tunnel
		}
		if err := tunnel.ValidateACME(cfg.AdvertiseURL, ac.Email, tc); err != nil {
			return err
		}
	}
	cfg.ACME = ac
	return writeConfig(hubDir, cfg)
}

// ensureServerCertHost re-issues server.crt/server.key from the CA with host
// added when the current certificate does not cover it.
func ensureServerCertHost(hubDir, host string) (bool, error) {
	host = strings.ToLower(host)
	certPEM, err := os.ReadFile(filepath.Join(hubDir, serverCertFile))
	if err != nil {
		return false, err
	}
	cur, err := parseCertPEM(certPEM)
	if err != nil {
		return false, err
	}
	if cur.VerifyHostname(host) == nil && time.Until(cur.NotAfter) > 24*time.Hour {
		return false, nil
	}
	hosts := slices.Clone(cur.DNSNames)
	for _, ip := range cur.IPAddresses {
		hosts = append(hosts, ip.String())
	}
	if !slices.Contains(hosts, host) {
		hosts = append([]string{host}, hosts...)
	}
	caPEM, err := os.ReadFile(filepath.Join(hubDir, caCertFile))
	if err != nil {
		return false, err
	}
	caKey, err := os.ReadFile(filepath.Join(hubDir, caKeyFile))
	if err != nil {
		return false, err
	}
	ca, err := identity.LoadCA(caPEM, caKey)
	if err != nil {
		return false, err
	}
	crt, key, err := ca.IssueServerCert(hosts, identity.ServerCertLifetime)
	if err != nil {
		return false, err
	}
	if _, err := tls.X509KeyPair(crt, key); err != nil {
		return false, err
	}
	// Each file is replaced atomically; a crash between the two leaves a
	// pair that fails to load, never a silently wrong one.
	if err := tunnel.WriteFileAtomic(filepath.Join(hubDir, serverKeyFile), key); err != nil {
		return false, err
	}
	if err := tunnel.WriteFileAtomic(filepath.Join(hubDir, serverCertFile), crt); err != nil {
		return false, err
	}
	if err := os.Chmod(filepath.Join(hubDir, serverCertFile), 0o644); err != nil { //nolint:gosec // a certificate is public
		return false, err
	}
	return true, nil
}
