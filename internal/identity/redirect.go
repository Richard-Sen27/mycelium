package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Phase 7 contract (owned by P7-move): signed hub redirects, see
// internal/hub/moving.go.

// Redirect freshness window (VerifyRedirect).
const (
	RedirectMaxAge    = 24 * 60 * 60 // seconds a redirect stays valid
	RedirectMaxFuture = 5 * 60       // seconds a redirect may be dated ahead
)

// RedirectPayload is the exact byte string the CA signs for a redirect.
func RedirectPayload(td TrustDomain, hubURL, busURL string, issuedAt int64) []byte {
	return []byte("mycel-redirect/1\n" + string(td) + "\n" + hubURL + "\n" + busURL + "\n" + strconv.FormatInt(issuedAt, 10) + "\n")
}

// CheckRedirectFields rejects payload fields a redirect must never carry:
// control characters (the payload is newline separated) and a bus address
// that is not host:port or tls://host:port with a numeric port 1-65535.
// SignRedirect and VerifyRedirect both apply it.
func CheckRedirectFields(td TrustDomain, hubURL, busURL string) error {
	for _, f := range []string{string(td), hubURL, busURL} {
		if strings.IndexFunc(f, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return fmt.Errorf("%w: control character in a redirect field", ErrInvalidArgument)
		}
	}
	hostPort := strings.TrimPrefix(busURL, "tls://")
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil || h == "" || strings.ContainsAny(hostPort, "/?#@ ") {
		return fmt.Errorf("%w: redirect bus address must be host:port or tls://host:port", ErrInvalidArgument)
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
		return fmt.Errorf("%w: redirect bus port must be a number from 1 to 65535", ErrInvalidArgument)
	}
	return nil
}

// SignRedirect signs the payload with the CA key (the key type decides the
// scheme: ECDSA keys sign the SHA-256 digest with ASN.1 encoding, Ed25519
// keys sign the message).
func SignRedirect(key crypto.Signer, td TrustDomain, hubURL, busURL string, issuedAt int64) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: no signing key", ErrInvalidKey)
	}
	if err := CheckRedirectFields(td, hubURL, busURL); err != nil {
		return nil, err
	}
	msg := RedirectPayload(td, hubURL, busURL, issuedAt)
	switch key.Public().(type) {
	case *ecdsa.PublicKey:
		d := sha256.Sum256(msg)
		sig, err := key.Sign(rand.Reader, d[:], crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("identity: sign redirect: %w", err)
		}
		return sig, nil
	case ed25519.PublicKey:
		sig, err := key.Sign(rand.Reader, msg, crypto.Hash(0))
		if err != nil {
			return nil, fmt.Errorf("identity: sign redirect: %w", err)
		}
		return sig, nil
	default:
		return nil, fmt.Errorf("%w: redirects need an ECDSA or Ed25519 key", ErrInvalidKey)
	}
}

// VerifyRedirect checks a redirect signature against the CA certificate the
// receiver pinned; it also rejects issued_at more than 24 h in the past or
// 5 min in the future relative to now.
func VerifyRedirect(ca *x509.Certificate, td TrustDomain, hubURL, busURL string, issuedAt int64, sig []byte, now int64) error {
	if ca == nil {
		return fmt.Errorf("%w: no CA certificate", ErrInvalidCert)
	}
	if err := CheckRedirectFields(td, hubURL, busURL); err != nil {
		return err
	}
	if issuedAt > now+RedirectMaxFuture {
		return fmt.Errorf("%w: redirect issued in the future", ErrExpired)
	}
	if issuedAt < now-RedirectMaxAge {
		return fmt.Errorf("%w: redirect older than 24 h", ErrExpired)
	}
	msg := RedirectPayload(td, hubURL, busURL, issuedAt)
	switch pub := ca.PublicKey.(type) {
	case *ecdsa.PublicKey:
		d := sha256.Sum256(msg)
		if !ecdsa.VerifyASN1(pub, d[:], sig) {
			return fmt.Errorf("%w: redirect", ErrBadSignature)
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(pub, msg, sig) {
			return fmt.Errorf("%w: redirect", ErrBadSignature)
		}
	default:
		return fmt.Errorf("%w: unsupported CA key type", ErrInvalidCert)
	}
	return nil
}

// SignRedirect signs a redirect for this CA's trust domain with its key.
func (c *CA) SignRedirect(hubURL, busURL string, issuedAt int64) ([]byte, error) {
	return SignRedirect(c.key, c.td, hubURL, busURL, issuedAt)
}

// ParseCertPEM parses the first CERTIFICATE block of pemBytes.
func ParseCertPEM(pemBytes []byte) (*x509.Certificate, error) { return parseFirstCertPEM(pemBytes) }
