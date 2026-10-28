package identity

import (
	"crypto"
	"crypto/x509"
	"errors"
	"strconv"
)

// Phase 7 contract (owned by P7-move): signed hub redirects, see
// internal/hub/moving.go.

// ErrNotImplemented marks contract stubs.
var ErrNotImplemented = errors.New("identity: not implemented")

// RedirectPayload is the exact byte string the CA signs for a redirect.
func RedirectPayload(td TrustDomain, hubURL, busURL string, issuedAt int64) []byte {
	return []byte("mycel-redirect/1\n" + string(td) + "\n" + hubURL + "\n" + busURL + "\n" + strconv.FormatInt(issuedAt, 10) + "\n")
}

// SignRedirect signs the payload with the CA key (the key type decides the
// scheme: ECDSA keys sign the SHA-256 digest with ASN.1 encoding, Ed25519
// keys sign the message).
func SignRedirect(key crypto.Signer, td TrustDomain, hubURL, busURL string, issuedAt int64) ([]byte, error) {
	panic(ErrNotImplemented)
}

// VerifyRedirect checks a redirect signature against the CA certificate the
// receiver pinned; it also rejects issued_at more than 24 h in the past or
// 5 min in the future relative to now.
func VerifyRedirect(ca *x509.Certificate, td TrustDomain, hubURL, busURL string, issuedAt int64, sig []byte, now int64) error {
	panic(ErrNotImplemented)
}
