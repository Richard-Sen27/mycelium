// Package identity holds the hub certificate authority, node certificates,
// join tokens and agent tokens.
//
// Contract (owned by the orchestrator): the exported names and signatures in
// this file are the interface other packages build against. The implementing
// agent fills in the bodies and may add unexported helpers and further
// exported functions, but must not change these signatures without asking.
//
// Security rules for this package:
//   - private keys are generated locally and never serialised anywhere but the
//     caller-provided destination (file with mode 0600);
//   - all randomness comes from crypto/rand;
//   - comparisons of secrets use crypto/subtle;
//   - nothing here logs secrets or token strings.
package identity

import (
	"crypto"
	"crypto/x509"
	"errors"
	"time"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("identity: not implemented")

// TrustDomain is the SPIFFE trust domain of a hub, e.g. "hub.example.com" or
// "my-team". It is chosen at `mycel hub init` and never changes.
type TrustDomain string

// NodeSPIFFEID returns "spiffe://<td>/node/<name>".
func NodeSPIFFEID(td TrustDomain, name string) string { panic(ErrNotImplemented) }

// ParseNodeSPIFFEID splits a node SPIFFE ID into trust domain and node name.
func ParseNodeSPIFFEID(id string) (TrustDomain, string, error) { panic(ErrNotImplemented) }

// ValidNodeName reports whether name is usable as a node name: 1–63 chars of
// [a-z0-9-], not starting or ending with "-".
func ValidNodeName(name string) bool { panic(ErrNotImplemented) }

// Default lifetimes.
const (
	CALifetime         = 10 * 365 * 24 * time.Hour
	NodeCertLifetime   = 30 * 24 * time.Hour
	ServerCertLifetime = 365 * 24 * time.Hour
	AgentTokenLifetime = time.Hour
	JoinTokenDefault   = 15 * time.Minute
	JoinTokenMax       = 24 * time.Hour
)

// ---- Keys ----

// GenerateKey creates an ECDSA P-256 key and returns it with its PKCS#8 PEM.
func GenerateKey() (crypto.Signer, []byte, error) { panic(ErrNotImplemented) }

// ParseKeyPEM parses a PKCS#8 or EC PEM private key.
func ParseKeyPEM(pemBytes []byte) (crypto.Signer, error) { panic(ErrNotImplemented) }

// NewCSR builds a PKCS#10 CSR (DER) with CN=name, signed by key.
func NewCSR(key crypto.Signer, name string) ([]byte, error) { panic(ErrNotImplemented) }

// ---- Certificate authority ----

// CA is the hub's root certificate authority.
type CA struct {
	cert *x509.Certificate
	key  crypto.Signer
	td   TrustDomain
}

// NewCA generates a new self-signed root CA for td with CALifetime.
func NewCA(td TrustDomain) (*CA, error) { panic(ErrNotImplemented) }

// LoadCA parses a CA from its certificate and key PEM.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) { panic(ErrNotImplemented) }

// TrustDomain returns the trust domain encoded in the CA certificate
// (URI SAN "spiffe://<td>").
func (c *CA) TrustDomain() TrustDomain { panic(ErrNotImplemented) }

// CertPEM returns the CA certificate PEM.
func (c *CA) CertPEM() []byte { panic(ErrNotImplemented) }

// KeyPEM returns the CA private key PEM (PKCS#8).
func (c *CA) KeyPEM() []byte { panic(ErrNotImplemented) }

// Fingerprint returns the SHA-256 of the CA certificate DER, lowercase hex.
func (c *CA) Fingerprint() string { panic(ErrNotImplemented) }

// Pool returns a cert pool containing only this CA.
func (c *CA) Pool() *x509.CertPool { panic(ErrNotImplemented) }

// SignNodeCSR verifies the CSR signature, ignores everything in the CSR except
// the public key, and issues a client certificate with URI SAN
// spiffe://<td>/node/<name>, CN=name, ExtKeyUsage ClientAuth, lifetime ttl.
// Returns the certificate PEM and its serial (lowercase hex).
func (c *CA) SignNodeCSR(csrDER []byte, name string, ttl time.Duration) (certPEM []byte, serial string, err error) {
	panic(ErrNotImplemented)
}

// IssueServerCert issues a TLS server certificate for the given DNS names and
// IP addresses (strings; IPs are detected), used by the hub API and bus
// listeners. Returns cert PEM and key PEM.
func (c *CA) IssueServerCert(hosts []string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	panic(ErrNotImplemented)
}

// VerifyNodeCert checks that certPEM chains to pool, is a client cert, is
// within its validity period and carries exactly one node SPIFFE ID in td.
// It returns the parsed cert and the node name.
func VerifyNodeCert(pool *x509.CertPool, td TrustDomain, certPEM []byte) (*x509.Certificate, string, error) {
	panic(ErrNotImplemented)
}

// NodeNameFromCert extracts the node name from an already-verified certificate.
func NodeNameFromCert(td TrustDomain, cert *x509.Certificate) (string, error) {
	panic(ErrNotImplemented)
}

// CertFingerprint returns the SHA-256 of DER as lowercase hex.
func CertFingerprint(der []byte) string { panic(ErrNotImplemented) }

// FingerprintPEM parses the first certificate in pemBytes and fingerprints it.
func FingerprintPEM(pemBytes []byte) (string, error) { panic(ErrNotImplemented) }

// SerialHex formats a certificate serial as lowercase hex.
func SerialHex(cert *x509.Certificate) string { panic(ErrNotImplemented) }

// ---- Join tokens ----

// JoinTokenPrefix is the leading marker of a join token string.
const JoinTokenPrefix = "mcl1_"

// JoinToken is the decoded form of a join token string (docs/decisions.md D005).
type JoinToken struct {
	HubURL        string
	ID            string // 16 random bytes, hex
	Secret        []byte // 32 random bytes
	CAFingerprint string
}

// NewJoinToken creates a token with a fresh ID and secret.
func NewJoinToken(hubURL, caFingerprint string) (*JoinToken, error) { panic(ErrNotImplemented) }

// String encodes the token as "mcl1_" + base64url(JoinTokenPayload).
func (t *JoinToken) String() string { panic(ErrNotImplemented) }

// SecretHash returns sha256(secret) as lowercase hex; this is what the hub stores.
func (t *JoinToken) SecretHash() string { panic(ErrNotImplemented) }

// ParseJoinToken decodes a token string. It validates the prefix, the field
// lengths and that hub_url is an https or http URL.
func ParseJoinToken(s string) (*JoinToken, error) { panic(ErrNotImplemented) }

// HashSecret returns sha256(secret) as lowercase hex.
func HashSecret(secret []byte) string { panic(ErrNotImplemented) }

// ConstantTimeEqualHex compares two hex digests in constant time.
func ConstantTimeEqualHex(a, b string) bool { panic(ErrNotImplemented) }

// ---- Agent tokens ----

// AgentClaims are the contents of an agent token.
type AgentClaims struct {
	AgentID   string // "agent:<name>@<node>"
	NodeName  string
	SandboxID string // empty for trusted-host agents
	OwnerID   string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// AgentTokenSigner issues and verifies agent tokens. Tokens are
// "mca1_" + base64url(json(claims)) + "." + base64url(hmac-sha256(key, payload)).
type AgentTokenSigner struct {
	key []byte
}

// AgentTokenPrefix is the leading marker of an agent token string.
const AgentTokenPrefix = "mca1_"

// NewAgentTokenSigner wraps a 32-byte HMAC key. Shorter keys are rejected.
func NewAgentTokenSigner(key []byte) (*AgentTokenSigner, error) { panic(ErrNotImplemented) }

// NewAgentTokenKey returns 32 random bytes.
func NewAgentTokenKey() ([]byte, error) { panic(ErrNotImplemented) }

// Issue signs claims. IssuedAt and ExpiresAt are set by Issue: now and now+ttl.
func (s *AgentTokenSigner) Issue(claims AgentClaims, ttl time.Duration) (string, error) {
	panic(ErrNotImplemented)
}

// Verify checks signature and expiry and returns the claims. Callers must
// additionally check that claims.NodeName matches the presenting node
// connection: that is what binds a token to its sandbox.
func (s *AgentTokenSigner) Verify(token string) (*AgentClaims, error) { panic(ErrNotImplemented) }

// ---- Errors ----

var (
	ErrInvalidToken  = errors.New("identity: invalid token")
	ErrExpired       = errors.New("identity: expired")
	ErrBadSignature  = errors.New("identity: bad signature")
	ErrInvalidName   = errors.New("identity: invalid node name")
	ErrInvalidCert   = errors.New("identity: invalid certificate")
	ErrWrongTrustDom = errors.New("identity: wrong trust domain")
)
