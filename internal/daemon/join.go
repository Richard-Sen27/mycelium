package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// maxCABody bounds the size of the /ca.pem response.
const maxCABody = 64 << 10

// httpTimeout bounds every single HTTP exchange with the hub API.
const httpTimeout = 30 * time.Second

// errCAMismatch is returned when the hub's CA does not match the token.
var errCAMismatch = errors.New("hub CA fingerprint does not match the token")

func join(ctx context.Context, opts JoinOptions) (*JoinResult, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.NodeDir == "" {
		return nil, errors.New("daemon: node directory is required")
	}
	tok, err := identity.ParseJoinToken(opts.Token)
	if err != nil {
		// The parse error never contains the token itself.
		return nil, fmt.Errorf("join token: %w", err)
	}
	hubURL := strings.TrimRight(tok.HubURL, "/")
	if opts.HubURL != "" {
		hubURL = strings.TrimRight(opts.HubURL, "/")
	}
	if err := checkHubURL(hubURL); err != nil {
		return nil, err
	}
	if HasState(opts.NodeDir) {
		return nil, ErrNodeExists
	}

	caPEM, err := fetchCA(ctx, hubURL)
	if err != nil {
		return nil, err
	}
	fp, err := identity.FingerprintPEM(caPEM)
	if err != nil {
		return nil, fmt.Errorf("hub CA: %w", err)
	}
	if !identity.ConstantTimeEqualHex(fp, tok.CAFingerprint) {
		return nil, errCAMismatch
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("hub CA: no certificate in PEM")
	}

	// From here on every connection verifies against the pinned CA only,
	// never the system roots: the join token must not reach a relay
	// operator or anyone else with a public certificate for the hub's name.
	key, keyPEM, err := identity.GenerateKey()
	if err != nil {
		return nil, err
	}
	csr, err := identity.NewCSR(key, "node")
	if err != nil {
		return nil, err
	}
	hc := pinnedClient(pool, nil)
	defer hc.CloseIdleConnections()
	client := mycelv1connect.NewEnrolServiceClient(hc, hubURL)
	resp, err := client.Enrol(ctx, connect.NewRequest(&mycelv1.EnrolRequest{Token: opts.Token, CsrDer: csr}))
	if err != nil {
		return nil, fmt.Errorf("enrol: %w", err)
	}
	msg := resp.Msg

	td, name, err := identity.ParseNodeSPIFFEID(msg.GetSpiffeId())
	if err != nil {
		return nil, fmt.Errorf("enrol response: %w", err)
	}
	if len(msg.GetCaPem()) > 0 {
		rfp, err := identity.FingerprintPEM(msg.GetCaPem())
		if err != nil || !identity.ConstantTimeEqualHex(rfp, fp) {
			return nil, errors.New("enrol response: CA differs from the pinned hub CA")
		}
	}
	certPEM := msg.GetCertPem()
	leaf, certName, err := identity.VerifyNodeCert(pool, td, certPEM)
	if err != nil {
		return nil, fmt.Errorf("enrol response: %w", err)
	}
	if certName != name {
		return nil, errors.New("enrol response: certificate name does not match SPIFFE ID")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, errors.New("enrol response: certificate does not match the node key")
	}
	if msg.GetBusUrl() == "" {
		return nil, errors.New("enrol response: no bus URL")
	}

	caps := append([]string{}, msg.GetCaps().GetNames()...)
	st := State{
		Name:        name,
		SPIFFEID:    msg.GetSpiffeId(),
		TrustDomain: string(td),
		HubURL:      hubURL,
		BusURL:      msg.GetBusUrl(),
		OwnerID:     msg.GetOwnerId(),
		Caps:        caps,
	}
	if err := WriteState(opts.NodeDir, st, keyPEM, certPEM, caPEM); err != nil {
		return nil, err
	}
	log.Info("joined hub as node "+name, "node", name, "hub", hubURL)
	return &JoinResult{
		NodeName:     name,
		SPIFFEID:     st.SPIFFEID,
		HubURL:       hubURL,
		BusURL:       st.BusURL,
		OwnerID:      st.OwnerID,
		Caps:         caps,
		CertNotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
	}, nil
}

// checkHubURL requires an https URL with a host: the join token and the
// renewal call must never travel over plain HTTP.
func checkHubURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return errors.New("hub URL must be an https URL with a host")
	}
	return nil
}

// fetchCA downloads <hub>/ca.pem without verifying the server: there is no
// trusted CA yet. The result is only trusted after its fingerprint matched the
// join token.
func fetchCA(ctx context.Context, hubURL string) ([]byte, error) {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		//nolint:gosec // G402: bootstrap fetch of the CA; the body is pinned by the token's CA fingerprint before use.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
	}
	hc := &http.Client{Transport: tr, Timeout: httpTimeout}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/ca.pem", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch hub CA: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch hub CA: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch hub CA: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCABody+1))
	if err != nil {
		return nil, fmt.Errorf("fetch hub CA: %w", err)
	}
	if len(body) > maxCABody {
		return nil, errors.New("fetch hub CA: response too large")
	}
	// Keep only the first certificate block.
	blk, _ := pem.Decode(bytes.TrimSpace(body))
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("fetch hub CA: no certificate in response")
	}
	return pem.EncodeToMemory(blk), nil
}

// pinnedClient returns an HTTP client that trusts only pool (the pinned hub
// CA, never the system roots, D026); the host name is still verified. Its
// hellos carry the identity.HubCAALPN marker, so a hub with an ACME
// certificate presents its CA-issued one. If clientCert is non-nil, it is
// called for the TLS client certificate.
func pinnedClient(pool *x509.CertPool, clientCert func() *tls.Certificate) *http.Client {
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: identity.HubClientNextProtos()}
	if clientCert != nil {
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return clientCert(), nil
		}
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg, ForceAttemptHTTP2: true},
		Timeout:   httpTimeout,
	}
}
